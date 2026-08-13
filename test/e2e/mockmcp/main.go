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

// Command mockmcp is an e2e test double: a stand-in MCP tool server the
// gateway's broker forwards to. It is NOT part of the product; the chart and
// release workflow never reference it. Sibling to mockprovider, same idioms.
//
// It speaks the MCP streamable-HTTP revision the broker speaks: JSON-RPC over
// POST with plain-JSON responses. initialize mints an Mcp-Session-Id; every
// later request must present it (an unknown session is the protocol's 404,
// which also exercises the broker's relayed-4xx path if a spec wants it).
// The declared catalog is web_search and fetch_page.
//
// With --require-bearer set, any request without that exact bearer token is
// rejected 401. The S18 spec runs it this way, which makes credential
// injection an intrinsic proof: brokered calls succeed only because the
// gateway injected the Secret from kaalm-system. GET /introspect/requests
// returns per-method and per-tool counters for assertions (the S17
// request-counter pattern).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// tool is one catalog entry in the tools/list wire shape.
type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// catalog is the declared tool set, matching the S18 ToolProvider fixture.
var catalog = []tool{
	{Name: "web_search", Description: "mock web search"},
	{Name: "fetch_page", Description: "mock page fetch"},
}

// rpcRequest is the slice of a JSON-RPC request the mock reads.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mock struct {
	requireBearer string

	mu       sync.Mutex
	nextSess int
	sessions map[string]bool
	methods  map[string]int
	tools    map[string]int
}

func newMock(requireBearer string) *mock {
	return &mock{
		requireBearer: requireBearer,
		sessions:      map[string]bool{},
		methods:       map[string]int{},
		tools:         map[string]int{},
	}
}

// result writes a JSON-RPC result envelope echoing the request id.
func result(w http.ResponseWriter, id json.RawMessage, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": payload})
}

// rpcError writes a JSON-RPC error envelope (HTTP 200, per JSON-RPC).
func rpcError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message}})
}

func (m *mock) rpc(w http.ResponseWriter, r *http.Request) {
	if m.requireBearer != "" && r.Header.Get("Authorization") != "Bearer "+m.requireBearer {
		http.Error(w, "missing or wrong bearer token", http.StatusUnauthorized)
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var msg rpcRequest
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method == "" {
		http.Error(w, "not a JSON-RPC message", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.methods[msg.Method]++
	m.mu.Unlock()

	// Notifications: acceptance is the whole contract.
	if strings.HasPrefix(msg.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if msg.Method == "initialize" {
		m.mu.Lock()
		m.nextSess++
		sess := fmt.Sprintf("mcp-sess-%d", m.nextSess)
		m.sessions[sess] = true
		m.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", sess)
		result(w, msg.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mockmcp", "version": "e2e"},
		})
		return
	}

	// Every non-initialize request must carry a session this server minted.
	// An unknown session is the protocol's 404.
	m.mu.Lock()
	known := m.sessions[r.Header.Get("Mcp-Session-Id")]
	m.mu.Unlock()
	if !known {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}

	switch msg.Method {
	case "ping":
		result(w, msg.ID, map[string]any{})
	case "tools/list":
		result(w, msg.ID, map[string]any{"tools": catalog})
	case "tools/call":
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		for _, t := range catalog {
			if t.Name == params.Name {
				m.mu.Lock()
				m.tools[params.Name]++
				m.mu.Unlock()
				result(w, msg.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "called " + params.Name}},
				})
				return
			}
		}
		rpcError(w, msg.ID, -32602, "unknown tool "+params.Name)
	default:
		rpcError(w, msg.ID, -32601, "method not supported by mockmcp: "+msg.Method)
	}
}

// introspect returns the per-method and per-tool counters.
func (m *mock) introspect(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Methods map[string]int `json:"methods"`
		Tools   map[string]int `json:"tools"`
	}{m.methods, m.tools})
}

func (m *mock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/introspect/requests", m.introspect)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		m.rpc(w, r)
	})
	return mux
}

func main() {
	var (
		addr          = flag.String("addr", ":8443", "HTTPS listen address")
		certFile      = flag.String("tls-cert", "/var/run/tls/tls.crt", "server certificate")
		keyFile       = flag.String("tls-key", "/var/run/tls/tls.key", "server key")
		requireBearer = flag.String("require-bearer", "", "reject requests without this exact bearer token")
	)
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	m := newMock(*requireBearer)
	srv := &http.Server{Addr: *addr, Handler: m.handler(), ReadHeaderTimeout: 10 * time.Second}
	logger.Info("mock MCP server listening", "addr", *addr, "bearer_required", *requireBearer != "")
	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}
