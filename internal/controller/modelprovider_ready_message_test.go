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
	"slices"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// Many long problems from every list of the ModelProvider checks still turn
// Ready False: each list names its first ten entries, quoted up to 256 bytes,
// and counts the rest, so the status write fits the condition message limit.
func TestModelProvider_LongProblemListsTurnReadyFalse(t *testing.T) {
	const name = "mp-long-lists"
	const child = name + "-fb"
	mkSecret(t, name+"-key")
	mkSecret(t, child+"-key")
	mkProvider(t, child, func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "m"}}
	})
	unpriced := longEntries("model-", "a")
	targets := longEntries("target-", "a")
	keys := longEntries("key-", "a")
	missing := longEntries("missing-", "a")
	mkProvider(t, name, func(mp *kaalmv1beta1.ModelProvider) {
		for _, id := range unpriced {
			mp.Spec.Models = append(mp.Spec.Models, kaalmv1beta1.ModelProviderModel{ID: id})
		}
		mp.Spec.Budget = hardBudgetSpec()
		mp.Spec.Budget.Policies = []kaalmv1beta1.ModelProviderBudgetPolicy{
			{AtPercent: 90, Action: kaalmv1beta1.BudgetActionBlock},
		}
		for _, target := range targets {
			mp.Spec.Budget.Policies = append(mp.Spec.Budget.Policies, kaalmv1beta1.ModelProviderBudgetPolicy{
				AtPercent: 50, Action: "degrade", DegradeTo: &target,
			})
		}
		modelMap := map[string]string{}
		for _, k := range keys {
			modelMap[k] = "m"
		}
		mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: child, ModelMap: modelMap}}
		for _, n := range missing {
			mp.Spec.Fallback = append(mp.Spec.Fallback, kaalmv1beta1.FallbackReference{Name: n})
		}
	})
	more := func(n int) int { return n - maxListed }
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: name}, &mp); err != nil {
			return err
		}
		c := condition(mp.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonInvalidDegradeTarget ||
			mp.Status.ObservedGeneration != mp.Generation {
			return fmt.Errorf("Ready = %+v, want False/%s at generation %d",
				c, kaalmv1beta1.ReasonInvalidDegradeTarget, mp.Generation)
		}
		if len(c.Message) > conditionMessageLimit {
			return fmt.Errorf("Ready message is %d bytes", len(c.Message))
		}
		for _, want := range []string{
			fmt.Sprintf("degradeTo has %d more targets that are not models in this provider, not listed", more(len(targets))),
			fmt.Sprintf("model catalog has %d more unpriced models, not listed", more(len(unpriced))),
			fmt.Sprintf("fallback tree has %d more ineligible fallbacks, not listed", more(len(missing))),
			fmt.Sprintf("modelMaps have %d more invalid entries, not listed", more(len(keys))),
		} {
			if !strings.Contains(c.Message, want) {
				return fmt.Errorf("Ready message does not hold %q", want)
			}
		}
		return nil
	})
}

// maxListed is the number of entries each list names; a literal, so the
// tests check the bound on their own.
const maxListed = 10

// Rule 18: validateDegradeTargets names the first ten bad targets and counts
// the rest, each with reason InvalidDegradeTarget.
func TestValidateDegradeTargetsIsBounded(t *testing.T) {
	mp := &kaalmv1beta1.ModelProvider{}
	mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "ok"}}
	ok := "ok"
	mp.Spec.Budget.Policies = []kaalmv1beta1.ModelProviderBudgetPolicy{{Action: "degrade", DegradeTo: &ok}}
	for i := range 12 {
		target := "t" + strconv.Itoa(i)
		mp.Spec.Budget.Policies = append(mp.Spec.Budget.Policies,
			kaalmv1beta1.ModelProviderBudgetPolicy{Action: "degrade", DegradeTo: &target})
	}
	// A repeat of a listed target is neither listed nor counted again.
	mp.Spec.Budget.Policies = append(mp.Spec.Budget.Policies, mp.Spec.Budget.Policies[1])
	var want []string
	for i := range maxListed {
		want = append(want, fmt.Sprintf("degradeTo %q is not a model in this provider", "t"+strconv.Itoa(i)))
	}
	want = append(want, "degradeTo has 2 more targets that are not models in this provider, not listed")
	got := validateDegradeTargets(mp)
	assertProblems(t, got, kaalmv1beta1.ReasonInvalidDegradeTarget, want)
}

// Rule 33: validateHardPricing names the first ten unpriced models and counts
// the rest, each with reason HardBudgetUnpriced.
func TestValidateHardPricingIsBounded(t *testing.T) {
	mp := &kaalmv1beta1.ModelProvider{}
	mp.Spec.Budget.Enforcement = kaalmv1beta1.BudgetEnforcementHard
	mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "priced", CostPer1MInputTokens: "1", CostPer1MOutputTokens: "2"}}
	for i := range 11 {
		mp.Spec.Models = append(mp.Spec.Models, kaalmv1beta1.ModelProviderModel{ID: "m" + strconv.Itoa(i)})
	}
	var want []string
	for i := range maxListed {
		want = append(want, fmt.Sprintf(
			"model %q is unpriced; hard budget enforcement requires a fully priced catalog (rule 33)", "m"+strconv.Itoa(i)))
	}
	want = append(want, "model catalog has 1 more unpriced model, not listed")
	got := validateHardPricing(mp)
	assertProblems(t, got, kaalmv1beta1.ReasonHardBudgetUnpriced, want)
}

func assertProblems(t *testing.T, got []validationProblem, reason string, want []string) {
	t.Helper()
	msgs := make([]string, len(got))
	for i, p := range got {
		msgs[i] = p.message
		if p.reason != reason {
			t.Errorf("problem %q has reason %s, want %s", p.message, p.reason, reason)
		}
	}
	if !slices.Equal(msgs, want) {
		t.Errorf("messages = %q, want %q", msgs, want)
	}
}

// The worst case for the ModelProvider Ready message: every list holds many
// long values made of control bytes, which the quote writes as four
// characters each. The joined message stays within the condition message
// limit, and the reason still follows the precedence.
func TestReadyFalseFromProblemsWorstCaseFitsConditionMessage(t *testing.T) {
	values := make([]string, 50)
	for i := range values {
		values[i] = strings.Repeat("\x01", 1000) + strconv.Itoa(i)
	}
	mp := &kaalmv1beta1.ModelProvider{}
	mp.Spec.Budget.Enforcement = kaalmv1beta1.BudgetEnforcementHard
	for i, v := range values {
		mp.Spec.Models = append(mp.Spec.Models, kaalmv1beta1.ModelProviderModel{ID: v})
		mp.Spec.Budget.Policies = append(mp.Spec.Budget.Policies,
			kaalmv1beta1.ModelProviderBudgetPolicy{Action: "degrade", DegradeTo: &values[(i+1)%len(values)]})
		mp.Spec.AllowedNamespaces = append(mp.Spec.AllowedNamespaces, "["+v)
	}
	// Every degrade target names a model, so add bad ones.
	for _, v := range values {
		bad := "x" + v
		mp.Spec.Budget.Policies = append(mp.Spec.Budget.Policies,
			kaalmv1beta1.ModelProviderBudgetPolicy{Action: "degrade", DegradeTo: &bad})
	}
	var problems []validationProblem
	problems = append(problems, validateDegradeTargets(mp)...)
	problems = append(problems, validateHardPricing(mp)...)
	problems = append(problems, validateNamespacePatterns(mp)...)
	escaped := strings.Repeat(`\x01`, 256)
	for i := range maxListed {
		problems = append(problems,
			validationProblem{reason: kaalmv1beta1.ReasonFallbackIneligible,
				message: fmt.Sprintf("fallback provider \"%s%d\" does not exist", escaped, i)},
			validationProblem{reason: kaalmv1beta1.ReasonInvalidModelMap,
				message: fmt.Sprintf("modelMap on fallback \"%s%d\": key \"k\" is not a model of \"p\"", escaped, i)},
		)
	}
	reason, msg := readyFalseFromProblems(problems)
	if reason != kaalmv1beta1.ReasonInvalidDegradeTarget {
		t.Errorf("reason = %s, want %s", reason, kaalmv1beta1.ReasonInvalidDegradeTarget)
	}
	if len(msg) > conditionMessageLimit {
		t.Errorf("worst-case message is %d bytes, want at most %d", len(msg), conditionMessageLimit)
	}
}

// Rule 41: the modelMap keys are checked in sorted order, so a map with more
// bad entries than the list names gives the same message on every pass.
func TestValidateFallbackModelMapIsDeterministic(t *testing.T) {
	modelMap := map[string]string{}
	for i := range 15 {
		modelMap[fmt.Sprintf("k%02d", i)] = "m"
	}
	primary := &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "primary"},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type:     "openai",
			Models:   []kaalmv1beta1.ModelProviderModel{{ID: "p"}},
			Fallback: []kaalmv1beta1.FallbackReference{{Name: "fb", ModelMap: modelMap}},
		},
	}
	fb := &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "fb"},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type:   "openai",
			Models: []kaalmv1beta1.ModelProviderModel{{ID: "m"}},
		},
	}
	r := deleteReconciler(t, interceptor.Funcs{}, fb)
	var want []string
	for i := range maxListed {
		want = append(want, fmt.Sprintf("modelMap on fallback \"fb\": key \"k%02d\" is not a model of \"primary\"", i))
	}
	want = append(want, "modelMaps have 5 more invalid entries, not listed")
	for range 20 {
		got := validateFallbackMessages(t, r, primary)
		if !slices.Equal(got, want) {
			t.Fatalf("validateFallback messages = %q, want %q", got, want)
		}
	}
}

// validateFallbackMessages runs validateFallback and returns its messages in
// the order it found them.
func validateFallbackMessages(t *testing.T, r *ModelProviderReconciler, primary *kaalmv1beta1.ModelProvider) []string {
	t.Helper()
	problems := r.validateFallback(ctxT(), primary.DeepCopy())
	msgs := make([]string, len(problems))
	for i, p := range problems {
		msgs[i] = p.message
	}
	return msgs
}
