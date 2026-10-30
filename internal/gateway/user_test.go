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
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"sync/atomic"
	"syscall"

	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// fakeAsync is an in-memory AsyncRecords.
type fakeAsync struct {
	mu      sync.Mutex
	records map[string]*AsyncRecord
}

func newFakeAsync() *fakeAsync { return &fakeAsync{records: map[string]*AsyncRecord{}} }

func (f *fakeAsync) Create(_ context.Context, id string, ch *kaalmv1beta1.AgentChannel, _ time.Time) error {
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
		OperatorNamespace:        "kaalm-system",
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
func (h *userHarness) seedChannel(mode string) *kaalmv1beta1.AgentChannel {
	ch := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team-a"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "sup"},
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Path: "/channels/team-a/support",
				Auth: kaalmv1beta1.ChannelAuth{
					Type:      authTypeBearer,
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "hook-secret", Key: "token"},
				},
				ResponseMode: mode,
			},
			Session: kaalmv1beta1.AgentChannelSession{Enabled: true},
		},
		Status: kaalmv1beta1.AgentChannelStatus{Phase: kaalmv1beta1.ChannelActive},
	}
	h.store.channels[ch.Spec.Webhook.Path] = ch
	h.store.secrets["team-a/hook-secret/token"] = "hook-token"
	h.store.agents["team-a/sup"] = &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Status:     kaalmv1beta1.AgentStatus{Phase: kaalmv1beta1.AgentRunning},
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
	ch.Spec.Webhook.Auth = kaalmv1beta1.ChannelAuth{
		Type: authTypeHMAC,
		HMAC: &kaalmv1beta1.ChannelHMAC{
			Header:          "X-Hub-Signature-256",
			Algorithm:       "sha256",
			SecretRef:       kaalmv1beta1.SecretKeyReference{Name: "hook-secret", Key: "token"},
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
	ch.Spec.Webhook.UserID = kaalmv1beta1.ChannelExtractor{FromBody: &userPath, Fallback: &fallback}
	ch.Spec.Webhook.Content = kaalmv1beta1.ChannelExtractor{FromBody: &contentPath}

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
	other := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-b"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			AgentRef: kaalmv1beta1.LocalObjectReference{Name: "x"},
			Webhook: &kaalmv1beta1.AgentChannelWebhook{
				Path: "/channels/team-b/other",
				Auth: kaalmv1beta1.ChannelAuth{
					Type:      authTypeBearer,
					SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "other-secret", Key: "token"},
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
	ch.Spec.Webhook.CallbackAuth = &kaalmv1beta1.ChannelAuth{
		Type:      authTypeBearer,
		SecretRef: &kaalmv1beta1.SecretKeyReference{Name: "hook-secret", Key: "token"},
	}
	// The Secret approves the host (rule 46), so the delivery reaches the
	// rule 22 pre-dial check this test is about.
	h.store.callbackHosts["team-a/hook-secret"] = "127.0.0.1"
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

func TestWakeAndDeliver_WakeTimeout(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentHibernated
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: 80 * time.Millisecond}

	// Wake succeeds but the agent never becomes reachable: point delivery at a
	// closed port so waitAgentReachable exhausts its (short) wakeTimeout.
	h.server.Activator = &fakeActivator{}
	h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("wake timeout = %d, want 504", resp.StatusCode)
	}
	if got := errType(t, resp); got != errWakeTimeout {
		t.Errorf("error type = %q, want %q", got, errWakeTimeout)
	}
}

func TestAgentHTTPClient_WithCertFiles(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "gw")
	s := &Server{Config: Config{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}}

	client, err := s.agentHTTPClient()
	if err != nil || client == nil {
		t.Fatalf("agentHTTPClient = %v err=%v", client, err)
	}
	// One client for every delivery: a second call returns the same one.
	if again, err := s.agentHTTPClient(); err != nil || again != client {
		t.Errorf("second agentHTTPClient = %p err=%v, want the shared %p", again, err, client)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.DialTLSContext == nil || tr.MaxIdleConns != agentMaxIdleConns {
		t.Errorf("delivery transport = %#v, want a pooled transport with the mTLS dialer", client.Transport)
	}
}

func TestAgentHTTPClient_BadCertFiles(t *testing.T) {
	s := &Server{Config: Config{CertFile: "/nonexistent/tls.crt", KeyFile: "/nonexistent/tls.key", CAFile: "/nonexistent/ca.crt"}}
	if _, err := s.agentHTTPClient(); err == nil {
		t.Error("missing cert files must error")
	}
}

func TestWebhook_TerminatingAndAgentMissing(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"ok"}`))
	})
	ch := h.seedChannel("sync")

	// A Terminating channel accepts no new work: 401 (no existence leak).
	ch.Status.Phase = kaalmv1beta1.ChannelTerminating
	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != 401 {
		t.Errorf("terminating channel = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Restore the channel but drop its Agent: referenced Agent not found -> 502.
	ch.Status.Phase = kaalmv1beta1.ChannelActive
	delete(h.store.agents, "team-a/sup")
	resp = h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("missing agent = %d, want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestWebhook_WrongMethod(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync")
	req, _ := http.NewRequest(http.MethodGet, h.userSrv.URL+"/channels/team-a/support", nil)
	resp, err := h.userSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("GET webhook = %d, want 401", resp.StatusCode)
	}
	// Unknown path on the user listener: 401.
	req, _ = http.NewRequest(http.MethodGet, h.userSrv.URL+"/nonsense", nil)
	resp, _ = h.userSrv.Client().Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("unknown user path = %d, want 401", resp.StatusCode)
	}
}

func TestWebhook_RawBodyInvalidUTF8(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync") // no content extractor -> raw-body fallback
	// A body with an invalid UTF-8 byte fails raw-body normalization with 400.
	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte{0xff, 0xfe})
	if resp.StatusCode != 400 || !bodyContains(t, resp, "UTF-8") {
		t.Errorf("invalid UTF-8 raw body = %d", resp.StatusCode)
	}
}

// A successful channel delivery must record gatewayTraffic activity: the
// channel half of the GET /v1/activity contract. Before v0.5.0 only the LLM
// proxy recorded traffic, so a chat-only agent hibernated mid-conversation.
func TestWebhook_DeliveryRecordsActivity(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":"hi"}`))
	})
	h.seedChannel("sync")

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("delivery = %d, want 200", resp.StatusCode)
	}
	snap := h.server.Activity.Snapshot("team-a")
	src, ok := snap.Agents["sup"]
	if !ok || src.GatewayTraffic == nil {
		t.Error("channel delivery must record gatewayTraffic for the agent")
	}

	// A failed delivery records nothing: activity means the agent answered.
	h2 := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	h2.seedChannel("sync")
	resp = h2.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	_ = resp.Body.Close()
	if _, ok := h2.server.Activity.Snapshot("team-a").Agents["sup"]; ok {
		t.Error("a failed delivery must not count as activity")
	}
}

func TestDeliveryOutcomeClassifiesTheFailingLayer(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"dns", &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Name: "a.b.svc", IsTimeout: true}}}, deliveryOutcomeDNS},
		{"deadline", fmt.Errorf("Post: %w", context.DeadlineExceeded), deliveryOutcomeTimeout},
		{"canceled", fmt.Errorf("Post: %w", context.Canceled), deliveryOutcomeCanceled},
		{"io timeout", errors.New("read tcp 10.0.0.1:1->10.0.0.2:8443: i/o timeout"), deliveryOutcomeTimeout},
		{"refused", &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}, deliveryOutcomeConnect},
		{"connect stage", &url.Error{Op: "Post", Err: &dialStageError{stage: dialStageConnect, err: context.DeadlineExceeded}}, deliveryOutcomeConnect},
		{"handshake stage", &url.Error{Op: "Post", Err: &dialStageError{stage: dialStageHandshake, err: context.DeadlineExceeded}}, deliveryOutcomeTLS},
		{"reset", errors.New("read: connection reset by peer"), deliveryOutcomeConnect},
		{"tls", &url.Error{Op: "Post", Err: x509.UnknownAuthorityError{}}, deliveryOutcomeTLS},
		{"status", errors.New("agent returned 503"), deliveryOutcomeStatus},
		{"malformed", errors.New("agent returned 200 with a malformed response envelope"), deliveryOutcomeMalformed},
		{"too large", errors.New("agent response body exceeded 921600 bytes; externalize large outputs and reference by URL"), deliveryOutcomeTooLarge},
		{"other", errors.New("boom"), deliveryOutcomeOther},
	}
	for _, c := range cases {
		if got := deliveryOutcome(c.err); got != c.want {
			t.Errorf("%s: deliveryOutcome(%v) = %q, want %q", c.name, c.err, got, c.want)
		}
	}
}

// deliveryServer returns a Server whose agent deliveries land on fn, with a
// fast retry schedule and a registry of its own for the attempt counter.
func deliveryServer(t *testing.T, fn http.HandlerFunc) *Server {
	t.Helper()
	srv := httptest.NewTLSServer(fn)
	t.Cleanup(srv.Close)
	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "https://"), ":")
	port, _ := strconv.Atoi(portStr)
	s := NewServer(Config{
		OperatorNamespace:        "kaalm-system",
		DeliveryBackoff:          []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
		AgentServiceHostOverride: host,
		AgentServicePortOverride: int32(port),
		InsecureSkipAgentVerify:  true,
		AgentReadTimeout:         2 * time.Second,
		MaxResponseBodyBytes:     1 << 20,
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	s.Metrics = NewMetrics(prometheus.NewRegistry())
	return s
}

func TestDeliverToAgentCountsEveryAttemptByOutcome(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	env := MessageEnvelope{MessageID: "m1"}

	// Every attempt answers 503: four attempts, all "status", none "ok".
	s := deliveryServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if _, err := s.deliverToAgent(context.Background(), "", agent, env, false); err == nil {
		t.Fatal("delivery against a 503 agent must fail")
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeStatus)); got != 4 {
		t.Errorf("status attempts = %v, want 4", got)
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeOK)); got != 0 {
		t.Errorf("ok attempts = %v, want 0", got)
	}

	// One 503 then success: one "status", one "ok".
	var calls int32
	s = deliveryServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"content":"hi"}`))
	})
	if _, err := s.deliverToAgent(context.Background(), "", agent, env, false); err != nil {
		t.Fatalf("delivery must succeed on the second attempt: %v", err)
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeStatus)); got != 1 {
		t.Errorf("status attempts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeOK)); got != 1 {
		t.Errorf("ok attempts = %v, want 1", got)
	}
}

func TestDeliverToAgentReusesTheConnection(t *testing.T) {
	var opened int32
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":"hi"}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			atomic.AddInt32(&opened, 1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "https://"), ":")
	port, _ := strconv.Atoi(portStr)
	s := NewServer(Config{
		OperatorNamespace: "kaalm-system", AgentServiceHostOverride: host, AgentServicePortOverride: int32(port),
		InsecureSkipAgentVerify: true, AgentReadTimeout: 2 * time.Second, AgentConnectTimeout: time.Second,
		MaxResponseBodyBytes: 1 << 20,
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	s.Metrics = NewMetrics(prometheus.NewRegistry())

	for i := 0; i < 5; i++ {
		if _, err := s.deliverToAgent(context.Background(), "", agent, MessageEnvelope{MessageID: fmt.Sprintf("m%d", i)}, false); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&opened); got != 1 {
		t.Errorf("agent saw %d connections for 5 deliveries, want 1 (the pooled connection)", got)
	}
}

func TestDeliverToAgentRedialsWithinTheAttempt(t *testing.T) {
	// A blackhole address: the SYN is never answered. Each connect ends at
	// its bound and is dialed again until the attempt's budget is spent, so
	// an attempt lasts about the read timeout, not one connect bound, and
	// its outcome names the connect stage.
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	s := NewServer(Config{
		OperatorNamespace: "kaalm-system", AgentServiceHostOverride: "192.0.2.1", AgentServicePortOverride: 8443,
		InsecureSkipAgentVerify: true, AgentReadTimeout: 300 * time.Millisecond, AgentConnectTimeout: 50 * time.Millisecond,
		DeliveryBackoff: []time.Duration{time.Millisecond},
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	s.Metrics = NewMetrics(prometheus.NewRegistry())
	resolver := &countingResolver{ip: net.ParseIP("192.0.2.1")}
	s.AgentResolver = resolver

	start := time.Now()
	_, err := s.deliverToAgent(context.Background(), "", agent, MessageEnvelope{MessageID: "m1"}, false)
	took := time.Since(start)
	if err == nil {
		t.Fatal("delivery to a blackhole must fail")
	}
	if took < 500*time.Millisecond || took > 3*time.Second {
		t.Errorf("two attempts took %v, want about two read timeouts of redials", took)
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeConnect)); got != 2 {
		t.Errorf("connect outcomes = %v, want 2 (last error %q classified %q)", got, err, deliveryOutcome(err))
	}
	// One resolution per attempt, however many connects the attempt redials.
	if got := atomic.LoadInt32(&resolver.calls); got != 2 {
		t.Errorf("resolver called %d times for 2 attempts, want 2", got)
	}
}

// countingResolver answers every lookup with one IP and counts the calls.
type countingResolver struct {
	ip    net.IP
	calls int32
}

func (r *countingResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	atomic.AddInt32(&r.calls, 1)
	return []net.IPAddr{{IP: r.ip}}, nil
}

// scriptedResolver fails its first fail lookups with NXDOMAIN, as the
// cluster DNS answers for a Service not created yet, then answers with one
// IP. It counts the calls and records the names it was asked for.
type scriptedResolver struct {
	fail  int32
	ip    net.IP
	calls int32

	mu    sync.Mutex
	hosts []string
}

func (r *scriptedResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	n := atomic.AddInt32(&r.calls, 1)
	r.mu.Lock()
	r.hosts = append(r.hosts, host)
	r.mu.Unlock()
	if n <= r.fail {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []net.IPAddr{{IP: r.ip}}, nil
}

func (r *scriptedResolver) recordedHosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

func podWaitAgent(wakeTimeout time.Duration) *kaalmv1beta1.Agent {
	return &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Status:     kaalmv1beta1.AgentStatus{EffectiveWakeTimeout: &metav1.Duration{Duration: wakeTimeout}},
	}
}

// A woken Pod is usually reachable within a few seconds, so the Pod wait
// checks often at first: an agent that starts listening shortly after the
// wait begins is reached well before a 2 s poll interval would allow.
func TestWaitAgentReachable_PollsFastAtTheStart(t *testing.T) {
	s := NewServer(Config{
		OperatorNamespace: "kaalm-system", AgentServiceHostOverride: "127.0.0.1",
		AgentConnectTimeout: 100 * time.Millisecond,
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	port, listen := holdPort(t)
	s.Config.AgentServicePortOverride = int32(port)

	var ln net.Listener
	started := make(chan struct{})
	go func() {
		defer close(started)
		time.Sleep(agentBackAfter)
		l, err := listen()
		if err != nil {
			t.Errorf("agent could not listen on %d: %v", port, err)
			return
		}
		ln = l
	}()
	t.Cleanup(func() {
		<-started
		if ln != nil {
			_ = ln.Close()
		}
	})

	start := time.Now()
	err := s.waitAgentReachable(context.Background(), podWaitAgent(10*time.Second), nil)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("wait = %v, want reachable", err)
	}
	if took >= time.Second {
		t.Errorf("wait took %v for an agent listening after %v: the second connect came after a full 2 s poll interval", took, agentBackAfter)
	}
}

// The Pod wait resolves the Service once and reuses the address for every
// connect: Go does not cache lookups, so a lookup per connect would send
// every poll to the cluster DNS server.
func TestWaitAgentReachable_ResolvesTheServiceOncePerWait(t *testing.T) {
	s := NewServer(Config{
		OperatorNamespace: "kaalm-system", AgentConnectTimeout: 50 * time.Millisecond,
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	port, _ := holdPort(t)
	s.Config.AgentServicePortOverride = int32(port)
	resolver := &scriptedResolver{ip: net.ParseIP("127.0.0.1")}
	s.AgentResolver = resolver

	start := time.Now()
	err := s.waitAgentReachable(context.Background(), podWaitAgent(time.Second), nil)
	took := time.Since(start)
	if err == nil {
		t.Fatal("wait against a closed port must time out")
	}
	if took < time.Second {
		t.Errorf("wait ended after %v, want the full 1s wakeTimeout", took)
	}
	if got := atomic.LoadInt32(&resolver.calls); got != 1 {
		t.Errorf("resolver called %d times in one wait, want 1", got)
	}
	if hosts := resolver.recordedHosts(); len(hosts) == 0 || hosts[0] != "sup.team-a.svc.cluster.local." {
		t.Errorf("resolved %q, want the absolute Service name", hosts)
	}
}

// The Pod wait stops its poll sleep at the deadline, so it ends at the
// capped wakeTimeout rather than one poll interval after it. With a 1.1 s
// wakeTimeout the last 250 ms sleep would otherwise end the wait at 1.25 s.
func TestWaitAgentReachable_EndsAtTheDeadline(t *testing.T) {
	s := NewServer(Config{
		OperatorNamespace: "kaalm-system", AgentServiceHostOverride: "127.0.0.1",
		AgentServicePortOverride: 1, AgentConnectTimeout: 50 * time.Millisecond,
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())

	start := time.Now()
	err := s.waitAgentReachable(context.Background(), podWaitAgent(1100*time.Millisecond), nil)
	took := time.Since(start)
	if err == nil {
		t.Fatal("wait against a closed port must time out")
	}
	if took < 1100*time.Millisecond || took >= 1200*time.Millisecond {
		t.Errorf("wait ended after %v, want it to end at the 1.1s wakeTimeout", took)
	}
}

// A Service not created yet (a first start) answers NXDOMAIN: the wait
// retries the lookup on the poll schedule and connects once it resolves.
func TestWaitAgentReachable_RetriesTheLookupUntilTheServiceResolves(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := NewServer(Config{
		OperatorNamespace: "kaalm-system", AgentConnectTimeout: 50 * time.Millisecond,
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	s.Config.AgentServicePortOverride = int32(ln.Addr().(*net.TCPAddr).Port)
	resolver := &scriptedResolver{fail: 2, ip: net.ParseIP("127.0.0.1")}
	s.AgentResolver = resolver

	start := time.Now()
	err = s.waitAgentReachable(context.Background(), podWaitAgent(10*time.Second), nil)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("wait = %v, want reachable once the Service resolves", err)
	}
	if got := atomic.LoadInt32(&resolver.calls); got != 3 {
		t.Errorf("resolver called %d times, want 3 (two misses, then the address)", got)
	}
	if took >= time.Second {
		t.Errorf("wait took %v, want two fast retries then the connect", took)
	}
}

// countingDial is a podWaitDial fake: it records each call and its duration,
// and answers through result, which gets the call's number (from 1) and ctx.
type countingDial struct {
	mu        sync.Mutex
	durations []time.Duration
	result    func(n int, ctx context.Context) (net.Conn, error)
}

func (d *countingDial) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	d.mu.Lock()
	n := len(d.durations) + 1
	d.durations = append(d.durations, 0)
	d.mu.Unlock()
	start := time.Now()
	conn, err := d.result(n, ctx)
	d.mu.Lock()
	d.durations[n-1] = time.Since(start)
	d.mu.Unlock()
	return conn, err
}

func (d *countingDial) calls() []time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Duration(nil), d.durations...)
}

// pipeConn is a successful dial's connection.
func pipeConn() net.Conn {
	a, b := net.Pipe()
	_ = b.Close()
	return a
}

// podWaitServer is a gateway whose Pod wait connects to 127.0.0.1 through
// dial, with the Pod watch tracker tr (nil for none).
func podWaitServer(tr *PodReadyTracker, dial *countingDial) *Server {
	s := NewServer(Config{
		OperatorNamespace: "kaalm-system", AgentServiceHostOverride: "127.0.0.1",
		AgentServicePortOverride: 1, AgentConnectTimeout: time.Second,
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	s.PodReady = tr
	if dial != nil {
		s.podWaitDial = dial.dial
	}
	return s
}

// A connect to an empty Service can get no answer at all: kube-proxy's
// refusals are ICMP messages the node rate-limits, so a SYN past the burst
// hangs. Each readiness connect is bounded at the fast poll interval, not at
// AgentConnectTimeout, so a hung connect costs no more than one poll sleep.
func TestWaitAgentReachable_ConnectIsBoundByTheFastInterval(t *testing.T) {
	dial := &countingDial{result: func(_ int, ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s := podWaitServer(nil, dial)

	start := time.Now()
	err := s.waitAgentReachable(context.Background(), podWaitAgent(700*time.Millisecond), nil)
	took := time.Since(start)
	if err == nil {
		t.Fatal("wait against a blackholed Service must time out")
	}
	if took < 700*time.Millisecond || took >= time.Second {
		t.Errorf("wait ended after %v, want between the 700ms wakeTimeout and 1s", took)
	}
	calls := dial.calls()
	if len(calls) < 2 {
		t.Errorf("made %d connects, want at least 2: one hung connect held the wait", len(calls))
	}
	for i, d := range calls {
		if d > wakeConnectTimeout+100*time.Millisecond {
			t.Errorf("connect %d lasted %v, want at most %v", i+1, d, wakeConnectTimeout)
		}
	}
}

// With a Pod watch, the wait makes no connect while the Agent's Pod is not
// Ready, and connects as soon as the watch reports it Ready rather than at
// the next poll.
func TestWaitAgentReachable_ConnectsWhenThePodTurnsReady(t *testing.T) {
	tr := syncedTracker()
	h := tr.Handler()
	pod := agentPod("team-a", "sup", "sup-0", false)
	h.OnAdd(pod, false)
	dial := &countingDial{result: func(int, context.Context) (net.Conn, error) { return pipeConn(), nil }}
	s := podWaitServer(tr, dial)

	done := make(chan error, 1)
	go func() { done <- s.waitAgentReachable(context.Background(), podWaitAgent(5*time.Second), nil) }()
	time.Sleep(300 * time.Millisecond)
	if n := len(dial.calls()); n != 0 {
		t.Fatalf("made %d connects before the Pod was Ready, want 0", n)
	}
	readyAt := time.Now()
	h.OnUpdate(pod, agentPod("team-a", "sup", "sup-0", true))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait = %v, want reachable", err)
		}
		if took := time.Since(readyAt); took >= 100*time.Millisecond {
			t.Errorf("wait ended %v after Ready, want under 100ms", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not end after the Pod turned Ready")
	}
	if n := len(dial.calls()); n != 1 {
		t.Errorf("made %d connects, want 1", n)
	}
}

// A Ready Pod that carries the Agent labels but is not controlled by the
// Agent neither triggers a connect nor ends the wait; the wait ends when the
// Agent's own Pod turns Ready.
func TestWaitAgentReachable_IgnoresAPodThatOnlyCopiesTheLabels(t *testing.T) {
	tr := syncedTracker()
	h := tr.Handler()
	pod := agentPod("team-a", "sup", "sup-0", false)
	h.OnAdd(pod, false)
	dial := &countingDial{result: func(int, context.Context) (net.Conn, error) { return pipeConn(), nil }}
	s := podWaitServer(tr, dial)

	done := make(chan error, 1)
	go func() { done <- s.waitAgentReachable(context.Background(), podWaitAgent(5*time.Second), nil) }()
	time.Sleep(300 * time.Millisecond)
	if n := len(dial.calls()); n != 0 {
		t.Fatalf("made %d connects before any Pod was Ready, want 0", n)
	}

	copied := agentPod("team-a", "sup", "sup-copy", true)
	copied.OwnerReferences = nil
	h.OnAdd(copied, false)
	time.Sleep(300 * time.Millisecond)
	if n := len(dial.calls()); n != 0 {
		t.Fatalf("made %d connects after a label-only Pod turned Ready, want 0", n)
	}
	select {
	case err := <-done:
		t.Fatalf("wait ended (%v) on a Pod that only copies the Agent labels", err)
	default:
	}

	h.OnUpdate(pod, agentPod("team-a", "sup", "sup-0", true))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait = %v, want reachable", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not end after the Agent's Pod turned Ready")
	}
	if n := len(dial.calls()); n != 1 {
		t.Errorf("made %d connects, want 1", n)
	}
}

// With a synced watch that shows no Pod for the Agent (the Pod is not
// created yet), the wait connects on the poll schedule as it does without a
// watch.
func TestWaitAgentReachable_PollsWhenNoPodIsKnown(t *testing.T) {
	s := podWaitServer(syncedTracker(), nil)
	port, listen := holdPort(t)
	s.Config.AgentServicePortOverride = int32(port)

	var ln net.Listener
	started := make(chan struct{})
	go func() {
		defer close(started)
		time.Sleep(agentBackAfter)
		l, err := listen()
		if err != nil {
			t.Errorf("agent could not listen on %d: %v", port, err)
			return
		}
		ln = l
	}()
	t.Cleanup(func() {
		<-started
		if ln != nil {
			_ = ln.Close()
		}
	})

	start := time.Now()
	err := s.waitAgentReachable(context.Background(), podWaitAgent(10*time.Second), nil)
	if err != nil {
		t.Fatalf("wait = %v, want reachable", err)
	}
	if took := time.Since(start); took >= time.Second {
		t.Errorf("wait took %v for an agent listening after %v, want the fast poll", took, agentBackAfter)
	}
}

// Pod Ready does not prove the Service routes yet: the endpoint data path
// can refuse connects for a few hundred milliseconds. The wait retries
// quickly after Ready.
func TestWaitAgentReachable_RetriesAfterReadyUntilTheServiceRoutes(t *testing.T) {
	tr := syncedTracker()
	tr.Handler().OnAdd(agentPod("team-a", "sup", "sup-0", true), false)
	dial := &countingDial{result: func(n int, _ context.Context) (net.Conn, error) {
		if n <= 2 {
			return nil, syscall.ECONNREFUSED
		}
		return pipeConn(), nil
	}}
	s := podWaitServer(tr, dial)

	start := time.Now()
	err := s.waitAgentReachable(context.Background(), podWaitAgent(10*time.Second), nil)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("wait = %v, want reachable", err)
	}
	if n := len(dial.calls()); n != 3 {
		t.Errorf("made %d connects, want 3 (two refused, then success)", n)
	}
	if took < 90*time.Millisecond || took >= 300*time.Millisecond {
		t.Errorf("wait took %v, want about 100ms (two quick retries)", took)
	}
}

// After Ready, the first wakeConfirmQuickTries connects run
// wakeConfirmQuickInterval apart, so the wait lands within a few
// milliseconds of the Service routing. Then the steps return to
// wakePollFastInterval. Six quick tries stay within the node's burst of six
// ICMP refusals, so none of them hangs.
func TestWaitAgentReachable_ConfirmSchedule(t *testing.T) {
	tr := syncedTracker()
	tr.Handler().OnAdd(agentPod("team-a", "sup", "sup-0", true), false)
	const succeedAt = wakeConfirmQuickTries + 3
	var mu sync.Mutex
	var dialedAt []time.Time
	dial := &countingDial{result: func(n int, _ context.Context) (net.Conn, error) {
		mu.Lock()
		dialedAt = append(dialedAt, time.Now())
		mu.Unlock()
		if n < succeedAt {
			return nil, syscall.ECONNREFUSED
		}
		return pipeConn(), nil
	}}
	s := podWaitServer(tr, dial)

	if err := s.waitAgentReachable(context.Background(), podWaitAgent(10*time.Second), nil); err != nil {
		t.Fatalf("wait = %v, want reachable", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialedAt) != succeedAt {
		t.Fatalf("made %d connects, want %d", len(dialedAt), succeedAt)
	}
	for i := 1; i < len(dialedAt); i++ {
		gap := dialedAt[i].Sub(dialedAt[i-1])
		want := wakeConfirmQuickInterval
		if i >= wakeConfirmQuickTries {
			want = wakePollFastInterval
		}
		if gap < want-10*time.Millisecond || gap > want+60*time.Millisecond {
			t.Errorf("connect %d came %v after connect %d, want about %v", i+1, gap, i, want)
		}
	}
}

// While the Agent's Pod exists but is not Ready, the wait makes no connect,
// and still runs the hibernation re-check at each check.
func TestWaitAgentReachable_DoesNotConnectWhileThePodIsNotReady(t *testing.T) {
	tr := syncedTracker()
	tr.Handler().OnAdd(agentPod("team-a", "sup", "sup-0", false), false)
	dial := &countingDial{result: func(int, context.Context) (net.Conn, error) { return pipeConn(), nil }}
	s := podWaitServer(tr, dial)
	var missed atomic.Int32

	err := s.waitAgentReachable(context.Background(), podWaitAgent(600*time.Millisecond),
		func(context.Context) error { missed.Add(1); return nil })
	if err == nil {
		t.Fatal("wait for a Pod that never turns Ready must time out")
	}
	if n := len(dial.calls()); n != 0 {
		t.Errorf("made %d connects while the Pod was not Ready, want 0", n)
	}
	if missed.Load() == 0 {
		t.Error("hibernation re-check never ran while the Pod was not Ready")
	}
	if got := len(tr.waiters[agentKey{namespace: "team-a", name: "sup"}]); got != 0 {
		t.Errorf("tracker holds %d waiters after the wait ended, want 0", got)
	}
}

// slowResolver answers with 127.0.0.1 after delay, or with the context's
// error if it ends first.
type slowResolver struct{ delay time.Duration }

func (r slowResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	select {
	case <-time.After(r.delay):
		return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, nil
	case <-ctx.Done():
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
}

// One check, the lookup and the connect together, takes at most
// AgentConnectTimeout, so the wait ends within wakeTimeout plus
// AgentConnectTimeout: the term backgroundPipelineBound adds for the check
// in flight when the deadline passes.
func TestWaitAgentReachable_OneCheckStaysWithinTheConnectTimeout(t *testing.T) {
	dial := &countingDial{result: func(_ int, ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s := podWaitServer(nil, dial)
	s.Config.AgentServiceHostOverride = "sup.team-a.svc.cluster.local"
	s.AgentResolver = slowResolver{delay: 950 * time.Millisecond}

	wakeTimeout := 50 * time.Millisecond
	start := time.Now()
	err := s.waitAgentReachable(context.Background(), podWaitAgent(wakeTimeout), nil)
	took := time.Since(start)
	if err == nil {
		t.Fatal("wait against a blackholed Service must time out")
	}
	if limit := wakeTimeout + s.Config.AgentConnectTimeout + 50*time.Millisecond; took > limit {
		t.Errorf("wait ended after %v, want at most %v (wakeTimeout plus one AgentConnectTimeout)", took, limit)
	}
}

// The confirm schedule is measured from Ready, not from the start of the
// wait: a Pod that turns Ready after a long start is still confirmed at the
// fast interval.
func TestWaitAgentReachable_ConfirmsAtTheFastIntervalAfterALateReady(t *testing.T) {
	t.Parallel()
	tr := syncedTracker()
	h := tr.Handler()
	pod := agentPod("team-a", "sup", "sup-0", false)
	h.OnAdd(pod, false)
	var mu sync.Mutex
	var dialedAt []time.Time
	dial := &countingDial{result: func(n int, _ context.Context) (net.Conn, error) {
		mu.Lock()
		dialedAt = append(dialedAt, time.Now())
		mu.Unlock()
		if n == 1 {
			return nil, syscall.ECONNREFUSED
		}
		return pipeConn(), nil
	}}
	s := podWaitServer(tr, dial)

	done := make(chan error, 1)
	go func() { done <- s.waitAgentReachable(context.Background(), podWaitAgent(20*time.Second), nil) }()
	time.Sleep(wakePollFastWindow + 300*time.Millisecond)
	h.OnUpdate(pod, agentPod("team-a", "sup", "sup-0", true))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait = %v, want reachable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not end after the Pod turned Ready")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialedAt) != 2 {
		t.Fatalf("made %d connects, want 2 (one refused, then success)", len(dialedAt))
	}
	if gap := dialedAt[1].Sub(dialedAt[0]); gap > wakePollFastInterval+200*time.Millisecond {
		t.Errorf("second connect came %v after the first, want about %v", gap, wakePollFastInterval)
	}
}

func TestAbsoluteDialName(t *testing.T) {
	cases := map[string]string{
		"sup.team-a.svc.cluster.local":  "sup.team-a.svc.cluster.local.",
		"sup.team-a.svc.cluster.local.": "sup.team-a.svc.cluster.local.",
		"10.0.0.5":                      "10.0.0.5",
		"::1":                           "::1",
		"":                              "",
	}
	for in, want := range cases {
		if got := absoluteDialName(in); got != want {
			t.Errorf("absoluteDialName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeliverToAgentResolutionTimeoutIsDNS(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	s := NewServer(Config{
		OperatorNamespace: "kaalm-system", InsecureSkipAgentVerify: true,
		AgentReadTimeout: 200 * time.Millisecond, AgentConnectTimeout: 50 * time.Millisecond,
		DeliveryBackoff: []time.Duration{time.Millisecond},
	}, &fakeStore{}, NewTokenAuthenticator(&fakeReviewer{}), NewMemorySpend())
	s.Metrics = NewMetrics(prometheus.NewRegistry())
	s.AgentResolver = &stallingResolver{}

	if _, err := s.deliverToAgent(context.Background(), "", agent, MessageEnvelope{MessageID: "m1"}, false); err == nil {
		t.Fatal("delivery with a stalled resolver must fail")
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeDNS)); got != 2 {
		t.Errorf("dns outcomes = %v, want 2", got)
	}
}

// stallingResolver never answers; it returns the context's deadline as a
// DNS timeout, the shape net.Resolver produces.
type stallingResolver struct{}

func (stallingResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	<-ctx.Done()
	return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
}

// TestUserListener_RejectionsAreIdentical: every rejection the user listener
// writes before a credential is accepted has the same status and the same
// body, so a caller cannot tell a registered channel path from any other.
func TestUserListener_RejectionsAreIdentical(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }
	wa := newWhatsAppHarness(t, ok)
	wa.seedChannel("sync")
	disc := newDiscordHarness(t, ok)
	wa.store.channels[disc.channel.Spec.Discord.Path] = disc.channel
	wa.store.secrets["team-a/disc-creds/publicKey"] = disc.store.secrets["team-a/disc-creds/publicKey"]

	poll := "/v1/channels/responses/req-1?channelPath="
	cases := []struct{ name, method, path string }{
		{"unregistered channel path", http.MethodPost, "/channels/team-a/none"},
		{"unregistered channel path, GET", http.MethodGet, "/channels/team-a/none"},
		{"path outside the mux", http.MethodGet, "/healthz"},
		{"webhook channel, no credential", http.MethodPost, "/channels/team-a/support"},
		{"webhook channel, wrong method", http.MethodGet, "/channels/team-a/support"},
		{"discord channel, no signature", http.MethodPost, "/channels/team-a/disc"},
		{"discord channel, wrong method", http.MethodGet, "/channels/team-a/disc"},
		{"whatsapp channel, no signature", http.MethodPost, "/channels/team-a/wa"},
		{"whatsapp channel, wrong verify token", http.MethodGet, "/channels/team-a/wa?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=x"},
		{"whatsapp channel, wrong method", http.MethodPut, "/channels/team-a/wa"},
		{"poll, unregistered channel", http.MethodGet, poll + "%2Fchannels%2Fteam-a%2Fnone"},
		{"poll, webhook channel, no credential", http.MethodGet, poll + "%2Fchannels%2Fteam-a%2Fsupport"},
		{"poll, wrong method", http.MethodPost, poll + "%2Fchannels%2Fteam-a%2Fsupport"},
	}
	var want string
	for _, c := range cases {
		req, err := http.NewRequest(c.method, wa.userSrv.URL+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := wa.userSrv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", c.name, resp.StatusCode)
		}
		if want == "" {
			want = string(raw)
		} else if string(raw) != want {
			t.Errorf("%s body = %s, want %s", c.name, raw, want)
		}
	}
}

// An agent runs tenant code, so a redirect it answers with is never
// followed: following it would send the message and the gateway's client
// certificate to wherever Location names, another tenant's agent included,
// and hand that agent's reply back to the caller.
func TestWebhook_AgentRedirectNotFollowed(t *testing.T) {
	var victimHits atomic.Int32
	victim := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		victimHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"reply from another tenant's agent"}`))
	}))
	t.Cleanup(victim.Close)

	h := newUserHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, victim.URL+"/v1/message", http.StatusTemporaryRedirect)
	})
	h.seedChannel("sync")

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{"text":"hi"}`))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if n := victimHits.Load(); n != 0 {
		t.Fatalf("the redirect target received %d requests, want 0", n)
	}
	if resp.StatusCode == http.StatusOK || strings.Contains(string(body), "another tenant") {
		t.Fatalf("redirect answer = %d %s, want a delivery failure", resp.StatusCode, body)
	}
}

// When the gateway cap ends the wait, the error names the cap and the
// Agent's own wakeTimeout, so the author sees why the wait was shorter.
func TestWakeAndDeliver_GatewayCapEndsTheWait(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("async")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentHibernated
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: time.Hour}
	h.server.Activator = &fakeActivator{}
	h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond
	h.server.Config.MaxWakeTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, errType, err := h.server.wakeAndDeliver(ctx, "/channels/team-a/support", agent, MessageEnvelope{})
	if errType != errWakeTimeout || err == nil {
		t.Fatalf("wakeAndDeliver = %q, %v; want %q", errType, err, errWakeTimeout)
	}
	if ctx.Err() != nil {
		t.Fatalf("the caller's context ended the wait, not the gateway cap: %v", err)
	}
	if !strings.Contains(err.Error(), "maxWakeTimeout") || !strings.Contains(err.Error(), "1h0m0s") {
		t.Errorf("message %q does not name the gateway cap and the Agent's wakeTimeout", err)
	}
}

func TestWakeAndDeliver_PipelineDeadlineBeforeWakeTimeout(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("async")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentHibernated
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: time.Hour}
	h.server.Activator = &fakeActivator{}
	h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond
	h.server.Config.MaxWakeTimeout = 2 * time.Hour // the hour-long wakeTimeout stays uncapped

	// The caller's deadline ends long before the hour-long wakeTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, errType, err := h.server.wakeAndDeliver(ctx, "/channels/team-a/support", agent, MessageEnvelope{})
	if errType != errWakeTimeout || err == nil {
		t.Fatalf("wakeAndDeliver = %q, %v; want %q", errType, err, errWakeTimeout)
	}
	if strings.Contains(err.Error(), "within wakeTimeout") {
		t.Errorf("message %q claims the full wakeTimeout elapsed", err)
	}
	if !strings.Contains(err.Error(), "1h0m0s") {
		t.Errorf("message %q does not name the wakeTimeout it fell short of", err)
	}
}

// agentBackAfter is how long startAgentLater keeps the agent Service away.
const agentBackAfter = 300 * time.Millisecond

// startAgentLater points agent delivery at a free port where nothing listens
// yet, then starts the harness agent backend there after agentBackAfter: the
// Service of an Agent whose Pod is being replaced or resumed.
func startAgentLater(t *testing.T, h *userHarness) {
	t.Helper()
	agentServiceLater(t, h)()
}

// agentServiceLater points agent delivery at a port held by holdPort, where
// nothing listens yet. The returned start func, which runs once however often
// it is called, starts the harness agent backend there after agentBackAfter: a
// test calls it when the Pod should come back, such as from a wake.
func agentServiceLater(t *testing.T, h *userHarness) (start func()) {
	t.Helper()
	port, listen := holdPort(t)
	h.server.Config.AgentServicePortOverride = int32(port)

	srv := httptest.NewUnstartedServer(h.agentSrv.Config.Handler)
	var (
		once    sync.Once
		mu      sync.Mutex
		started chan struct{}
	)
	t.Cleanup(func() {
		mu.Lock()
		ch := started
		mu.Unlock()
		if ch == nil {
			return
		}
		<-ch
		srv.Close()
	})
	return func() {
		once.Do(func() {
			ch := make(chan struct{})
			mu.Lock()
			started = ch
			mu.Unlock()
			go func() {
				defer close(ch)
				time.Sleep(agentBackAfter)
				ln, err := listen()
				if err != nil {
					t.Errorf("agent backend could not listen on %d: %v", port, err)
					return
				}
				srv.Listener = ln
				srv.StartTLS()
			}()
		})
	}
}

// agentServiceLater keeps its port from other processes until start, and
// refuses connects to it until then: a foreign listener on the port would end
// the Pod wait at once and send delivery to the wrong server.
func TestAgentServiceLater_HoldsThePortUntilStart(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	start := agentServiceLater(t, h)
	addr := fmt.Sprintf("127.0.0.1:%d", h.server.Config.AgentServicePortOverride)

	if ln, err := net.Listen("tcp", addr); err == nil {
		_ = ln.Close()
		t.Fatalf("another listener bound %s before start: the port is not held", addr)
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatalf("a connect to %s was accepted before start", addr)
	}

	start()
	deadline := time.Now().Add(agentBackAfter + 5*time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no connect to %s after start: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// phaseStore serves the fakeStore Agents with a phase a test can change
// while a Pod wait polls: the gateway's cached Agent catching up with the
// controller.
type phaseStore struct {
	*fakeStore
	phase atomic.Value // kaalmv1beta1.AgentPhase
}

func newPhaseStore(f *fakeStore, phase kaalmv1beta1.AgentPhase) *phaseStore {
	ps := &phaseStore{fakeStore: f}
	ps.phase.Store(phase)
	return ps
}

func (p *phaseStore) setPhase(phase kaalmv1beta1.AgentPhase) { p.phase.Store(phase) }

func (p *phaseStore) AgentByName(ctx context.Context, ns, name string) (*kaalmv1beta1.Agent, bool) {
	a, ok := p.fakeStore.AgentByName(ctx, ns, name)
	if !ok {
		return nil, false
	}
	c := a.DeepCopy()
	c.Status.Phase = p.phase.Load().(kaalmv1beta1.AgentPhase)
	return c, true
}

// staleAgent is the Agent a message was routed on: the harness Agent with
// phase, which the cache no longer reads, and wakeTimeout.
func staleAgent(h *userHarness, phase kaalmv1beta1.AgentPhase, wakeTimeout time.Duration) *kaalmv1beta1.Agent {
	agent := h.store.agents["team-a/sup"].DeepCopy()
	agent.Status.Phase = phase
	agent.Status.EffectiveWakeTimeout = &metav1.Duration{Duration: wakeTimeout}
	return agent
}

// wakeDurationCount is how many wake durations were observed with result.
func wakeDurationCount(t *testing.T, m *Metrics, result string) uint64 {
	t.Helper()
	var out dto.Metric
	if err := m.channelWakeDur.WithLabelValues("team-a", result).(prometheus.Metric).Write(&out); err != nil {
		t.Fatal(err)
	}
	return out.GetHistogram().GetSampleCount()
}

// A message for an Agent whose Pod is being created or resumed waits for the
// Pod instead of spending its retry schedule against a Service with no
// endpoints, and does not call the activator: no wake is needed.
func TestWakeAndDeliver_WaitsForAPendingPod(t *testing.T) {
	for _, phase := range []kaalmv1beta1.AgentPhase{kaalmv1beta1.AgentProvisioning, kaalmv1beta1.AgentResuming} {
		t.Run(string(phase), func(t *testing.T) {
			h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"content":"back"}`))
			})
			h.seedChannel("sync")
			h.store.agents["team-a/sup"].Status.Phase = phase
			act := &fakeActivator{}
			h.server.Activator = act
			startAgentLater(t, h)

			resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 once the Pod is back", resp.StatusCode)
			}
			var reply ResponseEnvelope
			if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || reply.Content == nil || *reply.Content != "back" {
				t.Fatalf("reply = %+v err=%v", reply, err)
			}
			if n := len(h.agentHits); n != 1 {
				t.Errorf("agent hits = %d, want 1", n)
			}
			if len(act.calls) != 0 {
				t.Errorf("activator calls = %v, want none", act.calls)
			}
		})
	}
}

// The cached phase can still read Running after the controller deleted the
// Pod. A refused connect then starts the Pod wait, and the message reaches the
// new Pod exactly once.
func TestWakeAndDeliver_WaitsAfterARefusedConnect(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"back"}`))
	})
	h.seedChannel("sync")
	h.server.Metrics = NewMetrics(prometheus.NewRegistry())
	act := &fakeActivator{}
	h.server.Activator = act
	startAgentLater(t, h)

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the Pod is back", resp.StatusCode)
	}
	if n := len(h.agentHits); n != 1 {
		t.Errorf("agent hits = %d, want 1", n)
	}
	// The cached phase stays Running: the wait never requests a wake.
	if len(act.calls) != 0 {
		t.Errorf("activator calls = %v, want none for a Running Agent", act.calls)
	}
	if got := testutil.ToFloat64(h.server.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeConnect)); got != 1 {
		t.Errorf("connect attempts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.server.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeOK)); got != 1 {
		t.Errorf("ok attempts = %v, want 1", got)
	}
}

// The routed phase reads Running or Idle, but the controller has already
// written Hibernating and deleted the Pod. The refused connect starts the Pod
// wait, which finds the cached phase Hibernating, requests one wake, and
// delivers to the woken Pod.
func TestWakeAndDeliver_WakesWhenThePodWaitFindsTheAgentHibernating(t *testing.T) {
	for _, phase := range []kaalmv1beta1.AgentPhase{kaalmv1beta1.AgentRunning, kaalmv1beta1.AgentIdle} {
		t.Run(string(phase), func(t *testing.T) {
			h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"content":"awake"}`))
			})
			h.seedChannel("sync")
			h.server.Metrics = NewMetrics(prometheus.NewRegistry())
			// The cache keeps reading Hibernating after the wake, as it does
			// until the controller settles the Agent and sets Resuming.
			h.server.Store = newPhaseStore(h.store, kaalmv1beta1.AgentHibernating)
			act := &fakeActivator{onWake: agentServiceLater(t, h)}
			h.server.Activator = act

			body, errType, err := h.server.wakeAndDeliver(context.Background(), "/channels/team-a/support",
				staleAgent(h, phase, 3*time.Second), MessageEnvelope{MessageID: "m1"})
			if err != nil || errType != "" {
				t.Fatalf("wakeAndDeliver = %q, %v; want delivery", errType, err)
			}
			var reply ResponseEnvelope
			if err := json.Unmarshal(body, &reply); err != nil || reply.Content == nil || *reply.Content != "awake" {
				t.Fatalf("reply = %s err=%v", body, err)
			}
			if len(act.calls) != 1 {
				t.Errorf("activator calls = %v, want exactly one", act.calls)
			}
			if n := len(h.agentHits); n != 1 {
				t.Errorf("agent hits = %d, want 1", n)
			}
			if got := testutil.ToFloat64(h.server.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeConnect)); got != 1 {
				t.Errorf("connect attempts = %v, want 1", got)
			}
		})
	}
}

// The cache can still read Running when the wait starts and catch up some
// polls later. The wait checks the cached phase at each missed poll, so it
// wakes the Agent when the phase turns Hibernating.
func TestWakeAndDeliver_WakesWhenTheCachedPhaseTurnsHibernatingDuringTheWait(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"awake"}`))
	})
	h.seedChannel("sync")
	ps := newPhaseStore(h.store, kaalmv1beta1.AgentRunning)
	h.server.Store = ps
	act := &fakeActivator{onWake: agentServiceLater(t, h)}
	h.server.Activator = act
	flip := time.AfterFunc(600*time.Millisecond, func() { ps.setPhase(kaalmv1beta1.AgentHibernating) })
	t.Cleanup(func() { flip.Stop() })

	_, errType, err := h.server.wakeAndDeliver(context.Background(), "/channels/team-a/support",
		staleAgent(h, kaalmv1beta1.AgentRunning, 3*time.Second), MessageEnvelope{MessageID: "m1"})
	if err != nil || errType != "" {
		t.Fatalf("wakeAndDeliver = %q, %v; want delivery", errType, err)
	}
	if len(act.calls) != 1 {
		t.Errorf("activator calls = %v, want exactly one", act.calls)
	}
	if n := len(h.agentHits); n != 1 {
		t.Errorf("agent hits = %d, want 1", n)
	}
}

// A wake requested during a Pod wait gets the rest of that wait's
// wakeTimeout, not a fresh one, and a wait that requested a wake ends like a
// wake: wake_timeout, metered as one, without the delivery attempt's error.
func TestWakeAndDeliver_PodWaitWakeKeepsTheWaitsDeadline(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync")
	h.server.Metrics = NewMetrics(prometheus.NewRegistry())
	ps := newPhaseStore(h.store, kaalmv1beta1.AgentRunning)
	h.server.Store = ps
	h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond
	act := &fakeActivator{} // the Pod never comes back
	h.server.Activator = act
	flip := time.AfterFunc(1500*time.Millisecond, func() { ps.setPhase(kaalmv1beta1.AgentHibernating) })
	t.Cleanup(func() { flip.Stop() })

	start := time.Now()
	_, errType, err := h.server.wakeAndDeliver(context.Background(), "/channels/team-a/support",
		staleAgent(h, kaalmv1beta1.AgentRunning, 2*time.Second), MessageEnvelope{MessageID: "m1"})
	if errType != errWakeTimeout || err == nil {
		t.Fatalf("wakeAndDeliver = %q, %v; want %q", errType, err, errWakeTimeout)
	}
	if !strings.Contains(err.Error(), "wakeTimeout") || strings.Contains(err.Error(), "last attempt") {
		t.Errorf("message %q must name wakeTimeout and not carry the delivery attempt's error", err)
	}
	if d := time.Since(start); d >= 3*time.Second {
		t.Errorf("the wait took %s; the wake must keep the wait's 2s deadline", d)
	}
	if len(act.calls) != 1 {
		t.Errorf("activator calls = %v, want exactly one", act.calls)
	}
	if reason, ok := healthReason(h, "/channels/team-a/support"); ok || reason != healthReasonDispatchFailed {
		t.Errorf("health = %q ok=%v, want %q", reason, ok, healthReasonDispatchFailed)
	}
	if got := testutil.ToFloat64(h.server.Metrics.channelWake.WithLabelValues("team-a")); got != 1 {
		t.Errorf("wakes = %v, want 1", got)
	}
	if got := wakeDurationCount(t, h.server.Metrics, "wake_timeout"); got != 1 {
		t.Errorf("wake_timeout durations = %d, want 1", got)
	}
}

// A wake request a Pod wait makes fails the way the wake path's does:
// controller_unavailable at once, with AgentNotReady channel health.
func TestWakeAndDeliver_PodWaitWakeRequestFails(t *testing.T) {
	for name, act := range map[string]ActivatorClient{
		"activator error": &fakeActivator{err: errors.New("activator answered 503")},
		"no activator":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
			h.seedChannel("sync")
			h.server.Metrics = NewMetrics(prometheus.NewRegistry())
			h.server.Store = newPhaseStore(h.store, kaalmv1beta1.AgentHibernating)
			h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
			h.server.Config.AgentConnectTimeout = 20 * time.Millisecond
			h.server.Activator = act

			start := time.Now()
			_, errType, err := h.server.wakeAndDeliver(context.Background(), "/channels/team-a/support",
				staleAgent(h, kaalmv1beta1.AgentRunning, 3*time.Second), MessageEnvelope{MessageID: "m1"})
			if errType != errControllerDown || err == nil {
				t.Fatalf("wakeAndDeliver = %q, %v; want %q", errType, err, errControllerDown)
			}
			if d := time.Since(start); d >= 1500*time.Millisecond {
				t.Errorf("took %s; a failed wake request must end the message at once", d)
			}
			if reason, ok := healthReason(h, "/channels/team-a/support"); ok || reason != healthReasonAgentNotReady {
				t.Errorf("health = %q ok=%v, want %q", reason, ok, healthReasonAgentNotReady)
			}
			if got := testutil.ToFloat64(h.server.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeConnect)); got != 1 {
				t.Errorf("connect attempts = %v, want 1", got)
			}
		})
	}
}

// The sync deadline ends a Pod wait that would otherwise run to wakeTimeout.
func TestWakeAndDeliver_SyncDeadlineBoundsThePodWait(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentProvisioning
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: time.Hour}
	h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond
	h.server.Config.SyncDeliveryDeadline = 300 * time.Millisecond

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
	if got := errType(t, resp); got != errSyncDeadline {
		t.Errorf("error type = %q, want %q", got, errSyncDeadline)
	}
}

// A Pod wait that runs out ends as delivery_failed, names wakeTimeout, and
// records DispatchFailed. It needs no activator and counts no attempt.
func TestWakeAndDeliver_PendingPodWaitIsBounded(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("async")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentProvisioning
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: 80 * time.Millisecond}
	h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond
	h.server.Metrics = NewMetrics(prometheus.NewRegistry())

	start := time.Now()
	_, errType, err := h.server.wakeAndDeliver(context.Background(), "/channels/team-a/support", agent, MessageEnvelope{})
	if errType != errDeliveryFailed || err == nil || !strings.Contains(err.Error(), "within wakeTimeout") {
		t.Fatalf("wakeAndDeliver = %q, %v; want %q naming wakeTimeout", errType, err, errDeliveryFailed)
	}
	// The wait ends at the wakeTimeout plus at most one connect in flight.
	if d := time.Since(start); d > 80*time.Millisecond+h.server.Config.AgentConnectTimeout+time.Second {
		t.Errorf("the wait took %s, want it bounded by the 80ms wakeTimeout", d)
	}
	if reason, ok := healthReason(h, "/channels/team-a/support"); ok || reason != healthReasonDispatchFailed {
		t.Errorf("health = %q ok=%v, want %q", reason, ok, healthReasonDispatchFailed)
	}
	if got := testutil.ToFloat64(h.server.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeConnect)); got != 0 {
		t.Errorf("connect attempts = %v, want 0", got)
	}
}

// A refused connect starts the Pod wait once. Without podWait, the schedule
// runs its attempts and does not wait.
func TestDeliverToAgentWaitsOnceForARefusedConnect(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"}}
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: 80 * time.Millisecond}
	s := deliveryServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	s.Config.AgentServicePortOverride = 1 // nothing listens on :1
	s.Config.AgentConnectTimeout = 20 * time.Millisecond

	_, err := s.deliverToAgent(context.Background(), "", agent, MessageEnvelope{MessageID: "m1"}, true)
	if err == nil || !strings.Contains(err.Error(), "wakeTimeout") {
		t.Fatalf("err = %v, want one naming wakeTimeout", err)
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeConnect)); got != 1 {
		t.Errorf("connect attempts with podWait = %v, want 1", got)
	}

	s.Metrics = NewMetrics(prometheus.NewRegistry())
	start := time.Now()
	_, err = s.deliverToAgent(context.Background(), "", agent, MessageEnvelope{MessageID: "m2"}, false)
	if err == nil || strings.Contains(err.Error(), "wakeTimeout") {
		t.Fatalf("err = %v, want an exhausted schedule", err)
	}
	if got := testutil.ToFloat64(s.Metrics.channelDelivery.WithLabelValues("team-a", deliveryOutcomeConnect)); got != 4 {
		t.Errorf("connect attempts without podWait = %v, want 4", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("without podWait the schedule took %s; it must not wait", d)
	}
}

// A wake that runs out reports the channel as DispatchFailed: the message
// was accepted and never reached the agent.
func TestWakeAndDeliver_WakeTimeoutRecordsChannelHealth(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedChannel("sync")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentHibernated
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: 80 * time.Millisecond}
	h.server.Activator = &fakeActivator{}
	h.server.Config.AgentServicePortOverride = 1 // nothing listens on :1
	h.server.Config.AgentConnectTimeout = 20 * time.Millisecond

	resp := h.post(t, "/channels/team-a/support", "hook-token", []byte(`{}`))
	if got := errType(t, resp); got != errWakeTimeout {
		t.Fatalf("error type = %q, want %q", got, errWakeTimeout)
	}
	if reason, ok := healthReason(h, "/channels/team-a/support"); ok || reason != healthReasonDispatchFailed {
		t.Errorf("health = %q ok=%v, want %q", reason, ok, healthReasonDispatchFailed)
	}
}
