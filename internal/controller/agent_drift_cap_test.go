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
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// driftReasons returns each named Agent's PodUpToDate reason.
func driftReasons(t *testing.T, names []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range names {
		out[n] = podUpToDateReason(getWorkloadAgent(t, n))
	}
	return out
}

// expectDriftSplit waits until exactly `replacing` of the named Agents are
// Replacing and the rest are ReplacementPending, and returns the Replacing
// ones.
func expectDriftSplit(t *testing.T, names []string, replacing int) []string {
	t.Helper()
	var holders []string
	eventually(t, func() error {
		holders = nil
		pending := 0
		reasons := driftReasons(t, names)
		for _, n := range names {
			switch reasons[n] {
			case kaalmv1beta1.ReasonReplacing:
				holders = append(holders, n)
			case kaalmv1beta1.ReasonReplacementPending:
				pending++
			}
		}
		if len(holders) != replacing || pending != len(names)-replacing {
			return fmt.Errorf("reasons %v, want %d Replacing and the rest ReplacementPending", reasons, replacing)
		}
		return nil
	})
	return holders
}

// holdDriftSplit checks for a while that the split does not move: the cap
// holds, not just a snapshot of it.
func holdDriftSplit(t *testing.T, names []string, replacing int) {
	t.Helper()
	for range 10 {
		n := 0
		for _, r := range driftReasons(t, names) {
			if r == kaalmv1beta1.ReasonReplacing {
				n++
			}
		}
		if n > replacing {
			t.Fatalf("%d Agents Replacing, cap is %d", n, replacing)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func expectClassDriftCounts(t *testing.T, className string, replacing, pending int32) {
	t.Helper()
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: className}, &ac); err != nil {
			return err
		}
		if ac.Status.AgentsReplacing != replacing || ac.Status.AgentsPendingReplacement != pending {
			return fmt.Errorf("class counts replacing=%d pending=%d, want %d and %d",
				ac.Status.AgentsReplacing, ac.Status.AgentsPendingReplacement, replacing, pending)
		}
		return nil
	})
}

func expectDriftReason(t *testing.T, name, reason string) {
	t.Helper()
	eventually(t, func() error {
		if got := podUpToDateReason(getWorkloadAgent(t, name)); got != reason {
			return fmt.Errorf("%s PodUpToDate reason %q, want %q", name, got, reason)
		}
		return nil
	})
}

func markPodCrashLooping(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "agent",
		RestartCount: crashLoopThreshold,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff", Message: "back-off restarting",
		}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("mark pod crash looping: %v", err)
	}
}

func setClassPodLabel(t *testing.T, className, value string) {
	t.Helper()
	updateWorkloadClass(t, className, func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.PodMetadata.Labels = map[string]string{"rollout": value}
	})
}

func TestAgent_DriftReplacementsAreCapped(t *testing.T) {
	one := intstr.FromInt32(1)
	mkWorkloadClass(t, "wc-cap", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Lifecycle.MaxUnavailableOnDrift = &one
	})
	names := []string{"cap-a", "cap-b", "cap-c"}
	pods := map[string]*corev1.Pod{}
	for _, n := range names {
		pods[n] = provisionRunningAgent(t, n, "wc-cap")
		expectDriftReason(t, n, kaalmv1beta1.ReasonPodCurrent)
	}

	setClassPodLabel(t, "wc-cap", "v2")

	remaining := append([]string{}, names...)
	for len(remaining) > 0 {
		holder := expectDriftSplit(t, remaining, 1)[0]
		holdDriftSplit(t, names, 1)
		expectClassDriftCounts(t, "wc-cap", 1, int32(len(remaining)-1))
		// The waiting Agents keep their old Pod and stay Ready.
		for _, n := range remaining {
			if n == holder {
				continue
			}
			if p := agentPod(t, n); p == nil || p.Name != pods[n].Name {
				t.Fatalf("%s lost its old Pod while waiting for a slot", n)
			}
			expectAgentReadyReason(t, n, kaalmv1beta1.ReasonPodRunning)
		}
		pod := expectPodReplaced(t, holder, pods[holder])
		if pod.Labels["rollout"] != "v2" {
			t.Fatalf("%s replacement lacks the new label", holder)
		}
		markPodReady(t, pod)
		expectDriftReason(t, holder, kaalmv1beta1.ReasonPodCurrent)
		remaining = slices.DeleteFunc(remaining, func(n string) bool { return n == holder })
	}
	expectClassDriftCounts(t, "wc-cap", 0, 0)
}

func TestAgent_FailedReplacementHoldsSlot(t *testing.T) {
	one := intstr.FromInt32(1)
	mkWorkloadClass(t, "wc-halt", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Lifecycle.MaxUnavailableOnDrift = &one
	})
	names := []string{"halt-a", "halt-b"}
	pods := map[string]*corev1.Pod{}
	for _, n := range names {
		pods[n] = provisionRunningAgent(t, n, "wc-halt")
		expectDriftReason(t, n, kaalmv1beta1.ReasonPodCurrent)
	}

	// A bad edit: the first replacement crash-loops.
	setClassPodLabel(t, "wc-halt", "bad")
	holder := expectDriftSplit(t, names, 1)[0]
	waiter := names[0]
	if waiter == holder {
		waiter = names[1]
	}
	bad := expectPodReplaced(t, holder, pods[holder])
	markPodCrashLooping(t, bad)
	expectAgentPhase(t, holder, kaalmv1beta1.AgentFailed)

	// The failed replacement keeps its slot, so the rollout halts.
	holdDriftSplit(t, names, 1)
	expectDriftReason(t, holder, kaalmv1beta1.ReasonReplacing)
	expectDriftReason(t, waiter, kaalmv1beta1.ReasonReplacementPending)

	// The fix replaces the failed Agent at once, on the slot it holds, and
	// the rollout resumes.
	setClassPodLabel(t, "wc-halt", "good")
	fixed := expectPodReplaced(t, holder, bad)
	if fixed.Labels["rollout"] != "good" {
		t.Fatalf("fixed Pod label %q, want good", fixed.Labels["rollout"])
	}
	markPodReady(t, fixed)
	expectDriftReason(t, holder, kaalmv1beta1.ReasonPodCurrent)
	expectDriftReason(t, waiter, kaalmv1beta1.ReasonReplacing)
	next := expectPodReplaced(t, waiter, pods[waiter])
	markPodReady(t, next)
	expectDriftReason(t, waiter, kaalmv1beta1.ReasonPodCurrent)
}

func TestAgent_DriftReplacesCrashLoopingPod(t *testing.T) {
	mkWorkloadClass(t, "wc-crash-drift", nil)
	pod := provisionRunningAgent(t, "crash-drift-agent", "wc-crash-drift")
	markPodCrashLooping(t, pod)
	expectAgentPhase(t, "crash-drift-agent", kaalmv1beta1.AgentFailed)

	// A spec change replaces the crash-looping Pod even while its container
	// sits in back-off.
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "crash-drift-agent")
		ag.Spec.Env = []corev1.EnvVar{{Name: "FIX", Value: "1"}}
		return testClient.Update(ctxT(), ag)
	})
	fresh := expectPodReplaced(t, "crash-drift-agent", pod)
	markPodReady(t, fresh)
	expectAgentPhase(t, "crash-drift-agent", kaalmv1beta1.AgentRunning)
}

func TestAgentClass_MaxUnavailableOnDriftValidation(t *testing.T) {
	for i, bad := range []intstr.IntOrString{
		intstr.FromInt32(0), intstr.FromString("0%"), intstr.FromString("150%"), intstr.FromString("abc"),
	} {
		ac := &kaalmv1beta1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wc-mu-bad-%d", i)},
			Spec: kaalmv1beta1.AgentClassSpec{
				Lifecycle: kaalmv1beta1.AgentClassLifecycle{MaxUnavailableOnDrift: &bad},
			},
		}
		err := testClient.Create(ctxT(), ac)
		if err == nil || !strings.Contains(err.Error(), "rule 44") {
			t.Errorf("maxUnavailableOnDrift %v: err = %v, want a rule 44 rejection", bad.String(), err)
		}
	}
	for i, good := range []intstr.IntOrString{intstr.FromInt32(3), intstr.FromString("100%"), intstr.FromString("5%")} {
		ac := &kaalmv1beta1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wc-mu-good-%d", i)},
			Spec: kaalmv1beta1.AgentClassSpec{
				Lifecycle: kaalmv1beta1.AgentClassLifecycle{MaxUnavailableOnDrift: &good},
			},
		}
		if err := testClient.Create(ctxT(), ac); err != nil {
			t.Errorf("maxUnavailableOnDrift %v rejected: %v", good.String(), err)
		}
	}

	mkWorkloadClass(t, "wc-mu-default", nil)
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: "wc-mu-default"}, &ac); err != nil {
			return err
		}
		if got := ac.Spec.Lifecycle.MaxUnavailableOnDrift; got == nil || got.String() != "25%" {
			return fmt.Errorf("default maxUnavailableOnDrift = %v, want 25%%", got)
		}
		return nil
	})
}
