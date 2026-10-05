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
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
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

func (f *failingAsync) Create(context.Context, string, *kaalmv1beta1.AgentChannel, time.Time) error {
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
	s.patchWithRetry(context.Background(), "req-1", "team-a", []byte(`{}`))
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
	s.patchWithRetry(ctx, "req-1", "team-a", []byte(`{}`))
	if fa.patches != 1 {
		t.Errorf("patch attempts = %d, want 1 (cancelled before retry)", fa.patches)
	}
}

// patchDropSignals runs patchWithRetry against a failing store and returns
// the patch-failed counter for the namespace and the error log lines that
// name requestID: every drop must produce exactly one of each.
// A zero cutoff runs under a context that never ends.
func patchDropSignals(t *testing.T, cutoff time.Duration, backoff []time.Duration, requestID string) (float64, []map[string]any) {
	t.Helper()
	ctx := context.Background()
	if cutoff > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cutoff)
		defer cancel()
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	s := &Server{
		Async: &failingAsync{}, Metrics: NewMetrics(prometheus.NewRegistry()),
		Config: Config{CallbackBackoff: backoff},
	}
	s.patchWithRetry(ctx, requestID, "team-a", []byte(`{}`))

	var logs []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil && rec["requestId"] == requestID && rec["level"] == "ERROR" {
			logs = append(logs, rec)
		}
	}
	return testutil.ToFloat64(s.Metrics.patchFailed.WithLabelValues("team-a")), logs
}

func TestPatchWithRetry_ExhaustionSignalsOnce(t *testing.T) {
	count, logs := patchDropSignals(t, 0, []time.Duration{time.Millisecond, time.Millisecond}, "req-exhausted")
	if count != 1 || len(logs) != 1 {
		t.Fatalf("exhaustion: counter = %v, error logs = %d; want 1 and 1", count, len(logs))
	}
	if logs[0]["reason"] != "retries_exhausted" {
		t.Errorf("reason = %v, want retries_exhausted", logs[0]["reason"])
	}
}

// A context that ends during a backoff sleep (the pipeline bound, or any
// cancellation) drops the payload too, and must say so.
func TestPatchWithRetry_ContextCutoffSignalsOnce(t *testing.T) {
	count, logs := patchDropSignals(t, 20*time.Millisecond, []time.Duration{time.Hour}, "req-cutoff")
	if count != 1 || len(logs) != 1 {
		t.Fatalf("context cutoff: counter = %v, error logs = %d; want 1 and 1", count, len(logs))
	}
	if logs[0]["reason"] != "context_done" {
		t.Errorf("reason = %v, want context_done", logs[0]["reason"])
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

// The callbackUrl target policy (deny ranges, allowlist, and the loopback /
// cloud-metadata floor) is tested in internal/callbackpolicy, which both the
// gateway pre-dial check and the controller's rule 22 share.

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

// deadlineActivator records the pipeline deadline the wake runs under, then
// fails so the pipeline ends at once.
type deadlineActivator struct {
	deadline time.Time
	bounded  bool
}

func (d *deadlineActivator) Wake(ctx context.Context, _, _ string) error {
	d.deadline, d.bounded = ctx.Deadline()
	return errors.New("activator down")
}

func TestRunAsyncPipeline_BoundCoversLargeWakeTimeout(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	channel := h.seedChannel("async")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentHibernated
	// Far above the old fixed 10-minute ceiling. The activator fails at once,
	// so nothing waits this long.
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: 20 * time.Minute}
	act := &deadlineActivator{}
	h.server.Activator = act

	start := time.Now()
	h.server.runAsyncPipeline(context.Background(), "req-large-wake", channel, agent, MessageEnvelope{})

	if !act.bounded {
		t.Fatal("the async pipeline runs without a deadline")
	}
	// The callback schedule and the polling-record patch follow the wake and
	// the delivery, so both count toward the bound.
	want := h.server.backgroundPipelineBound(agent, asyncResponseRequests)
	if got := act.deadline.Sub(start); got < want-time.Second || got > want+time.Second {
		t.Errorf("pipeline bound = %s, want %s derived from the 20m wakeTimeout", got, want)
	}
	if got := act.deadline.Sub(start); got <= 20*time.Minute {
		t.Errorf("pipeline bound %s cuts off the 20m wakeTimeout", got)
	}
}
