/*
Copyright 2026.

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
	"errors"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCrossCheck(t *testing.T) {
	fs := newFakeStore()
	fs.podsByIP["10.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	a := &Authenticator{Store: fs}

	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	if !a.crossCheck(r, "team-a") {
		t.Error("matching namespace must pass")
	}
	if a.crossCheck(r, "team-b") {
		t.Error("mismatched namespace must fail")
	}
	// No Pod at the source IP.
	r.RemoteAddr = "10.0.0.99:5555"
	if a.crossCheck(r, "team-a") {
		t.Error("no pod at source IP must fail")
	}
	// DisableSourceIPCheck short-circuits to true.
	a.DisableSourceIPCheck = true
	if !a.crossCheck(r, "team-a") {
		t.Error("disabled cross-check must pass")
	}
}

func TestSourceIP(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "192.168.1.5:44321"
	if got := sourceIP(r); got != "192.168.1.5" {
		t.Errorf("sourceIP = %q", got)
	}
	// No port: the raw RemoteAddr is returned.
	r.RemoteAddr = "bare-host"
	if got := sourceIP(r); got != "bare-host" {
		t.Errorf("sourceIP without port = %q", got)
	}
}

func TestLLMPaths_BearerErrorBranches(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	// A plain Deployment pod in team-b (not Kaalm-managed) so the precheck
	// passes and the TokenReview path runs.
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "team-b"}}

	// TokenReview transport error: 503.
	h.reviewer.err = errors.New("apiserver down")
	resp := postJSON(t, h.client(nil), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"}, map[string]string{"Authorization": "Bearer tok"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("TokenReview error = %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Token rejected (authenticated=false): 401.
	h.reviewer.err = nil
	h.reviewer.authenticated = false
	resp = postJSON(t, h.client(nil), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"}, map[string]string{"Authorization": "Bearer tok"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("rejected token = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// No credential at all: 401.
	resp = postJSON(t, h.client(nil), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
