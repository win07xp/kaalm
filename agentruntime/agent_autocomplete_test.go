// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func fastAutocompleteRetries(t *testing.T) {
	t.Helper()
	restore := autocompleteRetryDelay
	autocompleteRetryDelay = time.Millisecond
	t.Cleanup(func() { autocompleteRetryDelay = restore })
}

// autocompleteAttempts runs the KAALM_TASK_AUTOCOMPLETE hook against a
// gateway that always answers with the given status and body, and returns
// how many completions it sent.
func autocompleteAttempts(t *testing.T, status int, body string) int32 {
	t.Helper()
	fastStaleRetries(t)
	fastAutocompleteRetries(t)
	pki := newTestPKI(t)
	var attempts atomic.Int32
	srv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, body, status)
	}))
	completionAgent(t, pki, srv.URL).autocomplete(context.Background(), "success")
	return attempts.Load()
}

// A task that is already terminal answers every later report the same way,
// so the hook stops at the first ErrTaskAlreadyCompleted.
func TestAutocomplete_StopsWhenTheTaskAlreadyCompleted(t *testing.T) {
	got := autocompleteAttempts(t, http.StatusForbidden,
		`{"error":{"type":"access_denied","message":"TaskAlreadyCompleted: the task has reached a terminal phase"}}`)
	if got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

// An exitCode task has no completion mailbox: the gateway answers 403
// TaskNotAgentReported to every report, so the hook stops at the first one.
func TestAutocomplete_StopsForAnExitCodeTask(t *testing.T) {
	got := autocompleteAttempts(t, http.StatusForbidden,
		`{"error":{"type":"access_denied","message":"TaskNotAgentReported: this task completes via container exit"}}`)
	if got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

// A retryable failure keeps the hook going for its 6 attempts.
func TestAutocomplete_RetriesARetryableFailure(t *testing.T) {
	got := autocompleteAttempts(t, http.StatusServiceUnavailable,
		`{"error":{"type":"internal_unavailable","retryable":true}}`)
	if got != 6 {
		t.Errorf("attempts = %d, want 6", got)
	}
}
