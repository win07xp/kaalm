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

package console

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"time"

	"github.com/win07xp/kaalm/internal/tlsutil"
)

// Config carries the console's runtime settings.
type Config struct {
	// OperatorNamespace hosts the console and the gateway (kaalm-system).
	OperatorNamespace string
	// ListenAddr serves the pages and the read API over TLS (default :8443).
	ListenAddr string
	// HealthAddr serves /healthz and /readyz on a dedicated port (default
	// :8081), TLS with no client auth, outside the session machinery.
	HealthAddr string
	// CertFile/KeyFile are the serving cert (kaalm-console-tls), reloaded
	// from disk on rotation. CAFile is the Kaalm CA bundle.
	CertFile string
	KeyFile  string
	CAFile   string
	// MaxMessageBodyBytes caps a test-chat request body (default 1 MiB).
	// The chart passes gateway.maxMessageBodyBytes, the cap the gateway's
	// POST /v1/test-chat applies, so both hops refuse the same size.
	MaxMessageBodyBytes int64
}

// Server is the console: one data layer, two faces (JSON API and pages),
// one gate.
type Server struct {
	Config   Config
	Data     *Data
	Reviewer TokenReviewer
	Gate     *Gate
	Sessions *SessionStore
	Gateway  GatewayClient
}

// NewServer wires a Server from its parts, applying defaults.
func NewServer(cfg Config, data *Data, reviewer TokenReviewer, gate *Gate, gw GatewayClient) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8443"
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = ":8081"
	}
	if cfg.MaxMessageBodyBytes == 0 {
		cfg.MaxMessageBodyBytes = 1 << 20
	}
	return &Server{
		Config:   cfg,
		Data:     data,
		Reviewer: NewCachingReviewer(reviewer),
		Gate:     gate,
		Sessions: NewSessionStore(reviewer),
		Gateway:  gw,
	}
}

// Handler builds the console mux: the read API under /api/v1 (bearer token
// or session), and the server-rendered pages. Every response under /api/
// uses the JSON envelope: an unknown path answers 404 and a wrong method
// 405. The page routes keep the mux's plain-text errors.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The read API (docs/src/console/overview.md#the-read-api). Additive
	// within a minor series.
	routes := []struct {
		method, path string
		h            http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/namespaces", s.apiNamespaces},
		{http.MethodGet, "/api/v1/namespaces/{ns}/agents", s.apiFleet},
		{http.MethodGet, "/api/v1/namespaces/{ns}/agents/{name}", s.apiAgent},
		{http.MethodGet, "/api/v1/namespaces/{ns}/tasks", s.apiTasks},
		{http.MethodGet, "/api/v1/namespaces/{ns}/channels", s.apiChannels},
		{http.MethodGet, "/api/v1/namespaces/{ns}/spend", s.apiSpend},
		{http.MethodPost, "/api/v1/namespaces/{ns}/agents/{name}/chat", s.apiChat},
	}
	// A method pattern wins over its method-less twin, which wins over the
	// /api/ catch-all, so each answer is the most specific one.
	var paths []string
	methods := map[string][]string{}
	for _, rt := range routes {
		mux.HandleFunc(rt.method+" "+rt.path, s.requireAPI(rt.h))
		if _, seen := methods[rt.path]; !seen {
			paths = append(paths, rt.path)
		}
		methods[rt.path] = append(methods[rt.path], rt.method)
	}
	for _, p := range paths {
		mux.HandleFunc(p, apiMethodNotAllowed(methods[p]))
	}
	mux.HandleFunc("/api/", apiNotFound)
	mux.HandleFunc("/api", apiNotFound)

	s.uiRoutes(mux)
	return mux
}

// Run serves the console listener and the health port until ctx is
// cancelled, with the cache janitor running for the same lifetime. Rotation
// is handled by the tlsutil loader per handshake.
func (s *Server) Run(ctx context.Context) error {
	loader := &tlsutil.CertLoader{CertFile: s.Config.CertFile, KeyFile: s.Config.KeyFile, CAFile: s.Config.CAFile}
	if _, err := loader.Certificate(); err != nil {
		return err
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return loader.Certificate()
		},
	}
	main := &http.Server{
		Addr: s.Config.ListenAddr, Handler: s.Handler(), TLSConfig: tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	healthMux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	healthMux.HandleFunc("/healthz", ok)
	healthMux.HandleFunc("/readyz", ok)
	health := &http.Server{
		Addr: s.Config.HealthAddr, Handler: healthMux,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: tlsCfg.GetCertificate},
		ReadHeaderTimeout: 10 * time.Second,
	}

	go s.janitor(ctx, sweepInterval)

	errCh := make(chan error, 2)
	go func() { errCh <- main.ListenAndServeTLS("", "") }()
	go func() { errCh <- health.ListenAndServeTLS("", "") }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = main.Shutdown(shutdownCtx)
		_ = health.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
