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
	"crypto/tls"
	"errors"
	"net/http"
	"testing"
	"time"

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

func TestCrossCheckLive_Fallback(t *testing.T) {
	fs := newFakeStore()
	a := &Authenticator{Store: fs}
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.7:5555"

	// Informer miss, live miss: fail.
	if a.crossCheckLive(r, "team-a") {
		t.Error("miss on both lookups must fail")
	}
	// Informer miss, live hit in the SAN namespace: pass (#148).
	fs.livePodsByIP["10.0.0.7"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	if !a.crossCheckLive(r, "team-a") {
		t.Error("live fallback hit must pass")
	}
	// The live query is namespace-narrowed: the same Pod does not answer for
	// another namespace.
	if a.crossCheckLive(r, "team-b") {
		t.Error("live fallback must not match across namespaces")
	}
	// An informer hit still wins without touching the fallback.
	fs.podsByIP["10.0.0.7"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b"}}
	if a.crossCheckLive(r, "team-a") {
		t.Error("informer hit in another namespace must fail without fallback rescue")
	}
}

// TestHeartbeat_NoLiveFallback locks the spec's scoping: the live fallback
// belongs to /v1/task/complete alone; a heartbeat from a Pod only the live
// lookup could see stays a 401 and recovers on the next tick.
func TestHeartbeat_NoLiveFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	h.store.livePodsByIP["127.0.0.1"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sup-pod", Namespace: "team-a"},
	}
	cert := h.ca.issue(t, "sup.team-a.svc.cluster.local")

	resp := postJSON(t, h.client(&cert), h.url("/v1/agent/heartbeat"), map[string]any{}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401: heartbeat must not use the live fallback", resp.StatusCode)
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

func TestDualModePaths_BearerErrorBranches(t *testing.T) {
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

// TestInternalPaths_SourceIPCrossCheck: the controller and console SANs name
// operator-namespace Services, so the source IP must resolve to a Pod in the
// operator namespace. The SAN alone is not enough (#237).
func TestInternalPaths_SourceIPCrossCheck(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	controllerCert := h.ca.issue(t, "kaalm-controller.kaalm-system.svc.cluster.local")
	consoleCert := h.ca.issue(t, "kaalm-console.kaalm-system.svc.cluster.local")

	cases := []struct {
		name string
		cert *tls.Certificate
		path string
	}{
		{"controller", &controllerCert, "/v1/activity?namespace=team-a"},
		{"console", &consoleCert, "/v1/spend?namespace=team-a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, step := range []struct {
				pod  *corev1.Pod
				want int
			}{
				{nil, http.StatusUnauthorized},
				{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}, http.StatusUnauthorized},
				{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kaalm-system"}}, http.StatusOK},
			} {
				delete(h.store.podsByIP, "127.0.0.1")
				if step.pod != nil {
					h.store.podsByIP["127.0.0.1"] = step.pod
				}
				resp, err := h.client(c.cert).Get(h.url(c.path))
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != step.want {
					t.Errorf("pod %v: status %d, want %d", step.pod != nil, resp.StatusCode, step.want)
				}
				if step.want == http.StatusUnauthorized {
					if got := errType(t, resp); got != errUnauthorized {
						t.Errorf("error type %q, want %q", got, errUnauthorized)
					}
				} else {
					_ = resp.Body.Close()
				}
			}
		})
	}
}

// TestInternalPaths_MethodEnforced: a wrong method on the heartbeat, activity,
// and channel-health paths is 405 invalid_request with an Allow header (#237).
func TestInternalPaths_MethodEnforced(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	h.seedRoute()
	agentC := agentCert(t, h.ca)
	controllerCert := h.ca.issue(t, "kaalm-controller.kaalm-system.svc.cluster.local")

	cases := []struct {
		path   string
		cert   *tls.Certificate
		podNS  string
		method string
		allow  string
	}{
		{"/v1/agent/heartbeat", &agentC, "team-a", http.MethodGet, http.MethodPost},
		{"/v1/agent/heartbeat", &agentC, "team-a", http.MethodPut, http.MethodPost},
		{"/v1/activity?namespace=team-a", &controllerCert, "kaalm-system", http.MethodPost, http.MethodGet},
		{"/v1/activity?namespace=team-a", &controllerCert, "kaalm-system", http.MethodDelete, http.MethodGet},
		{"/v1/channels/health?namespace=team-a", &controllerCert, "kaalm-system", http.MethodPost, http.MethodGet},
		{"/v1/channels/health?namespace=team-a", &controllerCert, "kaalm-system", http.MethodPut, http.MethodGet},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			h.store.podsByIP["127.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: c.podNS}}
			req, err := http.NewRequest(c.method, h.url(c.path), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := h.client(c.cert).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status %d, want 405", resp.StatusCode)
			}
			if got := resp.Header.Get("Allow"); got != c.allow {
				t.Errorf("Allow %q, want %q", got, c.allow)
			}
			if got := errType(t, resp); got != errInvalidRequest {
				t.Errorf("error type %q, want %q", got, errInvalidRequest)
			}
		})
	}
}

// TestHeartbeat_RateCapped: past the per-agent burst the heartbeat answers
// 429 rate_limited with Retry-After; another agent has its own bucket (#237).
func TestHeartbeat_RateCapped(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	h.seedRoute()
	now := time.Now()
	h.server.RateLimiter.now = func() time.Time { return now }
	agentC := agentCert(t, h.ca)

	heartbeat := func(cert *tls.Certificate) *http.Response {
		return postJSON(t, h.client(cert), h.url("/v1/agent/heartbeat"), map[string]any{}, nil)
	}
	for i := 0; i < heartbeatBurst; i++ {
		resp := heartbeat(&agentC)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("heartbeat %d within the burst: status %d", i, resp.StatusCode)
		}
	}
	resp := heartbeat(&agentC)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("heartbeat over the cap: status %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if got := errType(t, resp); got != errRateLimited {
		t.Errorf("error type %q, want %q", got, errRateLimited)
	}

	other := h.ca.issue(t, "other.team-a.svc.cluster.local")
	resp = heartbeat(&other)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("another agent's heartbeat: status %d, want 200", resp.StatusCode)
	}
}
