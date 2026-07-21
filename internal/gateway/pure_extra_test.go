/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func TestExtractUsage_MalformedAndZero(t *testing.T) {
	adapters := []providerAdapter{anthropicAdapter{}, openaiAdapter{}, vertexAdapter{}}
	for _, a := range adapters {
		if _, ok := a.extractUsage([]byte("not json")); ok {
			t.Errorf("%s: malformed body must not yield usage", a.formatName())
		}
		if _, ok := a.extractUsage([]byte(`{}`)); ok {
			t.Errorf("%s: zero usage must not yield usage", a.formatName())
		}
	}
	// OpenAI-shaped zero usage.
	if _, ok := (openaiAdapter{}).extractUsage([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`)); ok {
		t.Error("openai zero usage must be false")
	}
}

func TestAccumulateStreamUsage_Malformed(t *testing.T) {
	var u Usage
	// Malformed data is ignored for every adapter.
	anthropicAdapter{}.accumulateStreamUsage([]byte("nope"), &u)
	openaiAdapter{}.accumulateStreamUsage([]byte("nope"), &u)
	openaiAdapter{}.accumulateStreamUsage([]byte("[DONE]"), &u)
	vertexAdapter{}.accumulateStreamUsage([]byte("nope"), &u)
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("malformed stream data must not accumulate: %+v", u)
	}
	// Anthropic message_stop carries no usage.
	anthropicAdapter{}.accumulateStreamUsage([]byte(`{"type":"message_stop"}`), &u)
	if u.InputTokens != 0 {
		t.Error("message_stop must not change usage")
	}
}

func TestVerifyHMACHeader(t *testing.T) {
	secret := []byte("k")
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("body"))
	expected := mac.Sum(nil)

	// Empty header.
	if verifyHMACHeader("", &agentryv1alpha1.ChannelHMAC{}, expected) {
		t.Error("empty header must fail")
	}
	// Valid hex (default encoding).
	if !verifyHMACHeader(hex.EncodeToString(expected), &agentryv1alpha1.ChannelHMAC{}, expected) {
		t.Error("valid hex must pass")
	}
	// Valid base64.
	if !verifyHMACHeader(base64.StdEncoding.EncodeToString(expected),
		&agentryv1alpha1.ChannelHMAC{Encoding: "base64"}, expected) {
		t.Error("valid base64 must pass")
	}
	// Prefix stripped then matched.
	prefix := "sha256="
	if !verifyHMACHeader("sha256="+hex.EncodeToString(expected),
		&agentryv1alpha1.ChannelHMAC{SignaturePrefix: &prefix}, expected) {
		t.Error("prefixed hex must pass after strip")
	}
	// Prefix expected but absent.
	if verifyHMACHeader(hex.EncodeToString(expected),
		&agentryv1alpha1.ChannelHMAC{SignaturePrefix: &prefix}, expected) {
		t.Error("missing required prefix must fail")
	}
	// Undecodable hex.
	if verifyHMACHeader("zz-not-hex", &agentryv1alpha1.ChannelHMAC{}, expected) {
		t.Error("bad hex must fail")
	}
	// Undecodable base64.
	if verifyHMACHeader("!!!!", &agentryv1alpha1.ChannelHMAC{Encoding: "base64"}, expected) {
		t.Error("bad base64 must fail")
	}
	// Well-formed but wrong digest.
	if verifyHMACHeader(hex.EncodeToString([]byte("wrongwrongwrongwrong")), &agentryv1alpha1.ChannelHMAC{}, expected) {
		t.Error("wrong digest must fail")
	}
}

func TestDottedLookup(t *testing.T) {
	body := map[string]any{
		"user":  map[string]any{"id": "u-1"},
		"count": float64(42),
		"ok":    true,
		"nested": map[string]any{
			"deep": map[string]any{"leaf": "found"},
		},
		"notmap": "scalar",
	}
	if got := dottedLookup(body, "user.id"); got != "u-1" {
		t.Errorf("string lookup = %q", got)
	}
	if got := dottedLookup(body, "count"); got != "42" {
		t.Errorf("float lookup = %q", got)
	}
	if got := dottedLookup(body, "ok"); got != "true" {
		t.Errorf("bool lookup = %q", got)
	}
	if got := dottedLookup(body, "nested.deep.leaf"); got != "found" {
		t.Errorf("deep lookup = %q", got)
	}
	// Missing intermediate.
	if got := dottedLookup(body, "user.absent"); got != "" {
		t.Errorf("missing leaf = %q", got)
	}
	// Intermediate is not a map.
	if got := dottedLookup(body, "notmap.x"); got != "" {
		t.Errorf("non-map intermediate = %q", got)
	}
	// Leaf is a map/other type: unsupported, empty string.
	if got := dottedLookup(body, "user"); got != "" {
		t.Errorf("map leaf must be empty = %q", got)
	}
}

func TestExtract(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-User", "hdr-user")
	fallback := "anon"

	// FromHeader.
	hdrName := "X-User"
	if got := extract(agentryv1alpha1.ChannelExtractor{FromHeader: &hdrName}, r, nil); got != "hdr-user" {
		t.Errorf("header extract = %q", got)
	}
	// FromHeader missing -> fallback.
	missing := "X-Absent"
	if got := extract(agentryv1alpha1.ChannelExtractor{FromHeader: &missing, Fallback: &fallback}, r, nil); got != "anon" {
		t.Errorf("fallback = %q", got)
	}
	// FromBody.
	path := "user.id"
	body := map[string]any{"user": map[string]any{"id": "b-user"}}
	if got := extract(agentryv1alpha1.ChannelExtractor{FromBody: &path}, r, body); got != "b-user" {
		t.Errorf("body extract = %q", got)
	}
	// No extractor and no fallback -> empty.
	if got := extract(agentryv1alpha1.ChannelExtractor{}, r, nil); got != "" {
		t.Errorf("empty extractor = %q", got)
	}
}

func TestJWTExpiry(t *testing.T) {
	// Not three parts.
	if _, ok := jwtExpiry("abc"); ok {
		t.Error("non-JWT must not parse")
	}
	// Bad base64 payload.
	if _, ok := jwtExpiry("a.$$$.c"); ok {
		t.Error("bad base64 payload must not parse")
	}
	// Valid payload with exp.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1700000000}`))
	exp, ok := jwtExpiry("h." + payload + ".s")
	if !ok || !exp.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("valid exp = %v ok=%v", exp, ok)
	}
	// Payload with no exp claim.
	noExp := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))
	if _, ok := jwtExpiry("h." + noExp + ".s"); ok {
		t.Error("missing exp must not parse")
	}
}

func TestParseBudgetPartial_BadFloat(t *testing.T) {
	// A non-numeric namespace value is skipped, not fatal.
	period, spend, err := ParseBudgetPartial(`{"period":"2099-01","team-a":"1.5","team-b":"notanumber"}`)
	if err != nil || period != "2099-01" {
		t.Fatalf("parse err=%v period=%q", err, period)
	}
	if spend["team-a"] != 1.5 {
		t.Errorf("team-a = %v", spend["team-a"])
	}
	if _, ok := spend["team-b"]; ok {
		t.Error("unparseable value must be skipped")
	}
	// Invalid JSON errors.
	if _, _, err := ParseBudgetPartial("not json"); err == nil {
		t.Error("invalid JSON must error")
	}
}
