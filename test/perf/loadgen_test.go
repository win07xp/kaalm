//go:build perftest

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
	"testing"
)

func TestToolCallBody(t *testing.T) {
	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(toolCallBody("web_search"), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.JSONRPC != "2.0" || msg.ID != 1 || msg.Method != "tools/call" || msg.Params.Name != "web_search" {
		t.Errorf("envelope = %+v", msg)
	}
	if msg.Params.Arguments == nil {
		t.Error("arguments must be an object")
	}
	if v := msg.Params.Meta["io.modelcontextprotocol/protocolVersion"]; v != "2026-07-28" {
		t.Errorf("_meta protocolVersion = %v", v)
	}
}

func TestToolCallStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`, "200"},
		{200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tool"}}`, "rpc_error"},
		{200, `not json`, "200"},
		{503, `{"error":{"type":"tool_unavailable"}}`, "503"},
		{400, `{"jsonrpc":"2.0","id":1,"error":{"code":-32020}}`, "400"},
	}
	for _, c := range cases {
		if got := toolCallStatus(c.status, []byte(c.body)); got != c.want {
			t.Errorf("toolCallStatus(%d, %s) = %q, want %q", c.status, c.body, got, c.want)
		}
	}
}

func TestRecorderLabels(t *testing.T) {
	rec := newRecorder()
	rec.recordLabel("rpc_error", 0, nil)
	rec.record(200, 0, nil)
	res := rec.result(modeTools, 1e9, 0)
	if res.Statuses["rpc_error"] != 1 || res.Statuses["200"] != 1 || res.Requests != 2 {
		t.Errorf("statuses = %v, requests %d", res.Statuses, res.Requests)
	}
}
