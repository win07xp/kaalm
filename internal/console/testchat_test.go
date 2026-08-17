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
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGatewayChatClient_RequestShapeAndRelay(t *testing.T) {
	var got map[string]string
	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"type":"delivery_failed"}}`))
	}))
	defer srv.Close()

	c := &GatewayChatClient{BaseURL: srv.URL, Insecure: true}
	status, body, err := c.Chat(context.Background(), "team-a", "sup", "priya", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/test-chat" {
		t.Errorf("path = %q", gotPath)
	}
	want := map[string]string{"namespace": "team-a", "agent": "sup", "userId": "priya", "content": "hello"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("request %s = %q, want %q", k, got[k], v)
		}
	}
	// The gateway's status and body are relayed verbatim, never re-mapped.
	if status != 502 || string(body) != `{"error":{"type":"delivery_failed"}}` {
		t.Errorf("relay = %d %s", status, body)
	}
}

func TestNewGatewayChatClient_Defaults(t *testing.T) {
	c := NewGatewayChatClient("kaalm-system", "/var/run/kaalm/tls.crt", "/var/run/kaalm/tls.key", "/var/run/kaalm/ca.crt")
	if c.BaseURL != "https://kaalm-gateway.kaalm-system.svc.cluster.local:8443" {
		t.Errorf("baseURL = %q", c.BaseURL)
	}
	if c.ServerName != "kaalm-gateway.kaalm-system.svc.cluster.local" {
		t.Errorf("serverName = %q", c.ServerName)
	}
	if c.Loader == nil || c.Loader.CertFile != "/var/run/kaalm/tls.crt" {
		t.Error("loader must carry the console identity paths")
	}

	// A missing identity surfaces as an error, not a certless call.
	if _, _, err := c.Chat(context.Background(), "ns", "a", "u", "c"); err == nil {
		t.Error("a missing cert file must error")
	}
}
