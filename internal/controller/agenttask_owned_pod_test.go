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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// TestOwnedTaskPod_ReadsThroughIndex: ownedTaskPod returns the Pod the
// AgentTask controls, prefers a live one over a terminating one, ignores
// Pods other owners control, and reads through the controller-owner index
// rather than every Pod in the namespace.
func TestOwnedTaskPod_ReadsThroughIndex(t *testing.T) {
	ctx := context.Background()
	task := &kaalmv1beta1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "team-a", UID: "u-t"}}
	live, terminating, foreign, nonController := ownedPodFixtures(task,
		kaalmv1beta1.GroupVersion.WithKind("AgentTask"), taskPodLabels(task))

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
			got, err := (&AgentTaskReconciler{Client: c}).ownedTaskPod(ctx, task)
			if err != nil {
				t.Fatalf("ownedTaskPod: %v", err)
			}
			gotName := ""
			if got != nil {
				gotName = got.Name
			}
			if gotName != tc.want {
				t.Errorf("ownedTaskPod = %q, want %q", gotName, tc.want)
			}
		})
	}

	// The lookup goes through the index: a cache without it cannot answer.
	unindexed := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(live).Build()
	if _, err := (&AgentTaskReconciler{Client: unindexed}).ownedTaskPod(ctx, task); err == nil ||
		!strings.Contains(err.Error(), IndexPodController) {
		t.Errorf("ownedTaskPod without the %s index returned err %v; want the indexed lookup", IndexPodController, err)
	}
}
