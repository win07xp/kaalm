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
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// gatewayPodLabels selects gateway Pods in the operator namespace, for the
// GatewayReachable condition and stale-replica pruning.
var gatewayPodLabels = map[string]string{"app.kubernetes.io/component": "gateway"}

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
// provider's status for kubectl-describe visibility (reconciler step 4).
func (r *ModelProviderReconciler) setGatewayReachable(mp *kaalmv1alpha1.ModelProvider, ready int) {
	cond := metav1.Condition{Type: kaalmv1alpha1.ConditionGatewayReachable}
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

// reconcileBudget is the reducer over the per-replica partials in the
// kaalm-budget-{provider} ConfigMap: prune keys with no live gateway Pod,
// archive and drop stale-period entries, sum current-period partials, write
// _canonical, and populate status.budgetUsage. See
// docs/src/gateways/llm/budgets-and-rate-limits.md.
func (r *ModelProviderReconciler) reconcileBudget(
	ctx context.Context, mp *kaalmv1alpha1.ModelProvider, liveGateways map[string]bool,
) error {
	scheme := mp.Spec.Budget.Period
	currentPeriod := gateway.PeriodKey(scheme, time.Now())
	if currentPeriod == "" {
		return nil
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: gateway.BudgetConfigMapName(mp.Name)}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // no replica has written yet
		}
		return err
	}

	current := map[string]float64{}  // ns -> USD, current period
	previous := map[string]float64{} // ns -> USD, prior-period entries pending archive
	previousPeriod := ""
	changed := false
	for k, raw := range cm.Data {
		if k == gateway.CanonicalKey {
			continue
		}
		// Prune keys left behind by scaled-down or replaced replicas.
		if !liveGateways[k] {
			delete(cm.Data, k)
			changed = true
			continue
		}
		period, spend, err := gateway.ParseBudgetPartial(raw)
		if err != nil {
			continue
		}
		if period == currentPeriod {
			for ns, v := range spend {
				current[ns] += v
			}
			continue
		}
		// Rollover: archive the old-period totals and delete the stale key;
		// the live replica rewrites a new-period partial on its next publish.
		previousPeriod = period
		for ns, v := range spend {
			previous[ns] += v
		}
		delete(cm.Data, k)
		changed = true
	}

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

// budgetUsageEntries renders per-namespace spend into status entries with the
// enforcement state derived from the provider's policies.
func budgetUsageEntries(
	mp *kaalmv1alpha1.ModelProvider, spend map[string]float64, period string,
) []kaalmv1alpha1.ModelProviderBudgetUsage {
	ceiling, _ := strconv.ParseFloat(mp.Spec.Budget.PerNamespaceUSD, 64)
	namespaces := make([]string, 0, len(spend))
	for ns := range spend {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	out := make([]kaalmv1alpha1.ModelProviderBudgetUsage, 0, len(namespaces))
	for _, ns := range namespaces {
		entry := kaalmv1alpha1.ModelProviderBudgetUsage{
			Namespace: ns,
			Period:    period,
			SpentUSD:  strconv.FormatFloat(spend[ns], 'f', 2, 64),
			State:     kaalmv1alpha1.BudgetStateNormal,
		}
		if ceiling > 0 {
			percent := spend[ns] / ceiling * 100
			entry.PercentUsed = int32(percent)
			for _, p := range mp.Spec.Budget.Policies {
				if percent < float64(p.AtPercent) {
					continue
				}
				switch p.Action {
				case kaalmv1alpha1.BudgetActionBlock:
					entry.State = kaalmv1alpha1.BudgetStateBlocked
				case kaalmv1alpha1.BudgetActionDegrade:
					if entry.State != kaalmv1alpha1.BudgetStateBlocked {
						entry.State = kaalmv1alpha1.BudgetStateThrottled
					}
				}
			}
		}
		out = append(out, entry)
	}
	return out
}
