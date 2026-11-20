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
	"bufio"
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

// TestProxy_TokensPerMinuteStreamWithoutUsageSettlesAtEstimate: a stream
// that finishes cleanly with no usage settles its reservation at the
// adapter's estimate (the body the provider received over four, plus the
// content relayed), so the next request is admitted.
func TestProxy_TokensPerMinuteStreamWithoutUsageSettlesAtEstimate(t *testing.T) {
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
	sent := <-h.upreqs
	missing := func() float64 {
		return testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1"))
	}
	for i := 0; i < 100 && missing() != 1; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := missing(); got != 1 {
		t.Fatalf("usage missing = %v, want 1", got)
	}
	// Input: the upstream body over four; output: one token for "hi".
	waitTokenLevel(t, h, float64(1000-(int64(len(sent.raw)/4)+1)))
	next := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_, _ = io.ReadAll(next.Body)
	_ = next.Body.Close()
	if next.StatusCode != http.StatusOK {
		t.Errorf("request after the estimate settled = %d, want 200", next.StatusCode)
	}
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

// TestProxy_TokensPerMinuteCutAnthropicStreamKeepsReservation: an Anthropic
// stream reports input tokens on message_start and output tokens only on
// its last message_delta. A stream cut in between, by the provider or by a
// caller that leaves, has read input usage only. Its output is the
// adapter's estimate of the deltas read, it counts as usage missing, and the
// token bucket settles at the larger of that usage and its reservation: the
// output it may have sent stays charged.
func TestProxy_TokensPerMinuteCutAnthropicStreamKeepsReservation(t *testing.T) {
	const start = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"
	for _, tc := range []struct {
		name         string
		callerLeaves bool
	}{
		{name: "provider breaks the stream"},
		{name: "caller leaves", callerLeaves: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tpmHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, start)
				w.(http.Flusher).Flush()
				if tc.callerLeaves {
					<-r.Context().Done()
					return
				}
				panic(http.ErrAbortHandler)
			})
			h.store.providers["prov"].Spec.Type = "anthropic"
			cert := agentCert(t, h.ca)
			body := map[string]any{"model": "prov/m1", "stream": true, "max_tokens": 400}
			reserve := reservationFor(body, 400)

			resp := postJSON(t, h.client(&cert), h.url("/v1/messages"), body, nil)
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				t.Fatalf("stream = %d, want 200", resp.StatusCode)
			}
			if tc.callerLeaves {
				br := bufio.NewReader(resp.Body)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("stream ended before the first delta: %v", err)
					}
					if strings.Contains(line, "content_block_delta") {
						break
					}
				}
			} else {
				_, _ = io.ReadAll(resp.Body)
			}
			_ = resp.Body.Close()
			for i := 0; i < 100 && h.spend.Total("team-a", "prov", "m1").InputTokens != 7; i++ {
				time.Sleep(10 * time.Millisecond)
			}
			// Spend is recorded after the settle, so it shows the relay
			// settled and did not just leave the reservation in place.
			if got := h.spend.Total("team-a", "prov", "m1").InputTokens; got != 7 {
				t.Fatalf("spend input tokens = %d, want 7: the relay never settled", got)
			}
			if got := h.spend.Total("team-a", "prov", "m1").OutputTokens; got != 1 {
				t.Errorf("spend output tokens = %d, want 1 estimated for the delta read", got)
			}
			// usageMissing runs after the spend is recorded.
			waitFor(t, func() bool {
				return testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")) == 1
			})
			waitTokenLevel(t, h, float64(1000-reserve))
		})
	}
}

// TestProxy_TokensPerMinuteCallerLeftAfterFinalEventSettlesUsage: a stream
// whose final event (OpenAI [DONE], Anthropic message_stop) the gateway has
// read is finished, even when the caller closes before the provider ends the
// body. Its usage is complete, so it settles at that usage, not at its
// reservation.
func TestProxy_TokensPerMinuteCallerLeftAfterFinalEventSettlesUsage(t *testing.T) {
	for _, tc := range []struct {
		name, provType, path, final string
		events                      []string
	}{
		{
			name: "openai", provType: "openai", path: "/v1/chat/completions", final: "[DONE]",
			events: []string{
				`data: {"choices":[{"delta":{"content":"hi"}}]}`,
				`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":100}}`,
				`data: [DONE]`,
			},
		},
		{
			name: "anthropic", provType: "anthropic", path: "/v1/messages", final: "message_stop",
			events: []string{
				"event: message_start\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":100}}}`,
				"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
				"event: message_delta\n" + `data: {"type":"message_delta","usage":{"output_tokens":100}}`,
				"event: message_stop\n" + `data: {"type":"message_stop"}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tpmHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(w, tc.events...)
				// The answer is complete, but the body stays open until the
				// caller leaves.
				<-r.Context().Done()
			})
			h.store.providers["prov"].Spec.Type = tc.provType
			cert := agentCert(t, h.ca)
			body := map[string]any{"model": "prov/m1", "stream": true, "max_tokens": 400}

			resp := postJSON(t, h.client(&cert), h.url(tc.path), body, nil)
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				t.Fatalf("stream = %d, want 200", resp.StatusCode)
			}
			br := bufio.NewReader(resp.Body)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("stream ended before %s: %v", tc.final, err)
				}
				if strings.Contains(line, tc.final) {
					break
				}
			}
			_ = resp.Body.Close()
			for i := 0; i < 100 && h.spend.Total("team-a", "prov", "m1").InputTokens != 100; i++ {
				time.Sleep(10 * time.Millisecond)
			}
			if got := h.spend.Total("team-a", "prov", "m1").InputTokens; got != 100 {
				t.Fatalf("spend input tokens = %d, want 100: the relay never settled", got)
			}
			waitTokenLevel(t, h, 800)
		})
	}
}

// mediaSuffix closes the media string and the members around it in a
// mediaRequest body.
const mediaSuffix = `"}}]}]}`

// mediaRequest builds a request body of exactly the bytes prefix, media, and
// suffix (mediaSuffix when none is given), and returns it with the media's
// length.
func mediaRequest(prefix, media string, suffix ...string) ([]byte, int) {
	end := mediaSuffix
	if len(suffix) > 0 {
		end = suffix[0]
	}
	return []byte(prefix + media + end), len(media)
}

// TestRequestTokenEstimate_LeavesOutMedia: a body of mediaScanMinBytes or
// more reserves its size less its images, audio, and video, under the
// request format's media rule, over four as the input; documents count, a
// smaller body counts whole, and a provider with no tokensPerMinute
// reserves nothing.
func TestRequestTokenEstimate_LeavesOutMedia(t *testing.T) {
	big := strings.Repeat("A", 20000)
	anthropicPrefix := `{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},`
	anthropicBody, anthropicMedia := mediaRequest(
		anthropicPrefix+`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`, big)
	anthropicAfterBody, anthropicAfterMedia := mediaRequest(
		anthropicPrefix+`{"type":"image","source":{"type":"base64","data":"`, big, `","media_type":"image/png"}}]}]}`)
	anthropicTextDoc, _ := mediaRequest(
		anthropicPrefix+`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"`, big)
	anthropicPDF, _ := mediaRequest(
		anthropicPrefix+`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"`, big)
	openaiPrefix := `{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
		`{"type":"image_url","image_url":{"url":"`
	openaiBody, openaiMedia := mediaRequest(openaiPrefix, "data:image/png;base64,"+big)
	openaiPDF, _ := mediaRequest(
		`{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":[`+
			`{"type":"file","file":{"filename":"a.pdf","file_data":"`, "data:application/pdf;base64,"+big)
	audioBody, audioMedia := mediaRequest(
		`{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":[`+
			`{"type":"input_audio","input_audio":{"format":"wav","data":"`, big)
	vertexPrefix := `{"generationConfig":{"maxOutputTokens":10},"contents":[{"role":"user","parts":[{"text":"what is this"},`
	vertexBody, vertexMedia := mediaRequest(vertexPrefix+`{"inlineData":{"mimeType":"image/png","data":"`, big)
	vertexVideo, vertexVideoMedia := mediaRequest(vertexPrefix+`{"inline_data":{"mime_type":"video/mp4","data":"`, big)
	vertexText, _ := mediaRequest(vertexPrefix+`{"inlineData":{"mimeType":"text/plain","data":"`, big)
	vertexPDF, _ := mediaRequest(vertexPrefix+`{"inlineData":{"mimeType":"application/pdf","data":"`, big)
	smallBody, _ := mediaRequest(openaiPrefix, "data:image/png;base64,"+strings.Repeat("A", 8000))
	pad := mediaScanMinBytes - len(openaiPrefix) - len(mediaSuffix) - len("data:image/png;base64,")
	edgeBody, edgeMedia := mediaRequest(openaiPrefix, "data:image/png;base64,"+strings.Repeat("A", pad))
	if len(edgeBody) != mediaScanMinBytes {
		t.Fatalf("edge body is %d bytes, want %d", len(edgeBody), mediaScanMinBytes)
	}
	left := func(body []byte, media int) tokenEstimate {
		return tokenEstimate{Input: int64((len(body) - media) / 4), Output: 10}
	}
	whole := func(body []byte) tokenEstimate { return tokenEstimate{Input: int64(len(body) / 4), Output: 10} }

	for _, tc := range []struct {
		name         string
		providerType string
		adapter      providerAdapter
		tpm          int32
		body         []byte
		want         tokenEstimate
	}{
		{"anthropic image", kaalmv1beta1.ProviderTypeAnthropic, nil, 1000, anthropicBody, left(anthropicBody, anthropicMedia)},
		{"anthropic image with the type after the data", kaalmv1beta1.ProviderTypeAnthropic, nil, 1000,
			anthropicAfterBody, left(anthropicAfterBody, anthropicAfterMedia)},
		{"anthropic text document counts", kaalmv1beta1.ProviderTypeAnthropic, nil, 1000, anthropicTextDoc, whole(anthropicTextDoc)},
		{"anthropic pdf counts", kaalmv1beta1.ProviderTypeAnthropic, nil, 1000, anthropicPDF, whole(anthropicPDF)},
		{"openai image data URL", kaalmv1beta1.ProviderTypeOpenAI, nil, 1000, openaiBody, left(openaiBody, openaiMedia)},
		{"openai input audio", kaalmv1beta1.ProviderTypeOpenAI, nil, 1000, audioBody, left(audioBody, audioMedia)},
		{"openai file_data pdf counts", kaalmv1beta1.ProviderTypeOpenAI, nil, 1000, openaiPDF, whole(openaiPDF)},
		{"openai-compatible image data URL", kaalmv1beta1.ProviderTypeOpenAICompatible, nil, 1000,
			openaiBody, left(openaiBody, openaiMedia)},
		{"google-vertex inline data", kaalmv1beta1.ProviderTypeGoogleVertex, nil, 1000, vertexBody, left(vertexBody, vertexMedia)},
		{"google-vertex inline_data video", kaalmv1beta1.ProviderTypeGoogleVertex, nil, 1000,
			vertexVideo, left(vertexVideo, vertexVideoMedia)},
		{"google-vertex text counts", kaalmv1beta1.ProviderTypeGoogleVertex, nil, 1000, vertexText, whole(vertexText)},
		{"google-vertex pdf counts", kaalmv1beta1.ProviderTypeGoogleVertex, nil, 1000, vertexPDF, whole(vertexPDF)},
		{"anthropic body on an openai provider uses the request format's rule", kaalmv1beta1.ProviderTypeOpenAI,
			anthropicAdapter{}, 1000, anthropicBody, left(anthropicBody, anthropicMedia)},
		{"body under the threshold is not scanned", kaalmv1beta1.ProviderTypeOpenAI, nil, 1000, smallBody, whole(smallBody)},
		{"body at the threshold is scanned", kaalmv1beta1.ProviderTypeOpenAI, nil, 1000, edgeBody, left(edgeBody, edgeMedia)},
		{"no tokensPerMinute reserves nothing", kaalmv1beta1.ProviderTypeOpenAI, nil, 0, openaiBody, tokenEstimate{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := tc.adapter
			if adapter == nil {
				var ok bool
				if adapter, ok = adapterForProviderType(tc.providerType); !ok {
					t.Fatalf("no adapter for %q", tc.providerType)
				}
			}
			provider := &kaalmv1beta1.ModelProvider{Spec: kaalmv1beta1.ModelProviderSpec{
				Type:       tc.providerType,
				RateLimits: kaalmv1beta1.ModelProviderRateLimits{TokensPerMinute: tc.tpm},
			}}
			if got := requestTokenEstimate(provider, "m1", tc.body, adapter); got != tc.want {
				t.Errorf("estimate = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRequestTokenEstimate_AgreesWithStreamEstimate: for a body of
// mediaScanMinBytes or more, the reservation's input is the stream
// estimate's input for the same body and provider type, with media or a
// document.
func TestRequestTokenEstimate_AgreesWithStreamEstimate(t *testing.T) {
	big := strings.Repeat("A", 20000)
	for _, tc := range []struct {
		name, providerType string
		body               string
	}{
		{"anthropic image", kaalmv1beta1.ProviderTypeAnthropic, `{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":[` +
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}`},
		{"anthropic pdf", kaalmv1beta1.ProviderTypeAnthropic, `{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":[` +
			`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + big + `"}}]}]}`},
		{"openai image", kaalmv1beta1.ProviderTypeOpenAI, `{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":[` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `"}}]}]}`},
		{"openai file_data pdf", kaalmv1beta1.ProviderTypeOpenAI, `{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":[` +
			`{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,` + big + `"}}]}]}`},
		{"vertex image", kaalmv1beta1.ProviderTypeGoogleVertex, `{"contents":[{"role":"user","parts":[` +
			`{"inlineData":{"mimeType":"image/png","data":"` + big + `"}}]}]}`},
		{"vertex pdf", kaalmv1beta1.ProviderTypeGoogleVertex, `{"contents":[{"role":"user","parts":[` +
			`{"inlineData":{"mimeType":"application/pdf","data":"` + big + `"}}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, _ := adapterForProviderType(tc.providerType)
			provider := &kaalmv1beta1.ModelProvider{Spec: kaalmv1beta1.ModelProviderSpec{
				Type:       tc.providerType,
				RateLimits: kaalmv1beta1.ModelProviderRateLimits{TokensPerMinute: 1000},
			}}
			body := []byte(tc.body)
			reserved := requestTokenEstimate(provider, "m1", body, adapter).Input
			if estimated := adapter.streamEstimator(body).estimateInput(); reserved != estimated {
				t.Errorf("reservation input = %d, stream estimate input = %d", reserved, estimated)
			}
		})
	}
}

// TestProxy_TokensPerMinuteMediaReservesNothing: a request with a base64
// image reserves its text and its output cap, not the image's bytes, so the
// next request for the same model is admitted. The provider reports no
// usage, so the bucket keeps exactly what admission took.
func TestProxy_TokensPerMinuteMediaReservesNothing(t *testing.T) {
	h := tpmHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	cert := agentCert(t, h.ca)
	client := h.client(&cert)
	dataURL := "data:image/png;base64," + strings.Repeat("A", 32768)
	body := map[string]any{"model": "prov/m1", "max_tokens": 10, "messages": []any{map[string]any{
		"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
		},
	}}}
	resp := postJSON(t, client, h.url("/v1/chat/completions"), body, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request with an image = %d, want 200", resp.StatusCode)
	}
	raw, _ := json.Marshal(body)
	waitTokenLevel(t, h, float64(1000-(int64((len(raw)-len(dataURL))/4)+10)))

	next := postJSON(t, client, h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_, _ = io.ReadAll(next.Body)
	_ = next.Body.Close()
	if next.StatusCode != http.StatusOK {
		t.Errorf("request after the image = %d, want 200", next.StatusCode)
	}
}

// blockingUpstream answers every request with response once release is
// closed, after recording its path and signalling arrived.
func blockingUpstream(response string) (fn http.HandlerFunc, arrived chan struct{}, release chan struct{}, path *atomic.Value) {
	arrived = make(chan struct{}, 4)
	release = make(chan struct{})
	path = &atomic.Value{}
	fn = func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		path.Store(r.URL.Path)
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}
	return fn, arrived, release, path
}

// waitArrived waits for the upstream to receive a request.
func waitArrived(t *testing.T, arrived chan struct{}, release chan struct{}) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the request did not reach the provider")
	}
}

// TestProxy_TokensPerMinuteDocumentReservesItsBytes: a text document counts
// at its bytes over four, so while the request runs its reservation holds
// the bucket at the debt clamp and the next request is refused.
func TestProxy_TokensPerMinuteDocumentReservesItsBytes(t *testing.T) {
	fn, arrived, release, _ := blockingUpstream(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`)
	h := tpmHarness(t, fn)
	h.store.providers["prov"].Spec.Type = "anthropic"
	cert := agentCert(t, h.ca)
	client := h.client(&cert)
	body := map[string]any{"model": "prov/m1", "max_tokens": 10, "messages": []any{map[string]any{
		"role": "user", "content": []any{
			map[string]any{"type": "document", "source": map[string]any{
				"type": "text", "media_type": "text/plain", "data": strings.Repeat("a", 32768)}},
			map[string]any{"type": "text", "text": "summarize"},
		},
	}}}
	status := make(chan int, 1)
	go func() {
		resp := postJSON(t, client, h.url("/v1/messages"), body, nil)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	waitArrived(t, arrived, release)
	// About 8,200 tokens reserved, clamped at minus one share.
	waitTokenLevel(t, h, -1000)
	expectTokenRateLimited(t, h, "60")
	close(release)
	if got := <-status; got != http.StatusOK {
		t.Errorf("request with a document = %d, want 200", got)
	}
}
