// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
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
	"testing"
	"time"
)

// testPKI is a throwaway certificate authority for exercising the runtime's
// real TLS stack in-process: serving certs, the gateway's client identity,
// wrong-SAN intruders, and task-SAN workload certs.
type testPKI struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "agentruntime-test-ca"},
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{
		caCert: cert,
		caKey:  key,
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue returns PEM cert/key for a leaf with the given DNS SANs, valid for
// both client and server use, with 127.0.0.1 included for local dialing.
func (p *testPKI) issue(t *testing.T, cn string, dnsSANs ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsSANs,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// writeMount lays cert material out as flat files (tls.crt, tls.key, ca.crt)
// in dir, the shape the runtime reads.
func writeMount(t *testing.T, dir string, certPEM, keyPEM, caPEM []byte) {
	t.Helper()
	files := map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM, "ca.crt": caPEM}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// setMountEnv points the runtime's TLS env vars at a mount directory.
func setMountEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("KAALM_TLS_CERT", filepath.Join(dir, "tls.crt"))
	t.Setenv("KAALM_TLS_KEY", filepath.Join(dir, "tls.key"))
	t.Setenv("KAALM_CA_CERT", filepath.Join(dir, "ca.crt"))
}

// clientFor issues a fresh identity with the given SANs and returns an HTTP
// client presenting it.
func (p *testPKI) clientFor(t *testing.T, cn string, sans ...string) *http.Client {
	t.Helper()
	certPEM, keyPEM := p.issue(t, cn, sans...)
	return p.client(t, certPEM, keyPEM)
}

// client returns an HTTP client trusting the test CA, optionally presenting
// the given PEM client identity (nil PEMs mean no client certificate).
func (p *testPKI) client(t *testing.T, certPEM, keyPEM []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if certPEM != nil {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}
}
