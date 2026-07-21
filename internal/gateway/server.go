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
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"
)

// Config carries the gateway's runtime settings.
type Config struct {
	// OperatorNamespace hosts the gateway (agentry-system); the controller
	// SAN check and the gateway endpoint derivation use it.
	OperatorNamespace string
	// ListenAddr is the LLM listener (default :8443).
	ListenAddr string
	// HealthAddr serves /healthz and /readyz over TLS with no client auth on
	// a dedicated port, outside the listener auth profiles.
	HealthAddr string
	// CertFile/KeyFile are the serving cert (agentry-gateway-tls), reloaded
	// from disk on rotation.
	CertFile string
	KeyFile  string
	// CAFile is the Agentry CA bundle used for the inbound ClientCAs pool.
	CAFile string
	// MaxBodyBytes caps inbound LLM request bodies (default 4 MiB).
	MaxBodyBytes int64
	// MaxFallbackDepth bounds the total providers attempted per request,
	// including the primary (default 3).
	MaxFallbackDepth int
	// Replicas returns the live gateway replica count for rate-limit
	// division; nil means a single replica.
	Replicas func() int
	// UpstreamTimeout bounds each upstream provider call.
	UpstreamTimeout time.Duration
	// UpstreamCAs, when set, replaces the system pool for upstream TLS
	// verification (the agentry-upstream-ca mechanism; tests use it too).
	UpstreamCAs *x509.CertPool
	// DisableSourceIPCheck skips the source-IP cross-check (dev/test only).
	DisableSourceIPCheck bool

	// UserListenAddr is the User Gateway listener (default :8080).
	UserListenAddr string
	// MaxMessageBodyBytes caps inbound webhook bodies (default 1 MiB).
	MaxMessageBodyBytes int64
	// MaxResponseBodyBytes caps agent replies (default 900 KiB, headroom
	// under the ~1 MiB ConfigMap object cap).
	MaxResponseBodyBytes int64
	// AgentReadTimeout bounds each delivery attempt (default 10s).
	AgentReadTimeout time.Duration
	// AgentConnectTimeout is the hibernation-detection connect bound (1s).
	AgentConnectTimeout time.Duration
	// SyncDeliveryDeadline bounds sync-mode wall-clock (default 30s).
	SyncDeliveryDeadline time.Duration
	// DeliveryBackoff is the agent-delivery retry schedule (1s, 5s, 25s).
	DeliveryBackoff []time.Duration
	// CallbackBackoff is the callback retry schedule (1s, 5s, 25s).
	CallbackBackoff []time.Duration
	// ChannelHealthWindow is the rolling health window (default 5m).
	ChannelHealthWindow time.Duration
	// AgentServiceHostOverride / AgentServicePortOverride redirect agent
	// delivery dials (dev/test only).
	AgentServiceHostOverride string
	AgentServicePortOverride int32
	// InsecureSkipAgentVerify disables agent cert verification (dev only).
	InsecureSkipAgentVerify bool
	// CallbackCAs, when set, replaces the system pool for callback TLS (tests).
	CallbackCAs *x509.CertPool
}

// Server is the Agentry Gateway's :8443 surface.
type Server struct {
	Config        Config
	Store         Store
	Auth          *Authenticator
	Spend         SpendRecorder
	Activity      *ActivityStore
	Budget        *BudgetLedger
	ChannelHealth *ChannelHealthStore
	RateLimiter   *RateLimiter
	Metrics       *Metrics
	// Recorder emits Kubernetes Events (runtime FallbackIneligible,
	// CredentialsInvalid). A nil recorder no-ops.
	Recorder EventRecorder
	// Activator wakes hibernated Agents via the controller (nil in tests or
	// when the controller identity is not configured).
	Activator ActivatorClient
	// Async persists async webhook response records.
	Async AsyncRecords
	// Completions patches per-task completion mailboxes.
	Completions CompletionWriter

	upstreamOnce   sync.Once
	upstreamClient *http.Client

	agentClientOnce   sync.Once
	agentClientLoader *certLoader
	agentClientErr    error
}

// NewServer wires a Server from its parts, applying defaults.
func NewServer(cfg Config, store Store, tokens *TokenAuthenticator, spend SpendRecorder) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8443"
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = ":8081"
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 4 << 20
	}
	if cfg.UpstreamTimeout == 0 {
		cfg.UpstreamTimeout = 120 * time.Second
	}
	if cfg.UserListenAddr == "" {
		cfg.UserListenAddr = ":8080"
	}
	if cfg.MaxMessageBodyBytes == 0 {
		cfg.MaxMessageBodyBytes = 1 << 20
	}
	if cfg.MaxResponseBodyBytes == 0 {
		cfg.MaxResponseBodyBytes = 900 << 10
	}
	if cfg.AgentReadTimeout == 0 {
		cfg.AgentReadTimeout = 10 * time.Second
	}
	if cfg.AgentConnectTimeout == 0 {
		cfg.AgentConnectTimeout = time.Second
	}
	if cfg.SyncDeliveryDeadline == 0 {
		cfg.SyncDeliveryDeadline = 30 * time.Second
	}
	if cfg.DeliveryBackoff == nil {
		cfg.DeliveryBackoff = []time.Duration{time.Second, 5 * time.Second, 25 * time.Second}
	}
	if cfg.CallbackBackoff == nil {
		cfg.CallbackBackoff = []time.Duration{time.Second, 5 * time.Second, 25 * time.Second}
	}
	if cfg.MaxFallbackDepth == 0 {
		cfg.MaxFallbackDepth = 3
	}
	return &Server{
		Config: cfg,
		Store:  store,
		Auth: &Authenticator{
			Store: store, Tokens: tokens,
			OperatorNamespace:    cfg.OperatorNamespace,
			DisableSourceIPCheck: cfg.DisableSourceIPCheck,
		},
		Spend:         spend,
		Activity:      NewActivityStore(),
		Budget:        NewBudgetLedger(),
		ChannelHealth: NewChannelHealthStore(cfg.ChannelHealthWindow),
		RateLimiter:   NewRateLimiter(cfg.Replicas),
	}
}

// Handler builds the :8443 mux with the per-path auth regimes. The mapping
// mirrors the listener profile table in docs/src/gateways/overview.md.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// LLM proxy paths: dual-mode (mTLS SAN or bearer token).
	mux.HandleFunc("/v1/messages", s.Auth.LLMPaths(s.handleLLMProxy))
	mux.HandleFunc("/v1/chat/completions", s.Auth.LLMPaths(s.handleLLMProxy))
	mux.HandleFunc("/v1/completions", s.Auth.LLMPaths(s.handleLLMProxy))

	// Agent-report paths: mTLS-only, kind split at the handler. The
	// task-complete body lands with the user-gateway phase.
	mux.HandleFunc("/v1/agent/heartbeat", s.Auth.AgentReportPaths(KindAgent, s.handleHeartbeat))
	mux.HandleFunc("/v1/task/complete", s.Auth.AgentReportPaths(KindAgentTask, s.handleTaskComplete))

	// Controller-only paths: controller SAN required.
	mux.HandleFunc("/v1/activity", s.Auth.ControllerPaths(s.handleActivity))
	mux.HandleFunc("/v1/channels/health", s.Auth.ControllerPaths(s.handleChannelsHealth))

	// Anything else on the LLM listener is an unrecognized path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		badRequest(w, "unrecognized path "+r.URL.Path)
	})
	return mux
}

// TLSConfig builds the listener TLS configuration: VerifyClientCertIfGiven so
// bearer-token callers complete the handshake, with the serving cert and CA
// pool reloaded from disk on rotation (kubelet swaps the projected volume).
func (s *Server) TLSConfig() (*tls.Config, error) {
	loader := &certLoader{certFile: s.Config.CertFile, keyFile: s.Config.KeyFile, caFile: s.Config.CAFile}
	if _, err := loader.certificate(); err != nil {
		return nil, err
	}
	pool, err := loader.caPool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  pool,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return loader.certificate()
		},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			// Rebuild ClientCAs when the CA bundle rotates: a CA change must
			// refresh the inbound trust pool, not only the serving cert.
			pool, err := loader.caPool()
			if err != nil {
				return nil, err
			}
			return &tls.Config{
				MinVersion: tls.VersionTLS12,
				ClientAuth: tls.VerifyClientCertIfGiven,
				ClientCAs:  pool,
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					return loader.certificate()
				},
			}, nil
		},
	}, nil
}

// Run serves the LLM listener and the health port until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	tlsCfg, err := s.TLSConfig()
	if err != nil {
		return err
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

	user := &http.Server{
		Addr: s.Config.UserListenAddr, Handler: s.UserHandler(),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: tlsCfg.GetCertificate},
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 3)
	go func() { errCh <- main.ListenAndServeTLS("", "") }()
	go func() { errCh <- health.ListenAndServeTLS("", "") }()
	go func() { errCh <- user.ListenAndServeTLS("", "") }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = main.Shutdown(shutdownCtx)
		_ = health.Shutdown(shutdownCtx)
		_ = user.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// upstream returns the shared provider-facing HTTP client.
func (s *Server) upstream() *http.Client {
	s.upstreamOnce.Do(func() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if s.Config.UpstreamCAs != nil {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: s.Config.UpstreamCAs}
		}
		s.upstreamClient = &http.Client{Timeout: s.Config.UpstreamTimeout, Transport: transport}
	})
	return s.upstreamClient
}

// certLoader reloads the serving cert and CA bundle from disk when their
// mtimes change, so cert-manager rotation needs no process restart.
type certLoader struct {
	certFile, keyFile, caFile string

	mu        sync.Mutex
	cert      *tls.Certificate
	certMtime time.Time
	pool      *x509.CertPool
	poolMtime time.Time
}

func (l *certLoader) certificate() (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, err := os.Stat(l.certFile)
	if err != nil {
		return nil, err
	}
	if l.cert != nil && info.ModTime().Equal(l.certMtime) {
		return l.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(l.certFile, l.keyFile)
	if err != nil {
		if l.cert != nil {
			return l.cert, nil // keep serving the old cert through a partial write
		}
		return nil, err
	}
	l.cert, l.certMtime = &cert, info.ModTime()
	return l.cert, nil
}

func (l *certLoader) caPool() (*x509.CertPool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, err := os.Stat(l.caFile)
	if err != nil {
		return nil, err
	}
	if l.pool != nil && info.ModTime().Equal(l.poolMtime) {
		return l.pool, nil
	}
	pem, err := os.ReadFile(l.caFile)
	if err != nil {
		if l.pool != nil {
			return l.pool, nil
		}
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		if l.pool != nil {
			return l.pool, nil
		}
		return nil, errors.New("no certificates parsed from CA bundle")
	}
	l.pool, l.poolMtime = pool, info.ModTime()
	return l.pool, nil
}
