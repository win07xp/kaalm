// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
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

type fakePoster struct {
	status int
	body   string
	seen   map[string]any
}

func (f *fakePoster) Post(_ context.Context, _ string, body any) (*http.Response, error) {
	f.seen, _ = body.(map[string]any)
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

func TestAskModel(t *testing.T) {
	f := &fakePoster{status: 200, body: `{"choices":[{"message":{"role":"assistant","content":"four"}}]}`}
	answer, err := askModel(context.Background(), f, "prov/m1", "2+2?")
	if err != nil || answer != "four" {
		t.Fatalf("askModel = %q, %v", answer, err)
	}
	if f.seen["model"] != "prov/m1" {
		t.Errorf("model sent = %v", f.seen["model"])
	}

	f = &fakePoster{status: 429, body: `{}`}
	if _, err := askModel(context.Background(), f, "prov/m1", "x"); err == nil {
		t.Error("a non-200 gateway reply must surface as an error")
	}

	f = &fakePoster{status: 200, body: `{"choices":[]}`}
	if _, err := askModel(context.Background(), f, "prov/m1", "x"); err == nil {
		t.Error("an empty choices list must surface as an error")
	}
}
