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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// setPhase is the one place a phase changes, so it is the one place
// PhaseChanged is emitted: once per transition, naming both phases, and only
// after the status write that persists it.
func TestSetPhase_EmitsPhaseChangedOnTransitionOnly(t *testing.T) {
	ag := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sp", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ag).WithStatusSubresource(ag).Build()
	rec := record.NewFakeRecorder(8)
	r := &AgentReconciler{Client: c, Recorder: rec}
	write := func() {
		t.Helper()
		if err := r.writeStatus(ctxT(), ag); err != nil {
			t.Fatalf("writeStatus: %v", err)
		}
	}

	// The first phase is not a transition from anything.
	r.setPhase(ag, kaalmv1beta1.AgentPending, "")
	write()
	if n := len(rec.Events); n != 0 {
		t.Fatalf("initial Pending emitted %d events, want 0: %s", n, <-rec.Events)
	}
	if ag.Status.PhaseTransitionTime == nil {
		t.Fatal("phaseTransitionTime not set on the first phase")
	}

	// The event waits for the write.
	r.setPhase(ag, kaalmv1beta1.AgentProvisioning, "")
	if n := len(rec.Events); n != 0 {
		t.Fatalf("PhaseChanged emitted before the status write")
	}
	write()
	want := "Normal PhaseChanged phase changed from Pending to Provisioning"
	if got := <-rec.Events; got != want {
		t.Errorf("event = %q, want %q", got, want)
	}

	// Setting the same phase again is not a transition.
	r.setPhase(ag, kaalmv1beta1.AgentProvisioning, "ignored")
	write()
	if n := len(rec.Events); n != 0 {
		t.Fatalf("same-phase set emitted %d events, want 0", n)
	}

	// A cause, when given, follows the two phases.
	r.setPhase(ag, kaalmv1beta1.AgentFailed, "container agent: CrashLoopBackOff")
	write()
	want = "Normal PhaseChanged phase changed from Provisioning to Failed: container agent: CrashLoopBackOff"
	if got := <-rec.Events; got != want {
		t.Errorf("event = %q, want %q", got, want)
	}
}

// A status write that fails reports nothing: the transition did not happen,
// and the pass that retries it emits the event then.
func TestWriteStatus_FailedWriteDropsHeldEvents(t *testing.T) {
	ag := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "sp-conflict", Namespace: "default"},
		Status: kaalmv1beta1.AgentStatus{Phase: kaalmv1beta1.AgentRunning}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ag).WithStatusSubresource(ag).Build()
	rec := record.NewFakeRecorder(8)
	r := &AgentReconciler{Client: c, Recorder: rec}

	stale := ag.DeepCopy()
	stale.ResourceVersion = "1"
	fresh := ag.DeepCopy()
	if err := c.Get(ctxT(), client.ObjectKeyFromObject(ag), fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Status.LastActivityTime = &metav1.Time{Time: time.Now()}
	if err := c.Status().Update(ctxT(), fresh); err != nil {
		t.Fatal(err)
	}

	r.setPhase(stale, kaalmv1beta1.AgentIdle, "no activity")
	if err := r.writeStatus(ctxT(), stale); err == nil {
		t.Fatal("stale write succeeded; want a conflict")
	}
	if n := len(rec.Events); n != 0 {
		t.Fatalf("a failed write emitted %d events: %s", n, <-rec.Events)
	}
	if held := r.events.take(stale); len(held) != 0 {
		t.Errorf("events still held after the failed write: %v", held)
	}
}

// Every transition of a live Agent emits PhaseChanged once, including the
// provisioning path and a return from Idle, which emitted nothing before.
func TestAgent_PhaseChangedOnEveryTransition(t *testing.T) {
	mkWorkloadClass(t, "wc-phase-ev", nil)
	provisionRunningAgentWithLifecycle(t, "phase-ev", "wc-phase-ev", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Second}
	})
	expectEvent(t, "Agent", "default", "phase-ev", kaalmv1beta1.ReasonPhaseChanged, corev1.EventTypeNormal,
		"from Pending to Provisioning")
	expectEvent(t, "Agent", "default", "phase-ev", kaalmv1beta1.ReasonPhaseChanged, corev1.EventTypeNormal,
		"from Provisioning to Running")

	fakeActivity.set([]ReplicaActivity{replicaWith(2*time.Hour, "phase-ev", time.Hour)}, 1)
	touchAgent(t, "phase-ev")
	expectAgentPhase(t, "phase-ev", kaalmv1beta1.AgentIdle)
	idle := expectEvent(t, "Agent", "default", "phase-ev", kaalmv1beta1.ReasonPhaseChanged, corev1.EventTypeNormal,
		"from Running to Idle")
	if n := eventCount(idle); n != 1 {
		t.Errorf("Running to Idle emitted %d PhaseChanged events, want 1", n)
	}

	fakeActivity.set([]ReplicaActivity{replicaWith(2*time.Hour, "phase-ev", 0)}, 1)
	touchAgent(t, "phase-ev")
	expectAgentPhase(t, "phase-ev", kaalmv1beta1.AgentRunning)
	expectEvent(t, "Agent", "default", "phase-ev", kaalmv1beta1.ReasonPhaseChanged, corev1.EventTypeNormal,
		"from Idle to Running")

	// No other PhaseChanged message names the Idle transition: the ad hoc
	// emitter is gone, so the transition is not reported twice.
	for _, ev := range objectEvents(t, "Agent", "default", "phase-ev", kaalmv1beta1.ReasonPhaseChanged) {
		if len(ev.Message) >= 5 && ev.Message[:5] == "idle:" {
			t.Errorf("old-style PhaseChanged event still emitted: %q", ev.Message)
		}
	}
}

// agentPass is one pass of an Agent reconciler step over a fresh read of the
// Agent, dropping what it held on the way out as Reconcile does.
func agentPass(t *testing.T, r *AgentReconciler, name string, step func(*kaalmv1beta1.Agent) error) error {
	t.Helper()
	var ag kaalmv1beta1.Agent
	if err := r.Get(ctxT(), types.NamespacedName{Name: name, Namespace: "default"}, &ag); err != nil {
		t.Fatal(err)
	}
	defer r.events.take(&ag)
	return step(&ag)
}

// The Agent's state events wait for the status write: a pass that loses its
// write to a conflict emits nothing, and the retry emits each once.
func TestAgent_StateEventsFollowTheStatusWrite(t *testing.T) {
	cases := []struct {
		name   string
		phase  kaalmv1beta1.AgentPhase
		prefix string
		step   func(r *AgentReconciler, ag *kaalmv1beta1.Agent) error
		steady bool // a third pass over the stored state must stay quiet
	}{{
		name: "ChildConflict", phase: kaalmv1beta1.AgentRunning,
		prefix: "Warning " + kaalmv1beta1.ReasonChildConflict, steady: true,
		step: func(r *AgentReconciler, ag *kaalmv1beta1.Agent) error {
			_, err := r.childConflict(ctxT(), ag, ag.Status.DeepCopy(),
				&ChildConflictError{Kind: "Service", Name: ag.Name, OwnerKind: "Agent"})
			return err
		},
	}, {
		name: "entering Degraded", phase: kaalmv1beta1.AgentRunning,
		prefix: "Warning " + kaalmv1beta1.ReasonHandlerMountNotAllowed, steady: true,
		step: func(r *AgentReconciler, ag *kaalmv1beta1.Agent) error {
			return r.enterOrStayDegraded(ctxT(), ag, []metav1.Condition{{
				Reason: kaalmv1beta1.ReasonHandlerMountNotAllowed, Message: "handler mounts are off",
			}})
		},
	}, {
		name: "Hibernated", phase: kaalmv1beta1.AgentHibernating,
		prefix: "Normal " + kaalmv1beta1.ReasonHibernated,
		step: func(r *AgentReconciler, ag *kaalmv1beta1.Agent) error {
			_, err := r.driveHibernating(ctxT(), ag)
			return err
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ag := &kaalmv1beta1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "ev-agent", Namespace: "default"},
				Status: kaalmv1beta1.AgentStatus{Phase: tc.phase}}
			conflicts := &statusConflicts{}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ag).
				WithStatusSubresource(ag).WithInterceptorFuncs(conflicts.funcs()).Build()
			rec := record.NewFakeRecorder(16)
			r := &AgentReconciler{Client: c, Recorder: rec}
			step := func(a *kaalmv1beta1.Agent) error { return tc.step(r, a) }

			conflicts.failNext()
			if err := agentPass(t, r, "ev-agent", step); !apierrors.IsConflict(err) {
				t.Fatalf("err = %v, want a conflict", err)
			}
			if got := drainEvents(rec); len(got) != 0 {
				t.Fatalf("a pass whose status write failed emitted %q", got)
			}
			if err := agentPass(t, r, "ev-agent", step); err != nil {
				t.Fatal(err)
			}
			if got := withPrefix(drainEvents(rec), tc.prefix); len(got) != 1 {
				t.Fatalf("the retry emitted %d %q events, want 1", len(got), tc.prefix)
			}
			if !tc.steady {
				return
			}
			if err := agentPass(t, r, "ev-agent", step); err != nil {
				t.Fatal(err)
			}
			if got := withPrefix(drainEvents(rec), tc.prefix); len(got) != 0 {
				t.Fatalf("a pass over the stored state emitted %q again", got)
			}
		})
	}
}
