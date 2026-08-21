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
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tlsServer serves 200s under a leaf from the given PKI.
func tlsServer(t *testing.T, pki *activatorPKI) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.issue(t, "localhost")}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func caPEM(pki *activatorPKI) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pki.caCert.Raw})
}

// The #86 probe trust: a configured bundle is honored additively, and a CA
// rotated in place is picked up without a new client (the restart-free
// contract every other outbound trust pool keeps).
func TestProbeClient_TrustsConfiguredCAAndFollowsRotation(t *testing.T) {
	pki := newActivatorPKI(t)
	srv := tlsServer(t, pki)

	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM(pki), 0o600); err != nil {
		t.Fatal(err)
	}

	client := NewProbeClient([]string{caFile})
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("probe against the configured CA failed: %v", err)
	}
	_ = resp.Body.Close()

	// The same endpoint under system roots only: refused. This is exactly
	// the pre-#86 failure mode of the nil-Client default.
	if _, err := (&http.Client{}).Get(srv.URL); err == nil {
		t.Fatal("a system-roots client trusted the private CA")
	}

	// Rotate the bundle in place to a fresh CA: a server under the new CA
	// becomes trusted through the same client, the old one stops being.
	pki2 := newActivatorPKI(t)
	srv2 := tlsServer(t, pki2)
	if err := os.WriteFile(caFile, caPEM(pki2), 0o600); err != nil {
		t.Fatal(err)
	}
	// The loader keys on mtime; make the rewrite unambiguous.
	if err := os.Chtimes(caFile, time.Now(), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err = client.Get(srv2.URL)
	if err != nil {
		t.Fatalf("probe after CA rotation failed: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("the rotated-out CA is still trusted")
	}
}
