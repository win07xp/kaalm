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

// mockdiscord is the e2e stand-in for Discord (S22). It plays both halves of
// the platform: on request (POST /send) it signs an interaction with a
// keypair derived from a fixed seed and delivers it to the gateway the way
// Discord would, returning the gateway's answer; and it serves the Discord
// API surface the gateway replies through (the follow-up webhook and channel
// messages), recording every reply for /introspect/replies. The public key
// for the channel's Secret is derived from the same seed, so the fixture
// needs no runtime coordination.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
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

type recordedReply struct {
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Authorization string          `json:"authorization"`
	Body          json.RawMessage `json:"body"`
	At            time.Time       `json:"at"`
}

type sendRequest struct {
	// Path is the channel path on the gateway, for example
	// /channels/e2e/s22-channel.
	Path string `json:"path"`
	// Interaction is the interaction object to deliver, verbatim.
	Interaction json.RawMessage `json:"interaction"`
	// BadSignature signs with a different key, the way Discord's save-time
	// check does.
	BadSignature bool `json:"badSignature"`
	// TimestampOffsetSeconds shifts the signed timestamp (a stale replay).
	TimestampOffsetSeconds int64 `json:"timestampOffsetSeconds"`
}

type sendResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type mock struct {
	priv        ed25519.PrivateKey
	other       ed25519.PrivateKey
	gatewayBase string
	client      *http.Client

	mu      sync.Mutex
	replies []recordedReply
}

func (m *mock) send(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" || len(req.Interaction) == 0 {
		http.Error(w, "send needs path and interaction", http.StatusBadRequest)
		return
	}
	ts := strconv.FormatInt(time.Now().Unix()+req.TimestampOffsetSeconds, 10)
	key := m.priv
	if req.BadSignature {
		key = m.other
	}
	sig := ed25519.Sign(key, append([]byte(ts), req.Interaction...))
	out, err := http.NewRequest(http.MethodPost, m.gatewayBase+req.Path, bytes.NewReader(req.Interaction))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("User-Agent", "Discord-Interactions/1.0 (+https://discord.com)")
	out.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
	out.Header.Set("X-Signature-Timestamp", ts)
	resp, err := m.client.Do(out)
	if err != nil {
		http.Error(w, "gateway unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	body := json.RawMessage(raw)
	if !json.Valid(raw) {
		body, _ = json.Marshal(string(raw))
	}
	slog.Info("delivered interaction", "path", req.Path, "status", resp.StatusCode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sendResponse{Status: resp.StatusCode, Body: body})
}

// api records any reply request under /api/v10/ and answers like Discord.
func (m *mock) api(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := json.RawMessage(raw)
	if !json.Valid(raw) {
		body, _ = json.Marshal(string(raw))
	}
	m.mu.Lock()
	m.replies = append(m.replies, recordedReply{
		Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body, At: time.Now(),
	})
	m.mu.Unlock()
	slog.Info("reply received", "method", r.Method, "path", r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"1310000000000000000"}`))
}

func (m *mock) introspectReplies(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m.replies)
}

func main() {
	var addr, seedHex, gatewayBase string
	var insecure bool
	flag.StringVar(&addr, "addr", ":8080", "listen address")
	flag.StringVar(&seedHex, "seed-hex", "", "32-byte Ed25519 seed, hex; the public key is derived from it")
	flag.StringVar(&gatewayBase, "gateway-base", "https://kaalm-gateway.kaalm-system.svc:8080",
		"the User Gateway listener interactions are delivered to")
	flag.BoolVar(&insecure, "insecure", true, "skip TLS verification of the gateway (test double)")
	flag.Parse()

	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		slog.Error("seed-hex must be 32 bytes of hex")
		os.Exit(2)
	}
	other := make([]byte, ed25519.SeedSize)
	copy(other, seed)
	other[0] ^= 0xff
	m := &mock{
		priv:        ed25519.NewKeyFromSeed(seed),
		other:       ed25519.NewKeyFromSeed(other),
		gatewayBase: strings.TrimSuffix(gatewayBase, "/"),
		client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // test double
		}},
	}
	pub := hex.EncodeToString(m.priv.Public().(ed25519.PublicKey))

	mux := http.NewServeMux()
	mux.HandleFunc("/send", m.send)
	mux.HandleFunc("/api/v10/", m.api)
	mux.HandleFunc("/introspect/replies", m.introspectReplies)
	mux.HandleFunc("/introspect/public-key", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(pub)) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	slog.Info("mock discord listening", "addr", addr, "publicKey", pub, "gateway", m.gatewayBase)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
