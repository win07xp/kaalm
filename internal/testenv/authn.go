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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	// backdate is how far before minting the certificates become valid. The
	// apiserver checks a client certificate on every request against its own
	// wall clock, so the margin must cover any backward clock step.
	backdate = time.Hour
	// certLifetime outlives any test binary.
	certLifetime = 7 * 24 * time.Hour
)

// backdatedCertAuthn is envtest's client-certificate authentication with one
// change: the CA and every user certificate are valid from an hour before
// they were minted. envtest's own CertAuthn stamps NotBefore with the mint
// time, so a wall clock stepped back by even a second or two right after
// minting makes the admin certificate "not yet valid" and the apiserver
// answers 401 Unauthorized until the clock catches up.
type backdatedCertAuthn struct {
	caCert *x509.Certificate
	caKey  crypto.Signer
	caPath string
}

var _ envtest.Authn = (*backdatedCertAuthn)(nil)

func newBackdatedCertAuthn() (*backdatedCertAuthn, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate client CA key: %w", err)
	}
	cert, err := mint(&x509.Certificate{
		Subject:               pkix.Name{CommonName: "envtest-environment", Organization: []string{"envtest"}},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}, key.Public(), nil, key)
	if err != nil {
		return nil, fmt.Errorf("create client CA: %w", err)
	}
	return &backdatedCertAuthn{caCert: cert, caKey: key}, nil
}

// mint signs template with signerKey, as parent (self-signed when parent is
// nil), valid from backdate ago for certLifetime.
func mint(template *x509.Certificate, pub crypto.PublicKey, parent *x509.Certificate,
	signerKey crypto.Signer) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template.SerialNumber = serial
	template.NotBefore = now.Add(-backdate).UTC()
	template.NotAfter = now.Add(certLifetime).UTC()
	if parent == nil {
		parent = template
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, signerKey)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// Configure points the apiserver at the client CA file Start writes.
func (a *backdatedCertAuthn) Configure(workDir string, args *envtest.Arguments) error {
	a.caPath = filepath.Join(workDir, "client-cert-auth-ca.crt")
	args.Set("client-ca-file", a.caPath)
	return nil
}

// Start writes the client CA where Configure pointed the apiserver.
func (a *backdatedCertAuthn) Start() error {
	if a.caPath == "" {
		return errors.New("start called before configure")
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.caCert.Raw})
	if err := os.WriteFile(a.caPath, caPEM, 0o600); err != nil {
		return fmt.Errorf("write client CA to %s: %w", a.caPath, err)
	}
	return nil
}

// AddUser mints a client certificate for user (CN is the name, O the
// groups) and returns a copy of baseCfg that presents it.
func (a *backdatedCertAuthn) AddUser(user envtest.User, baseCfg *rest.Config) (*rest.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key for %s: %w", user.Name, err)
	}
	cert, err := mint(&x509.Certificate{
		Subject:     pkix.Name{CommonName: user.Name, Organization: user.Groups},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, key.Public(), a.caCert, a.caKey)
	if err != nil {
		return nil, fmt.Errorf("create client certificate for %s: %w", user.Name, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode key for %s: %w", user.Name, err)
	}
	cfg := rest.CopyConfig(baseCfg)
	cfg.CertData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	cfg.KeyData = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return cfg, nil
}

// Stop has nothing to release: the CA file lives in the apiserver's
// directory, which envtest removes.
func (a *backdatedCertAuthn) Stop() error { return nil }
