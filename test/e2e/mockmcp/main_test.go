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

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, srv *httptest.Server, body, bearer, session string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return body
}

const initMsg = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
	`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func TestBearerEnforced(t *testing.T) {
	srv := httptest.NewServer(newMock("sekrit", false).handler())
	defer srv.Close()

	for _, wrong := range []string{"", "other"} {
		resp := post(t, srv, initMsg, wrong, "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("bearer %q: status %d, want 401", wrong, resp.StatusCode)
		}
	}
	resp := post(t, srv, initMsg, "sekrit", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct bearer: status %d, want 200", resp.StatusCode)
	}
}

func TestInitializeMintsSessionAndSessionIsRequired(t *testing.T) {
	srv := httptest.NewServer(newMock("", false).handler())
	defer srv.Close()

	// Non-initialize methods without a session get the protocol's 404.
	resp := post(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sessionless tools/list: status %d, want 404", resp.StatusCode)
	}

	resp = post(t, srv, initMsg, "", "")
	sess := resp.Header.Get("Mcp-Session-Id")
	body := decode(t, resp)
	if sess == "" {
		t.Fatal("initialize minted no Mcp-Session-Id")
	}
	res, _ := body["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}

	resp = post(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`, "", sess)
	body = decode(t, resp)
	res, _ = body["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("catalog size = %d, want 2 (%v)", len(tools), res)
	}
}

func TestToolsCallCountsAndIntrospection(t *testing.T) {
	m := newMock("", false)
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	resp := post(t, srv, initMsg, "", "")
	sess := resp.Header.Get("Mcp-Session-Id")
	_ = resp.Body.Close()

	call := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"web_search","arguments":{}}}`
	for i := 0; i < 2; i++ {
		body := decode(t, post(t, srv, call, "", sess))
		if body["error"] != nil {
			t.Fatalf("tools/call errored: %v", body["error"])
		}
	}
	// An uncataloged tool is a JSON-RPC error, not an HTTP failure.
	body := decode(t, post(t, srv, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"rm_rf"}}`, "", sess))
	if body["error"] == nil {
		t.Fatal("unknown tool must return a JSON-RPC error")
	}

	resp, err := http.Get(srv.URL + "/introspect/requests")
	if err != nil {
		t.Fatal(err)
	}
	intro := decode(t, resp)
	tools, _ := intro["tools"].(map[string]any)
	if tools["web_search"] != float64(2) {
		t.Errorf("web_search count = %v, want 2", tools["web_search"])
	}
	methods, _ := intro["methods"].(map[string]any)
	if methods["initialize"] != float64(1) || methods["tools/call"] != float64(3) {
		t.Errorf("method counts = %v", methods)
	}
}

func TestNotificationAndMalformed(t *testing.T) {
	srv := httptest.NewServer(newMock("", false).handler())
	defer srv.Close()

	resp := post(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: status %d, want 202", resp.StatusCode)
	}

	resp = post(t, srv, "not json", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body: status %d, want 400", resp.StatusCode)
	}

	getResp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET on the RPC path: status %d, want 404", getResp.StatusCode)
	}
}

// TestModernSurface proves the --modern server: discover answered,
// initialize refused with the versions named, headers validated, stateless
// tools/call served.
func TestModernSurface(t *testing.T) {
	srv := httptest.NewServer(newMock("", true).handler())
	defer srv.Close()

	post := func(method string, body string, headers map[string]string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Mcp-Method", method)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		return resp
	}
	decode := func(resp *http.Response) map[string]any {
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	resp := post("server/discover", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server/discover status = %d", resp.StatusCode)
	}
	result := decode(resp)["result"].(map[string]any)
	if v := result["supportedVersions"].([]any); len(v) != 1 || v[0] != "2026-07-28" {
		t.Fatalf("supportedVersions = %v", v)
	}

	resp = post("initialize", `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("initialize status = %d, want 404 on a modern-only server", resp.StatusCode)
	}
	if body := decode(resp); !strings.Contains(fmt.Sprint(body["error"]), "2026-07-28") {
		t.Fatalf("initialize rejection must name the supported versions: %v", body)
	}

	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{}}}`
	resp = post("tools/call", call, map[string]string{"Mcp-Name": "web_search"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stateless tools/call status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = post("tools/call", call, map[string]string{"Mcp-Name": "fetch_page"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched Mcp-Name status = %d, want 400", resp.StatusCode)
	}
	body := decode(resp)
	if code := body["error"].(map[string]any)["code"].(float64); code != -32020 {
		t.Fatalf("error code = %v, want -32020", code)
	}
}
