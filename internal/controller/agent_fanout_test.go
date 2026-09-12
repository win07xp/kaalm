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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestProviderChangeMatters(t *testing.T) {
	usage := func(ns, state string) kaalmv1beta1.ModelProviderBudgetUsage {
		return kaalmv1beta1.ModelProviderBudgetUsage{Namespace: ns, Period: "2026-09", SpentUSD: "1.00", State: state}
	}
	mp := func(gen int64, usages ...kaalmv1beta1.ModelProviderBudgetUsage) *kaalmv1beta1.ModelProvider {
		return &kaalmv1beta1.ModelProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "prov", Generation: gen},
			Status:     kaalmv1beta1.ModelProviderStatus{BudgetUsage: usages},
		}
	}
	spent := mp(1, usage("team-a", kaalmv1beta1.BudgetStateNormal))
	spentMore := mp(1, usage("team-a", kaalmv1beta1.BudgetStateNormal))
	spentMore.Status.BudgetUsage[0].SpentUSD = "2.00"
	spentMore.Status.BudgetUsage[0].PercentUsed = 40
	cases := []struct {
		name     string
		old, new *kaalmv1beta1.ModelProvider
		want     bool
	}{
		{"spec change fans out", mp(1), mp(2), true},
		{"spend counters alone do not", spent, spentMore, false},
		{"a namespace becoming blocked does", spent, mp(1, usage("team-a", kaalmv1beta1.BudgetStateBlocked)), true},
		{"a namespace unblocking does", mp(1, usage("team-a", kaalmv1beta1.BudgetStateBlocked)), spent, true},
		{"blocked set in another order does not", mp(1, usage("a", kaalmv1beta1.BudgetStateBlocked), usage("b", kaalmv1beta1.BudgetStateBlocked)),
			mp(1, usage("b", kaalmv1beta1.BudgetStateBlocked), usage("a", kaalmv1beta1.BudgetStateBlocked)), false},
		{"health condition alone does not", spent, func() *kaalmv1beta1.ModelProvider {
			m := mp(1, usage("team-a", kaalmv1beta1.BudgetStateNormal))
			m.Status.Conditions = []metav1.Condition{{Type: kaalmv1beta1.ConditionHealthy, Status: metav1.ConditionFalse}}
			return m
		}(), false},
	}
	p := providerChangeMatters()
	for _, c := range cases {
		if got := p.Update(event.UpdateEvent{ObjectOld: c.old, ObjectNew: c.new}); got != c.want {
			t.Errorf("%s: fan out = %v, want %v", c.name, got, c.want)
		}
	}
	// Creates and deletes always pass: a new or removed provider changes what
	// every referencing Agent can resolve.
	if !p.Create(event.CreateEvent{Object: spent}) || !p.Delete(event.DeleteEvent{Object: spent}) {
		t.Error("create and delete events must fan out")
	}
}
