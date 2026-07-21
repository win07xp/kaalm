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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// mkBlockingProvider stands up a ModelProvider with a per-namespace budget and
// a block-at-80% policy, plus a live gateway replica. Spend is driven through
// the budget ConfigMap the ModelProvider reconciler reduces, so the resulting
// status.budgetUsage[ns].State is produced by the real accounting path.
func mkBlockingProvider(t *testing.T, name, replicaKey string) {
	t.Helper()
	mkGatewayPod(t, replicaKey, true)
	mkSecret(t, name+"-key")
	mkProvider(t, name, func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.Budget = kaalmv1alpha1.ModelProviderBudget{
			Period: "monthly", PerNamespaceUSD: "100",
			Policies: []kaalmv1alpha1.ModelProviderBudgetPolicy{{AtPercent: 80, Action: "block"}},
		}
	})
}

// setProviderSpend upserts the "default" namespace's spend (where mkAgent puts
// agents) under one replica key in the provider's budget ConfigMap
// (create-or-update, conflict-tolerant).
func setProviderSpend(t *testing.T, providerName, replicaKey, spendUSD string) {
	t.Helper()
	period := gateway.PeriodKey("monthly", time.Now())
	cmName := gateway.BudgetConfigMapName(providerName)
	entry := fmt.Sprintf(`{"period":%q,"default":%q}`, period, spendUSD)
	eventually(t, func() error {
		var cm corev1.ConfigMap
		err := testClient.Get(ctxT(), types.NamespacedName{Name: cmName, Namespace: testOperatorNamespace}, &cm)
		if apierrors.IsNotFound(err) {
			return testClient.Create(ctxT(), &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: testOperatorNamespace},
				Data:       map[string]string{replicaKey: entry},
			})
		}
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[replicaKey] = entry
		return testClient.Update(ctxT(), &cm)
	})
}

func agentDegraded(t *testing.T, name string) *metav1.Condition {
	t.Helper()
	var ag kaalmv1alpha1.Agent
	_ = testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ag)
	return condition(ag.Status.Conditions, kaalmv1alpha1.ConditionDegraded)
}

func agentPhase(t *testing.T, name string) kaalmv1alpha1.AgentPhase {
	t.Helper()
	var ag kaalmv1alpha1.Agent
	_ = testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ag)
	return ag.Status.Phase
}

// S10: an Agent whose namespace is budget-Blocked on a referenced provider gets
// a Degraded condition (reason BudgetExhausted) WITHOUT a phase transition.
func TestAgent_BudgetExhaustedSetsDegradedConditionPreservingPhase(t *testing.T) {
	mkBlockingProvider(t, "s10-block", "s10-block-gw-0")
	setProviderSpend(t, "s10-block", "s10-block-gw-0", "250.00") // 250% of the 100 ceiling
	mkClass(t, "s10-block-class", "s10-block")
	mkAgent(t, "s10-block-agent", "s10-block-class", "s10-block")

	eventually(t, func() error {
		c := agentDegraded(t, "s10-block-agent")
		if c == nil {
			return errString("no Degraded condition yet")
		}
		if c.Status != metav1.ConditionTrue {
			return errString("Degraded=" + string(c.Status) + " want True")
		}
		if c.Reason != kaalmv1alpha1.ReasonBudgetExhausted {
			return errString("reason=" + c.Reason + " want BudgetExhausted")
		}
		return nil
	})

	// Phase must be preserved: budget exhaustion is a recoverable runtime
	// state, not a phase transition. The agent is anything but Degraded.
	if p := agentPhase(t, "s10-block-agent"); p == kaalmv1alpha1.AgentDegraded {
		t.Fatalf("phase should be preserved, not Degraded")
	}
}

// The Degraded condition clears on its own once the provider stops reporting
// the namespace as Blocked (here: spend drops below the ceiling).
func TestAgent_BudgetConditionClearsWhenUnblocked(t *testing.T) {
	mkBlockingProvider(t, "s10-clear", "s10-clear-gw-0")
	setProviderSpend(t, "s10-clear", "s10-clear-gw-0", "250.00")
	mkClass(t, "s10-clear-class", "s10-clear")
	mkAgent(t, "s10-clear-agent", "s10-clear-class", "s10-clear")

	eventually(t, func() error {
		if c := agentDegraded(t, "s10-clear-agent"); c == nil || c.Status != metav1.ConditionTrue {
			return errString("Degraded not True yet")
		}
		return nil
	})

	// Spend drops under the ceiling: the provider re-reports Normal, the watch
	// re-queues the agent, and the condition is removed.
	setProviderSpend(t, "s10-clear", "s10-clear-gw-0", "10.00")
	eventually(t, func() error {
		if c := agentDegraded(t, "s10-clear-agent"); c != nil {
			return errString("Degraded still present: " + string(c.Status))
		}
		return nil
	})
}

// An Agent whose namespace is within budget never gets a Degraded condition.
func TestAgent_BudgetConditionNotSetWhenWithinBudget(t *testing.T) {
	mkBlockingProvider(t, "s10-ok", "s10-ok-gw-0")
	setProviderSpend(t, "s10-ok", "s10-ok-gw-0", "10.00") // 10% of the ceiling
	mkClass(t, "s10-ok-class", "s10-ok")
	mkAgent(t, "s10-ok-agent", "s10-ok-class", "s10-ok")

	// The image-less agent settles at Ready=False/InvalidReference (the no-image
	// gate), which is set in the same reconcile pass just after the budget
	// check. Observing it proves reconcileBudgetCondition ran; a within-budget
	// namespace must leave no Degraded condition behind.
	expectReady(t, func() []metav1.Condition {
		var ag kaalmv1alpha1.Agent
		_ = testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "s10-ok-agent"}, &ag)
		return ag.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonInvalidReference)

	if c := agentDegraded(t, "s10-ok-agent"); c != nil {
		t.Fatalf("unexpected Degraded condition on a within-budget agent: %s", c.Reason)
	}
}
