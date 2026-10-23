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

// Package drain is the shutdown sequence the gateway and the console share.
//
// When a Pod terminates, Kubernetes removes it from the Service endpoints at
// once, but kube-proxy and ingress controllers take a few seconds to apply
// that. A server that closes its listeners on SIGTERM refuses the
// connections still routed to it in that window. So the sequence first
// marks the server not ready, keeps serving for a delay while the endpoint
// removal spreads, and only then shuts the traffic servers down, waiting a
// bounded time for in-flight work. The health server stops last, so probes
// keep getting an answer for the whole sequence.
//
// During the delay each response to a request that arrives after the drain
// began tells the client to close the connection after the reply (Connection: close on HTTP/1.1, a GOAWAY on HTTP/2), so
// the client reconnects and the Service routes it to another replica. Idle
// connections stay open: closing one races a client that is reusing it, and
// a client cannot retry a POST it has already sent. Shutdown closes what is
// still idle at the end of the delay.
package drain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Server is one HTTP server on its already-bound listener. A server with a
// TLSConfig serves TLS; one without serves plain HTTP.
type Server struct {
	HTTP     *http.Server
	Listener net.Listener
}

// Group runs a set of traffic servers and one health server, and drains
// them in order when its context ends.
type Group struct {
	// Delay is how long the traffic servers keep accepting after the drain
	// begins. Zero shuts them down at once.
	Delay time.Duration
	// Timeout bounds the wait for in-flight requests, then Pending, after
	// the delay. Zero waits for nothing.
	Timeout time.Duration
	// OnDrain runs when the drain begins; servers use it to fail readiness.
	OnDrain func()
	// Pending tracks background work that handlers start and that must
	// finish before the process exits. Nil means none.
	Pending *sync.WaitGroup
}

// healthShutdownTimeout bounds the health server's shutdown. Probe requests
// are short, so this only covers one that is mid-flight.
const healthShutdownTimeout = 2 * time.Second

// Validate rejects negative durations. Zero is valid for both.
func Validate(delay, timeout time.Duration) error {
	if delay < 0 {
		return fmt.Errorf("drain delay must not be negative, got %v", delay)
	}
	if timeout < 0 {
		return fmt.Errorf("shutdown timeout must not be negative, got %v", timeout)
	}
	return nil
}

// Serve runs every server until ctx ends or one of them fails, then drains.
// It returns the first serve error, or nil after a drain that ctx started.
func (g Group) Serve(ctx context.Context, traffic []Server, health Server) error {
	var draining atomic.Bool
	for _, s := range traffic {
		s.HTTP.Handler = closeWhenDraining(&draining, s.HTTP.Handler)
	}
	all := append(append([]Server(nil), traffic...), health)
	errCh := make(chan error, len(all))
	for _, s := range all {
		go func() { errCh <- serve(s) }()
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}

	if g.OnDrain != nil {
		g.OnDrain()
	}
	// Responses from here on carry Connection: close. Idle connections are
	// left open; see the package comment.
	draining.Store(true)
	slog.Info("draining", "delay", g.Delay, "timeout", g.Timeout)

	// A failed server skips the delay: the replica is going down anyway.
	if serveErr == nil && g.Delay > 0 {
		timer := time.NewTimer(g.Delay)
		select {
		case <-timer.C:
		case serveErr = <-errCh:
		}
		timer.Stop()
	}

	slog.Info("stopping traffic listeners")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), g.Timeout)
	defer cancel()
	results := make([]error, len(traffic))
	var wg sync.WaitGroup
	for i, s := range traffic {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.HTTP.Shutdown(shutdownCtx)
		}()
	}
	wg.Wait()
	timedOut := errors.Join(results...) != nil

	// Wait for Pending only when every handler returned: a handler that
	// outlived the timeout could still call Add during the Wait.
	if !timedOut && g.Pending != nil {
		done := make(chan struct{})
		go func() {
			g.Pending.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-shutdownCtx.Done():
			timedOut = true
		}
	}
	if timedOut {
		slog.Warn("shutdown timeout reached with work in flight", "timeout", g.Timeout)
	}

	healthCtx, cancelHealth := context.WithTimeout(context.Background(), healthShutdownTimeout)
	defer cancelHealth()
	_ = health.HTTP.Shutdown(healthCtx)

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// closeWhenDraining wraps next so that, once draining is set, the response to
// each request that starts after that carries Connection: close. Go's HTTP/1.1 server then closes the connection
// after the reply; its HTTP/2 server drops the header and sends a GOAWAY. The
// ResponseWriter passes through unwrapped, so streaming handlers keep their
// Flusher.
func closeWhenDraining(draining *atomic.Bool, next http.Handler) http.Handler {
	if next == nil {
		next = http.DefaultServeMux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}

func serve(s Server) error {
	if s.HTTP.TLSConfig != nil {
		return s.HTTP.ServeTLS(s.Listener, "", "")
	}
	return s.HTTP.Serve(s.Listener)
}
