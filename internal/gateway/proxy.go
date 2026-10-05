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
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/llmtranslate"
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
// and account for usage. Before forwarding it admits the call against the
// primary provider's budget and rate limits, then walks the fallback chain.
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

	// llm.request parents onto whatever context the agent propagated from
	// its delivery; the llm.forward children below cover each provider
	// attempt. Denials past this point (budget, rate limit) close the span
	// with an error status, so a blocked request is visible in its trace.
	ctx, endSpan := s.Tracing.Start(s.Tracing.Extract(r.Context(), r.Header), "llm.request",
		trace.SpanKindServer,
		attribute.String("kaalm.provider", providerName),
		attribute.String("kaalm.model", modelID),
		attribute.String("kaalm.namespace", c.Namespace),
		attribute.String("kaalm.workload", workloadKey(c)))
	defer endSpan(nil)
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
		spanError(ctx, "budget_denied")
		return
	}

	// Rate limit per (namespace, model), sharing the configured ceilings
	// across live replicas. The settled tokens are debited after the call
	// from the bucket that admitted it: the primary's limits and this
	// model, even when a fallback serves the request.
	if ok, retryAfter := s.RateLimiter.Allow(provider, c.Namespace, modelID); !ok {
		s.Metrics.LLMRequest(providerName, modelID, c.Namespace, "rate_limited")
		spanError(ctx, errRateLimited)
		writeError(w, http.StatusTooManyRequests, errorBody{
			Type: errRateLimited, Provider: providerName, Retryable: true,
			Message: fmt.Sprintf("rate limit exceeded for namespace %s on model %s", c.Namespace, modelID)}, retryAfter)
		return
	}
	admittedModel := modelID
	debitTokens := func(u Usage) {
		s.RateLimiter.DebitTokens(provider, c.Namespace, admittedModel, u.InputTokens+u.OutputTokens)
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

	// The latency histogram covers forwarded requests only (local denials
	// above complete in microseconds and would drag the percentiles toward
	// zero, the same reasoning as the tool broker's), from here to the end
	// of the response relay, labeled with the provider that answered.
	start := time.Now()
	answered := providerName
	defer func() {
		s.Metrics.Duration(answered, modelID, time.Since(start).Seconds())
		trace.SpanFromContext(ctx).SetAttributes(
			attribute.String("kaalm.provider", answered),
			attribute.String("kaalm.model", modelID))
	}()

	// Walk the fallback tree. Each attempt forwards to one candidate with its
	// own credential and endpoint; the first 2xx (or a non-fallbackable 4xx)
	// wins. observed collects the failure classes for the exhaustion mapping.
	// A candidate that crosses formats (since v0.7.0) gets the body
	// translated by the eligibility check, its own adapter, and its format's
	// canonical path; the response is translated back below.
	inboundFormat := formatForType(adapter.formatName())
	if r.URL.Path == "/v1/completions" {
		inboundFormat = "" // the legacy completions shape never crosses
	}
	st := &walkState{
		primary: provider, namespace: c.Namespace, workload: workload, modelID: modelID,
		maxDepth: s.Config.MaxFallbackDepth, visited: map[string]bool{},
		observed: map[failClass]bool{}, primarySettle: primarySettle,
		parsed: parsed, inboundFormat: inboundFormat, modelFor: map[string]string{provider.Name: modelID},
	}
	res, ok := s.tryWithFallbacks(ctx, provider, st, func(ctx context.Context, cand *kaalmv1beta1.ModelProvider) forwardResult {
		fctx, endForward := s.Tracing.Start(ctx, "llm.forward", trace.SpanKindClient,
			attribute.String("kaalm.provider", cand.Name))
		body, inboundPath, pathAdapter, candAdapter, model := s.candidateRequest(st, cand, outBody, r.URL.Path, adapter, typeAdapter)
		fr := s.forwardOnce(fctx, r, cand, body, inboundPath, pathAdapter, candAdapter, model)
		fr.model, fr.format = model, formatForType(cand.Spec.Type)
		if fr.class != classNone {
			endForward(errors.New(failClassName(fr.class)))
		} else {
			endForward(nil)
		}
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
		status, body, retryAfter := exhaustionError(st.observed, st.maxRetryAfter, st.budgetBlocked, providerName)
		s.Metrics.LLMRequest(providerName, modelID, c.Namespace, "error")
		spanError(ctx, body.Type)
		writeError(w, status, body, retryAfter)
		return
	}
	s.writeWalkResult(ctx, w, res, adapter, inboundFormat, c.Namespace, workload, &modelID, &answered, debitTokens)
}

// writeWalkResult relays the winning attempt: a non-fallbackable failure
// verbatim (in the caller's envelope shape when the candidate crossed
// formats), a stream through the relay (translated event by event when it
// crossed), or a buffered body with usage read by the serving candidate's
// adapter and, when it crossed, the body translated back. debitTokens takes
// the settled usage off the token rate limit that admitted the request.
func (s *Server) writeWalkResult(
	ctx context.Context, w http.ResponseWriter, res forwardResult, adapter providerAdapter,
	inboundFormat llmtranslate.Format, namespace, workload string, modelID, answered *string,
	debitTokens func(Usage),
) {
	defer func() { _ = res.resp.Body.Close() }()
	*answered = res.provider
	if res.model != "" {
		*modelID = res.model
	}
	// Usage is read with the SERVING candidate's adapter from the
	// untranslated upstream response; spend lands on that provider.
	servingAdapter := adapter
	if res.chosen != nil {
		if a, ok := adapterForProviderType(res.chosen.Spec.Type); ok {
			servingAdapter = a
		}
	}
	crossing := res.format != "" && inboundFormat != "" && res.format != inboundFormat

	// A non-fallbackable failure (400/422/other 4xx) is relayed verbatim, in
	// the caller's envelope shape when the candidate crossed formats.
	if res.resp.StatusCode < 200 || res.resp.StatusCode > 299 {
		if res.settle != nil {
			res.settle(0)
		}
		s.Metrics.LLMRequest(res.provider, *modelID, namespace, "error")
		spanError(ctx, "upstream_error")
		bodyLog("llm response", res.body)
		copyDownstreamHeaders(w.Header(), res.resp.Header)
		respBody := res.body
		if crossing {
			respBody = llmtranslate.Error(res.format, inboundFormat, res.body)
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(res.resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	s.Metrics.LLMRequest(res.provider, *modelID, namespace, "ok")
	if isSSE(res.resp) {
		var translator llmtranslate.Stream
		if crossing {
			translator = llmtranslate.NewStream(res.format, inboundFormat, *modelID)
		}
		s.relayStream(w, res.resp, servingAdapter, translator, formatForType(adapter.formatName()),
			namespace, workload, res.chosen, *modelID, res.settle, debitTokens)
		return
	}
	if usage, ok := servingAdapter.extractUsage(res.body); ok {
		s.settleUsage(res.chosen, namespace, workload, *modelID, usage, res.settle, debitTokens)
	} else {
		s.usageMissing(namespace, res.provider, *modelID)
		if res.settle != nil {
			res.settle(0)
		}
	}
	bodyLog("llm response", res.body)
	copyDownstreamHeaders(w.Header(), res.resp.Header)
	respBody := res.body
	if crossing {
		translated, err := llmtranslate.Response(res.format, inboundFormat, res.body)
		if err != nil {
			spanError(ctx, errProviderError)
			writeError(w, http.StatusBadGateway, errorBody{
				Type: errProviderError, Provider: res.provider,
				Message: "the fallback provider's response could not be translated: " + err.Error()}, 0)
			return
		}
		respBody = translated
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(res.resp.StatusCode)
	_, _ = w.Write(respBody)
}

// candidateRequest builds what one candidate is forwarded: the caller's
// bytes for a same-format candidate with the same model; a re-encoded body
// when only the model differs (a same-type edge with a modelMap); and, for a
// crossing, the body the eligibility check translated, with the candidate's
// adapter fixups, on the candidate format's canonical path.
func (s *Server) candidateRequest(
	st *walkState, cand *kaalmv1beta1.ModelProvider, outBody []byte, inboundPath string,
	adapter, typeAdapter providerAdapter,
) (body []byte, path string, pathAdapter, candAdapter providerAdapter, model string) {
	model = st.candidateModel(cand)
	candAdapter = typeAdapter
	if a, ok := adapterForProviderType(cand.Spec.Type); ok {
		candAdapter = a
	}
	if st.crosses(cand) {
		if translated, ok := st.translated[cand.Name]; ok {
			clone := make(map[string]any, len(translated))
			for k, v := range translated {
				clone[k] = v
			}
			candAdapter.fixupRequestBody(clone)
			if encoded, err := json.Marshal(clone); err == nil {
				return encoded, canonicalPath(formatForType(cand.Spec.Type)), candAdapter, candAdapter, model
			}
		}
	}
	if model != st.modelID && st.parsed != nil {
		clone := make(map[string]any, len(st.parsed))
		for k, v := range st.parsed {
			clone[k] = v
		}
		clone["model"] = model
		if encoded, err := json.Marshal(clone); err == nil {
			return encoded, inboundPath, adapter, candAdapter, model
		}
	}
	return outBody, inboundPath, adapter, candAdapter, model
}

// forwardOnce forwards the request to a single candidate provider under the
// forwarded-header contract and classifies the outcome for the fallback walk.
// The attempt runs under an attemptWatchdog: UpstreamTimeout bounds the wait
// for response headers, then each gap between body reads. A buffered body
// releases the watchdog once read; a streaming 2xx hands it to the relay
// through the wrapped body, whose Close releases it. A credential the store
// refuses is a connect-class failure, logged at most once a minute per provider.
func (s *Server) forwardOnce(
	ctx context.Context, r *http.Request, provider *kaalmv1beta1.ModelProvider,
	outBody []byte, inboundPath string, adapter, typeAdapter providerAdapter, modelID string,
) forwardResult {
	credential, err := s.Store.Credential(ctx, provider)
	if err != nil {
		// A done context means the caller left; the error is not a credential problem.
		if ctx.Err() == nil && s.credentialLog.allow(provider.Name, credentialLogInterval) {
			slog.Warn("llm credential unavailable", "provider", provider.Name, "error", err)
		}
		return forwardResult{fallilable: true, class: classConnect, err: err}
	}
	upstreamURL := strings.TrimSuffix(provider.Spec.Endpoint, "/") + adapter.upstreamPath(inboundPath, modelID)
	actx, dog := newAttemptWatchdog(ctx, s.Config.UpstreamTimeout)
	upReq, err := http.NewRequestWithContext(actx, r.Method, upstreamURL, bytes.NewReader(outBody))
	if err != nil {
		dog.release()
		return forwardResult{fallilable: true, class: classConnect, err: err}
	}
	copyForwardedHeaders(upReq.Header, r.Header)
	typeAdapter.injectCredential(upReq.Header, credential)
	upReq.Header.Set("Content-Type", "application/json")

	resp, err := s.upstream().Do(upReq)
	if err != nil {
		class := classConnect
		if dog.idle() || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
			class = classTimeout
		}
		dog.release()
		return forwardResult{fallilable: true, class: class, err: err}
	}
	dog.kick()
	resp.Body = dog.watch(resp.Body)

	fallback, class := isFallbackable(resp.StatusCode)
	fr := forwardResult{resp: resp, fallilable: fallback, class: class, provider: provider.Name, chosen: provider}
	if isSSE(resp) && class == classNone {
		// A streaming 2xx: relay begins after the walk; no body buffering.
		return fr
	}
	// Every other response is buffered, so an attempt the walk abandons
	// holds no connection and no timer.
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		class := classConnect
		if errors.Is(err, errUpstreamIdle) {
			class = classTimeout
		}
		return forwardResult{fallilable: true, class: class, err: err}
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
	case decision.Action == kaalmv1beta1.BudgetActionBlock:
		s.Metrics.BudgetThreshold(providerName, namespace, kaalmv1beta1.BudgetActionBlock)
		ceiling := "namespace budget exhausted: " + namespace
		if decision.Ceiling == "cluster" {
			ceiling = "cluster budget exhausted"
		}
		writeError(w, http.StatusTooManyRequests, errorBody{
			Type: errBudgetExhausted, Provider: providerName,
			Message: fmt.Sprintf("%s on provider %s (%d%% used)",
				ceiling, providerName, decision.Percent)}, decision.RetryAfter)
		return false
	case decision.Action == kaalmv1beta1.BudgetActionDegrade:
		s.Metrics.BudgetThreshold(providerName, namespace, kaalmv1beta1.BudgetActionDegrade)
		if decision.DegradeTo != "" && decision.DegradeTo != *modelID {
			*modelID = decision.DegradeTo
		}
	case decision.Action == kaalmv1beta1.BudgetActionWarn:
		s.Metrics.BudgetThreshold(providerName, namespace, kaalmv1beta1.BudgetActionWarn)
		slog.Warn("budget threshold crossed", "namespace", namespace,
			"provider", providerName, "percent", decision.Percent)
	}
	if engaged {
		s.Metrics.BudgetBoundary(providerName, namespace, "engaged")
	}
	return true
}

// settleUsage folds token usage into the token rate limit, spend, budget, and
// metrics. When the request holds a boundary admission slot (hard
// enforcement), the cost lands through its settle so the slot frees and the
// cost records in one atomic step; otherwise it lands through the plain
// ledger Add. debitTokens (nil for none) charges the input plus output
// tokens to the token bucket that admitted the request.
func (s *Server) settleUsage(
	provider *kaalmv1beta1.ModelProvider, namespace, workload, modelID string, usage Usage,
	settle func(float64), debitTokens func(Usage),
) {
	if debitTokens != nil {
		debitTokens(usage)
	}
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
// usage out of the events the adapter recognizes. Spend is recorded, and the
// tokens debited from the token rate limit, after the stream ends. A stream that ends without usage settles at zero spend and is
// reported by usageMissing. When the upstream read fails after the status is
// sent (the idle bound passed, or the connection broke), the relay ends the
// stream with one error event in the caller's format, so the agent can tell a
// truncated stream from a complete one.
func (s *Server) relayStream(
	w http.ResponseWriter, resp *http.Response, adapter providerAdapter, translator llmtranslate.Stream,
	callerFormat llmtranslate.Format, namespace, workload string, provider *kaalmv1beta1.ModelProvider,
	modelID string, settle func(float64), debitTokens func(Usage),
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
			s.settleUsage(provider, namespace, workload, modelID, usage, settle, debitTokens)
			return
		}
		s.usageMissing(namespace, provider.Name, modelID)
		if settle != nil {
			settle(0)
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	writeLine := func(line []byte) bool {
		if _, err := w.Write(append(line, '\n')); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			// Usage comes from the upstream's own events, before translation.
			adapter.accumulateStreamUsage(bytes.TrimSpace(data), &usage)
			bodyLog("llm stream", data)
		}
		if translator == nil {
			if !writeLine(line) {
				return
			}
			continue
		}
		for _, out := range translator.Feed(line) {
			if !writeLine(out) {
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// The status is already sent, so the truncation is signaled in the
		// body. No fallback after the first byte, and no translator Finish:
		// its closing events would make the stream look complete.
		slog.Warn("SSE relay read error", "namespace", namespace,
			"provider", provider.Name, "model", modelID, "err", err)
		for _, out := range streamErrorEvent(callerFormat, provider.Name, err) {
			if !writeLine(out) {
				return
			}
		}
		return
	}
	if translator != nil {
		for _, out := range translator.Finish() {
			if !writeLine(out) {
				return
			}
		}
	}
}

// anthropicErrorEvent is the Anthropic SSE event name, and data type, of a
// stream error.
const anthropicErrorEvent = "error"

// streamErrorEvent renders the event that ends a truncated stream, in the
// caller's format: an Anthropic "error" event, or an OpenAI-style data line
// carrying the gateway error envelope. The type is provider_timeout when the
// idle bound ended the stream, otherwise provider_error.
func streamErrorEvent(callerFormat llmtranslate.Format, provider string, cause error) [][]byte {
	errType, message := errProviderError, "the provider stream failed before it completed; the response is truncated"
	if errors.Is(cause, errUpstreamIdle) {
		errType, message = errProviderTimeout, "the provider sent no data within the upstream timeout; the response is truncated"
	}
	type eventError struct {
		Type     string `json:"type"`
		Message  string `json:"message"`
		Provider string `json:"provider,omitempty"`
	}
	if callerFormat == llmtranslate.FormatAnthropic {
		data, _ := json.Marshal(struct {
			Type  string     `json:"type"`
			Error eventError `json:"error"`
		}{Type: anthropicErrorEvent, Error: eventError{Type: errType, Message: message}})
		return [][]byte{[]byte("event: " + anthropicErrorEvent), append([]byte("data: "), data...), {}}
	}
	data, _ := json.Marshal(struct {
		Error eventError `json:"error"`
	}{Error: eventError{Type: errType, Message: message, Provider: provider}})
	return [][]byte{append([]byte("data: "), data...), {}}
}

// usageMissing reports a 2xx response that settles at zero spend because it
// carried no usage. Under hard enforcement such a response is invisible to
// the budget, so it is logged and counted.
func (s *Server) usageMissing(namespace, provider, modelID string) {
	slog.Warn("LLM response carried no usage; settled at zero spend",
		"namespace", namespace, "provider", provider, "model", modelID)
	s.Metrics.UsageMissing(provider, modelID)
}
