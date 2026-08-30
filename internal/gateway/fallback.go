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
	"context"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/llmtranslate"
)

// EventRecorder is the subset of record.EventRecorder the gateway uses for
// runtime warnings. A nil *Server.Recorder is handled by recordEvent.
type EventRecorder interface {
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...any)
}

// recordEvent emits a Warning Event when a recorder is configured.
func (s *Server) recordEvent(object runtime.Object, reason, messageFmt string, args ...any) {
	if s.Recorder == nil {
		return
	}
	s.Recorder.Eventf(object, "Warning", reason, messageFmt, args...)
}

// forwardResult is one upstream attempt's classified outcome. Exactly one of
// resp or the failure fields is meaningful.
type forwardResult struct {
	resp       *http.Response              // set on a completed HTTP round trip (any status)
	body       []byte                      // buffered non-stream body, if read
	fallilable bool                        // whether this outcome should trigger fallback
	class      failClass                   // the failure class, for the exhaustion mapping
	err        error                       // transport error, if any
	provider   string                      // the candidate that produced this result
	chosen     *kaalmv1beta1.ModelProvider // the candidate resource, for usage accounting
	model      string                      // the model the candidate served (the edge's map applied)
	format     llmtranslate.Format         // the candidate's wire format ("" when it cannot cross)
	// settle frees the winning candidate's boundary admission slot with the
	// request's actual cost (hard enforcement). Nil outside the boundary
	// region; idempotent; the caller MUST invoke it on every outcome path.
	settle func(costUSD float64)
}

// fallbackReasonSuccess is the metric reason label for a fallback attempt that
// succeeded (as opposed to the failClassName labels for failed attempts).
const fallbackReasonSuccess = "success"

// failClassName maps a failure class to a metric label.
func failClassName(c failClass) string {
	switch c {
	case classConnect:
		return "connect_error"
	case classTimeout:
		return "timeout"
	case classBudget:
		return "budget_blocked"
	case classBudgetUnavailable:
		return "budget_unavailable"
	case classUpstream:
		return "upstream_error"
	default:
		return "other"
	}
}

// failClass buckets a failed attempt for the exhaustion error mapping.
type failClass int

const (
	classNone              failClass = iota
	classConnect                     // connection/DNS/TLS error
	classTimeout                     // pre-stream timeout
	classUpstream                    // 5xx, upstream 429, 401/403
	classBudget                      // budget-blocked or throttled candidate (consumed a slot)
	classNonFallle                   // 400/422/other 4xx: not fallbackable
	classBudgetUnavailable           // hard-enforcement fail-closed candidate (consumed a slot)
)

// isFallbackable classifies an upstream HTTP status. Transport errors are
// classified separately by the caller. See docs/src/gateways/llm/fallback.md.
func isFallbackable(status int) (bool, failClass) {
	switch {
	case status >= 200 && status <= 299:
		return false, classNone
	case status == 429 || (status >= 500 && status <= 599):
		return true, classUpstream
	case status == 401 || status == 403:
		return true, classUpstream
	case status == 400 || status == 422:
		return false, classNonFallle
	case status >= 400 && status <= 499:
		return false, classNonFallle
	}
	return true, classUpstream
}

// walkState threads the traversal budget and dedup set through the recursion.
type walkState struct {
	primary      *kaalmv1beta1.ModelProvider
	namespace    string
	workload     string
	modelID      string
	attemptCount int
	maxDepth     int
	visited      map[string]bool
	// observed collects failure classes across the walk for the exhaustion
	// mapping; maxRetryAfter carries the largest budget Retry-After seen.
	observed      map[failClass]bool
	maxRetryAfter int
	// primarySettle is the pre-flight boundary settle for the primary
	// candidate (hard enforcement); the walk never re-admits the primary.
	primarySettle func(costUSD float64)

	// Crossing formats (since v0.7.0). parsed is the request as the caller
	// sent it; inboundFormat is its format, or "" when it cannot cross (the
	// legacy completions shape, Vertex). modelFor carries each candidate's
	// model with the edge's modelMap applied; translated caches the body
	// translated for each crossing candidate, built by the eligibility check
	// and consumed by the attempt.
	parsed        map[string]any
	inboundFormat llmtranslate.Format
	modelFor      map[string]string
	translated    map[string]map[string]any
}

// candidateModel is the model a candidate serves: the edge's mapping when
// one was recorded on the way down, else the model the walk started with.
func (st *walkState) candidateModel(provider *kaalmv1beta1.ModelProvider) string {
	if m, ok := st.modelFor[provider.Name]; ok && m != "" {
		return m
	}
	return st.modelID
}

// crosses reports whether reaching provider from the request's format needs
// translation.
func (st *walkState) crosses(provider *kaalmv1beta1.ModelProvider) bool {
	target := formatForType(provider.Spec.Type)
	return st.inboundFormat != "" && target != "" && target != st.inboundFormat
}

// formatForType maps a provider type to the translator's format, or "" for
// a type the translator does not speak.
func formatForType(providerType string) llmtranslate.Format {
	switch kaalmv1beta1.ProviderFormat(providerType) {
	case kaalmv1beta1.ProviderTypeAnthropic:
		return llmtranslate.FormatAnthropic
	case kaalmv1beta1.ProviderTypeOpenAI:
		return llmtranslate.FormatOpenAI
	}
	return ""
}

// canonicalPath is the inbound path a format's requests use, the path a
// crossing candidate is forwarded on.
func canonicalPath(format llmtranslate.Format) string {
	if format == llmtranslate.FormatAnthropic {
		return "/v1/messages"
	}
	return "/v1/chat/completions"
}

func maxOutputTokensOf(provider *kaalmv1beta1.ModelProvider, model string) int64 {
	for _, m := range provider.Spec.Models {
		if m.ID == model && m.MaxOutputTokens != nil {
			return *m.MaxOutputTokens
		}
	}
	return 0
}

// tryWithFallbacks walks the fallback tree depth-first in declared order,
// returning the first successful attempt or a classified exhaustion. attempt
// runs one candidate; it returns the classified result.
func (s *Server) tryWithFallbacks(
	ctx context.Context, provider *kaalmv1beta1.ModelProvider, st *walkState,
	attempt func(context.Context, *kaalmv1beta1.ModelProvider) forwardResult,
) (forwardResult, bool) {
	if st.visited[provider.Name] {
		return forwardResult{}, false // cycle: defense in depth
	}
	st.visited[provider.Name] = true

	if reason := s.staticallyIneligible(provider, st); reason != "" {
		// Misconfiguration: skip WITHOUT consuming a slot, warn on the
		// primary, and do not walk this provider's children.
		s.recordEvent(st.primary, kaalmv1beta1.ReasonFallbackIneligible,
			"fallback %q skipped: %s", provider.Name, reason)
		return forwardResult{}, false
	}

	if st.attemptCount >= st.maxDepth {
		return forwardResult{}, false
	}
	st.attemptCount++

	// Budget admission. The primary was admitted pre-flight (its settle
	// rides in via st.primarySettle); every other candidate is admitted
	// here under the same rules. Blocked, throttled, or failed-closed
	// candidates consume the attempt slot and fall through to children.
	var settle func(float64)
	if s.Budget != nil {
		if provider.Name == st.primary.Name {
			settle = st.primarySettle
		} else {
			d, sfn := s.Budget.Admit(provider, st.namespace, st.workload)
			if d.MarginRaisedNow {
				s.Metrics.BudgetBoundary(provider.Name, st.namespace, "margin_raised")
			}
			switch {
			case d.Unavailable:
				s.Metrics.BudgetBoundary(provider.Name, st.namespace, "fail_closed")
				st.observed[classBudgetUnavailable] = true
				if res, ok := s.walkChildren(ctx, provider, st, attempt); ok {
					return res, true
				}
				return forwardResult{class: classBudgetUnavailable}, false
			case d.Throttled:
				s.Metrics.BudgetBoundary(provider.Name, st.namespace, "throttled")
				st.observed[classBudget] = true
				if st.maxRetryAfter < 1 {
					st.maxRetryAfter = 1
				}
				if res, ok := s.walkChildren(ctx, provider, st, attempt); ok {
					return res, true
				}
				return forwardResult{class: classBudget}, false
			case d.Action == kaalmv1beta1.BudgetActionBlock:
				st.observed[classBudget] = true
				if d.RetryAfter > st.maxRetryAfter {
					st.maxRetryAfter = d.RetryAfter
				}
				if res, ok := s.walkChildren(ctx, provider, st, attempt); ok {
					return res, true
				}
				return forwardResult{class: classBudget}, false
			default:
				settle = sfn
				if sfn != nil {
					s.Metrics.BudgetBoundary(provider.Name, st.namespace, "engaged")
				}
			}
		}
	}

	res := attempt(ctx, provider)
	res.settle = settle
	if res.class == classNone && res.err == nil && res.resp != nil &&
		res.resp.StatusCode >= 200 && res.resp.StatusCode <= 299 {
		return res, true
	}
	if !res.fallilable {
		// 400/422/other 4xx: pass back to the caller verbatim, no walk.
		return res, true
	}

	// Fallbackable failure: free this candidate's slot BEFORE descending,
	// so no slot is ever held across another provider's upstream call.
	if settle != nil {
		settle(0)
		res.settle = nil
	}
	if res.class == classUpstream && res.resp != nil &&
		(res.resp.StatusCode == 401 || res.resp.StatusCode == 403) {
		s.recordEvent(provider, kaalmv1beta1.ReasonCredentialsInvalid,
			"upstream returned %d; credential rotation may be needed", res.resp.StatusCode)
	}
	if child, ok := s.walkChildren(ctx, provider, st, attempt); ok {
		return child, true
	}
	return res, false
}

func (s *Server) walkChildren(
	ctx context.Context, provider *kaalmv1beta1.ModelProvider, st *walkState,
	attempt func(context.Context, *kaalmv1beta1.ModelProvider) forwardResult,
) (forwardResult, bool) {
	parentModel := st.candidateModel(provider)
	for _, ref := range provider.Spec.Fallback {
		next, ok := s.Store.ProviderByName(ctx, ref.Name)
		if !ok {
			s.recordEvent(st.primary, kaalmv1beta1.ReasonFallbackIneligible,
				"fallback %q skipped: provider does not exist", ref.Name)
			continue
		}
		// The edge's model map, applied on the way down (rule 41 validated it).
		if st.modelFor == nil {
			st.modelFor = map[string]string{}
		}
		if mapped, ok := ref.ModelMap[parentModel]; ok && mapped != "" {
			st.modelFor[next.Name] = mapped
		} else {
			st.modelFor[next.Name] = parentModel
		}
		if res, ok := s.tryWithFallbacks(ctx, next, st, attempt); ok {
			return res, true
		}
	}
	return forwardResult{}, false
}

// staticallyIneligible returns a non-empty reason when a candidate fails a
// check that never consumes a slot: format compatibility (rule 12),
// namespace, the mapped model, and, for a crossing, translatability of this
// request (the one check derived from the body rather than configuration).
func (s *Server) staticallyIneligible(provider *kaalmv1beta1.ModelProvider, st *walkState) string {
	if provider.Name != st.primary.Name {
		if !kaalmv1beta1.FallbackFormatCompatible(st.primary.Spec.Type, provider.Spec.Type) {
			return fmt.Sprintf("type %q cannot follow primary type %q (rule 12)", provider.Spec.Type, st.primary.Spec.Type)
		}
		if kaalmv1beta1.FallbackCrossesFormat(st.primary.Spec.Type, provider.Spec.Type) && st.inboundFormat == "" {
			return fmt.Sprintf("the request's format cannot cross into type %q", provider.Spec.Type)
		}
	}
	if !namespaceGlobAllowed(st.namespace, provider.Spec.AllowedNamespaces) {
		return fmt.Sprintf("namespace %q not in allowedNamespaces", st.namespace)
	}
	model := st.candidateModel(provider)
	found := false
	for _, m := range provider.Spec.Models {
		if m.ID == model {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("model %q not offered", model)
	}
	if st.crosses(provider) && st.parsed != nil {
		if _, done := st.translated[provider.Name]; !done {
			body, err := llmtranslate.Request(st.inboundFormat, formatForType(provider.Spec.Type), st.parsed, model,
				maxOutputTokensOf(provider, model))
			if err != nil {
				return err.Error()
			}
			if st.translated == nil {
				st.translated = map[string]map[string]any{}
			}
			st.translated[provider.Name] = body
		}
	}
	return ""
}

// exhaustionError maps the failure classes observed across the walk to a
// terminal error, per the depth-cap-semantics table. A walk exhausted
// entirely by budget outcomes is a budget error, never 502: 429 when every
// outcome was a block or throttle, 503 when fail-closed candidates are why.
func exhaustionError(observed map[failClass]bool, maxRetryAfter int, providerName string) (int, errorBody, int) {
	budgetOnly := len(observed) > 0
	for class := range observed {
		if class != classBudget && class != classBudgetUnavailable {
			budgetOnly = false
			break
		}
	}
	switch {
	case budgetOnly && observed[classBudgetUnavailable]:
		return http.StatusServiceUnavailable, errorBody{
			Type: errBudgetUnavailable, Provider: providerName, Retryable: true,
			Message: "budget state could not be verified for the provider and all fallbacks; failing closed"}, 1
	case budgetOnly:
		return http.StatusTooManyRequests, errorBody{
			Type: errBudgetExhausted, Provider: providerName, Retryable: true,
			Message: "the provider and all fallbacks are budget-blocked"}, maxRetryAfter
	case len(observed) == 1 && observed[classConnect]:
		return http.StatusServiceUnavailable, errorBody{
			Type: errProviderUnavailable, Provider: providerName,
			Message: "all providers were unreachable"}, 0
	case len(observed) == 1 && observed[classTimeout]:
		return http.StatusGatewayTimeout, errorBody{
			Type: errProviderTimeout, Provider: providerName,
			Message: "all provider attempts timed out"}, 0
	default:
		return http.StatusBadGateway, errorBody{
			Type: errProviderError, Provider: providerName,
			Message: "the provider and all fallbacks failed"}, 0
	}
}
