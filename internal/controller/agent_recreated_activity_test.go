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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// The gateway keys activity by namespace and Agent name, so an Agent deleted
// and created again under the same name finds the old Agent's record. The
// record predates the new Agent and must not count: the new Agent's first
// Ready pass keeps it Running, and the next pass does not hibernate it.
func TestEvaluateActivity_RecreatedAgentIgnoresOldActivity(t *testing.T) {
	eff := effectiveAgentSpec{IdleTimeout: time.Hour, HibernationDelay: time.Second, HibernationEnabled: true}
	for _, source := range []string{"gatewayTraffic", "agentHeartbeat", "both"} {
		for _, tc := range []struct {
			name       string
			createdAgo time.Duration
			wantFirst  kaalmv1beta1.AgentPhase
			wantSecond kaalmv1beta1.AgentPhase
		}{
			{"recreated", time.Minute, kaalmv1beta1.AgentRunning, kaalmv1beta1.AgentRunning},
			{"not recreated", 3 * time.Hour, kaalmv1beta1.AgentIdle, kaalmv1beta1.AgentHibernating},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				old := time.Now().Add(-2 * time.Hour)
				var rec AgentActivity
				if source != "agentHeartbeat" {
					rec.GatewayTraffic = &old
				}
				if source != "gatewayTraffic" {
					rec.Heartbeat = &old
				}
				eff := eff
				eff.ActivitySource = source
				// No Clock: setPhase stamps phaseTransitionTime with the wall clock.
				r := &AgentReconciler{Activity: &stubActivity{
					reachable: []ReplicaActivity{{
						StartedAt: time.Now().Add(-3 * time.Hour),
						Agents:    map[string]AgentActivity{"recreated": rec},
					}},
					total: 1,
				}}
				ready := metav1.Now()
				agent := &kaalmv1beta1.Agent{
					ObjectMeta: metav1.ObjectMeta{
						Name: "recreated", Namespace: "default", UID: types.UID("new-uid"),
						CreationTimestamp: metav1.NewTime(time.Now().Add(-tc.createdAgo)),
					},
					Status: kaalmv1beta1.AgentStatus{
						Phase: kaalmv1beta1.AgentRunning, PhaseTransitionTime: &ready,
					},
				}

				first := r.evaluateActivity(context.Background(), agent, eff, false)
				if agent.Status.Phase != tc.wantFirst {
					t.Fatalf("first pass: phase = %s, want %s (lastActivityTime %v)",
						agent.Status.Phase, tc.wantFirst, agent.Status.LastActivityTime)
				}
				second := r.evaluateActivity(context.Background(), agent, eff, false)
				if agent.Status.Phase != tc.wantSecond {
					t.Fatalf("second pass: phase = %s, want %s", agent.Status.Phase, tc.wantSecond)
				}
				if tc.wantSecond != kaalmv1beta1.AgentRunning {
					return
				}
				if agent.Status.LastActivityTime != nil {
					t.Errorf("lastActivityTime = %v, want unset", agent.Status.LastActivityTime)
				}
				for i, res := range []ctrl.Result{first, second} {
					if res.RequeueAfter != activityCacheWindow {
						t.Errorf("pass %d: result %+v, want RequeueAfter %s", i+1, res, activityCacheWindow)
					}
				}
			})
		}
	}
}

// A recreated Agent (seeded here with the record its deleted namesake left in
// the gateway) stays Running and keeps its Pod after its first Ready pass. A
// real delete and create is not used: envtest runs no garbage collector, so
// the old Agent's children would keep its UID and block the new Agent.
func TestAgent_RecreatedAgentGetsAFullIdleTimeout(t *testing.T) {
	mkWorkloadClass(t, "wc-recreated", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		ac.Spec.Lifecycle.HibernationAllowed = true
	})
	stale := replicaWith(3*time.Hour, "recreated", 2*time.Hour)
	oldRecord := *stale.Agents["recreated"].GatewayTraffic
	fakeActivity.set([]ReplicaActivity{stale}, 1)

	mkWorkloadAgent(t, "recreated", "wc-recreated", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Persistence.Enabled = true
		ag.Spec.Lifecycle.HibernationEnabled = true
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Hour}
		ag.Spec.Lifecycle.HibernationDelay = metav1.Duration{Duration: time.Second}
	})
	markCertReady(t, "recreated")
	eventually(t, func() error {
		if agentPod(t, "recreated") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	pod := agentPod(t, "recreated")
	markPodReady(t, pod)
	eventually(t, func() error {
		switch p := getWorkloadAgent(t, "recreated").Status.Phase; p {
		case "", kaalmv1beta1.AgentPending, kaalmv1beta1.AgentProvisioning:
			return errString(fmt.Sprintf("phase=%s, waiting for the Ready pass", p))
		}
		return nil
	})
	consistently(t, 3*time.Second, func() error {
		ag := getWorkloadAgent(t, "recreated")
		if ag.Status.Phase != kaalmv1beta1.AgentRunning {
			return fmt.Errorf("phase=%s lastActivityTime=%v, want Running for idleTimeout from Ready",
				ag.Status.Phase, ag.Status.LastActivityTime)
		}
		if ag.Status.LastActivityTime != nil && !ag.Status.LastActivityTime.After(oldRecord) {
			return fmt.Errorf("lastActivityTime=%v comes from the old Agent's record", ag.Status.LastActivityTime)
		}
		if cur := agentPod(t, "recreated"); cur == nil || cur.UID != pod.UID {
			return errString("the first Pod was deleted")
		}
		return nil
	})
}
