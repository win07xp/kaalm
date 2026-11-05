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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func metav1ObjectMeta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }

func onceReset() sync.Once { return sync.Once{} }

// recordingRecorder is a no-op EventRecorder for tests that only need the
// gateway to have one wired.
type recordingRecorder struct{}

func (*recordingRecorder) Eventf(runtime.Object, runtime.Object, string, string, string, string, ...any) {
}

// addBackupProvider adds a fallback provider named "backup" (openai by
// default) backed by its own upstream, extending the newHarness setup;
// callers wire it onto the primary.
func (h *harness) addBackupProvider(t *testing.T, upstreamFn http.HandlerFunc) {
	t.Helper()
	const name = "backup"
	backendSrv := httptest.NewTLSServer(upstreamFn)
	t.Cleanup(backendSrv.Close)

	// Extend the upstream trust pool with the backup's cert.
	pool := h.server.Config.UpstreamCAs
	if pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AddCert(backendSrv.Certificate())
	h.server.Config.UpstreamCAs = pool
	// Reset the memoized upstream client so it rebuilds with the new pool.
	h.server.upstreamOnce = onceReset()

	h.store.providers[name] = &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1ObjectMeta(name),
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type:              "openai",
			Endpoint:          backendSrv.URL,
			AllowedNamespaces: []string{"team-*"},
			Models:            []kaalmv1beta1.ModelProviderModel{{ID: "m1"}},
		},
	}
	h.store.creds[name] = "sk-" + name
}

func TestIntegration_FallbackChainWalksToBackup(t *testing.T) {
	// S4: the primary returns 503; the gateway walks to the backup, which
	// succeeds, and the agent sees a 200.
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.seedRoute()
	h.server.Recorder = &recordingRecorder{}
	h.addBackupProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-backup","usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	// Wire the fallback and allow the backup in the workload + class.
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "backup"}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "messages": []any{}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("fallback chain should yield 200 from the backup, got %d", resp.StatusCode)
	}
	// The backup's spend was recorded, not the primary's.
	if u := waitUsage(t, h, "backup", "m1"); u.InputTokens != 3 {
		t.Errorf("backup usage not recorded: %+v", u)
	}
}

func TestIntegration_FallbackExhaustionMaps503(t *testing.T) {
	// Both providers fail at the connect layer -> 503 provider_unavailable.
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	// Point both providers at a dead endpoint.
	h.store.providers["prov"].Spec.Endpoint = "https://127.0.0.1:1"
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "backup"}}
	h.store.providers["backup"] = &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1ObjectMeta("backup"),
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type: "openai", Endpoint: "https://127.0.0.1:1",
			AllowedNamespaces: []string{"team-*"},
			Models:            []kaalmv1beta1.ModelProviderModel{{ID: "m1"}},
		},
	}
	h.store.creds["backup"] = "sk-backup"
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("all-connect-error exhaustion should be 503, got %d", resp.StatusCode)
	}
	if got := errType(t, resp); got != errProviderUnavailable {
		t.Errorf("error type %q", got)
	}
}

// TestIntegration_UpstreamRedirectIsRefused: the provider client never
// follows a redirect: Go strips Authorization cross-host but not
// x-api-key, so following would re-send an Anthropic-format credential to
// the redirect target. One request reaches the upstream; the refusal
// classifies as a connect failure and exhausts as provider_unavailable.
func TestIntegration_UpstreamRedirectIsRefused(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://internal.example/steal")
		w.WriteHeader(http.StatusFound)
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("redirect refusal should exhaust as 503, got %d", resp.StatusCode)
	}
	if got := errType(t, resp); got != errProviderUnavailable {
		t.Errorf("error type %q", got)
	}
	<-h.upreqs // the 302 answer itself
	select {
	case r := <-h.upreqs:
		t.Errorf("redirect was followed: a second upstream request arrived: %+v", r.header)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestIntegration_RateLimit429(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	h.seedRoute()
	h.server.RateLimiter = NewRateLimiter(func() int { return 1 })
	h.store.providers["prov"].Spec.RateLimits = kaalmv1beta1.ModelProviderRateLimits{RequestsPerMinute: 2}
	cert := agentCert(t, h.ca)
	call := func() int {
		resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// The ceiling of 2 lets two through, then 429.
	if first := call(); first != 200 {
		t.Fatalf("first call = %d, want 200", first)
	}
	if second := call(); second != 200 {
		t.Fatalf("second call = %d, want 200", second)
	}
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third call should be 429, got %d", resp.StatusCode)
	}
	if got := errType(t, resp); got != "rate_limited" {
		t.Errorf("error type %q", got)
	}
}

// ---- crossing formats (since v0.7.0) ----

// crossingHarness: an anthropic primary at the dead default upstream and an
// openai backup at its own upstream, joined by an edge with a model map.
// The caller speaks Anthropic; the backup speaks chat completions.
func crossingHarness(t *testing.T, backupFn http.HandlerFunc) (*harness, *tls.Certificate) {
	t.Helper()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.seedRoute()
	h.server.Recorder = &recordingRecorder{}
	h.store.providers["prov"].Spec.Type = "anthropic"
	h.addBackupProvider(t, backupFn)
	h.store.providers["backup"].Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "gpt-5-mini"}}
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{
		Name: "backup", ModelMap: map[string]string{"m1": "gpt-5-mini"}}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})
	cert := agentCert(t, h.ca)
	return h, &cert
}

func TestIntegration_CrossFormatFallback_AnthropicCallerOpenAIBackup(t *testing.T) {
	var got struct {
		path string
		body map[string]any
	}
	h, cert := crossingHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.path = r.URL.Path
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-9","object":"chat.completion","model":"gpt-5-mini",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"Cold."},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`))
	})
	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 64,
		"system":   "Be brief.",
		"messages": []any{map[string]any{"role": "user", "content": "Is it cold?"}},
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("crossing fallback should yield 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// The caller sees an Anthropic message naming the model that served.
	if out["type"] != "message" || out["model"] != "gpt-5-mini" || out["stop_reason"] != "end_turn" {
		t.Errorf("response not Anthropic-shaped: %v", out)
	}
	if content, _ := out["content"].([]any); len(content) != 1 || content[0].(map[string]any)["text"] != "Cold." {
		t.Errorf("content wrong: %v", out["content"])
	}
	// The backup received a chat completion on its own path, for the mapped
	// model, with the system prompt lifted and the stream_options fixup.
	if got.path != "/v1/chat/completions" || got.body["model"] != "gpt-5-mini" {
		t.Errorf("backup got %s %v", got.path, got.body["model"])
	}
	msgs, _ := got.body["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "Is it cold?" {
		t.Errorf("backup messages wrong: %v", msgs)
	}
	// Spend landed on the backup for the mapped model, read with its adapter.
	if u := waitUsage(t, h, "backup", "gpt-5-mini"); u.InputTokens != 7 || u.OutputTokens != 2 {
		t.Errorf("backup usage not recorded for the mapped model: %+v", u)
	}
	if u := h.spend.Total("team-a", "prov", "m1"); !u.isZero() {
		t.Errorf("primary must record nothing: %+v", u)
	}
}

func TestIntegration_CrossFormatFallback_Streaming(t *testing.T) {
	h, cert := crossingHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if body["stream"] != true {
			t.Errorf("backup did not receive stream: true: %v", body)
		}
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Errorf("the openai fixup must apply to the translated body: %v", body["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for _, line := range []string{
			`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-5-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"Co"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"ld."},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
			`data: [DONE]`,
		} {
			_, _ = w.Write([]byte(line + "\n\n"))
			if f != nil {
				f.Flush()
			}
		}
	})
	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 64, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d content-type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	for _, want := range []string{"event: message_start", `"text":"Co"`, `"text":"ld."`,
		"event: message_delta", `"stop_reason":"end_turn"`, `"output_tokens":3`, "event: message_stop"} {
		if !strings.Contains(text, want) {
			t.Errorf("translated stream lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "[DONE]") || strings.Contains(text, "chat.completion.chunk") {
		t.Errorf("OpenAI chunk shapes leaked to an Anthropic caller:\n%s", text)
	}
	// Usage was read from the upstream's own final chunk.
	waitForSpend(t, h, "backup", "gpt-5-mini", 5)
}

func waitForSpend(t *testing.T, h *harness, provider, model string, input int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if u := h.spend.Total("team-a", provider, model); u.InputTokens == input {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("spend for %s/%s never reached %d input tokens: %+v", provider, model, input, h.spend.Total("team-a", provider, model))
}

func TestIntegration_CrossFormatFallback_OpenAICallerAnthropicBackup(t *testing.T) {
	var gotBody map[string]any
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.seedRoute() // prov is openai; the caller speaks chat completions
	h.server.Recorder = &recordingRecorder{}
	h.addBackupProvider(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("anthropic backup got path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_9","type":"message","role":"assistant","model":"claude-sonnet-4-6",
		  "content":[{"type":"text","text":"Warm."}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":1}}`))
	})
	max := int64(4096)
	h.store.providers["backup"].Spec.Type = "anthropic"
	h.store.providers["backup"].Spec.Models = []kaalmv1beta1.ModelProviderModel{{ID: "claude-sonnet-4-6", MaxOutputTokens: &max}}
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{
		Name: "backup", ModelMap: map[string]string{"m1": "claude-sonnet-4-6"}}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})
	cert := agentCert(t, h.ca)

	// No max_tokens in the request: the catalog's ceiling is supplied.
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{
		"model": "prov/m1", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["object"] != "chat.completion" || out["model"] != "claude-sonnet-4-6" {
		t.Errorf("response not a chat completion: %v", out)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "Warm." || choice["finish_reason"] != "stop" {
		t.Errorf("choice wrong: %v", choice)
	}
	if gotBody["max_tokens"] != float64(4096) || gotBody["model"] != "claude-sonnet-4-6" {
		t.Errorf("backup request wrong: %v", gotBody)
	}
	if u := waitUsage(t, h, "backup", "claude-sonnet-4-6"); u.InputTokens != 4 {
		t.Errorf("spend wrong: %+v", u)
	}
}

func TestIntegration_CrossFormatFallback_UntranslatableSkipsTheCandidate(t *testing.T) {
	backupHit := false
	h, cert := crossingHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		backupHit = true
		w.WriteHeader(200)
	})
	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 64,
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	// The primary's 503 was the only attempt: the crossing candidate was
	// ineligible for this request, so the walk exhausted.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502 provider_error", resp.StatusCode)
	}
	if backupHit {
		t.Error("an untranslatable request must never reach the crossing candidate")
	}
}

func TestIntegration_CrossFormatFallback_ErrorEnvelopeIsTheCallers(t *testing.T) {
	h, cert := crossingHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"context too long","type":"invalid_request_error","code":null}}`))
	})
	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want the backup's 400 relayed", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["type"] != "error" || out["error"].(map[string]any)["message"] != "context too long" {
		t.Errorf("error not in the Anthropic envelope: %v", out)
	}
}

func TestIntegration_LegacyCompletionsNeverCross(t *testing.T) {
	backupHit := false
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.seedRoute()
	h.server.Recorder = &recordingRecorder{}
	h.addBackupProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		backupHit = true
		w.WriteHeader(200)
	})
	h.store.providers["backup"].Spec.Type = "anthropic"
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "backup"}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/completions"), map[string]any{"model": "prov/m1", "prompt": "hi"}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway || backupHit {
		t.Errorf("legacy completions must not cross: status %d backupHit %v", resp.StatusCode, backupHit)
	}
}

func TestIntegration_PreHeaderStallFallsBackAsATimeout(t *testing.T) {
	// The primary holds its headers past the bound: the attempt is a
	// timeout and the walk moves to the backup.
	h := newHarness(t, func(_ http.ResponseWriter, r *http.Request) { stall(r) })
	h.seedRoute()
	h.server.Recorder = &recordingRecorder{}
	h.server.Config.UpstreamTimeout = streamTimeout
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
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want the backup's 200", resp.StatusCode)
	}
	if u := waitUsage(t, h, "backup", "m1"); u.InputTokens != 3 {
		t.Errorf("backup usage not recorded: %+v", u)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmFallback.WithLabelValues("prov", "backup", "success")); got != 1 {
		t.Errorf("fallback success count = %v, want 1", got)
	}
}

func TestIntegration_PreHeaderStallWithNoFallbackIs504(t *testing.T) {
	h := newHarness(t, func(_ http.ResponseWriter, r *http.Request) { stall(r) })
	h.seedRoute()
	h.server.Config.UpstreamTimeout = streamTimeout
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status %d, want 504", resp.StatusCode)
	}
	if got := errType(t, resp); got != errProviderTimeout {
		t.Errorf("error type %q", got)
	}
}

func TestIntegration_CrossFormatStreamFailureUsesTheCallersShape(t *testing.T) {
	// The OpenAI backup drops the connection mid-stream; the Anthropic caller
	// gets an Anthropic error event, not a translated message_stop.
	h, cert := crossingHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `data: {"id":"c","object":"chat.completion.chunk","model":"gpt-5-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"Co"},"finish_reason":null}]}`)
		panic(http.ErrAbortHandler)
	})
	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 64, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if resp.StatusCode != 200 || !strings.Contains(text, `"text":"Co"`) {
		t.Fatalf("status %d, partial stream not relayed:\n%s", resp.StatusCode, text)
	}
	want := "event: error\n" + `data: {"type":"error","error":{"type":"provider_error","message":`
	if !strings.Contains(text, want) {
		t.Errorf("cross-format failure lacks the Anthropic error event:\n%s", text)
	}
	if strings.Contains(text, "message_stop") || strings.Contains(text, "[DONE]") {
		t.Errorf("a truncated stream must not look complete:\n%s", text)
	}
}

// A caller that leaves a non-stream request before the provider's headers
// arrive ends the walk: the request counts as client_closed, the fallback is
// not tried, and no spend settles, since no answer had started.
func TestIntegration_CallerLeftNonStreamEndsTheWalk(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	h := newHarness(t, func(_ http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	h.seedRoute()
	h.server.Recorder = &recordingRecorder{}
	var backupCalls atomic.Int64
	h.addBackupProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-backup","usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	h.store.providers["prov"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "backup"}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "backup"})

	cert := agentCert(t, h.ca)
	raw, _ := json.Marshal(map[string]any{"model": "prov/m1", "messages": []any{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url("/v1/chat/completions"), bytes.NewReader(raw))
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
		t.Fatal("the provider never got the request")
	}
	cancel()
	<-done

	requests := func(status string) float64 {
		return testutil.ToFloat64(h.server.Metrics.llmRequests.WithLabelValues("prov", "m1", "team-a", status))
	}
	waitFor(t, func() bool { return requests(outcomeClientClosed) == 1 })
	for _, status := range []string{outcomeOK, outcomeError} {
		if got := requests(status); got != 0 {
			t.Errorf("%s requests = %v, want 0", status, got)
		}
	}
	if got := backupCalls.Load(); got != 0 {
		t.Errorf("backup called %d times after the caller left", got)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmFallback.WithLabelValues("prov", "backup", "connect_error")); got != 0 {
		t.Errorf("fallback counted %v times after the caller left", got)
	}
	if u := h.spend.Total("team-a", "prov", "m1"); u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("spend settled for an answer that never started: %+v", u)
	}
}

// A caller that leaves while a crossing answer is held does not end the
// hold: the answer is read to its end, spend settles from its usage, which
// the provider sends last, and the request counts as client_closed.
func TestIntegration_CrossFormatFallback_CallerLeftDuringTheHoldSettlesUsage(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	h, cert := crossingHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-9","object":"chat.completion","model":"gpt-5-mini",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"Co`)
		w.(http.Flusher).Flush()
		arrived <- struct{}{}
		<-release
		_, _ = io.WriteString(w, `ld."},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	})
	raw, _ := json.Marshal(map[string]any{"model": "prov/m1", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url("/v1/messages"), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := h.client(cert).Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the backup never answered")
	}
	cancel()
	<-done
	// Let the gateway see the caller leave before the rest arrives.
	time.Sleep(100 * time.Millisecond)
	close(release)

	waitForSpend(t, h, "backup", "gpt-5-mini", 7)
	requests := func(status string) float64 {
		return testutil.ToFloat64(h.server.Metrics.llmRequests.WithLabelValues("backup", "gpt-5-mini", "team-a", status))
	}
	waitFor(t, func() bool { return requests(outcomeClientClosed) == 1 })
	for _, status := range []string{outcomeOK, outcomeError} {
		if got := requests(status); got != 0 {
			t.Errorf("%s requests = %v, want 0", status, got)
		}
	}
}

func TestIntegration_CrossFormatFallback_OverLimitAnswerIs502(t *testing.T) {
	// The Anthropic caller allows 16 output tokens; the OpenAI backup sends
	// an answer far past what 16 tokens can render, usage last.
	h, cert := crossingHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-9","choices":[{"index":0,"message":{"role":"assistant","content":"`)
		_, _ = io.WriteString(w, strings.Repeat("z", int(crossingHoldLimit(16))+256))
		_, _ = io.WriteString(w, `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	})
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})
	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if resp.StatusCode != http.StatusBadGateway {
		_ = resp.Body.Close()
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if got := errType(t, resp); got != errProviderError {
		t.Errorf("error type %q, want %s", got, errProviderError)
	}
	if u := waitUsage(t, h, "backup", "gpt-5-mini"); u.InputTokens != 7 || u.OutputTokens != 2 {
		t.Errorf("backup spend %+v, want the usage read while draining", u)
	}
	waitFor(t, func() bool { _, slots := boundarySlots(h); return slots == 0 })
}

func TestIntegration_CrossFormatFallback_CutOffAnswerDoesNotFallBack(t *testing.T) {
	// The crossing backup cuts its 2xx partway; its own fallback is never
	// tried, since the 2xx had started.
	h, cert := crossingHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-9","choices":[{"index":0,"message":{"content":"half`)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	childCalls := addCrossingChild(t, h)

	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if resp.StatusCode != http.StatusBadGateway {
		_ = resp.Body.Close()
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if got := errType(t, resp); got != errProviderError {
		t.Errorf("error type %q, want %s", got, errProviderError)
	}
	if got := childCalls.Load(); got != 0 {
		t.Errorf("the backup's fallback was called %d times after the 2xx started", got)
	}
}

// bigOpenAIAnswer answers a chat completion whose content is n bytes, past
// the 1 MiB hold floor when n is larger.
func bigOpenAIAnswer(n int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-9","choices":[{"index":0,"message":{"role":"assistant","content":"`)
		_, _ = io.WriteString(w, strings.Repeat("z", n))
		_, _ = io.WriteString(w, `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}
}

// answerTextLen decodes an Anthropic message and returns its text length.
func answerTextLen(t *testing.T, resp *http.Response) int {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 1 {
		t.Fatalf("content blocks %d, want 1", len(out.Content))
	}
	return len(out.Content[0].Text)
}

func TestIntegration_CrossFormatFallback_HoldLimitFollowsMaxTokens(t *testing.T) {
	// A request allowing 200000 output tokens may get an answer far past the
	// 1 MiB floor: the hold limit grows with max_tokens, so it translates.
	const size = 3 << 19 // 1.5 MiB
	h, cert := crossingHarness(t, bigOpenAIAnswer(size))
	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 200000,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("status %d, want 200: the answer is within what max_tokens allows", resp.StatusCode)
	}
	if got := answerTextLen(t, resp); got != size {
		t.Errorf("translated text %d bytes, want %d", got, size)
	}
}

func TestIntegration_CrossFormatFallback_HoldLimitFollowsMaxOutputTokens(t *testing.T) {
	// The caller sets no max_tokens, so the translated request carries none:
	// the limit comes from the backup model's declared maxOutputTokens.
	const size = 3 << 19 // 1.5 MiB
	h, cert := crossingHarness(t, bigOpenAIAnswer(size))
	maxOut := int64(100000)
	h.store.providers["backup"].Spec.Models[0].MaxOutputTokens = &maxOut
	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model":    "prov/m1",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("status %d, want 200: the answer is within what maxOutputTokens allows", resp.StatusCode)
	}
	if got := answerTextLen(t, resp); got != size {
		t.Errorf("translated text %d bytes, want %d", got, size)
	}
}

// addCrossingChild gives the crossing backup its own fallback, "child", and
// returns its call count.
func addCrossingChild(t *testing.T, h *harness) *atomic.Int64 {
	t.Helper()
	childCalls := new(atomic.Int64)
	childSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		childCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-child","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	t.Cleanup(childSrv.Close)
	h.server.Config.UpstreamCAs.AddCert(childSrv.Certificate())
	h.server.upstreamOnce = onceReset()
	h.store.providers["child"] = &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1ObjectMeta("child"),
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type: "openai", Endpoint: childSrv.URL, AllowedNamespaces: []string{"team-*"},
			Models: []kaalmv1beta1.ModelProviderModel{{ID: "gpt-5-mini"}},
		},
	}
	h.store.creds["child"] = "sk-child"
	h.store.providers["backup"].Spec.Fallback = []kaalmv1beta1.FallbackReference{{Name: "child"}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1beta1.AgentProviderReference{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "child"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1beta1.LocalObjectReference{Name: "child"})
	return childCalls
}

func TestIntegration_CrossFormatFallback_IdleAnswerIs504(t *testing.T) {
	// The crossing backup sends its 2xx headers and a prefix, then goes
	// idle: the gap bound ends the hold with 504 provider_timeout, and the
	// backup's own fallback is never tried, since the 2xx had started.
	h, cert := crossingHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-9","choices":[{"index":0,"message":{"content":"half`)
		w.(http.Flusher).Flush()
		stall(r)
	})
	h.server.Config.UpstreamTimeout = streamTimeout
	childCalls := addCrossingChild(t, h)
	prov := hardenProv(h)
	h.server.Budget.FoldPeers(prov, map[string]float64{"team-a": 96})

	resp := postJSON(t, h.client(cert), h.url("/v1/messages"), map[string]any{
		"model": "prov/m1", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		_ = resp.Body.Close()
		t.Fatalf("status %d, want 504", resp.StatusCode)
	}
	if got := errType(t, resp); got != errProviderTimeout {
		t.Errorf("error type %q, want %s", got, errProviderTimeout)
	}
	if got := childCalls.Load(); got != 0 {
		t.Errorf("the backup's fallback was called %d times after the 2xx started", got)
	}
	waitFor(t, func() bool { _, slots := boundarySlots(h); return slots == 0 })
}
