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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func decodeJSON(resp *http.Response, v any) error {
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(v)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// failingAsync always fails Patch, to exercise the retry-exhaustion path.
type failingAsync struct{ patches int }

func (f *failingAsync) Create(context.Context, string, *agentryv1alpha1.AgentChannel, time.Time) error {
	return nil
}
func (f *failingAsync) Patch(context.Context, string, []byte) error {
	f.patches++
	return errors.New("patch boom")
}
func (f *failingAsync) Get(context.Context, string) (*AsyncRecord, bool, error) {
	return nil, false, nil
}
func (f *failingAsync) CountPending(context.Context, string, string) (int, error) {
	return 0, nil
}

func TestPatchWithRetry_Exhaustion(t *testing.T) {
	fa := &failingAsync{}
	s := &Server{Async: fa, Config: Config{CallbackBackoff: []time.Duration{time.Millisecond, time.Millisecond}}}
	s.patchWithRetry(context.Background(), "req-1", []byte(`{}`))
	// One immediate attempt plus one per backoff entry = 3 total.
	if fa.patches != 3 {
		t.Errorf("patch attempts = %d, want 3", fa.patches)
	}
}

func TestPatchWithRetry_ContextCancel(t *testing.T) {
	fa := &failingAsync{}
	s := &Server{Async: fa, Config: Config{CallbackBackoff: []time.Duration{time.Hour}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the delayed retry aborts on ctx.Done before firing
	s.patchWithRetry(ctx, "req-1", []byte(`{}`))
	if fa.patches != 1 {
		t.Errorf("patch attempts = %d, want 1 (cancelled before retry)", fa.patches)
	}
}

func TestCertLoader_PartialWriteFallback(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "gw")
	l := &certLoader{certFile: certFile, keyFile: keyFile, caFile: caFile}

	// Prime the caches.
	cert, err := l.certificate()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := l.caPool()
	if err != nil {
		t.Fatal(err)
	}

	// A corrupt cert (partial write) keeps serving the cached certificate.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, certFile, []byte("-----BEGIN CERTIFICATE-----\ngarbage\n-----END CERTIFICATE-----"))
	got, err := l.certificate()
	if err != nil || got != cert {
		t.Errorf("corrupt cert must fall back to cached: got=%v err=%v", got, err)
	}

	// A corrupt CA bundle keeps serving the cached pool.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, caFile, []byte("not a certificate at all"))
	gotPool, err := l.caPool()
	if err != nil || gotPool != pool {
		t.Errorf("corrupt CA must fall back to cached pool: err=%v", err)
	}
}

func TestReview_Error(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, (*authnv1.TokenReview)(nil), errors.New("apiserver down")
	})
	r := &KubeTokenReviewer{Client: client}
	if _, _, err := r.Review(context.Background(), "tok"); err == nil {
		t.Error("Review must surface apiserver errors")
	}
}

func TestNewRateLimiter_DefaultReplicas(t *testing.T) {
	rl := NewRateLimiter(nil)
	if rl.Replicas() != 1 {
		t.Errorf("nil replicas must default to 1, got %d", rl.Replicas())
	}
	rl2 := NewRateLimiter(func() int { return 3 })
	if rl2.Replicas() != 3 {
		t.Errorf("explicit replicas = %d", rl2.Replicas())
	}
}

func TestWakeAndDeliver_WakeTimeout(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = agentryv1alpha1.AgentHibernated
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: 80 * time.Millisecond}

	// Wake succeeds but the agent never becomes reachable: point delivery at a
	// closed port so waitAgentReachable exhausts its (short) wakeTimeout.
	h.server.Activator = &fakeActivator{}
	h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("wake timeout = %d, want 504", resp.StatusCode)
	}
	if got := errType(t, resp); got != errWakeTimeout {
		t.Errorf("error type = %q, want %q", got, errWakeTimeout)
	}
}

func TestRunAsyncPipeline_DeliveryFailureStored(t *testing.T) {
	// The agent always fails: the async pipeline stores an error payload.
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	h.seedChannel("async")

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != 202 {
		t.Fatalf("accept = %d", resp.StatusCode)
	}
	var accept asyncAcceptResponse
	_ = decodeJSON(resp, &accept)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, ok, _ := h.async.Get(context.Background(), accept.RequestID)
		if ok && rec.Payload != nil {
			if !containsAll(string(rec.Payload), "error", "delivery_failed") {
				t.Fatalf("stored error payload wrong: %s", rec.Payload)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("error payload never stored")
}
