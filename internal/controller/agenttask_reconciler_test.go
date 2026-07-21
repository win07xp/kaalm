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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func mkTask(t *testing.T, name, className string, mutate func(*agentryv1alpha1.AgentTask)) {
	t.Helper()
	task := &agentryv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agentryv1alpha1.AgentTaskSpec{
			AgentClassRef: agentryv1alpha1.LocalObjectReference{Name: className},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	if mutate != nil {
		mutate(task)
	}
	if err := testClient.Create(ctxT(), task); err != nil {
		t.Fatalf("create task %s: %v", name, err)
	}
}

func getTask(t *testing.T, name string) *agentryv1alpha1.AgentTask {
	t.Helper()
	var task agentryv1alpha1.AgentTask
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &task); err != nil {
		t.Fatalf("get task %s: %v", name, err)
	}
	return &task
}

func taskPod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := testClient.List(ctxT(), &pods, client.InNamespace("default"),
		client.MatchingLabels(map[string]string{"agentry.io/task": name})); err != nil {
		t.Fatalf("list task pods: %v", err)
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp.IsZero() {
			return &pods.Items[i]
		}
	}
	return nil
}

func expectTaskPhase(t *testing.T, name string, phase agentryv1alpha1.AgentTaskPhase) {
	t.Helper()
	eventually(t, func() error {
		var task agentryv1alpha1.AgentTask
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &task); err != nil {
			return err
		}
		if task.Status.Phase != phase {
			return errString("phase=" + string(task.Status.Phase) + " want " + string(phase))
		}
		return nil
	})
}

// writeMailbox plays the gateway: it patches the completion ConfigMap.
func writeMailbox(t *testing.T, taskName string, data map[string]string) {
	t.Helper()
	eventually(t, func() error {
		var cm corev1.ConfigMap
		key := types.NamespacedName{Namespace: "default", Name: taskName + "-completion"}
		if err := testClient.Get(ctxT(), key, &cm); err != nil {
			return err
		}
		cm.Data = data
		return testClient.Update(ctxT(), &cm)
	})
}

// provisionRunningTask drives a task to Running and returns its Pod.
func provisionRunningTask(t *testing.T, name, className string, mutate func(*agentryv1alpha1.AgentTask)) *corev1.Pod {
	t.Helper()
	mkTask(t, name, className, mutate)
	eventually(t, func() error { return markCertReadyErr(name) })
	eventually(t, func() error {
		if taskPod(t, name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
	pod := taskPod(t, name)
	markPodReady(t, pod)
	expectTaskPhase(t, name, agentryv1alpha1.TaskRunning)
	return taskPod(t, name)
}

// ---- Provisioning ----

func TestTask_ProvisionToRunning_AgentReported(t *testing.T) {
	mkWorkloadClass(t, "tc-run", nil)
	mkTask(t, "t-run", "tc-run", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Artifacts = []agentryv1alpha1.AgentTaskArtifact{{Name: "out"}}
	})

	// Certificate gates the Pod.
	eventually(t, func() error { return markCertReadyErr("t-run") })
	eventually(t, func() error {
		if taskPod(t, "t-run") == nil {
			return errString("no pod yet")
		}
		return nil
	})

	// Mailbox, Role, and RoleBinding pre-created; Pod shaped per contract.
	var cm corev1.ConfigMap
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "t-run-completion"}, &cm); err != nil {
		t.Fatalf("completion mailbox missing: %v", err)
	}
	var role rbacv1.Role
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "agentry-task-t-run-completion"}, &role); err != nil {
		t.Fatalf("completion Role missing: %v", err)
	}
	var rb rbacv1.RoleBinding
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "agentry-task-t-run-completion"}, &rb); err != nil {
		t.Fatalf("completion RoleBinding missing: %v", err)
	}
	pod := taskPod(t, "t-run")
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("task pod must be restartPolicy Never")
	}
	if pod.Spec.Containers[0].ReadinessProbe != nil {
		t.Error("task pod must carry no probes")
	}

	// UID stamped before Running.
	eventually(t, func() error {
		task := getTask(t, "t-run")
		if task.Status.CurrentPodUID != string(pod.UID) {
			return errString("currentPodUID not stamped")
		}
		return nil
	})

	markPodReady(t, pod)
	expectTaskPhase(t, "t-run", agentryv1alpha1.TaskRunning)
	if task := getTask(t, "t-run"); task.Status.StartTime == nil {
		t.Error("startTime not stamped on Running")
	}
}

func TestTask_SystemNamespaceForbidden(t *testing.T) {
	mkWorkloadClass(t, "tc-sys", nil)
	task := &agentryv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t-sys", Namespace: testSystemNamespace},
		Spec: agentryv1alpha1.AgentTaskSpec{
			AgentClassRef: agentryv1alpha1.LocalObjectReference{Name: "tc-sys"},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	if err := testClient.Create(ctxT(), task); err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, func() error {
		var got agentryv1alpha1.AgentTask
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Namespace: testSystemNamespace, Name: "t-sys"}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, agentryv1alpha1.ConditionReady)
		if c == nil || c.Reason != agentryv1alpha1.ReasonSystemNamespaceForbidden {
			return errString("SystemNamespaceForbidden not set")
		}
		return nil
	})
}

func TestTask_PersistenceNotAllowedIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-per", nil) // persistence disabled
	mkTask(t, "t-per", "tc-per", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Persistence.Enabled = true
	})
	expectTaskPhase(t, "t-per", agentryv1alpha1.TaskFailed)
	task := getTask(t, "t-per")
	if task.Status.CompletionTime == nil {
		t.Error("terminal Failed must stamp completionTime")
	}
	c := condition(task.Status.Conditions, agentryv1alpha1.ConditionCompleted)
	if c == nil || c.Reason != agentryv1alpha1.ReasonPersistenceNotAllowed {
		t.Errorf("Completed condition wrong: %+v", c)
	}
	if taskPod(t, "t-per") != nil {
		t.Error("no Pod may be created for an irreconcilable task")
	}
}

// ---- agentReported completion ----

func TestTask_AgentReportedSuccessCollectsArtifacts(t *testing.T) {
	mkWorkloadClass(t, "tc-ok", nil)
	provisionRunningTask(t, "t-ok", "tc-ok", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Artifacts = []agentryv1alpha1.AgentTaskArtifact{{Name: "pr-url"}}
	})
	writeMailbox(t, "t-ok", map[string]string{
		"status":          "success",
		"message":         "PR opened",
		"artifact.pr-url": "https://example.com/pr/1",
	})
	expectTaskPhase(t, "t-ok", agentryv1alpha1.TaskSucceeded)
	task := getTask(t, "t-ok")
	if task.Status.ArtifactValues["pr-url"] != "https://example.com/pr/1" {
		t.Errorf("artifacts not collected: %v", task.Status.ArtifactValues)
	}
	if task.Status.AgentReportedStatus != "success" || task.Status.AgentReportedMessage != "PR opened" {
		t.Errorf("reported fields wrong: %+v", task.Status)
	}
	c := condition(task.Status.Conditions, agentryv1alpha1.ConditionCompleted)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Completed should be True: %+v", c)
	}
}

func TestTask_UndeclaredArtifactFails(t *testing.T) {
	mkWorkloadClass(t, "tc-art", nil)
	provisionRunningTask(t, "t-art", "tc-art", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Artifacts = []agentryv1alpha1.AgentTaskArtifact{{Name: "out"}}
	})
	writeMailbox(t, "t-art", map[string]string{
		"status":         "success",
		"artifact.out":   "x",
		"artifact.rogue": "y",
	})
	expectTaskPhase(t, "t-art", agentryv1alpha1.TaskFailed)
}

func TestTask_AgentReportedFailureRetriesThenFails(t *testing.T) {
	mkWorkloadClass(t, "tc-retry", nil)
	oldPod := provisionRunningTask(t, "t-retry", "tc-retry", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Completion.BackoffLimit = 1
	})
	writeMailbox(t, "t-retry", map[string]string{"status": "failure", "message": "boom"})

	// Retry: counter moves, mailbox resets, old Pod is replaced.
	eventually(t, func() error {
		task := getTask(t, "t-retry")
		if task.Status.Retries != 1 {
			return errString("retries not incremented")
		}
		return nil
	})
	// Finish the old Pod's graceful termination (kubelet-less envtest).
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: oldPod.Name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !got.DeletionTimestamp.IsZero() {
			forceDeletePod(t, &got)
		}
		return errString("old pod still present")
	})
	// Mailbox reset and a new Pod with a re-stamped UID.
	eventually(t, func() error {
		var cm corev1.ConfigMap
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Namespace: "default", Name: "t-retry-completion"}, &cm); err != nil {
			return err
		}
		if len(cm.Data) != 0 {
			return errString("mailbox not reset")
		}
		newPod := taskPod(t, "t-retry")
		if newPod == nil || newPod.Name == oldPod.Name {
			return errString("no replacement pod yet")
		}
		task := getTask(t, "t-retry")
		if task.Status.CurrentPodUID != string(newPod.UID) {
			return errString("UID not re-stamped to the new pod")
		}
		return nil
	})

	// Second failure exhausts backoffLimit: terminal Failed.
	markPodReady(t, taskPod(t, "t-retry"))
	expectTaskPhase(t, "t-retry", agentryv1alpha1.TaskRunning)
	writeMailbox(t, "t-retry", map[string]string{"status": "failure", "message": "boom again"})
	expectTaskPhase(t, "t-retry", agentryv1alpha1.TaskFailed)
	task := getTask(t, "t-retry")
	if task.Status.CompletionTime == nil {
		t.Error("terminal Failed must stamp completionTime")
	}
}

// ---- Pod-loss precedence ----

func TestTask_PodLossCompletionWins(t *testing.T) {
	mkWorkloadClass(t, "tc-race", nil)
	pod := provisionRunningTask(t, "t-race", "tc-race", nil)
	// Completion lands, then the Pod is lost before the reconciler settles.
	writeMailbox(t, "t-race", map[string]string{"status": "success"})
	forceDeletePod(t, pod)
	expectTaskPhase(t, "t-race", agentryv1alpha1.TaskSucceeded)
}

func TestTask_PodLossEmptyMailboxFails(t *testing.T) {
	mkWorkloadClass(t, "tc-loss", nil)
	pod := provisionRunningTask(t, "t-loss", "tc-loss", nil)
	forceDeletePod(t, pod)
	expectTaskPhase(t, "t-loss", agentryv1alpha1.TaskFailed)
}

// ---- exitCode mode ----

func TestTask_ExitCodeSuccessAndNoMailbox(t *testing.T) {
	mkWorkloadClass(t, "tc-exit", nil)
	pod := provisionRunningTask(t, "t-exit", "tc-exit", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
	})

	// exitCode tasks get no mailbox and no per-task RBAC.
	var cm corev1.ConfigMap
	err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-exit-completion"}, &cm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("exitCode task must not get a completion mailbox: %v", err)
	}
	var role rbacv1.Role
	err = testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "agentry-task-t-exit-completion"}, &role)
	if !apierrors.IsNotFound(err) {
		t.Errorf("exitCode task must not get a completion Role: %v", err)
	}
	if uid := getTask(t, "t-exit").Status.CurrentPodUID; uid != "" {
		t.Errorf("exitCode task must not stamp currentPodUID, got %q", uid)
	}

	// Container exits 0.
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "agent",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-exit", agentryv1alpha1.TaskSucceeded)
}

func TestTask_ExitCodeNonZeroFails(t *testing.T) {
	mkWorkloadClass(t, "tc-exit2", nil)
	pod := provisionRunningTask(t, "t-exit2", "tc-exit2", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
	})
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "agent",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-exit2", agentryv1alpha1.TaskFailed)
	c := condition(getTask(t, "t-exit2").Status.Conditions, agentryv1alpha1.ConditionCompleted)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("Completed should be False: %+v", c)
	}
}

// ---- Timeouts ----

func TestTask_TimeoutToTimedOut(t *testing.T) {
	mkWorkloadClass(t, "tc-to", nil)
	provisionRunningTask(t, "t-to", "tc-to", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Completion.Timeout = metav1.Duration{Duration: time.Second}
	})
	expectTaskPhase(t, "t-to", agentryv1alpha1.TaskTimedOut)
	// TimedOut is exempt from retries.
	if r := getTask(t, "t-to").Status.Retries; r != 0 {
		t.Errorf("timeout must not consume retries, got %d", r)
	}
}

func TestTask_TimeoutOnTimeoutSucceed(t *testing.T) {
	mkWorkloadClass(t, "tc-tos", nil)
	provisionRunningTask(t, "t-tos", "tc-tos", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Completion.Timeout = metav1.Duration{Duration: time.Second}
		task.Spec.Completion.OnTimeout = "Succeed"
	})
	expectTaskPhase(t, "t-tos", agentryv1alpha1.TaskSucceeded)
}

// ---- TTL ----

func TestTask_TTLDeletesFinishedTask(t *testing.T) {
	mkWorkloadClass(t, "tc-ttl", nil)
	// A few seconds of TTL leaves a reliable margin to observe the Succeeded
	// phase before the task is reaped, without slowing the suite noticeably.
	ttl := int32(3)
	pod := provisionRunningTask(t, "t-ttl", "tc-ttl", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
		task.Spec.TTLSecondsAfterFinished = &ttl
	})
	pod.Status.Phase = corev1.PodSucceeded
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-ttl", agentryv1alpha1.TaskSucceeded)

	// TTL fires and the finalizer needs the Pod's termination finished.
	eventually(t, func() error {
		var got agentryv1alpha1.AgentTask
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-ttl"}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var pods corev1.PodList
		if err := testClient.List(ctxT(), &pods, client.InNamespace("default"),
			client.MatchingLabels(map[string]string{"agentry.io/task": "t-ttl"})); err != nil {
			return err
		}
		for i := range pods.Items {
			if !pods.Items[i].DeletionTimestamp.IsZero() {
				forceDeletePod(t, &pods.Items[i])
			}
		}
		return errString("task not yet TTL-deleted")
	})
}
