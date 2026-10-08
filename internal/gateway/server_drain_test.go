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
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newConnGet sends a GET on a new connection, the way a client that the
// Service just routed to this Pod would.
func newConnGet(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test probe
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// After the signal, the gateway fails readiness but keeps serving both
// traffic listeners for the drain delay, so connections that kube-proxy
// still routes to it are answered instead of refused.
func TestServe_DrainsBeforeShutdown(t *testing.T) {
	h := newReadinessHarnessWith(t, func(c *Config) {
		c.DrainDelay = time.Second
		c.ShutdownTimeout = 5 * time.Second
	})
	if code, body := h.get(t, "/readyz"); code != http.StatusOK {
		t.Fatalf("readyz before the drain = %d; body:\n%s", code, body)
	}

	started := time.Now()
	h.cancel()

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		code, body := h.get(t, "/readyz")
		if code == http.StatusServiceUnavailable && strings.Contains(body, "draining: shutting down") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readyz during the drain = %d, want 503 draining; body:\n%s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _ := h.get(t, "/healthz"); code != http.StatusOK {
		t.Errorf("healthz during the drain = %d, want 200", code)
	}
	resp := newConnGet(t, "https://"+h.ls.main.Addr().String()+"/")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("cluster listener during the drain = %d, want 400 (unrecognized path)", resp.StatusCode)
	}
	if !resp.Close {
		t.Error("a cluster response during the drain must carry Connection: close")
	}
	newConnGet(t, "https://"+h.ls.user.Addr().String()+"/")

	select {
	case err := <-h.done:
		h.done <- err
		took := time.Since(started)
		if err != nil {
			t.Errorf("serve = %v, want nil", err)
		}
		if took < time.Second {
			t.Errorf("serve returned after %v, before the 1s drain delay", took)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return within 3s")
	}
	if conn, err := net.DialTimeout("tcp", h.ls.main.Addr().String(), time.Second); err == nil {
		_ = conn.Close()
		t.Error("the cluster listener still accepts after serve returned")
	}
}
