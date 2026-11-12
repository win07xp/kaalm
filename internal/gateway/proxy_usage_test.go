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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// A provider that never reports usage logs one warning a minute per
// provider, while the metric counts every response.
func TestUsageMissing_WarnsOncePerProviderPerInterval(t *testing.T) {
	buf := captureSlog(t)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","choices":[]}`)
	})
	h.seedRoute()
	clock := time.Unix(1757600000, 0)
	h.server.usageMissingLog.now = func() time.Time { return clock }
	cert := agentCert(t, h.ca)
	client := h.client(&cert)
	missing := func() float64 {
		return testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1"))
	}
	calls := 0.0
	call := func() {
		t.Helper()
		resp := postJSON(t, client, h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1", "messages": []any{}}, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		<-h.upreqs
		// The relay settles after the agent has the last byte.
		calls++
		waitFor(t, func() bool { return missing() == calls })
	}
	const msg = "LLM response carried no usage; settled at zero spend"

	call()
	call()
	if n := len(logRecords(t, buf, msg)); n != 1 {
		t.Errorf("warnings after two responses = %d, want 1", n)
	}
	h.server.usageMissing("team-a", "other", "m9", Usage{}, 0)
	if n := len(logRecords(t, buf, msg)); n != 2 {
		t.Errorf("warnings after another provider's response = %d, want 2", n)
	}
	clock = clock.Add(time.Minute)
	call()
	if n := len(logRecords(t, buf, msg)); n != 3 {
		t.Errorf("warnings a minute later = %d, want 3", n)
	}
}

// A stream that ends with part of its usage unreported logs that its spend
// was estimated, which parts, and the estimated dollars, under the same
// once-a-minute throttle as the zero-spend line.
func TestUsageMissing_StreamLogsEstimatedSpend(t *testing.T) {
	buf := captureSlog(t)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`+"\n\n"+
			"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello world!"}}`+"\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	h.seedRoute()
	prov := h.store.providers["prov"]
	prov.Spec.Type = providerTypeAnthropic
	prov.Spec.Models = []kaalmv1beta1.ModelProviderModel{
		{ID: "m1", CostPer1MInputTokens: "2", CostPer1MOutputTokens: "4"},
	}
	clock := time.Unix(1757600000, 0)
	h.server.usageMissingLog.now = func() time.Time { return clock }
	cert := agentCert(t, h.ca)
	client := h.client(&cert)
	calls := 0.0
	call := func() {
		t.Helper()
		resp := postJSON(t, client, h.url("/v1/messages"), map[string]any{"model": "prov/m1", "stream": true, "max_tokens": 100}, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		<-h.upreqs
		calls++
		waitFor(t, func() bool {
			return testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")) == calls
		})
	}
	const msg = "LLM stream carried incomplete usage; settled at estimated spend"

	call()
	call()
	recs := logRecords(t, buf, msg)
	if len(recs) != 1 {
		t.Fatalf("estimated-spend warnings after two streams = %d, want 1 (log: %s)", len(recs), buf.String())
	}
	// "hello world!" is 12 bytes: three output tokens at $4 per million.
	if got := recs[0]["estimated"]; got != "output" {
		t.Errorf("estimated = %v, want output", got)
	}
	if got, _ := recs[0]["usd"].(float64); !nearlyEqual(got, 3*4/1e6) {
		t.Errorf("usd = %v, want %v", recs[0]["usd"], 3*4/1e6)
	}
	if n := len(logRecords(t, buf, "LLM response carried no usage; settled at zero spend")); n != 0 {
		t.Errorf("zero-spend warnings = %d, want 0", n)
	}
}

// Whatever stream_options a streaming agent sends, the provider is asked for
// usage, so the call settles at its real spend.
func TestHandleLLMProxy_AgentCannotTurnOffStreamUsage(t *testing.T) {
	for _, opts := range []string{`{"include_usage":false}`, `null`, `{}`} {
		t.Run(opts, func(t *testing.T) {
			var h *harness
			h = newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				cr := <-h.upreqs
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
				if so, _ := cr.body["stream_options"].(map[string]any); so["include_usage"] == true {
					_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n")
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			})
			h.seedRoute()
			cert := agentCert(t, h.ca)
			body := `{"model":"prov/m1","stream":true,"stream_options":` + opts + `,"messages":[]}`
			resp := postRaw(t, h.client(&cert), h.url("/v1/chat/completions"), []byte(body))
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			waitForSpend(t, h, "prov", "m1", 12)
			if u := h.spend.Total("team-a", "prov", "m1"); u.OutputTokens != 3 {
				t.Errorf("output tokens %d, want 3", u.OutputTokens)
			}
			if got := testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")); got != 0 {
				t.Errorf("kaalm_llm_usage_missing_total = %v, want 0", got)
			}
		})
	}
}
