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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

const sha1Size = 20

func TestChannelSecret_Branches(t *testing.T) {
	s := &Server{Store: newFakeStore()}
	fs := s.Store.(*fakeStore)
	fs.secrets["team-a/bearer-secret/token"] = "bt"
	fs.secrets["team-a/hmac-secret/key"] = "hk"
	ctx := context.Background()

	// Bearer with secretRef resolves.
	bearer := &agentryv1alpha1.ChannelAuth{Type: authTypeBearer, SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "bearer-secret", Key: "token"}}
	if v, err := s.channelSecret(ctx, "team-a", bearer); err != nil || v != "bt" {
		t.Errorf("bearer secret = %q err=%v", v, err)
	}
	// Bearer without secretRef errors.
	if _, err := s.channelSecret(ctx, "team-a", &agentryv1alpha1.ChannelAuth{Type: authTypeBearer}); err == nil {
		t.Error("bearer without secretRef must error")
	}
	// HMAC with block resolves.
	hm := &agentryv1alpha1.ChannelAuth{Type: authTypeHMAC, HMAC: &agentryv1alpha1.ChannelHMAC{
		SecretRef: agentryv1alpha1.SecretKeyReference{Name: "hmac-secret", Key: "key"}}}
	if v, err := s.channelSecret(ctx, "team-a", hm); err != nil || v != "hk" {
		t.Errorf("hmac secret = %q err=%v", v, err)
	}
	// HMAC without block errors.
	if _, err := s.channelSecret(ctx, "team-a", &agentryv1alpha1.ChannelAuth{Type: authTypeHMAC}); err == nil {
		t.Error("hmac without block must error")
	}
	// Unknown type errors.
	if _, err := s.channelSecret(ctx, "team-a", &agentryv1alpha1.ChannelAuth{Type: "mystery"}); err == nil {
		t.Error("unknown auth type must error")
	}
}

func TestHmacHasher(t *testing.T) {
	if hmacHasher("sha1")().Size() != sha1Size {
		t.Error("sha1 hasher size wrong")
	}
	if hmacHasher("sha256")().Size() != sha256.Size {
		t.Error("sha256 hasher size wrong")
	}
	if hmacHasher("")().Size() != sha256.Size {
		t.Error("default hasher must be sha256")
	}
}

func TestAuthenticatePoll_HMAC(t *testing.T) {
	s := &Server{Store: newFakeStore()}
	fs := s.Store.(*fakeStore)
	fs.secrets["team-a/hmac-secret/key"] = "topsecret"

	channel := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "team-a"},
		Spec: agentryv1alpha1.AgentChannelSpec{Webhook: agentryv1alpha1.AgentChannelWebhook{
			Auth: agentryv1alpha1.ChannelAuth{Type: authTypeHMAC, HMAC: &agentryv1alpha1.ChannelHMAC{
				Header: "X-Sig", Algorithm: "sha256",
				SecretRef: agentryv1alpha1.SecretKeyReference{Name: "hmac-secret", Key: "key"}}},
		}},
	}
	ctx := context.Background()
	requestID := "req-9"
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)

	sign := func(id, timestamp string) string {
		mac := hmac.New(sha256.New, []byte("topsecret"))
		_, _ = fmt.Fprintf(mac, "%s\n%s", id, timestamp)
		return hex.EncodeToString(mac.Sum(nil))
	}

	// Valid signature within skew.
	good, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	good.Header.Set("X-Sig", sign(requestID, ts))
	good.Header.Set(timestampHeader, ts)
	if !s.authenticatePoll(ctx, channel, good, requestID) {
		t.Error("valid HMAC poll must pass")
	}

	// Missing/invalid timestamp.
	noTS, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	noTS.Header.Set("X-Sig", sign(requestID, ts))
	if s.authenticatePoll(ctx, channel, noTS, requestID) {
		t.Error("missing timestamp must fail")
	}

	// Timestamp outside the skew window.
	oldTS := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
	skewed, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	skewed.Header.Set("X-Sig", sign(requestID, oldTS))
	skewed.Header.Set(timestampHeader, oldTS)
	if s.authenticatePoll(ctx, channel, skewed, requestID) {
		t.Error("stale timestamp must fail")
	}

	// Wrong signature.
	wrongSig, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	wrongSig.Header.Set("X-Sig", sign("other-id", ts))
	wrongSig.Header.Set(timestampHeader, ts)
	if s.authenticatePoll(ctx, channel, wrongSig, requestID) {
		t.Error("signature over the wrong requestId must fail")
	}

	// Non-hex signature.
	badHex, _ := http.NewRequest(http.MethodGet, "/poll", nil)
	badHex.Header.Set("X-Sig", "not-hex-zz")
	badHex.Header.Set(timestampHeader, ts)
	if s.authenticatePoll(ctx, channel, badHex, requestID) {
		t.Error("non-hex signature must fail")
	}

	// Secret cannot be resolved: fail closed.
	fs2 := newFakeStore()
	s2 := &Server{Store: fs2}
	if s2.authenticatePoll(ctx, channel, good, requestID) {
		t.Error("unresolvable secret must fail")
	}
}

func TestSignCallback_Bearer(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/cb", nil)
	auth := &agentryv1alpha1.ChannelAuth{Type: authTypeBearer}
	signCallback(req, auth, "s3cr3t", "req-1", []byte("body"), time.Now())
	if got := req.Header.Get("Authorization"); got != "Bearer s3cr3t" {
		t.Errorf("bearer callback header = %q", got)
	}
}

func TestSignCallback_HMAC(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/cb", nil)
	auth := &agentryv1alpha1.ChannelAuth{
		Type: authTypeHMAC,
		HMAC: &agentryv1alpha1.ChannelHMAC{Header: "X-Sig", Algorithm: "sha256"},
	}
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"response":"ok"}`)
	signCallback(req, auth, "topsecret", "req-9", body, now)

	ts := req.Header.Get(timestampHeader)
	if ts != strconv.FormatInt(now.Unix(), 10) {
		t.Errorf("timestamp header = %q", ts)
	}
	sig := req.Header.Get("X-Sig")
	if sig == "" {
		t.Fatal("signature header missing")
	}
	if _, err := hex.DecodeString(sig); err != nil {
		t.Errorf("signature is not hex: %v", err)
	}
	// Signing again with the same inputs is deterministic.
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/cb", nil)
	signCallback(req2, auth, "topsecret", "req-9", body, now)
	if req2.Header.Get("X-Sig") != sig {
		t.Error("HMAC signature must be deterministic for fixed inputs")
	}
	// A different body changes the signature.
	req3, _ := http.NewRequest(http.MethodPost, "https://example.com/cb", nil)
	signCallback(req3, auth, "topsecret", "req-9", []byte("other"), now)
	if req3.Header.Get("X-Sig") == sig {
		t.Error("HMAC signature must depend on the body")
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
