// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// kubeletMount lays TLS material out the way the kubelet projects a volume:
// leaf names are symlinks through a ..data directory that rotation replaces
// atomically. Returns the mount dir and a rotate function.
func kubeletMount(
	t *testing.T, certPEM, keyPEM, caPEM []byte,
) (dir string, rotate func(certPEM, keyPEM, caPEM []byte)) {
	t.Helper()
	dir = t.TempDir()
	version := 0

	write := func(certPEM, keyPEM, caPEM []byte) string {
		version++
		data := filepath.Join(dir, "..data_"+time.Now().Format("150405")+"_"+string(rune('a'+version)))
		if err := os.MkdirAll(data, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM, "ca.crt": caPEM} {
			if err := os.WriteFile(filepath.Join(data, name), content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Base(data)
	}

	base := write(certPEM, keyPEM, caPEM)
	if err := os.Symlink(base, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tls.crt", "tls.key", "ca.crt"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	rotate = func(certPEM, keyPEM, caPEM []byte) {
		next := write(certPEM, keyPEM, caPEM)
		// The kubelet's AtomicWriter: symlink to a temp name, then rename
		// over ..data. The leaf symlinks never change.
		tmp := filepath.Join(dir, "..data_tmp")
		if err := os.Symlink(next, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	return dir, rotate
}

// The rotation watch anchors on the directory and fires on the ..data swap;
// a leaf-file watch would never fire (contract item 4).
func TestCertReloader_ReloadsOnKubeletRotation(t *testing.T) {
	pki := newTestPKI(t)
	certPEM, keyPEM := pki.issue(t, "workload-v1", "w.default.svc.cluster.local")
	dir, rotate := kubeletMount(t, certPEM, keyPEM, pki.caPEM)

	r, err := newCertReloader(
		filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.watch(t.Logf); err != nil {
		t.Fatal(err)
	}
	genBefore := r.generation()

	certPEM2, keyPEM2 := pki.issue(t, "workload-v2", "w.default.svc.cluster.local")
	rotate(certPEM2, keyPEM2, pki.caPEM)

	deadline := time.Now().Add(5 * time.Second)
	for r.generation() == genBefore {
		if time.Now().After(deadline) {
			t.Fatal("rotation never triggered a reload")
		}
		time.Sleep(10 * time.Millisecond)
	}

	leaf, err := x509.ParseCertificate(r.certificate().Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "workload-v2" {
		t.Errorf("served cert after rotation is %q, want workload-v2", leaf.Subject.CommonName)
	}
}

// A CA rotation must swap outbound trust, not just the client certificate:
// the old CA's servers become distrusted and the new CA's trusted. This is
// the property the reloadingTransport exists for; a plain snapshot transport
// fails the second half forever.
func TestReloadingTransport_TracksCARotation(t *testing.T) {
	oldPKI := newTestPKI(t)
	newPKI := newTestPKI(t)

	mount := t.TempDir()
	certPEM, keyPEM := oldPKI.issue(t, "workload", "w.default.svc.cluster.local")
	writeMount(t, mount, certPEM, keyPEM, oldPKI.caPEM)
	r, err := newCertReloader(
		filepath.Join(mount, "tls.crt"), filepath.Join(mount, "tls.key"), filepath.Join(mount, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}

	oldServer := mockGateway(t, oldPKI, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	newServer := mockGateway(t, newPKI, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	client := &http.Client{Timeout: 5 * time.Second, Transport: &reloadingTransport{reloader: r}}

	resp, err := client.Get(oldServer.URL)
	if err != nil {
		t.Fatalf("pre-rotation request to the old-CA server: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := client.Get(newServer.URL); err == nil {
		t.Fatal("the new CA must not be trusted before rotation")
	}

	// Rotate: the projected volume now carries the new CA bundle and a new
	// workload cert. (In production both roll from the same ClusterIssuer.)
	certPEM2, keyPEM2 := newPKI.issue(t, "workload", "w.default.svc.cluster.local")
	writeMount(t, mount, certPEM2, keyPEM2, newPKI.caPEM)
	if err := r.reload(); err != nil {
		t.Fatal(err)
	}

	resp, err = client.Get(newServer.URL)
	if err != nil {
		t.Fatalf("post-rotation request to the new-CA server: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := client.Get(oldServer.URL); err == nil {
		t.Fatal("the old CA must be distrusted after rotation, not appended to")
	}
}
