// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
)

func testReloader(t *testing.T, pki *testPKI) *certReloader {
	t.Helper()
	mount := t.TempDir()
	certPEM, keyPEM := pki.issue(t, "workload", "w.default.svc.cluster.local")
	writeMount(t, mount, certPEM, keyPEM, pki.caPEM)
	r, err := newCertReloader(
		filepath.Join(mount, "tls.crt"), filepath.Join(mount, "tls.key"), filepath.Join(mount, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGateway_PostAndGetSemantics(t *testing.T) {
	pki := newTestPKI(t)
	type seen struct {
		method, path, contentType, body string
		hadClientCert                   bool
	}
	requests := make(chan seen, 4)
	srv := mockGateway(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- seen{
			method:        r.Method,
			path:          r.URL.Path,
			contentType:   r.Header.Get("Content-Type"),
			body:          string(body),
			hadClientCert: len(r.TLS.PeerCertificates) > 0,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	g := newGateway(srv.URL+"/", testReloader(t, pki)) // trailing slash must be trimmed
	if g.Endpoint() != srv.URL {
		t.Errorf("Endpoint() = %q, want %q", g.Endpoint(), srv.URL)
	}

	resp, err := g.Post(context.Background(), "/v1/chat/completions", map[string]any{"model": "p/m"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil || !decoded["ok"] {
		t.Errorf("response passthrough broken: %v %v", decoded, err)
	}
	_ = resp.Body.Close()
	got := <-requests
	if got.method != http.MethodPost || got.path != "/v1/chat/completions" {
		t.Errorf("Post sent %s %s", got.method, got.path)
	}
	if got.contentType != "application/json" || got.body != `{"model":"p/m"}` {
		t.Errorf("Post body: %q (%s)", got.body, got.contentType)
	}
	if !got.hadClientCert {
		t.Error("Post must present the workload's mTLS identity")
	}

	// A nil body posts empty with no content type; a path without a leading
	// slash still resolves.
	resp, err = g.Post(context.Background(), "v1/agent/heartbeat", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got = <-requests
	if got.path != "/v1/agent/heartbeat" || got.body != "" || got.contentType != "" {
		t.Errorf("nil-body Post: path=%q body=%q ct=%q", got.path, got.body, got.contentType)
	}

	resp, err = g.Get(context.Background(), "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got = <-requests; got.method != http.MethodGet || got.path != "/v1/models" {
		t.Errorf("Get sent %s %s", got.method, got.path)
	}
}

func TestGateway_UnmarshalableBodyFails(t *testing.T) {
	g := newGateway("https://unused", testReloader(t, newTestPKI(t)))
	if _, err := g.Post(context.Background(), "/x", func() {}); err == nil {
		t.Error("an unmarshalable body must error before any request is sent")
	}
}
