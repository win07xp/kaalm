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

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
		retPeriod, retSpend, _, _, err := gateway.ParseBudgetPartial(retiredRaw)
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
// the one-minute budget cadence; the probe still never runs.
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

			expectMisconfiguredStatus(t, c, name, period, tc.reason, tc.config)
			expectMisconfiguredFold(t, c, name)
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

// expectMisconfiguredStatus checks a misconfigured provider's status after
// one pass: Ready names reason, GatewayReachable and the budget usage are
// fresh, and, after a configuration exit, FallbackIneligible is kept.
func expectMisconfiguredStatus(t *testing.T, c client.Client, name, period, reason string, config bool) {
	t.Helper()
	ctx := context.Background()
	var got kaalmv1beta1.ModelProvider
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady); c == nil ||
		c.Status != metav1.ConditionFalse || c.Reason != reason {
		t.Errorf("Ready = %+v, want False %s", c, reason)
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
	if config {
		if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionFallbackIneligible); c == nil ||
			c.Status != metav1.ConditionTrue || c.Message != "stale finding" {
			t.Errorf("FallbackIneligible = %+v, want it kept as it was", c)
		}
	}

}

// expectMisconfiguredFold checks the budget and agent-spend ConfigMaps were
// reduced: canonical written, dead and old-period keys pruned.
func expectMisconfiguredFold(t *testing.T, c client.Client, name string) {
	t.Helper()
	ctx := context.Background()
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

// staleBudgetStatus is the status a provider kept from when it tracked a
// budget: team-a Blocked, 95.00 spent, and BoundaryMarginRaised.
func staleBudgetStatus(mp *kaalmv1beta1.ModelProvider) {
	period := gateway.PeriodKey("monthly", time.Now())
	mp.Status.BudgetUsage = []kaalmv1beta1.ModelProviderBudgetUsage{{
		Namespace: "team-a", Period: period, SpentUSD: "95.00", PercentUsed: 95, State: "Blocked",
	}}
	mp.Status.ClusterSpentUSD = "95.00"
	mp.Status.Conditions = append(mp.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionBoundaryMarginRaised, Status: metav1.ConditionTrue,
		Reason: kaalmv1beta1.ReasonBoundaryMarginRaised, Message: "raised", LastTransitionTime: metav1.Now(),
	})
}

// A provider that stops tracking a budget, or whose budget ConfigMap is
// gone, clears the budget status nothing maintains any more.
func TestModelProvider_BudgetOffClearsBudgetStatus(t *testing.T) {
	ctx := context.Background()
	period := gateway.PeriodKey("monthly", time.Now())
	block := []kaalmv1beta1.ModelProviderBudgetPolicy{{AtPercent: 80, Action: "block"}}
	cases := []struct {
		name   string
		budget kaalmv1beta1.ModelProviderBudget
		mutate func(*kaalmv1beta1.ModelProvider)
		withCM bool
		reason string // the Ready reason a failing check sets, if any
	}{
		{name: "period none", budget: kaalmv1beta1.ModelProviderBudget{
			Period: "none", PerNamespaceUSD: "100", Policies: block}, withCM: true},
		{name: "budget removed"},
		{name: "budget ConfigMap missing", budget: kaalmv1beta1.ModelProviderBudget{
			Period: "monthly", PerNamespaceUSD: "100", Policies: block}},
		{name: "period none on a provider that fails a check", budget: kaalmv1beta1.ModelProviderBudget{
			Period: "none", PerNamespaceUSD: "100", Policies: block},
			mutate: func(mp *kaalmv1beta1.ModelProvider) { mp.Spec.AllowedNamespaces = []string{"*", "["} },
			reason: kaalmv1beta1.ReasonInvalidNamespacePattern},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("mp-budget-off-%d", i)
			mp := eventsProvider(name, func(mp *kaalmv1beta1.ModelProvider) {
				mp.Spec.Budget = tc.budget
				if tc.mutate != nil {
					tc.mutate(mp)
				}
			})
			staleBudgetStatus(mp)
			objs := []client.Object{mp, providerKey(name)}
			var cmData map[string]string
			if tc.withCM {
				extra := misconfiguredBudgetObjects(name, period)
				objs = append(objs, extra[0], extra[1])
				cmData = extra[1].(*corev1.ConfigMap).Data
			}
			r, rec := eventsProviderReconciler(t, &statusConflicts{}, nil, objs...)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			var got kaalmv1beta1.ModelProvider
			if err := r.Get(ctx, req.NamespacedName, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Status.BudgetUsage) != 0 || got.Status.ClusterSpentUSD != "" {
				t.Errorf("budgetUsage = %+v, clusterSpentUSD = %q; want both empty",
					got.Status.BudgetUsage, got.Status.ClusterSpentUSD)
			}
			if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionBoundaryMarginRaised); c != nil {
				t.Errorf("BoundaryMarginRaised = %+v, want it removed", c)
			}
			if tc.reason != "" {
				if c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady); c == nil || c.Reason != tc.reason {
					t.Errorf("Ready = %+v, want reason %s", c, tc.reason)
				}
			}
			if ev := withPrefix(drainEvents(rec), "Warning "+kaalmv1beta1.ReasonBoundaryMarginRaised); len(ev) != 0 {
				t.Errorf("BoundaryMarginRaised events = %q, want none", ev)
			}
			if tc.withCM {
				var cm corev1.ConfigMap
				if err := r.Get(ctx, types.NamespacedName{Namespace: testOperatorNamespace,
					Name: gateway.BudgetConfigMapName(name)}, &cm); err != nil {
					t.Fatal(err)
				}
				if !equality.Semantic.DeepEqual(cm.Data, cmData) {
					t.Errorf("budget ConfigMap changed: %v", cm.Data)
				}
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			var again kaalmv1beta1.ModelProvider
			if err := r.Get(ctx, req.NamespacedName, &again); err != nil {
				t.Fatal(err)
			}
			if again.ResourceVersion != got.ResourceVersion {
				t.Error("a steady pass wrote status again")
			}
		})
	}
}

// Turning a budget off drops the provider's canonical-spend series, and
// only that provider's.
func TestModelProvider_BudgetOffDropsCanonicalGauge(t *testing.T) {
	period := gateway.PeriodKey("monthly", time.Now())
	providerBudgetCanonical.WithLabelValues("gauge-off", "team-a", period).Set(95)
	providerBudgetCanonical.WithLabelValues("gauge-keep", "team-a", period).Set(10)
	mp := eventsProvider("gauge-off", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{Period: "none"}
	})
	r, _ := eventsProviderReconciler(t, &statusConflicts{}, nil, mp, providerKey("gauge-off"))
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "gauge-off"}}); err != nil {
		t.Fatal(err)
	}
	if n := providerBudgetCanonical.DeletePartialMatch(prometheus.Labels{"provider": "gauge-off"}); n != 0 {
		t.Errorf("gauge-off series left = %d, want 0", n)
	}
	if n := providerBudgetCanonical.DeletePartialMatch(prometheus.Labels{"provider": "gauge-keep"}); n != 1 {
		t.Errorf("gauge-keep series = %d, want 1 (untouched)", n)
	}
}

// A provider whose delete a referrer holds still mirrors gateway readiness,
// reduces the budget and agent-spend partials, and keeps the one-minute
// budget cadence: referrers still route through it and the gateway keeps
// counting its spend.
func TestModelProvider_HeldDeleteStillReducesBudget(t *testing.T) {
	ctx := context.Background()
	name := "mp-held-budget"
	period := gateway.PeriodKey("monthly", time.Now())
	now := metav1.Now()
	mp := probedProvider(name, metav1.ConditionTrue, time.Now().Add(-time.Hour))
	mp.DeletionTimestamp = &now
	mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "100",
		Policies: []kaalmv1beta1.ModelProviderBudgetPolicy{{AtPercent: 80, Action: "block"}},
	}
	mp.Status.Conditions = append(mp.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionGatewayReachable, Status: metav1.ConditionFalse,
		Reason: "GatewayUnavailable", Message: "no gateway", LastTransitionTime: now,
	})
	c := heldDeleteClient(t, name, mp, misconfiguredBudgetObjects(name, period)...)
	rec := record.NewFakeRecorder(16)
	r := &ModelProviderReconciler{
		Client: c, Recorder: rec, OperatorNamespace: testOperatorNamespace, Health: newFakeHealth(),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %v, want 1m", res.RequeueAfter)
	}
	expectMisconfiguredStatus(t, c, name, period, kaalmv1beta1.ReasonDeletionBlocked, false)
	expectMisconfiguredFold(t, c, name)

	var got kaalmv1beta1.ModelProvider
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, kaalmv1beta1.ProviderFinalizer) {
		t.Error("finalizer removed while a referrer holds the delete")
	}
	var events []string
	for len(rec.Events) > 0 {
		events = append(events, <-rec.Events)
	}
	if len(events) != 1 || !strings.Contains(events[0], "Warning "+kaalmv1beta1.ReasonDeletionBlocked) {
		t.Errorf("events = %q, want one DeletionBlocked Warning", events)
	}

	// The second pass drops the old period's archived entry, whose partial
	// the first fold pruned; from then on the hold is steady.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var again kaalmv1beta1.ModelProvider
	if err := c.Get(ctx, req.NamespacedName, &again); err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != got.ResourceVersion {
		t.Errorf("a steady hold wrote the provider: resourceVersion %s -> %s", got.ResourceVersion, again.ResourceVersion)
	}
	if len(rec.Events) != 0 {
		t.Errorf("a steady hold sent %q", <-rec.Events)
	}
}

// Keys written for a deleted provider of the same name (tagged with its UID)
// are deleted without being summed, retired, or archived; untagged keys
// count as the current provider's.
func TestModelProvider_BudgetReducerDropsOtherIncarnation(t *testing.T) {
	ctx := context.Background()
	period := gateway.PeriodKey("monthly", time.Now())
	tagged := func(p, ns, usd string) string {
		return fmt.Sprintf(`{"period":%q,%q:%q,"_providerUID":"uid-old"}`, p, ns, usd)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: gateway.BudgetConfigMapName("inc"), Namespace: testOperatorNamespace},
		Data: map[string]string{
			"gw-0":             tagged(period, "team-a", "90.00"),
			"gw-1":             fmt.Sprintf(`{"period":%q,"team-a":"5.00"}`, period),
			gateway.RetiredKey: tagged(period, "team-a", "7.00"),
			"gw-9":             tagged(period, "team-a", "3.00"),
			"gw-8":             tagged("1999-01", "team-a", "4.00"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cm).Build()
	r := &ModelProviderReconciler{Client: c, OperatorNamespace: testOperatorNamespace}
	mp := eventsProvider("inc", func(mp *kaalmv1beta1.ModelProvider) {
		mp.UID = "uid-new"
		mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{Period: "monthly", PerNamespaceUSD: "100"}
	})
	if err := r.reconcileBudget(ctx, mp, map[string]bool{"gw-0": true, "gw-1": true}); err != nil {
		t.Fatal(err)
	}

	var got corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &got); err != nil {
		t.Fatal(err)
	}
	for k, v := range got.Data {
		if strings.Contains(v, "uid-old") {
			t.Errorf("key %s written for the old provider survived: %s", k, v)
		}
	}
	if got.Data[gateway.CanonicalKey] != `{"team-a":"5.00"}` {
		t.Errorf("_canonical = %s, want only the untagged 5.00", got.Data[gateway.CanonicalKey])
	}
	if raw, ok := got.Data[gateway.RetiredKey]; ok {
		if _, _, _, uid, err := gateway.ParseBudgetPartial(raw); err != nil || uid != "uid-new" {
			t.Errorf("_retired = %s, want tagged uid-new", raw)
		}
	}
	want := []kaalmv1beta1.ModelProviderBudgetUsage{{
		Namespace: "team-a", Period: period, SpentUSD: "5.00", PercentUsed: 5, State: kaalmv1beta1.BudgetStateNormal,
	}}
	if !equality.Semantic.DeepEqual(mp.Status.BudgetUsage, want) {
		t.Errorf("budgetUsage = %+v, want %+v", mp.Status.BudgetUsage, want)
	}
}
