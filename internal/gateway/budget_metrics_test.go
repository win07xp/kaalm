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

package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

func TestBudgetLedger_Utilization(t *testing.T) {
	mp := budgetProvider()
	b, _ := fakeClockLedger(mp)
	b.Add(mp, "team-a", "agent/x", 15)
	b.FoldPeers(mp, map[string]float64{"team-a": 30, "team-b": 10})

	period, ratios := b.Utilization(mp)
	if period != "2099-06" {
		t.Fatalf("period = %q, want 2099-06", period)
	}
	if got := ratios["team-a"]; got != 0.45 {
		t.Errorf("team-a = %v, want 0.45 (own 15 + peers 30 over a 100 ceiling)", got)
	}
	if got := ratios["team-b"]; got != 0.10 {
		t.Errorf("team-b = %v, want 0.10 (peers only)", got)
	}
	if len(ratios) != 2 {
		t.Errorf("ratios = %v, want exactly the two namespaces with spend", ratios)
	}

	uncapped := budgetProvider()
	uncapped.Name = "open"
	uncapped.Spec.Budget.PerNamespaceUSD = ""
	b.Add(uncapped, "team-a", "agent/x", 5)
	if period, ratios := b.Utilization(uncapped); period != "" || ratios != nil {
		t.Errorf("no ceiling: got period %q ratios %v, want nothing to report", period, ratios)
	}
}

func TestBudgetUtilizationCollector_ServesTheGauge(t *testing.T) {
	mp := budgetProvider()
	b, _ := fakeClockLedger(mp)
	b.Add(mp, "team-a", "agent/x", 45)
	c := &BudgetUtilizationCollector{
		Ledger:    b,
		Providers: func(context.Context) []*kaalmv1alpha1.ModelProvider { return []*kaalmv1alpha1.ModelProvider{mp} },
	}
	expected := `
# HELP kaalm_llm_budget_utilization Per-namespace budget utilization as a 0-1 ratio of the per-namespace ceiling, from the replica's folded ledger.
# TYPE kaalm_llm_budget_utilization gauge
kaalm_llm_budget_utilization{namespace="team-a",period="2099-06",provider="prov"} 0.45
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}
