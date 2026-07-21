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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

	m.BudgetThreshold("prov", "team-a", "block")
	if got := testutil.ToFloat64(m.budgetThreshld.WithLabelValues("prov", "team-a", "block")); got != 1 {
		t.Errorf("budgetThreshold = %v, want 1", got)
	}

	m.ChannelMessage("team-a", "ok")
	if got := testutil.ToFloat64(m.channelMsgs.WithLabelValues("webhook", "team-a", "ok")); got != 1 {
		t.Errorf("channelMessage = %v, want 1", got)
	}

	m.ChannelWake("team-a")
	if got := testutil.ToFloat64(m.channelWake.WithLabelValues("team-a")); got != 1 {
		t.Errorf("channelWake = %v, want 1", got)
	}

	m.ChannelCallback("team-a", "delivered")
	if got := testutil.ToFloat64(m.channelCB.WithLabelValues("team-a", "delivered")); got != 1 {
		t.Errorf("channelCallback = %v, want 1", got)
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
	m.BudgetThreshold("p", "ns", "warn")
	m.ChannelMessage("ns", "ok")
	m.ChannelWake("ns")
	m.ChannelCallback("ns", "ok")
	m.ResponseTooLarge("ns", "async")
	m.AsyncPatchFailed("ns")
}
