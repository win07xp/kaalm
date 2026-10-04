// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// completionRequest is the POST /v1/task/complete body (contract item 6).
type completionRequest struct {
	Status    string            `json:"status"`
	Message   string            `json:"message,omitempty"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// staleRetrySchedule is the bounded backoff, after one immediate attempt, for
// the 409 stale_pod rejection (message prefix StalePodCompletion), which
// covers the brief reconciler lag between Pod creation and currentPodUID being
// set, for the 503 internal_unavailable answer (the gateway could not record
// the report; the wait after it is raised to at least Retry-After), and for
// transport errors. Distinct from (and much tighter than) the gateway's
// delivery retries. A package variable so tests can compress it.
var staleRetrySchedule = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}

// retryAfterCap bounds the Retry-After floor after a 503 internal_unavailable,
// so a wrong or proxy-set value cannot stall the report. A package variable so
// tests can compress it.
var retryAfterCap = 30 * time.Second

// defaultUnavailableWait is the floor when a 503 internal_unavailable carries
// no usable Retry-After: the 1-second minimum the gateway's 503 asks for.
const defaultUnavailableWait = time.Second

// unavailableWait returns the minimum wait after a 503 internal_unavailable:
// the Retry-After header as integer delta-seconds, or defaultUnavailableWait
// when it is missing or not a non-negative integer (an HTTP-date, a sign),
// never more than limit.
func unavailableWait(header string, limit time.Duration) time.Duration {
	wait := defaultUnavailableWait
	if n, err := strconv.ParseUint(strings.TrimSpace(header), 10, 32); err == nil {
		wait = time.Duration(n) * time.Second
	}
	return min(wait, limit)
}

// ErrTaskAlreadyCompleted signals a terminal 403: the task is already in a
// terminal phase, so the caller should log and exit rather than retry.
var ErrTaskAlreadyCompleted = errors.New("task already completed")

// completionRejected is the error CompleteTask returns for any other answer
// it does not retry: the status code and the (capped) body the gateway sent.
type completionRejected struct {
	status int
	body   string
}

func (e *completionRejected) Error() string {
	return fmt.Sprintf("task completion failed: %d %s", e.status, e.body)
}

// isTaskNotAgentReported reports whether err is the gateway's 403
// TaskNotAgentReported: the task completes via container exit (exitCode), so
// it has no completion mailbox and every report gets the same answer.
func isTaskNotAgentReported(err error) bool {
	var rejected *completionRejected
	return errors.As(err, &rejected) &&
		rejected.status == http.StatusForbidden &&
		strings.Contains(rejected.body, "TaskNotAgentReported")
}

// CompleteTask reports completion for an AgentTask (contract item 6),
// retrying the 409 stale_pod rejection, the 503 internal_unavailable answer,
// and transport errors on a bounded schedule (four attempts in all) and
// returning ErrTaskAlreadyCompleted on the terminal 403. The wait after a 503
// is at least its Retry-After (1s when missing or unparseable, at most
// retryAfterCap). Only meaningful in task mode; resident Agents never call it.
func (a *Agent) CompleteTask(ctx context.Context, status, message string, artifacts map[string]string) error {
	body := completionRequest{Status: status, Message: message, Artifacts: artifacts}

	attempts := append([]time.Duration{0}, staleRetrySchedule...)
	var lastErr error
	var floor time.Duration // Retry-After floor from a 503 on the previous attempt
	for _, delay := range attempts {
		if delay < floor {
			delay = floor
		}
		floor = 0
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
		case resp.StatusCode == http.StatusConflict && strings.Contains(string(respBody), `"stale_pod"`):
			lastErr = fmt.Errorf("stale pod completion; retrying")
			continue
		case resp.StatusCode == http.StatusServiceUnavailable && strings.Contains(string(respBody), `"internal_unavailable"`):
			lastErr = &completionRejected{status: resp.StatusCode, body: string(respBody)}
			floor = unavailableWait(resp.Header.Get("Retry-After"), retryAfterCap)
			continue
		case resp.StatusCode == http.StatusForbidden && strings.Contains(string(respBody), "TaskAlreadyCompleted"):
			return ErrTaskAlreadyCompleted
		default:
			return &completionRejected{status: resp.StatusCode, body: string(respBody)}
		}
	}
	return fmt.Errorf("task completion exhausted retries: %w", lastErr)
}
