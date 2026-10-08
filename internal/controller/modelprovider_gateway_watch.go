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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// isGatewayPod reports whether obj is a gateway Pod in the operator
// namespace, the set gatewayPods counts.
func isGatewayPod(obj client.Object, operatorNamespace string) bool {
	if obj == nil || obj.GetNamespace() != operatorNamespace {
		return false
	}
	for k, v := range gatewayPodLabels {
		if obj.GetLabels()[k] != v {
			return false
		}
	}
	return true
}

// countsAsReadyGateway reports whether GatewayReachable counts the Pod:
// Ready and not terminating, the test gatewayPods applies to its Ready count.
func countsAsReadyGateway(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	return ok && pod.DeletionTimestamp.IsZero() && podReady(pod)
}

// gatewayReadinessChanged admits the gateway Pod events that can move
// GatewayReachable: a gateway Pod appearing or going away, and an update
// that flips its Ready condition or starts its deletion. It also admits
// every gateway Pod delete, even of a Pod that already stopped counting as
// Ready: gatewayPods keeps a deleting Pod live until its object is gone, so
// that final delete is what runs the budget and agent-spend folds right
// after the stopped replica's last publish. The controller's
// Pod informer is cluster-wide (the Agent and AgentTask reconcilers own
// their Pods), so this watch adds no informer; the predicate keeps every
// other Pod event out of the ModelProvider queue.
func gatewayReadinessChanged(operatorNamespace string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isGatewayPod(e.Object, operatorNamespace)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isGatewayPod(e.Object, operatorNamespace)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !isGatewayPod(e.ObjectNew, operatorNamespace) {
				return false
			}
			return countsAsReadyGateway(e.ObjectOld) != countsAsReadyGateway(e.ObjectNew)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// allModelProviders re-enqueues every ModelProvider: GatewayReachable is a
// cluster-wide signal mirrored onto each one, so a gateway readiness change
// re-evaluates all of them.
func (r *ModelProviderReconciler) allModelProviders(ctx context.Context, _ client.Object) []reconcile.Request {
	var list kaalmv1beta1.ModelProviderList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
	}
	return reqs
}
