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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func slotClass(limit *intstr.IntOrString) *kaalmv1beta1.AgentClass {
	return &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "slots"},
		Spec: kaalmv1beta1.AgentClassSpec{
			Lifecycle: kaalmv1beta1.AgentClassLifecycle{MaxUnavailableOnDrift: limit},
		},
	}
}

// slotAgent builds an Agent of the "slots" class in the given phase, with a
// PodUpToDate condition of the given reason ("" for none).
func slotAgent(name string, phase kaalmv1beta1.AgentPhase, reason string) *kaalmv1beta1.Agent {
	ag := &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       kaalmv1beta1.AgentSpec{AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "slots"}},
		Status:     kaalmv1beta1.AgentStatus{Phase: phase},
	}
	if reason != "" {
		status := metav1.ConditionFalse
		if reason == kaalmv1beta1.ReasonPodCurrent {
			status = metav1.ConditionTrue
		}
		apimeta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
			Type: kaalmv1beta1.ConditionPodUpToDate, Status: status, Reason: reason,
		})
	}
	return ag
}

func slotClient(t *testing.T, agents ...*kaalmv1beta1.Agent) client.Client {
	t.Helper()
	objs := make([]client.Object, 0, len(agents))
	for _, a := range agents {
		objs = append(objs, a)
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithIndex(&kaalmv1beta1.Agent{}, IndexAgentClassRef, func(o client.Object) []string {
			return []string{o.(*kaalmv1beta1.Agent).Spec.AgentClassRef.Name}
		}).
		WithStatusSubresource(&kaalmv1beta1.Agent{}).
		WithObjects(objs...).Build()
}

func ptrIntOrString(v intstr.IntOrString) *intstr.IntOrString { return &v }

func TestMaxUnavailableOnDrift(t *testing.T) {
	cases := []struct {
		limit *intstr.IntOrString
		total int
		want  int
	}{
		{nil, 8, 2}, // the default is 25%
		{nil, 1, 1}, // rounded up, never below 1
		{nil, 0, 1}, // an empty class still lets one through
		{ptrIntOrString(intstr.FromString("25%")), 5, 2},
		{ptrIntOrString(intstr.FromString("100%")), 7, 7},
		{ptrIntOrString(intstr.FromString("1%")), 300, 3},
		{ptrIntOrString(intstr.FromInt32(1)), 50, 1},
		{ptrIntOrString(intstr.FromInt32(10)), 3, 10},
		{ptrIntOrString(intstr.FromString("bogus")), 8, 2}, // unparseable falls back to the default
	}
	for _, tc := range cases {
		if got := maxUnavailableOnDrift(slotClass(tc.limit), tc.total); got != tc.want {
			t.Errorf("maxUnavailableOnDrift(%v, %d) = %d, want %d", tc.limit, tc.total, got, tc.want)
		}
	}
}

func TestDriftSlots_CountsReplacingAgents(t *testing.T) {
	// A fresh driftSlots (a restarted controller) rebuilds the count from the
	// Agents' PodUpToDate conditions alone.
	agents := []*kaalmv1beta1.Agent{
		slotAgent("a", kaalmv1beta1.AgentProvisioning, kaalmv1beta1.ReasonReplacing),
		slotAgent("b", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonReplacementPending),
		slotAgent("c", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent),
		slotAgent("d", kaalmv1beta1.AgentRunning, ""),
	}
	c := slotClient(t, agents...)
	var s driftSlots
	ok, err := s.acquire(context.Background(), c, agents[1], slotClass(ptrIntOrString(intstr.FromInt32(1))), time.Now())
	if err != nil || ok {
		t.Fatalf("acquire with the only slot taken = %v, %v; want refused", ok, err)
	}
	ok, err = s.acquire(context.Background(), c, agents[1], slotClass(ptrIntOrString(intstr.FromInt32(2))), time.Now())
	if err != nil || !ok {
		t.Fatalf("acquire with a free slot = %v, %v; want granted", ok, err)
	}
}

func TestDriftSlots_ReservationsCoverStaleCache(t *testing.T) {
	// The cache has not yet seen any Replacing condition, so only the
	// in-memory reservations keep concurrent reconciles under the cap.
	var agents []*kaalmv1beta1.Agent
	for i := range 10 {
		agents = append(agents, slotAgent(fmt.Sprintf("a%d", i), kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent))
	}
	c := slotClient(t, agents...)
	class := slotClass(ptrIntOrString(intstr.FromInt32(3)))
	var s driftSlots
	var granted atomic.Int32
	var wg sync.WaitGroup
	now := time.Now()
	for _, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.acquire(context.Background(), c, a, class, now)
			if err != nil {
				t.Errorf("acquire %s: %v", a.Name, err)
			}
			if ok {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := granted.Load(); got != 3 {
		t.Fatalf("granted %d slots, want 3", got)
	}
}

func TestDriftSlots_ReservationLifetime(t *testing.T) {
	a := slotAgent("a", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent)
	b := slotAgent("b", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent)
	c := slotClient(t, a, b)
	class := slotClass(ptrIntOrString(intstr.FromInt32(1)))
	var s driftSlots
	t0 := time.Now()
	if ok, _ := s.acquire(context.Background(), c, a, class, t0); !ok {
		t.Fatal("first acquire refused")
	}
	// The holder asking again is not blocked by its own reservation.
	if ok, _ := s.acquire(context.Background(), c, a, class, t0); !ok {
		t.Fatal("re-acquire by the holder refused")
	}
	if ok, _ := s.acquire(context.Background(), c, b, class, t0.Add(time.Second)); ok {
		t.Fatal("second agent granted while the reservation is live")
	}
	// A reservation the cache never confirms expires.
	if ok, _ := s.acquire(context.Background(), c, b, class, t0.Add(driftReservationTTL+time.Second)); !ok {
		t.Fatal("second agent refused after the reservation expired")
	}
}

func TestDriftSlots_ConfirmedReservationCountsOnce(t *testing.T) {
	a := slotAgent("a", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent)
	b := slotAgent("b", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent)
	c := slotClient(t, a, b, slotAgent("x", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent))
	class := slotClass(ptrIntOrString(intstr.FromInt32(2)))
	var s driftSlots
	now := time.Now()
	if ok, _ := s.acquire(context.Background(), c, a, class, now); !ok {
		t.Fatal("acquire a refused")
	}
	// The cache catches up: a now shows Replacing. It must count once, not
	// once for the condition and again for the reservation.
	apimeta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionPodUpToDate, Status: metav1.ConditionFalse, Reason: kaalmv1beta1.ReasonReplacing,
	})
	if err := c.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.acquire(context.Background(), c, b, class, now); !ok {
		t.Fatal("acquire b refused: the confirmed reservation was counted twice")
	}
}

func TestDriftSlots_IdleFirst(t *testing.T) {
	idle := slotAgent("idle", kaalmv1beta1.AgentIdle, kaalmv1beta1.ReasonReplacementPending)
	busy := slotAgent("busy", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonReplacementPending)
	c := slotClient(t, idle, busy)
	class := slotClass(ptrIntOrString(intstr.FromInt32(2)))
	var s driftSlots
	now := time.Now()
	if ok, _ := s.acquire(context.Background(), c, busy, class, now); ok {
		t.Fatal("a Running agent took a slot while an Idle one waits")
	}
	if ok, _ := s.acquire(context.Background(), c, idle, class, now); !ok {
		t.Fatal("the Idle agent was refused")
	}
}

func TestDriftSlots_ListError(t *testing.T) {
	var s driftSlots
	_, err := s.acquire(context.Background(), newErrListClient(t),
		slotAgent("a", kaalmv1beta1.AgentRunning, ""), slotClass(nil), time.Now())
	if err == nil {
		t.Fatal("acquire swallowed the list error")
	}
}

func TestDriveHibernating_ClearsDriftWait(t *testing.T) {
	ag := slotAgent("sleeper", kaalmv1beta1.AgentHibernating, kaalmv1beta1.ReasonReplacementPending)
	c := slotClient(t, ag)
	r := &AgentReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.driveHibernating(context.Background(), ag); err != nil {
		t.Fatal(err)
	}
	if ag.Status.Phase != kaalmv1beta1.AgentHibernated {
		t.Fatalf("phase %s, want Hibernated", ag.Status.Phase)
	}
	if reason := podUpToDateReason(ag); reason != "" {
		t.Fatalf("PodUpToDate %q survived hibernation", reason)
	}
}
