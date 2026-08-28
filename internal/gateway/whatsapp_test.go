/*
Copyright 2026 The Kaalm Authors.

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
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// graphFake stands in for the Graph API: it records every reply request and
// answers from a queue of canned responses (200 with a wamid by default).
type graphFake struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []capturedDiscordRequest
	queue    []graphAnswer
	got      chan capturedDiscordRequest
}

type graphAnswer struct {
	status int
	body   string
}

func newGraphFake(t *testing.T) *graphFake {
	t.Helper()
	f := &graphFake{got: make(chan capturedDiscordRequest, 16)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		req := capturedDiscordRequest{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		ans := graphAnswer{status: 200, body: `{"messages":[{"id":"wamid.reply"}]}`}
		if len(f.queue) > 0 {
			ans, f.queue = f.queue[0], f.queue[1:]
		}
		f.mu.Unlock()
		f.got <- req
		w.WriteHeader(ans.status)
		_, _ = w.Write([]byte(ans.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *graphFake) next(t *testing.T) capturedDiscordRequest {
	t.Helper()
	select {
	case r := <-f.got:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no reply request reached the Graph fake")
		return capturedDiscordRequest{}
	}
}

func (f *graphFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type whatsAppHarness struct {
	*userHarness
	fake    *graphFake
	channel *kaalmv1beta1.AgentChannel
}

const (
	waTestSecret = "app-secret-0001"
	waTestNumber = "106540352242922"
)

func newWhatsAppHarness(t *testing.T, agentFn http.HandlerFunc) *whatsAppHarness {
	t.Helper()
	h := &whatsAppHarness{userHarness: newUserHarness(t, agentFn), fake: newGraphFake(t)}
	h.server.Config.WhatsAppAPIBaseURL = h.fake.srv.URL
	h.channel = &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "wa", Namespace: "team-a"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "sup"},
			Type:     kaalmv1beta1.ChannelTypeWhatsApp,
			WhatsApp: &kaalmv1beta1.AgentChannelWhatsApp{
				Path:           "/channels/team-a/wa",
				CredentialsRef: kaalmv1beta1.LocalObjectReference{Name: "wa-creds"},
				PhoneNumberID:  waTestNumber,
			},
			Session: kaalmv1beta1.AgentChannelSession{Enabled: true},
		},
		Status: kaalmv1beta1.AgentChannelStatus{Phase: kaalmv1beta1.ChannelActive},
	}
	h.store.channels[h.channel.Spec.WhatsApp.Path] = h.channel
	h.store.secrets["team-a/wa-creds/verifyToken"] = "verify-me"
	h.store.secrets["team-a/wa-creds/appSecret"] = waTestSecret
	h.store.secrets["team-a/wa-creds/accessToken"] = "graph-token"
	h.store.agents["team-a/sup"] = &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Status:     kaalmv1beta1.AgentStatus{Phase: kaalmv1beta1.AgentRunning},
	}
	return h
}

func waSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// post delivers body signed with secret ("" sends no signature header).
func (h *whatsAppHarness) post(t *testing.T, body []byte, secret string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.userSrv.URL+h.channel.Spec.WhatsApp.Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set(whatsAppSignatureHeader, waSign(secret, body))
	}
	resp, err := h.userSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *whatsAppHarness) verify(t *testing.T, mode, token, challenge string) (int, string) {
	t.Helper()
	url := h.userSrv.URL + h.channel.Spec.WhatsApp.Path +
		"?hub.mode=" + mode + "&hub.verify_token=" + token + "&hub.challenge=" + challenge
	resp, err := h.userSrv.Client().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// waEvent builds one webhook event for number with the given messages and
// statuses (raw JSON objects).
func waEvent(number string, messages []string, statuses []string) []byte {
	msgs := "[" + strings.Join(messages, ",") + "]"
	sts := "[" + strings.Join(statuses, ",") + "]"
	return []byte(`{"object":"whatsapp_business_account","entry":[{"id":"102290129340398","changes":[{"field":"messages","value":{` +
		`"messaging_product":"whatsapp","metadata":{"display_phone_number":"15550001234","phone_number_id":"` + number + `"},` +
		`"contacts":[{"profile":{"name":"Dev"},"wa_id":"15551234567"}],"messages":` + msgs + `,"statuses":` + sts + `}}]}]}`)
}

func waText(id, body string) string {
	return `{"from":"15551234567","id":"` + id + `","timestamp":"1756100000","type":"text","text":{"body":"` + body + `"}}`
}

func TestWhatsApp_VerificationHandshake(t *testing.T) {
	h := newWhatsAppHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	status, body := h.verify(t, "subscribe", "verify-me", "1158201444")
	if status != 200 || body != "1158201444" {
		t.Errorf("handshake = %d %q, want 200 with the challenge echoed", status, body)
	}
	if _, ok := healthReason(h.userHarness, h.channel.Spec.WhatsApp.Path); ok {
		t.Error("a handshake must not be a health observation")
	}
	if status, _ := h.verify(t, "subscribe", "wrong", "x"); status != 403 {
		t.Errorf("wrong token = %d, want 403", status)
	}
	if reason, _ := healthReason(h.userHarness, h.channel.Spec.WhatsApp.Path); reason != healthReasonAuthFailed {
		t.Errorf("health after wrong token = %q", reason)
	}
	if status, _ := h.verify(t, "unsubscribe", "verify-me", "x"); status != 403 {
		t.Errorf("wrong mode = %d, want 403", status)
	}
}

func TestWhatsApp_SignaturePosture(t *testing.T) {
	h := newWhatsAppHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	body := waEvent(waTestNumber, []string{waText("wamid.1", "hi")}, nil)
	if resp := h.post(t, body, "other-secret"); resp.StatusCode != 401 {
		t.Errorf("wrong secret = %d, want 401", resp.StatusCode)
	}
	if reason, _ := healthReason(h.userHarness, h.channel.Spec.WhatsApp.Path); reason != healthReasonAuthFailed {
		t.Errorf("health after bad signature = %q", reason)
	}
	if resp := h.post(t, body, ""); resp.StatusCode != 401 {
		t.Errorf("unsigned = %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPut, h.userSrv.URL+h.channel.Spec.WhatsApp.Path, bytes.NewReader(body))
	if resp, _ := h.userSrv.Client().Do(req); resp.StatusCode != 401 {
		t.Errorf("PUT = %d, want 401", resp.StatusCode)
	}
	// Signed but not JSON: 400.
	if resp := h.post(t, []byte("not json"), waTestSecret); resp.StatusCode != 400 {
		t.Errorf("non-JSON = %d, want 400", resp.StatusCode)
	}
	select {
	case env := <-h.agentHits:
		t.Errorf("a rejected event reached the agent: %+v", env)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWhatsApp_TextEventIsAcknowledgedThenAnsweredThroughTheGraphAPI(t *testing.T) {
	h := newWhatsAppHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"Your order ships tomorrow.","attachments":[],"metadata":{}}`))
	})
	resp := h.post(t, waEvent(waTestNumber, []string{waText("wamid.1", "Where is my order?")}, nil), waTestSecret)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(raw) != 0 {
		t.Fatalf("event answered %d %q, want 200 with an empty body", resp.StatusCode, raw)
	}

	env := <-h.agentHits
	if env.ChannelType != "whatsapp" || env.ChannelID != "/channels/team-a/wa" || env.UserID != "15551234567" {
		t.Errorf("envelope identity wrong: %+v", env)
	}
	if env.Content != "Where is my order?" || env.SessionID != SessionID(env.ChannelID, env.UserID) {
		t.Errorf("content/session wrong: %+v", env)
	}
	if env.Metadata["messageId"] != "wamid.1" || env.Metadata["profileName"] != "Dev" ||
		env.Metadata["messageType"] != "text" || env.Metadata["phoneNumberId"] != waTestNumber {
		t.Errorf("metadata wrong: %+v", env.Metadata)
	}
	if _, ok := env.Metadata["message"].(map[string]any); !ok {
		t.Errorf("raw message missing from metadata: %+v", env.Metadata)
	}

	got := h.fake.next(t)
	if got.Method != http.MethodPost || got.Path != "/"+waTestNumber+"/messages" || got.Authorization != "Bearer graph-token" {
		t.Errorf("reply request = %s %s auth=%q", got.Method, got.Path, got.Authorization)
	}
	text, _ := got.Body["text"].(map[string]any)
	if got.Body["to"] != "15551234567" || got.Body["messaging_product"] != "whatsapp" || text["body"] != "Your order ships tomorrow." {
		t.Errorf("reply body wrong: %+v", got.Body)
	}
	if reason, ok := healthReason(h.userHarness, h.channel.Spec.WhatsApp.Path); !ok || reason != healthReasonWebhookReady {
		t.Errorf("health after a delivered reply = %q ok=%v", reason, ok)
	}
}

func TestWhatsApp_BatchesStatusesAndOtherNumbers(t *testing.T) {
	h := newWhatsAppHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	status := `{"id":"wamid.reply","status":"delivered","timestamp":"1756100001","recipient_id":"15551234567"}`
	body := waEvent(waTestNumber, []string{waText("wamid.a", "first"), waText("wamid.b", "second")}, []string{status})
	if resp := h.post(t, body, waTestSecret); resp.StatusCode != 200 {
		t.Fatalf("batch = %d", resp.StatusCode)
	}
	first, second := <-h.agentHits, <-h.agentHits
	seen := map[string]bool{first.Content: true, second.Content: true}
	if !seen["first"] || !seen["second"] {
		t.Errorf("batch envelopes = %q, %q", first.Content, second.Content)
	}
	h.fake.next(t)
	h.fake.next(t)

	// A status-only event and another number's message: 200, nothing delivered.
	if resp := h.post(t, waEvent(waTestNumber, nil, []string{status}), waTestSecret); resp.StatusCode != 200 {
		t.Errorf("status-only = %d", resp.StatusCode)
	}
	if resp := h.post(t, waEvent("999", []string{waText("wamid.c", "elsewhere")}, nil), waTestSecret); resp.StatusCode != 200 {
		t.Errorf("other number = %d", resp.StatusCode)
	}
	select {
	case env := <-h.agentHits:
		t.Errorf("a dropped event reached the agent: %+v", env)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestWhatsApp_ContentByMessageType(t *testing.T) {
	h := newWhatsAppHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	cases := []struct {
		name, raw, content string
		attachment         string
	}{
		{"button reply", `{"from":"15551234567","id":"wamid.i","timestamp":"1","type":"interactive",` +
			`"interactive":{"type":"button_reply","button_reply":{"id":"yes","title":"Yes please"}}}`, "Yes please", ""},
		{"image with caption", `{"from":"15551234567","id":"wamid.m","timestamp":"1","type":"image",` +
			`"image":{"id":"media-1","mime_type":"image/jpeg","sha256":"abc","caption":"receipt"}}`, "receipt", "whatsapp.image"},
		{"document without caption", `{"from":"15551234567","id":"wamid.d","timestamp":"1","type":"document",` +
			`"document":{"id":"media-2","mime_type":"application/pdf","sha256":"def"}}`, "", "whatsapp.document"},
		{"location", `{"from":"15551234567","id":"wamid.l","timestamp":"1","type":"location",` +
			`"location":{"latitude":1.5,"longitude":2.5}}`, "", ""},
	}
	for _, c := range cases {
		h.post(t, waEvent(waTestNumber, []string{c.raw}, nil), waTestSecret)
		env := <-h.agentHits
		if env.Content != c.content {
			t.Errorf("%s: content = %q, want %q", c.name, env.Content, c.content)
		}
		if c.attachment == "" && len(env.Attachments) != 0 {
			t.Errorf("%s: unexpected attachments %s", c.name, env.Attachments)
		}
		if c.attachment != "" && (len(env.Attachments) != 1 || !strings.Contains(string(env.Attachments[0]), c.attachment)) {
			t.Errorf("%s: attachments = %s", c.name, env.Attachments)
		}
		if _, ok := env.Metadata["message"].(map[string]any); !ok {
			t.Errorf("%s: raw message missing", c.name)
		}
		h.fake.next(t)
	}
}

func TestWhatsApp_LongReplyChunksInOrder(t *testing.T) {
	long := strings.Repeat("a", 4090) + "\n" + strings.Repeat("b", 100)
	reply, _ := json.Marshal(map[string]any{"content": long})
	h := newWhatsAppHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	})
	h.post(t, waEvent(waTestNumber, []string{waText("wamid.1", "hi")}, nil), waTestSecret)
	<-h.agentHits
	first, second := h.fake.next(t), h.fake.next(t)
	t1, _ := first.Body["text"].(map[string]any)
	t2, _ := second.Body["text"].(map[string]any)
	b1, _ := t1["body"].(string)
	b2, _ := t2["body"].(string)
	if !strings.HasSuffix(b1, "\n") || !strings.HasPrefix(b2, "bbbb") {
		t.Errorf("chunks wrong: %d/%d", len(b1), len(b2))
	}
}

func TestWhatsApp_RateLimitRetriedAndWindowTerminal(t *testing.T) {
	h := newWhatsAppHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"x"}`))
	})
	// 130429 inside a 400: retried, then delivered.
	h.fake.mu.Lock()
	h.fake.queue = []graphAnswer{{400, `{"error":{"message":"Rate limit hit","code":130429}}`}}
	h.fake.mu.Unlock()
	h.post(t, waEvent(waTestNumber, []string{waText("wamid.1", "hi")}, nil), waTestSecret)
	<-h.agentHits
	h.fake.next(t)
	h.fake.next(t)
	if reason, ok := healthReason(h.userHarness, h.channel.Spec.WhatsApp.Path); !ok || reason != healthReasonWebhookReady {
		t.Errorf("health after a retried-then-delivered reply = %q ok=%v", reason, ok)
	}

	// 131047 inside a 400: terminal at once.
	before := h.fake.count()
	h.fake.mu.Lock()
	h.fake.queue = []graphAnswer{{400, `{"error":{"message":"Re-engagement message","code":131047}}`}}
	h.fake.mu.Unlock()
	h.post(t, waEvent(waTestNumber, []string{waText("wamid.2", "late")}, nil), waTestSecret)
	<-h.agentHits
	h.fake.next(t)
	waitFor(t, func() bool {
		reason, ok := healthReason(h.userHarness, h.channel.Spec.WhatsApp.Path)
		return !ok && reason == healthReasonCallbackRejected
	})
	time.Sleep(150 * time.Millisecond)
	if n := h.fake.count() - before; n != 1 {
		t.Errorf("terminal 400 was retried: %d requests", n)
	}
}

func TestWhatsApp_PipelineErrorTravelsAsReplyText(t *testing.T) {
	h := newWhatsAppHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	h.post(t, waEvent(waTestNumber, []string{waText("wamid.1", "hi")}, nil), waTestSecret)
	for i := 0; i < 4; i++ {
		<-h.agentHits
	}
	got := h.fake.next(t)
	text, _ := got.Body["text"].(map[string]any)
	if b, _ := text["body"].(string); !strings.HasPrefix(b, errDeliveryFailed+": ") {
		t.Errorf("error reply = %q", b)
	}
}

func TestClassifyWhatsAppReply(t *testing.T) {
	if classifyWhatsAppReply(400, []byte(`{"error":{"code":130429}}`)) != bucketRetried {
		t.Error("130429 must be retried")
	}
	if classifyWhatsAppReply(400, []byte(`{"error":{"code":131047}}`)) != bucketTerminal {
		t.Error("131047 must be terminal")
	}
	if classifyWhatsAppReply(400, []byte(`nonsense`)) != bucketTerminal {
		t.Error("an unreadable 400 is terminal")
	}
	if classifyWhatsAppReply(429, nil) != bucketRetried || classifyWhatsAppReply(200, nil) != bucketDelivered {
		t.Error("the shared table must still apply")
	}
}
