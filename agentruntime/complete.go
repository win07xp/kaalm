// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// completionRequest is the POST /v1/task/complete body (contract item 6).
type completionRequest struct {
	Status    string            `json:"status"`
	Message   string            `json:"message,omitempty"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// staleRetrySchedule is the bounded backoff for StalePodCompletion, which
// covers the brief reconciler lag between Pod creation and currentPodUID being
// stamped. Distinct from (and much tighter than) the gateway's delivery
// retries. A package variable so tests can compress it.
var staleRetrySchedule = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}

// ErrTaskAlreadyCompleted signals a terminal 403: the task is already in a
// terminal phase, so the caller should log and exit rather than retry.
var ErrTaskAlreadyCompleted = errors.New("task already completed")

// CompleteTask reports completion for an AgentTask (contract item 6),
// retrying StalePodCompletion on a bounded schedule and returning
// ErrTaskAlreadyCompleted on the terminal 403. Only meaningful in task mode;
// resident Agents never call it.
func (a *Agent) CompleteTask(ctx context.Context, status, message string, artifacts map[string]string) error {
	body := completionRequest{Status: status, Message: message, Artifacts: artifacts}

	attempts := append([]time.Duration{0}, staleRetrySchedule...)
	var lastErr error
	for _, delay := range attempts {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		resp, err := a.Gateway.Post(ctx, "/v1/task/complete", body)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode == http.StatusForbidden && strings.Contains(string(respBody), "StalePodCompletion"):
			lastErr = fmt.Errorf("stale pod completion; retrying")
			continue
		case resp.StatusCode == http.StatusForbidden && strings.Contains(string(respBody), "TaskAlreadyCompleted"):
			return ErrTaskAlreadyCompleted
		default:
			return fmt.Errorf("task completion failed: %d %s", resp.StatusCode, respBody)
		}
	}
	return fmt.Errorf("task completion exhausted retries: %w", lastErr)
}
