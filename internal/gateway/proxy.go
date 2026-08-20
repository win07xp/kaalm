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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
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

// workloadKey attributes spend to the attested workload. Token-mode callers
// (gateway-only tier) carry a namespace but no workload identity and land in
// the visible unattributed bucket, so per-workload rows still sum to the
// namespace total.
func workloadKey(c *caller) string {
	if c.Workload == nil {
		return UnattributedWorkload
	}
	if c.Workload.Kind == KindAgentTask {
		return "task/" + c.Workload.Name
	}
	return "agent/" + c.Workload.Name
}

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

	bodyLog("llm request", body)

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

	// Budget admission on the PRIMARY (last-known spend state, no pre-call
	// estimation): degrade rewrites the model; block, throttle, and
	// fail-closed all return with no fallback (a capped namespace must not
	// drain a fallback provider's budget). Inside a hard provider's
	// boundary region, Admit also acquires the serialized admission slot;
	// the deferred settle(0) is the safety net for every early-return path
	// (settle is idempotent, so the real settle always wins).
	workload := workloadKey(c)
	decision, primarySettle := s.Budget.Admit(provider, c.Namespace, workload)
	if primarySettle != nil {
		defer primarySettle(0)
	}
	if !s.applyBudgetDecision(w, decision, primarySettle != nil, providerName, c.Namespace, &modelID) {
		return
	}

	// Rate limit per (namespace, model), sharing the configured ceiling
	// across live replicas.
	if !s.RateLimiter.Allow(provider, c.Namespace, modelID) {
		s.Metrics.LLMRequest(providerName, modelID, c.Namespace, "rate_limited")
		writeError(w, http.StatusTooManyRequests, errorBody{
			Type: errRateLimited, Provider: providerName, Retryable: true,
			Message: fmt.Sprintf("rate limit exceeded for namespace %s on model %s", c.Namespace, modelID)}, 1)
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

	// Gateway traffic counts as activity for Agent callers (task Pods do not
	// hibernate, so their traffic is not tracked).
	if c.Workload != nil && c.Workload.Kind == KindAgent {
		s.Activity.RecordTraffic(c.Namespace, c.Workload.Name)
	}

	// Walk the fallback tree. Each attempt forwards to one candidate with its
	// own credential and endpoint; the first 2xx (or a non-fallbackable 4xx)
	// wins. observed collects the failure classes for the exhaustion mapping.
	st := &walkState{
		primary: provider, namespace: c.Namespace, workload: workload, modelID: modelID,
		maxDepth: s.Config.MaxFallbackDepth, visited: map[string]bool{},
		observed: map[failClass]bool{}, primarySettle: primarySettle,
	}
	res, ok := s.tryWithFallbacks(r.Context(), provider, st, func(ctx context.Context, cand *kaalmv1alpha1.ModelProvider) forwardResult {
		fr := s.forwardOnce(ctx, r, cand, outBody, adapter, typeAdapter, modelID)
		if fr.class != classNone {
			st.observed[fr.class] = true
		}
		// Count every attempt on a non-primary candidate as a fallback,
		// whatever its outcome (a succeeding attempt is labeled "success").
		if cand.Name != provider.Name {
			reason := fallbackReasonSuccess
			if fr.class != classNone {
				reason = failClassName(fr.class)
			}
			s.Metrics.Fallback(provider.Name, cand.Name, reason)
		}
		return fr
	})
	if !ok {
		status, body, retryAfter := exhaustionError(st.observed, st.maxRetryAfter, providerName)
		s.Metrics.LLMRequest(providerName, modelID, c.Namespace, "error")
		writeError(w, status, body, retryAfter)
		return
	}
	defer func() { _ = res.resp.Body.Close() }()

	// A non-fallbackable failure (400/422/other 4xx) is relayed verbatim.
	if res.resp.StatusCode < 200 || res.resp.StatusCode > 299 {
		if res.settle != nil {
			res.settle(0)
		}
		s.Metrics.LLMRequest(res.provider, modelID, c.Namespace, "error")
		bodyLog("llm response", res.body)
		copyDownstreamHeaders(w.Header(), res.resp.Header)
		w.WriteHeader(res.resp.StatusCode)
		_, _ = w.Write(res.body)
		return
	}

	s.Metrics.LLMRequest(res.provider, modelID, c.Namespace, "ok")
	if isSSE(res.resp) {
		s.relayStream(w, res.resp, adapter, c.Namespace, workload, res.chosen, modelID, res.settle)
		return
	}
	if usage, ok := adapter.extractUsage(res.body); ok {
		s.settleUsage(res.chosen, c.Namespace, workload, modelID, usage, res.settle)
	} else if res.settle != nil {
		res.settle(0)
	}
	bodyLog("llm response", res.body)
	copyDownstreamHeaders(w.Header(), res.resp.Header)
	w.WriteHeader(res.resp.StatusCode)
	_, _ = w.Write(res.body)
}

// forwardOnce forwards the request to a single candidate provider under the
// forwarded-header contract and classifies the outcome for the fallback walk.
func (s *Server) forwardOnce(
	ctx context.Context, r *http.Request, provider *kaalmv1alpha1.ModelProvider,
	outBody []byte, adapter, typeAdapter providerAdapter, modelID string,
) forwardResult {
	credential, err := s.Store.Credential(ctx, provider)
	if err != nil {
		return forwardResult{fallilable: true, class: classConnect, err: err}
	}
	upstreamURL := strings.TrimSuffix(provider.Spec.Endpoint, "/") + adapter.upstreamPath(r.URL.Path, modelID)
	upReq, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(outBody))
	if err != nil {
		return forwardResult{fallilable: true, class: classConnect, err: err}
	}
	copyForwardedHeaders(upReq.Header, r.Header)
	typeAdapter.injectCredential(upReq.Header, credential)
	upReq.Header.Set("Content-Type", "application/json")

	resp, err := s.upstream().Do(upReq)
	if err != nil {
		class := classConnect
		if errors.Is(err, os.ErrDeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") {
			class = classTimeout
		}
		return forwardResult{fallilable: true, class: class, err: err}
	}

	fallback, class := isFallbackable(resp.StatusCode)
	fr := forwardResult{resp: resp, fallilable: fallback, class: class, provider: provider.Name, chosen: provider}
	if isSSE(resp) {
		// A streaming 2xx: relay begins after the walk; no body buffering.
		return fr
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return forwardResult{fallilable: true, class: classConnect, err: err}
	}
	// Re-wrap the buffered body so downstream reads still work.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	fr.body = body
	return fr
}

// applyBudgetDecision renders a budget admission outcome: it writes the
// terminal response for fail-closed, throttled, and blocked outcomes
// (returning false), applies the degrade rewrite through modelID, and emits
// the metrics. See the admission comment at the call site.
func (s *Server) applyBudgetDecision(
	w http.ResponseWriter, decision budgetDecision, engaged bool,
	providerName, namespace string, modelID *string,
) bool {
	if decision.MarginRaisedNow {
		s.Metrics.BudgetBoundary(providerName, namespace, "margin_raised")
	}
	switch {
	case decision.Unavailable:
		s.Metrics.BudgetBoundary(providerName, namespace, "fail_closed")
		writeError(w, http.StatusServiceUnavailable, errorBody{
			Type: errBudgetUnavailable, Provider: providerName, Retryable: true,
			Message: fmt.Sprintf("budget state for provider %s cannot be verified inside the boundary region; failing closed", providerName)}, 1)
		return false
	case decision.Throttled:
		s.Metrics.BudgetBoundary(providerName, namespace, "throttled")
		writeError(w, http.StatusTooManyRequests, errorBody{
			Type: errBudgetThrottled, Provider: providerName, Retryable: true,
			Message: fmt.Sprintf("boundary admission for namespace %s on provider %s is busy; retry shortly", namespace, providerName)}, 1)
		return false
	case decision.Action == kaalmv1alpha1.BudgetActionBlock:
		s.Metrics.BudgetThreshold(providerName, namespace, kaalmv1alpha1.BudgetActionBlock)
		ceiling := "namespace budget exhausted: " + namespace
		if decision.Ceiling == "cluster" {
			ceiling = "cluster budget exhausted"
		}
		writeError(w, http.StatusTooManyRequests, errorBody{
			Type: errBudgetExhausted, Provider: providerName, Retryable: true,
			Message: fmt.Sprintf("%s on provider %s (%d%% used)",
				ceiling, providerName, decision.Percent)}, decision.RetryAfter)
		return false
	case decision.Action == kaalmv1alpha1.BudgetActionDegrade:
		s.Metrics.BudgetThreshold(providerName, namespace, kaalmv1alpha1.BudgetActionDegrade)
		if decision.DegradeTo != "" && decision.DegradeTo != *modelID {
			*modelID = decision.DegradeTo
		}
	case decision.Action == kaalmv1alpha1.BudgetActionWarn:
		s.Metrics.BudgetThreshold(providerName, namespace, kaalmv1alpha1.BudgetActionWarn)
		slog.Warn("budget threshold crossed", "namespace", namespace,
			"provider", providerName, "percent", decision.Percent)
	}
	if engaged {
		s.Metrics.BudgetBoundary(providerName, namespace, "engaged")
	}
	return true
}

// settleUsage folds token usage into spend, budget, and metrics. When the
// request holds a boundary admission slot (hard enforcement), the cost lands
// through its settle so the slot frees and the cost records in one atomic
// step; otherwise it lands through the plain ledger Add.
func (s *Server) settleUsage(provider *kaalmv1alpha1.ModelProvider, namespace, workload, modelID string, usage Usage, settle func(float64)) {
	cost := costOf(provider, modelID, usage)
	s.Spend.Record(namespace, provider.Name, modelID, usage)
	if settle != nil {
		settle(cost)
	} else {
		s.Budget.Add(provider, namespace, workload, cost)
	}
	s.Metrics.Tokens(provider.Name, modelID, namespace, usage)
	s.Metrics.Spend(provider.Name, namespace, cost)
	for tool, count := range usage.ServerTools {
		s.Metrics.ServerToolUse(provider.Name, namespace, tool, count)
	}
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
	namespace, workload string, provider *kaalmv1alpha1.ModelProvider, modelID string,
	settle func(float64),
) {
	copyDownstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	var usage Usage
	// Settle on EVERY exit, including the early return on a downstream
	// write error: near a hard ceiling, dropping accumulated usage would
	// under-settle exactly where undercounting voids the cap, and a held
	// admission slot must always free.
	defer func() {
		if !usage.isZero() {
			s.settleUsage(provider, namespace, workload, modelID, usage, settle)
		} else if settle != nil {
			settle(0)
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			adapter.accumulateStreamUsage(bytes.TrimSpace(data), &usage)
			bodyLog("llm stream", data)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		// The response headers and status are already flushed, so we cannot
		// signal the truncation downstream; log it and record whatever usage
		// was accumulated before the read failed.
		slog.Warn("SSE relay read error", "namespace", namespace,
			"provider", provider.Name, "model", modelID, "err", err)
	}
}
