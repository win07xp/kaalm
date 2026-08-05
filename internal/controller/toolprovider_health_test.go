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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// mcpTestHandler is a minimal MCP streamable-HTTP server: enough protocol
// for the probe's initialize + tools/list sequence.
func mcpTestHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("mcp handler: undecodable body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26"}}`, *req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"web_search"}]}}`, *req.ID)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}
}

func tpForEndpoint(endpoint string, hc *kaalmv1alpha1.ToolProviderHealthCheck) *kaalmv1alpha1.ToolProvider {
	tp := &kaalmv1alpha1.ToolProvider{}
	tp.Name = "probe-target"
	tp.Spec = kaalmv1alpha1.ToolProviderSpec{Type: "mcp", Endpoint: endpoint, HealthCheck: hc}
	return tp
}

func TestMCPToolHealthChecker_Healthy(t *testing.T) {
	srv := httptest.NewServer(mcpTestHandler(t))
	defer srv.Close()

	res := (&MCPToolHealthChecker{}).Probe(context.Background(), tpForEndpoint(srv.URL, nil), "tok")
	if !res.Healthy || res.AuthFailed || res.Err != nil {
		t.Fatalf("probe = %+v, want Healthy", res)
	}
}

func TestMCPToolHealthChecker_AuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	res := (&MCPToolHealthChecker{}).Probe(context.Background(), tpForEndpoint(srv.URL, nil), "bad")
	if !res.AuthFailed {
		t.Fatalf("probe = %+v, want AuthFailed on 403", res)
	}
}

func TestMCPToolHealthChecker_UnreachableIsErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(nil))
	srv.Close() // reachable address, refused connection

	res := (&MCPToolHealthChecker{}).Probe(context.Background(), tpForEndpoint(srv.URL, nil), "")
	if res.Err == nil || res.Healthy || res.AuthFailed {
		t.Fatalf("probe = %+v, want Err for a refused connection", res)
	}
}

func TestMCPToolHealthChecker_TimeoutHonored(t *testing.T) {
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

	hc := &kaalmv1alpha1.ToolProviderHealthCheck{Enabled: true, TimeoutSeconds: 1}
	start := time.Now()
	res := (&MCPToolHealthChecker{}).Probe(context.Background(), tpForEndpoint(srv.URL, hc), "")
	if res.Err == nil {
		t.Fatalf("probe = %+v, want Err after the timeout", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("probe took %v, want the 1s healthCheck.timeoutSeconds to bound it", elapsed)
	}
}
