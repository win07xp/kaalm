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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/mcp"
)

// seedToolRoute installs an agent in team-a granting ToolProvider "search"
// (catalog web_search + fetch_page, narrowed to web_search), the class
// allowlist, the credential, and the source-IP Pod mapping.
func (h *harness) seedToolRoute() {
	h.store.agents["team-a/sup"] = &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Spec: kaalmv1beta1.AgentSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "std"},
			Tools: []kaalmv1beta1.AgentToolGrant{
				{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "search"}, Tools: []string{"web_search"}},
			},
		},
	}
	h.store.classes["std"] = &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "std"},
		Spec: kaalmv1beta1.AgentClassSpec{
			AllowedToolProviders: []kaalmv1beta1.LocalObjectReference{{Name: "search"}},
		},
	}
	h.store.toolProviders["search"] = &kaalmv1beta1.ToolProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "search"},
		Spec: kaalmv1beta1.ToolProviderSpec{
			Type:              "mcp",
			Endpoint:          h.upstream.URL,
			CredentialsRef:    &kaalmv1beta1.SecretKeyReference{Name: "search-key", Key: "token"},
			AllowedNamespaces: []string{"team-*"},
			Tools: []kaalmv1beta1.ToolProviderTool{
				{ID: "web_search"}, {ID: "fetch_page"},
			},
		},
	}
	h.store.toolCreds["search"] = "tool-cred-1"
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sup-abc", Namespace: "team-a"},
	}
}

func mcpCall(tool string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{}},
	}
}

// expectMCPError asserts the broker denied with the given status and
// error.type, returning the decoded body message.
func expectMCPError(t *testing.T, resp *http.Response, status int, errType string) string {
	t.Helper()
	return expectMCPErrorBody(t, resp, status, errType).Message
}

// expectMCPErrorBody is expectMCPError returning the whole decoded error.
func expectMCPErrorBody(t *testing.T, resp *http.Response, status int, errType string) errorBody {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != status {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, body)
	}
	raw, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Error errorBody `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if envelope.Error.Type != errType {
		t.Fatalf("error.type = %q, want %q (raw body: %s)", envelope.Error.Type, errType, raw)
	}
	return envelope.Error
}

func TestMCPBroker_MTLSHappyPath(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "up-sess-1")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"ok"}]}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"),
		map[string]string{"x-api-key": "attacker-supplied", "Authorization": "Bearer stolen"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	up := <-h.upreqs
	if got := up.header.Get("Authorization"); got != "Bearer tool-cred-1" {
		t.Errorf("tool credential not injected: %q", got)
	}
	if up.header.Get("X-Api-Key") != "" {
		t.Error("inbound auth material must be stripped")
	}

	// The upstream session id is wrapped, never revealed.
	wrapped := resp.Header.Get("Mcp-Session-Id")
	if wrapped == "" || strings.Contains(wrapped, "up-sess-1") {
		t.Fatalf("session id not wrapped: %q", wrapped)
	}
	identity := callerIdentity(&caller{Namespace: "team-a",
		Workload: &Identity{Namespace: "team-a", Name: "sup", Kind: KindAgent}})
	if raw, ok := unwrapSessionID([]byte("test-session-key"), wrapped, identity); !ok || raw != "up-sess-1" {
		t.Fatalf("wrapped session does not verify for the caller: %q %v", raw, ok)
	}

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["result"] == nil {
		t.Fatalf("result not relayed: %v", body)
	}
}

func TestMCPBroker_BearerTierHappyPath(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	})
	h.seedToolRoute()
	h.reviewer.username = "system:serviceaccount:team-b:runner"
	h.reviewer.authenticated = true
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "team-b"},
	}

	// team-b matches team-*; gateway-only callers reduce to the namespace
	// check and may call any cataloged tool, including uncataloged narrowings
	// no workload grant exists for.
	resp := postJSON(t, h.client(nil), h.url("/v1/mcp/search"), mcpCall("fetch_page"),
		map[string]string{"Authorization": "Bearer projected-token"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("bearer tier status %d: %s", resp.StatusCode, body)
	}
	up := <-h.upreqs
	if got := up.header.Get("Authorization"); got != "Bearer tool-cred-1" {
		t.Errorf("credential not injected on bearer tier: %q", got)
	}
}

func TestMCPBroker_EnforcementMatrix(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)
	cl := h.client(&cert)

	// Unknown provider.
	resp := postJSON(t, cl, h.url("/v1/mcp/nope"), mcpCall("web_search"), nil)
	expectMCPError(t, resp, http.StatusBadRequest, errInvalidRequest)

	// Namespace not admitted.
	h.store.toolProviders["search"].Spec.AllowedNamespaces = []string{"prod-only"}
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	expectMCPError(t, resp, http.StatusForbidden, errAccessDenied)

	// Empty allowedNamespaces denies every namespace.
	h.store.toolProviders["search"].Spec.AllowedNamespaces = nil
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	expectMCPError(t, resp, http.StatusForbidden, errAccessDenied)
	h.store.toolProviders["search"].Spec.AllowedNamespaces = []string{"team-*"}

	// No grant on the workload.
	h.store.agents["team-a/sup"].Spec.Tools = nil
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	msg := expectMCPError(t, resp, http.StatusForbidden, errAccessDenied)
	if !strings.Contains(msg, "no tool grant") {
		t.Errorf("message = %q, want the missing-grant explanation", msg)
	}
	h.store.agents["team-a/sup"].Spec.Tools = []kaalmv1beta1.AgentToolGrant{
		{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "search"}, Tools: []string{"web_search"}},
	}

	// Class allowlist miss.
	h.store.classes["std"].Spec.AllowedToolProviders = nil
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	expectMCPError(t, resp, http.StatusForbidden, errAccessDenied)
	h.store.classes["std"].Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "search"}}

	// Class namespace miss (rule 47).
	h.store.classes["std"].Spec.AllowedNamespaces = []string{"prod-only"}
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	msg = expectMCPError(t, resp, http.StatusForbidden, errAccessDenied)
	if !strings.Contains(msg, "allowedNamespaces") {
		t.Errorf("message = %q, want the class allowedNamespaces explanation", msg)
	}
	h.store.classes["std"].Spec.AllowedNamespaces = nil

	// Narrowing miss: fetch_page is cataloged but not granted.
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("fetch_page"), nil)
	expectMCPError(t, resp, http.StatusForbidden, errToolDenied)

	// Catalog ceiling: a granted-but-uncataloged tool is denied.
	h.store.agents["team-a/sup"].Spec.Tools[0].Tools = []string{"rogue_tool"}
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("rogue_tool"), nil)
	expectMCPError(t, resp, http.StatusForbidden, errToolDenied)
	h.store.agents["team-a/sup"].Spec.Tools[0].Tools = []string{"web_search"}

	// Disallowed method.
	resp = postJSON(t, cl, h.url("/v1/mcp/search"),
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"}, nil)
	msg = expectMCPError(t, resp, http.StatusForbidden, errToolDenied)
	if !strings.Contains(msg, "resources/list") {
		t.Errorf("message = %q, want it to name the method", msg)
	}

	// Batch requests.
	raw, _ := json.Marshal([]any{mcpCall("web_search")})
	req, _ := http.NewRequest(http.MethodPost, h.url("/v1/mcp/search"), strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	batchResp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	expectMCPError(t, batchResp, http.StatusBadRequest, errInvalidRequest)
}

func TestMCPBroker_ToolsListFiltered(t *testing.T) {
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			list := `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"web_search"},{"name":"fetch_page"},{"name":"hidden"}]}}`
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				if mode == "sse" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, ": ping\n\ndata: %s\n\n", list)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, list)
			})
			h.seedToolRoute()
			cert := agentCert(t, h.ca)

			resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"),
				map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("filtered tools/list must be normalized to JSON, got %q", ct)
			}
			var parsed struct {
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				t.Fatal(err)
			}
			// Grant narrows to web_search; "hidden" is uncataloged and
			// ungranted; fetch_page is cataloged but ungranted.
			if len(parsed.Result.Tools) != 1 || parsed.Result.Tools[0].Name != "web_search" {
				t.Fatalf("filtered tools = %+v, want exactly web_search", parsed.Result.Tools)
			}
		})
	}
}

func TestMCPBroker_ToolsListFullForBearerTier(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"web_search"},{"name":"fetch_page"}]}}`
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, list)
	})
	h.seedToolRoute()
	h.reviewer.username = "system:serviceaccount:team-b:runner"
	h.reviewer.authenticated = true
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "team-b"},
	}

	resp := postJSON(t, h.client(nil), h.url("/v1/mcp/search"),
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"},
		map[string]string{"Authorization": "Bearer projected-token"})
	defer func() { _ = resp.Body.Close() }()
	var parsed struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Result.Tools) != 2 {
		t.Fatalf("bearer tier sees %d tools, want the full catalog of 2", len(parsed.Result.Tools))
	}
}

// toolsListUpstreamError is an upstream tools/list answer carrying a
// JSON-RPC error with data.
const toolsListUpstreamError = `{"jsonrpc":"2.0","id":3,"error":{"code":-32022,` +
	`"message":"unsupported protocol version","data":{"supported":["2025-06-18"]}}}`

// relayToolsList serves upstream as the tools/list answer, in JSON or as one
// SSE event, and returns the broker's reply body after checking it is a 200
// normalized to JSON.
func relayToolsList(t *testing.T, mode, upstream string) []byte {
	t.Helper()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		if mode == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", upstream)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, upstream)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"),
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, nil)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	return raw
}

// An upstream JSON-RPC error on tools/list reaches the caller whole, its
// data included.
func TestMCPBroker_ToolsListRelaysUpstreamError(t *testing.T) {
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			raw := relayToolsList(t, mode, toolsListUpstreamError)
			var got struct {
				Error struct {
					Code    int             `json:"code"`
					Message string          `json:"message"`
					Data    json.RawMessage `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if got.Error.Code != -32022 || got.Error.Message != "unsupported protocol version" {
				t.Errorf("error = %+v, want code -32022 and the upstream message", got.Error)
			}
			if string(got.Error.Data) != `{"supported":["2025-06-18"]}` {
				t.Errorf("error.data = %s, want the upstream data unchanged (body: %s)", got.Error.Data, raw)
			}
		})
	}
}

// A re-encoded tools/list answer carries result or error, never both:
// JSON-RPC 2.0 allows one, and strict MCP clients reject the pair.
func TestMCPBroker_ToolsListAnswerCarriesOneMember(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"web_search"},{"name":"fetch_page"}]}}`
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			for _, c := range []struct {
				name, upstream, want, absent string
			}{
				{"success", list, "result", "error"},
				{"error", toolsListUpstreamError, "error", "result"},
			} {
				var members map[string]json.RawMessage
				raw := relayToolsList(t, mode, c.upstream)
				if err := json.Unmarshal(raw, &members); err != nil {
					t.Fatalf("%s: decode %s: %v", c.name, raw, err)
				}
				if _, ok := members[c.want]; !ok {
					t.Errorf("%s: reply has no %q member: %s", c.name, c.want, raw)
				}
				if _, ok := members[c.absent]; ok {
					t.Errorf("%s: reply carries %q too: %s", c.name, c.absent, raw)
				}
			}
		})
	}
}

// A tools/list answer whose result is null carries no tools to filter: a
// narrowed caller gets it relayed, not a dropped connection.
func TestMCPBroker_ToolsListNullResultIsRelayed(t *testing.T) {
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			raw := relayToolsList(t, mode, `{"jsonrpc":"2.0","id":3,"result":null}`)
			var got map[string]json.RawMessage
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if string(got["id"]) != "3" {
				t.Errorf("id = %s, want 3 (body: %s)", got["id"], raw)
			}
			if r, ok := got["result"]; ok && string(r) != "null" {
				t.Errorf("result = %s, want null or absent (body: %s)", r, raw)
			}
		})
	}
}

// An answer carrying an error member alongside its result still has the
// result filtered: the caller never sees a tool its grant leaves out.
func TestMCPBroker_ToolsListFilteredEvenWithAnError(t *testing.T) {
	upstream := `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"partial"},` +
		`"result":{"tools":[{"name":"web_search"},{"name":"fetch_page"},{"name":"admin_reset"}]}}`
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			raw := relayToolsList(t, mode, upstream)
			var got struct {
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
				Error *mcp.RPCError `json:"error"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if len(got.Result.Tools) != 1 || got.Result.Tools[0].Name != "web_search" {
				t.Errorf("tools = %+v, want only the granted web_search (body: %s)", got.Result.Tools, raw)
			}
			if got.Error == nil || got.Error.Code != -32000 {
				t.Errorf("error = %+v, want the upstream error relayed (body: %s)", got.Error, raw)
			}
		})
	}
}

func TestMCPBroker_SessionOwnership(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":7,"result":{"echoSession":%q}}`, r.Header.Get("Mcp-Session-Id"))
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)
	identity := callerIdentity(&caller{Namespace: "team-a",
		Workload: &Identity{Namespace: "team-a", Name: "sup", Kind: KindAgent}})

	// A wrapped id round-trips to the raw upstream id.
	wrapped := wrapSessionID([]byte("test-session-key"), "up-sess-9", identity)
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"),
		map[string]string{"Mcp-Session-Id": wrapped})
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Result struct {
			EchoSession string `json:"echoSession"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Result.EchoSession != "up-sess-9" {
		t.Fatalf("upstream saw session %q, want the raw id", body.Result.EchoSession)
	}

	// Another caller's wrapped id is rejected before forwarding.
	other := wrapSessionID([]byte("test-session-key"), "up-sess-9",
		callerIdentity(&caller{Namespace: "team-a", Workload: &Identity{Namespace: "team-a", Name: "other", Kind: KindAgent}}))
	resp2 := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"),
		map[string]string{"Mcp-Session-Id": other})
	expectMCPError(t, resp2, http.StatusForbidden, errAccessDenied)

	// Garbage is rejected.
	resp3 := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"),
		map[string]string{"Mcp-Session-Id": "garbage"})
	expectMCPError(t, resp3, http.StatusForbidden, errAccessDenied)
}

func TestMCPBroker_SSEStreamRelayed(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\n\n")
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content type = %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "notifications/progress") || !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("stream not relayed intact: %s", raw)
	}
}

// A tool stream's event (a data line and a blank line) goes out in one
// flush, not one per line.
func TestRelayMCPStream_FlushesOncePerEventBoundary(t *testing.T) {
	events := mcpStreamEvents(9) // ten events
	w := newCountingWriter()
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", nil)
	if _, errType, _ := relayMCPStream(w, req, sseResponse(events), 1<<20, json.RawMessage("7"), "search"); errType != "" {
		t.Fatalf("errType = %q", errType)
	}
	if w.flushes != len(events) {
		t.Errorf("flushes = %d, want one per event (%d)", w.flushes, len(events))
	}
}

// A complete tool event reaches the caller while the tool server is still
// sending the next one.
func TestRelayMCPStream_CompleteEventNotHeldForTheNext(t *testing.T) {
	eventA := `data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}` + "\n\n"
	for _, c := range []struct{ name, next string }{
		{"next event line", "event: message\n"},
		{"partial next line", `data: {"js`},
	} {
		t.Run(c.name, func(t *testing.T) {
			release := make(chan struct{})
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, eventA+c.next)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			defer close(release)
			h.seedToolRoute()
			cert := agentCert(t, h.ca)

			resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
			defer func() { _ = resp.Body.Close() }()
			got := make(chan string, 1)
			go func() {
				br := bufio.NewReader(resp.Body)
				var seen strings.Builder
				for {
					line, err := br.ReadString('\n')
					seen.WriteString(line)
					if err != nil || line == "\n" {
						got <- seen.String()
						return
					}
				}
			}()
			select {
			case text := <-got:
				if text != eventA {
					t.Errorf("first event = %q, want %q", text, eventA)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the complete event was held until the tool server sent more")
			}
		})
	}
}

func TestMCPBroker_UpstreamFailureMapping(t *testing.T) {
	t.Run("refused connection", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
		h.seedToolRoute()
		h.upstream.Close() // reachable address, refused connection
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		msg := expectMCPError(t, resp, http.StatusServiceUnavailable, errToolUnavailable)
		// The tool server's address is platform tier: the caller gets a
		// fixed message, and the transport error stays in the audit log.
		if want := `tool provider "search" is unreachable`; msg != want {
			t.Errorf("message = %q, want %q", msg, want)
		}
	})

	t.Run("upstream 500", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		expectMCPError(t, resp, http.StatusServiceUnavailable, errToolUnavailable)
	})

	t.Run("upstream 401 masks the gateway credential", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		expectMCPError(t, resp, http.StatusServiceUnavailable, errToolUnavailable)
	})

	// A rejected gateway credential is an operator problem: the broker
	// records a Warning CredentialsInvalid event on the ToolProvider, as the
	// LLM path does on the ModelProvider.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("upstream %d records CredentialsInvalid on the ToolProvider", status), func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})
			capture := &eventCapture{}
			h.server.Recorder = capture
			h.seedToolRoute()
			cert := agentCert(t, h.ca)
			resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
			expectMCPError(t, resp, http.StatusServiceUnavailable, errToolUnavailable)

			capture.mu.Lock()
			defer capture.mu.Unlock()
			if len(capture.reasons) != 1 || capture.reasons[0] != kaalmv1beta1.ReasonCredentialsInvalid {
				t.Fatalf("event reasons = %v, want one %s", capture.reasons, kaalmv1beta1.ReasonCredentialsInvalid)
			}
			tp, ok := capture.objects[0].(*kaalmv1beta1.ToolProvider)
			if !ok || tp.Name != "search" {
				t.Fatalf("event object = %#v, want ToolProvider search", capture.objects[0])
			}
			if want := fmt.Sprintf("tool server returned %d", status); !strings.Contains(capture.messages[0], want) {
				t.Errorf("event message = %q, want it to contain %q", capture.messages[0], want)
			}
		})
	}

	t.Run("upstream 404 relayed for session semantics", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want the upstream 404 relayed (MCP expired-session signal)", resp.StatusCode)
		}
	})

	t.Run("redirect refused", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://attacker.example/", http.StatusFound)
		})
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		expectMCPError(t, resp, http.StatusServiceUnavailable, errToolUnavailable)
	})

	t.Run("timeout", func(t *testing.T) {
		blocked := make(chan struct{})
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-blocked:
			case <-r.Context().Done():
			}
		})
		// LIFO: blocked must close before the harness cleanup waits on the
		// parked handler.
		t.Cleanup(func() { close(blocked) })
		h.server.Config.MCPUpstreamTimeout = 200 * time.Millisecond
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		expectMCPError(t, resp, http.StatusGatewayTimeout, errToolTimeout)
	})
}

// Errors the buffered relay raises name the provider, as every broker error
// does.
func TestMCPBroker_BufferedRelayErrors(t *testing.T) {
	t.Run("response too large names the provider", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":7,"result":{"blob":%q}}`, strings.Repeat("y", 4096))
		})
		h.server.Config.MCPMaxBodyBytes = 1024
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		body := expectMCPErrorBody(t, resp, http.StatusRequestEntityTooLarge, errResponseTooLarge)
		if body.Provider != "search" {
			t.Errorf("provider = %q, want search", body.Provider)
		}
		if body.Retryable {
			t.Error("response_too_large must not be retryable")
		}
	})

	// The upstream timeout covers the response body too: a tool server
	// that sends headers and then stalls is a timeout, not unavailable.
	t.Run("response read times out", func(t *testing.T) {
		blocked := make(chan struct{})
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"jsonrpc":`)
			w.(http.Flusher).Flush()
			select {
			case <-blocked:
			case <-r.Context().Done():
			}
		})
		// LIFO: blocked must close before the harness cleanup waits on the
		// parked handler.
		t.Cleanup(func() { close(blocked) })
		h.server.Config.MCPUpstreamTimeout = 200 * time.Millisecond
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		retryAfter := resp.Header.Get("Retry-After")
		body := expectMCPErrorBody(t, resp, http.StatusGatewayTimeout, errToolTimeout)
		if body.Provider != "search" || !body.Retryable || retryAfter != "" {
			t.Errorf("envelope = %+v, Retry-After %q; want provider search, retryable, no Retry-After", body, retryAfter)
		}
		if got := mcpCalls(h, "web_search", errToolTimeout); got != 1 {
			t.Errorf("tool_timeout calls = %v, want 1", got)
		}
	})

	// The tools/list relay parses the answer before it relays it; a read
	// that hits the upstream timeout there is a timeout too, in either
	// encoding.
	for _, enc := range []struct{ name, ct, partial string }{
		{"tools/list read times out json", "application/json", `{"jsonrpc":`},
		{"tools/list read times out sse", "text/event-stream",
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"},
	} {
		t.Run(enc.name, func(t *testing.T) {
			blocked := make(chan struct{})
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", enc.ct)
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, enc.partial)
				w.(http.Flusher).Flush()
				select {
				case <-blocked:
				case <-r.Context().Done():
				}
			})
			t.Cleanup(func() { close(blocked) })
			h.server.Config.MCPUpstreamTimeout = 200 * time.Millisecond
			h.seedToolRoute()
			cert := agentCert(t, h.ca)
			resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"),
				map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, nil)
			retryAfter := resp.Header.Get("Retry-After")
			body := expectMCPErrorBody(t, resp, http.StatusGatewayTimeout, errToolTimeout)
			if body.Provider != "search" || !body.Retryable || retryAfter != "" {
				t.Errorf("envelope = %+v, Retry-After %q; want provider search, retryable, no Retry-After", body, retryAfter)
			}
		})
	}
}

// TestRelayMCPBuffered_ReadFailure drives the read-error branch directly: a
// connection reset mid-body is timing-dependent over a real socket.
func TestRelayMCPBuffered_ReadFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(iotest.ErrReader(&net.OpError{Op: "read", Net: "tcp",
			Addr: &net.TCPAddr{IP: net.IPv4(10, 43, 7, 9), Port: 8080}, Err: syscall.ECONNRESET})),
	}
	_, status, errType, detail := relayMCPBuffered(context.Background(), rec, resp, 1024, "search")
	if status != http.StatusServiceUnavailable || errType != errToolUnavailable {
		t.Fatalf("outcome = (%d, %q), want (503, tool_unavailable)", status, errType)
	}
	body := expectMCPErrorBody(t, rec.Result(), http.StatusServiceUnavailable, errToolUnavailable)
	if body.Provider != "search" {
		t.Errorf("provider = %q, want search", body.Provider)
	}
	if !body.Retryable {
		t.Error("a read failure must be retryable")
	}
	// The transport error names the tool server's address, which is
	// platform tier: it goes to the audit detail, not to the caller.
	if strings.Contains(body.Message, "10.43.7.9") {
		t.Errorf("caller message leaks the tool server address: %q", body.Message)
	}
	if !strings.Contains(detail, "10.43.7.9") {
		t.Errorf("audit detail = %q, want the transport error", detail)
	}
}

// retryable is set per cause, not by status: two 503s can disagree, and a
// 504 timeout is retryable although it carries no Retry-After.
func TestMCPBroker_RetryablePerCause(t *testing.T) {
	okResult := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	}
	toolsList := map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/list"}
	cases := []struct {
		name       string
		upstream   http.HandlerFunc
		setup      func(t *testing.T, h *harness, cl *http.Client)
		path       string
		body       map[string]any
		status     int
		errType    string
		retryable  bool
		retryAfter string
	}{
		{name: "unknown provider", upstream: okResult, path: "/v1/mcp/nope",
			status: http.StatusBadRequest, errType: errInvalidRequest},
		{name: "tool not granted", upstream: okResult, body: mcpCall("fetch_page"),
			status: http.StatusForbidden, errType: errToolDenied},
		{name: "rate limited", upstream: okResult,
			setup: func(t *testing.T, h *harness, cl *http.Client) {
				now := time.Now()
				h.server.RateLimiter.now = func() time.Time { return now }
				h.store.toolProviders["search"].Spec.RateLimits = kaalmv1beta1.ToolProviderRateLimits{RequestsPerMinute: 1}
				first := postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
				_ = first.Body.Close()
			},
			status: http.StatusTooManyRequests, errType: errRateLimited, retryable: true, retryAfter: "60"},
		{name: "credential unreadable", upstream: okResult,
			setup:  func(_ *testing.T, h *harness, _ *http.Client) { delete(h.store.toolCreds, "search") },
			status: http.StatusServiceUnavailable, errType: errToolUnavailable, retryable: true},
		{name: "refused connection", upstream: okResult,
			setup:  func(_ *testing.T, h *harness, _ *http.Client) { h.upstream.Close() },
			status: http.StatusServiceUnavailable, errType: errToolUnavailable, retryable: true, retryAfter: "1"},
		{name: "upstream 5xx", upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, status: http.StatusServiceUnavailable, errType: errToolUnavailable, retryable: true, retryAfter: "1"},
		{name: "upstream rejects the gateway credential", upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}, status: http.StatusServiceUnavailable, errType: errToolUnavailable},
		{name: "unparseable tools/list", upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `not json`)
		}, body: toolsList, status: http.StatusServiceUnavailable, errType: errToolUnavailable},
		{name: "timeout", upstream: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		}, setup: func(_ *testing.T, h *harness, _ *http.Client) {
			h.server.Config.MCPUpstreamTimeout = 100 * time.Millisecond
		}, status: http.StatusGatewayTimeout, errType: errToolTimeout, retryable: true},
		{name: "request too large", upstream: okResult,
			setup:  func(_ *testing.T, h *harness, _ *http.Client) { h.server.Config.MCPMaxBodyBytes = 16 },
			status: http.StatusRequestEntityTooLarge, errType: errRequestTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.upstream)
			h.seedToolRoute()
			cert := agentCert(t, h.ca)
			cl := h.client(&cert)
			if tc.setup != nil {
				tc.setup(t, h, cl)
			}
			path, body := tc.path, tc.body
			if path == "" {
				path = "/v1/mcp/search"
			}
			if body == nil {
				body = mcpCall("web_search")
			}
			resp := postJSON(t, cl, h.url(path), body, nil)
			defer func() { _ = resp.Body.Close() }()
			var envelope struct {
				Error errorBody `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&envelope)
			if resp.StatusCode != tc.status || envelope.Error.Type != tc.errType {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, envelope.Error.Type, tc.status, tc.errType)
			}
			if envelope.Error.Retryable != tc.retryable {
				t.Errorf("retryable = %v, want %v", envelope.Error.Retryable, tc.retryable)
			}
			if got := resp.Header.Get("Retry-After"); got != tc.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
		})
	}
}

func TestMCPBroker_SizeCaps(t *testing.T) {
	t.Run("request too large", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
		h.server.Config.MCPMaxBodyBytes = 128
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		big := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "web_search", "arguments": strings.Repeat("x", 512)}}
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), big, nil)
		expectMCPError(t, resp, http.StatusRequestEntityTooLarge, errRequestTooLarge)
	})

	t.Run("response too large", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":7,"result":{"blob":%q}}`, strings.Repeat("y", 4096))
		})
		h.server.Config.MCPMaxBodyBytes = 1024
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		// A small request under the cap; the response is what exceeds it.
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		expectMCPError(t, resp, http.StatusRequestEntityTooLarge, errResponseTooLarge)
	})

	// An oversized tools/list, in either encoding, is the cap and not a
	// parse failure: 413, not 503.
	for _, mode := range []string{"json", "sse"} {
		t.Run("tools/list response too large/"+mode, func(t *testing.T) {
			list := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"web_search","description":%q}]}}`,
				strings.Repeat("d", 4096))
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				if mode == "sse" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", list)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, list)
			})
			h.server.Config.MCPMaxBodyBytes = 1024
			h.seedToolRoute()
			cert := agentCert(t, h.ca)
			resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"),
				map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 413: %s", resp.StatusCode, body)
			}
			var envelope struct {
				Error errorBody `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&envelope)
			if envelope.Error.Type != errResponseTooLarge || envelope.Error.Provider != "search" {
				t.Fatalf("error = %+v, want type %q with provider search", envelope.Error, errResponseTooLarge)
			}
			if got := mcpCalls(h, "", errResponseTooLarge); got != 1 {
				t.Errorf("response_too_large counter = %v, want 1", got)
			}
		})
	}

	t.Run("tools/list at the cap is served", func(t *testing.T) {
		list := `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"web_search"},{"name":"fetch_page"}]}}`
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, list)
		})
		h.server.Config.MCPMaxBodyBytes = int64(len(list))
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"),
			map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, nil)
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "web_search") || strings.Contains(string(body), "fetch_page") {
			t.Fatalf("want the filtered list, got %s", body)
		}
	})

	// The SSE line bound follows the cap, not a fixed 1 MiB.
	sseToolsList := func(t *testing.T, body string, maxBytes int64) *http.Response {
		t.Helper()
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, body)
		})
		h.server.Config.MCPMaxBodyBytes = maxBytes
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"),
			map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, nil)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	listWith := func(description string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"web_search","description":%q},`+
			`{"name":"fetch_page"}]}}`, description)
	}
	expectFiltered := func(t *testing.T, resp *http.Response) {
		t.Helper()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %.300s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "web_search") || strings.Contains(string(body), "fetch_page") {
			t.Fatalf("want the filtered list, got %.300s", body)
		}
	}

	t.Run("tools/list SSE line over 1 MiB under the cap is served", func(t *testing.T) {
		body := "data: " + listWith(strings.Repeat("d", 2<<20)) + "\n\n"
		expectFiltered(t, sseToolsList(t, body, 4<<20))
	})

	t.Run("tools/list SSE line over the cap is 413", func(t *testing.T) {
		body := "data: " + listWith(strings.Repeat("d", 3<<20)) + "\n\n"
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, body)
		})
		h.server.Config.MCPMaxBodyBytes = 2 << 20
		h.seedToolRoute()
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"),
			map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 413: %.300s", resp.StatusCode, raw)
		}
		var envelope struct {
			Error errorBody `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&envelope)
		if envelope.Error.Type != errResponseTooLarge || envelope.Error.Provider != "search" {
			t.Fatalf("error = %+v, want type %q with provider search", envelope.Error, errResponseTooLarge)
		}
		if got := mcpCalls(h, "", errResponseTooLarge); got != 1 {
			t.Errorf("response_too_large counter = %v, want 1", got)
		}
	})

	t.Run("tools/list SSE at the cap without a trailing newline is served", func(t *testing.T) {
		body := "data: " + listWith(strings.Repeat("d", 100000))
		expectFiltered(t, sseToolsList(t, body, int64(len(body))))
	})
}

// lastSSEData returns the data of the stream's last event.
func lastSSEData(t *testing.T, stream string) string {
	t.Helper()
	events := strings.Split(strings.TrimRight(stream, "\n"), "\n\n")
	for _, line := range strings.Split(events[len(events)-1], "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return data
		}
	}
	t.Fatalf("last event has no data line: %q", stream)
	return ""
}

// expectStreamErrorEvent asserts the stream ends with the broker's error
// event: a JSON-RPC error for request id 7 carrying data.type errType. It
// returns the error message.
func expectStreamErrorEvent(t *testing.T, stream, errType string) string {
	t.Helper()
	var msg struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Type string `json:"type"`
			} `json:"data"`
		} `json:"error"`
	}
	data := lastSSEData(t, stream)
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		t.Fatalf("last event is not JSON: %v: %q", err, data)
	}
	if string(msg.ID) != "7" || msg.Error.Code != mcp.CodeInternalError || msg.Error.Data.Type != errType {
		t.Fatalf("last event = %s, want id 7, code %d, data.type %q", data, mcp.CodeInternalError, errType)
	}
	return msg.Error.Message
}

// A stream that passes the cap after its status line is sent ends with a
// JSON-RPC error event the caller's SDK raises, and the metric records
// response_too_large.
func TestMCPBroker_StreamSizeCap(t *testing.T) {
	streamHarness := func(t *testing.T, stream string, maxBytes int64) *harness {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, stream)
		})
		h.server.Config.MCPMaxBodyBytes = maxBytes
		h.seedToolRoute()
		return h
	}
	call := func(t *testing.T, h *harness) (*http.Response, string) {
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}

	t.Run("event passes the cap", func(t *testing.T) {
		stream := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
			"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"blob\":\"" +
			strings.Repeat("z", 4096) + "\"}}\n\n"
		h := streamHarness(t, stream, 1024)
		resp, body := call(t, h)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("got %d %q, want 200 text/event-stream", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(body, "notifications/progress") {
			t.Fatalf("events under the cap must be relayed: %q", body)
		}
		if strings.Contains(body, "zzzz") {
			t.Fatalf("the line that passes the cap must not be forwarded: %q", body)
		}
		expectStreamErrorEvent(t, body, errResponseTooLarge)
		if got := mcpCalls(h, "web_search", errResponseTooLarge); got != 1 {
			t.Errorf("response_too_large counter = %v, want 1", got)
		}
		if got := mcpCalls(h, "web_search", "ok"); got != 0 {
			t.Errorf("ok counter = %v, want 0", got)
		}
	})

	t.Run("line longer than the scanner buffer", func(t *testing.T) {
		stream := "data: " + strings.Repeat("z", 200000) + "\n\n"
		h := streamHarness(t, stream, 100000)
		_, body := call(t, h)
		if strings.Contains(body, "zzzz") {
			t.Fatalf("the oversized line must not be forwarded: %.200q", body)
		}
		expectStreamErrorEvent(t, body, errResponseTooLarge)
		if got := mcpCalls(h, "web_search", errResponseTooLarge); got != 1 {
			t.Errorf("response_too_large counter = %v, want 1", got)
		}
	})

	t.Run("stream at the cap is relayed whole", func(t *testing.T) {
		stream := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"pad\":\"" + strings.Repeat("p", 512) + "\"}}\n\n"
		h := streamHarness(t, stream, int64(len(stream)))
		_, body := call(t, h)
		if body != stream {
			t.Fatalf("stream at the cap must be relayed byte for byte:\n got %q\nwant %q", body, stream)
		}
		if got := mcpCalls(h, "web_search", "ok"); got != 1 {
			t.Errorf("ok counter = %v, want 1", got)
		}

		h = streamHarness(t, stream, int64(len(stream))-1)
		_, body = call(t, h)
		expectStreamErrorEvent(t, body, errResponseTooLarge)
	})
}

// A stream that times out or breaks after its status line is sent ends
// with the same JSON-RPC error event as the cap, typed by the cause, and the
// call counts under that type instead of ok.
func TestMCPBroker_StreamUpstreamFailure(t *testing.T) {
	const progress = "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"
	call := func(t *testing.T, h *harness) (*http.Response, string) {
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}

	t.Run("upstream timeout mid-stream", func(t *testing.T) {
		blocked := make(chan struct{})
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, progress)
			w.(http.Flusher).Flush()
			select {
			case <-blocked:
			case <-r.Context().Done():
			}
		})
		// LIFO: blocked must close before the harness cleanup waits on the
		// parked handler.
		t.Cleanup(func() { close(blocked) })
		h.server.Config.MCPUpstreamTimeout = 200 * time.Millisecond
		h.seedToolRoute()
		resp, body := call(t, h)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(body, "notifications/progress") {
			t.Fatalf("events before the timeout must be relayed: %q", body)
		}
		msg := expectStreamErrorEvent(t, body, errToolTimeout)
		if want := `tool provider "search" did not finish the stream within the upstream timeout; the stream is truncated`; msg != want {
			t.Errorf("message = %q, want %q", msg, want)
		}
		if got := mcpCalls(h, "web_search", errToolTimeout); got != 1 {
			t.Errorf("tool_timeout counter = %v, want 1", got)
		}
		if got := mcpCalls(h, "web_search", "ok"); got != 0 {
			t.Errorf("ok counter = %v, want 0", got)
		}
	})

	t.Run("connection broken mid-stream", func(t *testing.T) {
		const partial = "data: {\"jsonrpc\":\"2.0\",\"id\":7,\"res"
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", "4096")
			_, _ = fmt.Fprint(w, progress+partial)
		})
		h.seedToolRoute()
		_, body := call(t, h)
		if !strings.Contains(body, "notifications/progress") {
			t.Fatalf("events before the break must be relayed: %q", body)
		}
		for _, line := range strings.Split(body, "\n") {
			if line == partial {
				t.Fatalf("the line the break cut off must not be forwarded: %q", body)
			}
		}
		msg := expectStreamErrorEvent(t, body, errToolUnavailable)
		if want := `reading the stream from tool provider "search" failed; the stream is truncated`; msg != want {
			t.Errorf("message = %q, want %q", msg, want)
		}
		if got := mcpCalls(h, "web_search", errToolUnavailable); got != 1 {
			t.Errorf("tool_unavailable counter = %v, want 1", got)
		}
		if got := mcpCalls(h, "web_search", "ok"); got != 0 {
			t.Errorf("ok counter = %v, want 0", got)
		}
	})
}

// The transport error of a broken stream names the tool server's address,
// which is platform tier: it goes to the audit detail, never to the caller.
func TestRelayMCPStream_UpstreamFailureDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(io.MultiReader(
			strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"),
			iotest.ErrReader(&net.OpError{Op: "read", Net: "tcp",
				Addr: &net.TCPAddr{IP: net.IPv4(10, 43, 7, 9), Port: 8080}, Err: syscall.ECONNRESET}))),
	}
	_, errType, detail := relayMCPStream(rec, req, resp, 1<<20, json.RawMessage("7"), "search")
	if errType != errToolUnavailable {
		t.Fatalf("errType = %q, want %q", errType, errToolUnavailable)
	}
	if !strings.Contains(detail, "10.43.7.9") {
		t.Errorf("audit detail = %q, want the transport error", detail)
	}
	body := rec.Body.String()
	if strings.Contains(body, "10.43.7.9") {
		t.Errorf("caller stream leaks the tool server address: %q", body)
	}
	if !strings.Contains(body, "notifications/progress") {
		t.Errorf("events before the break must be relayed: %q", body)
	}
}

// failingBody is a response body whose reads fail with err.
type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (b failingBody) Close() error             { return nil }

// A caller that left is not a tool failure: the read the departure cancels
// gets no error event and no tool error type.
func TestRelayMCPStream_CallerGoneIsNotAToolFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", nil).WithContext(ctx)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       failingBody{err: context.Canceled},
	}
	_, errType, detail := relayMCPStream(rec, req, resp, 1<<20, json.RawMessage("7"), "search")
	if errType == errToolTimeout || errType == errToolUnavailable {
		t.Errorf("errType = %q, want neither tool_timeout nor tool_unavailable", errType)
	}
	if errType != "client_closed" {
		t.Errorf("errType = %q, want client_closed", errType)
	}
	if detail != "" {
		t.Errorf("detail = %q, want empty", detail)
	}
	if strings.Contains(rec.Body.String(), "jsonrpc") {
		t.Errorf("a departed caller must get no event: %q", rec.Body.String())
	}
}

// When the caller leaves, the upstream read can end in a clean io.EOF
// instead of the context error. That is still the caller leaving, not a
// stream the tool server finished.
func TestRelayMCPStream_CleanEndAfterCallerLeftIsClientClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", nil).WithContext(ctx)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	_, errType, _ := relayMCPStream(rec, req, resp, 1<<20, json.RawMessage("7"), "search")
	if errType != outcomeClientClosed {
		t.Errorf("errType = %q, want %q", errType, outcomeClientClosed)
	}
}

// failingWriter is a ResponseWriter whose writes fail, as a write to a
// connection the caller closed does.
type failingWriter struct{ header http.Header }

func (w *failingWriter) Header() http.Header       { return w.header }
func (w *failingWriter) WriteHeader(int)           {}
func (w *failingWriter) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }

// A stream the caller abandons counts as client_closed, with no event and
// no detail, whichever exit notices the departure.
func TestRelayMCPStream_CallerGone(t *testing.T) {
	const events = "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n"
	newResp := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(events)),
		}
	}

	t.Run("caller gone between lines", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", nil).WithContext(ctx)
		_, errType, detail := relayMCPStream(rec, req, newResp(), 1<<20, json.RawMessage("7"), "search")
		if errType != "client_closed" || detail != "" {
			t.Errorf("outcome = (%q, %q), want (client_closed, empty)", errType, detail)
		}
		if strings.Contains(rec.Body.String(), "data:") {
			t.Errorf("a departed caller must get nothing: %q", rec.Body.String())
		}
	})

	t.Run("downstream write fails", func(t *testing.T) {
		w := &failingWriter{header: http.Header{}}
		req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", nil)
		_, errType, detail := relayMCPStream(w, req, newResp(), 1<<20, json.RawMessage("7"), "search")
		if errType != "client_closed" || detail != "" {
			t.Errorf("outcome = (%q, %q), want (client_closed, empty)", errType, detail)
		}
	})
}

// newAbandonHarness is a harness whose tool server sends one progress event
// and then waits, so the caller can leave mid-stream.
func newAbandonHarness(t *testing.T) *harness {
	t.Helper()
	blocked := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	})
	// LIFO: blocked must close before the harness cleanup waits on the
	// parked handler.
	t.Cleanup(func() { close(blocked) })
	h.seedToolRoute()
	return h
}

// abandonStream calls the tool, reads until the progress event, and closes
// the response. The harness speaks HTTP/1.1, so the close drops the
// connection and the gateway cancels the request context.
func abandonStream(t *testing.T, h *harness) {
	t.Helper()
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if strings.Contains(line, "notifications/progress") {
			break
		}
		if err != nil {
			t.Fatalf("stream ended before the progress event: %v", err)
		}
	}
	_ = resp.Body.Close()
}

// A caller that disconnects mid-stream counts as client_closed, not ok and
// not a tool failure.
func TestMCPBroker_StreamCallerGone(t *testing.T) {
	h := newAbandonHarness(t)
	abandonStream(t, h)
	waitFor(t, func() bool { return mcpCalls(h, "web_search", "client_closed") == 1 })
	for _, status := range []string{"ok", errToolUnavailable, errToolTimeout} {
		if got := mcpCalls(h, "web_search", status); got != 0 {
			t.Errorf("%s counter = %v, want 0", status, got)
		}
	}
}

func TestMCPBroker_RateLimited(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	})
	h.seedToolRoute()
	// 2 calls per minute on 3 replicas: each replica's share is 2/3 call per
	// minute, so a drained bucket holds a call again after 90 seconds.
	h.server.RateLimiter.Replicas = func() int { return 3 }
	now := time.Now()
	h.server.RateLimiter.now = func() time.Time { return now }
	h.store.toolProviders["search"].Spec.RateLimits = kaalmv1beta1.ToolProviderRateLimits{RequestsPerMinute: 2}
	cert := agentCert(t, h.ca)

	first := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first call status %d", first.StatusCode)
	}
	second := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	defer func() { _ = second.Body.Close() }()
	var envelope struct {
		Error errorBody `json:"error"`
	}
	_ = json.NewDecoder(second.Body).Decode(&envelope)
	if second.StatusCode != http.StatusTooManyRequests || envelope.Error.Type != errRateLimited {
		t.Fatalf("got %d %q, want 429 %q", second.StatusCode, envelope.Error.Type, errRateLimited)
	}
	if !envelope.Error.Retryable {
		t.Error("a rate-limited call must be retryable")
	}
	if got := second.Header.Get("Retry-After"); got != "90" {
		t.Errorf("Retry-After = %q, want the computed wait \"90\"", got)
	}
}

func TestMCPBroker_UnauthenticatedCredentiallessServer(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":7,"result":{"auth":%q}}`, r.Header.Get("Authorization"))
	})
	h.seedToolRoute()
	h.store.toolProviders["search"].Spec.CredentialsRef = nil
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Result struct {
			Auth string `json:"auth"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Result.Auth != "" {
		t.Fatalf("credential-less server received Authorization %q", body.Result.Auth)
	}
}

// ---- audit and metrics ----

// mcpCalls reads the kaalm_tool_calls_total counter for one (tool, status)
// tuple on the seedToolRoute fixture (provider "search", namespace team-a).
func mcpCalls(h *harness, tool, status string) float64 {
	return testutil.ToFloat64(h.server.Metrics.toolCalls.WithLabelValues("search", "team-a", tool, status))
}

func TestMCPBroker_MetricsAcrossOutcomes(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)
	cl := h.client(&cert)

	// Allowed call: ok status under the real tool label, duration observed.
	resp := postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	_ = resp.Body.Close()
	if got := mcpCalls(h, "web_search", "ok"); got != 1 {
		t.Errorf("ok counter = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(h.server.Metrics.toolDuration); n != 1 {
		t.Errorf("duration series = %d, want 1 (forwarded call observed)", n)
	}

	// Cataloged but ungranted: tool_denied under the real label.
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("fetch_page"), nil)
	expectMCPError(t, resp, http.StatusForbidden, errToolDenied)
	if got := mcpCalls(h, "fetch_page", errToolDenied); got != 1 {
		t.Errorf("tool_denied counter = %v, want 1", got)
	}

	// A wire-supplied name outside the catalog collapses, so callers cannot
	// inflate label cardinality.
	resp = postJSON(t, cl, h.url("/v1/mcp/search"), mcpCall("rm_rf"), nil)
	expectMCPError(t, resp, http.StatusForbidden, errToolDenied)
	if got := mcpCalls(h, "uncataloged", errToolDenied); got != 1 {
		t.Errorf("uncataloged counter = %v, want 1", got)
	}

	// Local denials never touch the duration histogram.
	if n := testutil.CollectAndCount(h.server.Metrics.toolDuration); n != 1 {
		t.Errorf("duration series after denials = %d, want still 1", n)
	}
}

func TestMCPBroker_MetricsNamespaceDenied(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.seedToolRoute()
	h.store.toolProviders["search"].Spec.AllowedNamespaces = []string{"prod-*"}
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	expectMCPError(t, resp, http.StatusForbidden, errAccessDenied)
	// The namespace check refuses before the body is parsed: no method, no tool.
	if got := mcpCalls(h, "", errAccessDenied); got != 1 {
		t.Errorf("access_denied counter = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(h.server.Metrics.toolDuration); n != 0 {
		t.Errorf("local denial observed a duration: %d series", n)
	}
}

// A relayed protocol-level 4xx is a completed brokered exchange: status label
// upstream_error, duration observed (the upstream was really consulted).
func TestMCPBroker_MetricsUpstream4xxRelay(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"error":{"code":-32001,"message":"session expired"}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want the relayed 404", resp.StatusCode)
	}
	if got := mcpCalls(h, "web_search", "upstream_error"); got != 1 {
		t.Errorf("upstream_error counter = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(h.server.Metrics.toolDuration); n != 1 {
		t.Errorf("forwarded 4xx must observe a duration: %d series", n)
	}
}

// Without a declared catalog every tool label collapses to the sentinel,
// even on allowed calls: wire-supplied names are unbounded.
func TestMCPBroker_MetricsCataloglessSentinel(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	})
	h.seedToolRoute()
	h.store.toolProviders["search"].Spec.Tools = nil
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := mcpCalls(h, "uncataloged", "ok"); got != 1 {
		t.Errorf("sentinel counter = %v, want 1", got)
	}
}

// The audit record: one info-level structured line with the fields the tool
// plane chapter promises, real tool name included, bodies never.
func TestMCPResult_AuditRecord(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	s := &Server{} // nil Metrics no-ops; only the log line is under test
	c := &caller{Namespace: "team-a",
		Workload: &Identity{Namespace: "team-a", Name: "sup", Kind: KindAgent}}
	tp := &kaalmv1beta1.ToolProvider{}
	tp.Name = "search"
	s.mcpResult(c, tp, "search", "tools/call", "rm_rf", http.StatusForbidden,
		errToolDenied, `tool "rm_rf" is not granted to this workload`,
		time.Now().Add(-50*time.Millisecond), 128, 0, false)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("audit record is not one JSON line: %v (%s)", err, buf.String())
	}
	want := map[string]any{
		"level": "INFO", "msg": "mcp call", "namespace": "team-a",
		"provider": "search", "method": "tools/call", "tool": "rm_rf",
		"status": float64(http.StatusForbidden), "error_type": errToolDenied,
		"request_bytes": float64(128), "response_bytes": float64(0),
		"workload": "sup", "workload_kind": "Agent",
		"detail": `tool "rm_rf" is not granted to this workload`,
	}
	for key, expected := range want {
		if rec[key] != expected {
			t.Errorf("record[%q] = %v, want %v", key, rec[key], expected)
		}
	}
	dur, ok := rec["duration_seconds"].(float64)
	if !ok || dur <= 0 {
		t.Errorf("duration_seconds = %v, want positive", rec["duration_seconds"])
	}

	// Bearer-tier callers carry no workload fields, successes no detail.
	buf.Reset()
	s.mcpResult(&caller{Namespace: "team-b"}, nil, "search", "ping", "", 200, "", "",
		time.Now(), 10, 20, true)
	var bearer map[string]any
	_ = json.Unmarshal(buf.Bytes(), &bearer)
	if _, present := bearer["workload"]; present {
		t.Error("bearer-tier record must not carry a workload field")
	}
	if bearer["namespace"] != "team-b" {
		t.Errorf("bearer namespace = %v", bearer["namespace"])
	}
}

// modernHeaders is the header set the 2026-07-28 revision requires on a
// brokered tools/call.
func modernHeaders(tool string) map[string]string {
	h := map[string]string{
		"MCP-Protocol-Version": "2026-07-28",
		"Mcp-Method":           "tools/call",
	}
	if tool != "" {
		h["Mcp-Name"] = tool
	}
	return h
}

func TestMCPBroker_ModernHappyPathForwardsValidatedHeaders(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","content":[]}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	headers := modernHeaders("web_search")
	headers["Mcp-Param-Region"] = "us-west1"
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), headers)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	up := <-h.upreqs
	if got := up.header.Get("Mcp-Method"); got != "tools/call" {
		t.Errorf("Mcp-Method not forwarded upstream: %q", got)
	}
	if got := up.header.Get("Mcp-Name"); got != "web_search" {
		t.Errorf("Mcp-Name not forwarded upstream: %q", got)
	}
	if got := up.header.Get("Mcp-Param-Region"); got != "us-west1" {
		t.Errorf("Mcp-Param-* not forwarded upstream: %q", got)
	}
}

func TestMCPBroker_ModernHeaderValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(map[string]string)
	}{
		"missing Mcp-Method":    {func(h map[string]string) { delete(h, "Mcp-Method") }},
		"mismatched Mcp-Method": {func(h map[string]string) { h["Mcp-Method"] = "tools/list" }},
		"missing Mcp-Name":      {func(h map[string]string) { delete(h, "Mcp-Name") }},
		"mismatched Mcp-Name":   {func(h map[string]string) { h["Mcp-Name"] = "fetch_page" }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("a header-invalid request must never reach the upstream")
			})
			h.seedToolRoute()
			cert := agentCert(t, h.ca)

			headers := modernHeaders("web_search")
			tc.mutate(headers)
			resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), headers)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var rpc struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&rpc)
			if rpc.Error.Code != -32020 {
				t.Fatalf("error code = %d, want -32020 HeaderMismatch", rpc.Error.Code)
			}
		})
	}
}

func TestMCPBroker_ModernMcpNameBase64SentinelDecodes(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","content":[]}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	headers := modernHeaders("")
	headers["Mcp-Name"] = "=?base64?d2ViX3NlYXJjaA==?=" // "web_search"
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), headers)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want the encoded name to decode and match: %s", resp.StatusCode, body)
	}
	<-h.upreqs
}

func TestMCPBroker_ModernIgnoresSessionHeader(t *testing.T) {
	// The revision removed sessions; its rule for a stray Mcp-Session-Id is
	// to ignore it. In particular an unverifiable wrapped id must not be a
	// denial on a modern request, and nothing session-shaped goes upstream.
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","content":[]}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	headers := modernHeaders("web_search")
	headers["Mcp-Session-Id"] = "not-a-wrapped-id"
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), headers)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want the stray session header ignored: %s", resp.StatusCode, body)
	}
	up := <-h.upreqs
	if got := up.header.Get("Mcp-Session-Id"); got != "" {
		t.Errorf("session header forwarded on a modern request: %q", got)
	}
}

func TestMCPBroker_ModernServerDiscoverBrokered(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","supportedVersions":["2026-07-28"]}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	body := map[string]any{"jsonrpc": "2.0", "id": 7, "method": "server/discover", "params": map[string]any{}}
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), body,
		map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "server/discover"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	<-h.upreqs
}

func TestMCPBroker_SubscriptionsListenDenied(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a denied method must never reach the upstream")
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	body := map[string]any{"jsonrpc": "2.0", "id": 7, "method": "subscriptions/listen", "params": map[string]any{}}
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), body,
		map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "subscriptions/listen"})
	expectMCPError(t, resp, http.StatusForbidden, errToolDenied)
}

func TestMCPBroker_ModernToolsListRewritesCacheScope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","tools":[{"name":"web_search"},{"name":"fetch_page"},{"name":"admin_reset"}],"ttlMs":60000,"cacheScope":"public"}}`)
	})
	h.seedToolRoute()
	cert := agentCert(t, h.ca)

	body := map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/list", "params": map[string]any{}}
	resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), body,
		map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	<-h.upreqs
	var parsed struct {
		Result struct {
			Tools      []struct{ Name string }
			TTLMs      int    `json:"ttlMs"`
			CacheScope string `json:"cacheScope"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(parsed.Result.Tools) != 1 || parsed.Result.Tools[0].Name != "web_search" {
		t.Fatalf("filtered tools = %+v, want the granted web_search only", parsed.Result.Tools)
	}
	if parsed.Result.CacheScope != "private" {
		t.Fatalf("cacheScope = %q, want private: a per-caller-filtered list must never be shared-cached", parsed.Result.CacheScope)
	}
	if parsed.Result.TTLMs != 60000 {
		t.Fatalf("ttlMs = %d, want the upstream hint preserved", parsed.Result.TTLMs)
	}
}

// TestRelayFilteredToolsList_FailureDetail pins the audit detail on the
// tools/list relay's failures: the caller message stays fixed, and the
// cause goes to the audit record, as on the buffered relay.
func TestRelayFilteredToolsList_FailureDetail(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.server.Config.MCPMaxBodyBytes = 1024
	msg := mcpRequest{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "tools/list"}

	t.Run("read failure", func(t *testing.T) {
		rec := httptest.NewRecorder()
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(iotest.ErrReader(&net.OpError{Op: "read", Net: "tcp",
				Addr: &net.TCPAddr{IP: net.IPv4(10, 43, 7, 9), Port: 8080}, Err: syscall.ECONNRESET})),
		}
		_, status, errType, detail := h.server.relayFilteredToolsList(context.Background(), rec, resp, msg, &toolFilter{}, "search")
		if status != http.StatusServiceUnavailable || errType != errToolUnavailable {
			t.Fatalf("outcome = (%d, %q), want (503, tool_unavailable)", status, errType)
		}
		body := expectMCPErrorBody(t, rec.Result(), http.StatusServiceUnavailable, errToolUnavailable)
		if strings.Contains(body.Message, "10.43.7.9") {
			t.Errorf("caller message leaks the tool server address: %q", body.Message)
		}
		if !strings.HasPrefix(detail, body.Message+": ") || !strings.Contains(detail, "10.43.7.9") {
			t.Errorf("audit detail = %q, want the caller message and the transport error", detail)
		}
		// A failed read is a transport fault, retryable as on the buffered
		// relay; only a list that arrived whole and will not parse is not.
		if !body.Retryable {
			t.Error("read failure retryable = false, want true")
		}
		if want := `reading the response from tool provider "search" failed`; body.Message != want {
			t.Errorf("message = %q, want %q", body.Message, want)
		}
	})

	t.Run("unparseable list", func(t *testing.T) {
		rec := httptest.NewRecorder()
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader("not json")),
		}
		_, status, errType, detail := h.server.relayFilteredToolsList(context.Background(), rec, resp, msg, &toolFilter{}, "search")
		if status != http.StatusServiceUnavailable || errType != errToolUnavailable {
			t.Fatalf("outcome = (%d, %q), want (503, tool_unavailable)", status, errType)
		}
		body := expectMCPErrorBody(t, rec.Result(), http.StatusServiceUnavailable, errToolUnavailable)
		if body.Retryable {
			t.Error("unparseable list retryable = true, want false")
		}
		if want := "tool provider returned an unparseable tools/list response"; body.Message != want {
			t.Errorf("message = %q, want %q", body.Message, want)
		}
		if !strings.HasPrefix(detail, body.Message+": ") {
			t.Errorf("audit detail = %q, want the caller message and the parse error", detail)
		}
	})

	t.Run("caller gone", func(t *testing.T) {
		gone, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: failingBody{err: context.Canceled}}
		n, status, errType, detail := h.server.relayFilteredToolsList(gone, rec, resp, msg, &toolFilter{}, "search")
		if n != 0 || status != statusClientClosedRequest || errType != outcomeClientClosed || detail != "" {
			t.Errorf("outcome = (%d, %d, %q, %q), want (0, 499, client_closed, empty)", n, status, errType, detail)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("wrote %q to a caller that left", rec.Body)
		}
	})

	t.Run("response too large", func(t *testing.T) {
		rec := httptest.NewRecorder()
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", 4096))),
		}
		_, status, _, detail := h.server.relayFilteredToolsList(context.Background(), rec, resp, msg, &toolFilter{}, "search")
		body := expectMCPErrorBody(t, rec.Result(), http.StatusRequestEntityTooLarge, errResponseTooLarge)
		if status != http.StatusRequestEntityTooLarge || detail != body.Message {
			t.Errorf("outcome = (%d, detail %q), want (413, the caller message %q)", status, detail, body.Message)
		}
	})
}

// The broker's own JSON-RPC errors have the shape of mcp.Response: the same
// bytes a relayed upstream error decodes from, data included.
func TestJSONRPCError_MatchesMCPResponse(t *testing.T) {
	data := struct {
		Type string `json:"type"`
	}{"x"}
	cases := []struct {
		name string
		id   json.RawMessage
		data any
		want string
	}{
		{"id and data", json.RawMessage("7"), data,
			`{"jsonrpc":"2.0","id":7,"error":{"code":-32603,"message":"m","data":{"type":"x"}}}`},
		{"no id, no data", nil, nil, `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"m"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jsonrpcError(tc.id, mcp.CodeInternalError, "m", tc.data)
			if string(got) != tc.want {
				t.Errorf("jsonrpcError = %s, want %s", got, tc.want)
			}
			var resp mcp.Response
			if err := json.Unmarshal(got, &resp); err != nil || resp.Error == nil || resp.Error.Code != mcp.CodeInternalError {
				t.Errorf("decode as mcp.Response: %+v, %v", resp, err)
			}
		})
	}
}

// A buffered response read that fails because the caller left writes
// nothing and counts client_closed; a timeout with the caller present is
// still tool_timeout.
func TestRelayMCPBuffered_CallerGone(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: failingBody{err: context.Canceled}}
	n, status, errType, detail := relayMCPBuffered(gone, rec, resp, 1024, "search")
	if n != 0 || status != statusClientClosedRequest || errType != outcomeClientClosed || detail != "" {
		t.Errorf("outcome = (%d, %d, %q, %q), want (0, 499, client_closed, empty)", n, status, errType, detail)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Errorf("wrote %q (Content-Type %q) to a caller that left", rec.Body, rec.Header().Get("Content-Type"))
	}

	rec = httptest.NewRecorder()
	resp.Body = failingBody{err: context.DeadlineExceeded}
	if _, status, errType, _ := relayMCPBuffered(context.Background(), rec, resp, 1024, "search"); status != http.StatusGatewayTimeout || errType != errToolTimeout {
		t.Errorf("timeout with the caller present = (%d, %q), want (504, tool_timeout)", status, errType)
	}
}

// callerGoneHarness is a harness whose tool server signals arrived and then
// waits, after sending nothing (mode "silent") or a 200 with part of a JSON
// body (mode "partial"), so the caller can leave before the broker answers.
func callerGoneHarness(t *testing.T, mode string) (*harness, chan struct{}) {
	t.Helper()
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if mode == "partial" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"tools":[`)
			w.(http.Flusher).Flush()
		}
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// LIFO: release must close before the harness cleanup waits on the
	// parked handler.
	t.Cleanup(func() { close(release) })
	h.seedToolRoute()
	return h, arrived
}

// leaveBeforeAnswer sends body and cancels the call once the tool server has
// it, before the broker answers.
func leaveBeforeAnswer(t *testing.T, h *harness, arrived chan struct{}, body map[string]any) {
	t.Helper()
	cert := agentCert(t, h.ca)
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url("/v1/mcp/search"), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := h.client(&cert).Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool server never got the call")
	}
	cancel()
	<-done
}

// A caller that leaves before the broker answers counts client_closed, not
// a tool failure, whether the tool server had sent nothing or part of a
// buffered or tools/list answer.
func TestMCPBroker_CallerGoneBeforeAnswer(t *testing.T) {
	cases := []struct {
		name, mode, tool string
		body             map[string]any
	}{
		{"no answer yet", "silent", "web_search", mcpCall("web_search")},
		{"partial buffered answer", "partial", "web_search", mcpCall("web_search")},
		{"partial tools/list answer", "partial", "", map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/list"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			h, arrived := callerGoneHarness(t, tc.mode)
			leaveBeforeAnswer(t, h, arrived, tc.body)
			waitFor(t, func() bool { return mcpCalls(h, tc.tool, outcomeClientClosed) == 1 })
			for _, status := range []string{errToolUnavailable, errToolTimeout} {
				if got := mcpCalls(h, tc.tool, status); got != 0 {
					t.Errorf("%s counter = %v, want 0", status, got)
				}
			}
			recs := logRecords(t, buf, "mcp call")
			if len(recs) != 1 {
				t.Fatalf("audit records = %d, want 1", len(recs))
			}
			if recs[0]["error_type"] != outcomeClientClosed || recs[0]["status"] != float64(statusClientClosedRequest) {
				t.Errorf("audit record = %v, want error_type client_closed and status 499", recs[0])
			}
			if _, ok := recs[0]["detail"]; ok {
				t.Errorf("audit record carries a detail: %v", recs[0]["detail"])
			}
		})
	}
}

// A caller that disconnects mid-upload left; the request was not malformed.
func TestMCPBroker_RequestBodyCallerGone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h.seedToolRoute()
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), callerKey{}, &caller{Namespace: "team-a"}))
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)))
	rec := httptest.NewRecorder()
	h.server.handleMCPBroker(rec, req.WithContext(ctx))
	if got := mcpCalls(h, "", outcomeClientClosed); got != 1 {
		t.Errorf("client_closed counter = %v, want 1", got)
	}
	if got := mcpCalls(h, "", errInvalidRequest); got != 0 {
		t.Errorf("invalid_request counter = %v, want 0", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %q to a caller that left", rec.Body)
	}
}
