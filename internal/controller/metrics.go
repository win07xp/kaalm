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

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// labelNamespace is the shared Prometheus label name.
const labelNamespace = "namespace"

// The controller's Kaalm-specific Prometheus catalog. Standard reconcile
// metrics come from controller-runtime automatically. No metric carries
// per-Agent identity as a label (docs/src/operations/observability.md). These
// register against the shared controller-runtime registry, served on the
// manager's metrics port.
var (
	hibernationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kaalm_hibernations_total", Help: "Agent hibernations by namespace.",
	}, []string{labelNamespace})

	wakesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kaalm_wakes_total", Help: "Agent wakes by namespace and trigger.",
	}, []string{labelNamespace, "trigger"})

	budgetThresholdEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kaalm_budget_threshold_events_total", Help: "Reconcile-observed budget threshold actions.",
	}, []string{"provider", labelNamespace, "action"})

	providerBudgetCanonical = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kaalm_provider_budget_canonical_usd", Help: "Canonical per-namespace spend roll-up.",
	}, []string{"provider", labelNamespace, "period"})
)

func init() {
	metrics.Registry.MustRegister(hibernationsTotal, wakesTotal, budgetThresholdEvents, providerBudgetCanonical)
}
