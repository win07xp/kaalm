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
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
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
	resp       *http.Response               // set on a completed HTTP round trip (any status)
	body       []byte                       // buffered non-stream body, if read
	fallilable bool                         // whether this outcome should trigger fallback
	class      failClass                    // the failure class, for the exhaustion mapping
	err        error                        // transport error, if any
	provider   string                       // the candidate that produced this result
	chosen     *kaalmv1alpha1.ModelProvider // the candidate resource, for usage accounting
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
	case classUpstream:
		return "upstream_error"
	default:
		return "other"
	}
}

// failClass buckets a failed attempt for the exhaustion error mapping.
type failClass int

const (
	classNone      failClass = iota
	classConnect             // connection/DNS/TLS error
	classTimeout             // pre-stream timeout
	classUpstream            // 5xx, upstream 429, 401/403
	classBudget              // budget-blocked candidate (consumed a slot)
	classNonFallle           // 400/422/other 4xx: not fallbackable
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
	primary      *kaalmv1alpha1.ModelProvider
	namespace    string
	modelID      string
	attemptCount int
	maxDepth     int
	visited      map[string]bool
}

// tryWithFallbacks walks the fallback tree depth-first in declared order,
// returning the first successful attempt or a classified exhaustion. attempt
// runs one candidate; it returns the classified result.
func (s *Server) tryWithFallbacks(
	ctx context.Context, provider *kaalmv1alpha1.ModelProvider, st *walkState,
	attempt func(context.Context, *kaalmv1alpha1.ModelProvider) forwardResult,
) (forwardResult, bool) {
	if st.visited[provider.Name] {
		return forwardResult{}, false // cycle: defense in depth
	}
	st.visited[provider.Name] = true

	if reason := s.staticallyIneligible(provider, st); reason != "" {
		// Misconfiguration: skip WITHOUT consuming a slot, warn on the
		// primary, and do not walk this provider's children.
		s.recordEvent(st.primary, kaalmv1alpha1.ReasonFallbackIneligible,
			"fallback %q skipped: %s", provider.Name, reason)
		return forwardResult{}, false
	}

	if st.attemptCount >= st.maxDepth {
		return forwardResult{}, false
	}
	st.attemptCount++

	if s.Budget != nil {
		if d := s.Budget.Enforce(provider, st.namespace); d.Action == kaalmv1alpha1.BudgetActionBlock {
			// Consumed a slot; fall through to children without forwarding.
			if res, ok := s.walkChildren(ctx, provider, st, attempt); ok {
				return res, true
			}
			return forwardResult{class: classBudget}, false
		}
	}

	res := attempt(ctx, provider)
	if res.class == classNone && res.err == nil && res.resp != nil &&
		res.resp.StatusCode >= 200 && res.resp.StatusCode <= 299 {
		return res, true
	}
	if !res.fallilable {
		// 400/422/other 4xx: pass back to the caller verbatim, no walk.
		return res, true
	}

	// Fallbackable failure: record it and walk children.
	if res.class == classUpstream && res.resp != nil &&
		(res.resp.StatusCode == 401 || res.resp.StatusCode == 403) {
		s.recordEvent(provider, kaalmv1alpha1.ReasonCredentialsInvalid,
			"upstream returned %d; credential rotation may be needed", res.resp.StatusCode)
	}
	if child, ok := s.walkChildren(ctx, provider, st, attempt); ok {
		return child, true
	}
	return res, false
}

func (s *Server) walkChildren(
	ctx context.Context, provider *kaalmv1alpha1.ModelProvider, st *walkState,
	attempt func(context.Context, *kaalmv1alpha1.ModelProvider) forwardResult,
) (forwardResult, bool) {
	for _, ref := range provider.Spec.Fallback {
		next, ok := s.Store.ProviderByName(ctx, ref.Name)
		if !ok {
			s.recordEvent(st.primary, kaalmv1alpha1.ReasonFallbackIneligible,
				"fallback %q skipped: provider does not exist", ref.Name)
			continue
		}
		if res, ok := s.tryWithFallbacks(ctx, next, st, attempt); ok {
			return res, true
		}
	}
	return forwardResult{}, false
}

// staticallyIneligible returns a non-empty reason when a candidate fails a
// config-derived check (same type, namespace, model). These never consume a
// slot.
func (s *Server) staticallyIneligible(provider *kaalmv1alpha1.ModelProvider, st *walkState) string {
	if provider.Name != st.primary.Name && provider.Spec.Type != st.primary.Spec.Type {
		return fmt.Sprintf("type %q does not match primary type %q", provider.Spec.Type, st.primary.Spec.Type)
	}
	if !namespaceGlobAllowed(st.namespace, provider.Spec.AllowedNamespaces) {
		return fmt.Sprintf("namespace %q not in allowedNamespaces", st.namespace)
	}
	found := false
	for _, m := range provider.Spec.Models {
		if m.ID == st.modelID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("model %q not offered", st.modelID)
	}
	return ""
}

// exhaustionError maps the failure classes observed across the walk to a
// terminal error, per the depth-cap-semantics table.
func exhaustionError(observed map[failClass]bool, providerName string) (int, errorBody) {
	switch {
	case len(observed) == 1 && observed[classConnect]:
		return http.StatusServiceUnavailable, errorBody{
			Type: errProviderUnavailable, Provider: providerName,
			Message: "all providers were unreachable"}
	case len(observed) == 1 && observed[classTimeout]:
		return http.StatusGatewayTimeout, errorBody{
			Type: errProviderTimeout, Provider: providerName,
			Message: "all provider attempts timed out"}
	default:
		return http.StatusBadGateway, errorBody{
			Type: errProviderError, Provider: providerName,
			Message: "the provider and all fallbacks failed"}
	}
}
