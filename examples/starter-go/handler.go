// Copyright 2026. Licensed under the Apache License, Version 2.0.

package main

import "context"

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
func handleMessage(_ context.Context, env MessageEnvelope) (ResponseEnvelope, error) {
	return ResponseEnvelope{
		Content:  "starter-go received: " + env.Content,
		Metadata: map[string]any{},
	}, nil
}
