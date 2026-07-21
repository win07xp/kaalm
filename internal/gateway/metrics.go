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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// Metrics is the gateway's Prometheus catalog (docs/src/operations/observability.md).
// No metric carries per-Agent or per-AgentTask identity: that resolution lives
// in logs and Events to keep cardinality bounded at 1000+ agents. A nil
// *Metrics no-ops every method, so tests need no registry.
type Metrics struct {
	llmRequests    *prometheus.CounterVec
	llmDuration    *prometheus.HistogramVec
	llmTokens      *prometheus.CounterVec
	llmSpend       *prometheus.CounterVec
	llmFallback    *prometheus.CounterVec
	budgetThreshld *prometheus.CounterVec
	channelMsgs    *prometheus.CounterVec
	channelWake    *prometheus.CounterVec
	channelCB      *prometheus.CounterVec
	tooLarge       *prometheus.CounterVec
	patchFailed    *prometheus.CounterVec
}

// NewMetrics registers the gateway catalog with the given registerer.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		llmRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_llm_requests_total", Help: "LLM proxy requests by outcome.",
		}, []string{"provider", "model", "namespace", "status"}),
		llmDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kaalm_llm_request_duration_seconds", Help: "LLM proxy request duration.",
		}, []string{"provider", "model"}),
		llmTokens: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_llm_tokens_total", Help: "Token usage by direction.",
		}, []string{"provider", "model", "namespace", "direction"}),
		llmSpend: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_llm_spend_usd_total", Help: "Accumulated LLM spend in USD.",
		}, []string{"provider", "namespace"}),
		llmFallback: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_llm_fallback_total", Help: "Fallback attempts by reason.",
		}, []string{"from_provider", "to_provider", "reason"}),
		budgetThreshld: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_budget_threshold_events_total", Help: "Budget threshold actions fired.",
		}, []string{"provider", "namespace", "action"}),
		channelMsgs: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_channel_messages_total", Help: "Channel messages by outcome.",
		}, []string{"channel_type", "namespace", "status"}),
		channelWake: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_channel_wake_total", Help: "Wake-on-demand triggers.",
		}, []string{"namespace"}),
		channelCB: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_channel_callback_total", Help: "Async callback attempts by outcome.",
		}, []string{"namespace", "status"}),
		tooLarge: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_channel_response_too_large_total", Help: "Oversized agent responses.",
		}, []string{"namespace", "mode"}),
		patchFailed: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kaalm_channel_async_patch_failed_total", Help: "Async response patch exhaustions (v1 silent-loss).",
		}, []string{"namespace"}),
	}
}

// LLMRequest counts one proxied request by outcome (ok | error | rate_limited).
func (m *Metrics) LLMRequest(provider, model, namespace, status string) {
	if m == nil {
		return
	}
	m.llmRequests.WithLabelValues(provider, model, namespace, status).Inc()
}

// Duration records an LLM request's wall-clock.
func (m *Metrics) Duration(provider, model string, seconds float64) {
	if m == nil {
		return
	}
	m.llmDuration.WithLabelValues(provider, model).Observe(seconds)
}

// Tokens counts input and output tokens.
func (m *Metrics) Tokens(provider, model, namespace string, usage Usage) {
	if m == nil {
		return
	}
	m.llmTokens.WithLabelValues(provider, model, namespace, "input").Add(float64(usage.InputTokens))
	m.llmTokens.WithLabelValues(provider, model, namespace, "output").Add(float64(usage.OutputTokens))
}

// Spend accumulates USD spend.
func (m *Metrics) Spend(provider, namespace string, usd float64) {
	if m == nil || usd == 0 {
		return
	}
	m.llmSpend.WithLabelValues(provider, namespace).Add(usd)
}

// Fallback counts one fallback attempt.
func (m *Metrics) Fallback(from, to, reason string) {
	if m == nil {
		return
	}
	m.llmFallback.WithLabelValues(from, to, reason).Inc()
}

// BudgetThreshold counts one budget policy action.
func (m *Metrics) BudgetThreshold(provider, namespace, action string) {
	if m == nil {
		return
	}
	m.budgetThreshld.WithLabelValues(provider, namespace, action).Inc()
}

// ChannelMessage counts one webhook delivery by outcome.
func (m *Metrics) ChannelMessage(namespace, status string) {
	if m == nil {
		return
	}
	m.channelMsgs.WithLabelValues("webhook", namespace, status).Inc()
}

// ChannelWake counts one wake trigger.
func (m *Metrics) ChannelWake(namespace string) {
	if m == nil {
		return
	}
	m.channelWake.WithLabelValues(namespace).Inc()
}

// ChannelCallback counts one callback attempt.
func (m *Metrics) ChannelCallback(namespace, status string) {
	if m == nil {
		return
	}
	m.channelCB.WithLabelValues(namespace, status).Inc()
}

// ResponseTooLarge counts one oversized reply.
func (m *Metrics) ResponseTooLarge(namespace, mode string) {
	if m == nil {
		return
	}
	m.tooLarge.WithLabelValues(namespace, mode).Inc()
}

// AsyncPatchFailed counts one dropped async payload.
func (m *Metrics) AsyncPatchFailed(namespace string) {
	if m == nil {
		return
	}
	m.patchFailed.WithLabelValues(namespace).Inc()
}

var _ = kaalmv1alpha1.GroupVersion
