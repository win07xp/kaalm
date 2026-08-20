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
	"strings"
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

// AgentSpendConfigMapName returns the per-provider workload-spend ConfigMap
// name. Workload spend lives in its own object so the budget enforcement
// fold never sees its keys and the two never contend for the 1 MiB object
// cap (docs/src/gateways/llm/budgets-and-rate-limits.md#per-workload-spend).
func AgentSpendConfigMapName(providerName string) string { return "kaalm-agentspend-" + providerName }

// UnattributedWorkload is the visible spend bucket for gateway-only-tier
// callers, which authenticate by token and carry a namespace but no workload
// identity. Keeping the bucket visible is what lets per-workload rows sum to
// the namespace total.
const UnattributedWorkload = "(unattributed)"

// CanonicalKey is the reconciler-owned roll-up key in the budget ConfigMap.
const CanonicalKey = "_canonical"

// RetiredKey is the reconciler-owned, period-tagged accumulator that keeps a
// pruned replica's published current-period spend alive: without it a rolling
// restart would erase every replaced replica's spend from every view, which
// voids a hard ceiling once per rollout. Replicas fold it like a peer partial.
const RetiredKey = "_retired"

// budgetPartial is the JSON value under each per-replica key. The period tag
// lets the reducer drop stale entries during rollover.
type budgetPartial struct {
	Period string `json:"period"`
	// Spend is per-namespace USD as decimal strings, flattened into the same
	// JSON object as period on the wire.
	Spend map[string]string `json:"-"`
	// MarginExceeded reports that this replica's observed traffic required a
	// wider boundary margin than configured (hard enforcement). On the wire
	// it is the underscore-prefixed, non-numeric "_marginExceeded" field, so
	// an older parser cannot mistake it for namespace spend.
	MarginExceeded bool `json:"-"`
}

// marginExceededField is the flag field inside a partial. Underscore-prefixed
// fields are flags, never spend; values are never bare numbers.
const marginExceededField = "_marginExceeded"

// MarshalJSON flattens period and the namespace map into one object, matching
// the documented ConfigMap layout.
func (p budgetPartial) MarshalJSON() ([]byte, error) {
	out := map[string]string{"period": p.Period}
	for ns, v := range p.Spend {
		out[ns] = v
	}
	if p.MarginExceeded {
		out[marginExceededField] = "true"
	}
	return json.Marshal(out)
}

// ParseBudgetPartial decodes a per-replica ConfigMap value. Underscore-
// prefixed fields are flags, not spend; the only defined one is returned as
// marginExceeded.
func ParseBudgetPartial(raw string) (period string, spend map[string]float64, marginExceeded bool, err error) {
	var flat map[string]string
	if err := json.Unmarshal([]byte(raw), &flat); err != nil {
		return "", nil, false, err
	}
	spend = map[string]float64{}
	for k, v := range flat {
		if k == "period" {
			period = v
			continue
		}
		if strings.HasPrefix(k, "_") {
			if k == marginExceededField && v == "true" {
				marginExceeded = true
			}
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		spend[k] = f
	}
	return period, spend, marginExceeded, nil
}

// RetiredPartial builds the reconciler-owned _retired accumulator value in
// the same wire shape as a replica partial.
func RetiredPartial(period string, spend map[string]float64) any {
	out := map[string]string{}
	for ns, v := range spend {
		out[ns] = strconv.FormatFloat(v, 'f', 2, 64)
	}
	return budgetPartial{Period: period, Spend: out}
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
	// replicas returns the live gateway replica count for the effective
	// margin; nil means one replica.
	replicas func() int
	// kick carries provider names whose settles want an immediate partial
	// publish; the publisher selects on it. Sends never block.
	kick chan string
	// stalenessWindow bounds both fail-closed signals. Derived, not a knob:
	// three publish intervals.
	stalenessWindow time.Duration

	mu        sync.Mutex
	providers map[string]*providerLedger
}

// rateBucketWidth is the observed-spend-rate bucket, matching the publish
// interval so single-call spikes are covered by maxCostPerCall instead.
const rateBucketWidth = 10 * time.Second

// clusterSlotKey is the admission-slot key for the provider-wide (cluster
// ceiling) slot. Namespaces cannot be empty, so it cannot collide.
const clusterSlotKey = ""

type providerLedger struct {
	period string
	own    map[string]float64
	peers  map[string]float64
	// ownW / peersW are the per-workload spend view, keyed
	// "{namespace}/{workload}" where workload is agent/{name}, task/{name},
	// or the unattributed bucket ("/" cannot appear in a namespace name, so
	// the split is unambiguous). Purely additive beside the enforcement
	// maps: admission math never reads them.
	ownW   map[string]float64
	peersW map[string]float64

	// Hard-enforcement state, all guarded by the ledger mutex
	// (docs/src/gateways/llm/budgets-and-rate-limits.md#hard-enforcement).
	// adm maps a governed-ceiling key (a namespace, or clusterSlotKey) to
	// the settle token of the holder; an absent key is a free slot.
	adm       map[string]uint64
	nextToken uint64
	// Observed-traffic tracker: running max settled cost of one call, and
	// the peak spend rate over fixed buckets, monotone within the period.
	maxCostPerCall  float64
	rateBucketStart time.Time
	rateBucketSum   float64
	peakRatePerSec  float64
	// dirtySince is the oldest settle not yet published (zero = clean);
	// lastFoldOK is the last successful peer-view refresh. Both feed the
	// fail-closed check.
	dirtySince   time.Time
	lastFoldOK   time.Time
	marginRaised bool
}

// NewBudgetLedger builds an empty ledger.
func NewBudgetLedger() *BudgetLedger {
	return &BudgetLedger{
		now:             time.Now,
		kick:            make(chan string, 64),
		stalenessWindow: 3 * defaultPublishInterval,
		providers:       map[string]*providerLedger{},
	}
}

// ledgerFor returns the provider's ledger, rolling the period over (and
// resetting counters) when the clock has crossed a boundary.
func (b *BudgetLedger) ledgerFor(providerName, scheme string) *providerLedger {
	period := PeriodKey(scheme, b.now())
	l, ok := b.providers[providerName]
	if !ok {
		l = &providerLedger{
			period: period,
			own:    map[string]float64{}, peers: map[string]float64{},
			ownW: map[string]float64{}, peersW: map[string]float64{},
			adm: map[string]uint64{},
		}
		b.providers[providerName] = l
	}
	if l.period != period {
		l.period = period
		l.own = map[string]float64{}
		l.peers = map[string]float64{}
		l.ownW = map[string]float64{}
		l.peersW = map[string]float64{}
		// A held slot vanishes with the period: the next admit computes
		// near-zero utilization, outside any boundary, so no invariant is at
		// risk; in-flight settles land in the new period (like any midnight-
		// spanning call) and skip slot mutation via their token mismatch.
		l.adm = map[string]uint64{}
		l.maxCostPerCall = 0
		l.rateBucketStart = time.Time{}
		l.rateBucketSum = 0
		l.peakRatePerSec = 0
		l.marginRaised = false
	}
	return l
}

// Add records spend for a namespace and its attested workload after a call
// completes.
func (b *BudgetLedger) Add(provider *kaalmv1alpha1.ModelProvider, namespace, workload string, costUSD float64) {
	scheme := provider.Spec.Budget.Period
	if PeriodKey(scheme, b.now()) == "" || costUSD == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerFor(provider.Name, scheme)
	l.own[namespace] += costUSD
	l.ownW[namespace+"/"+workload] += costUSD
	b.trackLocked(l, costUSD)
	if l.dirtySince.IsZero() {
		l.dirtySince = b.now()
	}
}

// FoldPeers replaces the peer view for a provider from freshly read
// current-period partials (own key excluded by the caller).
func (b *BudgetLedger) FoldPeers(provider *kaalmv1alpha1.ModelProvider, peers map[string]float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerFor(provider.Name, provider.Spec.Budget.Period)
	l.peers = peers
	l.lastFoldOK = b.now()
}

// InitCanonical seeds the peer view from the reconciler's _canonical roll-up
// at startup (read exactly once per provider per replica lifetime).
func (b *BudgetLedger) InitCanonical(provider *kaalmv1alpha1.ModelProvider, canonical map[string]float64) {
	b.FoldPeers(provider, canonical)
}

// FoldWorkloadPeers replaces the per-workload peer view for a provider from
// freshly read current-period workload partials (own key excluded by the
// caller). It deliberately does not touch lastFoldOK: workload spend is a
// visibility surface, never an enforcement input.
func (b *BudgetLedger) FoldWorkloadPeers(provider *kaalmv1alpha1.ModelProvider, peers map[string]float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerFor(provider.Name, provider.Spec.Budget.Period)
	l.peersW = peers
}

// OwnWorkloadPartial snapshots this replica's per-workload counters for
// publishing, in the same wire shape as the budget partial.
func (b *BudgetLedger) OwnWorkloadPartial(providerName string) (period string, spend map[string]string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, exists := b.providers[providerName]
	if !exists || len(l.ownW) == 0 {
		return "", nil, false
	}
	out := map[string]string{}
	for k, v := range l.ownW {
		out[k] = strconv.FormatFloat(v, 'f', 2, 64)
	}
	return l.period, out, true
}

// WorkloadProviderSpend is one provider's per-workload view for a namespace:
// the folded union of this replica's live counters and every peer's latest
// partial, so any single replica can serve the read.
type WorkloadProviderSpend struct {
	Period    string            `json:"period"`
	Workloads map[string]string `json:"workloads"`
}

// WorkloadSpend returns every provider's per-workload spend rows for one
// namespace, USD as decimal strings.
func (b *BudgetLedger) WorkloadSpend(namespace string) map[string]WorkloadProviderSpend {
	b.mu.Lock()
	defer b.mu.Unlock()
	prefix := namespace + "/"
	out := map[string]WorkloadProviderSpend{}
	for providerName, l := range b.providers {
		sums := map[string]float64{}
		for key, v := range l.ownW {
			if workload, ok := strings.CutPrefix(key, prefix); ok {
				sums[workload] += v
			}
		}
		for key, v := range l.peersW {
			if workload, ok := strings.CutPrefix(key, prefix); ok {
				sums[workload] += v
			}
		}
		if len(sums) == 0 {
			continue
		}
		rows := map[string]string{}
		for workload, v := range sums {
			rows[workload] = strconv.FormatFloat(v, 'f', 2, 64)
		}
		out[providerName] = WorkloadProviderSpend{Period: l.period, Workloads: rows}
	}
	return out
}

// OwnPartial snapshots this replica's counters for publishing.
func (b *BudgetLedger) OwnPartial(providerName string) (period string, spend map[string]string, marginRaised, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, exists := b.providers[providerName]
	if !exists || len(l.own) == 0 {
		return "", nil, false, false
	}
	out := map[string]string{}
	for ns, v := range l.own {
		out[ns] = strconv.FormatFloat(v, 'f', 2, 64)
	}
	return l.period, out, l.marginRaised, true
}

// MarkPublished records a successful partial publish whose ledger snapshot
// was taken at snapshot. It clears the write-path staleness signal unless a
// newer settle landed after the snapshot.
func (b *BudgetLedger) MarkPublished(providerName string, snapshot time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, exists := b.providers[providerName]
	if !exists {
		return
	}
	if !l.dirtySince.IsZero() && !l.dirtySince.After(snapshot) {
		l.dirtySince = time.Time{}
	}
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
	// Ceiling attributes the utilization to "namespace" or "cluster"
	// (whichever ratio is worse) so a block names what fired.
	Ceiling string
	// Hard-enforcement outcomes (mutually exclusive with an admitted
	// request; see Admit): Throttled means the boundary admission slot is
	// held; Unavailable means the replica cannot verify budget state inside
	// the boundary region and fails closed.
	Throttled   bool
	Unavailable bool
	// BoundaryEngaged marks an admitted request that holds a boundary slot;
	// MarginRaisedNow marks the transition that first widened the margin
	// beyond the configured floor (for the metric's rising edge).
	BoundaryEngaged bool
	MarginRaisedNow bool
}

// utilization is the per-ceiling view the enforcement decision reads.
type utilization struct {
	nsPct, clPct   float64 // percent of each ceiling used; 0 when unset
	nsUSD, clUSD   float64 // the ceilings themselves; 0 when unset
	worst          float64
	worstIsCluster bool
}

// utilizationLocked computes both ceiling ratios from own + peers spend.
func utilizationLocked(budget kaalmv1alpha1.ModelProviderBudget, l *providerLedger, namespace string) utilization {
	var u utilization
	if ceiling, err := strconv.ParseFloat(budget.PerNamespaceUSD, 64); err == nil && ceiling > 0 {
		u.nsUSD = ceiling
		u.nsPct = l.spent(namespace) / ceiling * 100
		u.worst = u.nsPct
	}
	if budget.ClusterUSD != nil {
		if ceiling, err := strconv.ParseFloat(*budget.ClusterUSD, 64); err == nil && ceiling > 0 {
			u.clUSD = ceiling
			u.clPct = l.clusterSpent() / ceiling * 100
			if u.clPct > u.worst {
				u.worst = u.clPct
				u.worstIsCluster = true
			}
		}
	}
	return u
}

// decide applies the policy ladder to a utilization: the highest-threshold
// policy at or below the worst ratio wins.
func (b *BudgetLedger) decide(budget kaalmv1alpha1.ModelProviderBudget, u utilization) budgetDecision {
	d := budgetDecision{Percent: int(u.worst), Ceiling: "namespace"}
	if u.worstIsCluster {
		d.Ceiling = "cluster"
	}
	var winner *kaalmv1alpha1.ModelProviderBudgetPolicy
	for i := range budget.Policies {
		p := &budget.Policies[i]
		if u.worst >= float64(p.AtPercent) && (winner == nil || p.AtPercent > winner.AtPercent) {
			winner = p
		}
	}
	if winner == nil {
		return d
	}
	d.Action = winner.Action
	if winner.Action == kaalmv1alpha1.BudgetActionDegrade && winner.DegradeTo != nil {
		d.DegradeTo = *winner.DegradeTo
	}
	if winner.Action == kaalmv1alpha1.BudgetActionBlock {
		d.RetryAfter = int(time.Until(nextPeriodStart(budget.Period, b.now())).Seconds()) + 1
	}
	return d
}

// Enforce evaluates the provider's budget policies for a namespace using
// last-known spend state (no pre-call estimation). The highest-threshold
// policy at or below the current utilization wins. Utilization is the worse
// of the per-namespace and cluster-wide ratios. Soft-mode semantics only;
// hard-mode callers go through Admit.
func (b *BudgetLedger) Enforce(provider *kaalmv1alpha1.ModelProvider, namespace string) budgetDecision {
	budget := provider.Spec.Budget
	if PeriodKey(budget.Period, b.now()) == "" || len(budget.Policies) == 0 {
		return budgetDecision{}
	}
	b.mu.Lock()
	u := utilizationLocked(budget, b.ledgerFor(provider.Name, budget.Period), namespace)
	b.mu.Unlock()
	return b.decide(budget, u)
}

// BudgetPublisher runs the replica side of the budget counter exchange: every
// interval it server-side-applies this replica's partials (field manager =
// Pod name, so simultaneous writes never conflict) and folds peers' current-
// period partials into the enforcement view. The read-on-tick fold has the
// same staleness bound as a watch-driven fold: at most one publish interval.
type BudgetPublisher struct {
	Client            kubernetes.Interface
	Ledger            *BudgetLedger
	OperatorNamespace string
	PodName           string
	Interval          time.Duration
	// Providers enumerates the ModelProviders to exchange for.
	Providers func(ctx context.Context) []*kaalmv1alpha1.ModelProvider
	// now is the clock, overridable in tests; nil means time.Now.
	now func() time.Time
}

// defaultPublishInterval is the partial-publish tick; the ledger's staleness
// window derives from it (three intervals).
const defaultPublishInterval = 10 * time.Second

func (p *BudgetPublisher) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Run loops until ctx is done: the tick publishes and folds every interval,
// and the ledger's kick channel triggers an immediate publish for a provider
// whose settle happened inside the boundary region.
func (p *BudgetPublisher) Run(ctx context.Context) {
	interval := p.Interval
	if interval == 0 {
		interval = defaultPublishInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tick(ctx)
		case name := <-p.Ledger.kick:
			p.publishByName(ctx, name)
		}
	}
}

// publishByName resolves one provider from the cached list and publishes its
// partial immediately (the settle-publish path).
func (p *BudgetPublisher) publishByName(ctx context.Context, name string) {
	for _, provider := range p.Providers(ctx) {
		if provider.Name == name {
			p.publish(ctx, provider)
			return
		}
	}
}

func (p *BudgetPublisher) tick(ctx context.Context) {
	for _, provider := range p.Providers(ctx) {
		if PeriodKey(provider.Spec.Budget.Period, p.clock()) == "" {
			continue
		}
		p.publish(ctx, provider)
		p.fold(ctx, provider)
	}
}

func (p *BudgetPublisher) publish(ctx context.Context, provider *kaalmv1alpha1.ModelProvider) {
	snapshot := p.clock()
	period, spend, marginRaised, ok := p.Ledger.OwnPartial(provider.Name)
	if !ok {
		return
	}
	raw, err := json.Marshal(budgetPartial{Period: period, Spend: spend, MarginExceeded: marginRaised})
	if err != nil {
		return
	}
	apply := applycorev1.ConfigMap(BudgetConfigMapName(provider.Name), p.OperatorNamespace).
		WithData(map[string]string{p.PodName: string(raw)})
	_, err = p.Client.CoreV1().ConfigMaps(p.OperatorNamespace).Apply(ctx, apply,
		metav1.ApplyOptions{FieldManager: p.PodName, Force: true})
	if err != nil {
		slog.Warn("budget partial publish failed", "provider", provider.Name, "error", err)
		return
	}
	p.Ledger.MarkPublished(provider.Name, snapshot)
	p.publishWorkloads(ctx, provider)
}

// publishWorkloads publishes this replica's per-workload partial into the
// provider's agent-spend ConfigMap: same SSA one-key-per-replica exchange,
// deliberately outside the budget ConfigMap so the enforcement fold never
// sees workload keys. Best-effort: workload spend is visibility, not
// enforcement, so a failed publish only logs.
func (p *BudgetPublisher) publishWorkloads(ctx context.Context, provider *kaalmv1alpha1.ModelProvider) {
	period, spend, ok := p.Ledger.OwnWorkloadPartial(provider.Name)
	if !ok {
		return
	}
	raw, err := json.Marshal(budgetPartial{Period: period, Spend: spend})
	if err != nil {
		return
	}
	apply := applycorev1.ConfigMap(AgentSpendConfigMapName(provider.Name), p.OperatorNamespace).
		WithData(map[string]string{p.PodName: string(raw)})
	if _, err := p.Client.CoreV1().ConfigMaps(p.OperatorNamespace).Apply(ctx, apply,
		metav1.ApplyOptions{FieldManager: p.PodName, Force: true}); err != nil {
		slog.Warn("workload spend partial publish failed", "provider", provider.Name, "error", err)
	}
}

func (p *BudgetPublisher) fold(ctx context.Context, provider *kaalmv1alpha1.ModelProvider) {
	cm, err := p.Client.CoreV1().ConfigMaps(p.OperatorNamespace).Get(ctx, BudgetConfigMapName(provider.Name), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// No ConfigMap means no peer has published yet: an empty,
			// healthy peer view, not a read failure. Folding it keeps the
			// read-path staleness signal honest for a provider with no
			// spend anywhere.
			p.Ledger.FoldPeers(provider, map[string]float64{})
			return
		}
		slog.Warn("budget fold read failed", "provider", provider.Name, "error", err)
		return
	}
	p.Ledger.FoldPeers(provider, FoldPartials(cm.Data, p.PodName, PeriodKey(provider.Spec.Budget.Period, p.clock())))
	p.foldWorkloads(ctx, provider)
}

// foldWorkloads refreshes the per-workload peer view from the agent-spend
// ConfigMap, so any single replica serves the folded union on GET /v1/spend.
func (p *BudgetPublisher) foldWorkloads(ctx context.Context, provider *kaalmv1alpha1.ModelProvider) {
	cm, err := p.Client.CoreV1().ConfigMaps(p.OperatorNamespace).Get(ctx, AgentSpendConfigMapName(provider.Name), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			p.Ledger.FoldWorkloadPeers(provider, map[string]float64{})
		}
		return
	}
	p.Ledger.FoldWorkloadPeers(provider, FoldPartials(cm.Data, p.PodName, PeriodKey(provider.Spec.Budget.Period, p.clock())))
}

// FoldPartials sums every current-period partial in a budget ConfigMap except
// the caller's own key: peers plus the reconciler's _retired accumulator.
// _canonical is never on the enforcement path.
func FoldPartials(data map[string]string, ownPodName, currentPeriod string) map[string]float64 {
	peers := map[string]float64{}
	for key, raw := range data {
		if key == ownPodName || key == CanonicalKey {
			continue
		}
		period, spend, _, err := ParseBudgetPartial(raw)
		if err != nil || period != currentPeriod {
			continue
		}
		for ns, v := range spend {
			peers[ns] += v
		}
	}
	return peers
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
	p.seedWorkloadsFromCanonical(ctx)
}

// seedWorkloadsFromCanonical initializes the per-workload peer view from each
// provider's agent-spend _canonical key at startup, so a restarted replica
// serves the full current-period breakdown immediately.
func (p *BudgetPublisher) seedWorkloadsFromCanonical(ctx context.Context) {
	for _, provider := range p.Providers(ctx) {
		cm, err := p.Client.CoreV1().ConfigMaps(p.OperatorNamespace).Get(ctx, AgentSpendConfigMapName(provider.Name), metav1.GetOptions{})
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
		for k, v := range flat {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				canonical[k] = f
			}
		}
		p.Ledger.FoldWorkloadPeers(provider, canonical)
	}
}
