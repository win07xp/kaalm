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
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
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
	serving := pki.issue(t, "agentry-controller.agentry-system.svc.cluster.local", "localhost")
	certFile, keyFile, caFile := pki.writeFiles(t, serving)

	// A hibernatable agent to activate.
	mkWorkloadClass(t, "wc-activator", nil)
	mkWorkloadAgent(t, "act-agent", "wc-activator", nil)

	srv := &ActivatorServer{
		Client: testClient, OperatorNamespace: testSystemNamespace,
		Addr: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile, CAFile: caFile,
	}
	// Bind a fixed port so we know where to dial.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv.Addr = addr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

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

	gatewayCert := pki.issue(t, "agentry-gateway."+testSystemNamespace+".svc.cluster.local")
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
		var ag agentryv1alpha1.Agent
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "act-agent"}, &ag); err != nil {
			return err
		}
		if ag.Annotations[agentryv1alpha1.AnnotationWake] == agentryv1alpha1.AnnotationTrue {
			return nil
		}
		var events corev1.EventList
		if err := testClient.List(ctxT(), &events, client.InNamespace("default")); err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.Reason == agentryv1alpha1.ReasonWakeIgnored && e.InvolvedObject.Name == "act-agent" {
				return nil
			}
		}
		return errString("neither the wake annotation nor a WakeIgnored event observed")
	})
}
