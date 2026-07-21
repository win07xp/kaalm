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
	"os"
	"path/filepath"
	"testing"
)

// TestActivatorStart_Errors drives the ActivatorServer.Start setup failures:
// missing CA, non-PEM CA, a bad key pair, and a bind failure.
func TestActivatorStart_Errors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if err := (&ActivatorServer{CAFile: filepath.Join(dir, "absent.crt")}).Start(ctx); err == nil {
		t.Error("missing CA file must fail Start")
	}

	badPEM := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badPEM, []byte("not a pem block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&ActivatorServer{CAFile: badPEM}).Start(ctx); err == nil {
		t.Error("non-PEM CA must fail Start")
	}

	pki := newActivatorPKI(t)
	serving := pki.issue(t, "agentry-controller.agentry-system.svc.cluster.local")
	certFile, keyFile, caFile := pki.writeFiles(t, serving)

	badKeyPair := &ActivatorServer{
		CAFile: caFile, CertFile: filepath.Join(dir, "absent.crt"), KeyFile: filepath.Join(dir, "absent.key"),
	}
	if err := badKeyPair.Start(ctx); err == nil {
		t.Error("missing key pair must fail Start")
	}

	// Valid material but an unbindable address: ListenAndServeTLS returns an
	// error the select surfaces.
	badAddr := &ActivatorServer{CAFile: caFile, CertFile: certFile, KeyFile: keyFile, Addr: "not-a-valid-address"}
	if err := badAddr.Start(ctx); err == nil {
		t.Error("an unbindable address must fail Start")
	}
}

// TestGatewayHTTPClient covers the client builder: a cached second call plus the
// CA read and parse failures.
func TestGatewayHTTPClient(t *testing.T) {
	pki := newActivatorPKI(t)
	cert := pki.issue(t, "agentry-controller.default.svc.cluster.local")
	certFile, keyFile, caFile := pki.writeFiles(t, cert)

	g := &GatewayActivityClient{OperatorNamespace: "default", CertFile: certFile, KeyFile: keyFile, CAFile: caFile}
	c1, err := g.httpClient()
	if err != nil || c1 == nil {
		t.Fatalf("first httpClient: %v", err)
	}
	c2, err := g.httpClient()
	if err != nil {
		t.Fatalf("second httpClient: %v", err)
	}
	if c1 != c2 {
		t.Error("httpClient must memoize the constructed client")
	}

	dir := t.TempDir()
	badPEM := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badPEM, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&GatewayActivityClient{CertFile: certFile, KeyFile: keyFile, CAFile: filepath.Join(dir, "absent")}).httpClient(); err == nil {
		t.Error("a missing CA file must fail httpClient")
	}
	if _, err := (&GatewayActivityClient{CertFile: certFile, KeyFile: keyFile, CAFile: badPEM}).httpClient(); err == nil {
		t.Error("a non-PEM CA must fail httpClient")
	}
}
