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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// captureSlog routes the default logger to a JSON buffer until the test ends.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// logRecords returns the records in buf whose message is msg.
func logRecords(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v (%s)", err, line)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func TestLLMProxy_CredentialRefusalLoggedOncePerMinute(t *testing.T) {
	buf := captureSlog(t)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	delete(h.store.creds, "prov")
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	h.server.credentialLog.now = func() time.Time { return clock }

	cert := agentCert(t, h.ca)
	send := func() {
		t.Helper()
		resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
			map[string]any{"model": "prov/m1", "messages": []any{}}, nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("refused credential should still map to 503, got %d", resp.StatusCode)
		}
		if got := errType(t, resp); got != errProviderUnavailable {
			t.Errorf("error type %q, want %q", got, errProviderUnavailable)
		}
	}

	send()
	send()
	recs := logRecords(t, buf, "llm credential unavailable")
	if len(recs) != 1 {
		t.Fatalf("want 1 credential record within a minute, got %d (%s)", len(recs), buf.String())
	}
	rec := recs[0]
	if rec["level"] != "WARN" || rec["provider"] != "prov" {
		t.Errorf("record = %v, want level WARN and provider prov", rec)
	}
	if e, _ := rec["error"].(string); !strings.Contains(e, "no credential for prov") {
		t.Errorf("error field %q should carry the store's reason", e)
	}

	clock = clock.Add(time.Minute)
	send()
	if got := len(logRecords(t, buf, "llm credential unavailable")); got != 2 {
		t.Errorf("want a second record after a minute, got %d", got)
	}
}

func TestLLMProxy_CredentialRefusalLoggedWhenFallbackServes(t *testing.T) {
	buf := captureSlog(t)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	delete(h.store.creds, "prov")
	h.addBackupProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-backup","usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "backup"}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "messages": []any{}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backup should serve the request, got %d", resp.StatusCode)
	}
	recs := logRecords(t, buf, "llm credential unavailable")
	if len(recs) != 1 || recs[0]["provider"] != "prov" {
		t.Fatalf("want one credential record naming prov, got %v", recs)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmFallback.WithLabelValues("prov", "backup", "success")); got != 1 {
		t.Errorf("fallback metric {prov,backup,success} = %v, want 1", got)
	}
}

func TestForwardOnce_CancelledContextNotLogged(t *testing.T) {
	buf := captureSlog(t)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	delete(h.store.creds, "prov")
	provider := h.store.providers["prov"]
	adapter, ok := adapterForPath("/v1/chat/completions")
	if !ok {
		t.Fatal("no adapter for chat completions")
	}
	typeAdapter, ok := adapterForProviderType(provider.Spec.Type)
	if !ok {
		t.Fatalf("no adapter for type %q", provider.Spec.Type)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"m1"}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := h.server.forwardOnce(ctx, r, provider, body, "/v1/chat/completions", adapter, typeAdapter, "m1")
	if !res.fallilable || res.class != classConnect || res.err == nil {
		t.Errorf("cancelled refusal = %+v, want fallilable connect-class error", res)
	}
	if got := len(logRecords(t, buf, "llm credential unavailable")); got != 0 {
		t.Fatalf("cancelled request should not log, got %d records", got)
	}

	res = h.server.forwardOnce(context.Background(), r, provider, body, "/v1/chat/completions", adapter, typeAdapter, "m1")
	if !res.fallilable || res.class != classConnect {
		t.Errorf("live refusal = %+v, want fallilable connect-class", res)
	}
	if got := len(logRecords(t, buf, "llm credential unavailable")); got != 1 {
		t.Errorf("live request after a cancelled one should log once, got %d records", got)
	}
}
