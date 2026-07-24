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
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

func metav1ObjectMeta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }

func onceReset() sync.Once { return sync.Once{} }

// recordingRecorder is a no-op EventRecorder for tests that only need the
// gateway to have one wired.
type recordingRecorder struct{}

func (*recordingRecorder) Eventf(runtime.Object, string, string, string, ...any) {}

// addBackupProvider adds a same-type fallback provider backed by its own
// upstream and wires it onto the primary, extending the newHarness setup.
func (h *harness) addBackupProvider(t *testing.T, name string, upstreamFn http.HandlerFunc) *httptest.Server {
	t.Helper()
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

	h.store.providers[name] = &kaalmv1alpha1.ModelProvider{
		ObjectMeta: metav1ObjectMeta(name),
		Spec: kaalmv1alpha1.ModelProviderSpec{
			Type:              "openai",
			Endpoint:          backendSrv.URL,
			AllowedNamespaces: []string{"team-*"},
			Models:            []kaalmv1alpha1.ModelProviderModel{{ID: "m1"}},
		},
	}
	h.store.creds[name] = "sk-" + name
	return backendSrv
}

func TestIntegration_FallbackChainWalksToBackup(t *testing.T) {
	// S4: the primary returns 503; the gateway walks to the backup, which
	// succeeds, and the agent sees a 200.
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.seedRoute()
	h.server.Recorder = &recordingRecorder{}
	backup := h.addBackupProvider(t, "backup", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-backup","usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	_ = backup
	// Wire the fallback and allow the backup in the workload + class.
	h.store.providers["prov"].Spec.Fallback = []kaalmv1alpha1.LocalObjectReference{{Name: "backup"}}
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1alpha1.AgentProviderReference{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1alpha1.LocalObjectReference{Name: "backup"})

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "messages": []any{}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("fallback chain should yield 200 from the backup, got %d", resp.StatusCode)
	}
	// The backup's spend was recorded, not the primary's.
	if u := h.spend.Total("team-a", "backup", "m1"); u.InputTokens != 3 {
		t.Errorf("backup usage not recorded: %+v", u)
	}
}

func TestIntegration_FallbackExhaustionMaps503(t *testing.T) {
	// Both providers fail at the connect layer -> 503 provider_unavailable.
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	// Point both providers at a dead endpoint.
	h.store.providers["prov"].Spec.Endpoint = "https://127.0.0.1:1"
	h.store.providers["prov"].Spec.Fallback = []kaalmv1alpha1.LocalObjectReference{{Name: "backup"}}
	h.store.providers["backup"] = &kaalmv1alpha1.ModelProvider{
		ObjectMeta: metav1ObjectMeta("backup"),
		Spec: kaalmv1alpha1.ModelProviderSpec{
			Type: "openai", Endpoint: "https://127.0.0.1:1",
			AllowedNamespaces: []string{"team-*"},
			Models:            []kaalmv1alpha1.ModelProviderModel{{ID: "m1"}},
		},
	}
	h.store.creds["backup"] = "sk-backup"
	h.store.agents["team-a/sup"].Spec.Providers = append(h.store.agents["team-a/sup"].Spec.Providers,
		kaalmv1alpha1.AgentProviderReference{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: "backup"}})
	h.store.classes["std"].Spec.AllowedProviders = append(h.store.classes["std"].Spec.AllowedProviders,
		kaalmv1alpha1.LocalObjectReference{Name: "backup"})

	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("all-connect-error exhaustion should be 503, got %d", resp.StatusCode)
	}
	if got := errType(t, resp); got != errProviderUnavailable {
		t.Errorf("error type %q", got)
	}
}

func TestIntegration_RateLimit429(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	h.seedRoute()
	h.server.RateLimiter = NewRateLimiter(func() int { return 1 })
	h.store.providers["prov"].Spec.RateLimits = kaalmv1alpha1.ModelProviderRateLimits{RequestsPerMinute: 2}
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
