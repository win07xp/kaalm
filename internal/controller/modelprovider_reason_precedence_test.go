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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// When several config checks fail, Ready=False takes its reason from a fixed
// precedence (InvalidDegradeTarget, HardBudgetUnpriced, InvalidModelMap,
// FallbackIneligible, InvalidNamespacePattern), and the message lists every problem. The reason comes
// from which check failed, never from the message text, so a model, key, or
// provider name that contains "degradeTo", "unpriced", or "modelMap" does not
// change it.
func TestModelProvider_ReadyFalseReasonPrecedence(t *testing.T) {
	priced := func(id string) kaalmv1beta1.ModelProviderModel {
		return kaalmv1beta1.ModelProviderModel{ID: id, CostPer1MInputTokens: "1", CostPer1MOutputTokens: "2"}
	}
	degradeTo := func(target string) kaalmv1beta1.ModelProviderBudgetPolicy {
		return kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 80, Action: "degrade", DegradeTo: &target}
	}
	backup := eventsProvider("prec-backup", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{priced("m1")}
	})
	cases := []struct {
		name   string
		mutate func(*kaalmv1beta1.ModelProvider)
		reason string
		// substrings each name one problem the message must list.
		substrings []string
	}{{
		name: "every check fails: the degrade target wins",
		mutate: func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{priced("m1"), {ID: "cheap"}}
			mp.Spec.Budget = hardBudgetSpec()
			mp.Spec.Budget.Policies = append(mp.Spec.Budget.Policies, degradeTo("missing"))
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{
				{Name: "prec-gone"},
				{Name: "prec-backup", ModelMap: map[string]string{"not-a-model": "m1"}},
			}
		},
		reason: kaalmv1beta1.ReasonInvalidDegradeTarget,
		substrings: []string{
			`degradeTo "missing"`, `model "cheap" is unpriced`,
			`key "not-a-model"`, `fallback provider "prec-gone" does not exist`,
		},
	}, {
		name: "an unpriced model named degradeTo beats a bad model map",
		mutate: func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{priced("m1"), {ID: "degradeTo-mini"}}
			mp.Spec.Budget = hardBudgetSpec()
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{
				{Name: "prec-backup", ModelMap: map[string]string{"not-a-model": "m1"}},
			}
		},
		reason:     kaalmv1beta1.ReasonHardBudgetUnpriced,
		substrings: []string{`model "degradeTo-mini" is unpriced`, `key "not-a-model"`},
	}, {
		name: "a bad model map beats a missing fallback named unpriced",
		mutate: func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{
				{Name: "prec-unpriced"},
				{Name: "prec-backup", ModelMap: map[string]string{"not-a-model": "m1"}},
			}
		},
		reason:     kaalmv1beta1.ReasonInvalidModelMap,
		substrings: []string{`fallback provider "prec-unpriced" does not exist`, `key "not-a-model"`},
	}, {
		name: "a model map key named degradeTo is still a bad model map",
		mutate: func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{
				{Name: "prec-backup", ModelMap: map[string]string{"degradeTo-key": "m1"}},
			}
		},
		reason:     kaalmv1beta1.ReasonInvalidModelMap,
		substrings: []string{`key "degradeTo-key"`},
	}, {
		name: "a missing fallback named unpriced is fallback ineligible",
		mutate: func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "prec-unpriced"}}
		},
		reason:     kaalmv1beta1.ReasonFallbackIneligible,
		substrings: []string{`fallback provider "prec-unpriced" does not exist`},
	}, {
		name: "a bad model map beats a malformed namespace pattern",
		mutate: func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{
				{Name: "prec-backup", ModelMap: map[string]string{"not-a-model": "m1"}},
			}
			mp.Spec.AllowedNamespaces = []string{"["}
		},
		reason:     kaalmv1beta1.ReasonInvalidModelMap,
		substrings: []string{`key "not-a-model"`, `allowedNamespaces entry "["`},
	}, {
		name: "a missing fallback beats a malformed namespace pattern",
		mutate: func(mp *kaalmv1beta1.ModelProvider) {
			mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "prec-gone"}}
			mp.Spec.AllowedNamespaces = []string{"["}
		},
		reason:     kaalmv1beta1.ReasonFallbackIneligible,
		substrings: []string{`fallback provider "prec-gone" does not exist`, `allowedNamespaces entry "["`},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := eventsProvider("prec-primary", tc.mutate)
			objs := []client.Object{mp, backup.DeepCopy(), providerKey(mp.Name)}
			r, _ := eventsProviderReconciler(t, &statusConflicts{}, nil, objs...)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: mp.Name}}
			if _, err := r.Reconcile(ctxT(), req); err != nil {
				t.Fatal(err)
			}
			var got kaalmv1beta1.ModelProvider
			if err := r.Get(ctxT(), req.NamespacedName, &got); err != nil {
				t.Fatal(err)
			}
			c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tc.reason {
				t.Fatalf("Ready = %+v, want False/%s", c, tc.reason)
			}
			for _, s := range tc.substrings {
				if !strings.Contains(c.Message, s) {
					t.Errorf("Ready message %q does not list %q", c.Message, s)
				}
			}
		})
	}
}

// readyFalseFromProblems ranks a reason missing from the precedence list after
// every listed one, and sorts the messages whatever order the checks ran in.
func TestReadyFalseFromProblems(t *testing.T) {
	reason, msg := readyFalseFromProblems([]validationProblem{
		{reason: "SomeNewReason", message: "c"},
		{reason: kaalmv1beta1.ReasonFallbackIneligible, message: "b"},
		{reason: "SomeNewReason", message: "a"},
	})
	if reason != kaalmv1beta1.ReasonFallbackIneligible || msg != "a; b; c" {
		t.Errorf("got %q, %q; want %q, %q", reason, msg, kaalmv1beta1.ReasonFallbackIneligible, "a; b; c")
	}
	if reason, _ := readyFalseFromProblems([]validationProblem{{reason: "SomeNewReason", message: "a"}}); reason != "SomeNewReason" {
		t.Errorf("an unlisted reason alone: got %q, want SomeNewReason", reason)
	}
}
