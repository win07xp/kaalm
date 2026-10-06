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
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// The previous period's rows stay in status after the rollover pass, until
// the next rollover, and the archive lives in the ConfigMap so a later pass
// renders it again.
func TestReconcileBudget_PreviousPeriodOutlivesRolloverPass(t *testing.T) {
	current := gateway.PeriodKey("monthly", time.Now())
	h := newArchiveHarness(t, map[string]string{"gw-0": partialJSON("1999-01", "team-a", "10.00")}, nil)
	h.pass("gw-0")
	h.setKey("gw-0", partialJSON(current, "team-a", "1.00"))
	h.pass("gw-0")
	rows := h.rowsFor("1999-01")
	if len(rows) != 1 || rows[0].SpentUSD != "10.00" {
		t.Errorf("1999-01 rows after the second pass = %+v, want team-a 10.00", rows)
	}
	if _, ok := h.cm().Data[gateway.PreviousKey]; !ok {
		t.Error("_previous missing from the ConfigMap")
	}
}

// A replica's final old-period publish, arriving after the archive was made,
// is a newer snapshot of the same counter: it replaces that source's figure
// instead of adding to it.
func TestReconcileBudget_LatePartialJoinsArchive(t *testing.T) {
	h := newArchiveHarness(t, map[string]string{
		"gw-0": partialJSON("1999-01", "team-a", "10.00"),
		"gw-1": partialJSON("1999-01", "team-a", "5.00"),
	}, nil)
	h.pass("gw-0", "gw-1")
	if rows := h.rowsFor("1999-01"); len(rows) != 1 || rows[0].SpentUSD != "15.00" {
		t.Fatalf("first archive = %+v, want 15.00", rows)
	}
	h.setKey("gw-1", partialJSON("1999-01", "team-a", "7.00"))
	h.pass("gw-0", "gw-1")
	if rows := h.rowsFor("1999-01"); len(rows) != 1 || rows[0].SpentUSD != "17.00" {
		t.Fatalf("after gw-1's late publish = %+v, want 17.00", rows)
	}
	h.setKey("gw-0", partialJSON("1999-01", "team-a", "12.00"))
	h.pass("gw-0", "gw-1")
	if rows := h.rowsFor("1999-01"); len(rows) != 1 || rows[0].SpentUSD != "19.00" {
		t.Fatalf("after gw-0's late publish = %+v, want 19.00", rows)
	}
}

// A previous-period row is never Blocked: the gateway enforces only the
// current period, and Agents read Blocked as blocked now.
func TestReconcileBudget_PreviousPeriodNeverBlocks(t *testing.T) {
	current := gateway.PeriodKey("monthly", time.Now())
	h := newArchiveHarness(t, map[string]string{
		"gw-dead": partialJSON("1999-01", "team-a", "95.00"),
		"gw-0":    partialJSON(current, "team-a", "90.00"),
	}, func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.Budget.Policies = []kaalmv1beta1.ModelProviderBudgetPolicy{{AtPercent: 80, Action: "block"}}
	})
	h.pass("gw-0")
	if rows := h.rowsFor(current); len(rows) != 1 || rows[0].State != kaalmv1beta1.BudgetStateBlocked {
		t.Errorf("current rows = %+v, want team-a Blocked", rows)
	}
	prev := h.rowsFor("1999-01")
	if len(prev) != 1 || prev[0].State != kaalmv1beta1.BudgetStateNormal || prev[0].PercentUsed != 95 {
		t.Errorf("previous rows = %+v, want team-a Normal at 95%%", prev)
	}
}

// The archive lasts from the rollover that made it until the next one.
func TestReconcileBudget_ArchiveReplacedAtNextRollover(t *testing.T) {
	current := gateway.PeriodKey("monthly", time.Now())
	archive := func(period, archivedIn, uid string) string {
		return fmt.Sprintf(`{"period":%q,"archivedIn":%q,"providerUID":%q,"sources":{"gw-old":{"team-a":"4.00"}}}`,
			period, archivedIn, uid)
	}
	cases := []struct {
		name       string
		data       map[string]string
		uid        string
		wantPeriod string // "" means no archive
		wantSpent  string
	}{{
		name: "next rollover with no stale keys drops it",
		data: map[string]string{gateway.PreviousKey: archive("1999-02", "1999-03", "")},
	}, {
		name: "next rollover replaces it",
		data: map[string]string{
			gateway.PreviousKey: archive("1999-02", "1999-03", ""),
			"gw-0":              partialJSON("1999-03", "team-a", "9.00"),
		},
		wantPeriod: "1999-03", wantSpent: "9.00",
	}, {
		name: "an older stale key is dropped, not archived",
		data: map[string]string{
			gateway.PreviousKey: archive("1999-02", current, ""),
			"gw-0":              partialJSON("1999-01", "team-a", "9.00"),
		},
		wantPeriod: "1999-02", wantSpent: "4.00",
	}, {
		name: "a newer stale key replaces it",
		data: map[string]string{
			gateway.PreviousKey: archive("1999-01", current, ""),
			"gw-0":              partialJSON("1999-02", "team-a", "9.00"),
		},
		wantPeriod: "1999-02", wantSpent: "9.00",
	}, {
		name: "an archive for another provider UID is dropped",
		data: map[string]string{gateway.PreviousKey: archive("1999-02", current, "old-uid")},
		uid:  "new-uid",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArchiveHarness(t, tc.data, func(mp *kaalmv1beta1.ModelProvider) { mp.UID = types.UID(tc.uid) })
			h.pass("gw-0")
			cm := h.cm()
			raw, ok := cm.Data[gateway.PreviousKey]
			prev := h.previousRows()
			if tc.wantPeriod == "" {
				if ok || len(prev) != 0 {
					t.Errorf("_previous = %q, previous rows = %+v; want neither", raw, prev)
				}
				return
			}
			if !ok {
				t.Fatal("_previous missing")
			}
			if len(prev) != 1 || prev[0].Period != tc.wantPeriod || prev[0].SpentUSD != tc.wantSpent {
				t.Errorf("previous rows = %+v, want %s at %s", prev, tc.wantPeriod, tc.wantSpent)
			}
			if _, stale := cm.Data["gw-0"]; stale {
				t.Error("stale key gw-0 not deleted")
			}
		})
	}
}

// A pass with nothing new to archive writes nothing.
func TestReconcileBudget_ArchiveSteadyPassWritesNothing(t *testing.T) {
	current := gateway.PeriodKey("monthly", time.Now())
	h := newArchiveHarness(t, map[string]string{
		"gw-0":    partialJSON(current, "team-a", "1.00"),
		"gw-dead": partialJSON("1999-01", "team-a", "10.00"),
	}, nil)
	h.pass("gw-0")
	before := h.cm().ResourceVersion
	usage := append([]kaalmv1beta1.ModelProviderBudgetUsage(nil), h.mp.Status.BudgetUsage...)
	h.pass("gw-0")
	if after := h.cm().ResourceVersion; after != before {
		t.Errorf("steady pass wrote the ConfigMap: resourceVersion %s -> %s", before, after)
	}
	if !equality.Semantic.DeepEqual(usage, h.mp.Status.BudgetUsage) {
		t.Errorf("steady pass changed budgetUsage: %+v -> %+v", usage, h.mp.Status.BudgetUsage)
	}
}
