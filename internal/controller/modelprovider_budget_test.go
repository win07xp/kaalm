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
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
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
	mkProvider(t, "mp-budget", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-budget-key", Key: "token"}
		mp.Spec.Budget = kaalmv1alpha1.ModelProviderBudget{
			Period: "monthly", PerNamespaceUSD: "100",
			Policies: []kaalmv1alpha1.ModelProviderBudgetPolicy{
				{AtPercent: 80, Action: "block"},
			},
		}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-budget"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)

	// GatewayReachable=True: one gateway Pod is Ready.
	eventually(t, func() error {
		var mp kaalmv1alpha1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-budget"}, &mp); err != nil {
			return err
		}
		c := condition(mp.Status.Conditions, kaalmv1alpha1.ConditionGatewayReachable)
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
		raw, exists := got.Data[gateway.CanonicalKey]
		if !exists {
			return errString("_canonical not written")
		}
		var canonical map[string]string
		if err := json.Unmarshal([]byte(raw), &canonical); err != nil {
			return err
		}
		if canonical["team-a"] != "90.00" || canonical["team-b"] != "10.00" {
			return errString("canonical sums wrong: " + raw)
		}
		return nil
	})

	// Status: team-a at 90% is Blocked (>=80 block policy), team-b Normal.
	eventually(t, func() error {
		var mp kaalmv1alpha1.ModelProvider
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
		if percents["team-a"] != 90 {
			return errString(fmt.Sprintf("percent wrong: %v", percents))
		}
		if mp.Status.ClusterSpentUSD != "100.00" {
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
	mkProvider(t, "mp-roll", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-roll-key", Key: "token"}
		mp.Spec.Models = []kaalmv1alpha1.ModelProviderModel{{ID: "cheap"}}
		mp.Spec.Budget = kaalmv1alpha1.ModelProviderBudget{
			Period: "monthly", PerNamespaceUSD: "100",
			Policies: []kaalmv1alpha1.ModelProviderBudgetPolicy{
				{AtPercent: 50, Action: "degrade", DegradeTo: strptr("cheap")},
			},
		}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-roll"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)

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
		var mp kaalmv1alpha1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-roll"}, &mp); err != nil {
			return err
		}
		for _, u := range mp.Status.BudgetUsage {
			if u.Period == period && u.Namespace == "team-y" && u.State == kaalmv1alpha1.BudgetStateThrottled {
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
	mkProvider(t, "mp-budreq", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-budreq-key", Key: "token"}
		mp.Spec.HealthCheck = &kaalmv1alpha1.ModelProviderHealthCheck{Enabled: false}
		mp.Spec.Budget = kaalmv1alpha1.ModelProviderBudget{Period: "monthly", PerNamespaceUSD: "100"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-budreq"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)
}
