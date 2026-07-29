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
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	"github.com/win07xp/kaalm/internal/gateway"
)

func hardBudgetSpec() kaalmv1alpha1.ModelProviderBudget {
	return kaalmv1alpha1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "100",
		Enforcement: kaalmv1alpha1.BudgetEnforcementHard,
		Hard:        &kaalmv1alpha1.ModelProviderBudgetHard{BoundaryMarginPercent: 5},
		Policies: []kaalmv1alpha1.ModelProviderBudgetPolicy{
			{AtPercent: 100, Action: kaalmv1alpha1.BudgetActionBlock},
		},
	}
}

// Rule 33: hard enforcement over an unpriced catalog gates readiness with
// HardBudgetUnpriced, and pricing the catalog recovers it.
func TestModelProvider_HardBudgetUnpricedGatesAndRecovers(t *testing.T) {
	mkSecret(t, "mp-hard-key")
	mkProvider(t, "mp-hard", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.Budget = hardBudgetSpec()
		mp.Spec.Models = []kaalmv1alpha1.ModelProviderModel{{ID: "free-model"}}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-hard"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonHardBudgetUnpriced)

	eventually(t, func() error {
		var mp kaalmv1alpha1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-hard"}, &mp); err != nil {
			return err
		}
		mp.Spec.Models = []kaalmv1alpha1.ModelProviderModel{
			{ID: "free-model", CostPer1MInputTokens: "1.00", CostPer1MOutputTokens: "2.00"},
		}
		return testClient.Update(ctxT(), &mp)
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-hard"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)
}

// Rules 32 and 34 are apply-time CEL: the apiserver rejects a hard budget
// with no block policy, and a boundary margin at or above a block threshold.
func TestModelProvider_HardBudgetCELRules(t *testing.T) {
	noBlock := &kaalmv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "mp-cel-32"},
		Spec: kaalmv1alpha1.ModelProviderSpec{
			Type: "openai", Endpoint: "https://api.example.com",
			CredentialsRef: kaalmv1alpha1.SecretKeyReference{Name: "k", Key: "token"},
			Budget: kaalmv1alpha1.ModelProviderBudget{
				Period: "monthly", PerNamespaceUSD: "100",
				Enforcement: kaalmv1alpha1.BudgetEnforcementHard,
				Policies: []kaalmv1alpha1.ModelProviderBudgetPolicy{
					{AtPercent: 50, Action: kaalmv1alpha1.BudgetActionWarn},
				},
			},
		},
	}
	if err := testClient.Create(ctxT(), noBlock); err == nil || !strings.Contains(err.Error(), "rule 32") {
		t.Fatalf("hard budget without a block policy: err = %v, want rule 32 CEL rejection", err)
	}

	wideMargin := noBlock.DeepCopy()
	wideMargin.Name = "mp-cel-34"
	wideMargin.Spec.Budget.Policies = []kaalmv1alpha1.ModelProviderBudgetPolicy{
		{AtPercent: 80, Action: kaalmv1alpha1.BudgetActionBlock},
	}
	wideMargin.Spec.Budget.Hard = &kaalmv1alpha1.ModelProviderBudgetHard{BoundaryMarginPercent: 80}
	if err := testClient.Create(ctxT(), wideMargin); err == nil || !strings.Contains(err.Error(), "rule 34") {
		t.Fatalf("margin at the block threshold: err = %v, want rule 34 CEL rejection", err)
	}

	valid := wideMargin.DeepCopy()
	valid.Name = "mp-cel-ok"
	valid.Spec.Budget.Hard = &kaalmv1alpha1.ModelProviderBudgetHard{BoundaryMarginPercent: 5}
	if err := testClient.Create(ctxT(), valid); err != nil {
		t.Fatalf("valid hard budget rejected: %v", err)
	}
	_ = testClient.Delete(ctxT(), valid)
}

// The _marginExceeded flag in a live replica's partial surfaces as the
// BoundaryMarginRaised condition, and clears when the flag drops.
func TestModelProvider_BoundaryMarginRaisedCondition(t *testing.T) {
	mkGatewayPod(t, "margin-gw-0", true)
	mkSecret(t, "mp-margin-key")
	mkProvider(t, "mp-margin", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.Budget = hardBudgetSpec()
		mp.Spec.Models = []kaalmv1alpha1.ModelProviderModel{
			{ID: "m", CostPer1MInputTokens: "1.00", CostPer1MOutputTokens: "1.00"},
		}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-margin"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)

	period := gateway.PeriodKey("monthly", time.Now())
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: gateway.BudgetConfigMapName("mp-margin"), Namespace: testOperatorNamespace,
		},
		Data: map[string]string{
			"margin-gw-0": fmt.Sprintf(`{"period":%q,"team-a":"96.00","_marginExceeded":"true"}`, period),
		},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatalf("create budget cm: %v", err)
	}

	eventually(t, func() error {
		var mp kaalmv1alpha1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-margin"}, &mp); err != nil {
			return err
		}
		c := condition(mp.Status.Conditions, kaalmv1alpha1.ConditionBoundaryMarginRaised)
		if c == nil || c.Status != metav1.ConditionTrue {
			return errString("BoundaryMarginRaised not True yet")
		}
		return nil
	})

	// Flag drops: the condition clears on a later reconcile.
	eventually(t, func() error {
		var got corev1.ConfigMap
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Name: gateway.BudgetConfigMapName("mp-margin"), Namespace: testOperatorNamespace},
			&got); err != nil {
			return err
		}
		got.Data["margin-gw-0"] = fmt.Sprintf(`{"period":%q,"team-a":"96.00"}`, period)
		return testClient.Update(ctxT(), &got)
	})
	eventually(t, func() error {
		var mp kaalmv1alpha1.ModelProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mp-margin"}, &mp); err != nil {
			return err
		}
		c := condition(mp.Status.Conditions, kaalmv1alpha1.ConditionBoundaryMarginRaised)
		if c == nil || c.Status != metav1.ConditionFalse {
			return errString("BoundaryMarginRaised did not clear")
		}
		return nil
	})
}
