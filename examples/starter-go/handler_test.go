// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package main

import (
	"testing"

	"github.com/win07xp/kaalm/agentruntime"
)

// The reply format is asserted by the e2e suite (a reply must contain the
// delivered content, and "starter-go received" identifies this template) and
// quoted verbatim in the learn book, so changes here ripple; keep it stable.
func TestReply(t *testing.T) {
	env := agentruntime.Envelope{UserID: "u1", SessionID: "s1", Content: "ping"}

	first := reply(env, 1)
	if first.Content != "starter-go received: ping" {
		t.Errorf("first reply = %q", first.Content)
	}
	if first.Metadata["userId"] != "u1" || first.Metadata["sessionId"] != "s1" {
		t.Errorf("caller identity must be reflected: %+v", first.Metadata)
	}

	second := reply(env, 2)
	if second.Content != "starter-go received: ping (message 2 from you)" {
		t.Errorf("counted reply = %q", second.Content)
	}
}
