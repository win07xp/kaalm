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

package gateway

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// readinessHarness runs the gateway's three listeners on loopback ports and
// probes the health listener the way the kubelet does.
type readinessHarness struct {
	s        *Server
	ls       *listeners
	certFile string
	client   *http.Client
}

func newReadinessHarness(t *testing.T, informers ...InformerSync) *readinessHarness {
	t.Helper()
	ca := newTestCA(t)
	certFile, keyFile, caFile := certFiles(t, ca, "localhost")
	s := NewServer(Config{
		CertFile: certFile, KeyFile: keyFile, CAFile: caFile,
		ListenAddr: "127.0.0.1:0", HealthAddr: "127.0.0.1:0", UserListenAddr: "127.0.0.1:0",
	}, newFakeStore(), nil, nil)
	s.Informers = informers

	tlsCfg, err := s.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	ls, err := s.listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, ls, tlsCfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serve did not shut down")
		}
	})
	return &readinessHarness{
		s: s, ls: ls, certFile: certFile,
		client: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test probe
		},
	}
}

func (h *readinessHarness) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := h.client.Get("https://" + h.ls.health.Addr().String() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func synced(name string, ok *atomic.Bool) InformerSync {
	return InformerSync{Name: name, HasSynced: ok.Load}
}

func TestReadyz_AllChecksPass(t *testing.T) {
	var pod, agent atomic.Bool
	pod.Store(true)
	agent.Store(true)
	h := newReadinessHarness(t, synced("Pod", &pod), synced("Agent", &agent))

	code, body := h.get(t, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200; body:\n%s", code, body)
	}
	for _, line := range []string{
		"cluster_listener: ok", "user_listener: ok", "informers: ok", "serving_cert: ok",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("body missing %q:\n%s", line, body)
		}
	}
}

func TestReadyz_UnsyncedInformerFails(t *testing.T) {
	var pod, agent atomic.Bool
	pod.Store(true)
	h := newReadinessHarness(t, synced("Pod", &pod), synced("Agent", &agent))

	code, body := h.get(t, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503; body:\n%s", code, body)
	}
	if !strings.Contains(body, "informers: not synced: Agent") {
		t.Errorf("body must name the unsynced informer:\n%s", body)
	}
	if !strings.Contains(body, "cluster_listener: ok") {
		t.Errorf("the passing checks still report ok:\n%s", body)
	}

	// The probe reads live state: once the informer syncs, the replica is ready.
	agent.Store(true)
	if code, body := h.get(t, "/readyz"); code != http.StatusOK {
		t.Errorf("readyz after sync = %d, want 200; body:\n%s", code, body)
	}
}

// readyz runs the readiness handler against the given listener addresses,
// outside serve, so a test can point a check at a dead or silent port.
func (h *readinessHarness) readyz(t *testing.T, cluster, user net.Addr) (int, string, time.Duration) {
	t.Helper()
	handler := h.s.readyzHandler(cluster, user, func() error { return nil })
	rec := httptest.NewRecorder()
	start := time.Now()
	handler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code, rec.Body.String(), time.Since(start)
}

func TestReadyz_ListenerDownFails(t *testing.T) {
	h := newReadinessHarness(t)
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr()
	_ = dead.Close() // nothing listens here any more

	code, body, _ := h.readyz(t, h.ls.main.Addr(), deadAddr)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503; body:\n%s", code, body)
	}
	if !strings.Contains(body, "user_listener: ") || strings.Contains(body, "user_listener: ok") {
		t.Errorf("user_listener must fail:\n%s", body)
	}
	if !strings.Contains(body, "cluster_listener: ok") {
		t.Errorf("cluster_listener must still pass:\n%s", body)
	}
}

// A listener that accepts TCP but never completes the TLS handshake fails
// the check within the probe bound, well under the kubelet's 1s timeout.
func TestReadyz_SilentListenerTimesOut(t *testing.T) {
	h := newReadinessHarness(t)
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	go func() {
		var held []net.Conn // accepted and never answered
		defer func() {
			for _, conn := range held {
				_ = conn.Close()
			}
		}()
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()

	code, body, took := h.readyz(t, silent.Addr(), h.ls.user.Addr())
	if code != http.StatusServiceUnavailable || strings.Contains(body, "cluster_listener: ok") {
		t.Fatalf("readyz = %d, want 503 with cluster_listener failing; body:\n%s", code, body)
	}
	if took > readinessTimeout+250*time.Millisecond {
		t.Errorf("readyz took %v, want about %v", took, readinessTimeout)
	}
}

func TestReadyz_ServingCertMissingFails(t *testing.T) {
	h := newReadinessHarness(t)
	if code, body := h.get(t, "/readyz"); code != http.StatusOK {
		t.Fatalf("readyz before removal = %d; body:\n%s", code, body)
	}
	// The health listener keeps handshaking with the last cert it served,
	// so the probe gets a body that names the failure.
	if err := os.Remove(h.certFile); err != nil {
		t.Fatal(err)
	}

	code, body := h.get(t, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503; body:\n%s", code, body)
	}
	if !strings.Contains(body, "serving_cert: ") || strings.Contains(body, "serving_cert: ok") {
		t.Errorf("serving_cert must fail:\n%s", body)
	}
}

// /healthz is liveness only: it passes while readiness fails.
func TestHealthz_IgnoresReadiness(t *testing.T) {
	var agent atomic.Bool
	h := newReadinessHarness(t, synced("Agent", &agent))

	if code, _ := h.get(t, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", code)
	}
	if code, body := h.get(t, "/healthz"); code != http.StatusOK || body != "ok" {
		t.Errorf("healthz = %d %q, want 200 ok", code, body)
	}
}
