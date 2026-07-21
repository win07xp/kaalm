/*
Copyright 2026.

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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// replicaWith builds one gateway replica's activity view for a single agent.
func replicaWith(startedAgo time.Duration, agentName string, trafficAgo time.Duration) ReplicaActivity {
	traffic := time.Now().Add(-trafficAgo)
	return ReplicaActivity{
		StartedAt: time.Now().Add(-startedAgo),
		Agents:    map[string]AgentActivity{agentName: {GatewayTraffic: &traffic}},
	}
}

func TestAgent_IdleTransitionAndReturn(t *testing.T) {
	mkWorkloadClass(t, "wc-idle", nil)
	provisionRunningAgentWithLifecycle(t, "idle-agent", "wc-idle", func(ag *kaalmv1alpha1.Agent) {
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Second}
	})

	// Stale activity: last traffic an hour ago, replica up for two.
	fakeActivity.set([]ReplicaActivity{replicaWith(2*time.Hour, "idle-agent", time.Hour)}, 1)
	touchAgent(t, "idle-agent") // trigger a reconcile pass
	expectAgentPhase(t, "idle-agent", kaalmv1alpha1.AgentIdle)
	ag := getWorkloadAgent(t, "idle-agent")
	if ag.Status.LastActivityTime == nil {
		t.Error("lastActivityTime must be written on the Idle transition")
	}
	if agentPod(t, "idle-agent") == nil {
		t.Error("Idle is a pod-bearing phase; the Pod must survive")
	}

	// Fresh activity: back to Running.
	fakeActivity.set([]ReplicaActivity{replicaWith(2*time.Hour, "idle-agent", 0)}, 1)
	touchAgent(t, "idle-agent")
	expectAgentPhase(t, "idle-agent", kaalmv1alpha1.AgentRunning)
}

func TestAgent_HibernateAndWake(t *testing.T) {
	mkWorkloadClass(t, "wc-hibwake", func(ac *kaalmv1alpha1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
		ac.Spec.Lifecycle.HibernationAllowed = true
	})
	provisionRunningAgentWithLifecycle(t, "hib-wake", "wc-hibwake", func(ag *kaalmv1alpha1.Agent) {
		ag.Spec.Persistence.Enabled = true
		ag.Spec.Lifecycle.HibernationEnabled = true
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Second}
		ag.Spec.Lifecycle.HibernationDelay = metav1.Duration{Duration: time.Second}
	})

	// Long-stale activity drives Idle then Hibernating in successive passes.
	fakeActivity.set([]ReplicaActivity{replicaWith(3*time.Hour, "hib-wake", 2*time.Hour)}, 1)
	touchAgent(t, "hib-wake")

	// The reconciler deletes the Pod; finish its termination (no kubelet).
	eventually(t, func() error {
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods, listAgentPods("hib-wake")...); err != nil {
			return err
		}
		for i := range pods.Items {
			if !pods.Items[i].DeletionTimestamp.IsZero() {
				forceDeletePod(t, &pods.Items[i])
			}
		}
		var ag kaalmv1alpha1.Agent
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "hib-wake"}, &ag); err != nil {
			return err
		}
		if ag.Status.Phase != kaalmv1alpha1.AgentHibernated {
			return errString(fmt.Sprintf("phase=%s want Hibernated (hibEnabled=%v idle=%s delay=%s lastAct=%v ready=%+v)",
				ag.Status.Phase, ag.Spec.Lifecycle.HibernationEnabled,
				ag.Spec.Lifecycle.IdleTimeout.Duration, ag.Spec.Lifecycle.HibernationDelay.Duration,
				ag.Status.LastActivityTime, condition(ag.Status.Conditions, kaalmv1alpha1.ConditionReady)))
		}
		return nil
	})

	// Hibernated: no Pod, but PVC, Service, and Certificate survive.
	if agentPod(t, "hib-wake") != nil {
		t.Error("Hibernated must have no Pod")
	}
	var pvc corev1.PersistentVolumeClaim
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "hib-wake-memory"}, &pvc); err != nil {
		t.Errorf("PVC must survive hibernation: %v", err)
	}
	var svc corev1.Service
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "hib-wake"}, &svc); err != nil {
		t.Errorf("Service must survive hibernation: %v", err)
	}
	ag := getWorkloadAgent(t, "hib-wake")
	if ag.Status.HibernatedAt == nil {
		t.Error("hibernatedAt must be stamped")
	}

	// Wake via annotation (what the activator writes).
	eventually(t, func() error {
		got := getWorkloadAgent(t, "hib-wake")
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations[kaalmv1alpha1.AnnotationWake] = kaalmv1alpha1.AnnotationTrue
		return testClient.Update(ctxT(), got)
	})
	// Fresh activity so the woken agent does not immediately re-idle.
	fakeActivity.set([]ReplicaActivity{replicaWith(3*time.Hour, "hib-wake", 0)}, 1)

	eventually(t, func() error {
		if agentPod(t, "hib-wake") == nil {
			return errString("no recreated pod yet")
		}
		return nil
	})
	markPodReady(t, agentPod(t, "hib-wake"))
	expectAgentPhase(t, "hib-wake", kaalmv1alpha1.AgentRunning)

	got := getWorkloadAgent(t, "hib-wake")
	if _, still := got.Annotations[kaalmv1alpha1.AnnotationWake]; still {
		t.Error("wake annotation must be removed after the wake commits")
	}
	if got.Status.HibernatedAt != nil {
		t.Error("hibernatedAt must clear on wake")
	}
}

func TestAgent_WakeIgnoredOnRunning(t *testing.T) {
	mkWorkloadClass(t, "wc-wignore", nil)
	provisionRunningAgentWithLifecycle(t, "wignore", "wc-wignore", nil)

	eventually(t, func() error {
		ag := getWorkloadAgent(t, "wignore")
		if ag.Annotations == nil {
			ag.Annotations = map[string]string{}
		}
		ag.Annotations[kaalmv1alpha1.AnnotationWake] = kaalmv1alpha1.AnnotationTrue
		return testClient.Update(ctxT(), ag)
	})
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "wignore")
		if _, still := ag.Annotations[kaalmv1alpha1.AnnotationWake]; still {
			return errString("annotation not yet removed")
		}
		if ag.Status.Phase != kaalmv1alpha1.AgentRunning {
			return errString("phase changed on an ignored wake")
		}
		return nil
	})
}

func TestAgent_GatewayUnreachableDefersIdle(t *testing.T) {
	mkWorkloadClass(t, "wc-unreach", nil)
	provisionRunningAgentWithLifecycle(t, "unreach", "wc-unreach", func(ag *kaalmv1alpha1.Agent) {
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Second}
	})

	// All replicas unreachable: phase preserved, GatewayReachable=False.
	fakeActivity.set(nil, 2)
	touchAgent(t, "unreach")
	eventually(t, func() error {
		ag := getWorkloadAgent(t, "unreach")
		c := condition(ag.Status.Conditions, kaalmv1alpha1.ConditionGatewayReachable)
		if c == nil || c.Status != metav1.ConditionFalse {
			return errString("GatewayReachable should be False")
		}
		if ag.Status.Phase != kaalmv1alpha1.AgentRunning {
			return errString("phase must be preserved without activity data")
		}
		return nil
	})
}

func TestAgent_GatewayRestartDefersIdle(t *testing.T) {
	mkWorkloadClass(t, "wc-restart", nil)
	provisionRunningAgentWithLifecycle(t, "restarted", "wc-restart", func(ag *kaalmv1alpha1.Agent) {
		ag.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: time.Hour}
	})

	// A freshly restarted replica with an empty store: silence is unknown,
	// because no replica has been up for idleTimeout.
	fakeActivity.set([]ReplicaActivity{{StartedAt: time.Now(), Agents: map[string]AgentActivity{}}}, 1)
	touchAgent(t, "restarted")
	// The agent must stay Running despite zero recorded activity.
	time.Sleep(time.Second)
	if got := getWorkloadAgent(t, "restarted"); got.Status.Phase != kaalmv1alpha1.AgentRunning {
		t.Errorf("restart-unknown data must defer idle transitions, phase=%s", got.Status.Phase)
	}
}

// ---- helpers ----

// provisionRunningAgentWithLifecycle mirrors provisionRunningAgent but takes
// a mutator, so lifecycle fields land before creation.
func provisionRunningAgentWithLifecycle(t *testing.T, name, className string, mutate func(*kaalmv1alpha1.Agent)) {
	t.Helper()
	// Fresh activity by default so provisioning is not raced by idle logic.
	fakeActivity.set([]ReplicaActivity{replicaWith(2*time.Hour, name, 0)}, 1)
	mkWorkloadAgent(t, name, className, mutate)
	markCertReady(t, name)
	eventually(t, func() error {
		if agentPod(t, name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
	markPodReady(t, agentPod(t, name))
	expectAgentPhase(t, name, kaalmv1alpha1.AgentRunning)
}

// touchAgent bumps an annotation to force a reconcile pass.
func touchAgent(t *testing.T, name string) {
	t.Helper()
	eventually(t, func() error {
		ag := getWorkloadAgent(t, name)
		if ag.Annotations == nil {
			ag.Annotations = map[string]string{}
		}
		ag.Annotations["test.kaalm.io/touch"] = time.Now().Format(time.RFC3339Nano)
		return testClient.Update(ctxT(), ag)
	})
}

func listAgentPods(name string) []client.ListOption {
	return []client.ListOption{
		client.InNamespace("default"),
		client.MatchingLabels(map[string]string{"kaalm.io/agent": name}),
	}
}
