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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

func TestMetrics_CountersAndHistogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}

	m.LLMRequest("prov", "m1", "team-a", "ok")
	m.LLMRequest("prov", "m1", "team-a", "ok")
	if got := testutil.ToFloat64(m.llmRequests.WithLabelValues("prov", "m1", "team-a", "ok")); got != 2 {
		t.Errorf("llmRequests = %v, want 2", got)
	}

	m.Duration("prov", "m1", 0.5)
	if n := testutil.CollectAndCount(m.llmDuration); n == 0 {
		t.Error("Duration histogram recorded nothing")
	}

	m.Tokens("prov", "m1", "team-a", Usage{InputTokens: 10, OutputTokens: 3})
	if got := testutil.ToFloat64(m.llmTokens.WithLabelValues("prov", "m1", "team-a", "input")); got != 10 {
		t.Errorf("input tokens = %v, want 10", got)
	}
	if got := testutil.ToFloat64(m.llmTokens.WithLabelValues("prov", "m1", "team-a", "output")); got != 3 {
		t.Errorf("output tokens = %v, want 3", got)
	}

	m.Spend("prov", "team-a", 1.25)
	m.Spend("prov", "team-a", 0) // no-op path
	if got := testutil.ToFloat64(m.llmSpend.WithLabelValues("prov", "team-a")); got != 1.25 {
		t.Errorf("spend = %v, want 1.25", got)
	}

	m.Fallback("prov", "backup", "rate_limited")
	if got := testutil.ToFloat64(m.llmFallback.WithLabelValues("prov", "backup", "rate_limited")); got != 1 {
		t.Errorf("fallback = %v, want 1", got)
	}

	m.ServerToolUse("prov", "team-a", "web_search", 3)
	m.ServerToolUse("prov", "team-a", "web_search", 0)  // no-op path
	m.ServerToolUse("prov", "team-a", "web_search", -1) // no-op path
	if got := testutil.ToFloat64(m.llmServerTools.WithLabelValues("prov", "team-a", "web_search")); got != 3 {
		t.Errorf("serverToolUse = %v, want 3", got)
	}

	m.ToolCall("search", "team-a", "web_search", "ok")
	if got := testutil.ToFloat64(m.toolCalls.WithLabelValues("search", "team-a", "web_search", "ok")); got != 1 {
		t.Errorf("toolCalls = %v, want 1", got)
	}

	m.ToolCallDuration("search", "web_search", 0.2)
	if n := testutil.CollectAndCount(m.toolDuration); n == 0 {
		t.Error("ToolCallDuration histogram recorded nothing")
	}

	m.BudgetThreshold("prov", "team-a", "block")
	if got := testutil.ToFloat64(m.budgetThreshld.WithLabelValues("prov", "team-a", "block")); got != 1 {
		t.Errorf("budgetThreshold = %v, want 1", got)
	}

	m.ChannelMessage("webhook", "team-a", "delivered")
	if got := testutil.ToFloat64(m.channelMsgs.WithLabelValues("webhook", "team-a", "delivered")); got != 1 {
		t.Errorf("channelMessage = %v, want 1", got)
	}
	m.ChannelMessage("console", "team-a", "delivered")
	if got := testutil.ToFloat64(m.channelMsgs.WithLabelValues("console", "team-a", "delivered")); got != 1 {
		t.Errorf("channelMessage(console) = %v, want 1", got)
	}

	m.ChannelMessageDuration("webhook", 0.3)
	if n := testutil.CollectAndCount(m.channelMsgDur); n == 0 {
		t.Error("ChannelMessageDuration histogram recorded nothing")
	}

	m.ChannelWake("team-a")
	if got := testutil.ToFloat64(m.channelWake.WithLabelValues("team-a")); got != 1 {
		t.Errorf("channelWake = %v, want 1", got)
	}

	m.ChannelWakeDuration("team-a", "ready", 1.5)
	if n := testutil.CollectAndCount(m.channelWakeDur); n == 0 {
		t.Error("ChannelWakeDuration histogram recorded nothing")
	}

	m.ChannelCallback("team-a", "delivered")
	if got := testutil.ToFloat64(m.channelCB.WithLabelValues("team-a", "delivered")); got != 1 {
		t.Errorf("channelCallback = %v, want 1", got)
	}

	m.ChannelCallbackDuration("team-a", 0.4)
	if n := testutil.CollectAndCount(m.channelCBDur); n == 0 {
		t.Error("ChannelCallbackDuration histogram recorded nothing")
	}

	m.ResponseTooLarge("team-a", "sync")
	if got := testutil.ToFloat64(m.tooLarge.WithLabelValues("team-a", "sync")); got != 1 {
		t.Errorf("responseTooLarge = %v, want 1", got)
	}

	m.AsyncPatchFailed("team-a")
	if got := testutil.ToFloat64(m.patchFailed.WithLabelValues("team-a")); got != 1 {
		t.Errorf("asyncPatchFailed = %v, want 1", got)
	}
}

// A nil *Metrics no-ops every method so tests need no registry.
func TestMetrics_NilReceiverNoOps(t *testing.T) {
	var m *Metrics
	m.LLMRequest("p", "m", "ns", "ok")
	m.Duration("p", "m", 1)
	m.Tokens("p", "m", "ns", Usage{InputTokens: 1})
	m.Spend("p", "ns", 1)
	m.Fallback("a", "b", "r")
	m.ServerToolUse("p", "ns", "web_search", 1)
	m.ToolCall("p", "ns", "web_search", "ok")
	m.ToolCallDuration("p", "web_search", 0.1)
	m.BudgetThreshold("p", "ns", "warn")
	m.ChannelMessage("webhook", "ns", "ok")
	m.ChannelMessageDuration("webhook", 0.1)
	m.ChannelWake("ns")
	m.ChannelWakeDuration("ns", "ready", 0.1)
	m.ChannelCallback("ns", "ok")
	m.ChannelCallbackDuration("ns", 0.1)
	m.ResponseTooLarge("ns", "async")
	m.AsyncPatchFailed("ns")
}

// TestGatewayCatalog_EveryDocumentedMetricIsRegistered pins the observability
// page's aggregated catalog (the spec) to the gateway registry: every row
// sourced to the LLM Gateway, the tool broker, or the User Gateway must be a
// registered name. This is the check that would have caught
// kaalm_llm_budget_utilization, documented in v0.1 and implemented in v0.5.
func TestGatewayCatalog_EveryDocumentedMetricIsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	reg.MustRegister(&BudgetUtilizationCollector{
		Ledger:    NewBudgetLedger(),
		Providers: func(context.Context) []*kaalmv1alpha1.ModelProvider { return nil },
	})
	for _, source := range []string{"LLM Gateway", "Tool broker", "User Gateway"} {
		for _, name := range catalogMetrics(t, source) {
			assertRegistered(t, reg, name)
		}
	}
}

// catalogMetrics reads the metric names of one Source column value from the
// aggregated catalog table in docs/src/operations/observability.md.
func catalogMetrics(t *testing.T, source string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "src", "operations", "observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "| "+source+" |") {
			continue
		}
		cells := strings.Split(line, "|")
		name := strings.Trim(strings.TrimSpace(cells[2]), "`")
		if strings.HasPrefix(name, "kaalm_") { // skip the Endpoints table's port rows
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatalf("no catalog rows for source %q", source)
	}
	return names
}

// assertRegistered proves a name is taken in reg by trying to register a
// probe under it: an already-registered descriptor (same or different shape)
// makes Register fail, so success means the catalog name is missing.
func assertRegistered(t *testing.T, reg *prometheus.Registry, name string) {
	t.Helper()
	probe := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "catalog probe"})
	if err := reg.Register(probe); err == nil {
		reg.Unregister(probe)
		t.Errorf("catalog metric %s is documented but not registered", name)
	}
}

// TestProxy_ObservesRequestDuration pins the latency histogram to the proxy
// path: it was registered since v0.1 and observed by nothing until the
// dashboards' live verification found no buckets on the wire.
func TestProxy_ObservesRequestDuration(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	h.seedRoute()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	h.server.Metrics = m

	agentC := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&agentC), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("LLM call = %d", resp.StatusCode)
	}
	if n := testutil.CollectAndCount(m.llmDuration); n != 1 {
		t.Fatalf("duration histogram has %d label sets after one forwarded call, want 1", n)
	}
	if got := testutil.ToFloat64(m.llmRequests.WithLabelValues("prov", "m1", "team-a", "ok")); got != 1 {
		t.Errorf("requests ok = %v, want 1", got)
	}
}
