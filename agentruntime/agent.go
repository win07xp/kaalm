// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	gatewaySANLocal = "kaalm-gateway.kaalm-system.svc.cluster.local"
	gatewaySANShort = "kaalm-gateway.kaalm-system.svc"
)

// heartbeatPeriod paces the Agent-mode heartbeat loop (contract item 5). A
// package variable so tests can compress it.
var heartbeatPeriod = 30 * time.Second

// autocompleteRetryDelay paces the startup auto-complete smoke hook's
// retries. A package variable so tests can compress it.
var autocompleteRetryDelay = 5 * time.Second

// Agent is a configured runtime instance. Gateway and Memory are the
// capabilities a Handler reaches by closing over the Agent; everything else
// is contract machinery Run drives.
type Agent struct {
	// Gateway is the mTLS client for the Kaalm gateway.
	Gateway *Gateway
	// Memory is the handler's persistent key-value store.
	Memory *Memory

	reloader   *certReloader
	store      *store
	healthPort string
	isTask     bool
}

// New builds an Agent from the standard Kaalm environment: TLS material from
// the projected volume ($KAALM_TLS_CERT, $KAALM_TLS_KEY, $KAALM_CA_CERT), the
// gateway endpoint from $KAALM_GATEWAY_ENDPOINT, the serving port from
// $KAALM_HEALTH_PORT, and the persistence mount from $KAALM_MEMORY_DIR.
// Workload mode (Agent vs AgentTask) is detected from the certificate's SAN
// shape, so no extra configuration distinguishes the two.
func New() (*Agent, error) {
	reloader, err := newCertReloader(
		envOr("KAALM_TLS_CERT", "/var/run/kaalm/tls.crt"),
		envOr("KAALM_TLS_KEY", "/var/run/kaalm/tls.key"),
		envOr("KAALM_CA_CERT", "/var/run/kaalm/ca.crt"),
	)
	if err != nil {
		return nil, err
	}
	st := newStore(envOr("KAALM_MEMORY_DIR", defaultMemoryDir))
	return &Agent{
		Gateway:    newGateway(os.Getenv("KAALM_GATEWAY_ENDPOINT"), reloader),
		Memory:     &Memory{s: st},
		reloader:   reloader,
		store:      st,
		healthPort: envOr("KAALM_HEALTH_PORT", "8080"),
		isTask:     workloadIsTask(reloader.certificate()),
	}, nil
}

// IsTask reports whether this workload runs as an AgentTask, detected from
// the mounted client certificate's SAN shape ({name}.{ns}.task.kaalm.io).
func (a *Agent) IsTask() bool {
	return a.isTask
}

// Run serves the runtime contract until ctx is canceled: HTTPS with rotating
// certificates, per-path gateway mTLS on /v1/message, messageId dedup, and
// (in Agent mode) heartbeats. A nil handler serves the built-in echo default.
// Run returns nil after a clean drain on cancellation, or the serving error.
func (a *Agent) Run(ctx context.Context, h Handler) error {
	ln, err := net.Listen("tcp", ":"+a.healthPort)
	if err != nil {
		return err
	}
	return a.serve(ctx, ln, h)
}

// serve is Run minus the listener, so tests can bind :0 and know the port.
func (a *Agent) serve(ctx context.Context, ln net.Listener, h Handler) error {
	if h == nil {
		h = echoHandler
	}
	if err := a.reloader.watch(log.Printf); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/message", a.messageHandler(h))

	server := &http.Server{
		Handler:           mux,
		TLSConfig:         a.reloader.serverTLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if a.shouldHeartbeat() {
		go a.heartbeatLoop(ctx)
	}
	if status := taskAutocompleteStatus(a.isTask, os.Getenv("KAALM_TASK_AUTOCOMPLETE")); status != "" {
		go a.autocomplete(ctx, status)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("serving HTTPS on %s (task-mode=%v)", ln.Addr(), a.isTask)
		errCh <- server.ServeTLS(ln, "", "")
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Print("shutdown signal received; draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		log.Print("shut down cleanly")
		return nil
	}
}

// messageHandler enforces the per-path mTLS contract (item 4), deduplicates
// on messageId (item 7), and delegates to the Handler.
func (a *Agent) messageHandler(h Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		if !gatewaySANMatches(r.TLS.PeerCertificates[0]) {
			http.Error(w, "gateway identity required", http.StatusForbidden)
			return
		}

		var env Envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, "invalid message envelope", http.StatusBadRequest)
			return
		}

		// Dedup: return the cached reply for a redelivered messageId without
		// reprocessing (the gateway retries reuse the same id). Backed by the
		// mounted volume, so a Pod recreated by a wake still recognizes a
		// messageId it answered before hibernating (contract item 7). An
		// id-less envelope is dispatched but never cached: "" is not an
		// identity, and caching under it would glue unrelated messages
		// together.
		if env.MessageID != "" {
			if cached, ok := a.store.recall(env.MessageID); ok {
				writeJSON(w, cached)
				return
			}
		}

		// The gateway's delivery may carry W3C trace context; hand it to the
		// handler so every gateway call it makes stays on the same trace.
		ctx := r.Context()
		if tp := r.Header.Get("Traceparent"); tp != "" {
			ctx = withTraceContext(ctx, tp, r.Header.Get("Tracestate"))
		}
		resp, err := h(ctx, env)
		if err != nil {
			log.Printf("handler error for messageId %q: %v", env.MessageID, err)
			http.Error(w, "handler error", http.StatusInternalServerError)
			return
		}
		if env.MessageID != "" {
			a.store.remember(env.MessageID, resp, dedupBufferSize)
		}
		writeJSON(w, resp)
	}
}

// shouldHeartbeat decides whether to run the heartbeat loop: auto (default)
// emits in Agent mode only; off never emits. There is no force-on for tasks:
// the endpoint rejects task callers by design. With no gateway endpoint
// configured there is nothing to heartbeat at.
func (a *Agent) shouldHeartbeat() bool {
	if a.Gateway.Endpoint() == "" {
		return false
	}
	switch os.Getenv("KAALM_TEMPLATE_HEARTBEAT") {
	case "off":
		return false
	default: // auto
		return !a.isTask
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := a.Gateway.Post(ctx, "/v1/agent/heartbeat", nil)
			if err != nil {
				log.Printf("heartbeat failed: %v", err)
				continue
			}
			_ = resp.Body.Close()
		}
	}
}

// autocomplete reports the KAALM_TASK_AUTOCOMPLETE status at startup. This is
// a smoke/e2e hook: a real task reports completion from its own work.
// Completing at pod startup can race the gateway's source-IP check (its Pod
// informer may not have indexed this pod's IP yet), so it retries briefly.
func (a *Agent) autocomplete(ctx context.Context, status string) {
	const attempts = 6
	for attempt := 1; attempt <= attempts; attempt++ {
		err := a.CompleteTask(ctx, status, "auto-complete on startup", nil)
		if err == nil {
			log.Printf("task auto-complete reported %q (attempt %d)", status, attempt)
			return
		}
		log.Printf("task auto-complete attempt %d failed: %v", attempt, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(autocompleteRetryDelay):
		}
	}
	log.Printf("task auto-complete giving up after %d attempts; last status %q not reported", attempts, status)
}

// gatewaySANMatches reports whether a cert names the gateway Service DNS.
func gatewaySANMatches(cert *x509.Certificate) bool {
	for _, san := range cert.DNSNames {
		if san == gatewaySANLocal || san == gatewaySANShort {
			return true
		}
	}
	return false
}

// workloadIsTask detects AgentTask mode from the mounted client cert's SAN
// shape ({name}.{ns}.task.kaalm.io), so the heartbeat loop needs no config.
func workloadIsTask(cert *tls.Certificate) bool {
	if cert == nil || len(cert.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return false
	}
	for _, san := range leaf.DNSNames {
		if strings.HasSuffix(san, ".task.kaalm.io") {
			return true
		}
	}
	return false
}

// taskAutocompleteStatus returns the status an AgentTask should self-report on
// startup, or "" to disable. Honored only in task mode; the value comes from
// KAALM_TASK_AUTOCOMPLETE ("success" or "failure").
func taskAutocompleteStatus(isTask bool, env string) string {
	if !isTask {
		return ""
	}
	return env
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
