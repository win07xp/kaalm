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

package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeChat records the last call and relays a scripted response.
type fakeChat struct {
	status  int
	body    []byte
	lastNS  string
	lastAg  string
	lastUID string
}

func (f *fakeChat) Chat(_ context.Context, ns, agent, userID, _ string) (int, []byte, error) {
	f.lastNS, f.lastAg, f.lastUID = ns, agent, userID
	return f.status, f.body, nil
}

type apiHarness struct {
	server *Server
	srv    *httptest.Server
	authz  *fakeAuthorizer
	chat   *fakeChat
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	reviewer := &fakeReviewer{tokens: map[string]Identity{
		"priya-token": {Username: "priya", Groups: []string{"platform"}},
		"dev-token":   {Username: "dev"},
	}}
	authz := &fakeAuthorizer{allowed: map[string]bool{
		"priya/list/agents.kaalm.io/team-a":          true,
		"priya/list/agents.kaalm.io/team-b":          true,
		"priya/create/agentchannels.kaalm.io/team-a": true,
		"dev/list/agents.kaalm.io/team-a":            true,
		// dev may view team-a but not chat in it, and sees no team-b.
	}}
	chat := &fakeChat{status: 200, body: []byte(`{"content":"alive"}`)}
	s := NewServer(Config{OperatorNamespace: "kaalm-system"},
		seededData(t), reviewer, NewGate(authz), chat)
	h := &apiHarness{server: s, authz: authz, chat: chat}
	h.srv = httptest.NewTLSServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *apiHarness) get(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAPI_AuthMatrix(t *testing.T) {
	h := newAPIHarness(t)
	cases := []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{"no auth", "/api/v1/namespaces", "", 401},
		{"bad token", "/api/v1/namespaces/team-a/agents", "nope", 401},
		{"allowed fleet", "/api/v1/namespaces/team-a/agents", "priya-token", 200},
		{"denied namespace", "/api/v1/namespaces/team-b/agents", "dev-token", 403},
		{"allowed tasks", "/api/v1/namespaces/team-a/tasks", "priya-token", 200},
		{"allowed channels", "/api/v1/namespaces/team-a/channels", "priya-token", 200},
		{"allowed spend", "/api/v1/namespaces/team-a/spend", "priya-token", 200},
		{"agent detail", "/api/v1/namespaces/team-a/agents/coder", "priya-token", 200},
		{"agent missing", "/api/v1/namespaces/team-a/agents/nope", "priya-token", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := h.get(t, c.path, c.token)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d (%s)", resp.StatusCode, c.want, body)
			}
		})
	}
}

func TestAPI_NamespacesFiltered(t *testing.T) {
	h := newAPIHarness(t)

	got := decode[map[string][]string](t, h.get(t, "/api/v1/namespaces", "priya-token"))
	if ns := got["namespaces"]; len(ns) != 2 {
		t.Errorf("priya sees %v, want both namespaces", ns)
	}

	got = decode[map[string][]string](t, h.get(t, "/api/v1/namespaces", "dev-token"))
	if ns := got["namespaces"]; len(ns) != 1 || ns[0] != "team-a" {
		t.Errorf("dev sees %v, want team-a only", ns)
	}
}

func TestAPI_FleetShape(t *testing.T) {
	h := newAPIHarness(t)
	got := decode[map[string][]FleetRow](t, h.get(t, "/api/v1/namespaces/team-a/agents", "priya-token"))
	rows := got["agents"]
	if len(rows) != 2 || rows[1].Name != "support-assistant" || rows[1].Phase != "Hibernated" {
		t.Errorf("fleet = %+v", rows)
	}
}

func TestAPI_Chat(t *testing.T) {
	h := newAPIHarness(t)
	post := func(token, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost,
			h.srv.URL+"/api/v1/namespaces/team-a/agents/support-assistant/chat", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Viewing is not enough: chat needs the create gate.
	resp := post("dev-token", `{"content":"hi"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("chat without the create gate = %d, want 403", resp.StatusCode)
	}

	// Empty content is rejected before the gateway is called.
	resp = post("priya-token", `{"content":"  "}`)
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("empty content = %d, want 400", resp.StatusCode)
	}

	// The gateway response is relayed verbatim and the authenticated
	// username rides as the userId.
	resp = post("priya-token", `{"content":"are you alive?"}`)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"content":"alive"}` {
		t.Errorf("chat relay = %d %s", resp.StatusCode, body)
	}
	if h.chat.lastUID != "priya" || h.chat.lastNS != "team-a" || h.chat.lastAg != "support-assistant" {
		t.Errorf("chat call = %s/%s as %s", h.chat.lastNS, h.chat.lastAg, h.chat.lastUID)
	}

	// A gateway error status relays too (the wire contract is the gateway's).
	h.chat.status, h.chat.body = 502, []byte(`{"error":{"type":"delivery_failed"}}`)
	resp = post("priya-token", `{"content":"hi"}`)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 502 || !strings.Contains(string(body), "delivery_failed") {
		t.Errorf("gateway error relay = %d %s", resp.StatusCode, body)
	}
}

func TestAPI_SessionCookieAuth(t *testing.T) {
	h := newAPIHarness(t)
	value, _, err := h.server.Sessions.Create(context.Background(), "priya-token")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/namespaces/team-a/agents", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("session-cookie call = %d, want 200", resp.StatusCode)
	}
}
