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
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// hardenProv flips the harness's seeded provider to hard enforcement with a
// priced catalog (rule 33) and a block policy at 100.
func hardenProv(h *harness) *kaalmv1alpha1.ModelProvider {
	prov := h.store.providers["prov"]
	prov.Spec.Models = []kaalmv1alpha1.ModelProviderModel{
		{ID: "m1", CostPer1MInputTokens: "1.00", CostPer1MOutputTokens: "1.00"},
	}
	prov.Spec.Budget = kaalmv1alpha1.ModelProviderBudget{
		Period: "monthly", PerNamespaceUSD: "100",
		Enforcement: kaalmv1alpha1.BudgetEnforcementHard,
		Hard:        &kaalmv1alpha1.ModelProviderBudgetHard{BoundaryMarginPercent: 5},
		Policies: []kaalmv1alpha1.ModelProviderBudgetPolicy{
			{AtPercent: 100, Action: kaalmv1alpha1.BudgetActionBlock},
		},
	}
	return prov
}

func drainUpstream(h *harness) int {
	n := 0
	for {
		select {
		case <-h.upreqs:
			n++
		default:
			return n
		}
	}
}

// A throttled primary answers 429 budget_throttled immediately, with no
// fallback walk and no upstream call.
func TestProxyHard_ThrottledPrimary(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	})
	h.seedRoute()
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})
	if _, settle := h.server.Budget.Admit(prov, "team-a", "agent/test"); settle == nil {
		t.Fatal("setup: expected to hold the boundary slot")
	}

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", resp.Header.Get("Retry-After"))
	}
	if got := errType(t, resp); got != errBudgetThrottled {
		t.Fatalf("error type = %q, want budget_throttled", got)
	}
	if n := drainUpstream(h); n != 0 {
		t.Fatalf("upstream saw %d requests during a throttle", n)
	}
}

// A replica that cannot verify budget state inside the boundary region fails
// closed with 503 budget_state_unavailable and never calls upstream.
func TestProxyHard_FailClosed(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h.seedRoute()
	prov := hardenProv(h)
	// In the boundary with spend, but no fold has ever succeeded: the read
	// path is blind, so the replica must not spend.
	h.server.Budget.Add(prov, "team-a", "agent/test", 96)

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := errType(t, resp); got != errBudgetUnavailable {
		t.Fatalf("error type = %q, want budget_state_unavailable", got)
	}
	if n := drainUpstream(h); n != 0 {
		t.Fatalf("upstream saw %d requests while failing closed", n)
	}
}

// An admitted boundary request forwards, settles its actual cost, and frees
// the slot: the following request sees the settled spend.
func TestProxyHard_AdmitAndSettleBuffered(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	})
	h.seedRoute()
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	h.server.Budget.mu.Lock()
	l := h.server.Budget.providers["prov"]
	own, slots := l.own["team-a"], len(l.adm)
	h.server.Budget.mu.Unlock()
	if own != 1 || slots != 0 { // $1 for 1M input tokens at 1.00/1M
		t.Fatalf("own=%v slots=%d after settle, want 1 and 0", own, slots)
	}

	// 96 (peers) + 1 (own) = 97: still in boundary, slot must be free.
	if d, settle := h.server.Budget.Admit(prov, "team-a", "agent/test"); settle == nil || d.Throttled {
		t.Fatalf("post-settle admit = %+v, want a fresh slot", d)
	} else {
		settle(0)
	}
}

// A streaming request that dies mid-relay still settles its accumulated
// usage and frees the slot (the defer-settle fix).
func TestProxyHard_StreamDisconnectSettles(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":1000000,\"completion_tokens\":0}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hang until the relay's context dies with the client disconnect.
		// Cleanup order matters: this registration comes AFTER newHarness,
		// so close(block) runs BEFORE the upstream server's Close waits on
		// this handler.
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(block) })
	h.seedRoute()
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})

	cert := agentCert(t, h.ca)
	client := h.client(&cert)
	client.Timeout = 600 * time.Millisecond
	req, err := http.NewRequest(http.MethodPost, h.url("/v1/chat/completions"),
		strings.NewReader(`{"model":"prov/m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		// Read until the client timeout tears the connection down.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		h.server.Budget.mu.Lock()
		l := h.server.Budget.providers["prov"]
		own, slots := l.own["team-a"], len(l.adm)
		h.server.Budget.mu.Unlock()
		if own == 1 && slots == 0 {
			return // accumulated usage settled, slot freed
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream disconnect: own=%v slots=%d, want 1 and 0", own, slots)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
