/*
Copyright 2026.

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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
	"github.com/win07xp/kubeclaw/internal/gateway"
)

// TestModelProvider_BudgetRolloverAndThrottle covers the reducer's rollover
// (a live replica carrying an old-period partial), a malformed partial, and
// the degrade -> Throttled enforcement state.
func TestModelProvider_BudgetRolloverAndThrottle(t *testing.T) {
	mkGatewayPod(t, "roll-gw", true)
	mkGatewayPod(t, "roll-gw2", true)
	mkGatewayPod(t, "roll-bad-gw", true)

	mkSecret(t, "mp-roll-key")
	mkProvider(t, "mp-roll", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mp-roll-key", Key: "token"}
		mp.Spec.Models = []agentryv1alpha1.ModelProviderModel{{ID: "cheap"}}
		mp.Spec.Budget = agentryv1alpha1.ModelProviderBudget{
			Period: "monthly", PerNamespaceUSD: "100",
			Policies: []agentryv1alpha1.ModelProviderBudgetPolicy{
				{AtPercent: 50, Action: "degrade", DegradeTo: strptr("cheap")},
			},
		}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-roll"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, agentryv1alpha1.ReasonCredentialsValid)

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
		var mp agentryv1alpha1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-roll"}, &mp); err != nil {
			return err
		}
		for _, u := range mp.Status.BudgetUsage {
			if u.Period == period && u.Namespace == "team-y" && u.State == agentryv1alpha1.BudgetStateThrottled {
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
