/*
Copyright 2026 The Kaalm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// mkGatewayPod creates a Pod carrying the gateway component label in the
// operator namespace, optionally marked Ready.
func mkGatewayPod(t *testing.T, name string, ready bool) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testOperatorNamespace,
			Labels: map[string]string{"app.kubernetes.io/component": "gateway"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "gw", Image: "gw:v1"}}},
	}
	if err := testClient.Create(ctxT(), pod); err != nil {
		t.Fatalf("create gateway pod: %v", err)
	}
	if ready {
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		if err := testClient.Status().Update(ctxT(), pod); err != nil {
			t.Fatalf("mark gateway pod ready: %v", err)
		}
	}
}

func TestModelProvider_BudgetReducerAndGatewayReachable(t *testing.T) {
	mkGatewayPod(t, "budget-gw-0", true)
	mkGatewayPod(t, "budget-gw-1", false)

	mkSecret(t, "mp-budget-key")
	mkProvider(t, "mp-budget", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "mp-budget-key", Key: "token"}
		mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{
			Period: "monthly", PerNamespaceUSD: "100",
			Policies: []kaalmv1beta1.ModelProviderBudgetPolicy{
				{AtPercent: 80, Action: "block"},
			},
		}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-budget"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)

	// GatewayReachable=True: one gateway Pod is Ready.
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-budget"}, &mp); err != nil {
			return err
		}
		c := condition(mp.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable)
		if c == nil || c.Status != metav1.ConditionTrue {
			return errString("GatewayReachable not True yet")
		}
		return nil
	})

	// Two live-replica partials, one stale-replica key, one old-period key.
	period := gateway.PeriodKey("monthly", time.Now())
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: gateway.BudgetConfigMapName("mp-budget"), Namespace: testOperatorNamespace,
		},
		Data: map[string]string{
			"budget-gw-0": fmt.Sprintf(`{"period":%q,"team-a":"50.00","team-b":"10.00"}`, period),
			"budget-gw-1": fmt.Sprintf(`{"period":%q,"team-a":"40.00"}`, period),
			"dead-gw-9":   fmt.Sprintf(`{"period":%q,"team-a":"999.00"}`, period),
			"budget-gw-2": `{"period":"1999-01","team-z":"5.00"}`,
		},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatalf("create budget cm: %v", err)
	}
	// budget-gw-2 does not exist as a Pod, so its old-period entry is both
	// stale-replica and stale-period; prune order does not matter for it.

	eventually(t, func() error {
		var got corev1.ConfigMap
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Name: gateway.BudgetConfigMapName("mp-budget"), Namespace: testOperatorNamespace},
			&got); err != nil {
			return err
		}
		if _, exists := got.Data["dead-gw-9"]; exists {
			return errString("stale replica key not pruned")
		}
		// Pruning must not erase published spend: dead-gw-9's current-period
		// totals fold into _retired, and the canonical sum includes them.
		retiredRaw, exists := got.Data[gateway.RetiredKey]
		if !exists {
			return errString("_retired not written on prune")
		}
		retPeriod, retSpend, _, err := gateway.ParseBudgetPartial(retiredRaw)
		if err != nil {
			return err
		}
		if retPeriod != period || retSpend["team-a"] != 999.00 {
			return errString("_retired contents wrong: " + retiredRaw)
		}
		raw, exists := got.Data[gateway.CanonicalKey]
		if !exists {
			return errString("_canonical not written")
		}
		var canonical map[string]string
		if err := json.Unmarshal([]byte(raw), &canonical); err != nil {
			return err
		}
		if canonical["team-a"] != "1089.00" || canonical["team-b"] != "10.00" {
			return errString("canonical sums wrong: " + raw)
		}
		return nil
	})

	// Status: team-a far past the 80 block threshold is Blocked, team-b
	// Normal. The retired spend counts: 50 + 40 + 999 = 1089 of 100.
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-budget"}, &mp); err != nil {
			return err
		}
		states := map[string]string{}
		percents := map[string]int32{}
		for _, u := range mp.Status.BudgetUsage {
			if u.Period == period {
				states[u.Namespace] = u.State
				percents[u.Namespace] = u.PercentUsed
			}
		}
		if states["team-a"] != "Blocked" || states["team-b"] != "Normal" {
			return errString(fmt.Sprintf("states wrong: %v", states))
		}
		if percents["team-a"] != 1089 {
			return errString(fmt.Sprintf("percent wrong: %v", percents))
		}
		if mp.Status.ClusterSpentUSD != "1099.00" {
			return errString("clusterSpentUSD wrong: " + mp.Status.ClusterSpentUSD)
		}
		return nil
	})
}

// TestModelProvider_BudgetRolloverAndThrottle covers the reducer's rollover
// (a live replica carrying an old-period partial), a malformed partial, and
// the degrade -> Throttled enforcement state.
func TestModelProvider_BudgetRolloverAndThrottle(t *testing.T) {
	mkGatewayPod(t, "roll-gw", true)
	mkGatewayPod(t, "roll-gw2", true)
	mkGatewayPod(t, "roll-bad-gw", true)

	mkSecret(t, "mp-roll-key")
	mkProvider(t, "mp-roll", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "mp-roll-key", Key: "token"}
		mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "cheap"}}
		mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{
			Period: "monthly", PerNamespaceUSD: "100",
			Policies: []kaalmv1beta1.ModelProviderBudgetPolicy{
				{AtPercent: 50, Action: "degrade", DegradeTo: strptr("cheap")},
			},
		}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-roll"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)

	period := gateway.PeriodKey("monthly", time.Now())
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: gateway.BudgetConfigMapName("mp-roll"), Namespace: testOperatorNamespace,
		},
		Data: map[string]string{
			// Current-period spend at 60% of the ceiling -> Throttled by the degrade policy.
			"roll-gw2": fmt.Sprintf(`{"period":%q,"team-y":"60.00"}`, period),
			// A live replica still holding an old-period partial -> rollover/archive.
			"roll-gw": `{"period":"1999-01","team-x":"30.00"}`,
			// Malformed partial -> skipped by the reducer.
			"roll-bad-gw": `{not json`,
		},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatalf("create budget cm: %v", err)
	}

	// The degrade policy throttles team-y at 60% of its ceiling.
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-roll"}, &mp); err != nil {
			return err
		}
		for _, u := range mp.Status.BudgetUsage {
			if u.Period == period && u.Namespace == "team-y" && u.State == kaalmv1beta1.BudgetStateThrottled {
				return nil
			}
		}
		return errString("team-y should be Throttled by the degrade policy")
	})

	// The live replica's old-period partial is archived and its key deleted by
	// the rollover branch (the deletion is the observable proof it ran).
	eventually(t, func() error {
		var got corev1.ConfigMap
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Name: gateway.BudgetConfigMapName("mp-roll"), Namespace: testOperatorNamespace},
			&got); err != nil {
			return err
		}
		if _, exists := got.Data["roll-gw"]; exists {
			return errString("old-period key not pruned after rollover")
		}
		return nil
	})
}

// TestModelProvider_BudgetRequeueWithoutProbe covers the budget re-reconcile
// cadence with the health probe off.
func TestModelProvider_BudgetRequeueWithoutProbe(t *testing.T) {
	mkSecret(t, "mp-budreq-key")
	mkProvider(t, "mp-budreq", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "mp-budreq-key", Key: "token"}
		mp.Spec.HealthCheck = &kaalmv1beta1.ModelProviderHealthCheck{Enabled: false}
		mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{Period: "monthly", PerNamespaceUSD: "100"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-budreq"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
}

// A namespace under its own ceiling but inside a provider over the cluster
// ceiling reports the cluster ratio and the state the gateway enforces.
func TestBudgetUsageEntries_ClusterCeilingWins(t *testing.T) {
	cluster := "10.00"
	mp := &kaalmv1beta1.ModelProvider{Spec: kaalmv1beta1.ModelProviderSpec{
		Budget: kaalmv1beta1.ModelProviderBudget{
			Period:          "monthly",
			PerNamespaceUSD: "100.00",
			ClusterUSD:      &cluster,
			Policies: []kaalmv1beta1.ModelProviderBudgetPolicy{
				{AtPercent: 80, Action: kaalmv1beta1.BudgetActionWarn},
				{AtPercent: 100, Action: kaalmv1beta1.BudgetActionBlock},
			},
		},
	}}
	spend := map[string]float64{"team-a": 6, "team-b": 5}
	got := budgetUsageEntries(mp, spend, "2026-09")
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	for _, e := range got {
		if e.State != kaalmv1beta1.BudgetStateBlocked || e.PercentUsed != 110 {
			t.Errorf("%s: state=%s percentUsed=%d, want Blocked 110 (cluster ceiling)",
				e.Namespace, e.State, e.PercentUsed)
		}
	}

	// Without clusterUSD only the per-namespace ratio counts.
	mp.Spec.Budget.ClusterUSD = nil
	for _, e := range budgetUsageEntries(mp, spend, "2026-09") {
		if e.State != kaalmv1beta1.BudgetStateNormal {
			t.Errorf("%s: state=%s, want Normal without clusterUSD", e.Namespace, e.State)
		}
	}
}

// misconfiguredBudgetObjects are the objects a misconfigured provider's
// budget pass reads: a Ready gateway Pod, a budget ConfigMap with a live, a
// dead, and an old-period partial, and an agent-spend ConfigMap with a dead
// replica's partial.
func misconfiguredBudgetObjects(name, period string) []client.Object {
	gw := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-gw-0", Namespace: testOperatorNamespace, Labels: gatewayPodLabels,
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	budget := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: gateway.BudgetConfigMapName(name), Namespace: testOperatorNamespace},
		Data: map[string]string{
			name + "-gw-0":    fmt.Sprintf(`{"period":%q,"team-a":"90.00"}`, period),
			name + "-gw-dead": fmt.Sprintf(`{"period":%q,"team-a":"5.00"}`, period),
			name + "-gw-old":  `{"period":"1999-01","team-z":"7.00"}`,
		},
	}
	spend := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: gateway.AgentSpendConfigMapName(name), Namespace: testOperatorNamespace},
		Data: map[string]string{
			name + "-gw-dead": fmt.Sprintf(`{"period":%q,"team-a/agent/sup":"2.00"}`, period),
		},
	}
	return []client.Object{gw, budget, spend}
}

// A provider that fails a credential or configuration check still mirrors
// gateway readiness, reduces the budget and agent-spend partials, and keeps
// the one-minute budget cadence; the probe still never runs (#425).
func TestModelProvider_FailedCheckStillReducesBudget(t *testing.T) {
	ctx := context.Background()
	period := gateway.PeriodKey("monthly", time.Now())
	staleConds := func(mp *kaalmv1beta1.ModelProvider) {
		now := metav1.Now()
		mp.Status.Conditions = append(mp.Status.Conditions,
			metav1.Condition{Type: kaalmv1beta1.ConditionGatewayReachable, Status: metav1.ConditionFalse,
				Reason: "GatewayUnavailable", Message: "no gateway", LastTransitionTime: now},
			metav1.Condition{Type: kaalmv1beta1.ConditionFallbackIneligible, Status: metav1.ConditionTrue,
				Reason: kaalmv1beta1.ReasonFallbackIneligible, Message: "stale finding", LastTransitionTime: now})
	}
	budget := kaalmv1beta1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "100",
		Policies: []kaalmv1beta1.ModelProviderBudgetPolicy{{AtPercent: 80, Action: "block"}},
	}
	otherHost := func(name string) *corev1.Secret {
		s := providerKey(name)
		s.Annotations = map[string]string{kaalmv1beta1.AnnotationProviderHosts: "other.example.com"}
		return s
	}
	cases := []struct {
		name   string
		reason string
		secret func(name string) *corev1.Secret
		mutate func(*kaalmv1beta1.ModelProvider)
		config bool // a configuration exit, after the credential checks
	}{
		{"CredentialsMissing", kaalmv1beta1.ReasonCredentialsMissing, nil, nil, false},
		{"EndpointHostNotApproved", kaalmv1beta1.ReasonEndpointHostNotApproved, otherHost, nil, false},
		{"FallbackIneligible", kaalmv1beta1.ReasonFallbackIneligible, providerKey, func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "missing-fallback"}}
		}, true},
		{"InvalidNamespacePattern", kaalmv1beta1.ReasonInvalidNamespacePattern, providerKey,
			func(mp *kaalmv1beta1.ModelProvider) { mp.Spec.AllowedNamespaces = []string{"*", "["} }, true},
		{"HardBudgetUnpriced", kaalmv1beta1.ReasonHardBudgetUnpriced, providerKey, func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Budget = hardBudgetSpec()
			mp.Spec.Budget.Policies = budget.Policies
			mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "free-model"}}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "mp-misconf-" + strings.ToLower(tc.name)
			mp := probedProvider(name, metav1.ConditionTrue, time.Now().Add(-time.Hour))
			mp.Spec.Budget = budget
			staleConds(mp)
			if tc.mutate != nil {
				tc.mutate(mp)
			}
			objs := append(misconfiguredBudgetObjects(name, period), mp)
			if tc.secret != nil {
				objs = append(objs, tc.secret(name))
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(objs...).WithStatusSubresource(mp).Build()
			health := newFakeHealth()
			r := &ModelProviderReconciler{
				Client: c, Recorder: record.NewFakeRecorder(16),
				OperatorNamespace: testOperatorNamespace, Health: health,
			}
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
			if err != nil {
				t.Fatal(err)
			}
			if res.RequeueAfter != time.Minute {
				t.Errorf("RequeueAfter = %v, want 1m", res.RequeueAfter)
			}
			if n := health.count(name); n != 0 {
				t.Errorf("probe ran %d times, want 0", n)
			}

			var got kaalmv1beta1.ModelProvider
			if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
				t.Fatal(err)
			}
			if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady); c == nil ||
				c.Status != metav1.ConditionFalse || c.Reason != tc.reason {
				t.Errorf("Ready = %+v, want False %s", c, tc.reason)
			}
			if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable); c == nil ||
				c.Status != metav1.ConditionTrue || c.Reason != "GatewayReady" {
				t.Errorf("GatewayReachable = %+v, want True GatewayReady", c)
			}
			var current, archived bool
			for _, u := range got.Status.BudgetUsage {
				switch {
				case u.Namespace == "team-a" && u.Period == period:
					current = u.State == "Blocked" && u.SpentUSD == "95.00"
				case u.Namespace == "team-z" && u.Period == "1999-01":
					archived = true
				}
			}
			if !current || !archived {
				t.Errorf("budgetUsage = %+v, want team-a Blocked at 95.00 and team-z archived", got.Status.BudgetUsage)
			}
			if got.Status.ClusterSpentUSD != "95.00" {
				t.Errorf("clusterSpentUSD = %q, want 95.00", got.Status.ClusterSpentUSD)
			}
			if tc.config {
				if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible); c == nil ||
					c.Status != metav1.ConditionTrue || c.Message != "stale finding" {
					t.Errorf("FallbackIneligible = %+v, want it kept as it was", c)
				}
			}

			var cm corev1.ConfigMap
			if err := c.Get(ctx, types.NamespacedName{Namespace: testOperatorNamespace,
				Name: gateway.BudgetConfigMapName(name)}, &cm); err != nil {
				t.Fatal(err)
			}
			if cm.Data[gateway.CanonicalKey] != `{"team-a":"95.00"}` {
				t.Errorf("_canonical = %q, want team-a 95.00", cm.Data[gateway.CanonicalKey])
			}
			if _, ok := cm.Data[gateway.RetiredKey]; !ok {
				t.Error("_retired not written")
			}
			for _, k := range []string{name + "-gw-dead", name + "-gw-old"} {
				if _, ok := cm.Data[k]; ok {
					t.Errorf("budget key %s not pruned", k)
				}
			}
			var spend corev1.ConfigMap
			if err := c.Get(ctx, types.NamespacedName{Namespace: testOperatorNamespace,
				Name: gateway.AgentSpendConfigMapName(name)}, &spend); err != nil {
				t.Fatal(err)
			}
			if _, ok := spend.Data[gateway.CanonicalKey]; !ok {
				t.Error("agent-spend _canonical not written")
			}
			if _, ok := spend.Data[name+"-gw-dead"]; ok {
				t.Error("agent-spend dead key not pruned")
			}
		})
	}

	t.Run("credentials missing, no budget period", func(t *testing.T) {
		name := "mp-misconf-nobudget"
		mp := probedProvider(name, metav1.ConditionTrue, time.Now().Add(-time.Hour))
		gw := misconfiguredBudgetObjects(name, period)[0]
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(mp, gw).WithStatusSubresource(mp).Build()
		r := &ModelProviderReconciler{
			Client: c, Recorder: record.NewFakeRecorder(16),
			OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
		}
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		if res != (ctrl.Result{}) {
			t.Errorf("result = %+v, want none without a budget period", res)
		}
		var got kaalmv1beta1.ModelProvider
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
			t.Fatal(err)
		}
		if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable); c == nil ||
			c.Status != metav1.ConditionTrue {
			t.Errorf("GatewayReachable = %+v, want True", c)
		}
	})
}

// End to end: a provider stopped at a configuration check still folds a
// budget ConfigMap written after it went Ready=False.
func TestModelProvider_BudgetFoldRunsWhileMisconfigured(t *testing.T) {
	mkGatewayPod(t, "misconf-gw-0", true)
	mkSecret(t, "mp-misconf-key")
	mkProvider(t, "mp-misconf", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "mp-misconf-key", Key: "token"}
		mp.Spec.AllowedNamespaces = []string{"*", "["}
		mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{
			Period: "monthly", PerNamespaceUSD: "100",
			Policies: []kaalmv1beta1.ModelProviderBudgetPolicy{{AtPercent: 80, Action: "block"}},
		}
	})
	expectReady(t, providerConditions("mp-misconf"), metav1.ConditionFalse,
		kaalmv1beta1.ReasonInvalidNamespacePattern)

	period := gateway.PeriodKey("monthly", time.Now())
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: gateway.BudgetConfigMapName("mp-misconf"), Namespace: testOperatorNamespace},
		Data:       map[string]string{"misconf-gw-0": fmt.Sprintf(`{"period":%q,"team-a":"90.00"}`, period)},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-misconf"}, &mp); err != nil {
			return err
		}
		blocked := false
		for _, u := range mp.Status.BudgetUsage {
			blocked = blocked || (u.Namespace == "team-a" && u.State == "Blocked")
		}
		if !blocked {
			return fmt.Errorf("budgetUsage = %+v, want team-a Blocked", mp.Status.BudgetUsage)
		}
		if c := condition(mp.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable); c == nil ||
			c.Status != metav1.ConditionTrue {
			return fmt.Errorf("GatewayReachable = %+v, want True", c)
		}
		var got corev1.ConfigMap
		if err := testClient.Get(ctxT(), client.ObjectKeyFromObject(cm), &got); err != nil {
			return err
		}
		if _, ok := got.Data[gateway.CanonicalKey]; !ok {
			return errString("_canonical not written")
		}
		return nil
	})
}
