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
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestWorkloadKey(t *testing.T) {
	if got := workloadKey(&caller{Namespace: "team-a"}); got != UnattributedWorkload {
		t.Errorf("token caller = %q, want the unattributed bucket", got)
	}
	agent := &caller{Namespace: "team-a", Workload: &Identity{Namespace: "team-a", Name: "sup", Kind: KindAgent}}
	if got := workloadKey(agent); got != "agent/sup" {
		t.Errorf("agent caller = %q", got)
	}
	task := &caller{Namespace: "team-a", Workload: &Identity{Namespace: "team-a", Name: "fix-42", Kind: KindAgentTask}}
	if got := workloadKey(task); got != "task/fix-42" {
		t.Errorf("task caller = %q", got)
	}
}

func TestBudgetLedger_WorkloadAccounting(t *testing.T) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	b := NewBudgetLedger()

	b.Add(p, "team-a", "agent/sup", 3)
	b.Add(p, "team-a", "agent/sup", 2)
	b.Add(p, "team-a", "task/fix-42", 1)
	b.Add(p, "team-a", UnattributedWorkload, 4)
	b.Add(p, "team-b", "agent/other", 9)

	rows := b.WorkloadSpend("team-a")
	got, ok := rows["prov"]
	if !ok {
		t.Fatalf("WorkloadSpend = %+v, want prov", rows)
	}
	if got.Workloads["agent/sup"] != "5.00" || got.Workloads["task/fix-42"] != "1.00" ||
		got.Workloads[UnattributedWorkload] != "4.00" {
		t.Errorf("workloads = %+v", got.Workloads)
	}
	if _, leaked := got.Workloads["agent/other"]; leaked {
		t.Error("another namespace's workload leaked into the view")
	}

	// Peers fold into the same view: any single replica serves the union.
	b.FoldWorkloadPeers(p, map[string]float64{"team-a/agent/sup": 1.5, "team-b/agent/other": 7})
	if got := b.WorkloadSpend("team-a")["prov"].Workloads["agent/sup"]; got != "6.50" {
		t.Errorf("own+peers = %q, want 6.50", got)
	}

	// The per-workload rows sum to the namespace enforcement figure.
	b.mu.Lock()
	nsSpent := b.providers["prov"].spent("team-a")
	b.mu.Unlock()
	if nsSpent != 10 { // own only: peers map for enforcement was not folded here
		t.Errorf("namespace spent = %v", nsSpent)
	}
}

func TestBudgetLedger_WorkloadRolloverClears(t *testing.T) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	b := NewBudgetLedger()
	now := time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }

	b.Add(p, "team-a", "agent/sup", 5)
	b.FoldWorkloadPeers(p, map[string]float64{"team-a/agent/sup": 5})
	if b.WorkloadSpend("team-a")["prov"].Workloads["agent/sup"] != "10.00" {
		t.Fatal("pre-rollover view wrong")
	}

	// Cross the month boundary: the workload maps reset with the period.
	now = now.Add(2 * time.Hour)
	if rows := b.WorkloadSpend("team-a"); len(rows) != 0 {
		// ledgerFor is invoked lazily; WorkloadSpend does not roll over by
		// itself, so force it through the accounting path.
		b.Add(p, "team-a", "agent/sup", 1)
		if got := b.WorkloadSpend("team-a")["prov"].Workloads["agent/sup"]; got != "1.00" {
			t.Errorf("post-rollover = %q, want 1.00 (old period must clear)", got)
		}
	}
}

func TestBudgetLedger_HardSettleLandsWorkload(t *testing.T) {
	p := hardProvider(blockAt100())
	b, _ := fakeClockLedger(p)

	b.Add(p, "team-a", "agent/sup", 96) // inside the boundary region
	_, settle := b.Admit(p, "team-a", "agent/sup")
	if settle == nil {
		t.Fatal("expected a boundary settle")
	}
	settle(2)
	if got := b.WorkloadSpend("team-a")["prov"].Workloads["agent/sup"]; got != "98.00" {
		t.Errorf("hard settle workload spend = %q, want 98.00", got)
	}
}

func TestBudgetPublisher_WorkloadExchange(t *testing.T) {
	ctx := context.Background()
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})

	client := k8sfake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: BudgetConfigMapName("prov"), Namespace: "kaalm-system"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: AgentSpendConfigMapName("prov"), Namespace: "kaalm-system"}},
	)
	ledger := NewBudgetLedger()
	ledger.Add(p, "team-a", "agent/sup", 12.5)

	pub := &BudgetPublisher{
		Client: client, Ledger: ledger,
		OperatorNamespace: "kaalm-system", PodName: "gw-0",
		Providers: providersFn(p),
	}

	// publish writes the workload partial beside the budget partial.
	pub.publish(ctx, p)
	cm, err := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, AgentSpendConfigMapName("prov"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("agent-spend ConfigMap missing: %v", err)
	}
	period, spend, _, err := ParseBudgetPartial(cm.Data["gw-0"])
	if err != nil || period == "" || spend["team-a/agent/sup"] != 12.5 {
		t.Fatalf("workload partial = period %q spend %+v err %v", period, spend, err)
	}

	// The budget ConfigMap must NOT carry workload keys: the enforcement
	// fold sums every non-underscore key as namespace spend.
	bcm, _ := client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, BudgetConfigMapName("prov"), metav1.GetOptions{})
	_, bspend, _, _ := ParseBudgetPartial(bcm.Data["gw-0"])
	if _, leaked := bspend["team-a/agent/sup"]; leaked {
		t.Fatal("workload key leaked into the budget partial")
	}
	if bspend["team-a"] != 12.5 {
		t.Errorf("budget partial namespace spend = %+v", bspend)
	}

	// A peer's partial folds into this replica's served view.
	peerRaw, _ := json.Marshal(budgetPartial{Period: period, Spend: map[string]string{"team-a/agent/sup": "2.50"}})
	cm.Data["gw-1"] = string(peerRaw)
	_, _ = client.CoreV1().ConfigMaps("kaalm-system").Update(ctx, cm, metav1.UpdateOptions{})
	pub.foldWorkloads(ctx, p)
	if got := ledger.WorkloadSpend("team-a")["prov"].Workloads["agent/sup"]; got != "15.00" {
		t.Errorf("folded view = %q, want 15.00", got)
	}

	// Seeding from _canonical restores the view on a fresh replica.
	cm, _ = client.CoreV1().ConfigMaps("kaalm-system").Get(ctx, AgentSpendConfigMapName("prov"), metav1.GetOptions{})
	cm.Data[CanonicalKey] = `{"team-a/agent/sup":"7.25"}`
	_, _ = client.CoreV1().ConfigMaps("kaalm-system").Update(ctx, cm, metav1.UpdateOptions{})
	fresh := NewBudgetLedger()
	pub2 := &BudgetPublisher{Client: client, Ledger: fresh,
		OperatorNamespace: "kaalm-system", PodName: "gw-9", Providers: providersFn(p)}
	pub2.SeedFromCanonical(ctx)
	if got := fresh.WorkloadSpend("team-a")["prov"].Workloads["agent/sup"]; got != "7.25" {
		t.Errorf("seeded view = %q, want 7.25", got)
	}
}

func TestFoldAgentSpendConfigMapEvent(t *testing.T) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	store := newFakeStore()
	store.providers["prov"] = p
	ledger := NewBudgetLedger()

	raw, _ := json.Marshal(budgetPartial{
		Period: PeriodKey("monthly", time.Now()),
		Spend:  map[string]string{"team-a/agent/sup": "3.00"},
	})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentSpendConfigMapName("prov"), Namespace: "kaalm-system"},
		Data:       map[string]string{"gw-1": string(raw)},
	}
	FoldBudgetConfigMapEvent(context.Background(), cm, "gw-0", store, ledger)
	if got := ledger.WorkloadSpend("team-a")["prov"].Workloads["agent/sup"]; got != "3.00" {
		t.Errorf("watch fold = %q, want 3.00", got)
	}

	// The workload fold never enters the enforcement peer view.
	ledger.mu.Lock()
	peers := ledger.providers["prov"].peers
	ledger.mu.Unlock()
	if len(peers) != 0 {
		t.Errorf("enforcement peers polluted: %+v", peers)
	}
}

// TestSpendEndpoint_EndToEnd drives a priced LLM call through the proxy with
// an attested agent identity and reads the per-workload breakdown back
// through GET /v1/spend with the console identity.
func TestSpendEndpoint_EndToEnd(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`))
	})
	h.seedRoute()
	h.store.providers["prov"].Spec.Budget = kaalmv1beta1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "100",
	}
	h.store.providers["prov"].Spec.Models = []kaalmv1beta1.ModelProviderModel{
		{ID: "m1", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "30"},
	}
	agentC := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&agentC), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("LLM call = %d", resp.StatusCode)
	}

	consoleCert := h.ca.issue(t, "kaalm-console.kaalm-system.svc.cluster.local")
	c := h.client(&consoleCert)
	get, err := c.Get(h.url("/v1/spend?namespace=team-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = get.Body.Close() }()
	if get.StatusCode != 200 {
		body, _ := io.ReadAll(get.Body)
		t.Fatalf("spend read = %d (%s)", get.StatusCode, body)
	}
	var out struct {
		Providers map[string]struct {
			Period    string            `json:"period"`
			Workloads map[string]string `json:"workloads"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(get.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	prov, ok := out.Providers["prov"]
	if !ok || prov.Period == "" {
		t.Fatalf("spend response = %+v", out)
	}
	if got := prov.Workloads["agent/sup"]; got != "10.00" {
		t.Errorf("agent/sup spend = %q, want 10.00 (1M input tokens at $10/1M)", got)
	}

	// Missing namespace: past auth, into the handler's 400.
	bad, err := c.Get(h.url("/v1/spend"))
	if err != nil {
		t.Fatal(err)
	}
	_ = bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Errorf("missing namespace = %d, want 400", bad.StatusCode)
	}
}
