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

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The AgentClass advisory findings are states: one Warning on the rising
// edge of the condition, sent only after the status write that records it.
// FQDNPolicyUnsupported used to fire on every pass.
func TestAgentClass_AdvisoryWarningsFollowTheStatusWrite(t *testing.T) {
	ac := &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ev-class", Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
		},
		Spec: kaalmv1beta1.AgentClassSpec{
			Security: kaalmv1beta1.AgentClassSecurity{
				ContainerSecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: boolPtr(false)},
			},
		},
	}
	ac.Spec.Network.Egress.AllowedHosts = []string{"api.example.com"}
	ac.Spec.Network.AllowHostNetwork = true
	conflicts := &statusConflicts{}
	c := classClientBuilder(t, ac).WithInterceptorFuncs(conflicts.funcs()).Build()
	rec := record.NewFakeRecorder(16)
	r := &AgentClassReconciler{Client: c, Recorder: rec, FQDNSupport: func() (bool, error) { return false, nil }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ev-class"}}

	conflicts.failNext()
	if _, err := r.Reconcile(ctxT(), req); err == nil {
		t.Fatal("pass with a failing status write succeeded")
	}
	if got := drainEvents(rec); len(got) != 0 {
		t.Fatalf("a pass whose status write failed emitted %q", got)
	}
	for pass := 2; pass <= 3; pass++ {
		if _, err := r.Reconcile(ctxT(), req); err != nil {
			t.Fatal(err)
		}
		want := 0
		if pass == 2 {
			want = 1
		}
		got := drainEvents(rec)
		for _, reason := range []string{
			kaalmv1beta1.ReasonFQDNPolicyUnsupported, kaalmv1beta1.ReasonBelowRestrictedBaseline,
			kaalmv1beta1.ReasonDeprecatedFieldSet,
		} {
			if n := len(withPrefix(got, "Warning "+reason)); n != want {
				t.Errorf("pass %d emitted %d %s events, want %d", pass, n, reason, want)
			}
		}
	}
}

// classClientBuilder is a fake client holding ac, with the status subresource
// and the workload indexes the AgentClass reconciler lists by.
func classClientBuilder(t *testing.T, ac *kaalmv1beta1.AgentClass) *fake.ClientBuilder {
	t.Helper()
	byClass := func(o client.Object) []string {
		switch w := o.(type) {
		case *kaalmv1beta1.Agent:
			return []string{w.Spec.AgentClassRef.Name}
		case *kaalmv1beta1.AgentTask:
			return []string{w.Spec.AgentClassRef.Name}
		}
		return nil
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ac).WithStatusSubresource(ac).
		WithIndex(&kaalmv1beta1.Agent{}, IndexAgentClassRef, byClass).
		WithIndex(&kaalmv1beta1.AgentTask{}, IndexAgentClassRef, byClass)
}

// DeprecatedFields is advisory: absent on a class that never set a deprecated
// field, True with one Warning on each rising edge, False without an event
// once the field is cleared. Ready never depends on it.
func TestAgentClass_DeprecatedFieldsCondition(t *testing.T) {
	newClass := func(name string, hostNetwork bool) *kaalmv1beta1.AgentClass {
		ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{
			Name: name, Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
		}}
		ac.Spec.Network.AllowHostNetwork = hostNetwork
		return ac
	}
	reconcile := func(t *testing.T, c client.Client, rec *record.FakeRecorder, name string) (*kaalmv1beta1.AgentClass, int) {
		t.Helper()
		r := &AgentClassReconciler{Client: c, Recorder: rec}
		if _, err := r.Reconcile(ctxT(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
			t.Fatal(err)
		}
		var got kaalmv1beta1.AgentClass
		if err := c.Get(ctxT(), types.NamespacedName{Name: name}, &got); err != nil {
			t.Fatal(err)
		}
		if !apimeta.IsStatusConditionTrue(got.Status.Conditions, kaalmv1beta1.ConditionReady) {
			t.Errorf("Ready is not True: %+v", apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionReady))
		}
		return &got, len(withPrefix(drainEvents(rec), "Warning "+kaalmv1beta1.ReasonDeprecatedFieldSet))
	}
	expect := func(t *testing.T, ac *kaalmv1beta1.AgentClass, status metav1.ConditionStatus, reason string) {
		t.Helper()
		cond := apimeta.FindStatusCondition(ac.Status.Conditions, kaalmv1beta1.ConditionDeprecatedFields)
		if cond == nil {
			t.Fatalf("no %s condition", kaalmv1beta1.ConditionDeprecatedFields)
		}
		if cond.Status != status || cond.Reason != reason {
			t.Errorf("%s = %s/%s, want %s/%s", cond.Type, cond.Status, cond.Reason, status, reason)
		}
		if status == metav1.ConditionTrue && !strings.Contains(cond.Message, "network.allowHostNetwork") {
			t.Errorf("message %q does not name network.allowHostNetwork", cond.Message)
		}
	}

	t.Run("clean class has no condition", func(t *testing.T) {
		ac := newClass("clean", false)
		c := classClientBuilder(t, ac).Build()
		rec := record.NewFakeRecorder(16)
		got, events := reconcile(t, c, rec, ac.Name)
		if cond := apimeta.FindStatusCondition(got.Status.Conditions, kaalmv1beta1.ConditionDeprecatedFields); cond != nil {
			t.Errorf("clean class has condition %+v", cond)
		}
		if events != 0 {
			t.Errorf("clean class emitted %d DeprecatedFieldSet events", events)
		}
	})

	t.Run("set, steady, cleared, set again", func(t *testing.T) {
		ac := newClass("legacy", true)
		c := classClientBuilder(t, ac).Build()
		rec := record.NewFakeRecorder(16)

		got, events := reconcile(t, c, rec, ac.Name)
		expect(t, got, metav1.ConditionTrue, kaalmv1beta1.ReasonDeprecatedFieldSet)
		if events != 1 {
			t.Errorf("first pass emitted %d DeprecatedFieldSet events, want 1", events)
		}

		got, events = reconcile(t, c, rec, ac.Name)
		expect(t, got, metav1.ConditionTrue, kaalmv1beta1.ReasonDeprecatedFieldSet)
		if events != 0 {
			t.Errorf("steady pass emitted %d DeprecatedFieldSet events, want 0", events)
		}

		got.Spec.Network.AllowHostNetwork = false
		if err := c.Update(ctxT(), got); err != nil {
			t.Fatal(err)
		}
		got, events = reconcile(t, c, rec, ac.Name)
		expect(t, got, metav1.ConditionFalse, kaalmv1beta1.ReasonNoDeprecatedFields)
		if events != 0 {
			t.Errorf("clearing pass emitted %d DeprecatedFieldSet events, want 0", events)
		}

		got.Spec.Network.AllowHostNetwork = true
		if err := c.Update(ctxT(), got); err != nil {
			t.Fatal(err)
		}
		got, events = reconcile(t, c, rec, ac.Name)
		expect(t, got, metav1.ConditionTrue, kaalmv1beta1.ReasonDeprecatedFieldSet)
		if events != 1 {
			t.Errorf("second rising edge emitted %d DeprecatedFieldSet events, want 1", events)
		}
	})
}
