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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
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
	return envelope.Error.Message
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
	// gate and may call any cataloged tool, including uncataloged narrowings
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

func TestMCPBroker_UpstreamFailureMapping(t *testing.T) {
	t.Run("refused connection", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
		h.seedToolRoute()
		h.upstream.Close() // reachable address, refused connection
		cert := agentCert(t, h.ca)
		resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
		expectMCPError(t, resp, http.StatusServiceUnavailable, errToolUnavailable)
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
}

func TestMCPBroker_RateLimited(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{}}`)
	})
	h.seedToolRoute()
	h.store.toolProviders["search"].Spec.RateLimits = kaalmv1beta1.ToolProviderRateLimits{RequestsPerMinute: 1}
	cert := agentCert(t, h.ca)

	first := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first call status %d", first.StatusCode)
	}
	second := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
	expectMCPError(t, second, http.StatusTooManyRequests, errRateLimited)
	if second.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
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
	// The namespace gate fires before the body is parsed: no method, no tool.
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
