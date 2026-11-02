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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// llmRequestsWith reads kaalm_llm_requests_total for prov/m1 in team-a.
func llmRequestsWith(h *harness, status string) float64 {
	return testutil.ToFloat64(h.server.Metrics.llmRequests.WithLabelValues("prov", "m1", "team-a", status))
}

// wireBackup adds the openai "backup" provider as prov's fallback and allows
// it for the seeded agent.
func wireBackup(t *testing.T, h *harness, backupFn http.HandlerFunc) {
	t.Helper()
	h.server.Recorder = &recordingRecorder{}
	h.addBackupProvider(t, backupFn)
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "backup"}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})
}

// boundarySlots reads prov's hard-enforcement state for team-a.
func boundarySlots(h *harness) (own float64, slots int) {
	h.server.Budget.mu.Lock()
	defer h.server.Budget.mu.Unlock()
	l := h.server.Budget.providers["prov"]
	return l.own["team-a"], len(l.adm)
}

// A non-streamed answer reaches the agent as the provider sends it: the
// first bytes arrive while the provider still holds the rest.
func TestRelayJSON_WritesBytesAsTheyArrive(t *testing.T) {
	const head = `{"id":"x","choices":[`
	const tail = `{"message":{"content":"hi"}}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, head)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, tail)
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		close(release)
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	first := make([]byte, len(head))
	got := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, first); got <- err }()
	select {
	case err := <-got:
		if err != nil || string(first) != head {
			close(release)
			t.Fatalf("first bytes %q, %v; want %q", first, err, head)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the first bytes did not reach the agent while the provider held the rest")
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(first)+string(rest) != head+tail {
		t.Fatalf("body %q, %v; want %q", string(first)+string(rest), err, head+tail)
	}
	if u := waitUsage(t, h, "prov", "m1"); u.InputTokens != 4 || u.OutputTokens != 2 {
		t.Errorf("settled usage %+v, want 4 and 2", u)
	}
	waitFor(t, func() bool { return llmRequestsWith(h, outcomeOK) == 1 })
}

// A 2xx answer the provider cuts off reaches the agent cut off: the
// gateway does not fall back once the status is sent, and the agent's
// client sees an incomplete body rather than a clean end.
func TestRelayJSON_CutOffAnswerReachesTheAgentCutOff(t *testing.T) {
	const partial = `{"id":"x","choices":[{"message":{"content":"half`
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, partial)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	h.seedRoute()
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})
	var backupCalls atomic.Int64
	wireBackup(t, h, func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-backup","usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the 200 the provider started", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("body read ended cleanly with %q; want an incomplete body", body)
	}
	if string(body) != partial {
		t.Errorf("body %q, want the partial %q", body, partial)
	}
	waitFor(t, func() bool { return llmRequestsWith(h, outcomeError) == 1 })
	if got := backupCalls.Load(); got != 0 {
		t.Errorf("backup called %d times after the 2xx started", got)
	}
	if got := testutil.CollectAndCount(h.server.Metrics.llmFallback); got != 0 {
		t.Errorf("fallback series = %d, want none", got)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")); got != 1 {
		t.Errorf("usage missing = %v, want 1", got)
	}
	waitFor(t, func() bool { own, slots := boundarySlots(h); return own == 0 && slots == 0 })
}

// Usage that arrived before the cut settles, as a stream settles what it
// accumulated.
func TestRelayJSON_UsageBeforeTheCutSettles(t *testing.T) {
	head := `{"id":"x","usage":{"prompt_tokens":1000000,"completion_tokens":5},"choices":[{"message":{"content":"` +
		strings.Repeat("y", 32<<10)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, head)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	h.seedRoute()
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if u := waitUsage(t, h, "prov", "m1"); u.InputTokens != 1000000 || u.OutputTokens != 5 {
		t.Errorf("settled usage %+v, want the usage read before the cut", u)
	}
	waitFor(t, func() bool { return llmRequestsWith(h, outcomeError) == 1 })
	waitFor(t, func() bool { _, slots := boundarySlots(h); return slots == 0 })
}

// A caller that leaves mid-relay is client_closed. The gateway reads the
// rest of the answer for its usage, which the provider sends last, and
// settles it; it does not count a provider error.
func TestRelayJSON_CallerLeftMidRelayIsClientClosed(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[`)
		w.(http.Flusher).Flush()
		arrived <- struct{}{}
		<-release
		// Padding the gateway can no longer write to the agent, then usage.
		pad := strings.Repeat(" ", 4<<10)
		for i := 0; i < 64; i++ {
			_, _ = io.WriteString(w, pad)
		}
		_, _ = io.WriteString(w, `],"usage":{"prompt_tokens":6,"completion_tokens":1}}`)
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)
	resp, cancel := postCancellable(t, h, &cert)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the provider never answered")
	}
	cancel()
	_ = resp.Body.Close()
	// Let the gateway see the caller leave before the rest arrives.
	time.Sleep(100 * time.Millisecond)
	close(release)

	waitFor(t, func() bool { return llmRequestsWith(h, outcomeClientClosed) == 1 })
	if got := llmRequestsWith(h, outcomeError); got != 0 {
		t.Errorf("error requests = %v, want 0", got)
	}
	if u := waitUsage(t, h, "prov", "m1"); u.InputTokens != 6 || u.OutputTokens != 1 {
		t.Errorf("settled usage %+v, want 6 and 1 read after the caller left", u)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")); got != 0 {
		t.Errorf("usage missing = %v, want 0", got)
	}
}

// A provider that stalls after the caller left ends the read at the idle
// bound: the request is client_closed, the usage never read counts as
// missing, and the hard-enforcement slot frees.
func TestRelayJSON_DrainEndsAtTheIdleBound(t *testing.T) {
	arrived := make(chan struct{}, 1)
	left := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[`)
		w.(http.Flusher).Flush()
		arrived <- struct{}{}
		trickleUntil(w, left)
		stall(r)
	})
	h.server.Config.UpstreamTimeout = streamTimeout
	h.seedRoute()
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})
	cert := agentCert(t, h.ca)
	resp, cancel := postCancellable(t, h, &cert)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		close(left)
		t.Fatal("the provider never answered")
	}
	cancel()
	_ = resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	close(left)

	deadline := time.Now().Add(3 * time.Second)
	for llmRequestsWith(h, outcomeClientClosed) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := llmRequestsWith(h, outcomeClientClosed); got != 1 {
		t.Fatalf("client_closed requests = %v, want 1", got)
	}
	if got := llmRequestsWith(h, outcomeError); got != 0 {
		t.Errorf("error requests = %v, want 0", got)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")); got != 1 {
		t.Errorf("usage missing = %v, want 1", got)
	}
	waitFor(t, func() bool { own, slots := boundarySlots(h); return own == 0 && slots == 0 })
}

// trickleUntil writes a single space every 50ms, flushing each, until done
// closes: JSON whitespace that keeps the answer going without changing it.
func trickleUntil(w http.ResponseWriter, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-time.After(50 * time.Millisecond):
			_, _ = io.WriteString(w, " ")
			w.(http.Flusher).Flush()
		}
	}
}

// postCancellable sends a chat completion for prov/m1 whose context the
// caller cancels to leave mid-answer.
func postCancellable(t *testing.T, h *harness, cert *tls.Certificate) (*http.Response, context.CancelFunc) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"model": "prov/m1"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url("/v1/chat/completions"), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client(cert).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, cancel
}

// An answer with no usage relays in full, settles zero, and counts as
// usage missing.
func TestRelayJSON_NoUsageSettlesZeroAndRelaysInFull(t *testing.T) {
	const answer = `{"id":"x","choices":[{"message":{"content":"no usage here"}}]}`
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	})
	h.seedRoute()
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != answer || resp.StatusCode != http.StatusOK {
		t.Fatalf("answer %d %q, %v; want 200 %q", resp.StatusCode, body, err, answer)
	}
	waitFor(t, func() bool { return llmRequestsWith(h, outcomeOK) == 1 })
	if got := testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")); got != 1 {
		t.Errorf("usage missing = %v, want 1", got)
	}
	waitFor(t, func() bool { own, slots := boundarySlots(h); return own == 0 && slots == 0 })
}

// A non-fallbackable 4xx with a body over the error read limit reaches the
// agent cut to the limit, as a complete HTTP body.
func TestLLMProxy_NonFallbackable4xxOverTheLimitIsRelayedComplete(t *testing.T) {
	big := `{"error":{"message":"` + strings.Repeat("e", 1<<20) + `"}}`
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, big)
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the 4xx body: %v", err)
	}
	if len(body) != errorBodyLimit || string(body) != big[:errorBodyLimit] {
		t.Errorf("body length %d, want the first %d bytes", len(body), errorBodyLimit)
	}
}

func TestCrossingHoldLimit(t *testing.T) {
	cases := []struct {
		maxTokens, want int64
	}{
		{0, crossingHoldFloor},
		{-5, crossingHoldFloor},
		{4096, crossingHoldFloor + 4096*crossingHoldBytesPerToken},
		{math.MaxInt64, math.MaxInt64},
		{math.MaxInt64 / 8, math.MaxInt64},
	}
	for _, tc := range cases {
		if got := crossingHoldLimit(tc.maxTokens); got != tc.want {
			t.Errorf("crossingHoldLimit(%d) = %d, want %d", tc.maxTokens, got, tc.want)
		}
	}
}
