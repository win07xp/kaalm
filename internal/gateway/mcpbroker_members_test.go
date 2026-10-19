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
	"errors"
	"net/http"
	"strings"
	"testing"
)

// postMCPRaw POSTs body as sent, with headers, so a test can send members
// that a map cannot hold twice.
func postMCPRaw(t *testing.T, c *http.Client, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// A message naming method, params, or params.name twice, in any letter
// case encoding/json folds together, is refused before any upstream
// contact: the broker forwards the bytes as sent, and a tool server that
// reads the other copy would act on a value the broker never checked.
func TestMCPBroker_RefusesRepeatedMembers(t *testing.T) {
	cases := []struct {
		name, body, member string
		headers            map[string]string
	}{
		{"case variant of params.name",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_page","Name":"web_search"}}`,
			"params.name", nil},
		{"case variant of method",
			`{"jsonrpc":"2.0","id":1,"method":"resources/read","Method":"tools/list"}`,
			"method", nil},
		{"exact repeat of params.name",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_page","name":"web_search"}}`,
			"params.name", nil},
		{"escaped params.name",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_page","\u006eame":"web_search"}}`,
			"params.name", nil},
		{"escaped params",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search"},"\u0070arams":{"name":"fetch_page"}}`,
			"params", nil},
		{"long s in params",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search"},"param` + "ſ" + `":{"name":"fetch_page"}}`,
			"params", nil},
		{"upper case params",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_page"},"PARAMS":{"name":"web_search"}}`,
			"params", nil},
		{"method repeated with the same value",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","method":"tools/call","params":{"name":"web_search"}}`,
			"method", nil},
		{"modern era",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch_page","Name":"web_search"}}`,
			"params.name", modernHeaders("web_search")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(http.ResponseWriter, *http.Request) {
				t.Error("a message with a repeated member must never reach the upstream")
			})
			h.seedToolRoute()
			cert := agentCert(t, h.ca)
			resp := postMCPRaw(t, h.client(&cert), h.url("/v1/mcp/search"), tc.body, tc.headers)
			msg := expectMCPError(t, resp, http.StatusBadRequest, errInvalidRequest)
			if want := `request body has more than one "` + tc.member + `" field`; msg != want {
				t.Errorf("message = %q, want %q", msg, want)
			}
		})
	}
}

func TestRepeatedMCPMember(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`null`, ""},
		{`{}`, ""},
		{`{"method":"tools/call","params":{"name":"a","arguments":{"name":"b","Name":"c"}}}`, ""},
		{`{"method":"tools/call","params":[{"name":"a"},{"name":"b"}]}`, ""},
		{`{"method":"tools/call","params":null}`, ""},
		{`{"method":"tools/call","name":"a","Name":"b","params":{"name":"a"}}`, ""},
		{`{"method":"tools/call","params":{"Name":"a"}}`, ""},
		{`{"id":1,"ID":2,"method":"tools/list"}`, ""},
		{`{"note":"\"method\":\"x\"","method":"tools/list"}`, ""},
		{` { "method" : "tools/list" , "mEthod" : "tools/call" } `, "method"},
		{`{"method":"tools/call","params":{"name":"a"},"params":5}`, "params"},
		{`{"method":"tools/call","params":{"arguments":{},"name":"a","NAME":"b"}}`, "params.name"},
		{`{"method":"tools/call","params":{"name":"a"},"Params":{"name":"b","name":"c"}}`, "params"},
		{`{"method":"tools/call","params":{"name":"fetch_page","\u006eame":"web_search"}}`, "params.name"},
		{`{"method":"tools/call","params":{"name":"a"},"\u0070arams":{"name":"b"}}`, "params"},
		{`{"\u006dethod":"tools/call","method":"tools/list"}`, "method"},
	} {
		err := repeatedMCPMember([]byte(tc.body))
		var got string
		var dup *duplicateFieldError
		if errors.As(err, &dup) {
			got = dup.name
		} else if err != nil {
			t.Errorf("%s: error %v, want a *duplicateFieldError", tc.body, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: repeated member %q, want %q", tc.body, got, tc.want)
		}
	}
}

// A narrowed tools/list answer carries one filtered tools list under the
// exact name tools, and no tool entry that names name twice: kept entries
// are relayed as the upstream bytes, and a client parser that keeps another
// copy (encoding/json the last case-insensitive match, JavaScript and Python
// only the exact name) would otherwise see a tool the filter never checked.
func TestMCPBroker_ToolsListDropsRepeatedMembers(t *testing.T) {
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct{ name, result, want string }{
				{"long s variant",
					`{"tools":[{"name":"web_search"}],"tool` + "\u017f" + `":[{"name":"fetch_page"}]}`,
					`[{"name":"web_search"}]`},
				{"upper-case variant first",
					`{"Tools":[{"name":"fetch_page"}],"tools":[{"name":"web_search"}]}`,
					`[{"name":"web_search"}]`},
				{"lone case variant",
					`{"Tools":[{"name":"web_search"},{"name":"fetch_page"}]}`,
					`[{"name":"web_search"}]`},
				{"case-variant name in an entry",
					`{"tools":[{"name":"admin_reset","Name":"web_search"},{"name":"web_search"}]}`,
					`[{"name":"web_search"}]`},
				{"escaped repeat of name",
					`{"tools":[{"name":"web_search","\u006eame":"web_search"},{"name":"web_search"}]}`,
					`[{"name":"web_search"}]`},
				{"lone Name entry",
					`{"tools":[{"Name":"web_search"},{"Name":"fetch_page"}]}`,
					`[{"Name":"web_search"}]`},
				{"nested name",
					`{"tools":[{"name":"web_search","inputSchema":{"properties":{"name":{},"Name":{}}}}]}`,
					`[{"name":"web_search","inputSchema":{"properties":{"name":{},"Name":{}}}}]`},
			} {
				raw := relayToolsList(t, mode, `{"jsonrpc":"2.0","id":3,"result":`+tc.result+`}`)
				var reply struct {
					Result map[string]json.RawMessage `json:"result"`
				}
				if err := json.Unmarshal(raw, &reply); err != nil {
					t.Fatalf("%s: decode %s: %v", tc.name, raw, err)
				}
				var keys []string
				for k := range reply.Result {
					if strings.EqualFold(k, "tools") {
						keys = append(keys, k)
					}
				}
				if len(keys) != 1 || keys[0] != "tools" {
					t.Errorf("%s: tools members %q, want only the exact tools: %s", tc.name, keys, raw)
				}
				if got := string(reply.Result["tools"]); got != tc.want {
					t.Errorf("%s: tools = %s, want %s", tc.name, got, tc.want)
				}
				for _, ungranted := range []string{"fetch_page", "admin_reset"} {
					if bytes.Contains(raw, []byte(ungranted)) {
						t.Errorf("%s: reply carries %s: %s", tc.name, ungranted, raw)
					}
				}
			}
		})
	}
}
