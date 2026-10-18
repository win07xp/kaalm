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
	call := func() {
		t.Helper()
		resp := postJSON(t, client, h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1", "messages": []any{}}, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		<-h.upreqs
	}
	const msg = "LLM response carried no usage; settled at zero spend"

	call()
	call()
	if n := len(logRecords(t, buf, msg)); n != 1 {
		t.Errorf("warnings after two responses = %d, want 1", n)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")); got != 2 {
		t.Errorf("kaalm_llm_usage_missing_total = %v, want 2", got)
	}
	h.server.usageMissing("team-a", "other", "m9")
	if n := len(logRecords(t, buf, msg)); n != 2 {
		t.Errorf("warnings after another provider's response = %d, want 2", n)
	}
	clock = clock.Add(time.Minute)
	call()
	if n := len(logRecords(t, buf, msg)); n != 3 {
		t.Errorf("warnings a minute later = %d, want 3", n)
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
