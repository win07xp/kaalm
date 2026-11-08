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
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

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
// refused.
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
	// The relay debits when it ends, after the agent has the last byte; the
	// spend record lands after the debit.
	waitUsage(t, h, "prov", "m1")
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
	// The relay debits when it ends, after the agent has the last byte; the
	// spend record lands after the debit.
	waitUsage(t, h, "backup", "m1")
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

// tokenLevel reads team-a/m1's token bucket under the limiter's lock.
func tokenLevel(h *harness) float64 {
	rl := h.server.RateLimiter
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if b := rl.buckets[tokenKey("team-a", "m1")]; b != nil {
		return b.tokens
	}
	return math.NaN()
}

// waitTokenLevel polls until team-a/m1's token bucket holds want: the
// handler settles its reservation on its way out, after the caller can have
// the whole response.
func waitTokenLevel(t *testing.T, h *harness, want float64) {
	t.Helper()
	got := tokenLevel(h)
	for i := 0; i < 100 && got != want; i++ {
		time.Sleep(10 * time.Millisecond)
		got = tokenLevel(h)
	}
	if got != want {
		t.Errorf("token bucket = %v, want %v", got, want)
	}
}

// reservationFor is what the gateway reserves for body: its JSON size over
// four as the input estimate, plus the output cap.
func reservationFor(body map[string]any, outputCap int64) int64 {
	raw, _ := json.Marshal(body)
	return int64(len(raw)/4) + outputCap
}

// TestProxy_TokensPerMinuteConcurrentStreamsReserve: streams started at once
// each reserve their estimate at admission, so once they hold the bucket
// below zero the next request is refused while they run, and the usage they
// report replaces their reservations when they end.
func TestProxy_TokensPerMinuteConcurrentStreamsReserve(t *testing.T) {
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})
	h := tpmHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`)
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeSSE(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":100}}`, `data: [DONE]`)
	})
	cert := agentCert(t, h.ca)
	client := h.client(&cert)
	body := map[string]any{"model": "prov/m1", "stream": true, "max_tokens": 400}
	reserve := reservationFor(body, 400)

	var wg sync.WaitGroup
	statuses := make([]int, 3)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := postJSON(t, client, h.url("/v1/chat/completions"), body, nil)
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	for range statuses {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("the three streams did not all reach the provider")
		}
	}
	// Three reservations hold the bucket at 1000 - 3*reserve, below zero.
	expectTokenRateLimited(t, h, strconv.Itoa(retryAfterSeconds(float64(3*reserve-1000), 1000)))

	close(release)
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("stream %d = %d, want 200", i, status)
		}
	}
	// Each stream's 200 tokens replace its reservation: 1000 - 600 = 400.
	for i := 0; i < 100 && h.spend.Total("team-a", "prov", "m1").InputTokens != 300; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	waitTokenLevel(t, h, 400)
	resp := postJSON(t, client, h.url("/v1/chat/completions"), body, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("request after the streams settled = %d, want 200", resp.StatusCode)
	}
}

// TestProxy_TokensPerMinuteStreamWithoutUsageKeepsReservation: a stream that
// ends with no usage keeps its reservation as its charge.
func TestProxy_TokensPerMinuteStreamWithoutUsageKeepsReservation(t *testing.T) {
	h := tpmHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`, `data: [DONE]`)
	})
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "stream": true, "max_tokens": 5000}, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}
	missing := func() float64 {
		return testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1"))
	}
	for i := 0; i < 100 && missing() != 1; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := missing(); got != 1 {
		t.Fatalf("usage missing = %v, want 1", got)
	}
	// The reservation, cut at the floor of -1000, stays: a minute at 1000/min.
	waitTokenLevel(t, h, -1000)
	expectTokenRateLimited(t, h, "60")
}

// TestProxy_TokensPerMinuteNoCapReservesOneShare: a request with no output
// cap on a model that declares no maxOutputTokens reserves one replica
// share, so the next request waits for it; the model's maxOutputTokens
// bounds the reservation instead when it is declared.
func TestProxy_TokensPerMinuteNoCapReservesOneShare(t *testing.T) {
	for _, tc := range []struct {
		name            string
		maxOutputTokens *int64
		wantSecond      int
	}{
		{name: "no maxOutputTokens", wantSecond: http.StatusTooManyRequests},
		{name: "maxOutputTokens 400", maxOutputTokens: func() *int64 { v := int64(400); return &v }(), wantSecond: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			arrived := make(chan struct{}, 1)
			release := make(chan struct{})
			h := tpmHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					arrived <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			})
			h.store.providers["prov"].Spec.Models[0].MaxOutputTokens = tc.maxOutputTokens
			cert := agentCert(t, h.ca)
			client := h.client(&cert)
			body := map[string]any{"model": "prov/m1"}
			first := make(chan int, 1)
			go func() {
				resp := postJSON(t, client, h.url("/v1/chat/completions"), body, nil)
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				first <- resp.StatusCode
			}()
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("the first request did not reach the provider")
			}
			resp := postJSON(t, client, h.url("/v1/chat/completions"), body, nil)
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			close(release)
			if resp.StatusCode != tc.wantSecond {
				t.Errorf("second request while the first runs = %d, want %d", resp.StatusCode, tc.wantSecond)
			}
			if tc.wantSecond == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "1" {
				t.Errorf("Retry-After = %q, want 1", resp.Header.Get("Retry-After"))
			}
			if status := <-first; status != http.StatusOK {
				t.Errorf("first request = %d, want 200", status)
			}
		})
	}
}

// TestProxy_TokensPerMinuteFailedCallReleasesReservation: a call no
// candidate answers with a 2xx returns its whole reservation.
func TestProxy_TokensPerMinuteFailedCallReleasesReservation(t *testing.T) {
	h := tpmHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "max_tokens": 1500}, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed call = %d, want 502", resp.StatusCode)
	}
	waitTokenLevel(t, h, 1000)
}
