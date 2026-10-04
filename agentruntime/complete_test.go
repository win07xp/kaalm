// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastStaleRetries(t *testing.T) {
	t.Helper()
	restore := staleRetrySchedule
	restoreCap := retryAfterCap
	staleRetrySchedule = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	retryAfterCap = 5 * time.Millisecond
	t.Cleanup(func() {
		staleRetrySchedule = restore
		retryAfterCap = restoreCap
	})
}

func completionAgent(t *testing.T, pki *testPKI, gatewayURL string) *Agent {
	t.Helper()
	return &Agent{Gateway: newGateway(gatewayURL, testReloader(t, pki)), isTask: true}
}

// A 409 stale_pod (StalePodCompletion) covers reconciler lag and must be
// retried on the bounded schedule; the first clean 200 ends the attempt loop.
func TestCompleteTask_RetriesStaleThenSucceeds(t *testing.T) {
	fastStaleRetries(t)
	pki := newTestPKI(t)
	var attempts atomic.Int32
	var lastBody atomic.Value
	srv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req completionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		lastBody.Store(req)
		if attempts.Add(1) < 3 {
			http.Error(w, `{"error":{"type":"stale_pod","message":"StalePodCompletion: stale","retryable":true}}`,
				http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	a := completionAgent(t, pki, srv.URL)
	err := a.CompleteTask(context.Background(), "success", "did the thing", map[string]string{"out": "x"})
	if err != nil {
		t.Fatalf("completion must succeed after stale retries: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (two stale, one clean)", attempts.Load())
	}
	sent := lastBody.Load().(completionRequest)
	if sent.Status != "success" || sent.Message != "did the thing" || sent.Artifacts["out"] != "x" {
		t.Errorf("completion body wrong: %+v", sent)
	}
}

func TestCompleteTask_AlreadyCompletedIsTerminal(t *testing.T) {
	fastStaleRetries(t)
	pki := newTestPKI(t)
	var attempts atomic.Int32
	srv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, `{"error":{"type":"TaskAlreadyCompleted"}}`, http.StatusForbidden)
	}))

	err := completionAgent(t, pki, srv.URL).CompleteTask(context.Background(), "success", "", nil)
	if !errors.Is(err, ErrTaskAlreadyCompleted) {
		t.Fatalf("err = %v, want ErrTaskAlreadyCompleted", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("a terminal 403 must not be retried, saw %d attempts", attempts.Load())
	}
}

// The contract names only the 409 form; a 403 carrying the old
// StalePodCompletion text is a plain failure and is not retried.
func TestCompleteTask_Stale403IsNotRetried(t *testing.T) {
	fastStaleRetries(t)
	pki := newTestPKI(t)
	var attempts atomic.Int32
	srv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, `{"error":{"type":"access_denied","message":"StalePodCompletion: stale"}}`, http.StatusForbidden)
	}))

	if err := completionAgent(t, pki, srv.URL).CompleteTask(context.Background(), "success", "", nil); err == nil {
		t.Fatal("a 403 must surface as an error")
	}
	if attempts.Load() != 1 {
		t.Errorf("a 403 must not be retried, saw %d attempts", attempts.Load())
	}
}

func TestCompleteTask_OtherErrorsAreImmediate(t *testing.T) {
	fastStaleRetries(t)
	pki := newTestPKI(t)
	var attempts atomic.Int32
	srv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))

	if err := completionAgent(t, pki, srv.URL).CompleteTask(context.Background(), "failure", "", nil); err == nil {
		t.Fatal("a 500 must surface as an error")
	}
	if attempts.Load() != 1 {
		t.Errorf("non-retryable failures must not be retried, saw %d attempts", attempts.Load())
	}
}

// Transport-level failures retry on the schedule and exhaust with the last
// error wrapped; cancellation cuts the wait short.
func TestCompleteTask_ExhaustsAndHonorsContext(t *testing.T) {
	fastStaleRetries(t)
	pki := newTestPKI(t)
	unreachable := completionAgent(t, pki, "https://127.0.0.1:1")
	if err := unreachable.CompleteTask(context.Background(), "success", "", nil); err == nil {
		t.Fatal("an unreachable gateway must exhaust retries with an error")
	}

	staleRetrySchedule = []time.Duration{time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := completionAgent(t, pki, "https://127.0.0.1:1").CompleteTask(ctx, "success", "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation during backoff must return the context error, got %v", err)
	}
}

const unavailableBody = `{"error":{"type":"internal_unavailable","message":"unrecorded","retryable":true}}`

// unavailableGateway answers 503 internal_unavailable with the given
// Retry-After for the first failures requests, then 200. It counts requests.
func unavailableGateway(t *testing.T, pki *testPKI, failures int32, retryAfter string) (string, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	srv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= failures {
			w.Header().Set("Retry-After", retryAfter)
			http.Error(w, unavailableBody, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return srv.URL, &attempts
}

// A 503 internal_unavailable means the gateway could not record the report;
// it is retried, and the wait before the next attempt is at least
// Retry-After (capped), not the shorter schedule step.
func TestCompleteTask_RetriesUnavailableThenSucceeds(t *testing.T) {
	fastStaleRetries(t)
	retryAfterCap = 30 * time.Millisecond
	pki := newTestPKI(t)
	url, attempts := unavailableGateway(t, pki, 2, "1")

	start := time.Now()
	if err := completionAgent(t, pki, url).CompleteTask(context.Background(), "success", "", nil); err != nil {
		t.Fatalf("completion must succeed after 503 retries: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (two 503s, one clean)", attempts.Load())
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 60ms: each wait after a 503 must be raised to Retry-After (capped at 30ms)", elapsed)
	}
}

// Four 503s exhaust the schedule; the error wraps the last 503 rejection.
func TestCompleteTask_UnavailableExhausts(t *testing.T) {
	fastStaleRetries(t)
	pki := newTestPKI(t)
	url, attempts := unavailableGateway(t, pki, 100, "1")

	err := completionAgent(t, pki, url).CompleteTask(context.Background(), "success", "", nil)
	if err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("err = %v, want an exhausted-retries error", err)
	}
	var rejected *completionRejected
	if !errors.As(err, &rejected) || rejected.status != http.StatusServiceUnavailable {
		t.Errorf("err = %v, want it to wrap the 503 rejection", err)
	}
	if attempts.Load() != 4 {
		t.Errorf("attempts = %d, want 4", attempts.Load())
	}
}

// Cancellation cuts a Retry-After wait short.
func TestCompleteTask_UnavailableWaitHonorsContext(t *testing.T) {
	fastStaleRetries(t)
	retryAfterCap = time.Hour
	pki := newTestPKI(t)
	url, attempts := unavailableGateway(t, pki, 100, "3600")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := completionAgent(t, pki, url).CompleteTask(ctx, "success", "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want 1", attempts.Load())
	}
}

// Only the gateway's internal_unavailable 503 is retried; a bare 503 (a
// proxy, a plain-text body) is a rejection at once.
func TestCompleteTask_Bare503IsNotRetried(t *testing.T) {
	fastStaleRetries(t)
	pki := newTestPKI(t)
	var attempts atomic.Int32
	srv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "upstream down", http.StatusServiceUnavailable)
	}))

	if err := completionAgent(t, pki, srv.URL).CompleteTask(context.Background(), "success", "", nil); err == nil {
		t.Fatal("a bare 503 must surface as an error")
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want 1", attempts.Load())
	}
}

func TestUnavailableWait(t *testing.T) {
	const limit = 30 * time.Second
	cases := []struct {
		header string
		limit  time.Duration
		want   time.Duration
	}{
		{"1", limit, time.Second},
		{"", limit, time.Second},
		{"0", limit, 0},
		{"5", limit, 5 * time.Second},
		{" 2 ", limit, 2 * time.Second},
		{"600", limit, 30 * time.Second},
		{"-3", limit, time.Second},
		{"+5", limit, time.Second},
		{"Wed, 21 Oct 2015 07:28:00 GMT", limit, time.Second},
		{"", 5 * time.Millisecond, 5 * time.Millisecond},
	}
	for _, c := range cases {
		if got := unavailableWait(c.header, c.limit); got != c.want {
			t.Errorf("unavailableWait(%q, %v) = %v, want %v", c.header, c.limit, got, c.want)
		}
	}
}
