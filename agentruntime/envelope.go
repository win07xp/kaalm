// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"encoding/json"
)

// Envelope is the inbound message shape the gateway delivers to
// POST /v1/message (contract item 4).
type Envelope struct {
	MessageID   string            `json:"messageId"`
	ChannelType string            `json:"channelType"`
	ChannelID   string            `json:"channelId"`
	UserID      string            `json:"userId"`
	SessionID   string            `json:"sessionId,omitempty"`
	Content     string            `json:"content"`
	Attachments []json.RawMessage `json:"attachments"`
	Metadata    map[string]any    `json:"metadata"`
}

// Response is the reply shape returned to the gateway; Content is required.
type Response struct {
	Content     string            `json:"content"`
	Attachments []json.RawMessage `json:"attachments"`
	Metadata    map[string]any    `json:"metadata"`
}

// Handler is the developer-owned extension point: one delivered Envelope in,
// one Response out. The runtime has already authenticated the caller as the
// gateway and deduplicated redeliveries before a Handler runs, and it caches
// the returned Response against the messageId afterward. A returned error
// becomes a 500 to the gateway, which retries delivery on its own schedule.
type Handler func(ctx context.Context, env Envelope) (Response, error)

// echoHandler is the built-in default (docs/src/runtime/base-images.md): it
// replies with the received text prefixed by "echo: ", makes no LLM calls so
// it works on a provider-less Agent, and is deterministic so automated tests
// can tell it apart from a user-supplied handler.
func echoHandler(_ context.Context, env Envelope) (Response, error) {
	return Response{Content: "echo: " + env.Content}, nil
}
