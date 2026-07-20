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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// SpendRecorder accumulates token usage per (namespace, provider, model). The
// Phase 5 implementation is in-memory; the cross-replica budget ConfigMap
// exchange lands with the controller integration phase.
type SpendRecorder interface {
	Record(namespace, provider, model string, usage Usage)
}

// MemorySpend is the in-process SpendRecorder.
type MemorySpend struct {
	mu     sync.Mutex
	totals map[string]Usage // key: namespace/provider/model
}

// NewMemorySpend builds an empty recorder.
func NewMemorySpend() *MemorySpend { return &MemorySpend{totals: map[string]Usage{}} }

// Record folds usage into the running total.
func (m *MemorySpend) Record(namespace, provider, model string, usage Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := namespace + "/" + provider + "/" + model
	t := m.totals[key]
	t.InputTokens += usage.InputTokens
	t.OutputTokens += usage.OutputTokens
	m.totals[key] = t
}

// Total returns the accumulated usage for a (namespace, provider, model).
func (m *MemorySpend) Total(namespace, provider, model string) Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totals[namespace+"/"+provider+"/"+model]
}

// hopByHopHeaders are removed per RFC 7230 section 6.1: they are scoped to a
// single connection and must not be relayed across a proxy hop.
var hopByHopHeaders = []string{"Connection", "TE", "Upgrade", "Proxy-Authorization", "Keep-Alive", "Trailer", "Transfer-Encoding"}

// authMaterialHeaders carry inbound authentication material and are stripped
// before the provider credential is injected. Without the explicit strip, a
// live audience-bound Kubernetes credential would be forwarded verbatim into
// third-party provider logs.
var authMaterialHeaders = []string{"Authorization", "X-Api-Key", "Api-Key"}

// handleLLMProxy is the LLM proxy happy path: parse, authorize, inject the
// credential under the forwarded-header contract, relay (buffered or SSE),
// and account for usage. Budget checks, rate limits, and the fallback chain
// land in later phases.
func (s *Server) handleLLMProxy(w http.ResponseWriter, r *http.Request) {
	c := callerFrom(r.Context())
	adapter, ok := adapterForPath(r.URL.Path)
	if !ok {
		badRequest(w, "unrecognized LLM path "+r.URL.Path)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Config.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				errorBody{Type: errRequestTooLarge, Message: fmt.Sprintf("request body exceeds %d bytes", s.Config.MaxBodyBytes)}, 0)
			return
		}
		badRequest(w, "reading request body: "+err.Error())
		return
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		badRequest(w, "request body is not valid JSON")
		return
	}
	qualified, _ := parsed["model"].(string)
	providerName, modelID, ok := splitQualifiedModel(qualified)
	if !ok {
		badRequest(w, `model must be a qualified "{providerRef}/{modelId}" name`)
		return
	}

	provider, denial := s.authorizeRoute(r.Context(), c, providerName, modelID)
	if denial != nil {
		writeError(w, denial.status, errorBody{Type: denial.errType, Message: denial.message, Provider: providerName}, 0)
		return
	}
	typeAdapter, ok := adapterForProviderType(provider.Spec.Type)
	if !ok {
		writeError(w, http.StatusBadRequest, errorBody{
			Type: errInvalidRequest, Provider: providerName,
			Message: fmt.Sprintf("provider type %q is not supported by this gateway build", provider.Spec.Type)}, 0)
		return
	}

	// Budget check (pre-call, last-known spend state): degrade rewrites the
	// model, block returns 429 with Retry-After set to the next period start.
	if decision := s.Budget.Enforce(provider, c.Namespace); decision.Action != "" {
		switch decision.Action {
		case agentryv1alpha1.BudgetActionBlock:
			writeError(w, http.StatusTooManyRequests, errorBody{
				Type: "budget_exhausted", Provider: providerName, Retryable: true,
				Message: fmt.Sprintf("budget for namespace %s on provider %s is exhausted (%d%% used)",
					c.Namespace, providerName, decision.Percent)}, decision.RetryAfter)
			return
		case agentryv1alpha1.BudgetActionDegrade:
			if decision.DegradeTo != "" && decision.DegradeTo != modelID {
				modelID = decision.DegradeTo
			}
		case agentryv1alpha1.BudgetActionWarn:
			slog.Warn("budget threshold crossed", "namespace", c.Namespace,
				"provider", providerName, "percent", decision.Percent)
		}
	}

	credential, err := s.Store.Credential(r.Context(), provider)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errorBody{
			Type: errProviderUnavailable, Provider: providerName,
			Message: "provider credential unavailable"}, 0)
		return
	}

	// Strip the provider prefix so the upstream sees the raw model ID, and
	// apply adapter fixups (e.g. stream_options injection).
	parsed["model"] = modelID
	adapter.fixupRequestBody(parsed)
	outBody, err := json.Marshal(parsed)
	if err != nil {
		badRequest(w, "re-encoding request body: "+err.Error())
		return
	}

	upstreamURL := strings.TrimSuffix(provider.Spec.Endpoint, "/") + r.URL.Path
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(outBody))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	copyForwardedHeaders(upReq.Header, r.Header)
	typeAdapter.injectCredential(upReq.Header, credential)
	upReq.Header.Set("Content-Type", "application/json")

	resp, err := s.upstream().Do(upReq)
	if err != nil {
		status, errType := http.StatusServiceUnavailable, errProviderUnavailable
		if errors.Is(err, os.ErrDeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") {
			status, errType = http.StatusGatewayTimeout, errProviderTimeout
		}
		writeError(w, status, errorBody{Type: errType, Provider: providerName, Message: err.Error()}, 0)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Gateway traffic counts as activity for Agent callers (task Pods do not
	// hibernate, so their traffic is not tracked).
	if c.Workload != nil && c.Workload.Kind == KindAgent {
		s.Activity.RecordTraffic(c.Namespace, c.Workload.Name)
	}

	if isSSE(resp) {
		s.relayStream(w, resp, adapter, c.Namespace, provider, modelID)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway,
			errorBody{Type: errProviderError, Provider: providerName, Message: "reading upstream response: " + err.Error()}, 0)
		return
	}
	if usage, ok := adapter.extractUsage(respBody); ok {
		s.Spend.Record(c.Namespace, providerName, modelID, usage)
		s.Budget.Add(provider, c.Namespace, costOf(provider, modelID, usage))
	}
	copyDownstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// copyForwardedHeaders applies the forwarded-header contract: strip inbound
// auth material, drop hop-by-hop headers, pin Accept-Encoding to identity so
// usage extraction can read the response.
func copyForwardedHeaders(dst, src http.Header) {
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
	for _, h := range authMaterialHeaders {
		dst.Del(h)
	}
	// Headers named by the Connection header are also hop-by-hop.
	for _, name := range strings.Split(src.Get("Connection"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			dst.Del(name)
		}
	}
	for _, h := range hopByHopHeaders {
		dst.Del(h)
	}
	dst.Set("Accept-Encoding", "identity")
	dst.Del("Host")
	dst.Del("Content-Length")
}

func copyDownstreamHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower == "connection" || lower == "transfer-encoding" || lower == "keep-alive" {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

func isSSE(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

// relayStream forwards SSE chunks as they arrive with no buffering, folding
// usage out of the events the adapter recognizes. Spend is recorded after the
// stream completes; a stream ending without usage counts as zero spend.
func (s *Server) relayStream(
	w http.ResponseWriter, resp *http.Response, adapter providerAdapter,
	namespace string, provider *agentryv1alpha1.ModelProvider, modelID string,
) {
	copyDownstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	var usage Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			adapter.accumulateStreamUsage(bytes.TrimSpace(data), &usage)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if usage != (Usage{}) {
		s.Spend.Record(namespace, provider.Name, modelID, usage)
		s.Budget.Add(provider, namespace, costOf(provider, modelID, usage))
	}
}
