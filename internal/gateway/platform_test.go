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
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func TestTruncatedReplyBody(t *testing.T) {
	if got := truncatedReplyBody([]byte("  {\"code\": 50027}  ")); got != `{"code": 50027}` {
		t.Errorf("short body = %q", got)
	}
	long := strings.Repeat("x", 64<<10)
	got := truncatedReplyBody([]byte(long))
	if len(got) > maxReplyRefusalDetail+len("... (truncated)") {
		t.Errorf("truncated body still %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "... (truncated)") {
		t.Errorf("truncation marker missing: %q", got[len(got)-32:])
	}
	// Arbitrary bytes must reduce to valid UTF-8 for etcd.
	if got := truncatedReplyBody([]byte{0xff, 0xfe, 'o', 'k'}); !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 survived: %q", got)
	}
	// A truncation cut mid-rune must not leave a partial encoding behind.
	runes := strings.Repeat("\u20ac", maxReplyRefusalDetail) // 3 bytes each; the cut lands mid-rune
	if got := truncatedReplyBody([]byte(runes)); !utf8.ValidString(got) {
		t.Errorf("mid-rune cut produced invalid UTF-8")
	}
}

// eventCapture records Eventf calls for assertions.
type eventCapture struct {
	mu       sync.Mutex
	messages []string
	reasons  []string
	objects  []runtime.Object
}

func (c *eventCapture) Eventf(object runtime.Object, _, reason, messageFmt string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, fmt.Sprintf(messageFmt, args...))
	c.reasons = append(c.reasons, reason)
	c.objects = append(c.objects, object)
}

// TestReplyRefused_BoundsEventDetail: a 64 KiB platform error body must not
// travel into the Warning event or the health observation.
func TestReplyRefused_BoundsEventDetail(t *testing.T) {
	capture := &eventCapture{}
	s := &Server{ChannelHealth: NewChannelHealthStore(0), Recorder: capture}
	channel := &kaalmv1beta1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "disc", Namespace: "team-a"},
		Spec: kaalmv1beta1.AgentChannelSpec{
			Type:    kaalmv1beta1.ChannelTypeDiscord,
			Discord: &kaalmv1beta1.AgentChannelDiscord{Path: "/channels/team-a/disc"},
		},
	}
	huge := []byte(strings.Repeat("z", 64<<10))
	outcome := s.replyRefused(channel, "discord", replyResult{bucket: bucketTerminal, status: 400, body: huge})
	if outcome != callbackRejected {
		t.Fatalf("outcome = %q", outcome)
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.messages) != 1 {
		t.Fatalf("events = %d, want 1", len(capture.messages))
	}
	if len(capture.messages[0]) > 1024 {
		t.Errorf("event detail is %d bytes; the platform body was not truncated", len(capture.messages[0]))
	}

	s.ChannelHealth.mu.Lock()
	obs := s.ChannelHealth.observations[channel.Spec.Path()]
	s.ChannelHealth.mu.Unlock()
	if len(obs) != 1 {
		t.Fatalf("observations = %d, want 1", len(obs))
	}
	if len(obs[0].lastError) > 1024 {
		t.Errorf("health detail is %d bytes; the platform body was not truncated", len(obs[0].lastError))
	}
}

// A platform reply never carries transport detail: the activator's error
// names the controller Service and a Pod IP.
func TestPlatformErrorMessage_NoTransportDetail(t *testing.T) {
	leak := errors.New(`Post "https://kaalm-controller.kaalm-system.svc.cluster.local:9443/v1/activate/team-a/a": dial tcp 10.42.0.7:9443: connect: connection refused`)
	for _, errType := range []string{errControllerDown, errResponseTooLarge, errDeliveryFailed} {
		if got := platformErrorMessage(errType, leak); strings.Contains(got, "10.42.0.7") || strings.Contains(got, "svc.cluster.local") {
			t.Errorf("%s message leaks transport detail: %q", errType, got)
		}
	}
	wake := errors.New("agent did not become ready within wakeTimeout (2m0s)")
	if got := platformErrorMessage(errWakeTimeout, wake); got != wake.Error() {
		t.Errorf("wake_timeout message = %q, want the wakeTimeout text", got)
	}
}

// A failed reply's error never carries the request URL: a Discord reply URL
// holds the interaction token, and the error is written to channel health
// and an Event.
func TestWithoutRequestURL_DropsTheToken(t *testing.T) {
	_, err := http.Get("https://127.0.0.1:1/api/v10/webhooks/app/SECRET-INTERACTION-TOKEN/messages/@original")
	if err == nil {
		t.Skip("port 1 answered")
	}
	got := withoutRequestURL(err).Error()
	if strings.Contains(got, "SECRET-INTERACTION-TOKEN") || strings.Contains(got, "/webhooks/") {
		t.Errorf("error keeps the URL: %q", got)
	}
	if !strings.Contains(got, "Get request failed") {
		t.Errorf("error lost its operation: %q", got)
	}
}

// deadlineAdapter is a platform adapter that records the deadline its reply
// runs under and how many reply requests the pipeline was told to budget.
type deadlineAdapter struct {
	requests int
	text     string
	deadline time.Time
	bounded  bool
}

func (d *deadlineAdapter) Type() string { return "stub" }
func (d *deadlineAdapter) Handle(context.Context, http.ResponseWriter, *http.Request,
	*kaalmv1beta1.AgentChannel, []byte) inboundResult {
	return inboundResult{}
}
func (d *deadlineAdapter) ReplyRequests(text string) int {
	d.text = text
	return d.requests
}
func (d *deadlineAdapter) SendReply(ctx context.Context, _ *kaalmv1beta1.AgentChannel, _ platformMessage, _ string) string {
	d.deadline, d.bounded = ctx.Deadline()
	return callbackDelivered
}

func TestRunPlatformPipeline_BoundCoversLargeWakeTimeout(t *testing.T) {
	h := newUserHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	channel := h.seedChannel("async")
	agent := h.store.agents["team-a/sup"]
	agent.Status.Phase = kaalmv1beta1.AgentHibernated
	// Far above the old fixed 10-minute ceiling. The activator fails at once,
	// so nothing waits this long.
	agent.Spec.Lifecycle.WakeTimeout = metav1.Duration{Duration: 20 * time.Minute}
	act := &deadlineActivator{}
	h.server.Activator = act
	adapter := &deadlineAdapter{requests: 3}

	start := time.Now()
	h.server.runPlatformPipeline(context.Background(), channel, agent, adapter, platformMessage{})

	if !act.bounded || !adapter.bounded {
		t.Fatalf("pipeline runs without a deadline: wake %v, reply %v", act.bounded, adapter.bounded)
	}
	if !strings.HasPrefix(adapter.text, errControllerDown) {
		t.Errorf("reply requests were counted for %q, want the rendered error", adapter.text)
	}
	// Wake and delivery run under the bound without the reply leg; the reply
	// runs under the full bound, with one schedule per reply request.
	checks := []struct {
		name     string
		deadline time.Time
		want     time.Duration
	}{
		{"wake", act.deadline, h.server.backgroundPipelineBound(agent, 0)},
		{"reply", adapter.deadline, h.server.backgroundPipelineBound(agent, 3)},
	}
	for _, c := range checks {
		got := c.deadline.Sub(start)
		if got < c.want-time.Second || got > c.want+time.Second {
			t.Errorf("%s deadline = %s after start, want %s", c.name, got, c.want)
		}
		if got <= 20*time.Minute {
			t.Errorf("%s deadline %s cuts off the 20m wakeTimeout", c.name, got)
		}
	}
}

func TestPlatformClient_UsesCallbackReadTimeout(t *testing.T) {
	s := &Server{Config: Config{AgentReadTimeout: 3 * time.Second, CallbackReadTimeout: 7 * time.Second}}
	client, err := s.platformClient()
	if err != nil {
		t.Fatalf("platformClient: %v", err)
	}
	if client.Timeout != 7*time.Second {
		t.Errorf("platform request bound = %s, want gateway.callbackReadTimeout (7s)", client.Timeout)
	}
}

// countingTLSServer starts a TLS server that counts the connections it
// opens and closes, and a Server whose callback trust pool holds its
// certificate.
func countingTLSServer(t *testing.T) (*httptest.Server, *Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var opened, closed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			opened.Add(1)
		case http.StateClosed:
			closed.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	s := &Server{Config: Config{CallbackCAs: pool, CallbackReadTimeout: 5 * time.Second, CallbackBackoff: []time.Duration{}}}
	return srv, s, &opened, &closed
}

func sendTestReply(t *testing.T, s *Server, url string) {
	t.Helper()
	res := s.sendPlatformRequest(context.Background(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	}, func(status int, _ []byte) replyBucket { return classifyReplyStatus(status) })
	if res.bucket != bucketDelivered {
		t.Fatalf("reply bucket = %v (status %d, err %v), want delivered", res.bucket, res.status, res.err)
	}
}

// Replies share one pooled client, so a run of replies to one platform
// reuses one connection instead of a TLS handshake and a leaked transport
// per reply.
func TestSendPlatformRequest_ReusesTheConnection(t *testing.T) {
	srv, s, opened, _ := countingTLSServer(t)
	for range 3 {
		sendTestReply(t, s, srv.URL)
	}
	if got := opened.Load(); got != 1 {
		t.Errorf("connections opened for 3 replies = %d, want 1", got)
	}
}

func TestPlatformClient_RebuildsOnlyWhenTheTrustPoolChanges(t *testing.T) {
	srv, s, _, closed := countingTLSServer(t)
	first, err := s.platformClient()
	if err != nil {
		t.Fatalf("platformClient: %v", err)
	}
	again, err := s.platformClient()
	if err != nil {
		t.Fatalf("platformClient: %v", err)
	}
	if first != again {
		t.Fatal("platformClient built a new client for an unchanged trust pool")
	}
	sendTestReply(t, s, srv.URL) // leaves one idle connection in first's pool

	rotated := x509.NewCertPool()
	rotated.AddCert(srv.Certificate())
	s.Config.CallbackCAs = rotated
	next, err := s.platformClient()
	if err != nil {
		t.Fatalf("platformClient: %v", err)
	}
	if next == first {
		t.Fatal("platformClient kept the old client after the trust pool changed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for closed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the old client's idle connection was not closed after the rebuild")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The shutdown sequence waits on Server.pipelines, so a platform message
// must count as pending work until its reply is sent.
func TestPlatformPipeline_IsPendingUntilReplied(t *testing.T) {
	release := make(chan struct{})
	h := newDiscordHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"done"}`))
	})
	// Unblock the agent on a failed assertion too, so its server can close.
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	resp := h.send(t, discordCommand("123456789012345678", "987654321098765432", "555555555555555555", "hi"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status %d", resp.StatusCode)
	}
	<-h.agentHits

	done := make(chan struct{})
	go func() {
		h.server.pipelines.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the pipeline is not tracked as pending while the agent runs")
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	h.fake.next(t)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pending count never dropped after the reply")
	}
}
