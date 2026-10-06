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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// archiveHarness drives reconcileBudget directly over a fake client holding
// one budget ConfigMap.
type archiveHarness struct {
	t   *testing.T
	c   client.Client
	r   *ModelProviderReconciler
	mp  *kaalmv1beta1.ModelProvider
	key client.ObjectKey
}

func newArchiveHarness(t *testing.T, data map[string]string, mutate func(*kaalmv1beta1.ModelProvider)) *archiveHarness {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: gateway.BudgetConfigMapName("arch"), Namespace: testOperatorNamespace},
		Data:       data,
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cm).Build()
	mp := eventsProvider("arch", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Budget = kaalmv1beta1.ModelProviderBudget{Period: "monthly", PerNamespaceUSD: "100"}
		if mutate != nil {
			mutate(mp)
		}
	})
	return &archiveHarness{t: t, c: c, r: &ModelProviderReconciler{Client: c, OperatorNamespace: testOperatorNamespace},
		mp: mp, key: client.ObjectKeyFromObject(cm)}
}

// pass runs one reducer pass with the given live gateway Pods.
func (h *archiveHarness) pass(live ...string) {
	h.t.Helper()
	gws := map[string]bool{}
	for _, g := range live {
		gws[g] = true
	}
	if err := h.r.reconcileBudget(context.Background(), h.mp, gws); err != nil {
		h.t.Fatalf("reconcileBudget: %v", err)
	}
}

func (h *archiveHarness) cm() corev1.ConfigMap {
	h.t.Helper()
	var cm corev1.ConfigMap
	if err := h.c.Get(context.Background(), h.key, &cm); err != nil {
		h.t.Fatal(err)
	}
	return cm
}

// setKey writes one ConfigMap key, the way a replica's publish would.
func (h *archiveHarness) setKey(k, v string) {
	h.t.Helper()
	cm := h.cm()
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[k] = v
	if err := h.c.Update(context.Background(), &cm); err != nil {
		h.t.Fatal(err)
	}
}

// rowsFor returns the status rows of one period.
func (h *archiveHarness) rowsFor(period string) []kaalmv1beta1.ModelProviderBudgetUsage {
	var out []kaalmv1beta1.ModelProviderBudgetUsage
	for _, u := range h.mp.Status.BudgetUsage {
		if u.Period == period {
			out = append(out, u)
		}
	}
	return out
}

// previousRows returns the status rows of every period but the current one.
func (h *archiveHarness) previousRows() []kaalmv1beta1.ModelProviderBudgetUsage {
	current := gateway.PeriodKey("monthly", time.Now())
	var out []kaalmv1beta1.ModelProviderBudgetUsage
	for _, u := range h.mp.Status.BudgetUsage {
		if u.Period != current {
			out = append(out, u)
		}
	}
	return out
}

func partialJSON(period, ns, usd string) string {
	return fmt.Sprintf(`{"period":%q,%q:%q}`, period, ns, usd)
}

// Stale keys of two old periods (a controller down across two boundaries)
// archive only the newer period; the older keys are deleted unread.
func TestReconcileBudget_KeepsNewestStalePeriodOnly(t *testing.T) {
	h := newArchiveHarness(t, map[string]string{
		"gw-0":    partialJSON("1999-01", "team-z", "7.00"),
		"gw-dead": partialJSON("1999-02", "team-z", "3.00"),
	}, nil)
	h.pass("gw-0")
	prev := h.previousRows()
	if len(prev) != 1 || prev[0].Period != "1999-02" || prev[0].SpentUSD != "3.00" {
		t.Errorf("previous rows = %+v, want one 1999-02 row of 3.00", prev)
	}
	cm := h.cm()
	for _, k := range []string{"gw-0", "gw-dead"} {
		if _, ok := cm.Data[k]; ok {
			t.Errorf("stale key %s not deleted", k)
		}
	}
}
