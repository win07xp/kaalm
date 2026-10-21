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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// replacementClock is the fixed reconciler time the unit tests floor to.
var replacementClock = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// convergeReadyPod runs convergePod for agent against a Ready Pod on the
// current spec, with the reconciler clock fixed at replacementClock.
func convergeReadyPod(t *testing.T, agent *kaalmv1beta1.Agent) {
	t.Helper()
	agent.UID = tlsDriftUID
	c := slotClient(t, agent)
	mkReadyPod(t, c, agent, tlsDriftEff, "legacy-tls-0f1e2d3c", nil)
	r := &AgentReconciler{Client: c, Recorder: events.NewFakeRecorder(10), OperatorNamespace: "kaalm-system",
		Clock: func() time.Time { return replacementClock }}
	if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff, "legacy-tls-0f1e2d3c", false); err != nil {
		t.Fatalf("convergePod: %v", err)
	}
}

func lastActivityAt(agent *kaalmv1beta1.Agent, want time.Time) bool {
	return agent.Status.LastActivityTime != nil && agent.Status.LastActivityTime.Time.Equal(want)
}

// A drift-replacement Pod that turns Ready gets a full idleTimeout: the
// message the gateway holds for it is recorded only after delivery.
func TestConvergePod_ReplacementPodReadyFloorsLastActivity(t *testing.T) {
	agent := slotAgent("legacy", kaalmv1beta1.AgentProvisioning, kaalmv1beta1.ReasonReplacing)
	stale := metav1.NewTime(replacementClock.Add(-2 * time.Hour))
	agent.Status.LastActivityTime = &stale

	convergeReadyPod(t, agent)

	if agent.Status.Phase != kaalmv1beta1.AgentRunning {
		t.Errorf("phase = %s, want Running", agent.Status.Phase)
	}
	if cond := podUpToDate(agent); cond == nil || cond.Status != metav1.ConditionTrue ||
		cond.Reason != kaalmv1beta1.ReasonPodCurrent {
		t.Errorf("PodUpToDate = %+v, want True PodCurrent", cond)
	}
	if !lastActivityAt(agent, replacementClock) {
		t.Errorf("lastActivityTime = %v, want the Ready time %v", agent.Status.LastActivityTime, replacementClock)
	}
}

// A woken Pod that turns Ready gets a full idleTimeout from Ready, not from
// the wake, so a slow Pod start does not use up the window.
func TestConvergePod_WokenPodReadyFloorsLastActivity(t *testing.T) {
	agent := slotAgent("legacy", kaalmv1beta1.AgentResuming, "")
	woke := metav1.NewTime(replacementClock.Add(-10 * time.Minute))
	agent.Status.LastActivityTime = &woke

	convergeReadyPod(t, agent)

	if agent.Status.Phase != kaalmv1beta1.AgentRunning {
		t.Errorf("phase = %s, want Running", agent.Status.Phase)
	}
	if !lastActivityAt(agent, replacementClock) {
		t.Errorf("lastActivityTime = %v, want the Ready time %v", agent.Status.LastActivityTime, replacementClock)
	}
}

// A Pod that replaces a disrupted one gets a full idleTimeout from Ready,
// like a drift replacement, and PodUpToDate clears to Current.
func TestConvergePod_DisruptedReplacementPodReadyFloorsLastActivity(t *testing.T) {
	agent := slotAgent("legacy", kaalmv1beta1.AgentProvisioning, kaalmv1beta1.ReasonPodDisrupted)
	stale := metav1.NewTime(replacementClock.Add(-2 * time.Hour))
	agent.Status.LastActivityTime = &stale

	convergeReadyPod(t, agent)

	if agent.Status.Phase != kaalmv1beta1.AgentRunning {
		t.Errorf("phase = %s, want Running", agent.Status.Phase)
	}
	if cond := podUpToDate(agent); cond == nil || cond.Status != metav1.ConditionTrue ||
		cond.Reason != kaalmv1beta1.ReasonPodCurrent {
		t.Errorf("PodUpToDate = %+v, want True PodCurrent", cond)
	}
	if !lastActivityAt(agent, replacementClock) {
		t.Errorf("lastActivityTime = %v, want the Ready time %v", agent.Status.LastActivityTime, replacementClock)
	}
}

// First provisioning, a Pod that is already serving, and a Pod that turns
// Ready again after a readiness flap keep lastActivityTime.
func TestConvergePod_ReadyWithoutReplacementKeepsLastActivity(t *testing.T) {
	earlier := metav1.NewTime(replacementClock.Add(-time.Hour))
	for _, tc := range []struct {
		name  string
		phase kaalmv1beta1.AgentPhase
		last  *metav1.Time
	}{
		{"first provisioning", kaalmv1beta1.AgentProvisioning, nil},
		{"idle", kaalmv1beta1.AgentIdle, &earlier},
		{"running", kaalmv1beta1.AgentRunning, &earlier},
		// A readiness flap: the same Pod turns Ready again, with no
		// replacement marker, so the idle clock keeps running.
		{"readiness flap", kaalmv1beta1.AgentProvisioning, &earlier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := ""
			if tc.name == "readiness flap" {
				reason = kaalmv1beta1.ReasonPodCurrent
			}
			agent := slotAgent("legacy", tc.phase, reason)
			agent.Status.LastActivityTime = tc.last
			convergeReadyPod(t, agent)
			switch {
			case tc.last == nil && agent.Status.LastActivityTime != nil:
				t.Errorf("lastActivityTime = %v, want unset", agent.Status.LastActivityTime)
			case tc.last != nil && !lastActivityAt(agent, tc.last.Time):
				t.Errorf("lastActivityTime = %v, want %v", agent.Status.LastActivityTime, tc.last)
			}
		})
	}
}

// An Agent that was Idle before a spec change gets a full idleTimeout once
// its replacement Pod is Ready, instead of going Idle on the Ready pass while
// a message waits for the new Pod.
func TestAgent_ReplacementPodGetsAFullIdleTimeout(t *testing.T) {
	mkWorkloadClass(t, "wc-repl-act", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		ac.Spec.Lifecycle.HibernationAllowed = true
	})
	provisionRunningAgentWithLifecycle(t, "repl-act", "wc-repl-act", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
		ag.Spec.Lifecycle.HibernationEnabled = true
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: 5 * time.Second}
		ag.Spec.Lifecycle.HibernationDelay = metav1.Duration{Duration: time.Hour}
	})

	// Activity from 30 minutes ago: past idleTimeout, inside hibernationDelay.
	fakeActivity.set([]ReplicaActivity{replicaWith(3*time.Hour, "repl-act", 30*time.Minute)}, 1)
	touchAgent(t, "repl-act")
	expectAgentPhase(t, "repl-act", kaalmv1beta1.AgentIdle)

	old := agentPod(t, "repl-act")
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "repl-act")
		ag.Spec.Env = []corev1.EnvVar{{Name: "ROLL", Value: "1"}}
		return testClient.Update(ctxT(), ag)
	})
	fresh := expectPodReplaced(t, "repl-act", old)

	readyAt := time.Now()
	markPodReady(t, fresh)
	expectAgentPhase(t, "repl-act", kaalmv1beta1.AgentRunning)
	consistently(t, 2*time.Second, func() error {
		if ag := getWorkloadAgent(t, "repl-act"); ag.Status.Phase != kaalmv1beta1.AgentRunning {
			return fmt.Errorf("phase=%s, want Running for idleTimeout from Ready", ag.Status.Phase)
		}
		return nil
	})
	got := getWorkloadAgent(t, "repl-act")
	if got.Status.LastActivityTime == nil || got.Status.LastActivityTime.Time.Before(readyAt.Add(-time.Second)) {
		t.Errorf("lastActivityTime = %v, want at or after the Ready time %v", got.Status.LastActivityTime, readyAt)
	}
}

// expectNewPod waits for the Agent's Pod to be one other than old.
func expectNewPod(t *testing.T, agentName, old string) *corev1.Pod {
	t.Helper()
	var pod *corev1.Pod
	eventually(t, func() error {
		pod = agentPod(t, agentName)
		if pod == nil || pod.Name == old {
			return errString("no replacement pod yet")
		}
		return nil
	})
	return pod
}

// expectRunningFromReady marks fresh Ready and checks the Agent stays
// Running with its Pod kept, and lastActivityTime at or after Ready.
func expectRunningFromReady(t *testing.T, agentName string, fresh *corev1.Pod) {
	t.Helper()
	readyAt := time.Now()
	markPodReady(t, fresh)
	expectAgentPhase(t, agentName, kaalmv1beta1.AgentRunning)
	consistently(t, 2*time.Second, func() error {
		if ag := getWorkloadAgent(t, agentName); ag.Status.Phase != kaalmv1beta1.AgentRunning {
			return fmt.Errorf("phase=%s, want Running for idleTimeout from Ready", ag.Status.Phase)
		}
		if pod := agentPod(t, agentName); pod == nil || pod.Name != fresh.Name {
			return fmt.Errorf("replacement Pod %s was deleted", fresh.Name)
		}
		return nil
	})
	got := getWorkloadAgent(t, agentName)
	if got.Status.LastActivityTime == nil || got.Status.LastActivityTime.Time.Before(readyAt.Add(-time.Second)) {
		t.Errorf("lastActivityTime = %v, want at or after the Ready time %v", got.Status.LastActivityTime, readyAt)
	}
}

// A Pod that replaces one lost to an involuntary disruption gets a full
// idleTimeout from Ready, like a drift replacement: otherwise a long-silent
// Agent goes Idle and hibernates before the gateway delivers the message it
// holds for the new Pod.
func TestAgent_DisruptedPodReplacementGetsAFullIdleTimeout(t *testing.T) {
	mkWorkloadClass(t, "wc-disrupt-act", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		ac.Spec.Lifecycle.HibernationAllowed = true
	})

	t.Run("deleted out of band, silent past hibernation", func(t *testing.T) {
		const name = "disrupt-gone"
		provisionRunningAgentWithLifecycle(t, name, "wc-disrupt-act", func(ag *kaalmv1beta1.Agent) {
			ag.Spec.Persistence.Enabled = true
			ag.Spec.Lifecycle.HibernationEnabled = true
			ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: 5 * time.Second}
			ag.Spec.Lifecycle.HibernationDelay = metav1.Duration{Duration: time.Second}
		})
		old := agentPod(t, name)
		forceDeletePod(t, old)
		fresh := expectNewPod(t, name, old.Name)

		// Last traffic an hour ago: past idleTimeout + hibernationDelay.
		fakeActivity.set([]ReplicaActivity{replicaWith(3*time.Hour, name, time.Hour)}, 1)
		expectRunningFromReady(t, name, fresh)
	})

	t.Run("idle, terminal Pod", func(t *testing.T) {
		const name = "disrupt-term"
		provisionRunningAgentWithLifecycle(t, name, "wc-disrupt-act", func(ag *kaalmv1beta1.Agent) {
			ag.Spec.Persistence.Enabled = true
			ag.Spec.Lifecycle.HibernationEnabled = true
			ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: 5 * time.Second}
			ag.Spec.Lifecycle.HibernationDelay = metav1.Duration{Duration: time.Hour}
		})
		// Activity from 30 minutes ago: past idleTimeout, inside hibernationDelay.
		fakeActivity.set([]ReplicaActivity{replicaWith(3*time.Hour, name, 30*time.Minute)}, 1)
		touchAgent(t, name)
		expectAgentPhase(t, name, kaalmv1beta1.AgentIdle)

		old := agentPod(t, name)
		old.Status.Phase = corev1.PodFailed
		if err := testClient.Status().Update(ctxT(), old); err != nil {
			t.Fatalf("update pod status: %v", err)
		}
		// The reconciler deletes the terminal Pod; finish its termination.
		eventually(t, func() error {
			var got corev1.Pod
			err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: old.Name}, &got)
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !got.DeletionTimestamp.IsZero() {
				forceDeletePod(t, &got)
			}
			return errString("terminal pod still present")
		})
		expectRunningFromReady(t, name, expectNewPod(t, name, old.Name))
	})
}

// disruptedAgent is a "slots" Agent with the fixed UID, so the Pods the tests
// create are owned by it.
func disruptedAgent(phase kaalmv1beta1.AgentPhase, reason string) *kaalmv1beta1.Agent {
	ag := slotAgent("legacy", phase, reason)
	ag.UID = tlsDriftUID
	return ag
}

// convergePod marks an involuntary replacement only once a Pod has been
// Ready (PodUpToDate present), and leaves a drift replacement's Replacing
// alone.
func TestConvergePod_MarksInvoluntaryReplacement(t *testing.T) {
	terminal := func(t *testing.T, c client.Client, agent *kaalmv1beta1.Agent) {
		mkHeldPod(t, c, agent, "legacy-tls-0f1e2d3c", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	}
	terminating := func(t *testing.T, c client.Client, agent *kaalmv1beta1.Agent) {
		mkReadyPod(t, c, agent, tlsDriftEff, "legacy-tls-0f1e2d3c", func(p *corev1.Pod) {
			p.Finalizers = []string{holdFinalizer} // the fake client keeps the Pod terminating
		})
		pods := ownedPodsOf(t, c, agent)
		if err := c.Delete(context.Background(), &pods[0]); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name       string
		phase      kaalmv1beta1.AgentPhase
		reason     string
		pod        func(*testing.T, client.Client, *kaalmv1beta1.Agent)
		wantReason string
	}{
		{"pod gone after Ready", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent, nil, kaalmv1beta1.ReasonPodDisrupted},
		{"first provisioning", kaalmv1beta1.AgentProvisioning, "", nil, ""},
		{"drift replacement", kaalmv1beta1.AgentProvisioning, kaalmv1beta1.ReasonReplacing, nil, kaalmv1beta1.ReasonReplacing},
		{"wake", kaalmv1beta1.AgentResuming, "", nil, ""},
		{"pending drift", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonReplacementPending, nil, kaalmv1beta1.ReasonPodDisrupted},
		{"terminal pod", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent, terminal, kaalmv1beta1.ReasonPodDisrupted},
		{"terminating pod", kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent, terminating, kaalmv1beta1.ReasonPodDisrupted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := disruptedAgent(tc.phase, tc.reason)
			c := slotClient(t, agent)
			if tc.pod != nil {
				tc.pod(t, c, agent)
			}
			r := &AgentReconciler{Client: c, Recorder: events.NewFakeRecorder(10), OperatorNamespace: "kaalm-system"}
			if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
				"legacy-tls-0f1e2d3c", false); err != nil {
				t.Fatalf("convergePod: %v", err)
			}
			if got := podUpToDateReason(agent); got != tc.wantReason {
				t.Errorf("PodUpToDate reason = %q, want %q", got, tc.wantReason)
			}
			if tc.phase != kaalmv1beta1.AgentResuming && agent.Status.Phase != kaalmv1beta1.AgentProvisioning {
				t.Errorf("phase = %s, want Provisioning", agent.Status.Phase)
			}
			if tc.pod == nil {
				if pods := ownedPodsOf(t, c, agent); len(pods) != 1 {
					t.Fatalf("want 1 created Pod, got %d", len(pods))
				}
			}
			if tc.name == "terminal pod" {
				if pods := ownedPodsOf(t, c, agent); len(pods) != 0 {
					t.Errorf("terminal Pod was kept")
				}
			}
			if tc.name == "pod gone after Ready" {
				// Persisted before the create, not only by the end-of-pass write.
				var stored kaalmv1beta1.Agent
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(agent), &stored); err != nil {
					t.Fatal(err)
				}
				if got := podUpToDateReason(&stored); got != kaalmv1beta1.ReasonPodDisrupted {
					t.Errorf("stored PodUpToDate reason = %q, want PodDisrupted", got)
				}
			}
		})
	}
}

// A failed Pod create after the marker is set leaves the marker stored, so
// the retry still floors the activity of the Pod it creates.
func TestConvergePod_PersistsDisruptionBeforeCreate(t *testing.T) {
	agent := disruptedAgent(kaalmv1beta1.AgentRunning, kaalmv1beta1.ReasonPodCurrent)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&kaalmv1beta1.Agent{}).
		WithObjects(agent).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return errors.New("connection refused")
			},
		}).Build()
	r := &AgentReconciler{Client: c, Recorder: events.NewFakeRecorder(10), OperatorNamespace: "kaalm-system"}

	if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
		"legacy-tls-0f1e2d3c", false); err == nil {
		t.Fatal("convergePod returned no error for a failed create")
	}
	var stored kaalmv1beta1.Agent
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(agent), &stored); err != nil {
		t.Fatal(err)
	}
	if cond := podUpToDate(&stored); cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != kaalmv1beta1.ReasonPodDisrupted {
		t.Errorf("stored PodUpToDate = %+v, want False PodDisrupted", cond)
	}
}

// While the Certificate is held, TLS drift on a not-Ready replacement Pod
// keeps PodDisrupted, so the floor still applies when that Pod turns Ready.
func TestConvergePod_CertHeldKeepsDisruptionMarker(t *testing.T) {
	agent := disruptedAgent(kaalmv1beta1.AgentProvisioning, kaalmv1beta1.ReasonPodDisrupted)
	c := slotClient(t, agent)
	mkHeldPod(t, c, agent, "legacy-tls", podNotReady)
	r := &AgentReconciler{Client: c, Recorder: events.NewFakeRecorder(10), OperatorNamespace: "kaalm-system"}

	if _, _, err := r.convergePod(context.Background(), agent, slotClass(nil), tlsDriftEff,
		"legacy-tls-0f1e2d3c", true); err != nil {
		t.Fatal(err)
	}
	if got := podUpToDateReason(agent); got != kaalmv1beta1.ReasonPodDisrupted {
		t.Errorf("PodUpToDate reason = %q, want PodDisrupted", got)
	}
}
