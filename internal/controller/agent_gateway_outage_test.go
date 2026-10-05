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
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func gatewayReachableCond(status metav1.ConditionStatus, since time.Time) []metav1.Condition {
	return []metav1.Condition{{
		Type: kaalmv1beta1.ConditionGatewayReachable, Status: status, Reason: "x",
		LastTransitionTime: metav1.NewTime(since),
	}}
}

// While no gateway replica answers, the requeue starts at 30 seconds and
// doubles with each pass up to five minutes. The outage length is read from
// the GatewayReachable condition's lastTransitionTime, so a controller
// restart keeps the backoff. A jitter draw of 0.5 is the unjittered delay.
func TestGatewayOutageRequeue_DoublesToCap(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		conds []metav1.Condition
		want  time.Duration
	}{
		{"no condition", nil, 30 * time.Second},
		{"reachable", gatewayReachableCond(metav1.ConditionTrue, now.Add(-time.Hour)), 30 * time.Second},
		{"first pass", gatewayReachableCond(metav1.ConditionFalse, now), 30 * time.Second},
		{"second pass", gatewayReachableCond(metav1.ConditionFalse, now.Add(-30*time.Second)), time.Minute},
		{"third pass", gatewayReachableCond(metav1.ConditionFalse, now.Add(-90*time.Second)), 2 * time.Minute},
		{"fourth pass", gatewayReachableCond(metav1.ConditionFalse, now.Add(-210*time.Second)), 4 * time.Minute},
		{"capped", gatewayReachableCond(metav1.ConditionFalse, now.Add(-time.Hour)), 5 * time.Minute},
		{"future transition time", gatewayReachableCond(metav1.ConditionFalse, now.Add(time.Hour)), 30 * time.Second},
	}
	for _, tc := range cases {
		if got := gatewayOutageRequeue(tc.conds, now, 0.5); got != tc.want {
			t.Errorf("%s: gatewayOutageRequeue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Jitter spreads the delay by up to 10 percent either way, so Agents that
// lost the gateway together do not requeue together; the cap still holds.
func TestGatewayOutageRequeue_Jitter(t *testing.T) {
	now := time.Now()
	first := gatewayReachableCond(metav1.ConditionFalse, now)
	if got := gatewayOutageRequeue(first, now, 0); got != 27*time.Second {
		t.Errorf("lowest draw: %v, want 27s", got)
	}
	if got := gatewayOutageRequeue(first, now, 0.999999); got <= 32*time.Second || got > 33*time.Second {
		t.Errorf("highest draw: %v, want just under 33s", got)
	}
	capped := gatewayReachableCond(metav1.ConditionFalse, now.Add(-time.Hour))
	if got := gatewayOutageRequeue(capped, now, 0.999999); got != 5*time.Minute {
		t.Errorf("highest draw at the cap: %v, want 5m", got)
	}
	if got := gatewayOutageRequeue(capped, now, 0); got != 270*time.Second {
		t.Errorf("lowest draw at the cap: %v, want 4m30s", got)
	}
}

// stubActivity answers NamespaceActivity with a fixed result and records
// whether the recovery kick dropped its no-data cache entries.
type stubActivity struct {
	reachable []ReplicaActivity
	total     int
	forgot    int
}

func (s *stubActivity) NamespaceActivity(context.Context, string) ([]ReplicaActivity, int, error) {
	return s.reachable, s.total, nil
}

func (s *stubActivity) ForgetUnreachable() { s.forgot++ }

// evaluateActivity stamps GatewayReachable=False with the reconciler's
// clock on the first failed pass and backs off from there on later passes.
func TestEvaluateActivity_GatewayOutageBacksOff(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	defer func(orig func() float64) { outageJitter = orig }(outageJitter)
	outageJitter = func() float64 { return 0.5 }

	r := &AgentReconciler{Activity: &stubActivity{total: 2}, Clock: func() time.Time { return now }}
	eff := effectiveAgentSpec{IdleTimeout: time.Minute}

	agent := &kaalmv1beta1.Agent{Status: kaalmv1beta1.AgentStatus{Phase: kaalmv1beta1.AgentRunning}}
	res := r.evaluateActivity(context.Background(), agent, eff, false)
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("first failed pass: RequeueAfter = %v, want 30s", res.RequeueAfter)
	}
	c := condition(agent.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable)
	if c == nil || c.Status != metav1.ConditionFalse || !c.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("GatewayReachable = %+v, want False since the reconciler clock", c)
	}

	// Ninety seconds into the outage (for example after a controller
	// restart), the next pass waits two minutes.
	agent.Status.Conditions = gatewayReachableCond(metav1.ConditionFalse, now.Add(-90*time.Second))
	res = r.evaluateActivity(context.Background(), agent, eff, false)
	if res.RequeueAfter != 2*time.Minute {
		t.Errorf("90s into the outage: RequeueAfter = %v, want 2m", res.RequeueAfter)
	}
}

func agentWithGateway(name string, phase kaalmv1beta1.AgentPhase, conds []metav1.Condition) *kaalmv1beta1.Agent {
	return &kaalmv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Status:     kaalmv1beta1.AgentStatus{Phase: phase, Conditions: conds},
	}
}

// A gateway Pod turning Ready enqueues each Running or Idle Agent that is
// waiting on the gateway, once, and drops the activity client's cached
// no-data answers so those passes dial the gateway again. A Pod leaving
// Ready enqueues nothing: the Agents find the outage on their next pass.
func TestAgentsWaitingOnGateway(t *testing.T) {
	down := gatewayReachableCond(metav1.ConditionFalse, time.Now())
	up := gatewayReachableCond(metav1.ConditionTrue, time.Now())
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		agentWithGateway("running-down", kaalmv1beta1.AgentRunning, down),
		agentWithGateway("idle-down", kaalmv1beta1.AgentIdle, down),
		agentWithGateway("running-up", kaalmv1beta1.AgentRunning, up),
		agentWithGateway("hibernated-down", kaalmv1beta1.AgentHibernated, down),
		agentWithGateway("running-unknown", kaalmv1beta1.AgentRunning, nil),
	).Build()
	activity := &stubActivity{}
	r := &AgentReconciler{Client: c, Activity: activity, OperatorNamespace: testOperatorNamespace}
	gwLabels := map[string]string{labelKeyComponent: componentGateway}

	reqs := r.agentsWaitingOnGateway(context.Background(), gatewayPod(testOperatorNamespace, gwLabels, true))
	var got []string
	for _, req := range reqs {
		got = append(got, req.Name)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "idle-down" || got[1] != "running-down" {
		t.Errorf("enqueued %v, want [idle-down running-down]", got)
	}
	if activity.forgot != 1 {
		t.Errorf("ForgetUnreachable called %d times, want 1", activity.forgot)
	}

	if reqs := r.agentsWaitingOnGateway(context.Background(),
		gatewayPod(testOperatorNamespace, gwLabels, false)); len(reqs) != 0 {
		t.Errorf("a gateway Pod leaving Ready enqueued %d Agents, want 0", len(reqs))
	}
	if activity.forgot != 1 {
		t.Error("a gateway Pod leaving Ready must not drop the activity cache")
	}
}

// The kick's watch admits a gateway Pod becoming Ready but never a delete,
// whose last-seen state can still read Ready.
func TestGatewayTurnedReady(t *testing.T) {
	p := gatewayTurnedReady(testOperatorNamespace)
	gwLabels := map[string]string{labelKeyComponent: componentGateway}
	ready := gatewayPod(testOperatorNamespace, gwLabels, true)
	notReady := gatewayPod(testOperatorNamespace, gwLabels, false)
	if !p.Update(event.UpdateEvent{ObjectOld: notReady, ObjectNew: ready}) {
		t.Error("a gateway Pod becoming Ready must be admitted")
	}
	if !p.Create(event.CreateEvent{Object: ready}) {
		t.Error("a new gateway Pod must be admitted")
	}
	if p.Delete(event.DeleteEvent{Object: ready}) {
		t.Error("a deleted gateway Pod must not kick the waiting Agents")
	}
	if p.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: ready.DeepCopy()}) {
		t.Error("an update that keeps readiness must be filtered out")
	}
}

// The recovery kick end to end: an Agent waiting on the gateway returns to
// GatewayReachable=True as soon as a gateway Pod turns Ready, well inside
// the outage requeue it would otherwise wait for.
func TestAgent_GatewayRecoveryKick(t *testing.T) {
	mkWorkloadClass(t, "wc-kick", nil)
	provisionRunningAgentWithLifecycle(t, "kick", "wc-kick", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Hour}
	})

	fakeActivity.set(nil, 1)
	touchAgent(t, "kick")
	eventually(t, func() error {
		c := condition(getWorkloadAgent(t, "kick").Status.Conditions, kaalmv1beta1.ConditionGatewayReachable)
		if c == nil || c.Status != metav1.ConditionFalse {
			return errString("GatewayReachable should be False")
		}
		return nil
	})

	// Let the requeues left by the passes that had activity data (15s
	// each) fire, so the only requeue still pending is the outage backoff,
	// 30 seconds or more away. Only the kick can bring the Agent back
	// inside the window below.
	time.Sleep(activityCacheWindow + 2*time.Second)

	fakeActivity.set([]ReplicaActivity{replicaWith(2*time.Hour, "kick", 0)}, 1)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kick-gw", Namespace: testSystemNamespace,
			Labels: map[string]string{labelKeyComponent: componentGateway},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "gw", Image: "gw:v1"}}},
	}
	if err := testClient.Create(ctxT(), pod); err != nil {
		t.Fatalf("create gateway pod: %v", err)
	}
	defer func() { _ = testClient.Delete(context.Background(), pod) }()
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("mark gateway pod Ready: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		c := condition(getWorkloadAgent(t, "kick").Status.Conditions, kaalmv1beta1.ConditionGatewayReachable)
		if c != nil && c.Status == metav1.ConditionTrue {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("GatewayReachable did not return to True within 10s of a gateway Pod turning Ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The kick skips Agents the activity step does not evaluate: an Agent with
// idle detection off, and every Agent when the controller runs without an
// activity client. Their passes would not dial the gateway.
func TestAgentsWaitingOnGateway_SkipsAgentsWithoutActivityStep(t *testing.T) {
	down := gatewayReachableCond(metav1.ConditionFalse, time.Now())
	idleOff := append(gatewayReachableCond(metav1.ConditionFalse, time.Now()), metav1.Condition{
		Type: kaalmv1beta1.ConditionIdleDetection, Status: metav1.ConditionFalse,
		Reason: kaalmv1beta1.ReasonIdleDetectionDisabled,
	})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		agentWithGateway("running-down", kaalmv1beta1.AgentRunning, down),
		agentWithGateway("idle-off", kaalmv1beta1.AgentRunning, idleOff),
	).Build()
	gwLabels := map[string]string{labelKeyComponent: componentGateway}
	ready := gatewayPod(testOperatorNamespace, gwLabels, true)

	r := &AgentReconciler{Client: c, Activity: &stubActivity{}, OperatorNamespace: testOperatorNamespace}
	reqs := r.agentsWaitingOnGateway(context.Background(), ready)
	if len(reqs) != 1 || reqs[0].Name != "running-down" {
		t.Errorf("enqueued %v, want only running-down", reqs)
	}

	r.Activity = nil
	if reqs := r.agentsWaitingOnGateway(context.Background(), ready); len(reqs) != 0 {
		t.Errorf("without an activity client the kick enqueued %v, want none", reqs)
	}
}

// GatewayReachable describes the activity step, so it exists only while
// the step runs: Running or Idle, an effective idleTimeout above zero, and
// an activity client.
func TestSyncGatewayReachable_RemovesWhenStepDoesNotRun(t *testing.T) {
	down := gatewayReachableCond(metav1.ConditionFalse, time.Now())
	cases := []struct {
		name     string
		activity ActivityClient
		phase    kaalmv1beta1.AgentPhase
		idle     time.Duration
		keep     bool
	}{
		{"running", &stubActivity{}, kaalmv1beta1.AgentRunning, time.Hour, true},
		{"idle", &stubActivity{}, kaalmv1beta1.AgentIdle, time.Hour, true},
		{"idle timeout zero", &stubActivity{}, kaalmv1beta1.AgentRunning, 0, false},
		{"no activity client", nil, kaalmv1beta1.AgentRunning, time.Hour, false},
		{"provisioning", &stubActivity{}, kaalmv1beta1.AgentProvisioning, time.Hour, false},
		{"hibernated", &stubActivity{}, kaalmv1beta1.AgentHibernated, time.Hour, false},
		{"degraded", &stubActivity{}, kaalmv1beta1.AgentDegraded, time.Hour, false},
	}
	for _, tc := range cases {
		r := &AgentReconciler{Activity: tc.activity}
		agent := agentWithGateway("a", tc.phase, append([]metav1.Condition(nil), down...))
		runs := r.activityStepRuns(agent, tc.idle)
		if runs != tc.keep {
			t.Errorf("%s: activityStepRuns = %v, want %v", tc.name, runs, tc.keep)
		}
		r.dropGatewayReachableUnlessEvaluated(agent, tc.idle)
		if got := condition(agent.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable) != nil; got != tc.keep {
			t.Errorf("%s: GatewayReachable present = %v, want %v", tc.name, got, tc.keep)
		}
	}
}

// End to end: an Agent left with GatewayReachable=False loses the
// condition once the activity step stops running for it, here because its
// idle timeout drops to zero.
func TestAgent_GatewayReachableRemovedWhenIdleTimeoutDropsToZero(t *testing.T) {
	mkWorkloadClass(t, "wc-gr-zero", nil)
	provisionRunningAgentWithLifecycle(t, "gr-zero", "wc-gr-zero", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Hour}
	})

	fakeActivity.set(nil, 1)
	touchAgent(t, "gr-zero")
	eventually(t, func() error {
		c := condition(getWorkloadAgent(t, "gr-zero").Status.Conditions, kaalmv1beta1.ConditionGatewayReachable)
		if c == nil || c.Status != metav1.ConditionFalse {
			return errString("GatewayReachable should be False")
		}
		return nil
	})

	eventually(t, func() error {
		ag := getWorkloadAgent(t, "gr-zero")
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{}
		return testClient.Update(ctxT(), ag)
	})
	eventually(t, func() error {
		if condition(getWorkloadAgent(t, "gr-zero").Status.Conditions,
			kaalmv1beta1.ConditionGatewayReachable) != nil {
			return errString("GatewayReachable should be removed")
		}
		return nil
	})
}

// End to end: an Agent that leaves Running while the gateway is down loses
// the condition on the same pass, so it no longer reads as waiting on the
// gateway.
func TestAgent_GatewayReachableRemovedWhenAgentLeavesRunning(t *testing.T) {
	mkWorkloadClass(t, "wc-gr-leave", nil)
	provisionRunningAgentWithLifecycle(t, "gr-leave", "wc-gr-leave", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Hour}
	})

	fakeActivity.set(nil, 1)
	touchAgent(t, "gr-leave")
	eventually(t, func() error {
		c := condition(getWorkloadAgent(t, "gr-leave").Status.Conditions, kaalmv1beta1.ConditionGatewayReachable)
		if c == nil || c.Status != metav1.ConditionFalse {
			return errString("GatewayReachable should be False")
		}
		return nil
	})

	forceDeletePod(t, agentPod(t, "gr-leave"))
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "gr-leave")
		if ag.Status.Phase == kaalmv1beta1.AgentRunning {
			return errString("still Running")
		}
		if condition(ag.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable) != nil {
			return errString("GatewayReachable should be removed once the Agent leaves Running")
		}
		return nil
	})
}
