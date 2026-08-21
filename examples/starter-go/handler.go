// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/win07xp/kaalm/agentruntime"
)

// handler is the developer-owned part; everything else (TLS rotation, dedup,
// mTLS verification, heartbeats, task completion) lives in the runtime
// module. Replace the returned function's body with your agent logic.
//
// Capabilities come from closing over the Agent: a.Memory for state that
// survives hibernation, a.Gateway for LLM calls through the gateway (POST a
// qualified model request to /v1/chat/completions; the gateway proxies to
// your ModelProviders).
func handler(a *agentruntime.Agent) agentruntime.Handler {
	// With KAALM_STARTER_MODEL set (a qualified "{providerRef}/{modelId}"
	// name), a message starting "ask " is answered by that model through
	// the gateway instead of echoed; see askModel below.
	model := os.Getenv("KAALM_STARTER_MODEL")
	return func(ctx context.Context, env agentruntime.Envelope) (agentruntime.Response, error) {
		// Count this caller's messages in persistent memory. It is the
		// smallest honest demonstration that state outlives the Pod:
		// hibernate this agent and message it again, and the count continues
		// rather than restarting. Your own agent keeps conversation history
		// here instead.
		var count int
		if _, err := a.Memory.Get("seen/"+env.UserID, &count); err != nil {
			return agentruntime.Response{}, err
		}
		count++
		if err := a.Memory.Set("seen/"+env.UserID, count); err != nil {
			return agentruntime.Response{}, err
		}
		if prompt, ok := strings.CutPrefix(env.Content, "ask "); ok && model != "" {
			answer, err := askModel(ctx, a.Gateway, model, prompt)
			if err != nil {
				return agentruntime.Response{}, err
			}
			return agentruntime.Response{
				Content:  "starter-go asked " + model + ": " + answer,
				Metadata: map[string]any{"userId": env.UserID, "sessionId": env.SessionID},
			}, nil
		}
		return reply(env, count), nil
	}
}

// gatewayPoster is the slice of the runtime's Gateway client askModel needs;
// the indirection keeps askModel testable against a plain HTTP fake.
type gatewayPoster interface {
	Post(ctx context.Context, path string, body any) (*http.Response, error)
}

// askModel sends one chat completion through the gateway. Passing the
// handler's ctx is what keeps the call on the message's trace: the runtime
// forwards the delivery's trace context on every gateway call made with it
// (runtime contract item 8).
func askModel(ctx context.Context, g gatewayPoster, model, prompt string) (string, error) {
	resp, err := g.Post(ctx, "/v1/chat/completions", map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway returned %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("reply carried no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// reply formats the starter's answer: the delivered content, the running
// per-user count once it is past the first message, and the caller identity
// the gateway supplied, so a session-aware client can correlate replies.
// sessionId is present only when the AgentChannel enables session identity.
func reply(env agentruntime.Envelope, count int) agentruntime.Response {
	content := "starter-go received: " + env.Content
	if count > 1 {
		content += fmt.Sprintf(" (message %d from you)", count)
	}
	return agentruntime.Response{
		Content: content,
		Metadata: map[string]any{
			"userId":    env.UserID,
			"sessionId": env.SessionID,
		},
	}
}
