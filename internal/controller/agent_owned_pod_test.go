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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// ownedPodFixtures returns, for an owner, (a) a Pod it controls, (b) a
// terminating Pod it controls, (c) a Pod with the same labels that another
// owner of the same kind and name controls, and (d) a Pod that names the
// owner in a non-controller ownerRef.
func ownedPodFixtures(
	owner metav1.Object, gvk schema.GroupVersionKind, labels map[string]string,
) (live, terminating, foreign, nonController *corev1.Pod) {
	pod := func(name string, ref metav1.OwnerReference) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: owner.GetNamespace(), Labels: labels,
			OwnerReferences: []metav1.OwnerReference{ref},
		}}
	}
	live = pod("live", *metav1.NewControllerRef(owner, gvk))

	terminating = pod("terminating", *metav1.NewControllerRef(owner, gvk))
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test.kaalm.io/hold"}

	other := *metav1.NewControllerRef(owner, gvk)
	other.UID = types.UID("u-other")
	foreign = pod("foreign", other)

	nc := *metav1.NewControllerRef(owner, gvk)
	nc.Controller = nil
	nonController = pod("non-controller", nc)
	return live, terminating, foreign, nonController
}

// TestOwnedPod_ReadsThroughIndex: ownedPod returns the Pod the Agent
// controls, prefers a live one over a terminating one, ignores Pods other
// owners control, and reads through the controller-owner index rather than
// every Pod in the namespace.
func TestOwnedPod_ReadsThroughIndex(t *testing.T) {
	ctx := context.Background()
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "team-a", UID: "u-a"}}
	live, terminating, foreign, nonController := ownedPodFixtures(agent,
		kaalmv1beta1.GroupVersion.WithKind("Agent"), agentPodLabels(agent))

	cases := []struct {
		name string
		objs []client.Object
		want string
	}{
		{"live preferred", []client.Object{terminating, live, foreign, nonController}, "live"},
		{"terminating when alone", []client.Object{terminating, foreign, nonController}, "terminating"},
		{"none owned", []client.Object{foreign, nonController}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withPodControllerIndex(fake.NewClientBuilder().WithScheme(testScheme(t))).
				WithObjects(tc.objs...).Build()
			got, err := (&AgentReconciler{Client: c}).ownedPod(ctx, agent)
			if err != nil {
				t.Fatalf("ownedPod: %v", err)
			}
			gotName := ""
			if got != nil {
				gotName = got.Name
			}
			if gotName != tc.want {
				t.Errorf("ownedPod = %q, want %q", gotName, tc.want)
			}
		})
	}

	// The lookup goes through the index: a cache without it cannot answer.
	unindexed := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(live).Build()
	if _, err := (&AgentReconciler{Client: unindexed}).ownedPod(ctx, agent); err == nil ||
		!strings.Contains(err.Error(), IndexPodController) {
		t.Errorf("ownedPod without the %s index returned err %v; want the indexed lookup", IndexPodController, err)
	}
}
