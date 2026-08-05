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

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// call records one request the mock server saw.
type call struct {
	method  string
	headers http.Header
	id      *int64
}

// mockServer is a minimal MCP streamable-HTTP server for tests. sse selects
// the response encoding; sessionID, when non-empty, is issued on initialize
// and required afterward.
type mockServer struct {
	t         *testing.T
	sse       bool
	sessionID string
	calls     []call
}

func (m *mockServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			m.t.Errorf("mock: undecodable body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.calls = append(m.calls, call{method: req.Method, headers: r.Header.Clone(), id: req.ID})

		if m.sessionID != "" && req.Method != "initialize" {
			if got := r.Header.Get("Mcp-Session-Id"); got != m.sessionID {
				m.t.Errorf("mock: %s carried session %q, want %q", req.Method, got, m.sessionID)
			}
		}

		switch req.Method {
		case "initialize":
			if m.sessionID != "" {
				w.Header().Set("Mcp-Session-Id", m.sessionID)
			}
			m.respond(w, req, `{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"mock","version":"1"}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			m.respond(w, req, `{"tools":[{"name":"web_search","description":"search"},{"name":"fetch_page"}]}`)
		default:
			m.t.Errorf("mock: unexpected method %q", req.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}
}

func (m *mockServer) respond(w http.ResponseWriter, req request, result string) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *req.ID, result)
	if m.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		// A ping event first, then an unrelated notification, then the
		// response, exercising the scanner's skip logic.
		_, _ = fmt.Fprint(w, ": ping\n\n")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, body)
}

// initAndList runs the full probe sequence against the mock and returns the
// tools.
func initAndList(t *testing.T, c *Client) []Tool {
	t.Helper()
	session, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := c.ListTools(context.Background(), session)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return tools
}

func TestClient_JSONResponses(t *testing.T) {
	mock := &mockServer{t: t, sessionID: "sess-1"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, Credential: "tok-1"}
	tools := initAndList(t, c)

	if len(tools) != 2 || tools[0].Name != "web_search" || tools[1].Name != "fetch_page" {
		t.Fatalf("tools = %+v, want web_search and fetch_page", tools)
	}
	if len(mock.calls) != 3 {
		t.Fatalf("server saw %d calls, want 3 (initialize, initialized, tools/list)", len(mock.calls))
	}
	for _, cl := range mock.calls {
		if got := cl.headers.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("%s: Authorization = %q, want Bearer tok-1", cl.method, got)
		}
		if got := cl.headers.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("%s: Accept = %q", cl.method, got)
		}
	}
	if mock.calls[1].id != nil {
		t.Errorf("notifications/initialized carried id %d, want none", *mock.calls[1].id)
	}
	// The negotiated protocol version is echoed after initialize.
	for _, cl := range mock.calls[1:] {
		if got := cl.headers.Get("MCP-Protocol-Version"); got != "2025-03-26" {
			t.Errorf("%s: MCP-Protocol-Version = %q, want 2025-03-26", cl.method, got)
		}
	}
}

func TestClient_SSEResponses(t *testing.T) {
	mock := &mockServer{t: t, sse: true, sessionID: "sess-sse"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	tools := initAndList(t, &Client{Endpoint: srv.URL, Credential: "tok"})
	if len(tools) != 2 {
		t.Fatalf("tools over SSE = %+v, want 2 entries", tools)
	}
}

func TestClient_NoCredentialNoSession(t *testing.T) {
	mock := &mockServer{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	tools := initAndList(t, &Client{Endpoint: srv.URL})
	if len(tools) != 2 {
		t.Fatalf("tools = %+v, want 2 entries", tools)
	}
	for _, cl := range mock.calls {
		if _, present := cl.headers["Authorization"]; present {
			t.Errorf("%s: Authorization header present without a credential", cl.method)
		}
		if _, present := cl.headers["Mcp-Session-Id"]; present {
			t.Errorf("%s: Mcp-Session-Id present for a sessionless server", cl.method)
		}
	}
}

func TestClient_AuthRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := (&Client{Endpoint: srv.URL, Credential: "bad"}).Initialize(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want *HTTPError with 401", err)
	}
}

func TestClient_RPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	}))
	defer srv.Close()

	_, err := (&Client{Endpoint: srv.URL}).Initialize(context.Background())
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("err = %v, want *RPCError with code -32601", err)
	}
}

func TestClient_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{not json`)
	}))
	defer srv.Close()

	if _, err := (&Client{Endpoint: srv.URL}).Initialize(context.Background()); err == nil {
		t.Fatal("Initialize succeeded on a malformed body")
	}
}

func TestClient_SSEStreamWithoutResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
	}))
	defer srv.Close()

	if _, err := (&Client{Endpoint: srv.URL}).Initialize(context.Background()); err == nil {
		t.Fatal("Initialize succeeded on a stream that never answered")
	}
}

func TestClient_Timeout(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server's background read notices the client
		// disconnect and cancels r.Context.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	}))
	// LIFO: blocked must close before srv.Close waits on the parked handler.
	defer srv.Close()
	defer close(blocked)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := (&Client{Endpoint: srv.URL}).Initialize(ctx); err == nil {
		t.Fatal("Initialize succeeded past its context deadline")
	}
}
