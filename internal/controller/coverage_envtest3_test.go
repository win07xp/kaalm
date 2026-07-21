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
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// ---- isReferenced: Agent and AgentTask branches ----

func TestModelProviderIsReferenced_ByAgentAndTask(t *testing.T) {
	r := &ModelProviderReconciler{Client: testClient}

	// A provider referenced only by an Agent (no class lists it, so the agents
	// branch is the one that reports referenced).
	mkClass(t, "iref-cls")
	mkAgent(t, "iref-agent", "iref-cls", "iref-only-agent-prov")
	eventually(t, func() error {
		ok, err := r.isReferenced(context.Background(), "iref-only-agent-prov")
		if err != nil {
			return err
		}
		if !ok {
			return errString("provider referenced by an Agent should report referenced")
		}
		return nil
	})

	// A provider referenced only by an AgentTask.
	mkWorkloadClass(t, "iref-tcls", nil)
	mkTask(t, "iref-task", "iref-tcls", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Providers = []agentryv1alpha1.AgentProviderReference{
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "iref-task-prov"}},
		}
	})
	eventually(t, func() error {
		ok, err := r.isReferenced(context.Background(), "iref-task-prov")
		if err != nil {
			return err
		}
		if !ok {
			return errString("provider referenced by an AgentTask should report referenced")
		}
		return nil
	})

	// An unreferenced provider.
	if ok, err := r.isReferenced(context.Background(), "nobody-references-me"); err != nil || ok {
		t.Errorf("unreferenced provider: ok=%v err=%v", ok, err)
	}
}

// ---- Agent degradedReasons: provider named in the class but the CR is absent ----

func TestAgent_ProviderMissingDegrades(t *testing.T) {
	mkWorkloadClass(t, "wc-provmiss", func(ac *agentryv1alpha1.AgentClass) {
		ac.Spec.AllowedProviders = []agentryv1alpha1.LocalObjectReference{{Name: "ghost-prov"}}
	})
	mkWorkloadAgent(t, "provmiss-agent", "wc-provmiss", func(ag *agentryv1alpha1.Agent) {
		ag.Spec.Providers = []agentryv1alpha1.AgentProviderReference{
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "ghost-prov"}},
		}
	})
	expectAgentPhase(t, "provmiss-agent", agentryv1alpha1.AgentDegraded)
	expectAgentReadyReason(t, "provmiss-agent", agentryv1alpha1.ReasonClassConstraintViolation)
}

// ---- AgentTask exitCode: Pod succeeds before ever becoming Ready ----

func TestTask_ExitCodeSucceedsBeforeReady(t *testing.T) {
	mkWorkloadClass(t, "tc-early", nil)
	mkTask(t, "t-early", "tc-early", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
	})
	eventually(t, func() error { return markCertReadyErr("t-early") })
	eventually(t, func() error {
		if taskPod(t, "t-early") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	// The container runs to completion before the (never-set) Ready condition.
	pod := taskPod(t, "t-early")
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "agent",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-early", agentryv1alpha1.TaskSucceeded)
}

// ---- AgentTask driveProvisioning: a fatal image error fails immediately ----

func TestTask_InvalidImageNameFailsImmediately(t *testing.T) {
	mkWorkloadClass(t, "tc-badimg", nil)
	mkTask(t, "t-badimg", "tc-badimg", nil)
	eventually(t, func() error { return markCertReadyErr("t-badimg") })
	eventually(t, func() error {
		if taskPod(t, "t-badimg") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	pod := taskPod(t, "t-badimg")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "agent",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "InvalidImageName", Message: "couldn't parse image reference",
		}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-badimg", agentryv1alpha1.TaskFailed)
}

// ---- AgentTask ensureTaskChildren: persistence provisions a task PVC ----

func TestTask_PersistenceProvisionsPVC(t *testing.T) {
	mkWorkloadClass(t, "tc-pvc", func(ac *agentryv1alpha1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
	})
	mkTask(t, "t-pvc", "tc-pvc", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Persistence.Enabled = true
	})
	eventually(t, func() error { return markCertReadyErr("t-pvc") })
	eventually(t, func() error {
		var pvc corev1.PersistentVolumeClaim
		return testClient.Get(ctxT(),
			types.NamespacedName{Namespace: "default", Name: "t-pvc-workspace"}, &pvc)
	})
}

// ---- AgentTask reconcileDelete: a running task's Pod is terminated ----

func TestTask_DeleteTerminatesPod(t *testing.T) {
	mkWorkloadClass(t, "tc-del", nil)
	pod := provisionRunningTask(t, "t-del", "tc-del", nil)

	task := getTask(t, "t-del")
	if err := testClient.Delete(ctxT(), task); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	// The finalizer deletes the Pod; finish its termination (kubelet-less).
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !got.DeletionTimestamp.IsZero() {
			forceDeletePod(t, &got)
		}
		return errString("pod still present")
	})
	// The task then finalizes away.
	eventually(t, func() error {
		var got agentryv1alpha1.AgentTask
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-del"}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errString("task not yet finalized")
	})
}

// ---- AgentChannel reconcileDelete: no finalizer is a no-op ----

func TestChannel_DeleteBeforeFinalizerIsNoop(t *testing.T) {
	// A channel with the deletion timestamp already set but no finalizer must
	// short-circuit reconcileDelete without touching status.
	r := &AgentChannelReconciler{Client: testClient, OperatorNamespace: testSystemNamespace}
	now := metav1.Now()
	ch := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ephemeral", Namespace: "default", DeletionTimestamp: &now},
	}
	res, err := r.reconcileDelete(context.Background(), ch)
	if err != nil || res.RequeueAfter != 0 || res.Requeue {
		t.Errorf("no-finalizer delete should be a clean no-op: res=%+v err=%v", res, err)
	}
}

// ---- GatewayActivityClient: default port and unreachable replicas ----

func TestGatewayActivityClient_DefaultPortUnreachable(t *testing.T) {
	ns := "gw-unreach-ns"
	if err := testClient.Create(ctxT(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	pki := newActivatorPKI(t)
	clientCert := pki.issue(t, "agentry-controller."+ns+".svc.cluster.local")
	certFile, keyFile, caFile := pki.writeFiles(t, clientCert)

	// A gateway Pod with an IP but nothing listening on the default port.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-dead-0", Namespace: ns, Labels: gatewayPodLabels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "gw", Image: "gw:v1"}}},
	}
	if err := testClient.Create(ctxT(), pod); err != nil {
		t.Fatalf("create gateway pod: %v", err)
	}
	pod.Status.PodIP = "127.0.0.1"
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("set pod IP: %v", err)
	}

	// Wait for the Pod IP to reach the informer cache before the first fan-out
	// (the client caches its result for 15s).
	eventually(t, func() error {
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods,
			client.InNamespace(ns), client.MatchingLabels(gatewayPodLabels)); err != nil {
			return err
		}
		for i := range pods.Items {
			if pods.Items[i].Status.PodIP == "127.0.0.1" {
				return nil
			}
		}
		return errString("gateway pod IP not yet in cache")
	})

	// Port 0 exercises the default-gatewayPort branch; the dial then fails.
	g := &GatewayActivityClient{
		Reader: testClient, OperatorNamespace: ns,
		CertFile: certFile, KeyFile: keyFile, CAFile: caFile, Port: 0,
	}
	reachable, total, err := g.NamespaceActivity(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("activity error: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected the pod to be enumerated as a target, got total=%d", total)
	}
	if len(reachable) != 0 {
		t.Fatal("an unreachable replica must not appear in reachable")
	}
}
