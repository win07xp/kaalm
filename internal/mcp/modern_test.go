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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modernServer answers server/discover and a stateless tools/list, checking
// the request carries what the 2026-07-28 revision requires.
func modernServer(t *testing.T, supported []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("modern server: undecodable body: %v", err)
		}
		if got := r.Header.Get("Mcp-Method"); got != req.Method {
			t.Errorf("Mcp-Method = %q, want %q (required on every modern POST)", got, req.Method)
		}
		if got := r.Header.Get("MCP-Protocol-Version"); got != ModernRevision {
			t.Errorf("MCP-Protocol-Version = %q, want %q", got, ModernRevision)
		}
		if _, ok := req.Params.Meta[metaProtocolVersion]; !ok {
			t.Errorf("%s request carries no %s in _meta", req.Method, metaProtocolVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "server/discover":
			versions, _ := json.Marshal(supported)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":%s,"capabilities":{"tools":{}}}}`,
				req.ID, versions)
		case "tools/list":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"web_search"}],"ttlMs":60000,"cacheScope":"public"}}`,
				req.ID)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, req.ID)
		}
	}))
}

func TestDiscover_ModernServer(t *testing.T) {
	srv := modernServer(t, []string{ModernRevision, "2025-11-25"})
	defer srv.Close()

	c := &Client{Endpoint: srv.URL}
	session, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if session.ProtocolVersion != ModernRevision || session.ID != "" {
		t.Fatalf("session = %+v, want stateless %s", session, ModernRevision)
	}
	tools, err := c.ListTools(context.Background(), session)
	if err != nil || len(tools) != 1 || tools[0].Name != "web_search" {
		t.Fatalf("stateless ListTools = %v, %v", tools, err)
	}
}

func TestDiscover_LegacyServerClassification(t *testing.T) {
	// The 2026-07-28 rule: anything that is not a recognized modern
	// JSON-RPC error identifies a legacy server.
	cases := map[string]http.HandlerFunc{
		"400 with empty body": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		},
		"404 with a -32601 body": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
		},
		"200 with a legacy-shaped error": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"server not initialized"}}`))
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			_, err := (&Client{Endpoint: srv.URL}).Discover(context.Background())
			if !errors.Is(err, ErrLegacyServer) {
				t.Fatalf("Discover err = %v, want ErrLegacyServer", err)
			}
		})
	}
}

func TestDiscover_ModernRejectionIsNotLegacy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":%d,"message":"Unsupported protocol version"}}`,
			CodeUnsupportedProtocolVersion)
	}))
	defer srv.Close()

	_, err := (&Client{Endpoint: srv.URL}).Discover(context.Background())
	if err == nil || errors.Is(err, ErrLegacyServer) {
		t.Fatalf("Discover err = %v, want a modern rejection, not a legacy fallback", err)
	}
}

func TestDiscover_NoMutualVersion(t *testing.T) {
	srv := modernServer(t, []string{"2025-11-25"})
	defer srv.Close()

	_, err := (&Client{Endpoint: srv.URL}).Discover(context.Background())
	if err == nil || errors.Is(err, ErrLegacyServer) || !strings.Contains(err.Error(), "2025-11-25") {
		t.Fatalf("Discover err = %v, want a no-mutual-version error naming the server's list", err)
	}
}

func TestDiscover_AuthErrorStaysHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := (&Client{Endpoint: srv.URL}).Discover(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized || errors.Is(err, ErrLegacyServer) {
		t.Fatalf("Discover err = %v, want the 401 HTTPError, never a legacy fallback", err)
	}
}

func TestIsModern(t *testing.T) {
	for revision, want := range map[string]bool{
		ModernRevision: true, "2027-01-01": true,
		"2025-11-25": false, "2025-03-26": false, "": false,
	} {
		if got := IsModern(revision); got != want {
			t.Errorf("IsModern(%q) = %v, want %v", revision, got, want)
		}
	}
}

func TestLegacyRequestsCarryNoModernHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Mcp-Method"); got != "" {
			t.Errorf("legacy request carries Mcp-Method %q; the header belongs to the modern era", got)
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Params.Meta) != 0 {
			t.Errorf("legacy %s request carries _meta %v", req.Method, req.Params.Meta)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26"}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`, req.ID)
		}
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL}
	session, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.ListTools(context.Background(), session); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
}
