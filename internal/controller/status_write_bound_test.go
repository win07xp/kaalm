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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// boundFakeClient is a fake client with the status subresource on the six
// Kaalm kinds, wrapped in boundStatusWrites.
func boundFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.Agent{}, &kaalmv1beta1.AgentTask{}, &kaalmv1beta1.AgentChannel{},
			&kaalmv1beta1.AgentClass{}, &kaalmv1beta1.ModelProvider{}, &kaalmv1beta1.ToolProvider{}).
		Build()
	return boundStatusWrites(c)
}

// expectCut fails unless msg is cut to the condition limit with the marker.
func expectCut(t *testing.T, msg string) {
	t.Helper()
	if len(msg) > conditionMessageLimit || !strings.HasSuffix(msg, "... (truncated)") {
		t.Fatalf("message is %d bytes, want at most %d ending with the truncation marker", len(msg), conditionMessageLimit)
	}
}

func TestBoundStatusWrites_UpdateCutsLongMessageOnly(t *testing.T) {
	agent := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}
	c := boundFakeClient(t, agent)
	agent.Status.Conditions = []metav1.Condition{
		{Type: "Long", Status: metav1.ConditionTrue, Reason: "R", Message: strings.Repeat("x", 40000)},
		{Type: "Short", Status: metav1.ConditionTrue, Reason: "R", Message: "short"},
	}
	if err := c.Status().Update(context.Background(), agent); err != nil {
		t.Fatalf("status update: %v", err)
	}
	var got kaalmv1beta1.Agent
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(agent), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	expectCut(t, apimeta.FindStatusCondition(got.Status.Conditions, "Long").Message)
	if m := apimeta.FindStatusCondition(got.Status.Conditions, "Short").Message; m != "short" {
		t.Fatalf("short message = %q, want it unchanged", m)
	}
}

func TestBoundStatusWrites_MergePatchCutsLongMessage(t *testing.T) {
	mp := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "mp"}}
	c := boundFakeClient(t, mp)
	orig := mp.DeepCopy()
	mp.Status.Conditions = []metav1.Condition{
		{Type: "Long", Status: metav1.ConditionTrue, Reason: "R", Message: strings.Repeat("x", 40000)},
	}
	if err := c.Status().Patch(context.Background(), mp, client.MergeFrom(orig)); err != nil {
		t.Fatalf("status patch: %v", err)
	}
	var got kaalmv1beta1.ModelProvider
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(mp), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	expectCut(t, apimeta.FindStatusCondition(got.Status.Conditions, "Long").Message)
}

func TestCutConditionMessages_Boundary(t *testing.T) {
	atLimit := strings.Repeat("x", maxConditionMessageBytes)
	agent := &kaalmv1beta1.Agent{Status: kaalmv1beta1.AgentStatus{Conditions: []metav1.Condition{
		{Type: "AtLimit", Message: atLimit},
		{Type: "OverLimit", Message: atLimit + "x"},
	}}}
	cutConditionMessages(context.Background(), agent)
	if agent.Status.Conditions[0].Message != atLimit {
		t.Fatalf("a %d-byte message was changed", maxConditionMessageBytes)
	}
	expectCut(t, agent.Status.Conditions[1].Message)
}

// Objects the net cannot inspect pass through unchanged.
func TestBoundStatusWrites_PassesThroughOtherObjects(t *testing.T) {
	long := strings.Repeat("x", 40000)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	c := boundFakeClient(t, pod)
	pod.Status.Conditions = []corev1.PodCondition{{Type: "Long", Status: corev1.ConditionTrue, Message: long}}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("pod status update: %v", err)
	}
	if pod.Status.Conditions[0].Message != long {
		t.Fatalf("a Pod condition message was changed")
	}

	u := &unstructured.Unstructured{}
	u.SetAPIVersion(kaalmv1beta1.GroupVersion.String())
	u.SetKind("Agent")
	u.SetName("u")
	if err := unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Long", "message": long}},
		"status", "conditions"); err != nil {
		t.Fatalf("set conditions: %v", err)
	}
	cutConditionMessages(context.Background(), u)
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if conds[0].(map[string]any)["message"] != long {
		t.Fatalf("an unstructured Agent's condition message was changed")
	}
}

// The common case, where no message is too long, allocates nothing.
func TestCutConditionMessages_ShortMessagesAllocateNothing(t *testing.T) {
	agent := &kaalmv1beta1.Agent{Status: kaalmv1beta1.AgentStatus{Conditions: []metav1.Condition{
		{Type: "Ready", Message: "agent Pod is ready"},
		{Type: "PodUpToDate", Message: "agent Pod matches the derived spec"},
	}}}
	ctx := context.Background()
	if n := testing.AllocsPerRun(100, func() { cutConditionMessages(ctx, agent) }); n != 0 {
		t.Fatalf("cutConditionMessages allocated %v times per run, want 0", n)
	}
}

// Every Kaalm kind whose status has conditions is one conditionsOf knows, so
// a new kind cannot skip the net.
func TestConditionsOf_CoversEveryKindWithConditions(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kaalmv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	condType := reflect.TypeFor[[]metav1.Condition]()
	checked := 0
	for kind, typ := range scheme.KnownTypes(kaalmv1beta1.GroupVersion) {
		status, ok := typ.FieldByName("Status")
		if !ok || status.Type.Kind() != reflect.Struct {
			continue
		}
		field, ok := status.Type.FieldByName("Conditions")
		if !ok || field.Type != condType {
			continue
		}
		ptr := reflect.New(typ)
		ptr.Elem().FieldByName("Status").FieldByName("Conditions").
			Set(reflect.ValueOf([]metav1.Condition{{Type: "Ready"}}))
		obj, ok := ptr.Interface().(client.Object)
		if !ok {
			t.Fatalf("%s is not a client.Object", kind)
		}
		if got := len(conditionsOf(obj)); got != 1 {
			t.Errorf("conditionsOf(%s) has %d conditions, want 1: add it to conditionsOf", kind, got)
		}
		checked++
	}
	if checked < 6 {
		t.Fatalf("checked %d kinds, want at least the six with status conditions", checked)
	}
}
