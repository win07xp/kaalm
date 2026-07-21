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
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func mustIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad test IP %q", s)
	}
	return ip
}

func TestAdapterFormatNames(t *testing.T) {
	if (anthropicAdapter{}).formatName() != providerTypeAnthropic {
		t.Error("anthropic formatName wrong")
	}
	if (openaiAdapter{}).formatName() != "openai" {
		t.Error("openai formatName wrong")
	}
	if (vertexAdapter{}).formatName() != providerTypeVertex {
		t.Error("vertex formatName wrong")
	}
}

func TestAnthropicFixupIsNoOp(t *testing.T) {
	body := map[string]any{"stream": true}
	anthropicAdapter{}.fixupRequestBody(body)
	if _, ok := body["stream_options"]; ok {
		t.Error("anthropic must not inject stream_options")
	}
	// Vertex fixup is also a no-op.
	vBody := map[string]any{"stream": true}
	vertexAdapter{}.fixupRequestBody(vBody)
	if len(vBody) != 1 {
		t.Error("vertex fixup must not mutate the body")
	}
}

func TestOpenAIFixup(t *testing.T) {
	// Non-streaming: untouched.
	nonStream := map[string]any{"stream": false}
	openaiAdapter{}.fixupRequestBody(nonStream)
	if _, ok := nonStream["stream_options"]; ok {
		t.Error("non-streaming request must not get stream_options")
	}
	// Streaming without stream_options: injected.
	stream := map[string]any{"stream": true}
	openaiAdapter{}.fixupRequestBody(stream)
	opts, ok := stream["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("stream_options.include_usage not injected: %v", stream["stream_options"])
	}
	// Streaming with an existing stream_options: preserved.
	pre := map[string]any{"stream": true, "stream_options": map[string]any{"foo": "bar"}}
	openaiAdapter{}.fixupRequestBody(pre)
	if got := pre["stream_options"].(map[string]any); got["foo"] != "bar" {
		t.Error("existing stream_options must be preserved")
	}
}

func TestAdapterForProviderType(t *testing.T) {
	cases := map[string]string{
		providerTypeAnthropic:        providerTypeAnthropic,
		providerTypeOpenAI:           "openai",
		providerTypeOpenAICompatible: "openai",
		providerTypeVertex:           providerTypeVertex,
	}
	for ptype, want := range cases {
		a, ok := adapterForProviderType(ptype)
		if !ok || a.formatName() != want {
			t.Errorf("adapterForProviderType(%q) = %v ok=%v", ptype, a, ok)
		}
	}
	if _, ok := adapterForProviderType("mystery"); ok {
		t.Error("unknown provider type must not resolve")
	}
}

func TestUTF8ErrorOffset(t *testing.T) {
	if off := utf8ErrorOffset([]byte("hello")); off != -1 {
		t.Errorf("valid UTF-8 must return -1, got %d", off)
	}
	// 0xff is never a valid UTF-8 byte; here at offset 3.
	if off := utf8ErrorOffset([]byte{'a', 'b', 'c', 0xff, 'd'}); off != 3 {
		t.Errorf("bad byte offset = %d, want 3", off)
	}
}

func TestBodyLogIsNoOp(t *testing.T) {
	// Default build: bodyLog must not panic and logs nothing.
	bodyLog("prompt", []byte("secret content"))
	if DebugBodyLogging {
		t.Error("default build must have body logging disabled")
	}
}

func TestParseCompletionData(t *testing.T) {
	data := map[string]string{
		CompletionKeyStatus:                 CompletionStatusSuccess,
		CompletionKeyMessage:                "done",
		CompletionArtifactPrefix + "pr-url": "https://x/1",
		CompletionArtifactPrefix + "log":    "tail",
		"unrelated":                         "ignored",
	}
	status, message, artifacts := ParseCompletionData(data)
	if status != CompletionStatusSuccess || message != "done" {
		t.Errorf("status/message wrong: %q %q", status, message)
	}
	if artifacts["pr-url"] != "https://x/1" || artifacts["log"] != "tail" {
		t.Errorf("artifacts wrong: %v", artifacts)
	}
	if _, ok := artifacts["unrelated"]; ok {
		t.Error("non-artifact keys must be dropped")
	}
	if _, ok := artifacts["status"]; ok {
		t.Error("status key must not appear as artifact")
	}
}

func TestCompletionMailboxName(t *testing.T) {
	if got := CompletionMailboxName("fix-42"); got != "fix-42-completion" {
		t.Errorf("CompletionMailboxName = %q", got)
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

func TestBlockedCallbackIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "169.254.169.254", "::1", "fc00::1", "0.0.0.0", "fe80::1"}
	for _, ip := range blocked {
		if !blockedCallbackIP(mustIP(t, ip)) {
			t.Errorf("%s must be blocked", ip)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, ip := range allowed {
		if blockedCallbackIP(mustIP(t, ip)) {
			t.Errorf("%s must be allowed", ip)
		}
	}
}
