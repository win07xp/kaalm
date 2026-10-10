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

package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/win07xp/kaalm/internal/tlsutil"
)

func TestGatewayChatClient_RequestShapeAndRelay(t *testing.T) {
	var got map[string]string
	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"type":"delivery_failed"}}`))
	}))
	defer srv.Close()

	c := &GatewayChatClient{BaseURL: srv.URL, Insecure: true}
	status, body, err := c.Chat(context.Background(), "team-a", "sup", "priya", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/test-chat" {
		t.Errorf("path = %q", gotPath)
	}
	want := map[string]string{"namespace": "team-a", "agent": "sup", "userId": "priya", "content": "hello"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("request %s = %q, want %q", k, got[k], v)
		}
	}
	// The gateway's status and body are relayed verbatim, never re-mapped.
	if status != 502 || string(body) != `{"error":{"type":"delivery_failed"}}` {
		t.Errorf("relay = %d %s", status, body)
	}
}

func TestNewGatewayChatClient_Defaults(t *testing.T) {
	c := NewGatewayChatClient("kaalm-system", "/var/run/kaalm/tls.crt", "/var/run/kaalm/tls.key", "/var/run/kaalm/ca.crt")
	if c.BaseURL != "https://kaalm-gateway.kaalm-system.svc.cluster.local:8443" {
		t.Errorf("baseURL = %q", c.BaseURL)
	}
	if c.ServerName != "kaalm-gateway.kaalm-system.svc.cluster.local" {
		t.Errorf("serverName = %q", c.ServerName)
	}
	if c.Loader == nil || c.Loader.CertFile != "/var/run/kaalm/tls.crt" {
		t.Error("loader must carry the console identity paths")
	}

	// A missing identity surfaces as an error, not a certless call.
	if _, _, err := c.Chat(context.Background(), "ns", "a", "u", "c"); err == nil {
		t.Error("a missing cert file must error")
	}
}

// Test-chat and spend calls share one pooled client, so a run of calls
// reuses one connection instead of a TLS handshake and a leaked transport
// per call.
func TestGatewayChatClient_ReusesTheConnection(t *testing.T) {
	var opened atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	c := &GatewayChatClient{BaseURL: srv.URL, Insecure: true}
	for range 2 {
		if status, _, err := c.Chat(context.Background(), "ns", "a", "u", "hi"); err != nil || status != 200 {
			t.Fatalf("chat = %d, %v", status, err)
		}
		if status, _, err := c.WorkloadSpend(context.Background(), "ns"); err != nil || status != 200 {
			t.Fatalf("spend = %d, %v", status, err)
		}
	}
	if got := opened.Load(); got != 1 {
		t.Errorf("connections opened for 4 calls = %d, want 1", got)
	}
}

// writeSelfSigned writes a self-signed key pair with the given common name
// to certFile and keyFile, and moves both mtimes forward so a CertLoader
// sees the write as a rotation.
func writeSelfSigned(t *testing.T, certFile, keyFile, cn string, mtime time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER, mtime)
	writePEM(t, certFile, "CERTIFICATE", der, mtime)
}

func writePEM(t *testing.T, path, blockType string, der []byte, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// The pooled client still reads the console identity from disk: a rotated
// client certificate is presented on the next connection, and a rotated CA
// bundle decides the next gateway verification, with no new client.
func TestGatewayChatClient_PicksUpRotatedCertAndCA(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.TLS.PeerCertificates[0].Subject.CommonName)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	certFile, keyFile, caFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt")
	start := time.Now().Add(-time.Minute)
	writeSelfSigned(t, certFile, keyFile, "console-1", start)
	// The first CA bundle does not hold the gateway's CA: the call must fail
	// verification.
	writePEM(t, caFile, "CERTIFICATE", mustRead(t, certFile), start)

	c := &GatewayChatClient{
		BaseURL:    srv.URL,
		Loader:     &tlsutil.CertLoader{CertFile: certFile, KeyFile: keyFile, CAFile: caFile},
		ServerName: "example.com", // a SAN of the httptest certificate
	}
	if _, _, err := c.WorkloadSpend(context.Background(), "ns"); err == nil {
		t.Fatal("a gateway outside the CA bundle must fail verification")
	}

	// CA rotation: the bundle now holds the gateway's CA.
	writePEM(t, caFile, "CERTIFICATE", srv.Certificate().Raw, start.Add(time.Second))
	if status, _, err := c.WorkloadSpend(context.Background(), "ns"); err != nil || status != 200 {
		t.Fatalf("spend after CA rotation = %d, %v", status, err)
	}

	// Leaf rotation: the next connection presents the new certificate.
	writeSelfSigned(t, certFile, keyFile, "console-2", start.Add(2*time.Second))
	srv.CloseClientConnections()
	if status, _, err := c.WorkloadSpend(context.Background(), "ns"); err != nil || status != 200 {
		t.Fatalf("spend after cert rotation = %d, %v", status, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "console-1" || seen[1] != "console-2" {
		t.Errorf("client certificates seen = %v, want [console-1 console-2]", seen)
	}
}

// mustRead returns the DER bytes of the first PEM block in path.
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("%s holds no PEM block", path)
	}
	return block.Bytes
}

// BenchmarkGatewayChatClient_WorkloadSpend measures one spend call over the
// pooled client; before pooling, each call paid a full TLS handshake.
func BenchmarkGatewayChatClient_WorkloadSpend(b *testing.B) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &GatewayChatClient{BaseURL: srv.URL, Insecure: true}
	b.ReportAllocs()
	for range b.N {
		if _, _, err := c.WorkloadSpend(context.Background(), "ns"); err != nil {
			b.Fatal(err)
		}
	}
}
