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
	"strconv"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// costSanity names the first ten costlier degrade targets in policy order,
// each once, and counts the rest.
func TestCostSanityIsBounded(t *testing.T) {
	models := []kaalmv1beta1.ModelProviderModel{{ID: "cheap", CostPer1MInputTokens: "1", CostPer1MOutputTokens: "1"}}
	var targets []string
	for i := range 12 {
		id := "t" + strconv.Itoa(i)
		models = append(models, kaalmv1beta1.ModelProviderModel{ID: id, CostPer1MInputTokens: "10", CostPer1MOutputTokens: "10"})
		targets = append(targets, id)
	}
	// A repeat of a listed target is neither listed nor counted again.
	targets = append(targets, "t1")
	c := costSanityFindings(t, models, targets...)
	var want []string
	for i := range maxListed {
		want = append(want, fmt.Sprintf(`degradeTo "t%d" ($10.00 average) costs more than "cheap" ($1.00)`, i))
	}
	want = append(want, "degradeTo has 2 more targets that cost more than the cheapest model, not listed")
	if c == nil || c.Message != strings.Join(want, "; ") {
		t.Errorf("DegradeTargetNotCheapest = %+v\nwant message %q", c, strings.Join(want, "; "))
	}
}

// setMaxOutputTokensUnset names the first ten provider/model pairs, quoted,
// and counts the rest.
func TestSetMaxOutputTokensUnsetIsBounded(t *testing.T) {
	var names, quoted []string
	for i := range 12 {
		names = append(names, fmt.Sprintf("claude/m%02d", i))
		if i < maxListed {
			quoted = append(quoted, fmt.Sprintf("%q", names[i]))
		}
	}
	mp := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r := &ModelProviderReconciler{}
	r.setMaxOutputTokensUnset(mp, names)
	r.events.take(mp)
	want := "a request without max_tokens cannot cross into these anthropic models until they declare maxOutputTokens: " +
		strings.Join(quoted, ", ") + ", 2 more models, not listed"
	c := apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionMaxOutputTokensUnset)
	if c == nil || c.Message != want {
		t.Errorf("MaxOutputTokensUnset = %+v\nwant message %q", c, want)
	}
}

// Many long priced degrade targets and many long anthropic models without a
// maxOutputTokens still let the status write through: both advisory
// conditions list their first ten entries and count the rest.
func TestModelProvider_LongAdvisoryListsFitStatus(t *testing.T) {
	const name = "mp-long-advisory"
	const child = name + "-claude"
	ids := longEntries("model-", "a")
	mkSecret(t, name+"-key")
	mkSecret(t, child+"-key")
	mkProvider(t, child, func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Type = kaalmv1beta1.ProviderTypeAnthropic
		for _, id := range ids {
			mp.Spec.Models = append(mp.Spec.Models, kaalmv1beta1.ModelProviderModel{ID: id})
		}
	})
	mkProvider(t, name, func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "cheap", CostPer1MInputTokens: "1", CostPer1MOutputTokens: "1"}}
		mp.Spec.Budget = hardBudgetSpec()
		for _, id := range ids {
			mp.Spec.Models = append(mp.Spec.Models,
				kaalmv1beta1.ModelProviderModel{ID: id, CostPer1MInputTokens: "10", CostPer1MOutputTokens: "10"})
			mp.Spec.Budget.Policies = append(mp.Spec.Budget.Policies, kaalmv1beta1.ModelProviderBudgetPolicy{
				AtPercent: 50, Action: "degrade", DegradeTo: &id,
			})
		}
		mp.Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: child}}
	})
	eventually(t, func() error {
		var mp kaalmv1beta1.ModelProvider
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: name}, &mp); err != nil {
			return err
		}
		ready := condition(mp.Status.Conditions, kaalmv1beta1.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue || mp.Status.ObservedGeneration != mp.Generation {
			return fmt.Errorf("Ready = %+v, want True at generation %d", ready, mp.Generation)
		}
		for condType, tail := range map[string]string{
			kaalmv1beta1.ConditionDegradeTargetNotCheapest: "degradeTo has 2 more targets that cost more than the cheapest model, not listed",
			kaalmv1beta1.ConditionMaxOutputTokensUnset:     "2 more models, not listed",
		} {
			c := condition(mp.Status.Conditions, condType)
			if c == nil || c.Status != metav1.ConditionTrue {
				return fmt.Errorf("%s = %+v, want True", condType, c)
			}
			if len(c.Message) > conditionMessageLimit {
				return fmt.Errorf("%s message is %d bytes", condType, len(c.Message))
			}
			if !strings.HasSuffix(c.Message, tail) {
				return fmt.Errorf("%s message does not end in %q", condType, tail)
			}
		}
		return nil
	})
}
