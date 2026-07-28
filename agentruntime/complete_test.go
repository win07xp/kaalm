// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func fastStaleRetries(t *testing.T) {
	t.Helper()
	restore := staleRetrySchedule
	staleRetrySchedule = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	t.Cleanup(func() { staleRetrySchedule = restore })
}

func completionAgent(t *testing.T, pki *testPKI, gatewayURL string) *Agent {
	t.Helper()
	return &Agent{Gateway: newGateway(gatewayURL, testReloader(t, pki)), isTask: true}
}

// StalePodCompletion covers reconciler lag and must be retried on the
// bounded schedule; the first clean 200 ends the attempt loop.
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
			http.Error(w, `{"error":{"type":"StalePodCompletion"}}`, http.StatusForbidden)
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
