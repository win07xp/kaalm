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
	outcome := h.server.relayStream(ctx, httptest.NewRecorder(), resp, adapter, nil, formatForType("openai"),
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
	if out := h.server.relayStream(context.Background(), w, resp, adapter, translator, callerFormat,
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
