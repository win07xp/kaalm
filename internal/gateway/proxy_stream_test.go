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
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/llmtranslate"
)

// When the caller leaves, the upstream read can end in a clean io.EOF
// instead of the context error. The relay must still count the caller
// leaving, not a stream relayed in full.
func TestRelayStream_CleanEndAfterCallerLeftIsClientClosed(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.seedRoute()
	adapter, ok := adapterForProviderType("openai")
	if !ok {
		t.Fatal("no openai adapter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")),
	}
	outcome := h.server.relayStream(ctx, httptest.NewRecorder(), resp, adapter, nil, nil, formatForType("openai"),
		"team-a", "sup", h.store.providers["prov"], "m1", nil, nil)
	if outcome != outcomeClientClosed {
		t.Errorf("outcome = %q, want %q", outcome, outcomeClientClosed)
	}
}

// relayCounted relays chunks, one per upstream read, through relayStream
// into a countingWriter, and returns the writer and the reader.
func relayCounted(t *testing.T, upstream llmtranslate.Format, translator llmtranslate.Stream,
	callerFormat llmtranslate.Format, chunks [][]byte,
) (*countingWriter, *chunkReader) {
	t.Helper()
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.seedRoute()
	adapter, _ := adapterForProviderType(string(upstream))
	resp := sseResponse(chunks)
	w := newCountingWriter()
	if out := h.server.relayStream(context.Background(), w, resp, adapter, nil, translator, callerFormat,
		"team-a", "agent/sup", h.store.providers["prov"], "m1", nil, nil); out != outcomeOK {
		t.Fatalf("outcome = %q, want ok", out)
	}
	return w, resp.Body.(*chunkReader)
}

// An Anthropic event is three lines; the relay sends it in one flush, not
// one per line.
func TestRelayStream_FlushesOncePerEventBoundary(t *testing.T) {
	events := anthropicStreamEvents(1) // six events
	w, _ := relayCounted(t, llmtranslate.FormatAnthropic, nil, llmtranslate.FormatAnthropic, events)
	if w.flushes != len(events) {
		t.Errorf("flushes = %d, want one per event (%d)", w.flushes, len(events))
	}
}

// Events that reach the gateway in one read leave in one flush.
func TestRelayStream_CoalescesEventsReadTogether(t *testing.T) {
	whole := bytes.Join(anthropicStreamEvents(20), nil)
	w, _ := relayCounted(t, llmtranslate.FormatAnthropic, nil, llmtranslate.FormatAnthropic, [][]byte{whole})
	if w.flushes > 1 {
		t.Errorf("flushes = %d, want at most 1 for a stream read at once", w.flushes)
	}
}

// Translated events follow the upstream reads: the events one OpenAI chunk
// turns into share a flush.
func TestRelayStream_TranslatedEventsFlushPerUpstreamRead(t *testing.T) {
	events := openAIStreamEvents(5)
	// The reads that produce output: the usage chunk produces none, its
	// usage goes out with the closing events.
	producing := 0
	probe := llmtranslate.NewStream(llmtranslate.FormatOpenAI, llmtranslate.FormatAnthropic, "m1")
	for _, e := range events {
		out := 0
		for _, line := range bytes.Split(bytes.TrimSuffix(e, []byte("\n")), []byte("\n")) {
			out += len(probe.Feed(line))
		}
		if out > 0 {
			producing++
		}
	}
	translator := llmtranslate.NewStream(llmtranslate.FormatOpenAI, llmtranslate.FormatAnthropic, "m1")
	w, r := relayCounted(t, llmtranslate.FormatOpenAI, translator, llmtranslate.FormatAnthropic, cloneChunks(events))
	if r.reads != len(events) {
		t.Fatalf("reads = %d, want %d", r.reads, len(events))
	}
	if w.flushes != producing {
		t.Errorf("flushes = %d, want one per upstream read that produced events (%d)", w.flushes, producing)
	}
}

// A complete event reaches the caller while the provider is still sending
// the next one: the relay does not hold it back for the rest of the next
// event, whether the next event's first line or only part of it came along.
func TestRelayStream_CompleteEventNotHeldForTheNext(t *testing.T) {
	eventA := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}` + "\n\n"
	for _, c := range []struct{ name, next string }{
		{"next event line", "event: content_block_delta\n"},
		{"partial next line", `data: {"ty`},
	} {
		t.Run(c.name, func(t *testing.T) {
			release := make(chan struct{})
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, eventA+c.next)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			defer close(release)
			h.seedRoute()
			h.store.providers["prov"].Spec.Type = "anthropic"
			cert := agentCert(t, h.ca)

			resp := postJSON(t, h.client(&cert), h.url("/v1/messages"),
				map[string]any{"model": "prov/m1", "stream": true, "max_tokens": 16}, nil)
			defer func() { _ = resp.Body.Close() }()
			got := make(chan string, 1)
			go func() {
				br := bufio.NewReader(resp.Body)
				var seen strings.Builder
				for {
					line, err := br.ReadString('\n')
					seen.WriteString(line)
					if err != nil || (line == "\n" && strings.Contains(seen.String(), "message_start")) {
						got <- seen.String()
						return
					}
				}
			}()
			select {
			case text := <-got:
				if text != eventA {
					t.Errorf("first event = %q, want %q", text, eventA)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the complete event was held until the provider sent more")
			}
		})
	}
}

// noEstimateAdapter is the OpenAI adapter with no stream estimator.
type noEstimateAdapter struct{ openaiAdapter }

func (noEstimateAdapter) streamEstimator([]byte) streamEstimator { return nil }

// A stream that ends without its usage settles the parts the provider
// reported plus the adapter's estimate for the rest. The estimate counts as
// spend and on the estimated-spend counter, and the stream counts as usage
// missing with a log line that says the spend was estimated. An adapter with
// no estimator settles only what was reported, as before.
func TestRelayStream_EstimatesUnreportedUsage(t *testing.T) {
	const request = `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hello there, how are you?"}]}`
	inputEstimate := int64(len(request) / 4)
	const inPrice, outPrice = 2.0, 4.0
	imageData := "data:image/png;base64," + strings.Repeat("iVBORw0KGgo", 1000)
	visionRequest := `{"model":"m1","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"` + imageData + `"}}]}]}`
	visionEstimate := int64((len(visionRequest) - len(imageData)) / 4)
	cases := []struct {
		name         string
		providerType string
		adapter      providerAdapter
		// request is the body the provider received, when not the default.
		request string
		body    string
		wantIn  int64
		wantOut int64
		// wantEstimatedTokens are the input and output tokens priced on
		// the estimated-spend counter.
		wantEstimatedIn, wantEstimatedOut int64
		wantLog                           string
		wantParts                         string
	}{
		{name: "openai left before usage", providerType: "openai",
			body:   "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
			wantIn: inputEstimate, wantOut: 1, wantEstimatedIn: inputEstimate, wantEstimatedOut: 1,
			wantLog: "LLM stream carried incomplete usage; settled at estimated spend", wantParts: "input,output"},
		{name: "openai legacy completions left before usage", providerType: "openai",
			body:   "data: {\"object\":\"text_completion\",\"choices\":[{\"text\":\"some generated text here\",\"index\":0}]}\n\n",
			wantIn: inputEstimate, wantOut: 6, wantEstimatedIn: inputEstimate, wantEstimatedOut: 6,
			wantLog: "LLM stream carried incomplete usage; settled at estimated spend", wantParts: "input,output"},
		{name: "openai vision request left before usage", providerType: "openai", request: visionRequest,
			body:   "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
			wantIn: visionEstimate, wantOut: 1, wantEstimatedIn: visionEstimate, wantEstimatedOut: 1,
			wantLog: "LLM stream carried incomplete usage; settled at estimated spend", wantParts: "input,output"},
		{name: "anthropic cut after message_start", providerType: "anthropic",
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
			wantIn: 7, wantOut: 1, wantEstimatedOut: 1,
			wantLog: "LLM stream carried incomplete usage; settled at estimated spend", wantParts: "output"},
		{name: "adapter without an estimator", adapter: noEstimateAdapter{}, providerType: "openai",
			body:    "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
			wantLog: "LLM response carried no usage; settled at zero spend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
			h.seedRoute()
			prov := h.store.providers["prov"]
			prov.Spec.Models = []kaalmv1beta1.ModelProviderModel{
				{ID: "m1", CostPer1MInputTokens: "2", CostPer1MOutputTokens: "4"},
			}
			adapter := tc.adapter
			if adapter == nil {
				adapter, _ = adapterForProviderType(tc.providerType)
			}
			// The caller left: the relay reads what arrived, then ends
			// client_closed without the usage event.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			body := request
			if tc.request != "" {
				body = tc.request
			}
			outcome := h.server.relayStream(ctx, httptest.NewRecorder(), resp, adapter, []byte(body), nil,
				formatForType(tc.providerType), "team-a", "sup", prov, "m1", nil, nil)
			if outcome != outcomeClientClosed {
				t.Errorf("outcome = %q, want %q", outcome, outcomeClientClosed)
			}
			u := h.spend.Total("team-a", "prov", "m1")
			if u.InputTokens != tc.wantIn || u.OutputTokens != tc.wantOut {
				t.Errorf("settled usage = %d in, %d out; want %d, %d", u.InputTokens, u.OutputTokens, tc.wantIn, tc.wantOut)
			}
			wantSpend := (float64(tc.wantIn)*inPrice + float64(tc.wantOut)*outPrice) / 1e6
			if got := testutil.ToFloat64(h.server.Metrics.llmSpend.WithLabelValues("prov", "team-a")); !nearlyEqual(got, wantSpend) {
				t.Errorf("kaalm_llm_spend_usd_total = %v, want %v", got, wantSpend)
			}
			wantEstimated := (float64(tc.wantEstimatedIn)*inPrice + float64(tc.wantEstimatedOut)*outPrice) / 1e6
			if got := testutil.ToFloat64(h.server.Metrics.llmSpendEstimated.WithLabelValues("prov", "team-a")); !nearlyEqual(got, wantEstimated) {
				t.Errorf("kaalm_llm_spend_estimated_usd_total = %v, want %v", got, wantEstimated)
			}
			if got := testutil.ToFloat64(h.server.Metrics.llmUsageMissing.WithLabelValues("prov", "m1")); got != 1 {
				t.Errorf("usage missing = %v, want 1", got)
			}
			recs := logRecords(t, buf, tc.wantLog)
			if len(recs) != 1 {
				t.Fatalf("log lines %q = %d, want 1 (log: %s)", tc.wantLog, len(recs), buf.String())
			}
			if tc.wantParts != "" {
				if got := recs[0]["estimated"]; got != tc.wantParts {
					t.Errorf("estimated = %v, want %q", got, tc.wantParts)
				}
				if got, _ := recs[0]["usd"].(float64); !nearlyEqual(got, wantEstimated) {
					t.Errorf("usd = %v, want %v", recs[0]["usd"], wantEstimated)
				}
			}
		})
	}
}

// nearlyEqual compares two dollar amounts up to float rounding.
func nearlyEqual(a, b float64) bool {
	d := a - b
	return d < 1e-12 && d > -1e-12
}
