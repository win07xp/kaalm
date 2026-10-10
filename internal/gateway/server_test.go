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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/tlsutil"
)

// ---- test PKI ----

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t testing.TB) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kaalm-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// issue signs a leaf with the given DNS SANs and returns a tls.Certificate.
func (ca *testCA) issue(t testing.TB, sans ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: sans[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     sans,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// ---- fakes ----

type fakeStore struct {
	agents    map[string]*kaalmv1beta1.Agent
	tasks     map[string]*kaalmv1beta1.AgentTask
	classes   map[string]*kaalmv1beta1.AgentClass
	providers map[string]*kaalmv1beta1.ModelProvider
	creds     map[string]string
	podsByIP  map[string]*corev1.Pod
	// livePodsByIP backs PodByIPLive: Pods visible to the live fallback but
	// not the informer, the new-Pod window.
	livePodsByIP map[string]*corev1.Pod
	channels     map[string]*kaalmv1beta1.AgentChannel // key: webhook path
	secrets      map[string]string                     // key: ns/name/key
	// callbackHosts backs CallbackSecretValue: the approved hosts of each
	// Secret (key: ns/name), as the kaalm.io/callback-hosts annotation.
	callbackHosts map[string]string
	// callbackChecks counts CallbackSecretValue calls; refuseCallbackAfter,
	// when positive, refuses every call after that many.
	callbackChecks      int
	refuseCallbackAfter int

	toolProviders map[string]*kaalmv1beta1.ToolProvider
	toolCreds     map[string]string

	// podByIPCalls counts PodByIP calls; podByIPMisses, when positive,
	// makes that many calls miss first, the informer-lag window.
	podByIPCalls  atomic.Int64
	podByIPMisses atomic.Int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		agents:       map[string]*kaalmv1beta1.Agent{},
		tasks:        map[string]*kaalmv1beta1.AgentTask{},
		classes:      map[string]*kaalmv1beta1.AgentClass{},
		providers:    map[string]*kaalmv1beta1.ModelProvider{},
		creds:        map[string]string{},
		podsByIP:     map[string]*corev1.Pod{},
		livePodsByIP: map[string]*corev1.Pod{},
		channels:     map[string]*kaalmv1beta1.AgentChannel{},
		secrets:      map[string]string{},

		callbackHosts: map[string]string{},

		toolProviders: map[string]*kaalmv1beta1.ToolProvider{},
		toolCreds:     map[string]string{},
	}
}

func (f *fakeStore) ToolProviderByName(_ context.Context, name string) (*kaalmv1beta1.ToolProvider, bool) {
	tp, ok := f.toolProviders[name]
	return tp, ok
}

func (f *fakeStore) ToolCredential(_ context.Context, tp *kaalmv1beta1.ToolProvider) (string, error) {
	if tp.Spec.CredentialsRef == nil {
		return "", nil
	}
	cred, ok := f.toolCreds[tp.Name]
	if !ok {
		return "", fmt.Errorf("no credential for %s", tp.Name)
	}
	return cred, nil
}

func (f *fakeStore) AgentByName(_ context.Context, ns, name string) (*kaalmv1beta1.Agent, bool) {
	a, ok := f.agents[ns+"/"+name]
	return a, ok
}
func (f *fakeStore) TaskByName(_ context.Context, ns, name string) (*kaalmv1beta1.AgentTask, bool) {
	tk, ok := f.tasks[ns+"/"+name]
	return tk, ok
}
func (f *fakeStore) ClassByName(_ context.Context, name string) (*kaalmv1beta1.AgentClass, bool) {
	c, ok := f.classes[name]
	return c, ok
}
func (f *fakeStore) ProviderByName(_ context.Context, name string) (*kaalmv1beta1.ModelProvider, bool) {
	p, ok := f.providers[name]
	return p, ok
}
func (f *fakeStore) Credential(_ context.Context, p *kaalmv1beta1.ModelProvider) (string, error) {
	cred, ok := f.creds[p.Name]
	if !ok {
		return "", fmt.Errorf("no credential for %s", p.Name)
	}
	return cred, nil
}
func (f *fakeStore) PodByIP(_ context.Context, ip string) (*corev1.Pod, bool) {
	f.podByIPCalls.Add(1)
	if f.podByIPMisses.Add(-1) >= 0 {
		return nil, false
	}
	p, ok := f.podsByIP[ip]
	return p, ok
}

// PodByIPLive serves livePodsByIP, narrowed to namespace like production.
func (f *fakeStore) PodByIPLive(_ context.Context, namespace, ip string) (*corev1.Pod, bool) {
	p, ok := f.livePodsByIP[ip]
	if !ok || p.Namespace != namespace {
		return nil, false
	}
	return p, true
}
func (f *fakeStore) ChannelByPath(_ context.Context, path string) (*kaalmv1beta1.AgentChannel, bool) {
	ch, ok := f.channels[path]
	return ch, ok
}
func (f *fakeStore) SecretValue(_ context.Context, ns, name, key string) (string, error) {
	v, ok := f.secrets[ns+"/"+name+"/"+key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s key %s not found", ns, name, key)
	}
	return v, nil
}

// CallbackSecretValue applies rule 46 over callbackHosts, then SecretValue.
func (f *fakeStore) CallbackSecretValue(ctx context.Context, ns, name, key, host string) (string, error) {
	f.callbackChecks++
	if f.refuseCallbackAfter > 0 && f.callbackChecks > f.refuseCallbackAfter {
		return "", fmt.Errorf("secret %s/%s no longer approves host %s", ns, name, host)
	}
	annotations := map[string]string{kaalmv1beta1.AnnotationCallbackHosts: f.callbackHosts[ns+"/"+name]}
	if !kaalmv1beta1.CallbackHostApproved(annotations, host) {
		return "", fmt.Errorf("secret %s/%s does not approve host %s", ns, name, host)
	}
	return f.SecretValue(ctx, ns, name, key)
}

// ---- harness ----

type harness struct {
	ca       *testCA
	store    *fakeStore
	reviewer *fakeReviewer
	spend    *MemorySpend
	server   *Server
	listener net.Listener
	upstream *httptest.Server
	upreqs   chan *capturedRequest
}

type capturedRequest struct {
	header http.Header
	body   map[string]any
	raw    []byte
}

// newHarness starts a real TLS gateway listener and a TLS upstream. upstreamFn
// handles provider requests after capture.
func newHarness(t testing.TB, upstreamFn http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{ca: newTestCA(t), store: newFakeStore(), spend: NewMemorySpend(), upreqs: make(chan *capturedRequest, 8)}

	// Registered first so it runs last, after the gateway listener and the
	// upstream close: http.Server.Close does not wait for running handlers,
	// and a gateway handler that outlives its test writes its audit line and
	// metrics into the next one.
	var inflight atomic.Int64
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for inflight.Load() > 0 {
			if time.Now().After(deadline) {
				t.Errorf("%d gateway handlers still running 10s after the test", inflight.Load())
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	h.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		h.upreqs <- &capturedRequest{header: r.Header.Clone(), body: body, raw: raw}
		upstreamFn(w, r)
	}))
	t.Cleanup(h.upstream.Close)
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(h.upstream.Certificate())

	h.reviewer = &fakeReviewer{}
	cfg := Config{
		OperatorNamespace: "kaalm-system",
		MaxBodyBytes:      1 << 20,
		UpstreamTimeout:   10 * time.Second,
		UpstreamCAs:       upstreamPool,
		SessionKey:        []byte("test-session-key"),
	}
	h.server = NewServer(cfg, h.store, NewTokenAuthenticator(h.reviewer), h.spend)
	// Registry-backed metrics so every harness test also exercises the
	// emission paths; per-harness registries keep tests independent.
	h.server.Metrics = NewMetrics(prometheus.NewRegistry())

	serverCert := h.ca.issue(t, "kaalm-gateway.kaalm-system.svc.cluster.local", "localhost")
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    h.ca.pool,
		Certificates: []tls.Certificate{serverCert},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	h.listener = ln
	handler := h.server.Handler()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		handler.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return h
}

func (h *harness) url(path string) string { return "https://" + h.listener.Addr().String() + path }

// client builds an HTTPS client, optionally presenting a client cert.
func (h *harness) client(clientCert *tls.Certificate) *http.Client {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: h.ca.pool, ServerName: "localhost"}
	if clientCert != nil {
		tlsCfg.Certificates = []tls.Certificate{*clientCert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}, Timeout: 10 * time.Second}
}

// seedRoute installs an agent in team-a wired to provider "prov" offering
// model "m1", with the source IP mapped to a matching Pod.
func (h *harness) seedRoute() {
	h.store.agents["team-a/sup"] = &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sup", Namespace: "team-a"},
		Spec: kaalmv1beta1.AgentSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "std"},
			Providers: []kaalmv1beta1.AgentProviderReference{
				{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "prov"}},
			},
		},
	}
	h.store.classes["std"] = &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "std"},
		Spec: kaalmv1beta1.AgentClassSpec{
			AllowedProviders: []kaalmv1beta1.LocalObjectReference{{Name: "prov"}},
		},
	}
	h.store.providers["prov"] = &kaalmv1beta1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prov"},
		Spec: kaalmv1beta1.ModelProviderSpec{
			Type:              "openai",
			Endpoint:          h.upstream.URL,
			AllowedNamespaces: []string{"team-*"},
			Models:            []kaalmv1beta1.ModelProviderModel{{ID: "m1"}},
		},
	}
	h.store.creds["prov"] = "sk-test-cred"
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sup-abc", Namespace: "team-a"},
	}
}

func postJSON(t testing.TB, c *http.Client, url string, body map[string]any, headers map[string]string) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func errType(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var envelope struct {
		Error errorBody `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	return envelope.Error.Type
}

func agentCert(t testing.TB, ca *testCA) tls.Certificate {
	return ca.issue(t, "sup.team-a.svc.cluster.local", "sup.team-a.svc", "sup.team-a")
}

// ---- tests ----

func TestProxy_MTLSHappyPath(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","usage":{"prompt_tokens":30,"completion_tokens":7}}`))
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "messages": []any{}},
		map[string]string{"x-api-key": "attacker-supplied", "Connection": "X-Sneaky", "X-Sneaky": "1", "TE": "trailers"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	up := <-h.upreqs
	// Forwarded-header contract.
	if got := up.header.Get("Authorization"); got != "Bearer sk-test-cred" {
		t.Errorf("credential not injected: %q", got)
	}
	if up.header.Get("X-Api-Key") != "" {
		t.Error("inbound x-api-key must be stripped")
	}
	if up.header.Get("X-Sneaky") != "" || up.header.Get("TE") != "" {
		t.Error("hop-by-hop headers must be dropped")
	}
	if got := up.header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding must be pinned to identity, got %q", got)
	}
	// Provider prefix stripped from the model name.
	if up.body["model"] != "m1" {
		t.Errorf("model not rewritten: %v", up.body["model"])
	}
	// Spend recorded from the buffered usage object.
	if u := h.spend.Total("team-a", "prov", "m1"); u.InputTokens != 30 || u.OutputTokens != 7 {
		t.Errorf("spend not recorded: %+v", u)
	}
}

func TestProxy_AnthropicCredentialHeader(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":5,"output_tokens":2}}`))
	})
	h.seedRoute()
	h.store.providers["prov"].Spec.Type = providerTypeAnthropic
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/messages"),
		map[string]any{"model": "prov/m1"}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	up := <-h.upreqs
	if up.header.Get("X-Api-Key") != "sk-test-cred" {
		t.Error("anthropic credential must be injected as x-api-key")
	}
	if up.header.Get("Authorization") != "" {
		t.Error("no Authorization header expected for anthropic")
	}
	if u := h.spend.Total("team-a", "prov", "m1"); u.InputTokens != 5 {
		t.Errorf("anthropic usage not recorded: %+v", u)
	}
}

// A provider-side tool reported in the response usage lands on the
// kaalm_llm_server_tool_use_total counter next to the token metrics
// (docs/src/gateways/tool-plane.md#provider-side-tools).
func TestProxy_ServerToolUseMetric(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":6039,"output_tokens":931,` +
			`"server_tool_use":{"web_search_requests":2}}}`))
	})
	h.seedRoute()
	h.store.providers["prov"].Spec.Type = providerTypeAnthropic
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/messages"),
		map[string]any{"model": "prov/m1"}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := testutil.ToFloat64(h.server.Metrics.llmServerTools.WithLabelValues("prov", "team-a", "web_search")); got != 2 {
		t.Errorf("server tool use = %v, want 2", got)
	}
	// Token accounting is unaffected by the extra usage facet.
	if u := h.spend.Total("team-a", "prov", "m1"); u.InputTokens != 6039 || u.OutputTokens != 931 {
		t.Errorf("tokens not recorded alongside server tools: %+v", u)
	}
}

func TestProxy_BearerTierHappyPathAndPrecheck(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	h.seedRoute()
	// Bearer caller: a plain Deployment pod in team-b.
	h.reviewer.username = "system:serviceaccount:team-b:runner"
	h.reviewer.authenticated = true
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "team-b"},
	}

	resp := postJSON(t, h.client(nil), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"},
		map[string]string{"Authorization": "Bearer projected-token"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("bearer tier status %d: %s", resp.StatusCode, body)
	}
	up := <-h.upreqs
	// The caller's bearer token must never reach the provider.
	if got := up.header.Get("Authorization"); got != "Bearer sk-test-cred" {
		t.Errorf("provider credential expected, got %q", got)
	}

	// Precheck: an Kaalm-managed Pod cannot use its SA token.
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sup-abc", Namespace: "team-a",
			Labels: map[string]string{"kaalm.io/workload": "agent"},
		},
	}
	before := h.reviewer.calls
	resp = postJSON(t, h.client(nil), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1"},
		map[string]string{"Authorization": "Bearer projected-token"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("precheck should reject with 401, got %d", resp.StatusCode)
	}
	if got := errType(t, resp); got != errUnauthorized {
		t.Errorf("error type %q", got)
	}
	if h.reviewer.calls != before {
		t.Error("precheck must run BEFORE TokenReview; reviewer was called")
	}
}

func TestProxy_TenancyDenialsInOrder(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	cert := agentCert(t, h.ca)
	c := h.client(&cert)

	// Provider not in the workload's spec.providers.
	h.store.providers["other"] = h.store.providers["prov"]
	resp := postJSON(t, c, h.url("/v1/chat/completions"), map[string]any{"model": "other/m1"}, nil)
	if resp.StatusCode != 403 || errType(t, resp) != errAccessDenied {
		t.Error("provider outside spec.providers must be 403 access_denied")
	}

	// Namespace denial comes before model existence: the unknown model must
	// NOT leak through the error type.
	h.store.providers["prov"].Spec.AllowedNamespaces = []string{"prod-only"}
	resp = postJSON(t, c, h.url("/v1/chat/completions"), map[string]any{"model": "prov/does-not-exist"}, nil)
	if resp.StatusCode != 403 || errType(t, resp) != errAccessDenied {
		t.Error("namespace denial must fire before the model check")
	}
	h.store.providers["prov"].Spec.AllowedNamespaces = []string{"team-*"}

	// Unknown model with an allowed namespace: 400.
	resp = postJSON(t, c, h.url("/v1/chat/completions"), map[string]any{"model": "prov/nope"}, nil)
	if resp.StatusCode != 400 || errType(t, resp) != errInvalidRequest {
		t.Error("unknown model must be 400 invalid_request")
	}

	// Missing provider prefix: 400.
	resp = postJSON(t, c, h.url("/v1/chat/completions"), map[string]any{"model": "m1"}, nil)
	if resp.StatusCode != 400 || errType(t, resp) != errInvalidRequest {
		t.Error("unqualified model must be 400")
	}
}

func TestAuthMatrix(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	taskCert := h.ca.issue(t, "fix-42.team-a.task.kaalm.io")
	controllerCert := h.ca.issue(t, "kaalm-controller.kaalm-system.svc.cluster.local")
	consoleCert := h.ca.issue(t, "kaalm-console.kaalm-system.svc.cluster.local")
	gatewayShapedCert := h.ca.issue(t, "something.kaalm-system.svc") // valid CA, no recognized SAN
	agentC := agentCert(t, h.ca)
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}

	// sys marks a case whose source IP resolves to an operator-namespace
	// Pod, as the controller and console call from; every other case
	// resolves to the team-a Pod.
	cases := []struct {
		name       string
		cert       *tls.Certificate
		path       string
		wantStatus int
		sys        bool
	}{
		{"no cert no token on LLM path", nil, "/v1/chat/completions", 401, false},
		{"unrecognized SAN on LLM path", &gatewayShapedCert, "/v1/chat/completions", 403, false},
		{"agent cert on task-complete", &agentC, "/v1/task/complete", 403, false},
		{"task cert on heartbeat", &taskCert, "/v1/agent/heartbeat", 403, false},
		{"agent cert on controller path", &agentC, "/v1/activity", 403, false},
		{"no cert on controller path", nil, "/v1/activity", 401, false},
		// 405 = past auth into the handler (POST on a GET path).
		{"controller cert on activity passes auth", &controllerCert, "/v1/activity", 405, true},
		{"controller cert on channels-health passes auth", &controllerCert, "/v1/channels/health", 405, true},
		// 401 = the source IP is a tenant Pod, not an operator-namespace Pod.
		{"controller cert from a tenant Pod", &controllerCert, "/v1/activity", 401, false},
		{"console cert from a tenant Pod", &consoleCert, "/v1/test-chat", 401, false},
		// 400 = past auth into the handler (empty request body).
		{"console cert on test-chat passes auth", &consoleCert, "/v1/test-chat", 400, true},
		{"controller cert on test-chat", &controllerCert, "/v1/test-chat", 403, true},
		{"console cert on controller path", &consoleCert, "/v1/activity", 403, true},
		{"agent cert on test-chat", &agentC, "/v1/test-chat", 403, false},
		{"no cert on test-chat", nil, "/v1/test-chat", 401, false},
		{"task cert on task-complete reaches handler", &taskCert, "/v1/task/complete", 403, false},
		{"agent cert on heartbeat passes auth", &agentC, "/v1/agent/heartbeat", 200, false},
		{"unknown path", &agentC, "/v2/other", 400, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ns := "team-a"
			if c.sys {
				ns = "kaalm-system"
			}
			h.store.podsByIP["127.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns}}
			resp := postJSON(t, h.client(c.cert), h.url(c.path), map[string]any{}, nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != c.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status %d want %d (%s)", resp.StatusCode, c.wantStatus, body)
			}
		})
	}
}

func TestProxy_SourceIPCrossCheck(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	// The Pod at the source IP is in a DIFFERENT namespace than the SAN claims.
	h.store.podsByIP["127.0.0.1"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-z"}}
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("cross-check mismatch must be 401, got %d", resp.StatusCode)
	}
	if got := errType(t, resp); got != errUnauthorized {
		t.Errorf("error type %q", got)
	}
}

func TestProxy_StreamingRelay(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hel"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	h.seedRoute()
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "stream": true}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	relayed, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(relayed), `"content":"hel"`) || !strings.Contains(string(relayed), "[DONE]") {
		t.Errorf("stream not relayed verbatim: %q", relayed)
	}

	// The include_usage fixup landed on the upstream request.
	up := <-h.upreqs
	if _, ok := up.body["stream_options"]; !ok {
		t.Error("stream_options not injected into the upstream streaming request")
	}
	// Usage folded out of the stream. relayStream flushes each SSE event to the
	// client and records spend only after the scanner loop ends, so io.ReadAll
	// above can return while the server is still between its final Flush and
	// Spend.Record. Poll for the record instead of asserting instantly.
	// MemorySpend guards Record/Total with a mutex, so this read is race-free.
	var u Usage
	for i := 0; i < 100; i++ {
		if u = h.spend.Total("team-a", "prov", "m1"); !u.isZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if u.InputTokens != 12 || u.OutputTokens != 3 {
		t.Errorf("stream usage not recorded: %+v", u)
	}
}

// streamTimeout is the UpstreamTimeout the stream-bound tests run under: long
// enough that a steady 50ms chunk cadence never trips it, short enough that a
// stall ends the test quickly.
const streamTimeout = 200 * time.Millisecond

// writeSSE writes SSE lines, each followed by a blank line, and flushes.
func writeSSE(w http.ResponseWriter, lines ...string) {
	for _, line := range lines {
		_, _ = w.Write([]byte(line + "\n\n"))
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// stall blocks an upstream handler until the gateway gives up on the
// attempt (the connection closes) or a safety bound passes.
func stall(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

// waitUsage polls for recorded spend: the relay settles after its last
// flush, so the client can finish reading first.
func waitUsage(t *testing.T, h *harness, provider, model string) Usage {
	t.Helper()
	var u Usage
	for i := 0; i < 100; i++ {
		if u = h.spend.Total("team-a", provider, model); !u.isZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return u
}

func TestProxy_StreamOutlivesTheUpstreamTimeout(t *testing.T) {
	// Ten chunks 50ms apart run 500ms, past a 200ms bound: the bound is idle
	// time between chunks, not the length of the call.
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 10; i++ {
			writeSSE(w, `data: {"choices":[{"delta":{"content":"x"}}]}`)
			time.Sleep(50 * time.Millisecond)
		}
		writeSSE(w, `data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":10}}`, `data: [DONE]`)
	})
	h.seedRoute()
	h.server.Config.UpstreamTimeout = streamTimeout
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "stream": true}, nil)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if resp.StatusCode != 200 || !strings.Contains(text, "[DONE]") || strings.Contains(text, `"error"`) {
		t.Fatalf("status %d, stream cut or errored:\n%s", resp.StatusCode, text)
	}
	if u := waitUsage(t, h, "prov", "m1"); u.InputTokens != 4 || u.OutputTokens != 10 {
		t.Errorf("usage not settled: %+v", u)
	}
}

func TestProxy_StreamStallEndsWithAnOpenAIErrorEvent(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `data: {"choices":[{"delta":{"content":"hel"}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1}}`)
		stall(r)
	})
	h.seedRoute()
	h.server.Config.UpstreamTimeout = streamTimeout
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "stream": true}, nil)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if resp.StatusCode != 200 || !strings.Contains(text, `"content":"hel"`) {
		t.Fatalf("status %d, partial stream not relayed:\n%s", resp.StatusCode, text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Errorf("a truncated stream must not end with [DONE]:\n%s", text)
	}
	want := `data: {"error":{"type":"provider_timeout",`
	if !strings.Contains(text, want) || !strings.Contains(text, `"provider":"prov"`) || !strings.HasSuffix(text, "}}\n\n") {
		t.Errorf("stream does not end with the OpenAI error event %q:\n%s", want, text)
	}
	if u := waitUsage(t, h, "prov", "m1"); u.InputTokens != 9 || u.OutputTokens != 1 {
		t.Errorf("partial usage not settled: %+v", u)
	}
}

func TestProxy_StreamStallEndsWithAnAnthropicErrorEvent(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, "event: message_start\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`)
		stall(r)
	})
	h.seedRoute()
	h.store.providers["prov"].Spec.Type = "anthropic"
	h.server.Config.UpstreamTimeout = streamTimeout
	cert := agentCert(t, h.ca)

	resp := postJSON(t, h.client(&cert), h.url("/v1/messages"),
		map[string]any{"model": "prov/m1", "stream": true, "max_tokens": 16}, nil)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	want := "event: error\n" + `data: {"type":"error","error":{"type":"provider_timeout","message":`
	if resp.StatusCode != 200 || !strings.Contains(text, want) || !strings.HasSuffix(text, "}}\n\n") {
		t.Errorf("status %d, stream does not end with the Anthropic error event:\n%s", resp.StatusCode, text)
	}
	if u := waitUsage(t, h, "prov", "m1"); u.InputTokens != 7 {
		t.Errorf("partial usage not settled: %+v", u)
	}
}

func TestProxy_BufferedBodyStallIsBounded(t *testing.T) {
	// Headers arrive, then the JSON body stops: the timer bounds the read and
	// the attempt fails as a timeout (no fallback here, so 504).
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1",`))
		w.(http.Flusher).Flush()
		stall(r)
	})
	h.seedRoute()
	h.server.Config.UpstreamTimeout = streamTimeout
	cert := agentCert(t, h.ca)

	start := time.Now()
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("stalled body = %d, want 504", resp.StatusCode)
	}
	if got := errType(t, resp); got != errProviderTimeout {
		t.Errorf("error type %q", got)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("stalled body took %v, want about one %v bound", took, streamTimeout)
	}
}

func TestUpstreamClientHasNoWholeCallTimeout(t *testing.T) {
	s := &Server{Config: Config{UpstreamTimeout: time.Second}}
	if got := s.upstream().Timeout; got != 0 {
		t.Errorf("upstream client Timeout = %v; a whole-call bound cuts long streams", got)
	}
}

func TestProxy_BodyCap(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	h.server.Config.MaxBodyBytes = 256
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"),
		map[string]any{"model": "prov/m1", "padding": strings.Repeat("x", 1024)}, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body must be 413, got %d", resp.StatusCode)
	}
	if got := errType(t, resp); got != errRequestTooLarge {
		t.Errorf("error type %q", got)
	}
}

func TestProxy_RogueCACertFailsHandshake(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h.seedRoute()
	rogueCA := newTestCA(t)
	rogue := rogueCA.issue(t, "sup.team-a.svc.cluster.local")
	c := h.client(&rogue)
	_, err := c.Post(h.url("/v1/chat/completions"), "application/json", strings.NewReader("{}"))
	if err == nil {
		t.Fatal("certificate from an untrusted CA must fail the TLS handshake")
	}
}

func TestCertLoader_PartialWriteFallback(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "gw")
	l := &tlsutil.CertLoader{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}

	// Prime the caches.
	cert, err := l.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := l.CAPool()
	if err != nil {
		t.Fatal(err)
	}

	// A corrupt cert (partial write) keeps serving the cached certificate.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, certFile, []byte("-----BEGIN CERTIFICATE-----\ngarbage\n-----END CERTIFICATE-----"))
	got, err := l.Certificate()
	if err != nil || got != cert {
		t.Errorf("corrupt cert must fall back to cached: got=%v err=%v", got, err)
	}

	// A corrupt CA bundle keeps serving the cached pool.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, caFile, []byte("not a certificate at all"))
	gotPool, err := l.CAPool()
	if err != nil || gotPool != pool {
		t.Errorf("corrupt CA must fall back to cached pool: err=%v", err)
	}
}

func TestUpstreamTransportPoolsConnectionsPerHost(t *testing.T) {
	s := &Server{Config: Config{UpstreamTimeout: time.Second}}
	tr, ok := s.upstream().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("upstream transport is %T, want *http.Transport", s.upstream().Transport)
	}
	if tr.MaxIdleConnsPerHost != upstreamMaxIdleConnsPerHost || tr.MaxIdleConns != upstreamMaxIdleConns {
		t.Errorf("idle pool = %d per host / %d total, want %d / %d",
			tr.MaxIdleConnsPerHost, tr.MaxIdleConns, upstreamMaxIdleConnsPerHost, upstreamMaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("per-host pool %d is no larger than the default %d", tr.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
}

// TestProxy_ClassNamespaceCheck covers rule 47 on the LLM path: a workload
// whose AgentClass allowedNamespaces does not admit its namespace gets 403
// access_denied; a matching pattern or an unset field lets the call through.
func TestProxy_ClassNamespaceCheck(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	h.seedRoute()
	h.store.tasks["team-a/fix-42"] = &kaalmv1beta1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-42", Namespace: "team-a"},
		Spec: kaalmv1beta1.AgentTaskSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "std"},
			Providers: []kaalmv1beta1.AgentProviderReference{
				{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "prov"}},
			},
		},
	}
	agentC := agentCert(t, h.ca)
	taskC := h.ca.issue(t, "fix-42.team-a.task.kaalm.io")
	body := map[string]any{"model": "prov/m1", "messages": []any{}}

	expectDenied := func(t *testing.T, cert *tls.Certificate) {
		t.Helper()
		resp := postJSON(t, h.client(cert), h.url("/v1/chat/completions"), body, nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status %d, want 403", resp.StatusCode)
		}
		var envelope struct {
			Error errorBody `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Error.Type != errAccessDenied {
			t.Errorf("error type %q, want %q", envelope.Error.Type, errAccessDenied)
		}
		if !strings.Contains(envelope.Error.Message, "allowedNamespaces") ||
			!strings.Contains(envelope.Error.Message, `"std"`) {
			t.Errorf("message %q must name allowedNamespaces and the class", envelope.Error.Message)
		}
	}
	expectForwarded := func(t *testing.T, cert *tls.Certificate) {
		t.Helper()
		resp := postJSON(t, h.client(cert), h.url("/v1/chat/completions"), body, nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status %d, want 200 (%s)", resp.StatusCode, raw)
		}
	}

	h.store.classes["std"].Spec.AllowedNamespaces = []string{"prod-*"}
	t.Run("agent denied", func(t *testing.T) { expectDenied(t, &agentC) })
	t.Run("task denied", func(t *testing.T) { expectDenied(t, &taskC) })

	h.store.classes["std"].Spec.AllowedNamespaces = []string{"team-*"}
	t.Run("agent admitted by glob", func(t *testing.T) { expectForwarded(t, &agentC) })

	h.store.classes["std"].Spec.AllowedNamespaces = nil
	t.Run("agent admitted when unset", func(t *testing.T) { expectForwarded(t, &agentC) })
}
