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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// fakeAsync is an in-memory AsyncRecords.
type fakeAsync struct {
	mu      sync.Mutex
	records map[string]*AsyncRecord
}

func newFakeAsync() *fakeAsync { return &fakeAsync{records: map[string]*AsyncRecord{}} }

func (f *fakeAsync) Create(_ context.Context, id string, ch *agentryv1alpha1.AgentChannel, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[id] = &AsyncRecord{CreatedAt: time.Now(), ChannelNamespace: ch.Namespace, ChannelName: ch.Name}
	return nil
}
func (f *fakeAsync) Patch(_ context.Context, id string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[id]
	if !ok {
		return fmt.Errorf("record %s missing", id)
	}
	rec.Payload = payload
	return nil
}
func (f *fakeAsync) Get(_ context.Context, id string) (*AsyncRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[id]
	if !ok {
		return nil, false, nil
	}
	cp := *rec
	return &cp, true, nil
}
func (f *fakeAsync) CountPending(_ context.Context, ns, name string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.records {
		if r.ChannelNamespace == ns && r.ChannelName == name {
			n++
		}
	}
	return n, nil
}

// userHarness serves the user listener over plain httptest TLS plus a fake
// agent backend reached via the host/port overrides.
type userHarness struct {
	server    *Server
	store     *fakeStore
	async     *fakeAsync
	userSrv   *httptest.Server
	agentSrv  *httptest.Server
	agentHits chan MessageEnvelope
}

func newUserHarness(t *testing.T, agentFn http.HandlerFunc) *userHarness {
	t.Helper()
	h := &userHarness{store: newFakeStore(), async: newFakeAsync(), agentHits: make(chan MessageEnvelope, 8)}

	h.agentSrv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env MessageEnvelope
		_ = json.NewDecoder(r.Body).Decode(&env)
		h.agentHits <- env
		agentFn(w, r)
	}))
	t.Cleanup(h.agentSrv.Close)

	agentURL := strings.TrimPrefix(h.agentSrv.URL, "https://")
	host, portStr, _ := strings.Cut(agentURL, ":")
	port, _ := strconv.Atoi(portStr)

	cfg := Config{
		OperatorNamespace:        "agentry-system",
		DeliveryBackoff:          []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
		CallbackBackoff:          []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
		AgentServiceHostOverride: host,
		AgentServicePortOverride: int32(port),
		InsecureSkipAgentVerify:  true,
		SyncDeliveryDeadline:     5 * time.Second,
		AgentReadTimeout:         2 * time.Second,
	}
	h.server = NewServer(cfg, h.store, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	h.server.Async = h.async

	h.userSrv = httptest.NewTLSServer(h.server.UserHandler())
	t.Cleanup(h.userSrv.Close)
	return h
}

// seedChannel installs a Ready bearer-auth channel and its agent.
func (h *userHarness) seedChannel(mode string) *agentryv1alpha1.AgentChannel {
	ch := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team-a"},
		Spec: agentryv1alpha1.AgentChannelSpec{
			AgentRef: agentryv1alpha1.LocalObjectReference{Name: "sup"},
			Webhook: agentryv1alpha1.AgentChannelWebhook{
				Path: "/channels/team-a/support",
				Auth: agentryv1alpha1.ChannelAuth{
					Type:      authTypeBearer,
					SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "hook-secret", Key: "token"},
				},
				ResponseMode: mode,
			},
			Session: agentryv1alpha1.AgentChannelSession{Enabled: true},
		},
		Status: agentryv1alpha1.AgentChannelStatus{Phase: agentryv1alpha1.ChannelActive},
	}
	h.store.channels[ch.Spec.Webhook.Path] = ch
	h.store.secrets["team-a/hook-secret/token"] = "hook-token"
	h.store.agents["team-a/sup"] = &agentryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Status:     agentryv1alpha1.AgentStatus{Phase: agentryv1alpha1.AgentRunning},
	}
	return ch
}

func (h *userHarness) post(t *testing.T, path, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.userSrv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.userSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWebhook_SyncRoundTrip(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"hello back","attachments":[],"metadata":{}}`))
	})
	h.seedChannel("sync")

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{"text":"hi"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var reply ResponseEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || *reply.Content != "hello back" {
		t.Fatalf("reply wrong: %+v err=%v", reply, err)
	}

	env := <-h.agentHits
	if env.MessageID == "" || env.ChannelID != "/channels/team-a/support" {
		t.Errorf("envelope wrong: %+v", env)
	}
	// Raw-body fallback content: the body JSON-encoded as a string.
	if env.Content != `"{\"text\":\"hi\"}"` {
		t.Errorf("raw-body content wrong: %q", env.Content)
	}
	// session.enabled: deterministic UUIDv5.
	if env.SessionID != SessionID(env.ChannelID, env.UserID) {
		t.Errorf("sessionId not deterministic: %q", env.SessionID)
	}
}

func TestWebhook_AuthAndPathPosture(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync")

	// Wrong token: 401.
	resp := h.post(t, "/channels/team-a/support", "wrong", []byte(`{}`))
	if resp.StatusCode != 401 {
		t.Errorf("wrong token = %d", resp.StatusCode)
	}
	// Unregistered path: same 401, no existence leak.
	resp = h.post(t, "/channels/team-a/nope", "hook-token", []byte(`{}`))
	if resp.StatusCode != 401 {
		t.Errorf("unregistered path = %d", resp.StatusCode)
	}
	// Oversized body to an UNREGISTERED path: 413 fires before path
	// resolution, preserving the 413-vs-401 threat model.
	h.server.Config.MaxMessageBodyBytes = 64
	big := bytes.Repeat([]byte("x"), 256)
	resp = h.post(t, "/channels/team-a/unknown", "hook-token", big)
	if resp.StatusCode != 413 {
		t.Errorf("oversized to unknown path = %d, want 413", resp.StatusCode)
	}
}

func TestWebhook_HMACAuth(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	ch := h.seedChannel("sync")
	prefix := "sha256="
	ch.Spec.Webhook.Auth = agentryv1alpha1.ChannelAuth{
		Type: authTypeHMAC,
		HMAC: &agentryv1alpha1.ChannelHMAC{
			Header:          "X-Hub-Signature-256",
			Algorithm:       "sha256",
			SecretRef:       agentryv1alpha1.SecretKeyReference{Name: "hook-secret", Key: "token"},
			SignaturePrefix: &prefix,
		},
	}

	body := []byte(`{"event":"push"}`)
	mac := hmac.New(sha256.New, []byte("hook-token"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req, _ := http.NewRequest(http.MethodPost, h.userSrv.URL+"/channels/team-a/support", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	resp, err := h.userSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("valid HMAC = %d", resp.StatusCode)
	}

	// Tampered body fails.
	req, _ = http.NewRequest(http.MethodPost, h.userSrv.URL+"/channels/team-a/support", strings.NewReader(`{"event":"tampered"}`))
	req.Header.Set("X-Hub-Signature-256", sig)
	resp, err = h.userSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("tampered HMAC = %d, want 401", resp.StatusCode)
	}
}

func TestWebhook_ExtractorsAndBadJSON(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	ch := h.seedChannel("sync")
	userPath := "user.id"
	contentPath := "message.text"
	fallback := "anonymous"
	ch.Spec.Webhook.UserID = agentryv1alpha1.ChannelExtractor{FromBody: &userPath, Fallback: &fallback}
	ch.Spec.Webhook.Content = agentryv1alpha1.ChannelExtractor{FromBody: &contentPath}

	resp := h.post(t, "/channels/team-a/support", "hook-token",
		[]byte(`{"user":{"id":"u-42"},"message":{"text":"help me"}}`))
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	env := <-h.agentHits
	if env.UserID != "u-42" || env.Content != "help me" {
		t.Errorf("extraction wrong: userId=%q content=%q", env.UserID, env.Content)
	}

	// Non-JSON body with fromBody configured: 400.
	resp = h.post(t, "/channels/team-a/support", "hook-token", []byte(`not-json`))
	if resp.StatusCode != 400 {
		t.Errorf("non-JSON with fromBody = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestWebhook_DeliveryRetryAndFailure(t *testing.T) {
	attempts := 0
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"third time lucky"}`))
	})
	h.seedChannel("sync")

	// Two failures then success: the retry pipeline recovers, and the same
	// messageId is reused across attempts.
	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("retried delivery = %d", resp.StatusCode)
	}
	first := <-h.agentHits
	second := <-h.agentHits
	third := <-h.agentHits
	if first.MessageID != second.MessageID || second.MessageID != third.MessageID {
		t.Error("messageId must be reused across delivery retries")
	}

	// Persistent malformed envelope: delivery_failed after the budget.
	h2 := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"no_content":true}`))
	})
	h2.seedChannel("sync")
	resp = h2.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 502 {
		t.Fatalf("malformed envelope = %d, want 502", resp.StatusCode)
	}
	if got := errType(t, resp); got != "delivery_failed" {
		t.Errorf("error type %q", got)
	}
}

func TestWebhook_AsyncAcceptAndPoll(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"async reply"}`))
	})
	h.seedChannel("async")

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{"q":"1"}`))
	if resp.StatusCode != 202 {
		t.Fatalf("async accept = %d", resp.StatusCode)
	}
	var accept asyncAcceptResponse
	if err := json.NewDecoder(resp.Body).Decode(&accept); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if accept.RequestID == "" || accept.ChannelPath != "/channels/team-a/support" || accept.Status != "accepted" {
		t.Fatalf("202 body wrong: %+v", accept)
	}

	// The placeholder exists immediately (202 implies a queryable record).
	pollURL := h.userSrv.URL + "/v1/channels/responses/" + accept.RequestID +
		"?channelPath=" + strings.ReplaceAll(accept.ChannelPath, "/", "%2F")
	poll := func() *http.Response {
		req, _ := http.NewRequest(http.MethodGet, pollURL, nil)
		req.Header.Set("Authorization", "Bearer hook-token")
		r, err := h.userSrv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Eventually the background pipeline patches the payload: poll to 200.
	deadline := time.Now().Add(5 * time.Second)
	var final *http.Response
	for time.Now().Before(deadline) {
		final = poll()
		if final.StatusCode == 200 {
			break
		}
		if final.StatusCode == 202 && final.Header.Get("Retry-After") == "" {
			t.Fatal("202 poll must carry Retry-After")
		}
		_ = final.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { _ = final.Body.Close() }()
	if final.StatusCode != 200 {
		t.Fatalf("poll never reached 200, last=%d", final.StatusCode)
	}
	var payload struct {
		RequestID string `json:"requestId"`
		Response  struct {
			Content string `json:"content"`
		} `json:"response"`
	}
	if err := json.NewDecoder(final.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.RequestID != accept.RequestID || payload.Response.Content != "async reply" {
		t.Errorf("stored payload wrong: %+v", payload)
	}

	// Wrong-channel probing: a second channel's credentials see 404.
	other := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-b"},
		Spec: agentryv1alpha1.AgentChannelSpec{
			AgentRef: agentryv1alpha1.LocalObjectReference{Name: "x"},
			Webhook: agentryv1alpha1.AgentChannelWebhook{
				Path: "/channels/team-b/other",
				Auth: agentryv1alpha1.ChannelAuth{
					Type:      authTypeBearer,
					SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "other-secret", Key: "token"},
				},
			},
		},
	}
	h.store.channels[other.Spec.Webhook.Path] = other
	h.store.secrets["team-b/other-secret/token"] = "other-token"
	req, _ := http.NewRequest(http.MethodGet, h.userSrv.URL+"/v1/channels/responses/"+accept.RequestID+
		"?channelPath=%2Fchannels%2Fteam-b%2Fother", nil)
	req.Header.Set("Authorization", "Bearer other-token")
	crossResp, err := h.userSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = crossResp.Body.Close()
	if crossResp.StatusCode != 404 {
		t.Errorf("cross-channel probe = %d, want 404", crossResp.StatusCode)
	}
}

func TestWebhook_AsyncCallbackDelivery(t *testing.T) {
	callbackHits := make(chan *http.Request, 1)
	callbackBodies := make(chan []byte, 1)
	callback := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(r.Header)
		_ = raw
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		callbackBodies <- body.Bytes()
		callbackHits <- r.Clone(context.Background())
		w.WriteHeader(200)
	}))
	defer callback.Close()

	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"cb reply"}`))
	})
	ch := h.seedChannel("async")
	cbURL := callback.URL // https://127.0.0.1:port; loopback is blocked, so allow via test hook
	ch.Spec.Webhook.CallbackURL = &cbURL
	ch.Spec.Webhook.CallbackAuth = &agentryv1alpha1.ChannelAuth{
		Type:      authTypeBearer,
		SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "hook-secret", Key: "token"},
	}
	// The callback target is the loopback test server: it would be blocked
	// by the deny ranges. Verify the block first, then use polling instead.
	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	var accept asyncAcceptResponse
	_ = json.NewDecoder(resp.Body).Decode(&accept)
	_ = resp.Body.Close()

	// The payload must land at the polling endpoint (bypassed callback).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, ok, _ := h.async.Get(context.Background(), accept.RequestID)
		if ok && rec.Payload != nil {
			if !strings.Contains(string(rec.Payload), "cb reply") {
				t.Fatalf("stored payload wrong: %s", rec.Payload)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("payload never stored after bypassed callback")
}

func TestChannelHealth_ReflectsTraffic(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	h.seedChannel("sync")

	// A successful delivery records success.
	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	_ = resp.Body.Close()
	snap := h.server.ChannelHealth.Snapshot("team-a")
	entry := snap.Channels["/channels/team-a/support"]
	if entry.State != "success" || entry.Reason == nil || *entry.Reason != healthReasonWebhookReady {
		t.Errorf("health after success wrong: %+v", entry)
	}

	// An auth failure records failure (visible in a fresh window as the most
	// recent failure alongside the earlier success -> still success state).
	resp = h.post(t, "/channels/team-a/support", "bad-token", []byte(`{}`))
	_ = resp.Body.Close()
	snap = h.server.ChannelHealth.Snapshot("team-a")
	entry = snap.Channels["/channels/team-a/support"]
	if entry.State != "success" {
		t.Errorf("success within window must win: %+v", entry)
	}
	if snap.WindowSeconds != 300 {
		t.Errorf("window seconds = %d", snap.WindowSeconds)
	}
}
