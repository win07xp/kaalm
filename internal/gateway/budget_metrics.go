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
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// utilizationCollectTimeout bounds the provider list behind one scrape.
const utilizationCollectTimeout = 5 * time.Second

// budgetUtilizationDesc is the gauge the per-provider dashboard plots
// (docs/src/gateways/llm/operations.md#observability): the namespace's share
// of its per-namespace ceiling, 0-1 and uncapped above 1.
var budgetUtilizationDesc = prometheus.NewDesc("kaalm_llm_budget_utilization",
	"Per-namespace budget utilization as a 0-1 ratio of the per-namespace ceiling, from the replica's folded ledger.",
	[]string{labelProvider, labelNamespace, labelPeriod}, nil)

// Utilization reports, for every namespace with spend this period, that
// namespace's ratio of the provider's per-namespace ceiling, from the
// enforcement view (own live counter plus folded peer partials). Providers
// without a per-namespace ceiling report nothing: there is no ratio to take.
func (b *BudgetLedger) Utilization(provider *kaalmv1alpha1.ModelProvider) (period string, byNamespace map[string]float64) {
	ceiling, err := strconv.ParseFloat(provider.Spec.Budget.PerNamespaceUSD, 64)
	if err != nil || ceiling <= 0 {
		return "", nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerFor(provider.Name, provider.Spec.Budget.Period)
	out := map[string]float64{}
	for ns := range l.own {
		out[ns] = l.spent(ns) / ceiling
	}
	for ns := range l.peers {
		out[ns] = l.spent(ns) / ceiling
	}
	return l.period, out
}

// BudgetUtilizationCollector serves kaalm_llm_budget_utilization from the
// ledger on every scrape. Every replica holds the folded union, so every
// replica reports the same figure to within one publish interval; dashboards
// aggregate with max, not sum.
type BudgetUtilizationCollector struct {
	Ledger    *BudgetLedger
	Providers func(ctx context.Context) []*kaalmv1alpha1.ModelProvider
}

// Describe implements prometheus.Collector.
func (c *BudgetUtilizationCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- budgetUtilizationDesc
}

// Collect implements prometheus.Collector.
func (c *BudgetUtilizationCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), utilizationCollectTimeout)
	defer cancel()
	for _, p := range c.Providers(ctx) {
		period, ratios := c.Ledger.Utilization(p)
		for ns, ratio := range ratios {
			ch <- prometheus.MustNewConstMetric(budgetUtilizationDesc, prometheus.GaugeValue, ratio, p.Name, ns, period)
		}
	}
}
