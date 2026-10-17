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
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// statusConflicts fails the next armed status writes with a conflict, the way
// a pass that read a stale cache loses its write to the apiserver.
type statusConflicts struct {
	mu        sync.Mutex
	remaining int
}

// failNext makes the next status write fail.
func (s *statusConflicts) failNext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining = 1
}

func (s *statusConflicts) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			s.mu.Lock()
			fail := s.remaining > 0
			if fail {
				s.remaining--
			}
			s.mu.Unlock()
			if fail {
				return apierrors.NewConflict(schema.GroupResource{Group: kaalmv1beta1.GroupVersion.Group},
					obj.GetName(), errors.New("the object has been modified"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
}

// statusWrites counts the status writes a fake client receives, so a test
// can assert that a pass which changes nothing writes nothing.
type statusWrites struct {
	mu sync.Mutex
	n  int
}

func (s *statusWrites) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *statusWrites) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			s.mu.Lock()
			s.n++
			s.mu.Unlock()
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
}

// withPrefix keeps the fake recorder's events that start with prefix
// ("Warning Reason").
func withPrefix(events []string, prefix string) []string {
	var out []string
	for _, e := range events {
		if strings.HasPrefix(e, prefix+" ") {
			out = append(out, e)
		}
	}
	return out
}

// expectEventOnceAcrossConflict runs three passes of r on req: the first
// loses its status write to a conflict and must emit no prefix event, the
// retry must emit it once, and a third pass over the stored state must not
// emit it again. It returns the one event.
func expectEventOnceAcrossConflict(
	t *testing.T, r reconcile.Reconciler, rec *events.FakeRecorder, conflicts *statusConflicts,
	req ctrl.Request, prefix string,
) string {
	t.Helper()
	conflicts.failNext()
	if _, err := r.Reconcile(ctxT(), req); !apierrors.IsConflict(err) {
		t.Fatalf("pass with a failing status write: err = %v, want a conflict", err)
	}
	if got := withPrefix(drainEvents(rec), prefix); len(got) != 0 {
		t.Fatalf("a pass whose status write failed emitted %q", got)
	}
	if _, err := r.Reconcile(ctxT(), req); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got := withPrefix(drainEvents(rec), prefix)
	if len(got) != 1 {
		t.Fatalf("the retry emitted %d %q events, want 1: %q", len(got), prefix, got)
	}
	if _, err := r.Reconcile(ctxT(), req); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if again := withPrefix(drainEvents(rec), prefix); len(again) != 0 {
		t.Fatalf("a pass over the stored state emitted %q again", again)
	}
	return got[0]
}

// flush sends the held events only for a pass whose write succeeded, and
// clears them either way.
func TestHeldEvents_Flush(t *testing.T) {
	obj := &kaalmv1beta1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "mp"}}
	var h heldEvents
	rec := events.NewFakeRecorder(4)

	h.add(obj, "Warning", "A", "Validate", nil, "first")
	h.flush(rec, obj, false)
	if n := len(rec.Events); n != 0 {
		t.Fatalf("a failed write sent %d events", n)
	}
	if left := h.take(obj); len(left) != 0 {
		t.Fatalf("a failed write left %d events held", len(left))
	}

	h.add(obj, "Warning", "A", "Validate", nil, "first")
	h.add(obj, "Normal", "B", "Validate", nil, "second")
	h.flush(rec, obj, true)
	if got := drainEvents(rec); len(got) != 2 || got[0] != "Warning A first" || got[1] != "Normal B second" {
		t.Fatalf("events after a successful write = %q", got)
	}

	h.add(obj, "Warning", "A", "Validate", nil, "first")
	h.flush(nil, obj, true)
	if left := h.take(obj); len(left) != 0 {
		t.Fatalf("a nil recorder left %d events held", len(left))
	}
}

// actionRecorder keeps each Event's reason, action, and related object.
type actionRecorder struct {
	mu      sync.Mutex
	reasons []string
	actions []string
	related []runtime.Object
}

func (a *actionRecorder) Eventf(_, related runtime.Object, _, reason, action, _ string, _ ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasons = append(a.reasons, reason)
	a.actions = append(a.actions, action)
	a.related = append(a.related, related)
}

// PhaseChanged's action is the new phase, so the API server keeps different
// transitions as separate Events instead of counting them as repeats.
func TestSetPhase_ActionIsTheNewPhase(t *testing.T) {
	ag := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "act", Namespace: "default"},
		Status: kaalmv1beta1.AgentStatus{Phase: kaalmv1beta1.AgentRunning}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ag).WithStatusSubresource(ag).Build()
	rec := &actionRecorder{}
	r := &AgentReconciler{Client: c, Recorder: rec}
	for _, phase := range []kaalmv1beta1.AgentPhase{kaalmv1beta1.AgentIdle, kaalmv1beta1.AgentHibernating} {
		r.setPhase(ag, phase, "")
		if err := r.writeStatus(ctxT(), ag); err != nil {
			t.Fatalf("writeStatus: %v", err)
		}
	}
	want := []string{string(kaalmv1beta1.AgentIdle), string(kaalmv1beta1.AgentHibernating)}
	if len(rec.actions) != 2 || rec.actions[0] != want[0] || rec.actions[1] != want[1] {
		t.Fatalf("actions = %v, want %v", rec.actions, want)
	}
}

func TestNotReadyAction(t *testing.T) {
	cases := map[string]string{
		kaalmv1beta1.ReasonPodCreateRejected:  kaalmv1beta1.ActionCreatePod,
		kaalmv1beta1.ReasonChildWriteRejected: kaalmv1beta1.ActionWriteChild,
		kaalmv1beta1.ReasonChildConflict:      kaalmv1beta1.ActionClaimChild,
		kaalmv1beta1.ReasonCredentialsInvalid: kaalmv1beta1.ActionProbe,
		kaalmv1beta1.ReasonInvalidReference:   kaalmv1beta1.ActionValidate,
	}
	for reason, want := range cases {
		if got := notReadyAction(reason); got != want {
			t.Errorf("notReadyAction(%s) = %s, want %s", reason, got, want)
		}
	}
}

// A ChildConflict Event names the existing child as its related object.
func TestRequireControlled_ConflictCarriesTheChild(t *testing.T) {
	owner := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "u1"}}
	child := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default"}}
	cc, ok := asChildConflict(requireControlled(testScheme(t), owner, child))
	if !ok || cc.Object != child {
		t.Fatalf("conflict = %+v, want one carrying the child", cc)
	}
}
