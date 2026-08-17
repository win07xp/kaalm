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
}

// Server is the console: one data layer, two faces (JSON API and pages),
// one gate.
type Server struct {
	Config   Config
	Data     *Data
	Reviewer TokenReviewer
	Gate     *Gate
	Sessions *SessionStore
	Chat     ChatClient
}

// NewServer wires a Server from its parts, applying defaults.
func NewServer(cfg Config, data *Data, reviewer TokenReviewer, gate *Gate, chat ChatClient) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8443"
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = ":8081"
	}
	return &Server{
		Config:   cfg,
		Data:     data,
		Reviewer: NewCachingReviewer(reviewer),
		Gate:     gate,
		Sessions: NewSessionStore(reviewer),
		Chat:     chat,
	}
}

// Handler builds the console mux: the read API under /api/v1 (bearer token
// or session), and the server-rendered pages.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The read API (docs/src/console/overview.md, The Read API). Additive
	// within a minor series.
	mux.HandleFunc("GET /api/v1/namespaces", s.requireAPI(s.apiNamespaces))
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/agents", s.requireAPI(s.apiFleet))
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/agents/{name}", s.requireAPI(s.apiAgent))
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/tasks", s.requireAPI(s.apiTasks))
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/channels", s.requireAPI(s.apiChannels))
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/spend", s.requireAPI(s.apiSpend))
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/agents/{name}/chat", s.requireAPI(s.apiChat))

	s.uiRoutes(mux)
	return mux
}

// Run serves the console listener and the health port until ctx is
// cancelled. Rotation is handled by the tlsutil loader per handshake.
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
