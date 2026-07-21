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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// certFiles writes a leaf cert.pem/key.pem signed by ca plus the ca.pem bundle
// into a temp dir and returns their paths.
func certFiles(t *testing.T, ca *testCA, sans ...string) (certFile, keyFile, caFile string) {
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

	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	caFile = filepath.Join(dir, "ca.crt")
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	writeFile(t, caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}))
	return certFile, keyFile, caFile
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServer_TLSConfig(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "kaalm-gateway.kaalm-system.svc.cluster.local")
	s := NewServer(Config{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}, newFakeStore(), nil, nil)

	cfg, err := s.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %v", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs pool must be built")
	}
	// GetCertificate serves the loaded leaf.
	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Errorf("GetCertificate: %v", err)
	}
	// GetConfigForClient rebuilds a config with a fresh CA pool.
	inner, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil || inner == nil || inner.ClientCAs == nil {
		t.Errorf("GetConfigForClient: cfg=%v err=%v", inner, err)
	}
	if _, err := inner.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Errorf("inner GetCertificate: %v", err)
	}
}

func TestServer_TLSConfig_Errors(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "gw")

	// Missing cert file: certificate() errors, TLSConfig fails.
	s := NewServer(Config{CertFile: "/nonexistent/tls.crt", KeyFile: keyFile, CAFile: caFile}, newFakeStore(), nil, nil)
	if _, err := s.TLSConfig(); err == nil {
		t.Error("missing cert file must error")
	}

	// Valid cert, missing CA file: caPool() errors.
	s2 := NewServer(Config{CertFile: certFile, KeyFile: keyFile, CAFile: "/nonexistent/ca.crt"}, newFakeStore(), nil, nil)
	if _, err := s2.TLSConfig(); err == nil {
		t.Error("missing CA file must error")
	}

	// A CA file with no PEM certificates: caPool() errors.
	dir := t.TempDir()
	emptyCA := filepath.Join(dir, "empty-ca.crt")
	writeFile(t, emptyCA, []byte("not a certificate"))
	s3 := NewServer(Config{CertFile: certFile, KeyFile: keyFile, CAFile: emptyCA}, newFakeStore(), nil, nil)
	if _, err := s3.TLSConfig(); err == nil {
		t.Error("CA bundle with no certs must error")
	}
}

func TestCertLoader_CachesByMtime(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "gw")
	l := &certLoader{certFile: certFile, keyFile: keyFile, caFile: caFile}

	c1, err := l.certificate()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := l.certificate()
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Error("unchanged cert file must return the cached certificate")
	}
	p1, err := l.caPool()
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := l.caPool()
	if p1 != p2 {
		t.Error("unchanged CA file must return the cached pool")
	}
}

func TestServer_Run_TLSConfigErrorReturns(t *testing.T) {
	// A bad cert path makes TLSConfig fail before any listener is opened.
	s := NewServer(Config{CertFile: "/nonexistent/tls.crt", KeyFile: "/nonexistent/tls.key", CAFile: "/nonexistent/ca.crt"},
		newFakeStore(), nil, nil)
	if err := s.Run(context.Background()); err == nil {
		t.Error("Run must return the TLSConfig error")
	}
}

func TestServer_Run_ServesAndShutsDown(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "localhost")
	s := NewServer(Config{
		CertFile: certFile, KeyFile: keyFile, CAFile: caFile,
		ListenAddr: "127.0.0.1:0", HealthAddr: "127.0.0.1:0", UserListenAddr: "127.0.0.1:0",
	}, newFakeStore(), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	// Give the listeners a moment to bind, then cancel to hit the shutdown path.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v, want nil after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down after cancellation")
	}
}
