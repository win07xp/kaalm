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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
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

func TestRecorderStampsErrorSamples(t *testing.T) {
	rec := newRecorder()
	before := time.Now().UTC().Truncate(time.Millisecond)
	rec.recordOutcome(outcome{err: errors.New("Post x: EOF")})
	after := time.Now().UTC()
	res := rec.result(modeGateway, time.Second, 0)
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want one sample", res.Errors)
	}
	stamp, msg, ok := strings.Cut(res.Errors[0], " ")
	if !ok || msg != "Post x: EOF" {
		t.Fatalf("sample = %q, want <time> Post x: EOF", res.Errors[0])
	}
	at, err := time.Parse(errorStampLayout, stamp)
	if err != nil {
		t.Fatalf("sample time %q: %v", stamp, err)
	}
	if len(stamp) != len("2006-01-02T15:04:05.000Z") || at.Before(before) || at.After(after) {
		t.Errorf("sample time %v, want a UTC time with milliseconds between %v and %v", stamp, before, after)
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

func TestLegacyToolCallBody(t *testing.T) {
	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(legacyToolCallBody("web_search"), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.JSONRPC != "2.0" || msg.Method != "tools/call" || msg.Params.Name != "web_search" {
		t.Errorf("envelope = %+v", msg)
	}
	if msg.Params.Arguments == nil {
		t.Error("arguments must be an object")
	}
	if msg.Params.Meta != nil {
		t.Errorf("a legacy-era call carries no _meta, got %v", msg.Params.Meta)
	}
}

// legacyMCP plays the broker in front of a legacy-session tool server:
// initialize mints a session id, and every later request must carry one.
type legacyMCP struct {
	mu            sync.Mutex
	sessions      map[string]bool
	initializes   int
	notifications int
	noSession     bool // answer initialize without an Mcp-Session-Id
	initStatus    int  // answer initialize with this status when set
	failures      []string
}

func newLegacyMCP(t *testing.T) (*legacyMCP, *httptest.Server) {
	t.Helper()
	m := &legacyMCP{sessions: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	return m, srv
}

func (m *legacyMCP) fail(w http.ResponseWriter, format string, args ...any) {
	m.failures = append(m.failures, fmt.Sprintf(format, args...))
	http.Error(w, "bad request", http.StatusBadRequest)
}

func (m *legacyMCP) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		m.fail(w, "body is not JSON-RPC: %s", raw)
		return
	}
	if r.Header.Get("Mcp-Method") != "" {
		m.fail(w, "%s carries Mcp-Method", msg.Method)
		return
	}
	switch msg.Method {
	case "initialize":
		m.initializes++
		if m.initStatus != 0 {
			w.WriteHeader(m.initStatus)
			return
		}
		if !m.noSession {
			sess := fmt.Sprintf("wrapped-%d", m.initializes)
			m.sessions[sess] = true
			w.Header().Set("Mcp-Session-Id", sess)
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
	case "notifications/initialized":
		if !m.sessions[r.Header.Get("Mcp-Session-Id")] {
			m.fail(w, "notification without a known session")
			return
		}
		m.notifications++
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		if !m.sessions[r.Header.Get("Mcp-Session-Id")] {
			m.fail(w, "tools/call without a known session")
			return
		}
		if v := r.Header.Get("MCP-Protocol-Version"); v != "2025-03-26" {
			m.fail(w, "tools/call MCP-Protocol-Version = %q", v)
			return
		}
		if strings.Contains(string(msg.Params), "_meta") {
			m.fail(w, "tools/call carries _meta")
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)
	default:
		m.fail(w, "unexpected method %s", msg.Method)
	}
}

func legacyConfig(url string) loadgenConfig {
	return loadgenConfig{toolURL: url, tool: "web_search", legacySession: true}
}

func TestLegacyToolCallerUsesItsSession(t *testing.T) {
	m, srv := newLegacyMCP(t)
	call, err := newLegacyToolCaller(srv.Client(), legacyConfig(srv.URL), "")
	if err != nil {
		t.Fatalf("opening the session: %v", err)
	}
	for i := 0; i < 3; i++ {
		if o := call(); o.err != nil || o.label != "200" {
			t.Errorf("call %d = %+v", i, o)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initializes != 1 || m.notifications != 1 {
		t.Errorf("handshake: %d initialize and %d notifications, want 1 each", m.initializes, m.notifications)
	}
	if len(m.failures) > 0 {
		t.Errorf("server saw: %v", m.failures)
	}
}

func TestLegacyToolCallerNeedsASession(t *testing.T) {
	m, srv := newLegacyMCP(t)
	m.noSession = true
	if _, err := newLegacyToolCaller(srv.Client(), legacyConfig(srv.URL), ""); err == nil ||
		!strings.Contains(err.Error(), "Mcp-Session-Id") {
		t.Errorf("initialize without a session id: err = %v, want one naming Mcp-Session-Id", err)
	}

	m.mu.Lock()
	m.noSession, m.initStatus = false, http.StatusForbidden
	m.mu.Unlock()
	if _, err := newLegacyToolCaller(srv.Client(), legacyConfig(srv.URL), ""); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Errorf("initialize answered 403: err = %v, want one naming the status", err)
	}
}

func TestFixedConcurrencyOpensOneCallerPerWorker(t *testing.T) {
	var opened atomic.Int32
	newCaller := func() (func() outcome, error) {
		opened.Add(1)
		return func() outcome {
			time.Sleep(time.Millisecond)
			return outcome{label: "200", latency: time.Millisecond}
		}, nil
	}
	c := loadgenConfig{concurrency: 3, duration: 50 * time.Millisecond, warmup: 5 * time.Second}
	res, err := fixedConcurrencyCallers(c, modeTools, newCaller)
	if err != nil {
		t.Fatal(err)
	}
	if got := opened.Load(); got != 4 {
		t.Errorf("opened %d callers, want 4 (one for warmup, one per worker)", got)
	}
	if res.Statuses["200"] == 0 {
		t.Errorf("no calls recorded: %+v", res)
	}
}

func TestChannelLoadMapsJobIndexToAgentAndSlot(t *testing.T) {
	c := loadgenConfig{pathPrefix: "/channels/perf/ramp-", pad: 4, perAgent: 2}
	f := fleet{phase: phaseRamp, prefix: "ramp-", namespaces: []string{"perf"}}
	cfg := config{ChannelsPerAgent: 2}
	for j, want := range []string{"ramp-0000", "ramp-0000-c1", "ramp-0001", "ramp-0001-c1"} {
		ch := f.channels(j/2, cfg, "https://cb")[j%2].(*kaalmv1beta1.AgentChannel)
		if ch.Name != want {
			t.Fatalf("fleet channel %d is %s, want %s", j, ch.Name, want)
		}
		if got := channelJobURL(c, "", j); got != ch.Spec.Webhook.Path {
			t.Errorf("job %d: loadgen posts to %s, fleet serves %s at %s", j, got, ch.Name, ch.Spec.Webhook.Path)
		}
	}
	one := loadgenConfig{pathPrefix: "/channels/perf/ramp-", pad: 4, perAgent: 1}
	if got := channelJobURL(one, "https://gw", 7); got != channelURL("https://gw", one.pathPrefix, nil, 4, 7) {
		t.Errorf("one channel per agent: %s, want channelURL's mapping", got)
	}
}

// sweepServer answers channel 0 with 401 twice and then 202, channel 1 with
// 202 at once, and channel 2 with 500 always.
func sweepServer(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hook-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		seen[r.URL.Path]++
		n := seen[r.URL.Path]
		mu.Unlock()
		switch r.URL.Path {
		case "/channels/perf/ramp-0000":
			if n <= 2 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case "/channels/perf/ramp-0001":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSweepServesEveryChannelAfterRetries(t *testing.T) {
	bearer := t.TempDir() + "/token"
	if err := os.WriteFile(bearer, []byte("hook-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, b := sweepServer(t), sweepServer(t)
	c := loadgenConfig{
		pathPrefix: "/channels/perf/ramp-", pad: 4, count: 3, perAgent: 1,
		bearerFile: bearer, targets: []string{a.URL, b.URL}, sweepTimeout: 3 * time.Second,
	}
	res, err := sweepLoad(&http.Client{Timeout: 5 * time.Second}, c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != modeSweep || len(res.Sweep) != 2 {
		t.Fatalf("mode %s with %d targets, want sweep with 2", res.Mode, len(res.Sweep))
	}
	total := 0
	for i, st := range res.Sweep {
		if st.Target != c.targets[i] {
			t.Errorf("target %d is %s, want %s", i, st.Target, c.targets[i])
		}
		if st.Channels != 3 || st.Served != 2 || st.Unserved != 1 {
			t.Errorf("%s: %d channels, %d served, %d unserved; want 3, 2, 1", st.Target, st.Channels, st.Served, st.Unserved)
		}
		if st.AcceptMs.Count != 2 || st.ServedSec <= 0 {
			t.Errorf("%s: %d acceptance samples, servedSec %.3f", st.Target, st.AcceptMs.Count, st.ServedSec)
		}
		if st.Statuses["401"] != 2 || st.Statuses["202"] != 2 || st.Statuses["500"] < 1 {
			t.Errorf("%s: statuses %v", st.Target, st.Statuses)
		}
		if st.Requests != st.Statuses["401"]+st.Statuses["202"]+st.Statuses["500"] {
			t.Errorf("%s: %d requests, statuses %v", st.Target, st.Requests, st.Statuses)
		}
		total += st.Requests
	}
	if res.Requests != total || res.Statuses["202"] != 4 {
		t.Errorf("summary: %d requests (targets %d), statuses %v", res.Requests, total, res.Statuses)
	}
}
