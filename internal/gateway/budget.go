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
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// BudgetConfigMapName returns the per-provider budget ConfigMap name.
func BudgetConfigMapName(providerName string) string { return "kaalm-budget-" + providerName }

// CanonicalKey is the reconciler-owned roll-up key in the budget ConfigMap.
const CanonicalKey = "_canonical"

// budgetPartial is the JSON value under each per-replica key. The period tag
// lets the reducer drop stale entries during rollover.
type budgetPartial struct {
	Period string `json:"period"`
	// Spend is per-namespace USD as decimal strings, flattened into the same
	// JSON object as period on the wire.
	Spend map[string]string `json:"-"`
}

// MarshalJSON flattens period and the namespace map into one object, matching
// the documented ConfigMap layout.
func (p budgetPartial) MarshalJSON() ([]byte, error) {
	out := map[string]string{"period": p.Period}
	for ns, v := range p.Spend {
		out[ns] = v
	}
	return json.Marshal(out)
}

// ParseBudgetPartial decodes a per-replica ConfigMap value.
func ParseBudgetPartial(raw string) (period string, spend map[string]float64, err error) {
	var flat map[string]string
	if err := json.Unmarshal([]byte(raw), &flat); err != nil {
		return "", nil, err
	}
	spend = map[string]float64{}
	for k, v := range flat {
		if k == "period" {
			period = v
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		spend[k] = f
	}
	return period, spend, nil
}

// PeriodKey computes the budget period identifier for a scheme at time t
// (UTC). Scheme none returns "" and disables budget tracking.
func PeriodKey(scheme string, t time.Time) string {
	t = t.UTC()
	switch scheme {
	case "monthly":
		return t.Format("2006-01")
	case "weekly":
		year, week := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", year, week)
	case "daily":
		return t.Format("2006-01-02")
	}
	return ""
}

// nextPeriodStart returns when the current period rolls over, for Retry-After.
func nextPeriodStart(scheme string, t time.Time) time.Time {
	t = t.UTC()
	switch scheme {
	case "monthly":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	case "weekly":
		daysUntilMonday := (8 - int(t.Weekday())) % 7
		if daysUntilMonday == 0 {
			daysUntilMonday = 7
		}
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, daysUntilMonday)
	default: // daily
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	}
}

// costOf prices a call from the provider's model catalog. Unpriced models
// cost zero (spend visibility is a soft guardrail).
func costOf(provider *kaalmv1alpha1.ModelProvider, modelID string, usage Usage) float64 {
	for _, m := range provider.Spec.Models {
		if m.ID != modelID {
			continue
		}
		in, errIn := strconv.ParseFloat(m.CostPer1MInputTokens, 64)
		out, errOut := strconv.ParseFloat(m.CostPer1MOutputTokens, 64)
		if errIn != nil || errOut != nil {
			return 0
		}
		return float64(usage.InputTokens)*in/1e6 + float64(usage.OutputTokens)*out/1e6
	}
	return 0
}

// BudgetLedger keeps this replica's in-memory spend counters plus the folded
// view of peer partials, per (provider, namespace, period). Enforcement reads
// own live spend plus peers' latest partials, at most one publish interval
// stale. See docs/src/gateways/llm/budgets-and-rate-limits.md.
type BudgetLedger struct {
	now func() time.Time

	mu        sync.Mutex
	providers map[string]*providerLedger
}

type providerLedger struct {
	period string
	own    map[string]float64
	peers  map[string]float64
}

// NewBudgetLedger builds an empty ledger.
func NewBudgetLedger() *BudgetLedger {
	return &BudgetLedger{now: time.Now, providers: map[string]*providerLedger{}}
}

// ledgerFor returns the provider's ledger, rolling the period over (and
// resetting counters) when the clock has crossed a boundary.
func (b *BudgetLedger) ledgerFor(providerName, scheme string) *providerLedger {
	period := PeriodKey(scheme, b.now())
	l, ok := b.providers[providerName]
	if !ok {
		l = &providerLedger{period: period, own: map[string]float64{}, peers: map[string]float64{}}
		b.providers[providerName] = l
	}
	if l.period != period {
		l.period = period
		l.own = map[string]float64{}
		l.peers = map[string]float64{}
	}
	return l
}

// Add records spend for a namespace after a call completes.
func (b *BudgetLedger) Add(provider *kaalmv1alpha1.ModelProvider, namespace string, costUSD float64) {
	scheme := provider.Spec.Budget.Period
	if PeriodKey(scheme, b.now()) == "" || costUSD == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ledgerFor(provider.Name, scheme).own[namespace] += costUSD
}

// FoldPeers replaces the peer view for a provider from freshly read
// current-period partials (own key excluded by the caller).
func (b *BudgetLedger) FoldPeers(provider *kaalmv1alpha1.ModelProvider, peers map[string]float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerFor(provider.Name, provider.Spec.Budget.Period)
	l.peers = peers
}

// InitCanonical seeds the peer view from the reconciler's _canonical roll-up
// at startup (read exactly once per provider per replica lifetime).
func (b *BudgetLedger) InitCanonical(provider *kaalmv1alpha1.ModelProvider, canonical map[string]float64) {
	b.FoldPeers(provider, canonical)
}

// OwnPartial snapshots this replica's counters for publishing.
func (b *BudgetLedger) OwnPartial(providerName string) (period string, spend map[string]string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, exists := b.providers[providerName]
	if !exists || len(l.own) == 0 {
		return "", nil, false
	}
	out := map[string]string{}
	for ns, v := range l.own {
		out[ns] = strconv.FormatFloat(v, 'f', 2, 64)
	}
	return l.period, out, true
}

// spent returns the enforcement view: own live counter plus peer partials.
func (l *providerLedger) spent(namespace string) float64 {
	return l.own[namespace] + l.peers[namespace]
}

func (l *providerLedger) clusterSpent() float64 {
	var total float64
	for _, v := range l.own {
		total += v
	}
	for _, v := range l.peers {
		total += v
	}
	return total
}

// budgetDecision is the pre-call enforcement outcome.
type budgetDecision struct {
	// Action is "", "warn", "degrade", or "block".
	Action     string
	DegradeTo  string
	RetryAfter int // seconds until the next period, for block
	Percent    int
}

// Enforce evaluates the provider's budget policies for a namespace using
// last-known spend state (no pre-call estimation). The highest-threshold
// policy at or below the current utilization wins. Utilization is the worse
// of the per-namespace and cluster-wide ratios.
func (b *BudgetLedger) Enforce(provider *kaalmv1alpha1.ModelProvider, namespace string) budgetDecision {
	budget := provider.Spec.Budget
	scheme := budget.Period
	if PeriodKey(scheme, b.now()) == "" || len(budget.Policies) == 0 {
		return budgetDecision{}
	}
	b.mu.Lock()
	l := b.ledgerFor(provider.Name, scheme)
	nsSpent := l.spent(namespace)
	clusterSpent := l.clusterSpent()
	b.mu.Unlock()

	percent := 0.0
	if ceiling, err := strconv.ParseFloat(budget.PerNamespaceUSD, 64); err == nil && ceiling > 0 {
		percent = nsSpent / ceiling * 100
	}
	if budget.ClusterUSD != nil {
		if ceiling, err := strconv.ParseFloat(*budget.ClusterUSD, 64); err == nil && ceiling > 0 {
			if p := clusterSpent / ceiling * 100; p > percent {
				percent = p
			}
		}
	}

	var winner *kaalmv1alpha1.ModelProviderBudgetPolicy
	for i := range budget.Policies {
		p := &budget.Policies[i]
		if percent >= float64(p.AtPercent) && (winner == nil || p.AtPercent > winner.AtPercent) {
			winner = p
		}
	}
	if winner == nil {
		return budgetDecision{Percent: int(percent)}
	}
	d := budgetDecision{Action: winner.Action, Percent: int(percent)}
	if winner.Action == kaalmv1alpha1.BudgetActionDegrade && winner.DegradeTo != nil {
		d.DegradeTo = *winner.DegradeTo
	}
	if winner.Action == kaalmv1alpha1.BudgetActionBlock {
		d.RetryAfter = int(time.Until(nextPeriodStart(scheme, b.now())).Seconds()) + 1
	}
	return d
}

// BudgetPublisher runs the replica side of the budget counter exchange: every
// interval it server-side-applies this replica's partials (field manager =
// Pod name, so simultaneous writes never conflict) and folds peers' current-
// period partials into the enforcement view. The read-on-tick fold has the
// same staleness bound as a watch-driven fold: at most one publish interval.
type BudgetPublisher struct {
	Client            kubernetes.Interface
	Store             Store
	Ledger            *BudgetLedger
	OperatorNamespace string
	PodName           string
	Interval          time.Duration
	// Providers enumerates the ModelProviders to exchange for.
	Providers func(ctx context.Context) []*kaalmv1alpha1.ModelProvider
}

// Run loops until ctx is done.
func (p *BudgetPublisher) Run(ctx context.Context) {
	interval := p.Interval
	if interval == 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

func (p *BudgetPublisher) tick(ctx context.Context) {
	for _, provider := range p.Providers(ctx) {
		if PeriodKey(provider.Spec.Budget.Period, time.Now()) == "" {
			continue
		}
		p.publish(ctx, provider)
		p.fold(ctx, provider)
	}
}

func (p *BudgetPublisher) publish(ctx context.Context, provider *kaalmv1alpha1.ModelProvider) {
	period, spend, ok := p.Ledger.OwnPartial(provider.Name)
	if !ok {
		return
	}
	raw, err := json.Marshal(budgetPartial{Period: period, Spend: spend})
	if err != nil {
		return
	}
	apply := applycorev1.ConfigMap(BudgetConfigMapName(provider.Name), p.OperatorNamespace).
		WithData(map[string]string{p.PodName: string(raw)})
	_, err = p.Client.CoreV1().ConfigMaps(p.OperatorNamespace).Apply(ctx, apply,
		metav1.ApplyOptions{FieldManager: p.PodName, Force: true})
	if err != nil {
		slog.Warn("budget partial publish failed", "provider", provider.Name, "error", err)
	}
}

func (p *BudgetPublisher) fold(ctx context.Context, provider *kaalmv1alpha1.ModelProvider) {
	cm, err := p.Client.CoreV1().ConfigMaps(p.OperatorNamespace).Get(ctx, BudgetConfigMapName(provider.Name), metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			slog.Warn("budget fold read failed", "provider", provider.Name, "error", err)
		}
		return
	}
	currentPeriod := PeriodKey(provider.Spec.Budget.Period, time.Now())
	peers := map[string]float64{}
	for key, raw := range cm.Data {
		if key == p.PodName || key == CanonicalKey {
			continue
		}
		period, spend, err := ParseBudgetPartial(raw)
		if err != nil || period != currentPeriod {
			continue
		}
		for ns, v := range spend {
			peers[ns] += v
		}
	}
	p.Ledger.FoldPeers(provider, peers)
}

// SeedFromCanonical initializes the ledger from each provider's _canonical
// key, called once at startup.
func (p *BudgetPublisher) SeedFromCanonical(ctx context.Context) {
	for _, provider := range p.Providers(ctx) {
		cm, err := p.Client.CoreV1().ConfigMaps(p.OperatorNamespace).Get(ctx, BudgetConfigMapName(provider.Name), metav1.GetOptions{})
		if err != nil {
			continue
		}
		raw, ok := cm.Data[CanonicalKey]
		if !ok {
			continue
		}
		var flat map[string]string
		if err := json.Unmarshal([]byte(raw), &flat); err != nil {
			continue
		}
		canonical := map[string]float64{}
		for ns, v := range flat {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				canonical[ns] = f
			}
		}
		p.Ledger.InitCanonical(provider, canonical)
	}
}
