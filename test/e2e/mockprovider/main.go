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
//
// GET .../v1/models returns 200 for probe compatibility. POST /callback records
// async-webhook deliveries; GET /introspect/callbacks returns them for
// assertions.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
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
}

// chatCompletion is the minimal OpenAI-shaped success body. The gateway reads
// only usage.prompt_tokens / usage.completion_tokens; the rest is relayed to the
// caller verbatim.
func chatCompletion(model string, in, out int64) []byte {
	choice := map[string]any{
		"index":         0,
		"finish_reason": "stop",
		"message":       map[string]any{"role": "assistant", "content": "ok from mock"},
	}
	body, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
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
	default: // "/ok" and anything else
		return http.StatusOK, 11, 22
	}
}

func (m *mock) chat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	model, _ := parsed["model"].(string)
	if model == "" {
		model = "mock-model"
	}

	status, in, out := behaviorFor(r.URL.Path)
	if status != http.StatusOK {
		http.Error(w, `{"error":{"message":"mock failure"}}`, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(chatCompletion(model, in, out))
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

func (m *mock) handler() http.Handler {
	mux := http.NewServeMux()
	// Chat completions and models match on any prefix (the provider endpoint
	// prefix precedes the inbound path).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/chat/completions") && r.Method == http.MethodPost:
			m.chat(w, r)
		case strings.HasSuffix(r.URL.Path, "/v1/models") && r.Method == http.MethodGet:
			m.models(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/callback", m.callback)
	mux.HandleFunc("/introspect/callbacks", m.introspect)
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
