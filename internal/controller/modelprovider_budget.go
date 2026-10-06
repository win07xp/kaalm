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

package controller

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// gatewayPodLabels selects gateway Pods in the operator namespace, for the
// GatewayReachable condition and stale-replica pruning.
var gatewayPodLabels = map[string]string{labelKeyComponent: componentGateway}

// gatewayPods returns the live gateway Pod names and how many are Ready.
func (r *ModelProviderReconciler) gatewayPods(ctx context.Context) (names map[string]bool, ready int, err error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.OperatorNamespace),
		client.MatchingLabels(gatewayPodLabels)); err != nil {
		return nil, 0, err
	}
	names = map[string]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		names[p.Name] = true
		if podReady(p) {
			ready++
		}
	}
	return names, ready, nil
}

// setGatewayReachable mirrors the cluster-wide gateway readiness onto this
// provider's status for kubectl-describe visibility (GatewayReachable under
// What it reports in docs/src/controller/reconcilers/modelprovider.md).
func (r *ModelProviderReconciler) setGatewayReachable(mp *kaalmv1beta1.ModelProvider, ready int) {
	cond := metav1.Condition{Type: kaalmv1beta1.ConditionGatewayReachable}
	if ready >= 1 {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "GatewayReady"
		cond.Message = "at least one gateway Pod is Ready"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "GatewayUnavailable"
		cond.Message = "no Ready gateway Pods in " + r.OperatorNamespace
	}
	apimeta.SetStatusCondition(&mp.Status.Conditions, cond)
}

// budgetRequeue returns res, or a one-minute requeue when res schedules
// none and the provider tracks a budget period: a budget-tracked provider
// re-reconciles every minute so the spend roll-up and the period rollover
// stay fresh without ConfigMap events, including on a pass that fails a
// check.
func budgetRequeue(mp *kaalmv1beta1.ModelProvider, res ctrl.Result) ctrl.Result {
	if res.RequeueAfter != 0 || gateway.PeriodKey(mp.Spec.Budget.Period, time.Now()) == "" {
		return res
	}
	return ctrl.Result{RequeueAfter: time.Minute}
}

// clearBudgetStatus empties the budget status of a provider whose budget
// nothing maintains: budgetUsage, clusterSpentUSD, and the
// BoundaryMarginRaised condition. Without a period the gateway neither
// counts nor enforces a budget, and without the budget ConfigMap no
// replica's spend is visible, so the reducer has nothing to report. Leftover
// figures would claim a state that nothing enforces, and Agents read
// budgetUsage for BudgetExhausted. Nil, not an empty slice, so a steady pass
// compares equal to the stored status and writes nothing. The provider's
// kaalm_provider_budget_canonical_usd series go too, so dashboards stop
// showing the old spend.
func clearBudgetStatus(mp *kaalmv1beta1.ModelProvider) {
	mp.Status.BudgetUsage = nil
	mp.Status.ClusterSpentUSD = ""
	apimeta.RemoveStatusCondition(&mp.Status.Conditions, kaalmv1beta1.ConditionBoundaryMarginRaised)
	providerBudgetCanonical.DeletePartialMatch(prometheus.Labels{"provider": mp.Name})
}

// deleteSpendConfigMaps deletes the provider's budget and agent-spend
// ConfigMaps, which the gateway writes with no owner reference. NotFound is
// not an error: a provider with no spend has neither.
func (r *ModelProviderReconciler) deleteSpendConfigMaps(ctx context.Context, name string) error {
	for _, cmName := range []string{gateway.BudgetConfigMapName(name), gateway.AgentSpendConfigMapName(name)} {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: r.OperatorNamespace, Name: cmName}}
		if err := client.IgnoreNotFound(r.Delete(ctx, cm)); err != nil {
			return err
		}
	}
	return nil
}

// reconcileBudget is the reducer over the per-replica partials in the
// kaalm-budget-{provider} ConfigMap: delete keys written for another
// provider UID (a deleted provider of the same name), prune keys with no live
// gateway Pod, archive and drop stale-period entries, sum current-period
// partials, write _canonical, and populate status.budgetUsage. It clears the budget status
// when the provider tracks no budget or the ConfigMap is absent. See
// docs/src/gateways/llm/budgets-and-rate-limits.md.
func (r *ModelProviderReconciler) reconcileBudget(
	ctx context.Context, mp *kaalmv1beta1.ModelProvider, liveGateways map[string]bool,
) error {
	scheme := mp.Spec.Budget.Period
	currentPeriod := gateway.PeriodKey(scheme, time.Now())
	if currentPeriod == "" {
		clearBudgetStatus(mp)
		return nil
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: gateway.BudgetConfigMapName(mp.Name)}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			// No replica has published yet, or the ConfigMap was deleted;
			// either way the gateway's peer view is empty too.
			clearBudgetStatus(mp)
			return nil
		}
		return err
	}

	fold := foldBudgetKeys(&cm, liveGateways, currentPeriod, string(mp.UID))
	current, previous, previousPeriod := fold.current, fold.previous, fold.previousPeriod
	changed := fold.changed

	// current-period spend everyone must see = live partials + retired.
	for ns, v := range fold.retired {
		current[ns] += v
	}
	if fold.retiredChanged {
		rawRetired, err := json.Marshal(gateway.RetiredPartial(currentPeriod, fold.retired, string(mp.UID)))
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[gateway.RetiredKey] = string(rawRetired)
		changed = true
	}

	r.setBoundaryMargin(mp, fold.marginRaised)

	canonical := map[string]string{}
	for ns, v := range current {
		canonical[ns] = strconv.FormatFloat(v, 'f', 2, 64)
	}
	rawCanonical, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	if cm.Data[gateway.CanonicalKey] != string(rawCanonical) {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[gateway.CanonicalKey] = string(rawCanonical)
		changed = true
	}
	if changed {
		if err := r.Update(ctx, &cm); err != nil {
			return err
		}
	}

	// Status: current-period usage per namespace, plus archived prior-period
	// totals kept alongside (distinguished by their period tag).
	usage := budgetUsageEntries(mp, current, currentPeriod)
	if previousPeriod != "" {
		usage = append(usage, budgetUsageEntries(mp, previous, previousPeriod)...)
	}
	mp.Status.BudgetUsage = usage
	var clusterTotal float64
	for _, v := range current {
		clusterTotal += v
	}
	mp.Status.ClusterSpentUSD = strconv.FormatFloat(clusterTotal, 'f', 2, 64)
	for ns, v := range current {
		providerBudgetCanonical.WithLabelValues(mp.Name, ns, currentPeriod).Set(v)
	}
	return nil
}

// budgetFold is foldBudgetKeys' result: the reducer's view of one budget
// ConfigMap pass.
type budgetFold struct {
	current        map[string]float64 // ns -> USD, current period, live replicas
	retired        map[string]float64 // ns -> USD, current period, pruned replicas
	previous       map[string]float64 // ns -> USD, prior-period entries pending archive
	previousPeriod string
	changed        bool
	retiredChanged bool
	marginRaised   bool
}

// foldBudgetKeys walks every key in the budget ConfigMap once: summing live
// current-period partials (and their margin flags), folding pruned replicas'
// current-period totals into the retired view (deleting a key must not
// delete the spend it recorded; load-bearing under hard enforcement, where a
// rollout would otherwise erase every replaced replica's published spend),
// and archiving prior-period entries. A key tagged with another provider UID
// was written for a deleted provider of the same name; it is deleted before
// anything else reads it.
func foldBudgetKeys(cm *corev1.ConfigMap, liveGateways map[string]bool, currentPeriod, providerUID string) budgetFold {
	f := budgetFold{
		current:  map[string]float64{},
		retired:  map[string]float64{},
		previous: map[string]float64{},
	}
	sum := func(dst map[string]float64, src map[string]float64) {
		for ns, v := range src {
			dst[ns] += v
		}
	}
	for k, raw := range cm.Data {
		if k == gateway.CanonicalKey {
			continue
		}
		period, spend, margin, uid, err := gateway.ParseBudgetPartial(raw)
		if err == nil && uid != "" && uid != providerUID {
			// Written for a deleted provider of the same name: never this
			// provider's spend, current or archived.
			delete(cm.Data, k)
			f.changed = true
			continue
		}
		switch {
		case k == gateway.RetiredKey:
			// Reconciler-owned: carried while current, archived at rollover.
			if err != nil {
				continue
			}
			if period == currentPeriod {
				sum(f.retired, spend)
			} else {
				f.previousPeriod = period
				sum(f.previous, spend)
				delete(cm.Data, k)
				f.changed = true
			}
		case !liveGateways[k]:
			if err == nil && period == currentPeriod {
				sum(f.retired, spend)
				f.retiredChanged = true
			} else if err == nil && period != currentPeriod {
				f.previousPeriod = period
				sum(f.previous, spend)
			}
			delete(cm.Data, k)
			f.changed = true
		case err != nil:
		case period == currentPeriod:
			sum(f.current, spend)
			if margin {
				f.marginRaised = true
			}
		default:
			// Rollover: archive the old-period totals and delete the stale
			// key; the live replica rewrites a new-period partial on its
			// next publish.
			f.previousPeriod = period
			sum(f.previous, spend)
			delete(cm.Data, k)
			f.changed = true
		}
	}
	return f
}

// budgetUsageEntries renders per-namespace spend into status entries with the
// enforcement state derived from the provider's policies. Utilization and the
// winning policy come from the gateway's own rule: the worse of the
// per-namespace and cluster-wide ratios, then the highest threshold at or
// below it.
func budgetUsageEntries(
	mp *kaalmv1beta1.ModelProvider, spend map[string]float64, period string,
) []kaalmv1beta1.ModelProviderBudgetUsage {
	var clusterSpent float64
	namespaces := make([]string, 0, len(spend))
	for ns, v := range spend {
		namespaces = append(namespaces, ns)
		clusterSpent += v
	}
	sort.Strings(namespaces)

	out := make([]kaalmv1beta1.ModelProviderBudgetUsage, 0, len(namespaces))
	for _, ns := range namespaces {
		percent := gateway.BudgetUtilization(mp.Spec.Budget, spend[ns], clusterSpent)
		entry := kaalmv1beta1.ModelProviderBudgetUsage{
			Namespace:   ns,
			Period:      period,
			SpentUSD:    strconv.FormatFloat(spend[ns], 'f', 2, 64),
			PercentUsed: int32(percent),
			State:       kaalmv1beta1.BudgetStateNormal,
		}
		if p := gateway.BudgetPolicyAt(mp.Spec.Budget, percent); p != nil {
			switch p.Action {
			case kaalmv1beta1.BudgetActionBlock:
				entry.State = kaalmv1beta1.BudgetStateBlocked
			case kaalmv1beta1.BudgetActionDegrade:
				entry.State = kaalmv1beta1.BudgetStateThrottled
			}
		}
		out = append(out, entry)
	}
	return out
}
