// Copyright 2026. Licensed under the Apache License, Version 2.0.

package main

import (
	"bytes"
	"context"
	"encoding/json"
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
// retries.
var staleRetrySchedule = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}

// errTaskAlreadyCompleted signals a terminal 403: the task is already in a
// terminal phase, so the caller should log and exit.
var errTaskAlreadyCompleted = fmt.Errorf("task already completed")

// completeTask reports completion for an AgentTask, retrying
// StalePodCompletion on the bounded schedule and treating TaskAlreadyCompleted
// as terminal. Only meaningful for agentReported AgentTasks; Agent images
// never call it.
func (a *agent) completeTask(ctx context.Context, status, message string, artifacts map[string]string) error {
	body, err := json.Marshal(completionRequest{Status: status, Message: message, Artifacts: artifacts})
	if err != nil {
		return err
	}

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
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			a.gatewayURL+"/v1/task/complete", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.gatewayCli.Do(req)
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
			return errTaskAlreadyCompleted
		default:
			return fmt.Errorf("task completion failed: %d %s", resp.StatusCode, respBody)
		}
	}
	return fmt.Errorf("task completion exhausted retries: %w", lastErr)
}
