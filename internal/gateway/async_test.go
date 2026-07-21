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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
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

func mustIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad test IP %q", s)
	}
	return ip
}

// failingAsync always fails Patch, to exercise the retry-exhaustion path.
type failingAsync struct{ patches int }

func (f *failingAsync) Create(context.Context, string, *kaalmv1alpha1.AgentChannel, time.Time) error {
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

func TestBlockedCallbackIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "169.254.169.254", "::1", "fc00::1", "0.0.0.0", "fe80::1"}
	for _, ip := range blocked {
		if !blockedCallbackIP(mustIP(t, ip)) {
			t.Errorf("%s must be blocked", ip)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, ip := range allowed {
		if blockedCallbackIP(mustIP(t, ip)) {
			t.Errorf("%s must be allowed", ip)
		}
	}
}

func TestHandleAsyncAccept_PendingCap(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	ch := h.seedChannel("async")
	ch.Spec.Webhook.MaxPendingAsyncResponses = 1
	// Pre-fill the pending count to the cap.
	_ = h.async.Create(context.Background(), "pre-1", ch, metav1.Now().Time)

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("at-cap async = %d, want 503", resp.StatusCode)
	}
	if !bytes.Contains([]byte(resp.Header.Get("Content-Type")), []byte("json")) {
		t.Log("content-type:", resp.Header.Get("Content-Type"))
	}
	_ = resp.Body.Close()
}
