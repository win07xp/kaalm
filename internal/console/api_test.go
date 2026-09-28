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

// fakeChat records the last call and relays scripted responses for both
// gateway surfaces.
type fakeChat struct {
	status  int
	body    []byte
	lastNS  string
	lastAg  string
	lastUID string
	// spendStatus/spendBody script WorkloadSpend; zero status means 200
	// with an empty providers object.
	spendStatus int
	spendBody   []byte
	spendErr    error
}

func (f *fakeChat) Chat(_ context.Context, ns, agent, userID, _ string) (int, []byte, error) {
	f.lastNS, f.lastAg, f.lastUID = ns, agent, userID
	return f.status, f.body, nil
}

func (f *fakeChat) WorkloadSpend(_ context.Context, ns string) (int, []byte, error) {
	if f.spendErr != nil {
		return 0, nil, f.spendErr
	}
	if f.spendStatus == 0 {
		return 200, []byte(`{"providers":{}}`), nil
	}
	return f.spendStatus, f.spendBody, nil
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
	got := decode[struct {
		Agents []FleetRow `json:"agents"`
	}](t, h.get(t, "/api/v1/namespaces/team-a/agents", "priya-token"))
	rows := got.Agents
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

// countingReader serves an endless body and counts the bytes read from it.
type countingReader struct{ n int64 }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	c.n += int64(len(p))
	return len(p), nil
}

func TestAPI_ChatBodyCap(t *testing.T) {
	h := newAPIHarness(t)
	h.server.Config.MaxMessageBodyBytes = 64
	h.chat.lastUID = ""

	body := &countingReader{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/team-a/agents/support-assistant/chat",
		io.MultiReader(strings.NewReader(`{"content":"`), body))
	req.Header.Set("Authorization", "Bearer priya-token")
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized chat = %d, want 413 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "request_too_large") {
		t.Errorf("413 body = %s, want the request_too_large envelope", rec.Body)
	}
	// The cap stops the read: the console never buffers the whole body.
	if body.n > 4096 {
		t.Errorf("read %d bytes of an endless body; the cap must stop the read", body.n)
	}
	if h.chat.lastUID != "" {
		t.Error("an oversized chat must not reach the gateway")
	}

	// A body under the cap still goes through.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/team-a/agents/support-assistant/chat",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Authorization", "Bearer priya-token")
	rec = httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("small chat = %d, want 200", rec.Code)
	}
}

func TestNewServer_DefaultsChatBodyCap(t *testing.T) {
	s := NewServer(Config{}, seededData(t), &fakeReviewer{}, NewGate(&fakeAuthorizer{}), &fakeChat{})
	if s.Config.MaxMessageBodyBytes != 1<<20 {
		t.Errorf("default chat body cap = %d, want 1 MiB", s.Config.MaxMessageBodyBytes)
	}
}

func TestAPI_NamespacesClusterWideGrantSkipsTheLoop(t *testing.T) {
	h := newAPIHarness(t)
	h.authz.allowed["priya/list/agents.kaalm.io/"] = true

	before := h.authz.calls
	got := decode[map[string][]string](t, h.get(t, "/api/v1/namespaces", "priya-token"))
	if ns := got["namespaces"]; len(ns) != 2 {
		t.Errorf("priya sees %v, want both namespaces", ns)
	}
	if calls := h.authz.calls - before; calls != 1 {
		t.Errorf("a cluster-wide grant took %d reviews, want 1", calls)
	}

	// Without the cluster-wide grant the per-namespace loop still filters.
	got = decode[map[string][]string](t, h.get(t, "/api/v1/namespaces", "dev-token"))
	if ns := got["namespaces"]; len(ns) != 1 || ns[0] != "team-a" {
		t.Errorf("dev sees %v, want team-a only", ns)
	}
}

func TestAPI_ListLimit(t *testing.T) {
	h := newAPIHarness(t)
	type page struct {
		Agents    []FleetRow   `json:"agents"`
		Tasks     []TaskRow    `json:"tasks"`
		Channels  []ChannelRow `json:"channels"`
		Total     int          `json:"total"`
		Truncated bool         `json:"truncated"`
	}

	got := decode[page](t, h.get(t, "/api/v1/namespaces/team-a/agents", "priya-token"))
	if len(got.Agents) != 2 || got.Total != 2 || got.Truncated {
		t.Errorf("default agents = %d of %d, truncated %v", len(got.Agents), got.Total, got.Truncated)
	}

	got = decode[page](t, h.get(t, "/api/v1/namespaces/team-a/agents?limit=1", "priya-token"))
	if len(got.Agents) != 1 || got.Total != 2 || !got.Truncated {
		t.Errorf("limit=1 agents = %d of %d, truncated %v", len(got.Agents), got.Total, got.Truncated)
	}
	got = decode[page](t, h.get(t, "/api/v1/namespaces/team-a/tasks?limit=1", "priya-token"))
	if len(got.Tasks) != 1 || !got.Truncated {
		t.Errorf("limit=1 tasks = %+v", got)
	}
	got = decode[page](t, h.get(t, "/api/v1/namespaces/team-a/channels?limit=1", "priya-token"))
	if len(got.Channels) != 1 || got.Truncated {
		t.Errorf("limit=1 channels = %+v", got)
	}

	// Above the hard maximum the limit clamps rather than fails.
	resp := h.get(t, "/api/v1/namespaces/team-a/agents?limit=5000", "priya-token")
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("limit above the maximum = %d, want 200", resp.StatusCode)
	}

	for _, bad := range []string{"0", "-1", "ten"} {
		resp := h.get(t, "/api/v1/namespaces/team-a/tasks?limit="+bad, "priya-token")
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("limit=%s = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestListLimit(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", defaultListLimit, false},
		{"1", 1, false},
		{"1000", maxListLimit, false},
		{"1001", maxListLimit, false},
		{"0", 0, true},
		{"x", 0, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/?limit="+c.raw, nil)
		got, err := listLimit(r)
		if (err != nil) != c.wantErr || (!c.wantErr && got != c.want) {
			t.Errorf("listLimit(%q) = %d, %v", c.raw, got, err)
		}
	}
	if defaultListLimit != 100 || maxListLimit != 1000 {
		t.Errorf("limits = %d/%d, the book states 100/1000", defaultListLimit, maxListLimit)
	}
}
