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
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// uiClient is an http client with a cookie jar and no redirect following,
// so redirects are assertable. It trusts the harness's TLS server: the
// console serves TLS only, and the session cookie is Secure, so the tests
// must ride https like production does (older Go cookiejars correctly
// refuse to send Secure cookies over plain http).
func uiClient(t *testing.T, h *apiHarness) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: h.srv.Client().Transport,
		Jar:       jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func login(t *testing.T, h *apiHarness, c *http.Client, token string) *http.Response {
	t.Helper()
	resp, err := c.PostForm(h.srv.URL+"/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func page(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestUI_LoginFlow(t *testing.T) {
	h := newAPIHarness(t)
	c := uiClient(t, h)

	// Logged out, every page redirects to /login.
	status, _ := page(t, c, h.srv.URL+"/")
	if status != http.StatusSeeOther {
		t.Fatalf("logged-out home = %d, want redirect", status)
	}

	// The login page renders the form.
	status, body := page(t, c, h.srv.URL+"/login")
	if status != 200 || !strings.Contains(body, `name="token"`) {
		t.Fatalf("login page = %d", status)
	}

	// A bad token re-renders the form with the error.
	resp, err := c.PostForm(h.srv.URL+"/login", url.Values{"token": {"nope"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "did not accept") {
		t.Errorf("bad login = %d %s", resp.StatusCode, b)
	}

	// A good token sets the session cookie and redirects home.
	resp = login(t, h, c, "priya-token")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want redirect", resp.StatusCode)
	}
	status, body = page(t, c, h.srv.URL+"/")
	if status != 200 || !strings.Contains(body, "team-a") || !strings.Contains(body, "priya") {
		t.Errorf("home after login = %d", status)
	}

	// Logout kills the session.
	resp, err = c.PostForm(h.srv.URL+"/logout", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if status, _ = page(t, c, h.srv.URL+"/"); status != http.StatusSeeOther {
		t.Errorf("home after logout = %d, want redirect", status)
	}
}

func TestUI_HomeFiltersNamespaces(t *testing.T) {
	h := newAPIHarness(t)
	c := uiClient(t, h)
	login(t, h, c, "dev-token")
	_, body := page(t, c, h.srv.URL+"/")
	if !strings.Contains(body, "team-a") || strings.Contains(body, "team-b") {
		t.Error("dev must see team-a only")
	}
}

func TestUI_NamespacePanels(t *testing.T) {
	h := newAPIHarness(t)
	c := uiClient(t, h)
	login(t, h, c, "priya-token")

	status, body := page(t, c, h.srv.URL+"/ns/team-a")
	if status != 200 {
		t.Fatalf("namespace page = %d", status)
	}
	for _, want := range []string{
		"support-assistant", "Hibernated", // fleet
		"anthropic-shared", "12.34", "100.00", // spend against budget
		"newer-task", "summary, report", // task history with artifact names
		"Channel health", "Unknown", // channel health with tri-state PlatformConnected
	} {
		if !strings.Contains(body, want) {
			t.Errorf("namespace page missing %q", want)
		}
	}
	// Artifact values never render.
	if strings.Contains(body, "SENSITIVE OUTPUT") {
		t.Error("artifact values leaked into the page")
	}

	// A namespace the identity may not view is denied.
	c2 := uiClient(t, h)
	login(t, h, c2, "dev-token")
	if status, _ := page(t, c2, h.srv.URL+"/ns/team-b"); status != 403 {
		t.Errorf("denied namespace page = %d, want 403", status)
	}
}

func TestUI_AgentPageAndChat(t *testing.T) {
	h := newAPIHarness(t)
	c := uiClient(t, h)
	login(t, h, c, "priya-token")

	status, body := page(t, c, h.srv.URL+"/ns/team-a/agents/support-assistant")
	if status != 200 {
		t.Fatalf("agent page = %d", status)
	}
	for _, want := range []string{"search-tools", "web_search", "Hibernated", "Test chat", `name="content"`} {
		if !strings.Contains(body, want) {
			t.Errorf("agent page missing %q", want)
		}
	}

	// The chat form round-trips and renders the reply.
	resp, err := c.PostForm(h.srv.URL+"/ns/team-a/agents/support-assistant/chat",
		url.Values{"content": {"are you alive?"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), "alive") {
		t.Error("chat reply must render on the page")
	}
	if h.chat.lastUID != "priya" {
		t.Errorf("chat rode as %q, want the session identity", h.chat.lastUID)
	}

	// A gateway failure renders the error type, not a raw body.
	h.chat.status, h.chat.body = 502, []byte(`{"error":{"type":"delivery_failed","message":"x"}}`)
	resp, err = c.PostForm(h.srv.URL+"/ns/team-a/agents/support-assistant/chat",
		url.Values{"content": {"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), "delivery_failed") {
		t.Error("chat failure must render the error type")
	}

	// Viewing is not chatting: dev sees the page but the form is refused.
	c2 := uiClient(t, h)
	login(t, h, c2, "dev-token")
	resp, err = c2.PostForm(h.srv.URL+"/ns/team-a/agents/support-assistant/chat",
		url.Values{"content": {"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), "requires permission") {
		t.Error("denied chat must render the permission error")
	}

	// Unknown agent is a 404 page.
	if status, _ := page(t, c, h.srv.URL+"/ns/team-a/agents/nope"); status != 404 {
		t.Errorf("unknown agent page = %d, want 404", status)
	}
}
