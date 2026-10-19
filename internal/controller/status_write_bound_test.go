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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// boundCheckCondition is a condition no reconciler sets, so a test can write
// it without a reconciler racing to change it.
const boundCheckCondition = "BoundCheck"

// conditionsPtr returns a pointer to the status conditions of one of the six
// Kaalm kinds that have them.
func conditionsPtr(t *testing.T, obj client.Object) *[]metav1.Condition {
	t.Helper()
	switch o := obj.(type) {
	case *kaalmv1beta1.Agent:
		return &o.Status.Conditions
	case *kaalmv1beta1.AgentTask:
		return &o.Status.Conditions
	case *kaalmv1beta1.AgentChannel:
		return &o.Status.Conditions
	case *kaalmv1beta1.AgentClass:
		return &o.Status.Conditions
	case *kaalmv1beta1.ModelProvider:
		return &o.Status.Conditions
	case *kaalmv1beta1.ToolProvider:
		return &o.Status.Conditions
	}
	t.Fatalf("no conditions on %T", obj)
	return nil
}

// The manager client cuts a condition message past the schema's limit on
// every status write, so the write succeeds. Every reconciler and
// holdDeletion hold mgr.GetClient(), the same client as testClient, so this
// write takes the same path as each reconciler's status writes.
func TestManagerClient_CutsOversizedConditionMessage(t *testing.T) {
	long := strings.Repeat("x", 40000)
	cases := []struct {
		kind   string
		create func(t *testing.T)
		obj    client.Object
		key    types.NamespacedName
	}{
		{
			kind:   "Agent",
			create: func(t *testing.T) { mkWorkloadAgent(t, "bound-agent", "bound-class", nil) },
			obj:    &kaalmv1beta1.Agent{},
			key:    types.NamespacedName{Namespace: "default", Name: "bound-agent"},
		},
		{
			kind:   "AgentTask",
			create: func(t *testing.T) { mkTask(t, "bound-task", "bound-class", nil) },
			obj:    &kaalmv1beta1.AgentTask{},
			key:    types.NamespacedName{Namespace: "default", Name: "bound-task"},
		},
		{
			kind:   "AgentChannel",
			create: func(t *testing.T) { mkChannel(t, "bound-channel", "bound-agent", "/channels/default/bound", nil) },
			obj:    &kaalmv1beta1.AgentChannel{},
			key:    types.NamespacedName{Namespace: "default", Name: "bound-channel"},
		},
		{
			kind:   "AgentClass",
			create: func(t *testing.T) { mkWorkloadClass(t, "bound-class", nil) },
			obj:    &kaalmv1beta1.AgentClass{},
			key:    types.NamespacedName{Name: "bound-class"},
		},
		{
			kind:   "ModelProvider",
			create: func(t *testing.T) { mkProvider(t, "bound-mp", nil) },
			obj:    &kaalmv1beta1.ModelProvider{},
			key:    types.NamespacedName{Name: "bound-mp"},
		},
		{
			kind:   "ToolProvider",
			create: func(t *testing.T) { mkToolProvider(t, "bound-tp", nil) },
			obj:    &kaalmv1beta1.ToolProvider{},
			key:    types.NamespacedName{Name: "bound-tp"},
		},
	}
	// The class comes first: the Agent and AgentTask name it.
	for _, i := range []int{3, 0, 1, 2, 4, 5} {
		cases[i].create(t)
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			eventually(t, func() error {
				obj := tc.obj.DeepCopyObject().(client.Object)
				if err := testAPIReader.Get(ctxT(), tc.key, obj); err != nil {
					return err
				}
				apimeta.SetStatusCondition(conditionsPtr(t, obj), metav1.Condition{
					Type: boundCheckCondition, Status: metav1.ConditionTrue,
					Reason: boundCheckCondition, Message: long,
				})
				err := testClient.Status().Update(ctxT(), obj)
				if apierrors.IsConflict(err) {
					return err
				}
				if err != nil {
					t.Fatalf("status update: %v", err)
				}
				if c := apimeta.FindStatusCondition(*conditionsPtr(t, obj), boundCheckCondition); c == nil ||
					c.Message == long {
					t.Fatalf("the caller's object keeps the uncut message")
				}
				return nil
			})
			got := tc.obj.DeepCopyObject().(client.Object)
			if err := testAPIReader.Get(ctxT(), tc.key, got); err != nil {
				t.Fatalf("get: %v", err)
			}
			c := apimeta.FindStatusCondition(*conditionsPtr(t, got), boundCheckCondition)
			if c == nil {
				t.Fatalf("no %s condition stored", boundCheckCondition)
			}
			if len(c.Message) > conditionMessageLimit || !strings.HasSuffix(c.Message, "... (truncated)") {
				t.Fatalf("stored message is %d bytes, want at most %d ending with the truncation marker",
					len(c.Message), conditionMessageLimit)
			}
		})
	}
}
