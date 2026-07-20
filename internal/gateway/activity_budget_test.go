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

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestActivityStore_SourcesAndSnapshot(t *testing.T) {
	s := NewActivityStore()
	base := time.Now()
	clock := base
	s.now = func() time.Time { return clock }

	s.RecordTraffic("team-a", "sup")
	clock = base.Add(time.Minute)
	s.RecordHeartbeat("team-a", "sup")
	s.RecordTraffic("team-b", "other")

	snap := s.Snapshot("team-a")
	if len(snap.Agents) != 1 {
		t.Fatalf("namespace isolation broken: %v", snap.Agents)
	}
	sup := snap.Agents["sup"]
	if sup.GatewayTraffic == nil || !sup.GatewayTraffic.Equal(base) {
		t.Errorf("gatewayTraffic wrong: %v", sup.GatewayTraffic)
	}
	if sup.Heartbeat == nil || !sup.Heartbeat.Equal(base.Add(time.Minute)) {
		t.Errorf("heartbeat wrong: %v", sup.Heartbeat)
	}
	// An agent with only traffic has a null heartbeat.
	other := s.Snapshot("team-b").Agents["other"]
	if other.Heartbeat != nil {
		t.Error("unseen source must be null")
	}
	if snap.ReplicaStartedAt.IsZero() {
		t.Error("replicaStartedAt must be stamped")
	}
}

func TestHeartbeatFeedsActivityEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	agentC := agentCert(t, h.ca)
	controllerCert := h.ca.issue(t, "agentry-controller.agentry-system.svc.cluster.local")

	// Agent heartbeats; controller reads it back with both sources present.
	resp := postJSON(t, h.client(&agentC), h.url("/v1/agent/heartbeat"), map[string]any{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat status %d", resp.StatusCode)
	}

	got, err := h.client(&controllerCert).Get(h.url("/v1/activity?namespace=team-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Body.Close() }()
	raw, _ := io.ReadAll(got.Body)
	var payload struct {
		ReplicaStartedAt time.Time `json:"replicaStartedAt"`
		Agents           map[string]struct {
			GatewayTraffic *time.Time `json:"gatewayTraffic"`
			Heartbeat      *time.Time `json:"heartbeat"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("bad activity payload: %v (%s)", err, raw)
	}
	sup, ok := payload.Agents["sup"]
	if !ok || sup.Heartbeat == nil {
		t.Fatalf("heartbeat not visible in activity: %s", raw)
	}
	if sup.GatewayTraffic != nil {
		t.Error("no traffic was recorded; gatewayTraffic must be null")
	}
	if payload.ReplicaStartedAt.IsZero() {
		t.Error("replicaStartedAt missing")
	}
}

const smallModel = "small"

func budgetProvider(policies ...agentryv1alpha1.ModelProviderBudgetPolicy) *agentryv1alpha1.ModelProvider {
	return &agentryv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prov"},
		Spec: agentryv1alpha1.ModelProviderSpec{
			Budget: agentryv1alpha1.ModelProviderBudget{
				Period:          "monthly",
				PerNamespaceUSD: "100",
				Policies:        policies,
			},
			Models: []agentryv1alpha1.ModelProviderModel{
				{ID: "big", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "30"},
				{ID: smallModel, CostPer1MInputTokens: "1", CostPer1MOutputTokens: "3"},
			},
		},
	}
}

func TestPeriodKey(t *testing.T) {
	at := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	if got := PeriodKey("monthly", at); got != "2026-07" {
		t.Errorf("monthly = %q", got)
	}
	if got := PeriodKey("daily", at); got != "2026-07-19" {
		t.Errorf("daily = %q", got)
	}
	if got := PeriodKey("none", at); got != "" {
		t.Errorf("none = %q", got)
	}
	if got := PeriodKey("weekly", at); got != "2026-W29" {
		t.Errorf("weekly = %q", got)
	}
}

func TestCostOf(t *testing.T) {
	p := budgetProvider()
	// 1M input at $10 + 500k output at $30 = 10 + 15 = 25.
	got := costOf(p, "big", Usage{InputTokens: 1_000_000, OutputTokens: 500_000})
	if got != 25 {
		t.Errorf("cost = %v, want 25", got)
	}
	if costOf(p, "unknown", Usage{InputTokens: 100}) != 0 {
		t.Error("unknown model must cost zero")
	}
}

func TestBudgetLedger_EnforceThresholds(t *testing.T) {
	degradeTo := smallModel
	p := budgetProvider(
		agentryv1alpha1.ModelProviderBudgetPolicy{AtPercent: 50, Action: "warn"},
		agentryv1alpha1.ModelProviderBudgetPolicy{AtPercent: 80, Action: "degrade", DegradeTo: &degradeTo},
		agentryv1alpha1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"},
	)
	b := NewBudgetLedger()

	if d := b.Enforce(p, "team-a"); d.Action != "" {
		t.Errorf("no spend must be no action, got %q", d.Action)
	}
	b.Add(p, "team-a", 60) // 60%
	if d := b.Enforce(p, "team-a"); d.Action != agentryv1alpha1.BudgetActionWarn {
		t.Errorf("60%% should warn, got %q", d.Action)
	}
	b.Add(p, "team-a", 25) // 85%
	if d := b.Enforce(p, "team-a"); d.Action != agentryv1alpha1.BudgetActionDegrade || d.DegradeTo != smallModel {
		t.Errorf("85%% should degrade to small, got %+v", d)
	}
	b.Add(p, "team-a", 20) // 105%
	d := b.Enforce(p, "team-a")
	if d.Action != agentryv1alpha1.BudgetActionBlock || d.RetryAfter <= 0 {
		t.Errorf("105%% should block with Retry-After, got %+v", d)
	}
	// Another namespace is unaffected by per-namespace ceilings.
	if d := b.Enforce(p, "team-b"); d.Action != "" {
		t.Errorf("team-b should be clean, got %q", d.Action)
	}
}

func TestBudgetLedger_PeerFoldAndPartials(t *testing.T) {
	p := budgetProvider(agentryv1alpha1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	b := NewBudgetLedger()
	b.Add(p, "team-a", 40)
	// Peer partials push the enforcement view over the ceiling.
	b.FoldPeers(p, map[string]float64{"team-a": 70})
	if d := b.Enforce(p, "team-a"); d.Action != agentryv1alpha1.BudgetActionBlock {
		t.Errorf("own 40 + peers 70 = 110%% should block, got %+v", d)
	}

	period, spend, ok := b.OwnPartial("prov")
	if !ok || spend["team-a"] != "40.00" {
		t.Errorf("own partial wrong: %v %v %v", period, spend, ok)
	}
	if period != PeriodKey("monthly", time.Now()) {
		t.Errorf("partial period wrong: %q", period)
	}
}

func TestBudgetLedger_PeriodRollover(t *testing.T) {
	p := budgetProvider(agentryv1alpha1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	b := NewBudgetLedger()
	now := time.Date(2026, 7, 31, 23, 59, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	b.Add(p, "team-a", 150)
	if d := b.Enforce(p, "team-a"); d.Action != agentryv1alpha1.BudgetActionBlock {
		t.Fatal("should block at 150%")
	}
	// Midnight UTC: the counter resets on the next touch.
	now = time.Date(2026, 8, 1, 0, 1, 0, 0, time.UTC)
	if d := b.Enforce(p, "team-a"); d.Action != "" {
		t.Errorf("new period must reset spend, got %+v", d)
	}
}

func TestParseBudgetPartialRoundTrip(t *testing.T) {
	raw, err := json.Marshal(budgetPartial{Period: "2026-07", Spend: map[string]string{"team-a": "12.34"}})
	if err != nil {
		t.Fatal(err)
	}
	period, spend, err := ParseBudgetPartial(string(raw))
	if err != nil || period != "2026-07" || spend["team-a"] != 12.34 {
		t.Errorf("round trip failed: %q %v %v", period, spend, err)
	}
}

func TestProxy_BudgetDegradeAndBlock(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	h.seedRoute()
	degradeTo := smallModel
	h.store.providers["prov"].Spec.Budget = agentryv1alpha1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "100",
		Policies: []agentryv1alpha1.ModelProviderBudgetPolicy{
			{AtPercent: 80, Action: "degrade", DegradeTo: &degradeTo},
			{AtPercent: 100, Action: "block"},
		},
	}
	h.store.providers["prov"].Spec.Models = []agentryv1alpha1.ModelProviderModel{
		{ID: "m1", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "30"},
		{ID: smallModel, CostPer1MInputTokens: "1", CostPer1MOutputTokens: "3"},
	}
	cert := agentCert(t, h.ca)

	// 85% spent: the request goes through with the model rewritten.
	h.server.Budget.Add(h.store.providers["prov"], "team-a", 85)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("degraded call should succeed, got %d", resp.StatusCode)
	}
	up := <-h.upreqs
	if up.body["model"] != "small" {
		t.Errorf("degrade must rewrite the model, upstream saw %v", up.body["model"])
	}

	// 105%: blocked with Retry-After.
	h.server.Budget.Add(h.store.providers["prov"], "team-a", 20)
	resp = postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked call should be 429, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("block must carry Retry-After")
	}
	if got := errType(t, resp); got != "budget_exhausted" {
		t.Errorf("error type %q", got)
	}
}
