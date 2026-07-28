// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"fmt"

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
	return func(_ context.Context, env agentruntime.Envelope) (agentruntime.Response, error) {
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
		return reply(env, count), nil
	}
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
