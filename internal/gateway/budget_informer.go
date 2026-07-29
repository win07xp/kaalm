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
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// budgetConfigMapPrefix names the per-provider budget exchange ConfigMaps.
const budgetConfigMapPrefix = "kaalm-budget-"

// FoldBudgetConfigMapEvent is the watch-driven half of the budget fold
// (docs/src/gateways/llm/budgets-and-rate-limits.md#cross-replica-enforcement-view):
// every add or update of a kaalm-budget-* ConfigMap refolds that provider's
// peer view, so peer spend lands in the enforcement view one watch
// propagation after it is published instead of one tick later. The exchange
// tick remains the backstop and the read-path liveness probe. Own-key SSA
// writes refold an identical peer map; that is idempotent by construction,
// so self-write events need no suppression.
func FoldBudgetConfigMapEvent(ctx context.Context, obj any, podName string, store Store, ledger *BudgetLedger) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	providerName, ok := strings.CutPrefix(cm.Name, budgetConfigMapPrefix)
	if !ok || providerName == "" {
		return
	}
	provider, ok := store.ProviderByName(ctx, providerName)
	if !ok {
		return
	}
	currentPeriod := PeriodKey(provider.Spec.Budget.Period, ledger.now())
	if currentPeriod == "" {
		return
	}
	ledger.FoldPeers(provider, FoldPartials(cm.Data, podName, currentPeriod))
}
