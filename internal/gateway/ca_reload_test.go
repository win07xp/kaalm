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
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCABundle writes the given CAs as a concatenated PEM bundle.
func writeCABundle(t *testing.T, path string, cas ...*testCA) {
	t.Helper()
	var buf []byte
	for _, ca := range cas {
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})...)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

// trusts reports whether pool accepts the CA's own self-signed certificate,
// which is true exactly when that CA is in the pool.
func trusts(pool *x509.CertPool, ca *testCA) bool {
	_, err := ca.cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	return err == nil
}

// bumpMtime moves a file's mtime forward so the loader sees a rotation even
// when the write lands inside the filesystem's timestamp granularity.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func TestCAPoolLoader_ReloadsOnRotation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ca.crt")
	original, added := newTestCA(t), newTestCA(t)
	writeCABundle(t, file, original)

	loader := &caPoolLoader{file: file}
	first, err := loader.load()
	if err != nil {
		t.Fatal(err)
	}
	if !trusts(first, original) {
		t.Fatal("the initial pool must trust the bundled CA")
	}

	// An unchanged bundle must not be re-parsed on every call.
	cached, err := loader.load()
	if err != nil {
		t.Fatal(err)
	}
	if cached != first {
		t.Error("pool must be cached while the bundle's mtime is unchanged")
	}

	// Rotate the bundle: the loader must pick up the added CA with no restart.
	writeCABundle(t, file, original, added)
	bumpMtime(t, file)

	rotated, err := loader.load()
	if err != nil {
		t.Fatal(err)
	}
	if !trusts(rotated, added) {
		t.Error("the rotated pool must trust the newly added CA")
	}
	if trusts(first, added) {
		t.Error("the pre-rotation pool must not trust the added CA (test is not proving a reload)")
	}
}

func TestCAPoolLoader_KeepsPoolThroughUnparseableWrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ca.crt")
	ca := newTestCA(t)
	writeCABundle(t, file, ca)

	loader := &caPoolLoader{file: file}
	good, err := loader.load()
	if err != nil {
		t.Fatal(err)
	}

	// A partially written bundle must not drop the working trust pool.
	if err := os.WriteFile(file, []byte("-----BEGIN CERTIFICATE-----\ntrunc"), 0o600); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, file)

	after, err := loader.load()
	if err != nil {
		t.Fatalf("a partial write must not surface an error while a pool is held: %v", err)
	}
	if after != good || !trusts(after, ca) {
		t.Error("the previous pool must be retained through an unparseable write")
	}
}

func TestCAPoolLoader_AdditiveKeepsCustomCA(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ca.crt")
	ca := newTestCA(t)
	writeCABundle(t, file, ca)

	// additive starts from the system roots; the custom CA must still be
	// trusted on top of them (the system roots themselves vary by host, so
	// only the additive CA is asserted here).
	loader := &caPoolLoader{file: file, additive: true}
	pool, err := loader.load()
	if err != nil {
		t.Fatal(err)
	}
	if !trusts(pool, ca) {
		t.Error("an additive pool must trust the bundled CA alongside the system roots")
	}
}

func TestCAPoolLoader_MissingFileErrors(t *testing.T) {
	loader := &caPoolLoader{file: filepath.Join(t.TempDir(), "absent.crt")}
	if _, err := loader.load(); err == nil {
		t.Fatal("a missing bundle must report an error rather than silently trusting nothing")
	}
}
