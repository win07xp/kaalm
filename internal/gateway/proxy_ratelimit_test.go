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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// tpmHarness wires a 1000 tokens-per-minute ceiling on "prov" with the
// limiter's clock pinned, so Retry-After is exact.
func tpmHarness(t *testing.T, upstreamFn http.HandlerFunc) *harness {
	t.Helper()
	h := newHarness(t, upstreamFn)
	h.seedRoute()
	h.server.RateLimiter = NewRateLimiter(func() int { return 1 })
	now := time.Now()
	h.server.RateLimiter.now = func() time.Time { return now }
	h.store.providers["prov"].Spec.RateLimits = kaalmv1beta1.ModelProviderRateLimits{TokensPerMinute: 1000}
	return h
}

// expectTokenRateLimited sends one more request and checks it is refused
// with 429 rate_limited and the Retry-After the debt implies.
func expectTokenRateLimited(t *testing.T, h *harness, retryAfter string) {
	t.Helper()
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		_ = resp.Body.Close()
		t.Fatalf("request after the debit = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != retryAfter {
		t.Errorf("Retry-After = %q, want %q", got, retryAfter)
	}
	if got := errType(t, resp); got != errRateLimited {
		t.Errorf("error type %q, want %q", got, errRateLimited)
	}
}

// TestProxy_TokensPerMinuteBlocksAfterLargeCall: a buffered response's
// settled input plus output tokens are debited, so the next request is
// refused (#202).
func TestProxy_TokensPerMinuteBlocksAfterLargeCall(t *testing.T) {
	h := tpmHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":900,"completion_tokens":200}}`))
	})
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first call = %d, want 200", resp.StatusCode)
	}
	// 1000 - 1100 = -100 tokens at 1000/min: 6 seconds.
	expectTokenRateLimited(t, h, "6")
}

// TestProxy_TokensPerMinuteDebitsAtStreamEnd: a stream debits its usage
// when it ends, and the next request is refused.
func TestProxy_TokensPerMinuteDebitsAtStreamEnd(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":300}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	h := tpmHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "stream": true}, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}
	// The relay settles after its last flush; wait for the spend record,
	// which lands after the debit.
	for i := 0; i < 100 && h.spend.Total("team-a", "prov", "m1").isZero(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	// 1000 - 1500 = -500 tokens at 1000/min: 30 seconds.
	expectTokenRateLimited(t, h, "30")
}

// TestProxy_TokensPerMinuteDebitsPrimaryOnFallback: a call the backup
// serves debits the bucket that admitted it, the primary's.
func TestProxy_TokensPerMinuteDebitsPrimaryOnFallback(t *testing.T) {
	h := tpmHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.server.Recorder = &recordingRecorder{}
	h.addBackupProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":1000}}`))
	})
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "backup"}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("fallback call = %d, want 200", resp.StatusCode)
	}
	// 1000 - 2000 = -1000 tokens (the clamp) at 1000/min: 60 seconds.
	expectTokenRateLimited(t, h, "60")
}

// TestProxy_RequestRateLimitRetryAfter: a request refusal carries the time
// until the next request token instead of a fixed second.
func TestProxy_RequestRateLimitRetryAfter(t *testing.T) {
	h := tpmHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	h.store.providers["prov"].Spec.RateLimits = kaalmv1beta1.ModelProviderRateLimits{RequestsPerMinute: 2}
	cert := agentCert(t, h.ca)
	for i := 0; i < 2; i++ {
		resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("call %d = %d, want 200", i, resp.StatusCode)
		}
	}
	// Two requests a minute: the next token is 30 seconds away.
	expectTokenRateLimited(t, h, "30")
}
