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
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	byClass := func(o client.Object) []string {
		switch w := o.(type) {
		case *kaalmv1beta1.Agent:
			return []string{w.Spec.AgentClassRef.Name}
		case *kaalmv1beta1.AgentTask:
			return []string{w.Spec.AgentClassRef.Name}
		}
		return nil
	}
	conflicts := &statusConflicts{}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ac).WithStatusSubresource(ac).
		WithIndex(&kaalmv1beta1.Agent{}, IndexAgentClassRef, byClass).
		WithIndex(&kaalmv1beta1.AgentTask{}, IndexAgentClassRef, byClass).
		WithInterceptorFuncs(conflicts.funcs()).Build()
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
		for _, reason := range []string{kaalmv1beta1.ReasonFQDNPolicyUnsupported, kaalmv1beta1.ReasonBelowRestrictedBaseline} {
			if n := len(withPrefix(got, "Warning "+reason)); n != want {
				t.Errorf("pass %d emitted %d %s events, want %d", pass, n, reason, want)
			}
		}
	}
}
