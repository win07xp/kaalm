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

// Command mockprovider is an e2e test double: a stand-in LLM provider the
// gateway can forward to, plus an async-webhook callback receiver. It is NOT
// part of the product; the chart and release workflow never reference it.
//
// It speaks enough of the OpenAI wire format for the gateway's openai-compatible
// adapter. Behavior is keyed by the request path PREFIX, so several
// ModelProviders can point at one Service by giving each a distinct
// spec.endpoint prefix:
//
//	/ok        (default) -> 200 chat completion with non-zero usage
//	/fail                -> 503 (a fallbackable status, drives fallback tests)
//	/bigusage            -> 200 with large usage (drives budget-exhaustion tests)
//	/slow<ms>            -> 200 after <ms> milliseconds (the perf harness's
//	                        realistic-latency provider)
//	/pace<ms>            -> 200; a streamed answer waits <ms> milliseconds
//	                        before each event after the first, so time to
//	                        first byte and time to last byte differ (the perf
//	                        harness's streaming relay check); a non-streamed
//	                        answer is immediate
//	/echo                -> 200 chat completion whose content is the JSON
//	                        {"messages": N, "user": [texts]} built from the
//	                        request, so a spec can see the history a framework
//	                        agent sent; /v1/messages answers as /ok does
//
// It also stands in for Google Vertex, for a google-vertex ModelProvider:
//
//	POST .../token       -> the OAuth2 token endpoint: a JWT-bearer grant
//	                        gets the access token vertexMockToken
//	POST {prefix}/{v1|v1beta1}/projects/{p}/locations/{l}/publishers/google/models/{model}:generateContent
//	                     -> 200 Gemini answer with usageMetadata, given
//	                        Authorization: Bearer vertexMockToken
//	POST ...:streamGenerateContent?alt=sse
//	                     -> the same as two SSE chunks, usage on the last
//	GET .../publishers/google/models
//	                     -> 200 for the controller's google-vertex probe
//
// GET .../v1/models returns 200 for probe compatibility. POST /callback records
// async-webhook deliveries; GET /introspect/callbacks returns them for
// assertions. Every chat call is counted by its endpoint prefix and exposed
// at GET /introspect/requests, so specs can assert that a request never
// reached the upstream (S17's hard-budget proof). Served Vertex calls count
// under their endpoint prefix too, and token grants under "/token".
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// recordedCallback is one async-webhook delivery the mock received.
type recordedCallback struct {
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

type mock struct {
	mu        sync.Mutex
	callbacks []recordedCallback
	// requests counts chat calls by endpoint prefix (the path with the
	// /v1/chat/completions suffix stripped; "/" for a bare path).
	requests map[string]int
}

// chatPrefix reduces a chat path to its counting key.
func chatPrefix(path string) string {
	prefix := strings.TrimSuffix(strings.TrimSuffix(path, "/v1/chat/completions"), "/v1/messages")
	if prefix == "" {
		return "/"
	}
	return prefix
}

const mockReplyText = "ok from mock"

// echoReply is the /echo answer: what the chat request carried.
type echoReply struct {
	Messages int      `json:"messages"`
	User     []string `json:"user"`
}

// replyContent picks the chat-completions answer text for a path: the echo
// JSON under the /echo prefix, the fixed reply otherwise.
func replyContent(path string, parsed map[string]any) string {
	if strings.HasPrefix(path, "/echo") && strings.HasSuffix(path, "/v1/chat/completions") {
		return echoContent(parsed)
	}
	return mockReplyText
}

// echoContent reports the request's message count and its user messages'
// texts in order. An array content (the multi-part form) contributes its
// joined text parts.
func echoContent(parsed map[string]any) string {
	msgs, _ := parsed["messages"].([]any)
	reply := echoReply{Messages: len(msgs), User: []string{}}
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			reply.User = append(reply.User, c)
		case []any:
			var text strings.Builder
			for _, part := range c {
				p, _ := part.(map[string]any)
				if t, _ := p["type"].(string); t == "text" {
					s, _ := p["text"].(string)
					text.WriteString(s)
				}
			}
			reply.User = append(reply.User, text.String())
		}
	}
	out, _ := json.Marshal(reply)
	return string(out)
}

// anthropicMessage is the /v1/messages answer: the Anthropic shape with the
// same usage the chat completion carries (S24 proves the crossing both
// ways against one mock).
func anthropicMessage(model string, in, out int64) []byte {
	body, _ := json.Marshal(map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": model,
		"content":     []any{map[string]any{"type": "text", "text": mockReplyText}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": in, "output_tokens": out},
	})
	return body
}

// pacer sleeps the pacing before every event after the first, so the first
// event goes out at once and the rest are spaced.
type pacer struct {
	pace    time.Duration
	started bool
}

func (p *pacer) wait() {
	if p.started && p.pace > 0 {
		time.Sleep(p.pace)
	}
	p.started = true
}

// streamChat writes a chat-completions SSE stream: the content split at its
// last space into two chunks (one chunk when it has no space), the finish
// chunk, a usage chunk when stream_options asked for one, [DONE]. A nonzero
// pace spaces the events.
func streamChat(w http.ResponseWriter, model, content string, in, out int64, includeUsage bool, pace time.Duration) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	p := &pacer{pace: pace}
	chunk := func(v map[string]any) {
		p.wait()
		raw, _ := json.Marshal(v)
		_, _ = w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
		if f != nil {
			f.Flush()
		}
	}
	created := time.Now().Unix()
	base := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	if i := strings.LastIndexByte(content, ' '); i > 0 {
		chunk(base(map[string]any{"role": "assistant", "content": content[:i]}, nil))
		chunk(base(map[string]any{"content": content[i:]}, nil))
	} else {
		chunk(base(map[string]any{"role": "assistant", "content": content}, nil))
	}
	chunk(base(map[string]any{}, "stop"))
	if includeUsage {
		chunk(map[string]any{"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}})
	}
	p.wait()
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f != nil {
		f.Flush()
	}
}

// streamMessages writes an Anthropic messages SSE stream. A nonzero pace
// spaces the events.
func streamMessages(w http.ResponseWriter, model string, in, out int64, pace time.Duration) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	p := &pacer{pace: pace}
	event := func(name string, v map[string]any) {
		p.wait()
		raw, _ := json.Marshal(v)
		_, _ = w.Write([]byte("event: " + name + "\ndata: " + string(raw) + "\n\n"))
		if f != nil {
			f.Flush()
		}
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": in, "output_tokens": 0}}})
	event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": mockReplyText}})
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": out}})
	event("message_stop", map[string]any{"type": "message_stop"})
}

// chatCompletion is the minimal OpenAI-shaped success body. The gateway reads
// only usage.prompt_tokens / usage.completion_tokens; the rest is relayed to the
// caller verbatim.
func chatCompletion(model, content string, in, out int64) []byte {
	choice := map[string]any{
		"index":         0,
		"finish_reason": "stop",
		"message":       map[string]any{"role": "assistant", "content": content},
	}
	body, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{choice},
		"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out},
	})
	return body
}

// behaviorFor maps a request path to (status, inputTokens, outputTokens). The
// gateway forwards to {endpoint-prefix}{inbound path}, so the prefix selects
// behavior while the inbound path stays /v1/chat/completions.
func behaviorFor(path string) (status int, in, out int64) {
	switch {
	case strings.HasPrefix(path, "/fail"):
		return http.StatusServiceUnavailable, 0, 0
	case strings.HasPrefix(path, "/bigusage"):
		return http.StatusOK, 5_000_000, 5_000_000
	default: // "/ok", "/slow<ms>", "/pace<ms>", "/echo", and anything else
		return http.StatusOK, 11, 22
	}
}

// delayFor reads a simulated upstream latency off a "/slow<ms>" prefix, so the
// perf harness can measure the gateway against a provider that takes realistic
// time to answer. Any other prefix answers immediately.
func delayFor(path string) time.Duration { return prefixMillis(path, "/slow") }

// paceFor reads the spacing between streamed events off a "/pace<ms>"
// prefix. Any other prefix streams every event at once.
func paceFor(path string) time.Duration { return prefixMillis(path, "/pace") }

// prefixMillis reads the positive millisecond count that directly follows
// prefix at the start of path, up to the next slash; 0 when there is none.
func prefixMillis(path, prefix string) time.Duration {
	if !strings.HasPrefix(path, prefix) {
		return 0
	}
	digits := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(digits, '/'); i >= 0 {
		digits = digits[:i]
	}
	ms, err := strconv.Atoi(digits)
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// chat serves both LLM paths: /v1/chat/completions in the OpenAI shape and
// /v1/messages in the Anthropic shape, streaming when the request asks.
func (m *mock) chat(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if m.requests == nil {
		m.requests = map[string]int{}
	}
	m.requests[chatPrefix(r.URL.Path)]++
	m.mu.Unlock()

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	model, _ := parsed["model"].(string)
	if model == "" {
		model = "mock-model"
	}
	stream, _ := parsed["stream"].(bool)
	anthropic := strings.HasSuffix(r.URL.Path, "/v1/messages")

	if d := delayFor(r.URL.Path); d > 0 {
		time.Sleep(d)
	}
	status, in, out := behaviorFor(r.URL.Path)
	if status != http.StatusOK {
		if anthropic {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"mock failure"}}`, status)
			return
		}
		http.Error(w, `{"error":{"message":"mock failure"}}`, status)
		return
	}
	switch {
	case anthropic && stream:
		streamMessages(w, model, in, out, paceFor(r.URL.Path))
	case anthropic:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(anthropicMessage(model, in, out))
	case stream:
		so, _ := parsed["stream_options"].(map[string]any)
		include, _ := so["include_usage"].(bool)
		streamChat(w, model, replyContent(r.URL.Path, parsed), in, out, include, paceFor(r.URL.Path))
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(chatCompletion(model, replyContent(r.URL.Path, parsed), in, out))
	}
}

// vertexMockToken is the access token the mock's token endpoint mints and
// its Vertex calls require.
const vertexMockToken = "mock-vertex-token"

// vertexPrefix returns the endpoint prefix of a Vertex call path (what
// precedes /{version}/projects/), "/" for a bare path, and false when the
// path is not a generateContent or streamGenerateContent call on a google
// model.
func vertexPrefix(path string) (prefix, method string, ok bool) {
	i := strings.Index(path, "/publishers/google/models/")
	if i < 0 {
		return "", "", false
	}
	last := path[strings.LastIndexByte(path, '/')+1:]
	_, method, found := strings.Cut(last, ":")
	if !found || (method != "generateContent" && method != "streamGenerateContent") {
		return "", "", false
	}
	for _, version := range []string{"/v1/projects/", "/v1beta1/projects/"} {
		if j := strings.Index(path[:i], version); j >= 0 {
			if j == 0 {
				return "/", method, true
			}
			return path[:j], method, true
		}
	}
	return "", "", false
}

// token answers a JWT-bearer grant with vertexMockToken, standing in for
// Google's token endpoint. The assertion's signature is not checked.
func (m *mock) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil ||
		r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.PostForm.Get("assertion") == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	m.count("/token")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"` + vertexMockToken + `","token_type":"Bearer","expires_in":3600}`))
}

// vertex serves generateContent and streamGenerateContent (with alt=sse)
// for a caller that presents vertexMockToken.
func (m *mock) vertex(w http.ResponseWriter, r *http.Request, prefix, method string) {
	if r.Header.Get("Authorization") != "Bearer "+vertexMockToken {
		http.Error(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
		return
	}
	stream := method == "streamGenerateContent"
	if stream && r.URL.Query().Get("alt") != "sse" {
		http.Error(w, `{"error":{"code":400,"message":"mock serves only alt=sse streams"}}`, http.StatusBadRequest)
		return
	}
	m.count(prefix)
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	usage := map[string]any{"promptTokenCount": 11, "candidatesTokenCount": 22, "totalTokenCount": 33}
	candidate := func(text string) map[string]any {
		return map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}}
	}
	if !stream {
		c := candidate(mockReplyText)
		c["finishReason"] = "STOP"
		body, _ := json.Marshal(map[string]any{"candidates": []any{c}, "usageMetadata": usage})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	i := strings.LastIndexByte(mockReplyText, ' ')
	last := candidate(mockReplyText[i:])
	last["finishReason"] = "STOP"
	for _, chunk := range []map[string]any{
		{"candidates": []any{candidate(mockReplyText[:i])}},
		{"candidates": []any{last}, "usageMetadata": usage},
	} {
		raw, _ := json.Marshal(chunk)
		_, _ = w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
		if f != nil {
			f.Flush()
		}
	}
}

// count adds one call to a prefix's counter.
func (m *mock) count(prefix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requests == nil {
		m.requests = map[string]int{}
	}
	m.requests[prefix]++
}

// models answers the health probe (GET .../v1/models).
func (m *mock) models(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mock-model","object":"model"}]}`))
}

// callback records an async-webhook delivery (S15) and acks it.
func (m *mock) callback(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	m.mu.Lock()
	m.callbacks = append(m.callbacks, recordedCallback{Path: r.URL.Path, Headers: r.Header, Body: string(body)})
	m.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// introspect returns the recorded callbacks for test assertions.
func (m *mock) introspect(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m.callbacks)
}

// introspectRequests returns the per-prefix chat call counts.
func (m *mock) introspectRequests(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if m.requests == nil {
		_, _ = w.Write([]byte("{}"))
		return
	}
	_ = json.NewEncoder(w).Encode(m.requests)
}

func (m *mock) handler() http.Handler {
	mux := http.NewServeMux()
	// Chat completions and models match on any prefix (the provider endpoint
	// prefix precedes the inbound path).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case (strings.HasSuffix(r.URL.Path, "/v1/chat/completions") || strings.HasSuffix(r.URL.Path, "/v1/messages")) &&
			r.Method == http.MethodPost:
			m.chat(w, r)
		case strings.HasSuffix(r.URL.Path, "/v1/models") && r.Method == http.MethodGet:
			m.models(w, r)
		case strings.HasSuffix(r.URL.Path, "/token") && r.Method == http.MethodPost:
			m.token(w, r)
		case strings.HasSuffix(r.URL.Path, "/publishers/google/models") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"publisherModels":[]}`))
		default:
			if prefix, method, ok := vertexPrefix(r.URL.Path); ok && r.Method == http.MethodPost {
				m.vertex(w, r, prefix, method)
				return
			}
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/callback", m.callback)
	mux.HandleFunc("/introspect/callbacks", m.introspect)
	mux.HandleFunc("/introspect/requests", m.introspectRequests)
	return mux
}

func main() {
	var (
		addr     = flag.String("addr", ":8443", "HTTPS listen address")
		certFile = flag.String("tls-cert", "/var/run/tls/tls.crt", "server certificate")
		keyFile  = flag.String("tls-key", "/var/run/tls/tls.key", "server key")
	)
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	m := &mock{}
	srv := &http.Server{Addr: *addr, Handler: m.handler(), ReadHeaderTimeout: 10 * time.Second}
	logger.Info("mock provider listening", "addr", *addr)
	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}
