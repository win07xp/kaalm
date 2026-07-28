// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const taskSAN = "t1.default.task.kaalm.io"

var agentSANs = []string{"a1.default.svc.cluster.local"}

// startAgent builds an Agent via New() from a real mount directory and serves
// it on a loopback listener. stop() cancels serving and asserts a clean
// drain; tests that do not call it get it at cleanup.
func startAgent(
	t *testing.T, pki *testPKI, sans []string, memDir, gatewayURL string, h Handler,
) (a *Agent, addr string, stop func()) {
	t.Helper()
	mount := t.TempDir()
	certPEM, keyPEM := pki.issue(t, "workload", sans...)
	writeMount(t, mount, certPEM, keyPEM, pki.caPEM)
	setMountEnv(t, mount)
	t.Setenv("KAALM_MEMORY_DIR", memDir)
	t.Setenv("KAALM_GATEWAY_ENDPOINT", gatewayURL)
	t.Setenv("KAALM_HEALTH_PORT", "0")

	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.serve(ctx, ln, h) }()

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve must drain cleanly on cancellation, got %v", err)
		}
	}
	t.Cleanup(stop)
	return a, "https://" + ln.Addr().String(), stop
}

// mockGateway is a TLS server presenting a test-CA cert, standing in for the
// Kaalm gateway on the runtime's outbound calls.
func mockGateway(t *testing.T, pki *testPKI, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	certPEM, keyPEM := pki.issue(t, "gateway-server", "localhost")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pki.caPEM)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func postMessage(t *testing.T, client *http.Client, addr string, env Envelope) (*http.Response, Response) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(addr+"/v1/message", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out Response
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp, out
}

// The per-path mTLS contract (item 4): probes pass without a certificate,
// /v1/message requires the gateway's identity, and the default handler is
// the deterministic echo.
func TestServe_MTLSMatrixAndEchoDefault(t *testing.T) {
	pki := newTestPKI(t)
	_, addr, _ := startAgent(t, pki, agentSANs, t.TempDir(), "", nil)

	probe := pki.client(t, nil, nil)
	resp, err := probe.Get(addr + "/readyz")
	if err != nil {
		t.Fatalf("readyz over TLS without client cert: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("readyz = %d %q, want 200 ok", resp.StatusCode, body)
	}

	env := Envelope{MessageID: "m1", Content: "hi"}

	if resp, _ := postMessage(t, probe, addr, env); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no client cert must be 401, got %d", resp.StatusCode)
	}

	intruder := pki.clientFor(t, "intruder", "intruder.default.svc.cluster.local")
	if resp, _ := postMessage(t, intruder, addr, env); resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong SAN must be 403, got %d", resp.StatusCode)
	}

	gateway := pki.clientFor(t, "gateway", gatewaySANLocal)
	resp2, reply := postMessage(t, gateway, addr, env)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("gateway SAN must be 200, got %d", resp2.StatusCode)
	}
	if reply.Content != "echo: hi" {
		t.Errorf("default handler reply = %q, want %q", reply.Content, "echo: hi")
	}
}

func TestServe_DedupByMessageID(t *testing.T) {
	pki := newTestPKI(t)
	var calls atomic.Int32
	h := func(_ context.Context, _ Envelope) (Response, error) {
		return Response{Content: fmt.Sprintf("call %d", calls.Add(1))}, nil
	}
	_, addr, _ := startAgent(t, pki, agentSANs, t.TempDir(), "", h)
	gateway := pki.clientFor(t, "gateway", gatewaySANLocal)

	_, first := postMessage(t, gateway, addr, Envelope{MessageID: "m1", Content: "x"})
	_, second := postMessage(t, gateway, addr, Envelope{MessageID: "m1", Content: "x"})
	if first.Content != "call 1" || second.Content != "call 1" {
		t.Errorf("redelivery must return the cached reply: %q then %q", first.Content, second.Content)
	}
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times for one messageId, want 1", calls.Load())
	}

	// An id-less envelope is dispatched every time and never cached: "" is
	// not an identity.
	postMessage(t, gateway, addr, Envelope{Content: "y"})
	postMessage(t, gateway, addr, Envelope{Content: "y"})
	if calls.Load() != 3 {
		t.Errorf("id-less envelopes must each dispatch, handler ran %d times, want 3", calls.Load())
	}
}

// Contract item 7 end to end: a wake-replacement Pod (a fresh process over
// the same volume) recognizes a messageId the previous Pod answered.
func TestServe_DedupSurvivesPodReplacement(t *testing.T) {
	pki := newTestPKI(t)
	memDir := t.TempDir()
	env := Envelope{MessageID: "m-before-hibernate", Content: "hello"}

	_, addr, stop := startAgent(t, pki, agentSANs, memDir, "",
		func(context.Context, Envelope) (Response, error) {
			return Response{Content: "first life"}, nil
		})
	gateway := pki.clientFor(t, "gateway", gatewaySANLocal)
	if _, reply := postMessage(t, gateway, addr, env); reply.Content != "first life" {
		t.Fatalf("first delivery reply = %q", reply.Content)
	}
	stop()

	var secondCalls atomic.Int32
	_, addr2, _ := startAgent(t, pki, agentSANs, memDir, "",
		func(context.Context, Envelope) (Response, error) {
			secondCalls.Add(1)
			return Response{Content: "second life"}, nil
		})
	_, reply := postMessage(t, gateway, addr2, env)
	if reply.Content != "first life" {
		t.Errorf("redelivery after replacement = %q, want the cached %q", reply.Content, "first life")
	}
	if secondCalls.Load() != 0 {
		t.Errorf("the replacement's handler must not run for a recognized id, ran %d times", secondCalls.Load())
	}
}

func TestServe_BadEnvelopeAndHandlerError(t *testing.T) {
	pki := newTestPKI(t)
	h := func(context.Context, Envelope) (Response, error) {
		return Response{}, fmt.Errorf("boom")
	}
	_, addr, _ := startAgent(t, pki, agentSANs, t.TempDir(), "", h)
	gateway := pki.clientFor(t, "gateway", gatewaySANLocal)

	resp, err := gateway.Post(addr+"/v1/message", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed envelope must be 400, got %d", resp.StatusCode)
	}

	resp, _ = postMessage(t, gateway, addr, Envelope{MessageID: "m1"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("handler error must be 500, got %d", resp.StatusCode)
	}
	// A failed handler must not poison the dedup cache: a retry with the
	// same id reaches the handler again instead of a cached error.
	resp, _ = postMessage(t, gateway, addr, Envelope{MessageID: "m1"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("retry after handler error must reach the handler, got %d", resp.StatusCode)
	}
}

// Agent mode: the heartbeat loop posts to the gateway with the workload's
// mTLS identity (contract item 5).
func TestServe_AgentModeHeartbeats(t *testing.T) {
	restore := heartbeatPeriod
	heartbeatPeriod = 30 * time.Millisecond
	defer func() { heartbeatPeriod = restore }()

	pki := newTestPKI(t)
	beats := make(chan string, 16)
	gw := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no client cert", http.StatusUnauthorized)
			return
		}
		select {
		case beats <- r.URL.Path:
		default:
		}
	}))

	startAgent(t, pki, agentSANs, t.TempDir(), gw.URL, nil)

	select {
	case path := <-beats:
		if path != "/v1/agent/heartbeat" {
			t.Errorf("heartbeat path = %q", path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat arrived")
	}
}

// Task mode, detected purely from the certificate SAN shape: no heartbeat,
// and the KAALM_TASK_AUTOCOMPLETE smoke hook reports completion.
func TestServe_TaskModeAutoCompletesAndSkipsHeartbeat(t *testing.T) {
	restoreHB := heartbeatPeriod
	heartbeatPeriod = 20 * time.Millisecond
	defer func() { heartbeatPeriod = restoreHB }()

	pki := newTestPKI(t)
	requests := make(chan string, 16)
	gw := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.URL.Path:
		default:
		}
	}))

	t.Setenv("KAALM_TASK_AUTOCOMPLETE", "success")
	a, _, _ := startAgent(t, pki, []string{taskSAN}, t.TempDir(), gw.URL, nil)
	if !a.IsTask() {
		t.Fatal("task SAN must put the runtime in task mode")
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case path := <-requests:
			if path == "/v1/agent/heartbeat" {
				t.Fatal("task mode must not heartbeat")
			}
			if path == "/v1/task/complete" {
				return // the hook fired; success
			}
		case <-deadline:
			t.Fatal("auto-complete never reached the gateway")
		}
	}
}

func TestGatewaySANMatches(t *testing.T) {
	for san, want := range map[string]bool{
		gatewaySANLocal:                      true,
		gatewaySANShort:                      true,
		"intruder.default.svc.cluster.local": false,
	} {
		cert := &x509.Certificate{DNSNames: []string{san}}
		if got := gatewaySANMatches(cert); got != want {
			t.Errorf("gatewaySANMatches(%s) = %v, want %v", san, got, want)
		}
	}
}

func TestWorkloadIsTask(t *testing.T) {
	pki := newTestPKI(t)
	load := func(certPEM, keyPEM []byte) *tls.Certificate {
		c, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return &c
	}
	if workloadIsTask(nil) {
		t.Error("nil cert must not be task mode")
	}
	if workloadIsTask(load(pki.issue(t, "a", agentSANs...))) {
		t.Error("agent SAN must not be task mode")
	}
	if !workloadIsTask(load(pki.issue(t, "t", taskSAN))) {
		t.Error("task SAN must be task mode")
	}
}

func TestShouldHeartbeat(t *testing.T) {
	agentMode := &Agent{Gateway: &Gateway{endpoint: "https://gw"}}
	taskMode := &Agent{Gateway: &Gateway{endpoint: "https://gw"}, isTask: true}

	t.Setenv("KAALM_TEMPLATE_HEARTBEAT", "auto")
	if !agentMode.shouldHeartbeat() {
		t.Error("agent mode auto must heartbeat")
	}
	if taskMode.shouldHeartbeat() {
		t.Error("task mode auto must not heartbeat")
	}
	t.Setenv("KAALM_TEMPLATE_HEARTBEAT", "off")
	if agentMode.shouldHeartbeat() {
		t.Error("off must never heartbeat")
	}
	t.Setenv("KAALM_TEMPLATE_HEARTBEAT", "auto")
	if (&Agent{Gateway: &Gateway{}}).shouldHeartbeat() {
		t.Error("no gateway endpoint means nothing to heartbeat")
	}
}

func TestTaskAutocompleteStatus(t *testing.T) {
	if got := taskAutocompleteStatus(false, "success"); got != "" {
		t.Errorf("agent mode must never auto-complete, got %q", got)
	}
	if got := taskAutocompleteStatus(true, ""); got != "" {
		t.Errorf("unset env must not auto-complete, got %q", got)
	}
	if got := taskAutocompleteStatus(true, "success"); got != "success" {
		t.Errorf("task mode with env must return the status, got %q", got)
	}
}
