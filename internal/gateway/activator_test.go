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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestControllerActivator_Wake(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if strings.Contains(r.URL.Path, "boom") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	act := &ControllerActivator{BaseURL: srv.URL + "/", Client: srv.Client()}
	if err := act.Wake(context.Background(), "team-a", "sup"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if gotPath != "/v1/activate/team-a/sup" {
		t.Errorf("activate path = %q", gotPath)
	}

	// Non-202 is an error.
	if err := act.Wake(context.Background(), "team-a", "boom"); err == nil {
		t.Error("non-202 must be an error")
	}

	// Unreachable base URL: transport error.
	dead := &ControllerActivator{BaseURL: "http://127.0.0.1:1", Client: &http.Client{Timeout: time.Second}}
	if err := dead.Wake(context.Background(), "team-a", "sup"); err == nil {
		t.Error("unreachable activator must error")
	}
}

// TestControllerActivator_WakeRetry pins the single retry (#207): a transport
// error or a 5xx is retried once on a fresh connection, a 4xx never is, and
// the retry stays inside the caller's deadline.
func TestControllerActivator_WakeRetry(t *testing.T) {
	serve := func(first func(w http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if n.Add(1) == 1 {
				first(w)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(srv.Close)
		return srv, &n
	}

	// A 5xx on the first attempt: the second is accepted.
	srv, n := serve(func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
	act := &ControllerActivator{BaseURL: srv.URL, Client: srv.Client()}
	if err := act.Wake(context.Background(), "team-a", "sup"); err != nil {
		t.Errorf("Wake after one 503: %v", err)
	}
	if got := n.Load(); got != 2 {
		t.Errorf("attempts after a 503 = %d, want 2", got)
	}

	// A dropped connection on the first attempt (a replica shutting down):
	// the retry dials anew and is accepted.
	srv, n = serve(func(w http.ResponseWriter) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	act = &ControllerActivator{BaseURL: srv.URL, Client: srv.Client()}
	if err := act.Wake(context.Background(), "team-a", "sup"); err != nil {
		t.Errorf("Wake after a dropped connection: %v", err)
	}
	if got := n.Load(); got != 2 {
		t.Errorf("attempts after a dropped connection = %d, want 2", got)
	}

	// A 4xx is final.
	srv, n = serve(func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) })
	act = &ControllerActivator{BaseURL: srv.URL, Client: srv.Client()}
	if err := act.Wake(context.Background(), "team-a", "sup"); err == nil {
		t.Error("a 404 must be an error")
	}
	if got := n.Load(); got != 1 {
		t.Errorf("attempts after a 404 = %d, want 1 (no retry)", got)
	}

	// A hung activator: both attempts together respect the caller deadline.
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hung.Close()
	defer close(release)
	act = &ControllerActivator{BaseURL: hung.URL, Client: hung.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := act.Wake(ctx, "team-a", "sup"); err == nil {
		t.Error("a hung activator must error")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Wake took %s, past the 300ms caller deadline", took)
	}
}

func TestNewControllerActivator(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "gw")
	act, err := NewControllerActivator("kaalm-system", certFile, keyFile, caFile)
	if err != nil {
		t.Fatalf("NewControllerActivator: %v", err)
	}
	if !strings.Contains(act.BaseURL, "kaalm-controller.kaalm-system.svc.cluster.local:9443") {
		t.Errorf("base URL = %q", act.BaseURL)
	}
	if act.Client == nil {
		t.Error("client must be built")
	}

	// A missing cert file fails construction.
	if _, err := NewControllerActivator("kaalm-system", "/nope/tls.crt", keyFile, caFile); err == nil {
		t.Error("missing cert must error")
	}
	if _, err := NewControllerActivator("kaalm-system", certFile, keyFile, "/nope/ca.crt"); err == nil {
		t.Error("missing CA must error")
	}
}

func TestKubeTokenReviewer_Review(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	// The fake apiserver echoes an authenticated TokenReview.
	client.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		tr := action.(clienttesting.CreateAction).GetObject().(*authnv1.TokenReview)
		if tr.Spec.Audiences[0] != tokenAudience {
			t.Errorf("audience = %v", tr.Spec.Audiences)
		}
		tr.Status = authnv1.TokenReviewStatus{
			Authenticated: true,
			User:          authnv1.UserInfo{Username: "system:serviceaccount:team-a:runner"},
		}
		return true, tr, nil
	})
	r := &KubeTokenReviewer{Client: client}
	username, ok, err := r.Review(context.Background(), "tok")
	if err != nil || !ok || username != "system:serviceaccount:team-a:runner" {
		t.Fatalf("Review = %q ok=%v err=%v", username, ok, err)
	}
}

// fakeActivator records wake calls and returns a configurable error.
type fakeActivator struct {
	calls []string
	err   error
}

func (f *fakeActivator) Wake(_ context.Context, ns, name string) error {
	f.calls = append(f.calls, ns+"/"+name)
	return f.err
}

func TestWakeAndDeliver_Hibernated(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"awake"}`))
	})
	h.seedChannel("sync")
	h.store.agents["team-a/sup"].Status.Phase = kaalmv1beta1.AgentHibernated

	// No activator configured: controller-down error surfaces as 504.
	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("hibernated without activator = %d, want 504", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// With a working activator: wake fires, the agent becomes reachable (the
	// override points at the running agent server), and delivery succeeds.
	act := &fakeActivator{}
	h.server.Activator = act
	resp = h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != 200 {
		t.Errorf("hibernated with activator = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(act.calls) == 0 || act.calls[0] != "team-a/sup" {
		t.Errorf("activator not called: %v", act.calls)
	}

	// Activator returns an error: controller-down 504.
	act2 := &fakeActivator{err: context.DeadlineExceeded}
	h.server.Activator = act2
	resp = h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("activator error = %d, want 504", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestWakeAndDeliver_Hibernating pins that a message arriving while the Pod
// is being deleted requests a wake instead of failing delivery (#207).
func TestWakeAndDeliver_Hibernating(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"awake"}`))
	})
	h.seedChannel("sync")
	h.store.agents["team-a/sup"].Status.Phase = kaalmv1beta1.AgentHibernating
	act := &fakeActivator{}
	h.server.Activator = act

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != 200 {
		t.Errorf("hibernating with activator = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(act.calls) != 1 || act.calls[0] != "team-a/sup" {
		t.Errorf("activator calls = %v, want one wake for team-a/sup", act.calls)
	}
}

func TestWakeTimeout(t *testing.T) {
	s := &Server{}
	// Default when unset.
	def := &kaalmv1beta1.Agent{}
	if got := s.wakeTimeout(def); got != 120*time.Second {
		t.Errorf("default wakeTimeout = %v", got)
	}
	// Spec override.
	withTimeout := &kaalmv1beta1.Agent{
		Spec: kaalmv1beta1.AgentSpec{
			Lifecycle: kaalmv1beta1.AgentLifecycle{
				WakeTimeout: metav1.Duration{Duration: 42 * time.Second},
			},
		},
	}
	if got := s.wakeTimeout(withTimeout); got != 42*time.Second {
		t.Errorf("spec wakeTimeout = %v", got)
	}
	// The controller's effective value (class default and cap applied)
	// wins over the spec.
	withTimeout.Status.EffectiveWakeTimeout = &metav1.Duration{Duration: 5 * time.Minute}
	if got := s.wakeTimeout(withTimeout); got != 5*time.Minute {
		t.Errorf("status effectiveWakeTimeout = %v", got)
	}
}

func TestAgentServiceHostPort(t *testing.T) {
	// No overrides: derived Service DNS and default port.
	s := &Server{}
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	if host := s.agentServiceHost(agent); host != "sup.team-a.svc.cluster.local" {
		t.Errorf("host = %q", host)
	}
	if port := s.agentServicePort(agent); port != 8080 {
		t.Errorf("default port = %d", port)
	}
	// Spec service port wins over the default.
	agent.Spec.Service = &kaalmv1beta1.AgentService{Port: 9000}
	if port := s.agentServicePort(agent); port != 9000 {
		t.Errorf("spec port = %d", port)
	}
	// Overrides win over everything.
	s.Config.AgentServiceHostOverride = "127.0.0.1"
	s.Config.AgentServicePortOverride = 7000
	if host := s.agentServiceHost(agent); host != "127.0.0.1" {
		t.Errorf("override host = %q", host)
	}
	if port := s.agentServicePort(agent); port != 7000 {
		t.Errorf("override port = %d", port)
	}
}
