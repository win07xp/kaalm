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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const mcpCredentialMsg = "mcp credential unavailable"

// refusedToolHarness seeds the tool route with the ToolProvider's
// credential refused, and a fake clock on the broker's log throttle.
func refusedToolHarness(t *testing.T) (*harness, *time.Time) {
	t.Helper()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h.seedToolRoute()
	delete(h.store.toolCreds, "search")
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	h.server.toolCredentialLog.now = func() time.Time { return clock }
	return h, &clock
}

func sendRefusedToolCall(t *testing.T, h *harness) {
	t.Helper()
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	expectMCPError(t, resp, http.StatusServiceUnavailable, errToolUnavailable)
}

func TestMCPBroker_CredentialRefusalLoggedOncePerMinute(t *testing.T) {
	buf := captureSlog(t)
	h, clock := refusedToolHarness(t)

	sendRefusedToolCall(t, h)
	sendRefusedToolCall(t, h)
	recs := logRecords(t, buf, mcpCredentialMsg)
	if len(recs) != 1 {
		t.Fatalf("want 1 credential record within a minute, got %d (%s)", len(recs), buf.String())
	}
	if rec := recs[0]; rec["level"] != "WARN" || rec["provider"] != "search" {
		t.Errorf("record = %v, want level WARN and provider search", rec)
	}
	if e, _ := recs[0]["error"].(string); !strings.Contains(e, "no credential for search") {
		t.Errorf("error field %q should carry the store's reason", e)
	}
	// The per-call audit record is not paced.
	if got := len(logRecords(t, buf, "mcp call")); got != 2 {
		t.Errorf("want an audit record per refused call, got %d", got)
	}

	*clock = clock.Add(time.Minute)
	sendRefusedToolCall(t, h)
	if got := len(logRecords(t, buf, mcpCredentialMsg)); got != 2 {
		t.Errorf("want a second record after a minute, got %d", got)
	}
}

// A ModelProvider and a ToolProvider can share a name: an LLM refusal
// line must not hold back the tool one.
func TestMCPBroker_CredentialLogSeparateFromLLM(t *testing.T) {
	buf := captureSlog(t)
	h, _ := refusedToolHarness(t)
	h.server.credentialLog.allow("search", credentialLogInterval)

	sendRefusedToolCall(t, h)
	if got := len(logRecords(t, buf, mcpCredentialMsg)); got != 1 {
		t.Fatalf("want 1 tool credential record, got %d", got)
	}
}

// A caller that left is not a credential problem: no line, and no slot
// spent that would hide the next live refusal.
func TestMCPBroker_CancelledContextNotLogged(t *testing.T) {
	buf := captureSlog(t)
	h, _ := refusedToolHarness(t)

	call := func(ctx context.Context, want int) {
		t.Helper()
		body, _ := json.Marshal(mcpCall("web_search"))
		req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ctx = context.WithValue(ctx, callerKey{}, &caller{Namespace: "team-a"})
		rec := httptest.NewRecorder()
		h.server.handleMCPBroker(rec, req.WithContext(ctx))
		if rec.Code != want {
			t.Fatalf("status = %d, want %d (%s)", rec.Code, want, rec.Body)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Nothing is written to a caller that left, so the recorder keeps its
	// default 200.
	call(ctx, http.StatusOK)
	if got := len(logRecords(t, buf, mcpCredentialMsg)); got != 0 {
		t.Fatalf("cancelled request should not log, got %d records", got)
	}
	call(context.Background(), http.StatusServiceUnavailable)
	if got := len(logRecords(t, buf, mcpCredentialMsg)); got != 1 {
		t.Errorf("live request after a cancelled one should log once, got %d records", got)
	}
}

const toolRejectedMsg = "tool server rejected the gateway credential"

// A tool server that rejects the gateway credential is logged once a minute
// per ToolProvider, while the Warning event is recorded on every call (the
// event recorder folds the repeats). The credential-unavailable line has its
// own pacing and does not hold this one back.
func TestMCPBroker_CredentialRejectionLoggedOncePerMinute(t *testing.T) {
	buf := captureSlog(t)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	h.seedToolRoute()
	capture := &eventCapture{}
	h.server.Recorder = capture
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	h.server.toolRejectedLog.now = func() time.Time { return clock }
	h.server.toolCredentialLog.allow("search", credentialLogInterval)

	cert := agentCert(t, h.ca)
	call := func() {
		t.Helper()
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		expectMCPError(t, resp, http.StatusServiceUnavailable, errToolUnavailable)
	}
	call()
	call()
	if got := len(logRecords(t, buf, toolRejectedMsg)); got != 1 {
		t.Fatalf("want 1 rejection record within a minute, got %d (%s)", got, buf.String())
	}
	capture.mu.Lock()
	events := len(capture.reasons)
	capture.mu.Unlock()
	if events != 2 {
		t.Errorf("want a CredentialsInvalid event per call, got %d", events)
	}

	clock = clock.Add(time.Minute)
	call()
	if got := len(logRecords(t, buf, toolRejectedMsg)); got != 2 {
		t.Errorf("want a second record after a minute, got %d", got)
	}
}

// A caller that leaves while the broker reads the credential left; the
// tool server is not at fault.
func TestMCPBroker_CredentialReadCallerGone(t *testing.T) {
	h, _ := refusedToolHarness(t)
	body, _ := json.Marshal(mcpCall("web_search"))
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), callerKey{}, &caller{Namespace: "team-a"}))
	cancel()
	rec := httptest.NewRecorder()
	h.server.handleMCPBroker(rec, req.WithContext(ctx))
	if got := mcpCalls(h, "web_search", toolStatusClientClosed); got != 1 {
		t.Errorf("client_closed counter = %v, want 1", got)
	}
	if got := mcpCalls(h, "web_search", errToolUnavailable); got != 0 {
		t.Errorf("tool_unavailable counter = %v, want 0", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %q to a caller that left", rec.Body)
	}
}
