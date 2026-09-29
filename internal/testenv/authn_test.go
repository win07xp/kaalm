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

package testenv

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"slices"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestBackdatedCertAuthn_SurvivesBackwardClockStep proves the admin
// credentials stay valid when the wall clock steps back right after they are
// minted, which is what makes the apiserver answer 401 on a host whose clock
// is stepped backward (#310).
func TestBackdatedCertAuthn_SurvivesBackwardClockStep(t *testing.T) {
	authn, err := newBackdatedCertAuthn()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := envtest.EmptyArguments()
	if err := authn.Configure(dir, args); err != nil {
		t.Fatal(err)
	}
	caFlag := args.Get("client-ca-file").Get(nil)
	if len(caFlag) != 1 {
		t.Fatalf("client-ca-file = %v, want one path", caFlag)
	}
	if err := authn.Start(); err != nil {
		t.Fatal(err)
	}

	base := &rest.Config{Host: "https://127.0.0.1:6443", QPS: 7}
	cfg, err := authn.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != base.Host || cfg.QPS != base.QPS {
		t.Errorf("AddUser dropped the base config: %+v", cfg)
	}
	if base.CertData != nil {
		t.Error("AddUser changed the base config")
	}

	caPEM, err := os.ReadFile(caFlag[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("client-ca-file holds no certificate")
	}
	block, _ := pem.Decode(cfg.CertData)
	if block == nil {
		t.Fatal("CertData holds no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "admin" || !slices.Equal(cert.Subject.Organization, []string{"system:masters"}) {
		t.Errorf("subject = %v, want CN=admin O=system:masters", cert.Subject)
	}
	if block, _ := pem.Decode(cfg.KeyData); block == nil {
		t.Error("KeyData holds no PEM block")
	}

	// The apiserver verifies the client certificate on every request, at its
	// own clock. A clock stepped back past the mint time must not make the
	// chain "not yet valid".
	for _, step := range []time.Duration{0, 2 * time.Second, 5 * time.Minute} {
		_, err := cert.Verify(x509.VerifyOptions{
			Roots:       roots,
			CurrentTime: time.Now().Add(-step),
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
		if err != nil {
			t.Errorf("clock stepped back %s: %v", step, err)
		}
	}
}

func TestBackdatedCertAuthn_StartBeforeConfigure(t *testing.T) {
	authn, err := newBackdatedCertAuthn()
	if err != nil {
		t.Fatal(err)
	}
	if err := authn.Start(); err == nil {
		t.Error("Start before Configure succeeded")
	}
	if err := authn.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
