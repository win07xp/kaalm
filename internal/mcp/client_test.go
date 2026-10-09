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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// listResult, when non-empty, is the tools/list result.
	listResult string
	calls      []call
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
			result := m.listResult
			if result == "" {
				result = `{"tools":[{"name":"web_search","description":"search"},{"name":"fetch_page"}]}`
			}
			m.respond(w, req, result)
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

// The SSE line bound is the caller's: a long line parses under a large
// bound and fails at a small one, and a JSON body ignores the bound.
func TestParseResponse_SSELineBound(t *testing.T) {
	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"result":{"blob":%q}}`, strings.Repeat("d", 2<<20))
	stream := "data: " + line + "\n\n"
	if _, err := ParseResponse("text/event-stream", strings.NewReader(stream), []byte("3"), 4<<20); err != nil {
		t.Fatalf("4 MiB bound: %v", err)
	}
	if _, err := ParseResponse("text/event-stream", strings.NewReader(stream), []byte("3"), 1<<20); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("1 MiB bound: err = %v, want bufio.ErrTooLong", err)
	}
	if _, err := ParseResponse("application/json", strings.NewReader(line), []byte("3"), 16); err != nil {
		t.Fatalf("JSON body: %v", err)
	}
}

// largeListResult is a tools/list result with a 2 MiB description.
func largeListResult() string {
	return fmt.Sprintf(`{"tools":[{"name":"web_search","description":%q},{"name":"fetch_page"}]}`,
		strings.Repeat("d", 2<<20))
}

// A zero-value Client reads a catalog over 1 MiB, in either encoding,
// because its default limit is the broker's default cap.
func TestClient_LargeToolsList(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			mock := &mockServer{t: t, sse: sse, listResult: largeListResult()}
			srv := httptest.NewServer(mock.handler())
			defer srv.Close()
			tools := initAndList(t, &Client{Endpoint: srv.URL})
			if len(tools) != 2 || tools[0].Name != "web_search" {
				t.Fatalf("tools = %d entries, want web_search and fetch_page", len(tools))
			}
		})
	}
}

// MaxResponseBytes bounds what the client reads: an answer over it fails
// with a message naming the limit, in either encoding.
func TestClient_ResponseLimit(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			mock := &mockServer{t: t, sse: sse, listResult: largeListResult()}
			srv := httptest.NewServer(mock.handler())
			defer srv.Close()

			c := &Client{Endpoint: srv.URL, MaxResponseBytes: 1 << 20}
			session, err := c.Initialize(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.ListTools(context.Background(), session)
			if err == nil || !strings.Contains(err.Error(), "exceeds 1048576 bytes") {
				t.Fatalf("err = %v, want the 1 MiB limit named", err)
			}

			c = &Client{Endpoint: srv.URL, MaxResponseBytes: 4 << 20}
			if tools := initAndList(t, c); len(tools) != 2 {
				t.Fatalf("tools = %d entries under a 4 MiB limit, want 2", len(tools))
			}
		})
	}

	t.Run("json body exactly at the limit", func(t *testing.T) {
		// A fresh client's first request carries id 1.
		const body = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"web_search"}]}}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, body)
		}))
		defer srv.Close()
		c := &Client{Endpoint: srv.URL, MaxResponseBytes: int64(len(body))}
		if _, err := c.ListTools(context.Background(), Session{}); err != nil {
			t.Fatalf("a body exactly at the limit: %v", err)
		}
	})
}

// ParseResponseRaw returns the bytes the response was decoded from: the
// whole JSON body, or the matching event's data.
func TestParseResponseRaw(t *testing.T) {
	answer := `{"jsonrpc":"2.0","id":3,"result":{"tools":[]}}`
	t.Run("json", func(t *testing.T) {
		body := " " + answer + "\n"
		resp, raw, err := ParseResponseRaw("application/json", strings.NewReader(body), []byte("3"), 1<<20)
		if err != nil || string(raw) != body || string(resp.ID) != "3" {
			t.Fatalf("resp %+v raw %q err %v, want the body as read", resp, raw, err)
		}
	})
	for _, c := range []struct{ name, stream, want string }{
		{"one data line", "data: " + answer + "\n\n", answer},
		{"no space after the colon", "data:" + answer + "\n\n", answer},
		{"one leading space removed", "data:  " + answer + "\n\n", " " + answer},
		{"split across data lines", "data: {\"jsonrpc\":\"2.0\",\"id\":3,\ndata: \"result\":{\"tools\":[]}}\n\n",
			`{"jsonrpc":"2.0","id":3,"result":{"tools":[]}}`},
		{"CRLF lines", "event: message\r\ndata: " + answer + "\r\n\r\n", answer},
		{"earlier events skipped", ": ping\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":9,\"result\":{}}\n\ndata: " + answer + "\n\n", answer},
		{"unterminated last event", "data: " + answer, answer},
	} {
		t.Run("sse/"+c.name, func(t *testing.T) {
			resp, raw, err := ParseResponseRaw("text/event-stream", strings.NewReader(c.stream), []byte("3"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != c.want || string(resp.ID) != "3" {
				t.Errorf("raw %q id %s, want %q", raw, resp.ID, c.want)
			}
		})
	}
}
