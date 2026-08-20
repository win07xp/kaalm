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
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
	"github.com/win07xp/kaalm/internal/gateway"
)

// reconcileAgentSpend is the reducer over the per-replica workload partials
// in the kaalm-agentspend-{provider} ConfigMap: prune keys with no live
// gateway Pod (folding their current-period spend into _retired first, so a
// rollout does not erase the breakdown), drop stale-period entries, and
// write _canonical for fresh-replica seeding. Deliberately narrower than the
// budget reducer: workload spend keeps the current period only, writes no
// provider status (the CR would grow with namespaces times workloads), and
// feeds nothing on the enforcement path.
func (r *ModelProviderReconciler) reconcileAgentSpend(
	ctx context.Context, mp *kaalmv1alpha1.ModelProvider, liveGateways map[string]bool,
) error {
	currentPeriod := gateway.PeriodKey(mp.Spec.Budget.Period, time.Now())
	if currentPeriod == "" {
		return nil
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: gateway.AgentSpendConfigMapName(mp.Name)}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // no replica has written yet
		}
		return err
	}

	current := map[string]float64{}
	retired := map[string]float64{}
	changed, retiredChanged := false, false
	sum := func(dst, src map[string]float64) {
		for k, v := range src {
			dst[k] += v
		}
	}
	for k, raw := range cm.Data {
		if k == gateway.CanonicalKey {
			continue
		}
		period, spend, _, err := gateway.ParseBudgetPartial(raw)
		switch {
		case k == gateway.RetiredKey:
			if err == nil && period == currentPeriod {
				sum(retired, spend)
			} else {
				// Stale or unreadable: workload history ends at the period
				// boundary by design (the namespace figures keep one
				// archived generation; the breakdown does not).
				delete(cm.Data, k)
				changed = true
			}
		case !liveGateways[k]:
			if err == nil && period == currentPeriod {
				sum(retired, spend)
				retiredChanged = true
			}
			delete(cm.Data, k)
			changed = true
		case err != nil:
		case period == currentPeriod:
			sum(current, spend)
		default:
			delete(cm.Data, k)
			changed = true
		}
	}

	if retiredChanged {
		rawRetired, err := json.Marshal(gateway.RetiredPartial(currentPeriod, retired))
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[gateway.RetiredKey] = string(rawRetired)
		changed = true
	}

	sum(current, retired)
	canonical := map[string]string{}
	for k, v := range current {
		canonical[k] = strconv.FormatFloat(v, 'f', 2, 64)
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
		return r.Update(ctx, &cm)
	}
	return nil
}
