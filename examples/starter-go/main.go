// Copyright 2026. Licensed under the Apache License, Version 2.0.
//
// Command starter-go is a minimal, working implementation of the Agentry
// Runtime Contract, meant to be copied and modified. It handles every
// repetitive and error-prone part of the contract so you only replace
// handleMessage in handler.go. See docs/src/runtime/starter-templates.md.
package main

import (
	"container/list"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	gatewaySANLocal = "agentry-gateway.agentry-system.svc.cluster.local"
	gatewaySANShort = "agentry-gateway.agentry-system.svc"
	dedupBufferSize = 1024
	heartbeatPeriod = 30 * time.Second
)

// MessageEnvelope is the inbound message shape (contract item 4).
type MessageEnvelope struct {
	MessageID   string            `json:"messageId"`
	ChannelType string            `json:"channelType"`
	ChannelID   string            `json:"channelId"`
	UserID      string            `json:"userId"`
	SessionID   string            `json:"sessionId,omitempty"`
	Content     string            `json:"content"`
	Attachments []json.RawMessage `json:"attachments"`
	Metadata    map[string]any    `json:"metadata"`
}

// ResponseEnvelope is the reply shape; content is required.
type ResponseEnvelope struct {
	Content     string            `json:"content"`
	Attachments []json.RawMessage `json:"attachments"`
	Metadata    map[string]any    `json:"metadata"`
}

type agent struct {
	reloader   *certReloader
	gatewayURL string
	healthPort string
	gatewayCli *http.Client
	isTask     bool
	dedup      *dedupBuffer
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[agent] ")

	healthPort := envOr("AGENTRY_HEALTH_PORT", "8080")
	certFile := envOr("AGENTRY_TLS_CERT", "/var/run/agentry/tls.crt")
	keyFile := envOr("AGENTRY_TLS_KEY", "/var/run/agentry/tls.key")
	caFile := envOr("AGENTRY_CA_CERT", "/var/run/agentry/ca.crt")

	reloader, err := newCertReloader(certFile, keyFile, caFile)
	if err != nil {
		log.Fatalf("initializing TLS material: %v", err)
	}
	if err := reloader.watch(log.Printf); err != nil {
		log.Fatalf("starting cert watch: %v", err)
	}

	a := &agent{
		reloader:   reloader,
		gatewayURL: strings.TrimSuffix(os.Getenv("AGENTRY_GATEWAY_ENDPOINT"), "/"),
		healthPort: healthPort,
		dedup:      newDedupBuffer(dedupBufferSize),
	}
	a.isTask = workloadIsTask(reloader.certificate())
	a.gatewayCli = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: reloader.clientTLSConfig()},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/message", a.handleV1Message)

	server := &http.Server{
		Addr:              ":" + healthPort,
		Handler:           mux,
		TLSConfig:         reloader.serverTLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if a.shouldHeartbeat() {
		go a.heartbeatLoop(ctx)
	}

	go func() {
		log.Printf("serving HTTPS on :%s (task-mode=%v)", healthPort, a.isTask)
		if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed: %v", err)
		}
	}()

	if status := taskAutocompleteStatus(a.isTask, os.Getenv("AGENTRY_TASK_AUTOCOMPLETE")); status != "" {
		go func() {
			// Completing at pod startup can race the gateway's source-IP check:
			// its Pod informer may not have indexed this pod's IP yet, so the
			// first attempt can transiently 401. A real task does work before
			// reporting, avoiding this; the smoke hook retries a few times.
			for attempt := 1; attempt <= 6; attempt++ {
				err := a.completeTask(ctx, status, "auto-complete on startup", nil)
				if err == nil {
					log.Printf("task auto-complete reported %q (attempt %d)", status, attempt)
					return
				}
				log.Printf("task auto-complete attempt %d failed: %v", attempt, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
			log.Printf("task auto-complete giving up after %d attempts; last status %q not reported", 6, status)
		}()
	}

	<-ctx.Done()
	log.Print("SIGTERM received; draining")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	log.Print("shut down cleanly")
}

// handleV1Message enforces the per-path mTLS contract (item 4), deduplicates
// on messageId (item 7), and delegates to the user-owned handleMessage.
func (a *agent) handleV1Message(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}
	if !gatewaySANMatches(r.TLS.PeerCertificates[0]) {
		http.Error(w, "gateway identity required", http.StatusForbidden)
		return
	}

	var env MessageEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		http.Error(w, "invalid message envelope", http.StatusBadRequest)
		return
	}

	// Dedup: return the cached reply for a redelivered messageId without
	// reprocessing (the gateway retries reuse the same id).
	if cached, ok := a.dedup.get(env.MessageID); ok {
		writeJSON(w, cached)
		return
	}

	resp, err := handleMessage(r.Context(), env)
	if err != nil {
		http.Error(w, "handler error", http.StatusInternalServerError)
		return
	}
	a.dedup.put(env.MessageID, resp)
	writeJSON(w, resp)
}

// shouldHeartbeat decides whether to run the heartbeat loop: auto (default)
// emits in Agent mode only; off never emits. There is no force-on for tasks:
// the endpoint rejects task callers by design.
func (a *agent) shouldHeartbeat() bool {
	switch os.Getenv("AGENTRY_TEMPLATE_HEARTBEAT") {
	case "off":
		return false
	default: // auto
		return !a.isTask
	}
}

func (a *agent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.gatewayURL+"/v1/agent/heartbeat", nil)
			if err != nil {
				continue
			}
			resp, err := a.gatewayCli.Do(req)
			if err != nil {
				log.Printf("heartbeat failed: %v", err)
				continue
			}
			_ = resp.Body.Close()
		}
	}
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
// shape ({name}.{ns}.task.agentry.io), so the heartbeat loop needs no config.
func workloadIsTask(cert *tls.Certificate) bool {
	if cert == nil || len(cert.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return false
	}
	for _, san := range leaf.DNSNames {
		if strings.HasSuffix(san, ".task.agentry.io") {
			return true
		}
	}
	return false
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

// dedupBuffer is an LRU of the last N messageIds and their cached responses
// (contract item 7). Hibernation-enabled adopters back this with the PVC.
type dedupBuffer struct {
	mu    sync.Mutex
	size  int
	order *list.List
	items map[string]*list.Element
}

type dedupEntry struct {
	id    string
	reply ResponseEnvelope
}

func newDedupBuffer(size int) *dedupBuffer {
	return &dedupBuffer{size: size, order: list.New(), items: map[string]*list.Element{}}
}

func (d *dedupBuffer) get(id string) (ResponseEnvelope, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.items[id]; ok {
		d.order.MoveToFront(el)
		return el.Value.(*dedupEntry).reply, true
	}
	return ResponseEnvelope{}, false
}

func (d *dedupBuffer) put(id string, reply ResponseEnvelope) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.items[id]; ok {
		el.Value.(*dedupEntry).reply = reply
		d.order.MoveToFront(el)
		return
	}
	el := d.order.PushFront(&dedupEntry{id: id, reply: reply})
	d.items[id] = el
	for d.order.Len() > d.size {
		oldest := d.order.Back()
		if oldest == nil {
			break
		}
		d.order.Remove(oldest)
		delete(d.items, oldest.Value.(*dedupEntry).id)
	}
}

// taskAutocompleteStatus returns the status an AgentTask should self-report on
// startup, or "" to disable. Honored only in task mode; the value comes from
// AGENTRY_TASK_AUTOCOMPLETE ("success" or "failure"). This is a smoke/e2e hook:
// a real task reports completion from its own work, not on startup.
func taskAutocompleteStatus(isTask bool, env string) string {
	if !isTask {
		return ""
	}
	return env
}
