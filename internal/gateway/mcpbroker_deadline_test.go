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

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// When the upstream timeout ends a response body, the transport can end the
// read with a clean io.EOF instead of the context error. Each relay must
// still report the timeout: these responses carry an upstream request whose
// deadline has passed and a body that stops early without an error.
func TestRelays_UpstreamDeadlineEndingInCleanEOF(t *testing.T) {
	expired := func(t *testing.T) *http.Request {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return httptest.NewRequest(http.MethodPost, "https://tools.example/mcp", nil).WithContext(ctx)
	}
	cut := func(t *testing.T, contentType, partial string) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {contentType}},
			Body:       io.NopCloser(strings.NewReader(partial)),
			Request:    expired(t),
		}
	}

	t.Run("tools/list over SSE", func(t *testing.T) {
		h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
		msg := mcpRequest{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "tools/list"}
		rec := httptest.NewRecorder()
		resp := cut(t, "text/event-stream", "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		_, status, errType, _ := h.server.relayFilteredToolsList(context.Background(), rec, resp, msg, &toolFilter{}, "search")
		if status != http.StatusGatewayTimeout || errType != errToolTimeout {
			t.Fatalf("outcome = (%d, %q), want (504, tool_timeout)", status, errType)
		}
		body := expectMCPErrorBody(t, rec.Result(), http.StatusGatewayTimeout, errToolTimeout)
		if !body.Retryable {
			t.Error("a timeout must be retryable")
		}
	})

	t.Run("buffered JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		_, status, errType, _ := relayMCPBuffered(context.Background(), rec, cut(t, "application/json", `{"jsonrpc":`), 1024, "search")
		if status != http.StatusGatewayTimeout || errType != errToolTimeout {
			t.Fatalf("outcome = (%d, %q), want (504, tool_timeout); a cut body must not be relayed as the upstream's 200", status, errType)
		}
		expectMCPErrorBody(t, rec.Result(), http.StatusGatewayTimeout, errToolTimeout)
	})

	t.Run("stream", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", nil)
		resp := cut(t, "text/event-stream", "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		_, errType, _ := relayMCPStream(rec, req, resp, 1<<20, json.RawMessage("7"), "search")
		if errType != errToolTimeout {
			t.Fatalf("errType = %q, want %q", errType, errToolTimeout)
		}
		if !strings.Contains(rec.Body.String(), errToolTimeout) {
			t.Errorf("stream ended without a tool_timeout error event: %q", rec.Body.String())
		}
	})
}
