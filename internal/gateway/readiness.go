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
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// InformerSync names one informer the request path depends on and reports
// whether its initial list has landed. The gateway binary fills
// Server.Informers from the controller-runtime cache; the readiness probe
// reads HasSynced on every call, so it costs a flag read per informer.
type InformerSync struct {
	Name      string
	HasSynced func() bool
}

// readinessTimeout bounds one /readyz call. The checks run concurrently, so
// the whole answer lands within it, inside the kubelet's default 1s probe
// timeout.
const readinessTimeout = 500 * time.Millisecond

// readinessCheck is one line of the /readyz body.
type readinessCheck struct {
	name string
	run  func(ctx context.Context) error
}

// readyzHandler answers the readiness probe with the four checks in
// docs/src/gateways/llm/operations.md#gateway-readiness: a local TLS dial
// of each listener, the informer sync state, and the serving certificate.
// Every check reports one line; any failure answers 503. Once the shutdown
// sequence begins, it answers 503 "draining" without running the checks, so
// the Pod leaves the Service endpoints while it still serves.
func (s *Server) readyzHandler(clusterAddr, userAddr net.Addr, servingCert func() error) http.HandlerFunc {
	checks := []readinessCheck{
		{"cluster_listener", func(ctx context.Context) error { return dialTLS(ctx, clusterAddr) }},
		{"user_listener", func(ctx context.Context) error { return dialTLS(ctx, userAddr) }},
		{"informers", func(context.Context) error { return s.informersSynced() }},
		{"serving_cert", func(context.Context) error { return servingCert() }},
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("draining: shutting down\n"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		results := make([]error, len(checks))
		var wg sync.WaitGroup
		for i, check := range checks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = check.run(ctx)
			}()
		}
		wg.Wait()

		var body strings.Builder
		status := http.StatusOK
		for i, check := range checks {
			if results[i] != nil {
				status = http.StatusServiceUnavailable
				fmt.Fprintf(&body, "%s: %v\n", check.name, results[i])
				continue
			}
			fmt.Fprintf(&body, "%s: ok\n", check.name)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body.String()))
	}
}

// informersSynced reports the informers whose initial sync has not landed.
func (s *Server) informersSynced() error {
	var pending []string
	for _, inf := range s.Informers {
		if !inf.HasSynced() {
			pending = append(pending, inf.Name)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("not synced: %s", strings.Join(pending, ", "))
	}
	return nil
}

// dialTLS completes a TLS handshake with a local listener and hangs up. It
// confirms the listener is bound and serving the certificate; it does not
// verify the chain, since the serving cert names the Service, not loopback.
func dialTLS(ctx context.Context, addr net.Addr) error {
	dialer := &tls.Dialer{Config: &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // loopback handshake check; the peer is this process
	}}
	conn, err := dialer.DialContext(ctx, "tcp", loopbackAddr(addr))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("TLS handshake timed out after %v", readinessTimeout)
		}
		return err
	}
	return conn.Close()
}

// loopbackAddr turns a listener's bound address into one this process can
// dial: a wildcard bind (":8443") is reached over localhost.
func loopbackAddr(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return addr.String()
	}
	if tcp.IP == nil || tcp.IP.IsUnspecified() {
		return net.JoinHostPort("localhost", fmt.Sprint(tcp.Port))
	}
	return tcp.String()
}
