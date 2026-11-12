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
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gcpauth"
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
// before the provider credential is injected. The list names every header a
// provider reads as a credential, so only the injected one reaches it.
// Without the explicit strip, a live audience-bound Kubernetes credential
// would be forwarded verbatim into third-party provider logs.
var authMaterialHeaders = []string{"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key"}

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
// credential under the forwarded-header contract, relay (JSON or SSE),
// and account for usage. Before forwarding it admits the call against the
// primary provider's budget and rate limits, then walks the fallback chain.
func (s *Server) handleLLMProxy(w http.ResponseWriter, r *http.Request) {
	c := callerFrom(r.Context())
	adapter, ok := adapterForPath(r.URL.Path)
	if _, isVertex := adapter.(vertexAdapter); isVertex && adapter.modelFromPath(r.URL.Path) == "" {
		// A Vertex request names its model only in the path; one whose path
		// yields none is never served on the body's model instead.
		ok = false
	}
	if !ok {
		// Constant, like the mux's catch-all: the answer never echoes the
		// caller's path (the Vertex prefixes route any path here).
		badRequest(w, "unrecognized path")
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

	if !json.Valid(body) {
		badRequest(w, "request body is not valid JSON")
		return
	}
	fields, err := scanRequestFields(body)
	if err != nil {
		var dup *duplicateFieldError
		if errors.As(err, &dup) {
			badRequest(w, dup.Error())
			return
		}
		if errors.Is(err, errStreamNotBoolean) {
			badRequest(w, err.Error())
			return
		}
		badRequest(w, "request body is not valid JSON")
		return
	}
	// The Vertex format carries the model in the URL; the others in the body.
	qualified := adapter.modelFromPath(r.URL.Path)
	if qualified == "" {
		qualified = fields.model(body)
	}
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
	// across live replicas. Under a token limit, the request's estimate (its
	// body size over four, plus its output cap or the model's
	// maxOutputTokens, or one replica share when neither exists) is reserved
	// on the bucket that admits it: the primary's limits and this model. The
	// reservation is settled when the call ends, even when a fallback serves
	// the request.
	ok, retryAfter, reservation := s.RateLimiter.Allow(provider, c.Namespace, modelID,
		requestTokenEstimate(provider, modelID, body))
	if !ok {
		s.Metrics.LLMRequest(providerName, modelID, c.Namespace, "rate_limited")
		spanError(ctx, errRateLimited)
		writeError(w, http.StatusTooManyRequests, errorBody{
			Type: errRateLimited, Provider: providerName, Retryable: true,
			Message: fmt.Sprintf("rate limit exceeded for namespace %s on model %s", c.Namespace, modelID)}, retryAfter)
		return
	}
	// The safety net for every early return (an exhausted walk, a caller
	// that left before a 2xx, a cut JSON relay's unwind): the first settle,
	// keep, or release wins, so this does nothing after a real one.
	defer reservation.release()

	// Strip the provider prefix so the upstream sees the raw model ID, write
	// the routing member names exactly, and set stream_options.include_usage
	// where the format needs it for stream usage. Every other byte goes out
	// as the agent sent it.
	outBody := rewriteRequestBody(body, fields, modelID, needsStreamOptions(adapter, fields))

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
	// A candidate that crosses formats gets the body
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
		body: outBody, inboundFormat: inboundFormat, modelFor: map[string]string{provider.Name: modelID},
	}
	res, ok := s.tryWithFallbacks(ctx, provider, st, func(ctx context.Context, cand *kaalmv1beta1.ModelProvider) forwardResult {
		fctx, endForward := s.Tracing.Start(ctx, "llm.forward", trace.SpanKindClient,
			attribute.String("kaalm.provider", cand.Name))
		body, inboundPath, pathAdapter, candAdapter, model := s.candidateRequest(st, cand, outBody, r.URL.Path, adapter, typeAdapter)
		fr := s.forwardOnce(fctx, r, cand, body, inboundPath, pathAdapter, candAdapter, model)
		fr.model, fr.format = model, formatForType(cand.Spec.Type)
		if st.crosses(cand) {
			mt, ok := llmtranslate.MaxTokens(st.translated[cand.Name])
			if !ok {
				mt = maxOutputTokensOf(cand, model)
			}
			fr.holdLimit = crossingHoldLimit(mt)
		}
		// A caller that left fails the attempt's read; that is not the
		// candidate's failure.
		callerLeft := ctx.Err() != nil
		switch {
		case callerLeft:
			endForward(errors.New(outcomeClientClosed))
		case fr.class != classNone:
			endForward(errors.New(failClassName(fr.class)))
			st.observed[fr.class] = true
		default:
			endForward(nil)
		}
		// Count every attempt on a non-primary candidate as a fallback,
		// whatever its outcome (a succeeding attempt is labeled "success").
		if cand.Name != provider.Name {
			reason := fallbackReasonSuccess
			switch {
			case callerLeft:
				reason = outcomeClientClosed
			case fr.class != classNone:
				reason = failClassName(fr.class)
			}
			s.Metrics.Fallback(provider.Name, cand.Name, reason)
		}
		return fr
	})
	if ctx.Err() != nil && !readsOnAfterCallerLeft(res) {
		s.settleCallerLeft(ctx, res, c.Namespace, workload, &modelID, &answered, reservation)
		return
	}
	if !ok {
		status, body, retryAfter := exhaustionError(st.observed, st.maxRetryAfter, st.budgetBlocked, providerName)
		s.Metrics.LLMRequest(providerName, modelID, c.Namespace, outcomeError)
		spanError(ctx, body.Type)
		writeError(w, status, body, retryAfter)
		return
	}
	s.writeWalkResult(ctx, w, res, adapter, inboundFormat, c.Namespace, workload, &modelID, &answered, reservation)
}

// requestTokenEstimate is what a request reserves on provider's token
// bucket: its body size over bytesPerToken as the input
// estimate, plus its output cap. The cap is the request's own, lowered to
// the model's maxOutputTokens when that is smaller (the provider generates
// no more than its ceiling); the model's maxOutputTokens when the request
// sets none; or outputCapUnknown when neither exists. A provider with no
// tokensPerMinute reserves nothing, and its body is not read for the cap.
func requestTokenEstimate(provider *kaalmv1beta1.ModelProvider, modelID string, body []byte) tokenEstimate {
	if provider.Spec.RateLimits.TokensPerMinute <= 0 {
		return tokenEstimate{}
	}
	outputCap, ok := requestOutputCap(body)
	if m := maxOutputTokensOf(provider, modelID); m > 0 && (!ok || outputCap > m) {
		outputCap, ok = m, true
	}
	if !ok {
		outputCap = outputCapUnknown
	}
	return tokenEstimate{Input: int64(len(body) / bytesPerToken), Output: outputCap}
}

// settleCallerLeft ends a request whose caller left before the gateway
// answered, and writes nothing. A 2xx that reaches it is a stream: it
// arrives unread and can no longer be read, since its read derives from the
// caller's context. The provider read the request, so the stream is charged
// the serving adapter's input estimate from the body it received (see
// settleRelay): it counts as usage missing, and its token reservation stays
// as the token charge. A non-streamed 2xx goes to its relay instead and is
// read to its end (see readsOnAfterCallerLeft). Any other attempt releases
// its token reservation and settles zero. A relay that had started settles
// what it read itself. The request counts as client_closed.
func (s *Server) settleCallerLeft(
	ctx context.Context, res forwardResult, namespace, workload string, modelID, answered *string,
	reservation *tokenReservation,
) {
	if res.resp != nil {
		defer func() { _ = res.resp.Body.Close() }()
	}
	if res.provider != "" {
		*answered = res.provider
	}
	if res.model != "" {
		*modelID = res.model
	}
	if res.resp != nil && res.resp.StatusCode >= 200 && res.resp.StatusCode <= 299 {
		var est streamEstimator
		if a, ok := adapterForProviderType(res.chosen.Spec.Type); ok {
			est = a.streamEstimator(res.request)
		}
		s.settleRelay(withEstimate(Usage{}, est), false, res.chosen, namespace, workload, *modelID,
			res.settle, reservation)
	} else {
		reservation.release()
		if res.settle != nil {
			res.settle(0)
		}
	}
	s.Metrics.LLMRequest(*answered, *modelID, namespace, outcomeClientClosed)
	spanError(ctx, outcomeClientClosed)
}

// writeWalkResult relays the winning attempt: a non-fallbackable failure
// verbatim (in the caller's envelope shape when the candidate crossed
// formats), a stream through the stream relay (translated event by event
// when it crossed), a JSON answer through the JSON relay, or, when it
// crossed, a JSON answer held whole and translated back. Usage is read by
// the serving candidate's adapter from the untranslated bytes as they pass.
// reservation is the request's token reservation: a relay settles it with
// the usage it read (at least the reservation when the relay did not finish
// cleanly), or keeps it when it read none, and a non-2xx relayed verbatim
// releases it. A 2xx is counted on kaalm_llm_requests_total when
// its relay ends, so the status says how it ended. A non-streamed 2xx comes
// here after the caller left too: the JSON relay and the hold read it to
// its end, settle its usage, and count client_closed.
func (s *Server) writeWalkResult(
	ctx context.Context, w http.ResponseWriter, res forwardResult, adapter providerAdapter,
	inboundFormat llmtranslate.Format, namespace, workload string, modelID, answered *string,
	reservation *tokenReservation,
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
		reservation.release()
		if res.settle != nil {
			res.settle(0)
		}
		s.Metrics.LLMRequest(res.provider, *modelID, namespace, outcomeError)
		// The llm.request server span stays unset, as tool.call does for a
		// relayed upstream 4xx: the gateway handled the request correctly
		// (the OpenTelemetry HTTP server-span convention), and the failed
		// candidate's llm.forward span carries the error.
		bodyLog("llm response", res.body)
		copyDownstreamHeaders(w.Header(), res.resp.Header)
		respBody := res.body
		if crossing {
			respBody = llmtranslate.Error(res.format, inboundFormat, res.body)
			w.Header().Del("Content-Length")
		} else if int64(len(res.body)) != res.resp.ContentLength {
			// The body was cut at errorBodyLimit, or its length was unknown.
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(res.resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	if isSSE(res.resp) {
		var translator llmtranslate.Stream
		if crossing {
			translator = llmtranslate.NewStream(res.format, inboundFormat, *modelID)
		}
		outcome := s.relayStream(ctx, w, res.resp, servingAdapter, res.request, translator, formatForType(adapter.formatName()),
			namespace, workload, res.chosen, *modelID, res.settle, reservation)
		s.Metrics.LLMRequest(res.provider, *modelID, namespace, outcome)
		return
	}
	if crossing {
		outcome := s.relayCrossing(ctx, w, res.resp, servingAdapter, res.format, inboundFormat, res.holdLimit,
			namespace, workload, res.chosen, *modelID, res.settle, reservation)
		s.Metrics.LLMRequest(res.provider, *modelID, namespace, outcome)
		return
	}
	outcome, cut := s.relayJSON(ctx, w, res.resp, servingAdapter, namespace, workload, res.chosen, *modelID,
		res.settle, reservation)
	s.Metrics.LLMRequest(res.provider, *modelID, namespace, outcome)
	if cut {
		// The status is sent and JSON has no in-band error event, so the
		// response ends without its final bytes (the chunk terminator, or
		// the rest of the Content-Length): the agent's client reports an
		// incomplete body instead of a clean end. net/http logs nothing for
		// this value, and the deferred Body.Close and settles run on the way
		// out.
		panic(http.ErrAbortHandler)
	}
}

// candidateRequest builds what one candidate is forwarded: the prepared
// body for a same-format candidate with the same model; the same bytes with
// only the model value replaced when the model differs (a same-type edge
// with a modelMap); and, for a crossing, the body the eligibility check
// translated, encoded, with stream_options set where the candidate's
// format needs it, on the candidate format's canonical path.
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
			if encoded, err := json.Marshal(translated); err == nil {
				if f, err := scanRequestFields(encoded); err == nil {
					body = rewriteRequestBody(encoded, f, model, needsStreamOptions(candAdapter, f))
					return body, canonicalPath(formatForType(cand.Spec.Type)), candAdapter, candAdapter, model
				}
			}
		}
	}
	if model != st.modelID && len(st.body) > 0 {
		if f, err := scanRequestFields(st.body); err == nil {
			// st.body already carries the primary's include_usage, so this
			// rewrite changes only the model.
			return rewriteRequestBody(st.body, f, model, false), inboundPath, adapter, candAdapter, model
		}
	}
	return outBody, inboundPath, adapter, candAdapter, model
}

// forwardOnce forwards the request to a single candidate provider under the
// forwarded-header contract and classifies the outcome for the fallback walk.
// The attempt runs under an attemptWatchdog: UpstreamTimeout bounds the wait
// for response headers, then each gap between body reads. The attempt ends
// when the caller leaves, except a non-streamed 2xx: once its headers
// arrive, it is read to its end (see relayJSON). A 2xx, JSON or SSE, is
// returned unread and hands the watchdog to the relay through the wrapped
// body, whose Close releases it. Any other status is read up to
// errorBodyLimit and closed, which releases the watchdog. A credential the store
// refuses is a connect-class failure, logged at most once a minute per provider.
// For google-vertex the credential is a service-account key, exchanged for
// an access token first (see vertexCredential), and a 401 forgets the token
// the attempt sent.
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
	if provider.Spec.Type == providerTypeVertex {
		token, fr, ok := s.vertexCredential(ctx, provider, credential)
		if !ok {
			return fr
		}
		credential = token
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

	// A 401 means Vertex refused this token, so the next request mints
	// instead of resending it for up to 55 minutes. A 403 is a valid token
	// that lacks permission, which a new token does not fix, so it stays.
	// Only the token this attempt sent is dropped, so a newer one stays.
	if provider.Spec.Type == providerTypeVertex && resp.StatusCode == http.StatusUnauthorized {
		s.vertexTokens.forget(provider.Name, credential)
	}
	fallback, class := isFallbackable(resp.StatusCode)
	fr := forwardResult{resp: resp, fallilable: fallback, class: class, provider: provider.Name, chosen: provider,
		request: outBody}
	if class == classNone {
		// A 2xx goes to the relay unread: the walk never abandons a 2xx
		// (tryWithFallbacks returns it), and the handler closes the body.
		// A non-streamed 2xx is read to its end whatever the caller does:
		// the provider billed the whole answer before it sent the headers,
		// and usage can be its last member. A stream stays tied to the
		// caller, whose leaving stops the provider generating.
		if !isSSE(resp) {
			dog.detach()
		}
		return fr
	}
	// Any other status is read up to errorBodyLimit and closed, so an
	// attempt the walk abandons holds no connection and no timer. Providers
	// do not bill errors, so the dropped rest is nothing paid for.
	body, err := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	_ = resp.Body.Close()
	if err != nil && ctx.Err() != nil {
		// The caller left mid-read. The answer goes to the handler as read
		// so far, the same as when the cancel ends the read cleanly.
		resp.Body = io.NopCloser(bytes.NewReader(body))
		fr.body = body
		return fr
	}
	if err != nil {
		class := classConnect
		if errors.Is(err, errUpstreamIdle) {
			class = classTimeout
		}
		return forwardResult{fallilable: true, class: class, err: err}
	}
	// Re-wrap the read body so downstream reads still work.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	fr.body = body
	return fr
}

// errorBodyLimit bounds how much of a non-2xx answer the gateway reads.
// Error bodies are short; the rest of a longer one is dropped.
const errorBodyLimit = 64 << 10

// The hold of a cross-format answer, which translation needs whole, is
// bounded by the output the request allows: crossingHoldFloor plus
// crossingHoldBytesPerToken for each max_tokens. A token renders to about 4
// bytes of text, and to at most 12 when every character is a \u-escaped
// surrogate pair, so 16 exceeds any honest rendering. The floor covers what
// max_tokens does not count: ids, tool-call structure, thinking signatures,
// and server tool results. Only a provider breaking its contract passes it.
const (
	crossingHoldFloor         = 1 << 20
	crossingHoldBytesPerToken = 16
)

// crossingHoldLimit is the most bytes the translation hold keeps for an
// answer whose request allows maxTokens output tokens.
func crossingHoldLimit(maxTokens int64) int64 {
	if maxTokens > (math.MaxInt64-crossingHoldFloor)/crossingHoldBytesPerToken {
		return math.MaxInt64 // a caller's max_tokens never overflows the bound
	}
	return crossingHoldFloor + max(maxTokens, 0)*crossingHoldBytesPerToken
}

// vertexCredential exchanges a google-vertex provider's service-account key
// for an access token, from the cache or a mint over the upstream client (so
// the upstream trust pool, proxy settings, and redirect refusal apply to the
// token endpoint too). On failure it returns the attempt's result: a key
// that does not parse is refused like a credential the store refuses; a
// token endpoint that refuses the key is a credential failure with no
// upstream response, like an upstream 401; anything else is a connect or
// timeout failure, logged at most once a minute per provider.
func (s *Server) vertexCredential(
	ctx context.Context, provider *kaalmv1beta1.ModelProvider, key string,
) (string, forwardResult, bool) {
	token, err := s.vertexTokens.token(ctx, s.upstream(), provider.Name, key, s.Config.UpstreamTimeout)
	if err == nil {
		return token, forwardResult{}, true
	}
	switch {
	case errors.Is(err, gcpauth.ErrInvalidKey):
		if ctx.Err() == nil && s.credentialLog.allow(provider.Name, credentialLogInterval) {
			slog.Warn("llm credential unavailable", "provider", provider.Name, "error", err)
		}
		return "", forwardResult{fallilable: true, class: classConnect, err: err}, false
	case errors.Is(err, gcpauth.ErrRejected):
		return "", forwardResult{fallilable: true, class: classUpstream, err: err, credentialRejected: true}, false
	}
	class := classConnect
	if errors.Is(err, context.DeadlineExceeded) {
		class = classTimeout
	}
	// A done context means the caller left; the error is not the provider's.
	if ctx.Err() == nil && s.credentialLog.allow(provider.Name+"/token", credentialLogInterval) {
		slog.Warn("llm access token unavailable", "provider", provider.Name, "error", err)
	}
	return "", forwardResult{fallilable: true, class: class, err: err}, false
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
// ledger Add. reservation (nil for none) is settled with the input plus
// output tokens on the token bucket that admitted the request. When part of
// usage is the adapter's estimate, that part's cost also lands on the
// estimated-spend counter and is returned as estimatedUSD.
func (s *Server) settleUsage(
	provider *kaalmv1beta1.ModelProvider, namespace, workload, modelID string, usage Usage,
	settle func(float64), reservation *tokenReservation,
) (estimatedUSD float64) {
	reservation.settle(usage.InputTokens + usage.OutputTokens)
	cost := costOf(provider, modelID, usage)
	s.Spend.Record(namespace, provider.Name, modelID, usage)
	if settle != nil {
		settle(cost)
	} else {
		s.Budget.Add(provider, namespace, workload, cost)
	}
	s.Metrics.Tokens(provider.Name, modelID, namespace, usage)
	s.Metrics.Spend(provider.Name, namespace, cost)
	if usage.estimated() {
		estimatedUSD = costOf(provider, modelID, usage.estimatedPart())
		s.Metrics.SpendEstimated(provider.Name, namespace, estimatedUSD)
	}
	for tool, count := range usage.ServerTools {
		s.Metrics.ServerToolUse(provider.Name, namespace, tool, count)
	}
	return estimatedUSD
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

// readsOnAfterCallerLeft reports whether the walk's result is a non-streamed
// 2xx, which its relay reads to its end for its usage after the caller
// leaves (see forwardOnce).
func readsOnAfterCallerLeft(res forwardResult) bool {
	return res.resp != nil && res.resp.StatusCode >= 200 && res.resp.StatusCode <= 299 && !isSSE(res.resp)
}

// relayStream forwards SSE events to the caller as each one completes, and
// events that arrive together leave in one write (see eventFlusher). It folds
// usage out of the events the adapter recognizes, and feeds the same events
// to the adapter's stream estimator, built from request, the body the
// provider received. Spend is recorded, and the token reservation settled,
// after the stream ends (see settleRelay). A stream counts as finished once
// the adapter reports its final event read, even when the caller leaves or
// the read fails after it: its usage is then complete. A stream that ends
// without complete usage settles the parts the provider reported plus the
// adapter's estimate for the rest, marked estimated and reported by
// usageMissing; a stream with nothing reported or estimated settles at zero
// spend and keeps its token reservation. When the upstream read fails
// after the status is sent (the idle bound passed, or the connection broke),
// the relay ends the stream with one error event in the caller's format, so the
// agent can tell a truncated stream from a complete one. It returns the outcome
// for kaalm_llm_requests_total and marks the llm.request span (ctx) the same
// way: ok, error for a stream the provider broke or let go idle, or
// client_closed for a caller that left, which gets no event. ctx derives from
// the caller's request context, and the upstream read from ctx, so ctx.Err() is
// set only when the caller left.
func (s *Server) relayStream(
	ctx context.Context, w http.ResponseWriter, resp *http.Response, adapter providerAdapter, request []byte,
	translator llmtranslate.Stream, callerFormat llmtranslate.Format, namespace, workload string,
	provider *kaalmv1beta1.ModelProvider, modelID string, settle func(float64), reservation *tokenReservation,
) (outcome string) {
	copyDownstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	var usage Usage
	est := adapter.streamEstimator(request)
	finalRead := false
	// Settle on EVERY exit, including the early return on a downstream
	// write error: near a hard ceiling, dropping accumulated usage would
	// under-settle exactly where undercounting voids the cap, and a held
	// admission slot must always free.
	defer func() {
		s.settleRelay(withEstimate(usage, est), outcome == outcomeOK || finalRead, provider, namespace, workload,
			modelID, settle, reservation)
	}()

	ef := &eventFlusher{r: resp.Body, f: flusher}
	scanner := bufio.NewScanner(ef)
	buf := getScanBuf()
	defer putScanBuf(buf)
	scanner.Buffer((*buf)[:0], 1024*1024)
	callerLeft := func() string {
		spanError(ctx, outcomeClientClosed)
		return outcomeClientClosed
	}
	writeLine := func(line []byte) bool {
		if _, err := w.Write(append(line, '\n')); err != nil {
			return false
		}
		ef.wrote(line)
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			// Usage and the estimate come from the upstream's own events,
			// before translation.
			data = bytes.TrimSpace(data)
			if adapter.accumulateStreamUsage(data, &usage) {
				finalRead = true
			}
			if est != nil {
				est.event(data)
			}
			bodyLog("llm stream", data)
		}
		if translator == nil {
			if !writeLine(line) {
				return callerLeft()
			}
			continue
		}
		for _, out := range translator.Feed(line) {
			if !writeLine(out) {
				return callerLeft()
			}
		}
	}
	// The upstream read derives from the caller's context: a caller that
	// left cancels it. The read then ends in the context error or, as the
	// transport can report the cancel, a clean io.EOF; either way the stream
	// did not finish and no one is there to read an event.
	if ctx.Err() != nil {
		return callerLeft()
	}
	if err := scanner.Err(); err != nil {
		// The status is already sent, so the truncation is signaled in the
		// body. No fallback after the first byte, and no translator Finish:
		// its closing events would make the stream look complete.
		slog.Warn("SSE relay read error", "namespace", namespace,
			"provider", provider.Name, "model", modelID, "err", err)
		spanError(ctx, streamErrorType(err))
		for _, out := range streamErrorEvent(callerFormat, provider.Name, err) {
			if !writeLine(out) {
				// The provider failure came first.
				break
			}
		}
		ef.end()
		return outcomeError
	}
	if translator != nil {
		for _, out := range translator.Finish() {
			if !writeLine(out) {
				return callerLeft()
			}
		}
	}
	ef.end()
	return outcomeOK
}

// settleRelay settles a relay when it ends, on every exit: the usage it
// read, or, when it read none, zero spend reported by usageMissing, with the
// token reservation kept as the call's charge. A stream's usage can hold the
// adapter's estimate for the parts the provider did not report; it settles
// like reported usage, and usageMissing reports it with the estimated
// dollars. finished reports that the
// relay finished cleanly: its outcome is ok, or, for a stream, the
// provider's final event was read, whatever happened after it. A relay that
// did not (outcome client_closed or error: the caller left, the provider
// broke the answer or let it go idle, or a crossing answer read in full
// went over the hold limit or could not be translated) may have sent output
// its usage read does not count, so its token reservation settles at the
// larger of that usage and what it took; spend still settles the usage
// read. A held admission slot always frees. Every 2xx settles here.
func (s *Server) settleRelay(
	usage Usage, finished bool, provider *kaalmv1beta1.ModelProvider, namespace, workload, modelID string,
	settle func(float64), reservation *tokenReservation,
) {
	if !usage.isZero() {
		if !finished {
			// Settled first, so the settle in settleUsage does nothing.
			reservation.settleAtLeast(usage.InputTokens + usage.OutputTokens)
		}
		estimatedUSD := s.settleUsage(provider, namespace, workload, modelID, usage, settle, reservation)
		if usage.estimated() {
			s.usageMissing(namespace, provider.Name, modelID, usage, estimatedUSD)
		}
		return
	}
	reservation.keep()
	s.usageMissing(namespace, provider.Name, modelID, Usage{}, 0)
	if settle != nil {
		settle(0)
	}
}

// relayBody copies body to sink, feeding every byte to reader first. It
// stops at the body's end (readErr nil), a read error, or a write error. It
// reads with a small buffer until a read fills it, then with a scan buffer:
// each read leaves as one write and flush, so a large answer needs the larger
// reads, and a small one should not pay for them.
func relayBody(body io.Reader, sink io.Writer, reader *usageReader) (readErr, writeErr error) {
	small := getRelayBuf()
	defer putRelayBuf(small)
	var large *[]byte
	defer func() {
		if large != nil {
			putScanBuf(large)
		}
	}()
	buf := *small
	for {
		n, err := body.Read(buf)
		if n > 0 {
			_, _ = reader.Write(buf[:n])
			if _, werr := sink.Write(buf[:n]); werr != nil {
				return nil, werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return err, nil
		}
		if large == nil && n == len(buf) {
			large = getScanBuf()
			buf = *large
		}
	}
}

// bodyLogPrefixBytes is how much of a relayed answer the debug build logs:
// the gateway does not hold the answer, so it logs only its start.
const bodyLogPrefixBytes = 64 << 10

// logPrefix keeps the first bodyLogPrefixBytes written through it for one
// debug log line. It keeps nothing in the default build.
type logPrefix []byte

func (l *logPrefix) add(p []byte) {
	if DebugBodyLogging && len(*l) < bodyLogPrefixBytes {
		*l = append(*l, p[:min(len(p), bodyLogPrefixBytes-len(*l))]...)
	}
}

// flushWriter writes a relayed answer to the agent, flushing each write so
// the bytes leave as they arrive.
type flushWriter struct {
	w      http.ResponseWriter
	f      http.Flusher // nil when the writer cannot flush
	logged logPrefix
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	fw.logged.add(p)
	n, err := fw.w.Write(p)
	if err == nil && fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// relayJSON relays a non-streamed 2xx answer to the agent as its bytes
// arrive, so the gateway's memory per request does not grow with the
// answer's size. Usage is read from the same bytes by the adapter's usage
// reader, and spend settles when the relay ends, on every exit, as in
// relayStream. A caller that leaves does not end the upstream read: the
// relay stops writing and reads to the end, so spend settles the usage
// read, including usage that arrives after the caller left. A provider that
// stalls ends the read at the idle bound, and usage not read by then is
// missing. The upstream Content-Length passes through. It returns the
// outcome for kaalm_llm_requests_total and marks the llm.request span (ctx)
// the same way: ok, client_closed for a caller that left, or error for an
// answer the provider broke or let go idle after the status was sent. cut
// reports that last case: the caller must end the response without its
// final bytes, since JSON has no in-band error event.
func (s *Server) relayJSON(
	ctx context.Context, w http.ResponseWriter, resp *http.Response, adapter providerAdapter,
	namespace, workload string, provider *kaalmv1beta1.ModelProvider, modelID string,
	settle func(float64), reservation *tokenReservation,
) (outcome string, cut bool) {
	copyDownstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	reader := adapter.usageReader()
	defer func() {
		s.settleRelay(reader.usage, outcome == outcomeOK, provider, namespace, workload, modelID, settle, reservation)
	}()

	sink := &flushWriter{w: w}
	sink.f, _ = w.(http.Flusher)
	readErr, writeErr := relayBody(resp.Body, sink, reader)
	if writeErr != nil {
		// The agent is gone, or (ErrContentLength) must be cut off. The
		// provider billed the whole answer when it sent the headers, so the
		// rest is read for its usage and discarded. The attempt no longer
		// follows the caller; the idle bound still ends a stalled provider.
		readErr, _ = relayBody(resp.Body, io.Discard, reader)
	}
	bodyLog("llm response", sink.logged)
	switch {
	// ctx.Err() is set only when the caller left. A failed write to the
	// agent is the caller leaving too, except ErrContentLength: the
	// provider sent more than the length it declared.
	case ctx.Err() != nil, writeErr != nil && !errors.Is(writeErr, http.ErrContentLength):
		spanError(ctx, outcomeClientClosed)
		return outcomeClientClosed, false
	case readErr != nil, writeErr != nil:
		err := readErr
		if err == nil {
			err = writeErr
		}
		slog.Warn("JSON relay read error", "namespace", namespace,
			"provider", provider.Name, "model", modelID, "err", err)
		spanError(ctx, streamErrorType(err))
		// Send the status if no body byte did: the caller's abort would
		// otherwise close the connection with the 2xx still buffered, and
		// the agent would see no response at all.
		if sink.f != nil {
			sink.f.Flush()
		}
		return outcomeError, true
	}
	return outcomeOK, false
}

// holdBuffer holds a crossing answer for translation, up to limit bytes.
// Past the limit it drops what it holds and keeps accepting bytes, so the
// relay drains the answer through the usage reader to its end.
type holdBuffer struct {
	limit  int64
	buf    []byte
	over   bool
	logged logPrefix
}

func (h *holdBuffer) Write(p []byte) (int, error) {
	h.logged.add(p)
	if h.over {
		return len(p), nil
	}
	if int64(len(h.buf))+int64(len(p)) > h.limit {
		h.over, h.buf = true, nil
		return len(p), nil
	}
	h.buf = append(h.buf, p...)
	return len(p), nil
}

// relayCrossing answers with a non-streamed 2xx from a candidate of
// another format. Translation needs the whole answer, so this is the one
// relay that holds it, bounded by limit (see crossingHoldLimit). Usage is
// read from the untranslated bytes as they arrive and settles on every
// exit, as in relayJSON. Nothing is sent before the hold ends, so a failure
// gets a whole error response: 502 provider_error for an answer over the
// limit, cut off, or untranslatable, 504 provider_timeout for one that went
// idle. None of them continues the walk: the provider's 2xx had started.
// A caller that leaves does not end the hold: the answer is read to its
// end for its usage, nothing is translated or written, and the outcome is
// client_closed. It returns the outcome for kaalm_llm_requests_total.
func (s *Server) relayCrossing(
	ctx context.Context, w http.ResponseWriter, resp *http.Response, servingAdapter providerAdapter,
	from, to llmtranslate.Format, limit int64, namespace, workload string,
	provider *kaalmv1beta1.ModelProvider, modelID string, settle func(float64), reservation *tokenReservation,
) (outcome string) {
	reader := servingAdapter.usageReader()
	defer func() {
		s.settleRelay(reader.usage, outcome == outcomeOK, provider, namespace, workload, modelID, settle, reservation)
	}()

	hold := &holdBuffer{limit: limit}
	readErr, _ := relayBody(resp.Body, hold, reader)
	bodyLog("llm response", hold.logged)
	fail := func(status int, errType, message string) string {
		spanError(ctx, errType)
		writeError(w, status, errorBody{Type: errType, Provider: provider.Name, Message: message}, 0)
		return outcomeError
	}
	switch {
	case ctx.Err() != nil:
		spanError(ctx, outcomeClientClosed)
		return outcomeClientClosed
	case readErr != nil:
		slog.Warn("JSON relay read error", "namespace", namespace,
			"provider", provider.Name, "model", modelID, "err", readErr)
		status := http.StatusBadGateway
		if streamErrorType(readErr) == errProviderTimeout {
			status = http.StatusGatewayTimeout
		}
		return fail(status, streamErrorType(readErr), "the fallback provider's response ended before it completed")
	case hold.over:
		return fail(http.StatusBadGateway, errProviderError,
			"the fallback provider's response exceeded the size its max_tokens allows")
	}
	translated, err := llmtranslate.Response(from, to, hold.buf)
	if err != nil {
		return fail(http.StatusBadGateway, errProviderError,
			"the fallback provider's response could not be translated: "+err.Error())
	}
	copyDownstreamHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(translated)
	return outcomeOK
}

// anthropicErrorEvent is the Anthropic SSE event name, and data type, of a
// stream error.
const anthropicErrorEvent = "error"

// streamErrorEvent renders the event that ends a truncated stream, in the
// caller's format: an Anthropic "error" event, or an OpenAI-style data line
// carrying the gateway error envelope. Its type is streamErrorType's.
func streamErrorEvent(callerFormat llmtranslate.Format, provider string, cause error) [][]byte {
	errType := streamErrorType(cause)
	message := "the provider stream failed before it completed; the response is truncated"
	if errType == errProviderTimeout {
		message = "the provider sent no data within the upstream timeout; the response is truncated"
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

// streamErrorType is the error type of a stream the provider did not finish:
// provider_timeout when the idle bound ended it, otherwise provider_error.
// The error event and the llm.request span both carry it.
func streamErrorType(cause error) string {
	if errors.Is(cause, errUpstreamIdle) {
		return errProviderTimeout
	}
	return errProviderError
}

// usageMissing reports a 2xx response whose usage the provider did not
// report in full: one that settles at zero spend because it carried no
// usage, which under hard enforcement is invisible to the budget, or a
// stream that settles partly or wholly at the adapter's estimate (usage,
// whose estimated parts cost estimatedUSD). Each is counted, and logged at
// most once a minute per provider: the line names the first response of
// each window.
func (s *Server) usageMissing(namespace, provider, modelID string, usage Usage, estimatedUSD float64) {
	if s.usageMissingLog.allow(provider, usageMissingLogInterval) {
		if usage.estimated() {
			slog.Warn("LLM stream carried incomplete usage; settled at estimated spend",
				"namespace", namespace, "provider", provider, "model", modelID,
				"estimated", estimatedParts(usage), "usd", estimatedUSD)
		} else {
			slog.Warn("LLM response carried no usage; settled at zero spend",
				"namespace", namespace, "provider", provider, "model", modelID)
		}
	}
	s.Metrics.UsageMissing(provider, modelID)
}

// estimatedParts names the estimated parts of usage for the usageMissing
// log line: input, output, or input,output.
func estimatedParts(u Usage) string {
	switch {
	case u.InputEstimated && u.OutputEstimated:
		return "input,output"
	case u.InputEstimated:
		return "input"
	}
	return "output"
}
