// Copyright 2026. Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"fmt"
)

// handleMessage is the single developer-owned extension point. Replace its
// body with your agent logic. Everything else in this template (TLS, cert
// reload, dedup, mTLS verification, heartbeats, task completion) is contract
// boilerplate you should not need to touch.
//
// The gateway has already authenticated the caller and deduplicated retries
// before this is invoked. Return a ResponseEnvelope whose Content is the
// agent's reply; the gateway relays it back to the webhook caller (sync mode)
// or to the callbackUrl/polling endpoint (async mode).
//
// To make LLM calls, POST to a.gatewayURL using the mTLS client the template
// pre-configured (a.gatewayCli): the gateway proxies to your ModelProviders.
func handleMessage(_ context.Context, env MessageEnvelope, memory *store) (ResponseEnvelope, error) {
	// Count this caller's messages in the mounted volume. It is the smallest
	// honest demonstration that state outlives the Pod: hibernate this agent
	// and message it again, and the count continues rather than restarting.
	// Your own agent keeps conversation history here instead.
	count := memory.note(env.UserID)

	content := "starter-go received: " + env.Content
	if count > 1 {
		content += fmt.Sprintf(" (message %d from you)", count)
	}

	// Reflect the caller identity and derived session id the gateway supplied,
	// so a session-aware client can correlate replies. sessionId is present only
	// when the AgentChannel enables session identity.
	return ResponseEnvelope{
		Content: content,
		Metadata: map[string]any{
			"userId":    env.UserID,
			"sessionId": env.SessionID,
		},
	}, nil
}
