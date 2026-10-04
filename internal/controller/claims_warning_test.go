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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func claimsTestAgent() *kaalmv1beta1.Agent {
	return &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "claims", Namespace: "default", UID: "claims-uid"}}
}

func resourceClaimsEvents(rec *record.FakeRecorder) []string {
	return withPrefix(drainEvents(rec), "Warning "+kaalmv1beta1.ReasonResourceClaimsIgnored)
}

// Rule 53: the Warning fires when an object's claims first appear or change,
// not on every pass, and again after they are removed and added back.
func TestClaimsWarnings_EmitsOnFirstAppearanceAndChange(t *testing.T) {
	var w claimsWarnings
	rec := record.NewFakeRecorder(8)
	obj := claimsTestAgent()
	gpu := []corev1.ResourceClaim{{Name: "gpu"}}

	w.note(rec, obj, "spec.resources", gpu)
	got := resourceClaimsEvents(rec)
	if len(got) != 1 || !strings.Contains(got[0], "spec.resources.claims") || !strings.Contains(got[0], "gpu") {
		t.Fatalf("first note emitted %q, want one Warning naming spec.resources.claims and gpu", got)
	}
	w.note(rec, obj, "spec.resources", gpu)
	if got := resourceClaimsEvents(rec); len(got) != 0 {
		t.Fatalf("an unchanged note emitted %q", got)
	}
	w.note(rec, obj, "spec.resources", []corev1.ResourceClaim{{Name: "gpu"}, {Name: "fpga", Request: "large"}})
	if got := resourceClaimsEvents(rec); len(got) != 1 || !strings.Contains(got[0], "gpu, fpga/large") {
		t.Fatalf("a changed claim list emitted %q, want one Warning naming gpu, fpga/large", got)
	}
	w.note(rec, obj, "spec.resources", nil)
	if got := resourceClaimsEvents(rec); len(got) != 0 {
		t.Fatalf("removing the claims emitted %q", got)
	}
	w.note(rec, obj, "spec.resources", gpu)
	if got := resourceClaimsEvents(rec); len(got) != 1 {
		t.Fatalf("claims added back emitted %q, want one Warning", got)
	}
}

// forget drops the memory, so the next note emits again; a nil recorder is a
// no-op that records nothing.
func TestClaimsWarnings_ForgetAndNilRecorder(t *testing.T) {
	var w claimsWarnings
	rec := record.NewFakeRecorder(8)
	obj := claimsTestAgent()
	gpu := []corev1.ResourceClaim{{Name: "gpu"}}

	w.note(nil, obj, "spec.resources", gpu)
	w.note(rec, obj, "spec.resources", gpu)
	if got := resourceClaimsEvents(rec); len(got) != 1 {
		t.Fatalf("note after a nil-recorder note emitted %q, want one Warning", got)
	}
	w.forget(obj.UID)
	w.note(rec, obj, "spec.resources", gpu)
	if got := resourceClaimsEvents(rec); len(got) != 1 {
		t.Fatalf("note after forget emitted %q, want one Warning", got)
	}
}

// The class emits the Warning only after its status write succeeds, once,
// and Ready stays as computed.
func TestAgentClass_ResourceClaimsWarningFollowsStatusWrite(t *testing.T) {
	ac := &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "claims-class", Generation: 1, Finalizers: []string{kaalmv1beta1.ClassFinalizer},
		},
	}
	ac.Spec.Resources.Defaults = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		Claims:   []corev1.ResourceClaim{{Name: "gpu"}},
	}
	conflicts := &statusConflicts{}
	c := classClientBuilder(t, ac).WithInterceptorFuncs(conflicts.funcs()).Build()
	rec := record.NewFakeRecorder(16)
	r := &AgentClassReconciler{Client: c, Recorder: rec, FQDNSupport: func() (bool, error) { return false, nil }}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "claims-class"}}

	conflicts.failNext()
	if _, err := r.Reconcile(ctxT(), req); err == nil {
		t.Fatal("pass with a failing status write succeeded")
	}
	if got := resourceClaimsEvents(rec); len(got) != 0 {
		t.Fatalf("a pass whose status write failed emitted %q", got)
	}
	if _, err := r.Reconcile(ctxT(), req); err != nil {
		t.Fatal(err)
	}
	got := resourceClaimsEvents(rec)
	if len(got) != 1 || !strings.Contains(got[0], "spec.resources.defaults.claims") {
		t.Fatalf("pass 2 emitted %q, want one Warning naming spec.resources.defaults.claims", got)
	}
	if _, err := r.Reconcile(ctxT(), req); err != nil {
		t.Fatal(err)
	}
	if got := resourceClaimsEvents(rec); len(got) != 0 {
		t.Fatalf("pass 3 emitted %q, want none", got)
	}
	var stored kaalmv1beta1.AgentClass
	if err := c.Get(ctxT(), req.NamespacedName, &stored); err != nil {
		t.Fatal(err)
	}
	ready := apimeta.FindStatusCondition(stored.Status.Conditions, kaalmv1beta1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != kaalmv1beta1.ReasonAllReferencesResolved {
		t.Fatalf("Ready = %+v, want True/%s", ready, kaalmv1beta1.ReasonAllReferencesResolved)
	}
}

func TestAgent_ResourceClaimsIgnoredEvent(t *testing.T) {
	mkWorkloadClass(t, "wc-claims-event", nil)
	mkWorkloadAgent(t, "claims-event-agent", "wc-claims-event", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			Claims:   []corev1.ResourceClaim{{Name: "gpu"}},
		}
	})
	expectEvent(t, "Agent", "default", "claims-event-agent", kaalmv1beta1.ReasonResourceClaimsIgnored,
		corev1.EventTypeWarning, "spec.resources.claims")
}

func TestAgentTask_ResourceClaimsIgnoredEvent(t *testing.T) {
	mkWorkloadClass(t, "wc-claims-task", nil)
	mkTask(t, "claims-event-task", "wc-claims-task", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			Claims:   []corev1.ResourceClaim{{Name: "gpu"}},
		}
	})
	expectEvent(t, "AgentTask", "default", "claims-event-task", kaalmv1beta1.ReasonResourceClaimsIgnored,
		corev1.EventTypeWarning, "spec.resources.claims")
}

// A settled task never warns, and its memory entry is dropped.
func TestAgentTask_TerminalTaskDoesNotWarnAboutClaims(t *testing.T) {
	done := metav1.Now()
	task := &kaalmv1beta1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: "claims-done", Namespace: "default", UID: "claims-done-uid",
			Finalizers: []string{kaalmv1beta1.TaskFinalizer},
		},
		Spec: kaalmv1beta1.AgentTaskSpec{
			Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}},
		},
		Status: kaalmv1beta1.AgentTaskStatus{Phase: kaalmv1beta1.TaskSucceeded, CompletionTime: &done},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task).WithStatusSubresource(task).Build()
	rec := record.NewFakeRecorder(8)
	r := &AgentTaskReconciler{Client: c, Recorder: rec}
	r.claimsWarned.sent = map[types.UID]string{task.UID: "earlier"}
	if _, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "claims-done"}}); err != nil {
		t.Fatal(err)
	}
	if got := resourceClaimsEvents(rec); len(got) != 0 {
		t.Fatalf("a settled task emitted %q", got)
	}
	if _, ok := r.claimsWarned.sent[task.UID]; ok {
		t.Error("a settled task's entry was kept")
	}
}
