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
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func gatewayPod(ns string, labels map[string]string, ready bool) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns, Labels: labels}}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return p
}

// The Pod watch admits only gateway Pods in the operator namespace, and of
// their updates only those that change what GatewayReachable counts: the
// Ready condition or the start of a deletion.
func TestGatewayReadinessChanged(t *testing.T) {
	p := gatewayReadinessChanged(testOperatorNamespace)
	gwLabels := map[string]string{labelKeyComponent: componentGateway}
	other := map[string]string{labelKeyComponent: "agent"}

	if !p.Create(event.CreateEvent{Object: gatewayPod(testOperatorNamespace, gwLabels, false)}) {
		t.Error("a new gateway Pod must be admitted")
	}
	if p.Create(event.CreateEvent{Object: gatewayPod(testOperatorNamespace, other, true)}) {
		t.Error("a non-gateway Pod in the operator namespace must be filtered out")
	}
	if p.Create(event.CreateEvent{Object: gatewayPod("team-a", gwLabels, true)}) {
		t.Error("a gateway-labelled Pod outside the operator namespace must be filtered out")
	}
	if !p.Delete(event.DeleteEvent{Object: gatewayPod(testOperatorNamespace, gwLabels, true)}) {
		t.Error("a deleted gateway Pod must be admitted")
	}
	// The final delete of a draining Pod moves no readiness (it stopped
	// counting when its deletion started), but it runs the budget and
	// agent-spend folds after the Pod's last publish.
	gone := gatewayPod(testOperatorNamespace, gwLabels, false)
	goneAt := metav1.Now()
	gone.DeletionTimestamp = &goneAt
	if !p.Delete(event.DeleteEvent{Object: gone}) {
		t.Error("the final delete of a terminating, not-Ready gateway Pod must be admitted")
	}
	if p.Generic(event.GenericEvent{Object: gatewayPod(testOperatorNamespace, gwLabels, true)}) {
		t.Error("generic events carry no readiness change")
	}

	notReady := gatewayPod(testOperatorNamespace, gwLabels, false)
	ready := gatewayPod(testOperatorNamespace, gwLabels, true)
	if !p.Update(event.UpdateEvent{ObjectOld: notReady, ObjectNew: ready}) {
		t.Error("a gateway Pod becoming Ready must be admitted")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: notReady}) {
		t.Error("a gateway Pod losing Ready must be admitted")
	}
	if p.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: ready.DeepCopy()}) {
		t.Error("an update that keeps readiness must be filtered out")
	}
	deleting := ready.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	if !p.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: deleting}) {
		t.Error("a gateway Pod starting to terminate must be admitted")
	}
	otherReady := gatewayPod(testOperatorNamespace, other, true)
	if p.Update(event.UpdateEvent{ObjectOld: gatewayPod(testOperatorNamespace, other, false), ObjectNew: otherReady}) {
		t.Error("a readiness change on a non-gateway Pod must be filtered out")
	}
}

// A gateway Pod event re-enqueues every ModelProvider: GatewayReachable is
// a cluster-wide signal mirrored onto each one.
func TestAllModelProviders(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
		&kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
	).Build()
	r := &ModelProviderReconciler{Client: c, OperatorNamespace: testOperatorNamespace}
	reqs := r.allModelProviders(context.Background(), gatewayPod(testOperatorNamespace, nil, true))
	var names []string
	for _, req := range reqs {
		names = append(names, req.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("enqueued %v, want [a b]", names)
	}
}
