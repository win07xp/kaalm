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
	"sync"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// This file is the hard-enforcement half of the budget ledger:
// docs/src/gateways/llm/budgets-and-rate-limits.md#hard-enforcement. Inside
// the boundary region below a block threshold, admission serializes to one
// in-flight request per replica per governed ceiling, settles publish
// immediately, and a replica that cannot verify budget state fails closed.
// Overshoot is bounded by count, never by cost estimates.

// hardActive reports whether hard enforcement governs this provider's block
// policies. Rule 32 guarantees a block policy exists when enforcement is
// hard; the check here is defense in depth.
func hardActive(provider *kaalmv1alpha1.ModelProvider) bool {
	if provider.Spec.Budget.Enforcement != kaalmv1alpha1.BudgetEnforcementHard {
		return false
	}
	for _, p := range provider.Spec.Budget.Policies {
		if p.Action == kaalmv1alpha1.BudgetActionBlock {
			return true
		}
	}
	return false
}

// minBlockAt returns the lowest block policy threshold: the ceiling whose
// boundary region hard mode defends first.
func minBlockAt(budget kaalmv1alpha1.ModelProviderBudget) float64 {
	at := float64(101) // no block policy: unreachable boundary
	for _, p := range budget.Policies {
		if p.Action == kaalmv1alpha1.BudgetActionBlock && float64(p.AtPercent) < at {
			at = float64(p.AtPercent)
		}
	}
	return at
}

// configuredMarginPercent is the rule 34 floor: hard.boundaryMarginPercent,
// defaulting to 5 when the hard block is absent.
func configuredMarginPercent(budget kaalmv1alpha1.ModelProviderBudget) float64 {
	if budget.Hard != nil {
		return float64(budget.Hard.BoundaryMarginPercent)
	}
	return 5
}

func (b *BudgetLedger) replicasCount() int {
	if b.replicas == nil {
		return 1
	}
	if r := b.replicas(); r > 1 {
		return r
	}
	return 1
}

// trackLocked feeds the observed-traffic tracker on every settled cost:
// the running max single-call cost, and the peak spend rate over fixed
// buckets, monotone within the period. Caller holds b.mu.
func (b *BudgetLedger) trackLocked(l *providerLedger, cost float64) {
	if cost <= 0 {
		return
	}
	now := b.now()
	if cost > l.maxCostPerCall {
		l.maxCostPerCall = cost
	}
	if l.rateBucketStart.IsZero() || now.Sub(l.rateBucketStart) >= rateBucketWidth {
		if !l.rateBucketStart.IsZero() {
			if r := l.rateBucketSum / rateBucketWidth.Seconds(); r > l.peakRatePerSec {
				l.peakRatePerSec = r
			}
		}
		l.rateBucketStart = now
		l.rateBucketSum = 0
	}
	l.rateBucketSum += cost
}

// effectiveMarginPctLocked widens the configured margin from observed
// traffic: one unsettled in-flight request per replica, plus each peer's
// settled-but-unpropagated spend over one staleness window. The configured
// knob is a floor, so an undersized knob cannot void the guarantee. Caller
// holds b.mu.
func (b *BudgetLedger) effectiveMarginPctLocked(l *providerLedger, ceilingUSD, configured float64) float64 {
	if ceilingUSD <= 0 {
		return configured
	}
	reps := float64(b.replicasCount())
	marginUSD := reps*l.maxCostPerCall + (reps-1)*l.peakRatePerSec*b.stalenessWindow.Seconds()
	if pct := 100 * marginUSD / ceilingUSD; pct > configured {
		return pct
	}
	return configured
}

// Admit is the enforcement entry point for both modes. For soft providers
// (and hard providers outside every boundary region) it returns exactly
// Enforce's decision with a nil settle. For a hard provider inside a
// boundary region it try-acquires the governed admission slots and returns
// a settle the caller MUST invoke exactly once with the request's actual
// cost (zero if nothing was spent); settle is idempotent and rollover-safe.
func (b *BudgetLedger) Admit(provider *kaalmv1alpha1.ModelProvider, namespace, workload string) (budgetDecision, func(costUSD float64)) {
	budget := provider.Spec.Budget
	scheme := budget.Period
	if PeriodKey(scheme, b.now()) == "" || len(budget.Policies) == 0 {
		return budgetDecision{}, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerFor(provider.Name, scheme)
	u := utilizationLocked(budget, l, namespace)
	d := b.decide(budget, u)
	if !hardActive(provider) || d.Action == kaalmv1alpha1.BudgetActionBlock {
		return d, nil
	}

	// Sticky slot: a held slot throttles even when a fold (a peer prune
	// during a rollout, say) has dropped utilization back below the
	// boundary. The slot releases on settle, never on recomputation.
	if _, held := l.adm[namespace]; held {
		d.Throttled = true
		return d, nil
	}
	if _, held := l.adm[clusterSlotKey]; held && u.clUSD > 0 {
		d.Throttled = true
		return d, nil
	}

	blockAt := minBlockAt(budget)
	configured := configuredMarginPercent(budget)
	effNS := b.effectiveMarginPctLocked(l, u.nsUSD, configured)
	effCL := b.effectiveMarginPctLocked(l, u.clUSD, configured)
	inBoundaryNS := u.nsUSD > 0 && u.nsPct >= blockAt-effNS
	inBoundaryCL := u.clUSD > 0 && u.clPct >= blockAt-effCL
	if !inBoundaryNS && !inBoundaryCL {
		return d, nil
	}

	// Fail closed, but only where the guarantee is live: unpublishable
	// settles (write path) or a stale peer view (read path) mean this
	// replica cannot verify budget state and must not spend blind.
	now := b.now()
	if (!l.dirtySince.IsZero() && now.Sub(l.dirtySince) > b.stalenessWindow) ||
		l.lastFoldOK.IsZero() || now.Sub(l.lastFoldOK) > b.stalenessWindow {
		d.Unavailable = true
		return d, nil
	}

	if (inBoundaryNS && effNS > configured) || (inBoundaryCL && effCL > configured) {
		if !l.marginRaised {
			l.marginRaised = true
			d.MarginRaisedNow = true
		}
	}

	l.nextToken++
	token := l.nextToken
	var keys []string
	if inBoundaryNS {
		l.adm[namespace] = token
		keys = append(keys, namespace)
	}
	if inBoundaryCL {
		l.adm[clusterSlotKey] = token
		keys = append(keys, clusterSlotKey)
	}
	d.BoundaryEngaged = true

	providerName := provider.Name
	capturedPeriod := l.period
	var once sync.Once
	settle := func(costUSD float64) {
		once.Do(func() {
			b.settle(providerName, scheme, capturedPeriod, namespace, workload, keys, token, costUSD)
		})
	}
	return d, settle
}

// settle lands an admitted request's actual cost and frees its slots in one
// critical section, so the next admit always sees the previous request's
// real spend. A settle that crosses a period rollover lands its cost in the
// current period (the same attribution a midnight-spanning call gets under
// soft mode) and skips slot mutation via the token mismatch.
func (b *BudgetLedger) settle(providerName, scheme, capturedPeriod, namespace, workload string, keys []string, token uint64, costUSD float64) {
	b.mu.Lock()
	l := b.ledgerFor(providerName, scheme)
	if l.period == capturedPeriod {
		for _, k := range keys {
			if l.adm[k] == token {
				delete(l.adm, k)
			}
		}
	}
	dirty := false
	if costUSD != 0 {
		l.own[namespace] += costUSD
		l.ownW[namespace+"/"+workload] += costUSD
		b.trackLocked(l, costUSD)
		if l.dirtySince.IsZero() {
			l.dirtySince = b.now()
		}
		dirty = true
	}
	b.mu.Unlock()

	if dirty {
		// Kick the publisher for an immediate settle-publish; a full kick
		// channel just means a publish is already imminent.
		select {
		case b.kick <- providerName:
		default:
		}
	}
}

// SetReplicas injects the live replica counter (nil-safe default of one).
func (b *BudgetLedger) SetReplicas(f func() int) { b.replicas = f }
