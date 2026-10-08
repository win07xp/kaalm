//go:build perftest

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

package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestToolCallBody(t *testing.T) {
	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(toolCallBody("web_search"), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.JSONRPC != "2.0" || msg.ID != 1 || msg.Method != "tools/call" || msg.Params.Name != "web_search" {
		t.Errorf("envelope = %+v", msg)
	}
	if msg.Params.Arguments == nil {
		t.Error("arguments must be an object")
	}
	if v := msg.Params.Meta["io.modelcontextprotocol/protocolVersion"]; v != "2026-07-28" {
		t.Errorf("_meta protocolVersion = %v", v)
	}
}

func TestToolCallStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`, "200"},
		{200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tool"}}`, "rpc_error"},
		{200, `not json`, "200"},
		{503, `{"error":{"type":"tool_unavailable"}}`, "503"},
		{400, `{"jsonrpc":"2.0","id":1,"error":{"code":-32020}}`, "400"},
	}
	for _, c := range cases {
		if got := toolCallStatus(c.status, []byte(c.body)); got != c.want {
			t.Errorf("toolCallStatus(%d, %s) = %q, want %q", c.status, c.body, got, c.want)
		}
	}
}

func TestRecorderLabels(t *testing.T) {
	rec := newRecorder()
	rec.recordLabel("rpc_error", 0, nil)
	rec.record(200, 0, nil)
	res := rec.result(modeTools, 1e9, 0)
	if res.Statuses["rpc_error"] != 1 || res.Statuses["200"] != 1 || res.Requests != 2 {
		t.Errorf("statuses = %v, requests %d", res.Statuses, res.Requests)
	}
}

func TestLLMRequestBody(t *testing.T) {
	cases := []struct {
		format    string
		stream    bool
		maxTokens bool
	}{
		{formatOpenAI, false, false},
		{formatOpenAI, true, false},
		{formatAnthropic, false, true},
		{formatAnthropic, true, true},
	}
	for _, c := range cases {
		var body map[string]any
		if err := json.Unmarshal(llmRequestBody(c.format, "perf-fast/mock-model", c.stream), &body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "perf-fast/mock-model" {
			t.Errorf("%s: model = %v", c.format, body["model"])
		}
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 1 {
			t.Errorf("%s: messages = %v", c.format, body["messages"])
		}
		if got, _ := body["stream"].(bool); got != c.stream {
			t.Errorf("%s stream=%v: body stream = %v", c.format, c.stream, body["stream"])
		}
		if _, ok := body["max_tokens"]; ok != c.maxTokens {
			t.Errorf("%s: max_tokens present = %v, want %v", c.format, ok, c.maxTokens)
		}
	}
}

const openAIStream = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11}}\n\n" +
	"data: [DONE]\n\n"

const anthropicStream = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestReadStreamToTerminator(t *testing.T) {
	anthropicCut := anthropicStream[:strings.Index(anthropicStream, "event: message_stop")]
	cases := []struct {
		name, format, body string
		complete           bool
	}{
		{"openai to [DONE]", formatOpenAI, openAIStream, true},
		{"anthropic to message_stop", formatAnthropic, anthropicStream, true},
		{"anthropic data line only", formatAnthropic, "data: {\"type\":\"message_stop\"}\n\n", true},
		{"openai cut before [DONE]", formatOpenAI, strings.TrimSuffix(openAIStream, "data: [DONE]\n\n"), false},
		{"anthropic cut before message_stop", formatAnthropic, anthropicCut, false},
		{"openai terminator on an anthropic read", formatAnthropic, openAIStream, false},
	}
	for _, c := range cases {
		start := time.Now()
		ttfb, ttlb, complete, err := readStream(strings.NewReader(c.body), c.format, start)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if complete != c.complete {
			t.Errorf("%s: complete = %v, want %v", c.name, complete, c.complete)
		}
		if ttfb <= 0 || (c.complete && ttlb < ttfb) {
			t.Errorf("%s: ttfb %v ttlb %v", c.name, ttfb, ttlb)
		}
	}
}

// TestReadStreamSeparatesFirstAndLastByte feeds events with a pause between
// them; only the lower bound is asserted, so a slow machine cannot flake it.
func TestReadStreamSeparatesFirstAndLastByte(t *testing.T) {
	const pause = 30 * time.Millisecond
	pr, pw := io.Pipe()
	go func() {
		for i, ev := range strings.SplitAfter(openAIStream, "\n\n") {
			if ev == "" {
				continue
			}
			if i > 0 {
				time.Sleep(pause)
			}
			_, _ = pw.Write([]byte(ev))
		}
		_ = pw.Close()
	}()
	ttfb, ttlb, complete, err := readStream(pr, formatOpenAI, time.Now())
	if err != nil || !complete {
		t.Fatalf("complete %v err %v", complete, err)
	}
	if ttlb-ttfb < 2*pause {
		t.Errorf("ttlb %v - ttfb %v < %v", ttlb, ttfb, 2*pause)
	}
}

func TestRecordStream(t *testing.T) {
	rec := newRecorder()
	rec.recordStream(200, 2*time.Millisecond, 5*time.Millisecond, true, nil)
	rec.recordStream(200, 3*time.Millisecond, 9*time.Millisecond, false, nil)
	rec.recordStream(503, 0, 4*time.Millisecond, false, nil)
	res := rec.result(modeGateway, time.Second, 0)
	if res.Statuses["200"] != 1 || res.Statuses["incomplete"] != 1 || res.Statuses["503"] != 1 {
		t.Errorf("statuses = %v", res.Statuses)
	}
	if res.TTFBMs == nil || res.TTFBMs.Count != 2 {
		t.Errorf("ttfbMs = %+v, want the two 200s", res.TTFBMs)
	}
	// Latency is time to last byte, for answers that ended.
	if res.LatencyMs.Count != 2 || res.LatencyMs.Max != 5 {
		t.Errorf("latencyMs = %+v", res.LatencyMs)
	}

	plain := newRecorder()
	plain.record(200, time.Millisecond, nil)
	if plain.result(modeGateway, time.Second, 0).TTFBMs != nil {
		t.Error("a non-streaming run reports no ttfbMs")
	}
}

func TestChannelURL(t *testing.T) {
	base := "https://gw:8080"
	if got := channelURL(base, "/channels/perf/ramp-", nil, 4, 7); got != base+"/channels/perf/ramp-0007" {
		t.Errorf("without namespaces: %s", got)
	}
	nss := []string{"a", "b", "c"}
	for i, want := range []string{"/channels/a/s-0000", "/channels/b/s-0001", "/channels/c/s-0002", "/channels/a/s-0003"} {
		if got := channelURL(base, "/channels/{ns}/s-", nss, 4, i); got != base+want {
			t.Errorf("channel %d: %s, want %s", i, got, base+want)
		}
	}
}
