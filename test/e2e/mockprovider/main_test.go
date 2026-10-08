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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBehaviorFor(t *testing.T) {
	cases := []struct {
		path       string
		wantStatus int
		wantUsage  bool // expect non-zero token counts
	}{
		{"/ok/v1/chat/completions", http.StatusOK, true},
		{"/v1/chat/completions", http.StatusOK, true}, // default is ok
		{"/fail/v1/chat/completions", http.StatusServiceUnavailable, false},
		{"/bigusage/v1/chat/completions", http.StatusOK, true},
	}
	for _, c := range cases {
		status, in, out := behaviorFor(c.path)
		if status != c.wantStatus {
			t.Errorf("%s: status=%d want %d", c.path, status, c.wantStatus)
		}
		if got := in > 0 && out > 0; got != c.wantUsage {
			t.Errorf("%s: nonzero usage=%v want %v (in=%d out=%d)", c.path, got, c.wantUsage, in, out)
		}
	}
	// bigusage must dwarf ok so budget tests cross the ceiling in few calls.
	_, bigIn, _ := behaviorFor("/bigusage/x")
	_, okIn, _ := behaviorFor("/ok/x")
	if bigIn <= okIn {
		t.Errorf("bigusage input tokens %d not greater than ok %d", bigIn, okIn)
	}
}

func TestDelayForSlowPrefix(t *testing.T) {
	cases := []struct {
		path string
		want time.Duration
	}{
		{"/slow50/v1/chat/completions", 50 * time.Millisecond},
		{"/slow250/leg/v1/messages", 250 * time.Millisecond},
		{"/slow/v1/chat/completions", 0},   // no number
		{"/slow-5/v1/chat/completions", 0}, // not a positive integer
		{"/ok/v1/chat/completions", 0},
		{"/v1/chat/completions", 0},
	}
	for _, c := range cases {
		if got := delayFor(c.path); got != c.want {
			t.Errorf("delayFor(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	// A slow prefix still answers as /ok does.
	if status, in, out := behaviorFor("/slow50/v1/chat/completions"); status != http.StatusOK || in == 0 || out == 0 {
		t.Errorf("slow prefix behavior = (%d, %d, %d), want 200 with usage", status, in, out)
	}
}

func TestChatSuccessCarriesUsage(t *testing.T) {
	m := &mock{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ok/v1/chat/completions",
		strings.NewReader(`{"model":"mock-model","messages":[]}`))
	m.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var body struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Usage.PromptTokens == 0 || body.Usage.CompletionTokens == 0 {
		t.Errorf("usage fields must be non-zero, got %+v", body.Usage)
	}
}

func TestChatFailReturns503(t *testing.T) {
	m := &mock{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/fail/v1/chat/completions", strings.NewReader(`{}`))
	m.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
}

func TestCallbackRecordedAndIntrospected(t *testing.T) {
	m := &mock{}
	post := httptest.NewRecorder()
	m.handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/callback",
		strings.NewReader(`{"requestId":"abc"}`)))
	if post.Code != http.StatusOK {
		t.Fatalf("callback status=%d want 200", post.Code)
	}

	get := httptest.NewRecorder()
	m.handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/introspect/callbacks", nil))
	var recorded []recordedCallback
	if err := json.Unmarshal(get.Body.Bytes(), &recorded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(recorded) != 1 || !strings.Contains(recorded[0].Body, "abc") {
		t.Errorf("callback not recorded: %+v", recorded)
	}
}

func TestRequestCounterAndIntrospection(t *testing.T) {
	m := &mock{}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/s17hard/v1/chat/completions", strings.NewReader(`{}`))
		m.chat(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	m.chat(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	m.introspectRequests(rec, httptest.NewRequest(http.MethodGet, "/introspect/requests", nil))
	var counts map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decoding counts: %v", err)
	}
	if counts["/s17hard"] != 3 || counts["/"] != 1 {
		t.Fatalf("counts = %v, want /s17hard:3 and /:1", counts)
	}
}

func TestMessagesPathSpeaksAnthropic(t *testing.T) {
	m := &mock{}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ok/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "message" || out["model"] != "claude-x" || out["stop_reason"] != "end_turn" {
		t.Errorf("not an Anthropic message: %v", out)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(22) {
		t.Errorf("usage wrong: %v", usage)
	}
	// The counter keys both paths by prefix.
	if m.requests["/ok"] != 1 {
		t.Errorf("request counter = %v", m.requests)
	}
	// /fail answers the Anthropic error envelope.
	resp2, _ := http.Post(srv.URL+"/fail/v1/messages", "application/json", strings.NewReader(`{}`))
	defer func() { _ = resp2.Body.Close() }()
	raw, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 503 || !strings.Contains(string(raw), `"type":"error"`) {
		t.Errorf("fail = %d %s", resp2.StatusCode, raw)
	}
}

func TestStreamingBothShapes(t *testing.T) {
	m := &mock{}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ok/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"g","stream":true,"stream_options":{"include_usage":true},"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	text := string(raw)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(text, `"content":"ok from"`) || !strings.Contains(text, `"finish_reason":"stop"`) ||
		!strings.Contains(text, `"prompt_tokens":11`) || !strings.HasSuffix(strings.TrimSpace(text), "data: [DONE]") {
		t.Errorf("chat stream wrong:\n%s", text)
	}

	resp, err = http.Post(srv.URL+"/ok/v1/messages", "application/json",
		strings.NewReader(`{"model":"c","stream":true,"max_tokens":5,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	text = string(raw)
	for _, want := range []string{"event: message_start", `"input_tokens":11`, "event: content_block_delta",
		`"text":"ok from mock"`, "event: message_delta", `"output_tokens":22`, "event: message_stop"} {
		if !strings.Contains(text, want) {
			t.Errorf("messages stream lacks %q:\n%s", want, text)
		}
	}
}

// postChat sends one chat request to the mock and returns the response.
func postChat(t *testing.T, srv *httptest.Server, path, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(raw)
}

// completionContent decodes a non-streaming chat completion's answer text.
func completionContent(t *testing.T, raw string) string {
	t.Helper()
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || len(body.Choices) == 0 {
		t.Fatalf("not a chat completion (%v): %s", err, raw)
	}
	return body.Choices[0].Message.Content
}

const echoHistory = `{"model":"m","messages":[` +
	`{"role":"user","content":"a"},` +
	`{"role":"assistant","content":"x"},` +
	`{"role":"user","content":[{"type":"text","text":"b"}]}]`

func TestEchoReportsHistory(t *testing.T) {
	srv := httptest.NewServer((&mock{}).handler())
	defer srv.Close()

	resp, raw := postChat(t, srv, "/echo/v1/chat/completions", echoHistory+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200: %s", resp.StatusCode, raw)
	}
	var got echoReply
	if err := json.Unmarshal([]byte(completionContent(t, raw)), &got); err != nil {
		t.Fatalf("content is not the echo JSON: %v", err)
	}
	if got.Messages != 3 || len(got.User) != 2 || got.User[0] != "a" || got.User[1] != "b" {
		t.Errorf("echo = %+v, want {3 [a b]}", got)
	}
	if !strings.Contains(raw, `"prompt_tokens":11`) || !strings.Contains(raw, `"completion_tokens":22`) {
		t.Errorf("echo answer lacks usage: %s", raw)
	}
}

func TestEchoStreams(t *testing.T) {
	srv := httptest.NewServer((&mock{}).handler())
	defer srv.Close()

	_, raw := postChat(t, srv, "/echo/v1/chat/completions",
		echoHistory+`,"stream":true,"stream_options":{"include_usage":true}}`)
	if !strings.HasSuffix(strings.TrimSpace(raw), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]:\n%s", raw)
	}
	var content strings.Builder
	usage := false
	for _, line := range strings.Split(raw, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
		}
		if c.Usage != nil {
			usage = true
		}
	}
	var got echoReply
	if err := json.Unmarshal([]byte(content.String()), &got); err != nil {
		t.Fatalf("streamed content %q is not the echo JSON: %v", content.String(), err)
	}
	if got.Messages != 3 || len(got.User) != 2 || got.User[0] != "a" || got.User[1] != "b" {
		t.Errorf("streamed echo = %+v, want {3 [a b]}", got)
	}
	if !usage {
		t.Errorf("stream lacks a usage chunk:\n%s", raw)
	}
}

func TestEchoEmptyHistory(t *testing.T) {
	srv := httptest.NewServer((&mock{}).handler())
	defer srv.Close()

	_, raw := postChat(t, srv, "/echo/v1/chat/completions", `{"model":"m","messages":[]}`)
	if got := completionContent(t, raw); got != `{"messages":0,"user":[]}` {
		t.Errorf("empty echo = %s, want {\"messages\":0,\"user\":[]}", got)
	}
}

func TestDefaultReplyUnchanged(t *testing.T) {
	srv := httptest.NewServer((&mock{}).handler())
	defer srv.Close()

	for _, path := range []string{"/ok/v1/chat/completions", "/v1/chat/completions"} {
		_, raw := postChat(t, srv, path, echoHistory+`}`)
		if got := completionContent(t, raw); got != mockReplyText {
			t.Errorf("%s content = %q, want %q", path, got, mockReplyText)
		}
	}
	// /echo on the Anthropic path answers as /ok does.
	_, raw := postChat(t, srv, "/echo/v1/messages", `{"model":"c","max_tokens":5,"messages":[]}`)
	if !strings.Contains(raw, `"text":"ok from mock"`) || !strings.Contains(raw, `"type":"message"`) {
		t.Errorf("/echo/v1/messages = %s, want the /ok Anthropic message", raw)
	}
}

func TestChatCarriesCreated(t *testing.T) {
	srv := httptest.NewServer((&mock{}).handler())
	defer srv.Close()

	_, raw := postChat(t, srv, "/ok/v1/chat/completions", `{"model":"m","messages":[]}`)
	var body struct {
		Created int64 `json:"created"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || body.Created <= 0 {
		t.Errorf("chat.completion created = %d (%v), want a positive integer", body.Created, err)
	}

	_, raw = postChat(t, srv, "/ok/v1/chat/completions",
		`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	chunks := 0
	for _, line := range strings.Split(raw, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		chunks++
		var c struct {
			Created int64 `json:"created"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil || c.Created <= 0 {
			t.Errorf("chunk %s: created = %d (%v), want a positive integer", data, c.Created, err)
		}
	}
	if chunks != 4 {
		t.Errorf("stream had %d chunks, want 4 (two content, finish, usage)", chunks)
	}
}

func TestPaceForPrefix(t *testing.T) {
	cases := []struct {
		path string
		want time.Duration
	}{
		{"/pace10/x", 10 * time.Millisecond},
		{"/pace10/paced/v1/chat/completions", 10 * time.Millisecond},
		{"/ok/v1/chat/completions", 0},
		{"/slow50/v1/chat/completions", 0},
		{"/paceX/v1/chat/completions", 0},
		{"/pace/v1/chat/completions", 0},
	}
	for _, c := range cases {
		if got := paceFor(c.path); got != c.want {
			t.Errorf("paceFor(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	// Pacing spaces a stream's events; it never delays the first answer.
	if got := delayFor("/pace10/x"); got != 0 {
		t.Errorf("delayFor(/pace10/x) = %v, want 0", got)
	}
	if status, in, out := behaviorFor("/pace10/v1/chat/completions"); status != http.StatusOK || in == 0 || out == 0 {
		t.Errorf("pace prefix behavior = (%d, %d, %d), want 200 with usage", status, in, out)
	}
}

// TestPacedStreamsSpaceEvents checks that a /pace<ms> stream still carries
// every event through its terminator and takes at least the pacing between
// them. Only the lower bound is asserted, so a slow machine cannot flake it.
func TestPacedStreamsSpaceEvents(t *testing.T) {
	srv := httptest.NewServer((&mock{}).handler())
	defer srv.Close()
	const pace = 30 * time.Millisecond

	start := time.Now()
	_, text := postChat(t, srv, "/pace30/v1/chat/completions",
		`{"model":"g","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	took := time.Since(start)
	if !strings.Contains(text, `"prompt_tokens":11`) || !strings.HasSuffix(strings.TrimSpace(text), "data: [DONE]") {
		t.Errorf("paced chat stream incomplete:\n%s", text)
	}
	if took < 3*pace {
		t.Errorf("paced chat stream took %v, want at least %v", took, 3*pace)
	}

	start = time.Now()
	_, text = postChat(t, srv, "/pace30/v1/messages", `{"model":"c","stream":true,"max_tokens":5,"messages":[]}`)
	took = time.Since(start)
	if !strings.Contains(text, `"output_tokens":22`) || !strings.Contains(text, "event: message_stop") {
		t.Errorf("paced messages stream incomplete:\n%s", text)
	}
	if took < 3*pace {
		t.Errorf("paced messages stream took %v, want at least %v", took, 3*pace)
	}
}
