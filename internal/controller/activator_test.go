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

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// activatorPKI mints a throwaway CA plus leaves for the activator TLS test.
type activatorPKI struct {
	caCert  *x509.Certificate
	caKey   *ecdsa.PrivateKey
	caPool  *x509.CertPool
	certDir string
}

func newActivatorPKI(t *testing.T) *activatorPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "activator-test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &activatorPKI{caCert: caCert, caKey: key, caPool: pool, certDir: t.TempDir()}
}

func (p *activatorPKI) issue(t *testing.T, sans ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: sans[0]},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    sans,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
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

// writeFiles persists the CA and a serving cert to disk for the server.
func (p *activatorPKI) writeFiles(t *testing.T, serving tls.Certificate) (certFile, keyFile, caFile string) {
	t.Helper()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caCert.Raw})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serving.Certificate[0]})
	keyDER, _ := x509.MarshalECPrivateKey(serving.PrivateKey.(*ecdsa.PrivateKey))
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certFile = filepath.Join(p.certDir, "tls.crt")
	keyFile = filepath.Join(p.certDir, "tls.key")
	caFile = filepath.Join(p.certDir, "ca.crt")
	for f, data := range map[string][]byte{certFile: certPEM, keyFile: keyPEM, caFile: caPEM} {
		if err := os.WriteFile(f, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return certFile, keyFile, caFile
}

func TestActivator_WritesWakeAnnotation(t *testing.T) {
	pki := newActivatorPKI(t)
	serving := pki.issue(t, "kaalm-controller.kaalm-system.svc.cluster.local", "localhost")
	certFile, keyFile, caFile := pki.writeFiles(t, serving)

	// A hibernatable agent to activate.
	mkWorkloadClass(t, "wc-activator", nil)
	mkWorkloadAgent(t, "act-agent", "wc-activator", nil)

	srv := &ActivatorServer{
		Client: testClient, OperatorNamespace: testSystemNamespace,
		Addr: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile, CAFile: caFile,
	}
	server, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	addr := server.Listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Start(ctx) }()

	dial := func(clientCert *tls.Certificate) *http.Client {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pki.caPool, ServerName: "localhost"}
		if clientCert != nil {
			cfg.Certificates = []tls.Certificate{*clientCert}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	}
	waitUp := func() {
		eventually(t, func() error {
			resp, err := dial(nil).Get("https://" + addr + "/healthz")
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			return nil
		})
	}
	waitUp()

	gatewayCert := pki.issue(t, "kaalm-gateway."+testSystemNamespace+".svc.cluster.local")
	agentCert := pki.issue(t, "sup.team-a.svc.cluster.local")

	post := func(c *http.Client, path string) int {
		resp, err := c.Post("https://"+addr+path, "application/json", strings.NewReader(""))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// No cert: 401. Agent cert: 403. Gateway cert on a missing agent: 404.
	if got := post(dial(nil), "/v1/activate/default/act-agent"); got != 401 {
		t.Errorf("no cert = %d, want 401", got)
	}
	if got := post(dial(&agentCert), "/v1/activate/default/act-agent"); got != 403 {
		t.Errorf("agent cert = %d, want 403", got)
	}
	if got := post(dial(&gatewayCert), "/v1/activate/default/no-such-agent"); got != 404 {
		t.Errorf("missing agent = %d, want 404", got)
	}

	// Gateway cert on a real agent: 202 and the annotation lands.
	if got := post(dial(&gatewayCert), "/v1/activate/default/act-agent"); got != 202 {
		t.Fatalf("gateway cert = %d, want 202", got)
	}
	// The agent is not Hibernated, so the reconciler consumes the annotation
	// and emits WakeIgnored. Seeing either the annotation or that event
	// proves the activator's write landed.
	eventually(t, func() error {
		var ag kaalmv1beta1.Agent
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "act-agent"}, &ag); err != nil {
			return err
		}
		// The activator writes the wake and its trigger in one patch.
		if ag.Annotations[kaalmv1beta1.AnnotationWake] == kaalmv1beta1.AnnotationTrue {
			if ag.Annotations[kaalmv1beta1.AnnotationWakeTrigger] != kaalmv1beta1.AnnotationWakeTriggerChannel {
				t.Errorf("wake written without %s=%s", kaalmv1beta1.AnnotationWakeTrigger,
					kaalmv1beta1.AnnotationWakeTriggerChannel)
			}
			return nil
		}
		var events corev1.EventList
		if err := testAPIReader.List(ctxT(), &events, client.InNamespace("default")); err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.Reason == kaalmv1beta1.ReasonWakeIgnored && e.InvolvedObject.Name == "act-agent" {
				return nil
			}
		}
		return errString("neither the wake annotation nor a WakeIgnored event observed")
	})
}

// TestHandleActivate_Guards drives handleActivate directly, exercising the
// method, client-cert, SAN, and path-shape guards without a live listener.
func TestHandleActivate_Guards(t *testing.T) {
	s := &ActivatorServer{OperatorNamespace: "kaalm-system"}

	gatewayTLS := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
		{DNSNames: []string{"kaalm-gateway.kaalm-system.svc.cluster.local"}},
	}}
	agentTLS := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
		{DNSNames: []string{"sup.team-a.svc.cluster.local"}},
	}}

	cases := []struct {
		name   string
		method string
		path   string
		tls    *tls.ConnectionState
		want   int
	}{
		{"GET not allowed", http.MethodGet, "/v1/activate/default/a", gatewayTLS, http.StatusMethodNotAllowed},
		{"no client cert", http.MethodPost, "/v1/activate/default/a", nil, http.StatusUnauthorized},
		{"wrong identity", http.MethodPost, "/v1/activate/default/a", agentTLS, http.StatusForbidden},
		{"bad path shape", http.MethodPost, "/v1/activate/only-one-segment", gatewayTLS, http.StatusBadRequest},
		{"empty segment", http.MethodPost, "/v1/activate/default/", gatewayTLS, http.StatusBadRequest},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(""))
		req.TLS = c.tls
		w := httptest.NewRecorder()
		s.handleActivate(w, req)
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// TestHandleActivate_AgentNotFound exercises the Get/NotFound -> 404 path with
// the shared envtest client.
func TestHandleActivate_AgentNotFound(t *testing.T) {
	s := &ActivatorServer{Client: testClient, OperatorNamespace: testSystemNamespace}
	req := httptest.NewRequest(http.MethodPost, "/v1/activate/default/no-such-agent-xyz", strings.NewReader(""))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
		{DNSNames: []string{"kaalm-gateway." + testSystemNamespace + ".svc.cluster.local"}},
	}}
	w := httptest.NewRecorder()
	s.handleActivate(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("missing agent: status = %d, want 404", w.Code)
	}
}

func TestIsGatewayCert_ShortSAN(t *testing.T) {
	s := &ActivatorServer{OperatorNamespace: "kaalm-system"}
	// The short svc SAN form is also accepted.
	if !s.isGatewayCert(&x509.Certificate{DNSNames: []string{"kaalm-gateway.kaalm-system.svc"}}) {
		t.Error("short svc SAN must be recognized as the gateway identity")
	}
	if s.isGatewayCert(&x509.Certificate{DNSNames: []string{"unrelated.example.com"}}) {
		t.Error("an unrelated SAN must not be recognized as the gateway")
	}
}

// TestActivatorListen_Errors drives the ActivatorServer.Listen setup
// failures: missing CA, non-PEM CA, a bad key pair, and a bind failure.
func TestActivatorListen_Errors(t *testing.T) {
	dir := t.TempDir()

	if _, err := (&ActivatorServer{CAFile: filepath.Join(dir, "absent.crt")}).Listen(); err == nil {
		t.Error("missing CA file must fail Listen")
	}

	badPEM := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badPEM, []byte("not a pem block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ActivatorServer{CAFile: badPEM}).Listen(); err == nil {
		t.Error("non-PEM CA must fail Listen")
	}

	pki := newActivatorPKI(t)
	serving := pki.issue(t, "kaalm-controller.kaalm-system.svc.cluster.local")
	certFile, keyFile, caFile := pki.writeFiles(t, serving)

	badKeyPair := &ActivatorServer{
		CAFile: caFile, CertFile: filepath.Join(dir, "absent.crt"), KeyFile: filepath.Join(dir, "absent.key"),
	}
	if _, err := badKeyPair.Listen(); err == nil {
		t.Error("missing key pair must fail Listen")
	}

	badAddr := &ActivatorServer{CAFile: caFile, CertFile: certFile, KeyFile: keyFile, Addr: "not-a-valid-address"}
	if _, err := badAddr.Listen(); err == nil {
		t.Error("an unbindable address must fail Listen")
	}
}

// The activator is a manager.Server that runs on every replica. The manager
// puts such a server in its HTTPServers group, which starts before the
// caches, so the activator never waits on a cache sync.
func TestActivator_ServerRunsOnEveryReplica(t *testing.T) {
	pki := newActivatorPKI(t)
	certFile, keyFile, caFile := pki.writeFiles(t, pki.issue(t, "localhost"))
	server, err := (&ActivatorServer{Addr: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile, CAFile: caFile}).Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Listener.Close() }()
	if server.NeedLeaderElection() {
		t.Error("the activator must run on every replica (OnlyServeWhenLeader=false)")
	}
}

// The activator's readiness check fails until Listen binds the listener,
// passes while it serves, and fails again once the server shuts down. A
// bind failure never passes it.
func TestActivator_ReadyCheck(t *testing.T) {
	pki := newActivatorPKI(t)
	serving := pki.issue(t, "kaalm-controller.kaalm-system.svc.cluster.local", "localhost")
	certFile, keyFile, caFile := pki.writeFiles(t, serving)

	srv := &ActivatorServer{Addr: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile, CAFile: caFile}
	if err := srv.ReadyCheck(nil); err == nil {
		t.Fatal("the check must fail before Listen binds the listener")
	}
	server, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.ReadyCheck(nil); err != nil {
		t.Fatalf("the check must pass once the listener is bound: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start after cancel: %v", err)
	}
	if err := srv.ReadyCheck(nil); err == nil {
		t.Error("the check must fail once the listener has shut down")
	}

	badAddr := &ActivatorServer{CAFile: caFile, CertFile: certFile, KeyFile: keyFile, Addr: "not-a-valid-address"}
	if _, err := badAddr.Listen(); err == nil {
		t.Fatal("an unbindable address must fail Listen")
	}
	if err := badAddr.ReadyCheck(nil); err == nil {
		t.Error("a replica whose listener never bound must not pass the check")
	}
}

// neverSyncedCache stands in for a cache that can't sync: the case of an
// upgrade whose stored v1alpha1 objects need the conversion webhook, which
// the apiserver reaches only through Ready controller Pods.
type neverSyncedCache struct{ cache.Cache }

func (neverSyncedCache) WaitForCacheSync(ctx context.Context) bool {
	<-ctx.Done()
	return false
}

// blockedCacheRunnable joins the manager's cache group with a cache that
// never syncs, so every runnable that waits on the caches stays unstarted.
type blockedCacheRunnable struct{}

func (blockedCacheRunnable) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (blockedCacheRunnable) GetCache() cache.Cache           { return neverSyncedCache{} }

// The activator serves, and its readiness check passes, while the manager's
// caches have not synced. Controller readiness covers the conversion webhook,
// which the caches may need in order to sync, so a check that waited on the
// caches would deadlock an upgrade from before the v1beta1 graduation.
func TestActivator_ServesBeforeCachesSync(t *testing.T) {
	pki := newActivatorPKI(t)
	certFile, keyFile, caFile := pki.writeFiles(t, pki.issue(t, "localhost"))
	gatewayCert := pki.issue(t, "kaalm-gateway."+testSystemNamespace+".svc.cluster.local")

	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testClient.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(blockedCacheRunnable{}); err != nil {
		t.Fatal(err)
	}
	srv := &ActivatorServer{
		Client: testClient, OperatorNamespace: testSystemNamespace,
		Addr: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile, CAFile: caFile,
	}
	server, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(server); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = mgr.Start(ctx) }()

	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: pki.caPool, ServerName: "localhost",
		Certificates: []tls.Certificate{gatewayCert},
	}}}
	// A missing Agent answers 404: the request reached the handler and the
	// apiserver while the caches were still unsynced.
	eventually(t, func() error {
		resp, err := hc.Post("https://"+server.Listener.Addr().String()+"/v1/activate/default/no-such-agent",
			"application/json", strings.NewReader(""))
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("status %d, want 404", resp.StatusCode)
		}
		return nil
	})
	if err := srv.ReadyCheck(nil); err != nil {
		t.Errorf("readiness must not wait on the caches: %v", err)
	}
}

// The handler never reads the Agent: in production its client reads from the
// manager's cache, which may not have synced when a wake arrives. A patch on
// a missing Agent answers NotFound on its own.
func TestHandleActivate_PatchesWithoutRead(t *testing.T) {
	ag := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sleeper", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ag).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errString("the activator must not read the Agent")
			},
		}).Build()
	s := &ActivatorServer{Client: c, OperatorNamespace: testSystemNamespace}
	post := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{
			{DNSNames: []string{"kaalm-gateway." + testSystemNamespace + ".svc.cluster.local"}},
		}}
		w := httptest.NewRecorder()
		s.handleActivate(w, req)
		return w.Code
	}
	if got := post("/v1/activate/default/sleeper"); got != http.StatusAccepted {
		t.Fatalf("existing agent: status %d, want 202", got)
	}
	if got := post("/v1/activate/default/missing"); got != http.StatusNotFound {
		t.Errorf("missing agent: status %d, want 404", got)
	}
}
