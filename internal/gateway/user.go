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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// ActivatorClient asks the controller to wake a hibernated Agent. Injected so
// tests need no controller.
type ActivatorClient interface {
	Wake(ctx context.Context, namespace, name string) error
}

// ControllerActivator is the production ActivatorClient: POST
// /v1/activate/{ns}/{name} on the controller's :9443 over mTLS.
type ControllerActivator struct {
	// BaseURL is the controller activator base, e.g.
	// https://agentry-controller.agentry-system.svc.cluster.local:9443.
	BaseURL string
	Client  *http.Client
}

// Wake posts the activation request. Any non-202 is an error.
func (c *ControllerActivator) Wake(ctx context.Context, namespace, name string) error {
	url := fmt.Sprintf("%s/v1/activate/%s/%s", strings.TrimSuffix(c.BaseURL, "/"), namespace, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("activator returned %d", resp.StatusCode)
	}
	return nil
}

// UserHandler builds the :8080 mux: webhook intake under /channels/ and the
// async polling endpoint. Everything else is 401, matching the
// path-not-registered posture (no path-existence leaks).
func (s *Server) UserHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/channels/", s.handleWebhook)
	mux.HandleFunc("/v1/channels/responses/", s.handlePoll)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		unauthorized(w, "unknown path")
	})
	return mux
}

// handleWebhook is the inbound intake: size cap first (on the raw frame,
// before path resolution, so 413 never leaks which paths exist), then channel
// lookup, auth, normalization, and mode dispatch.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		unauthorized(w, "unknown path")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Config.MaxMessageBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, errorBody{
				Type:    errRequestTooLarge,
				Message: fmt.Sprintf("request body exceeds %d bytes", s.Config.MaxMessageBodyBytes)}, 0)
			return
		}
		badRequest(w, "reading request body: "+err.Error())
		return
	}

	channel, ok := s.Store.ChannelByPath(r.Context(), r.URL.Path)
	if !ok {
		unauthorized(w, "auth failed or path not registered")
		return
	}
	// Write gate: a Terminating channel accepts no new work, which is what
	// makes the delete-time finalizer sweep race-free.
	if channel.Status.Phase == agentryv1alpha1.ChannelTerminating {
		unauthorized(w, "auth failed or path not registered")
		return
	}

	if !s.authenticateWebhook(r.Context(), channel, r, body) {
		s.ChannelHealth.RecordFailure(channel.Spec.Webhook.Path, healthReasonAuthFailed,
			"webhook auth validation failed: 401 Unauthorized")
		unauthorized(w, "auth failed or path not registered")
		return
	}

	env, err := normalize(channel, r, body)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	agent, ok := s.Store.AgentByName(r.Context(), channel.Namespace, channel.Spec.AgentRef.Name)
	if !ok {
		s.ChannelHealth.RecordFailure(channel.Spec.Webhook.Path, healthReasonAgentNotReady,
			"referenced Agent not found")
		writeError(w, http.StatusBadGateway, errorBody{
			Type: errDeliveryFailed, Message: "referenced Agent not found"}, 0)
		return
	}

	if channel.Spec.Webhook.ResponseMode == "async" {
		s.handleAsyncAccept(w, r, channel, agent, env)
		return
	}
	s.handleSyncDelivery(w, r.Context(), channel, agent, env)
}

// handleSyncDelivery runs the wake-then-deliver pipeline with the caller
// attached, bounded by syncDeliveryDeadline.
func (s *Server) handleSyncDelivery(
	w http.ResponseWriter, ctx context.Context,
	channel *agentryv1alpha1.AgentChannel, agent *agentryv1alpha1.Agent, env MessageEnvelope,
) {
	deadline := time.Now().Add(s.Config.SyncDeliveryDeadline)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	respBody, errType, err := s.wakeAndDeliver(ctx, channel, agent, env)
	if err != nil {
		if ctx.Err() != nil {
			// The sync wall-clock budget fired before the pipeline settled.
			writeError(w, http.StatusGatewayTimeout, errorBody{
				Type: errSyncDeadline, Retryable: true,
				Message: fmt.Sprintf("sync-mode wall-clock exceeded %s", s.Config.SyncDeliveryDeadline)}, 0)
			return
		}
		switch errType {
		case errControllerDown:
			writeError(w, http.StatusGatewayTimeout, errorBody{
				Type: errType, Retryable: true,
				Message: "controller activator endpoint unreachable; wake could not be triggered"}, 5)
		case errWakeTimeout:
			writeError(w, http.StatusGatewayTimeout, errorBody{
				Type: errType, Message: err.Error()}, 0)
		case errResponseTooLarge:
			writeError(w, http.StatusRequestEntityTooLarge, errorBody{
				Type: errType, Message: err.Error()}, 0)
		default:
			writeError(w, http.StatusBadGateway, errorBody{
				Type: errDeliveryFailed, Message: err.Error()}, 0)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBody)
}

// wakeAndDeliver wakes a hibernated agent when needed, then runs the bounded
// delivery pipeline. Returns the raw agent response body on success.
func (s *Server) wakeAndDeliver(
	ctx context.Context, channel *agentryv1alpha1.AgentChannel,
	agent *agentryv1alpha1.Agent, env MessageEnvelope,
) (respBody []byte, errType string, err error) {
	if agent.Status.Phase == agentryv1alpha1.AgentHibernated {
		if s.Activator == nil {
			s.ChannelHealth.RecordFailure(channel.Spec.Webhook.Path, healthReasonAgentNotReady,
				"agent hibernated and no activator configured")
			return nil, errControllerDown, fmt.Errorf("no activator configured")
		}
		if err := s.Activator.Wake(ctx, agent.Namespace, agent.Name); err != nil {
			s.ChannelHealth.RecordFailure(channel.Spec.Webhook.Path, healthReasonAgentNotReady,
				"activator unreachable: "+err.Error())
			return nil, errControllerDown, err
		}
		if err := s.waitAgentReachable(ctx, agent); err != nil {
			return nil, errWakeTimeout, fmt.Errorf(
				"agent did not become ready within wakeTimeout (%s)", s.wakeTimeout(agent))
		}
	}
	respBody, err = s.deliverToAgent(ctx, agent, env)
	if err != nil {
		if strings.Contains(err.Error(), "response body exceeded") {
			return nil, errResponseTooLarge, err
		}
		s.ChannelHealth.RecordFailure(channel.Spec.Webhook.Path, healthReasonDispatchFailed, err.Error())
		return nil, errDeliveryFailed, err
	}
	s.ChannelHealth.RecordSuccess(channel.Spec.Webhook.Path)
	return respBody, "", nil
}

func (s *Server) wakeTimeout(agent *agentryv1alpha1.Agent) time.Duration {
	if d := agent.Spec.Lifecycle.WakeTimeout.Duration; d > 0 {
		return d
	}
	return 120 * time.Second
}

// waitAgentReachable polls the agent Service with TCP connects until it
// accepts, bounded by wakeTimeout. Connect failure is the whole hibernation
// detection mechanism; connect success is the readiness signal.
func (s *Server) waitAgentReachable(ctx context.Context, agent *agentryv1alpha1.Agent) error {
	deadline := time.Now().Add(s.wakeTimeout(agent))
	addr := net.JoinHostPort(s.agentServiceHost(agent), fmt.Sprintf("%d", s.agentServicePort(agent)))
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, s.Config.AgentConnectTimeout)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("wake timeout")
}

func (s *Server) agentServiceHost(agent *agentryv1alpha1.Agent) string {
	if s.Config.AgentServiceHostOverride != "" {
		return s.Config.AgentServiceHostOverride
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local", agent.Name, agent.Namespace)
}

func (s *Server) agentServicePort(agent *agentryv1alpha1.Agent) int32 {
	if s.Config.AgentServicePortOverride != 0 {
		return s.Config.AgentServicePortOverride
	}
	if agent.Spec.Service != nil && agent.Spec.Service.Port != 0 {
		return agent.Spec.Service.Port
	}
	return 8080
}

// deliverToAgent runs the bounded agent-delivery pipeline: POST /v1/message
// over mTLS, retried on the 1s/5s/25s schedule (4 attempts total), each
// attempt bounded by agentReadTimeout. The same messageId is reused across
// attempts; agents deduplicate. A 200 with a malformed envelope (missing or
// non-string content) counts as a failed attempt.
func (s *Server) deliverToAgent(
	ctx context.Context, agent *agentryv1alpha1.Agent, env MessageEnvelope,
) ([]byte, error) {
	payload, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("https://%s:%d/v1/message", s.agentServiceHost(agent), s.agentServicePort(agent))

	var lastErr error
	backoff := append([]time.Duration{0}, s.Config.DeliveryBackoff...)
	for attempt, delay := range backoff {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		respBody, err := s.deliverOnce(ctx, url, agent, payload)
		if err == nil {
			return respBody, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "response body exceeded") {
			return nil, err // size violations are not retried
		}
		_ = attempt
	}
	return nil, fmt.Errorf("failed to deliver message to agent after %d attempts: %w", len(backoff), lastErr)
}

func (s *Server) deliverOnce(ctx context.Context, url string, agent *agentryv1alpha1.Agent, payload []byte) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, s.Config.AgentReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client, err := s.agentHTTPClient(agent)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("agent returned %d", resp.StatusCode)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, s.Config.MaxResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(respBody)) > s.Config.MaxResponseBodyBytes {
		return nil, fmt.Errorf("agent response body exceeded %d bytes; externalize large outputs and reference by URL",
			s.Config.MaxResponseBodyBytes)
	}
	var envelope ResponseEnvelope
	if err := json.Unmarshal(respBody, &envelope); err != nil || envelope.Content == nil {
		return nil, fmt.Errorf("agent returned 200 with a malformed response envelope")
	}
	return respBody, nil
}

// agentHTTPClient builds (once) the mTLS client for gateway-to-agent
// delivery: the gateway presents its own cert, verifies the agent's against
// the Agentry CA, and pins ServerName to the agent's Service DNS.
func (s *Server) agentHTTPClient(agent *agentryv1alpha1.Agent) (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: fmt.Sprintf("%s.%s.svc.cluster.local", agent.Name, agent.Namespace),
	}
	if s.Config.InsecureSkipAgentVerify {
		tlsCfg.InsecureSkipVerify = true // dev/test only
	}
	// A missing TLS identity (dev/test) sends no client cert; production
	// always configures the gateway cert for the bidirectional mTLS contract.
	if s.Config.CertFile != "" {
		s.agentClientOnce.Do(func() {
			loader := &certLoader{certFile: s.Config.CertFile, keyFile: s.Config.KeyFile, caFile: s.Config.CAFile}
			if _, err := loader.certificate(); err != nil {
				s.agentClientErr = err
				return
			}
			if _, err := loader.caPool(); err != nil {
				s.agentClientErr = err
				return
			}
			s.agentClientLoader = loader
		})
		if s.agentClientErr != nil {
			return nil, s.agentClientErr
		}
		cert, err := s.agentClientLoader.certificate()
		if err != nil {
			return nil, err
		}
		pool, err := s.agentClientLoader.caPool()
		if err != nil {
			return nil, err
		}
		tlsCfg.Certificates = []tls.Certificate{*cert}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}, nil
}

// NewControllerActivator builds the production activator client from the
// gateway's own TLS identity, pinned to the controller Service DNS.
func NewControllerActivator(operatorNamespace, certFile, keyFile, caFile string) (*ControllerActivator, error) {
	loader := &certLoader{certFile: certFile, keyFile: keyFile, caFile: caFile}
	cert, err := loader.certificate()
	if err != nil {
		return nil, err
	}
	pool, err := loader.caPool()
	if err != nil {
		return nil, err
	}
	host := fmt.Sprintf("agentry-controller.%s.svc.cluster.local", operatorNamespace)
	return &ControllerActivator{
		BaseURL: fmt.Sprintf("https://%s:9443", host),
		Client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12, RootCAs: pool,
				Certificates: []tls.Certificate{*cert}, ServerName: host,
			}},
		},
	}, nil
}
