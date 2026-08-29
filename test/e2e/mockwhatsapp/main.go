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

// mockwhatsapp is the e2e stand-in for Meta's WhatsApp Cloud API (S23). It
// plays both halves of the platform: on request it performs the verification
// GET against the gateway (GET /verify) or signs an event with the app secret
// and delivers it (POST /send), returning the gateway's answer; and it serves
// the Graph API surface the gateway replies through (/{phone-number-id}/messages),
// recording every reply for /introspect/replies. POST /control/next queues a
// canned answer for the next reply request.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type recordedReply struct {
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Authorization string          `json:"authorization"`
	Body          json.RawMessage `json:"body"`
	At            time.Time       `json:"at"`
}

type sendRequest struct {
	Path         string          `json:"path"`
	Event        json.RawMessage `json:"event"`
	BadSignature bool            `json:"badSignature"`
}

type gatewayAnswer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type cannedAnswer struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

type mock struct {
	appSecret   string
	gatewayBase string
	client      *http.Client

	mu      sync.Mutex
	replies []recordedReply
	queue   []cannedAnswer
}

func answerJSON(raw []byte) json.RawMessage {
	if json.Valid(raw) {
		return json.RawMessage(raw)
	}
	b, _ := json.Marshal(string(raw))
	return b
}

func (m *mock) relay(w http.ResponseWriter, req *http.Request) {
	resp, err := m.client.Do(req)
	if err != nil {
		http.Error(w, "gateway unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	slog.Info("gateway answered", "path", req.URL.Path, "status", resp.StatusCode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gatewayAnswer{Status: resp.StatusCode, Body: answerJSON(raw)})
}

// verify performs Meta's verification GET: /verify?path=&token=&challenge=.
func (m *mock) verify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("path") == "" {
		http.Error(w, "verify needs path", http.StatusBadRequest)
		return
	}
	target := m.gatewayBase + q.Get("path") + "?" + url.Values{
		"hub.mode": {"subscribe"}, "hub.verify_token": {q.Get("token")}, "hub.challenge": {q.Get("challenge")},
	}.Encode()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.relay(w, req)
}

// send signs the event with the app secret (or a wrong one) and POSTs it.
func (m *mock) send(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" || len(req.Event) == 0 {
		http.Error(w, "send needs path and event", http.StatusBadRequest)
		return
	}
	secret := m.appSecret
	if req.BadSignature {
		secret = "not-the-" + secret
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(req.Event)
	out, err := http.NewRequest(http.MethodPost, m.gatewayBase+req.Path, bytes.NewReader(req.Event))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("User-Agent", "facebookexternalua")
	out.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	m.relay(w, out)
}

// graph records any reply request and answers from the queue.
func (m *mock) graph(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.replies = append(m.replies, recordedReply{
		Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"),
		Body: answerJSON(raw), At: time.Now(),
	})
	ans := cannedAnswer{Status: http.StatusOK, Body: `{"messages":[{"id":"wamid.reply"}]}`}
	if len(m.queue) > 0 {
		ans, m.queue = m.queue[0], m.queue[1:]
	}
	m.mu.Unlock()
	slog.Info("reply received", "method", r.Method, "path", r.URL.Path, "answer", ans.Status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ans.Status)
	_, _ = w.Write([]byte(ans.Body))
}

func (m *mock) controlNext(w http.ResponseWriter, r *http.Request) {
	var ans cannedAnswer
	if err := json.NewDecoder(r.Body).Decode(&ans); err != nil || ans.Status == 0 {
		http.Error(w, "control/next needs status and body", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.queue = append(m.queue, ans)
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (m *mock) introspectReplies(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m.replies)
}

func main() {
	var addr, appSecret, gatewayBase string
	var insecure bool
	flag.StringVar(&addr, "addr", ":8080", "listen address")
	flag.StringVar(&appSecret, "app-secret", "", "the app secret events are signed with")
	flag.StringVar(&gatewayBase, "gateway-base", "https://kaalm-gateway.kaalm-system.svc:8080",
		"the User Gateway listener events are delivered to")
	flag.BoolVar(&insecure, "insecure", true, "skip TLS verification of the gateway (test double)")
	flag.Parse()
	if appSecret == "" {
		slog.Error("app-secret is required")
		os.Exit(2)
	}
	m := &mock{
		appSecret:   appSecret,
		gatewayBase: strings.TrimSuffix(gatewayBase, "/"),
		client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // test double
		}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/verify", m.verify)
	mux.HandleFunc("/send", m.send)
	mux.HandleFunc("/control/next", m.controlNext)
	mux.HandleFunc("/introspect/replies", m.introspectReplies)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", m.graph) // /{phone-number-id}/messages and anything else Graph-shaped

	slog.Info("mock whatsapp listening", "addr", addr, "gateway", m.gatewayBase)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
