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
	"strings"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

func mkTask(t *testing.T, name, className string, mutate func(*kaalmv1beta1.AgentTask)) {
	t.Helper()
	task := &kaalmv1beta1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kaalmv1beta1.AgentTaskSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: className},
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

func getTask(t *testing.T, name string) *kaalmv1beta1.AgentTask {
	t.Helper()
	var task kaalmv1beta1.AgentTask
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &task); err != nil {
		t.Fatalf("get task %s: %v", name, err)
	}
	return &task
}

func taskPod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := testAPIReader.List(ctxT(), &pods, client.InNamespace("default"),
		client.MatchingLabels(map[string]string{"kaalm.io/task": name})); err != nil {
		t.Fatalf("list task pods: %v", err)
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp.IsZero() {
			return &pods.Items[i]
		}
	}
	return nil
}

func expectTaskPhase(t *testing.T, name string, phase kaalmv1beta1.AgentTaskPhase) {
	t.Helper()
	eventually(t, func() error {
		var task kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &task); err != nil {
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
		if err := testAPIReader.Get(ctxT(), key, &cm); err != nil {
			return err
		}
		cm.Data = data
		return testClient.Update(ctxT(), &cm)
	})
}

// provisionRunningTask drives a task to Running and returns its Pod.
func provisionRunningTask(t *testing.T, name, className string, mutate func(*kaalmv1beta1.AgentTask)) *corev1.Pod {
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
	// StartTime is written in the same status update that sets Running and
	// is never set anywhere else, so it is the durable witness of the
	// transition: a task with a short completion timeout can settle before a
	// poll samples the Running phase on a loaded machine.
	eventually(t, func() error {
		var got kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &got); err != nil {
			return err
		}
		if got.Status.Phase == kaalmv1beta1.TaskRunning || got.Status.StartTime != nil {
			return nil
		}
		return errString("phase=" + string(got.Status.Phase) + ", startTime unset: not yet Running")
	})
	return taskPod(t, name)
}

// ---- Provisioning ----

func TestTask_ProvisionToRunning_AgentReported(t *testing.T) {
	mkWorkloadClass(t, "tc-run", nil)
	mkTask(t, "t-run", "tc-run", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Artifacts = []kaalmv1beta1.AgentTaskArtifact{{Name: "out"}}
	})

	// The Certificate holds the Pod until it is Ready.
	eventually(t, func() error { return markCertReadyErr("t-run") })
	eventually(t, func() error {
		if taskPod(t, "t-run") == nil {
			return errString("no pod yet")
		}
		return nil
	})

	// Mailbox, Role, and RoleBinding pre-created; Pod shaped per contract.
	var cm corev1.ConfigMap
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "t-run-completion"}, &cm); err != nil {
		t.Fatalf("completion mailbox missing: %v", err)
	}
	var role rbacv1.Role
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "kaalm-task-t-run-completion"}, &role); err != nil {
		t.Fatalf("completion Role missing: %v", err)
	}
	var rb rbacv1.RoleBinding
	if err := testAPIReader.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "kaalm-task-t-run-completion"}, &rb); err != nil {
		t.Fatalf("completion RoleBinding missing: %v", err)
	}
	pod := taskPod(t, "t-run")
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("task pod must be restartPolicy Never")
	}
	if pod.Spec.Containers[0].ReadinessProbe != nil {
		t.Error("task pod must carry no probes")
	}
	var cert cmapi.Certificate
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-run-tls"}, &cert); err != nil {
		t.Fatalf("get certificate: %v", err)
	}
	wantSecret := "t-run-tls-" + string(getTask(t, "t-run").UID)[:8]
	if cert.Spec.SecretName != wantSecret {
		t.Errorf("certificate secretName = %q, want %q", cert.Spec.SecretName, wantSecret)
	}
	if got := tlsSecretOf(pod); got != wantSecret {
		t.Errorf("pod TLS volume names %q, want %q", got, wantSecret)
	}

	// UID set before Running.
	eventually(t, func() error {
		task := getTask(t, "t-run")
		if task.Status.CurrentPodUID != string(pod.UID) {
			return errString("currentPodUID not set")
		}
		return nil
	})

	markPodReady(t, pod)
	expectTaskPhase(t, "t-run", kaalmv1beta1.TaskRunning)
	if task := getTask(t, "t-run"); task.Status.StartTime == nil {
		t.Error("startTime not set on Running")
	}
}

func TestTask_SystemNamespaceForbidden(t *testing.T) {
	mkWorkloadClass(t, "tc-sys", nil)
	task := &kaalmv1beta1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t-sys", Namespace: testSystemNamespace},
		Spec: kaalmv1beta1.AgentTaskSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "tc-sys"},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	if err := testClient.Create(ctxT(), task); err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, func() error {
		var got kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(),
			types.NamespacedName{Namespace: testSystemNamespace, Name: "t-sys"}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != kaalmv1beta1.ReasonSystemNamespaceForbidden {
			return errString("SystemNamespaceForbidden not set")
		}
		return nil
	})
}

func TestTask_PersistenceNotAllowedIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-per", nil) // persistence disabled
	mkTask(t, "t-per", "tc-per", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Persistence.Enabled = true
	})
	expectTaskPhase(t, "t-per", kaalmv1beta1.TaskFailed)
	task := getTask(t, "t-per")
	if task.Status.CompletionTime == nil {
		t.Error("terminal Failed must set completionTime")
	}
	c := condition(task.Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Reason != kaalmv1beta1.ReasonPersistenceNotAllowed {
		t.Errorf("Completed condition wrong: %+v", c)
	}
	if taskPod(t, "t-per") != nil {
		t.Error("no Pod may be created for an irreconcilable task")
	}
}

// ---- agentReported completion ----

func TestTask_AgentReportedSuccessCollectsArtifacts(t *testing.T) {
	mkWorkloadClass(t, "tc-ok", nil)
	provisionRunningTask(t, "t-ok", "tc-ok", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Artifacts = []kaalmv1beta1.AgentTaskArtifact{{Name: "pr-url"}}
	})
	writeMailbox(t, "t-ok", map[string]string{
		"status":          "success",
		"message":         "PR opened",
		"artifact.pr-url": "https://example.com/pr/1",
	})
	expectTaskPhase(t, "t-ok", kaalmv1beta1.TaskSucceeded)
	task := getTask(t, "t-ok")
	if task.Status.ArtifactValues["pr-url"] != "https://example.com/pr/1" {
		t.Errorf("artifacts not collected: %v", task.Status.ArtifactValues)
	}
	if task.Status.AgentReportedStatus != "success" || task.Status.AgentReportedMessage != "PR opened" {
		t.Errorf("reported fields wrong: %+v", task.Status)
	}
	c := condition(task.Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Completed should be True: %+v", c)
	}
}

func TestTask_UndeclaredArtifactFails(t *testing.T) {
	mkWorkloadClass(t, "tc-art", nil)
	provisionRunningTask(t, "t-art", "tc-art", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Artifacts = []kaalmv1beta1.AgentTaskArtifact{{Name: "out"}}
	})
	writeMailbox(t, "t-art", map[string]string{
		"status":         "success",
		"artifact.out":   "x",
		"artifact.rogue": "y",
	})
	expectTaskPhase(t, "t-art", kaalmv1beta1.TaskFailed)
}

func TestTask_AgentReportedFailureRetriesThenFails(t *testing.T) {
	mkWorkloadClass(t, "tc-retry", nil)
	oldPod := provisionRunningTask(t, "t-retry", "tc-retry", func(task *kaalmv1beta1.AgentTask) {
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
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: oldPod.Name}, &got)
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
	// Mailbox reset and a new Pod with its UID recorded.
	eventually(t, func() error {
		var cm corev1.ConfigMap
		if err := testAPIReader.Get(ctxT(),
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
			return errString("UID not rewritten to the new pod")
		}
		return nil
	})

	// Second failure exhausts backoffLimit: terminal Failed.
	markPodReady(t, taskPod(t, "t-retry"))
	expectTaskPhase(t, "t-retry", kaalmv1beta1.TaskRunning)
	writeMailbox(t, "t-retry", map[string]string{"status": "failure", "message": "boom again"})
	expectTaskPhase(t, "t-retry", kaalmv1beta1.TaskFailed)
	task := getTask(t, "t-retry")
	if task.Status.CompletionTime == nil {
		t.Error("terminal Failed must set completionTime")
	}
}

// ---- Pod-loss precedence ----

func TestTask_PodLossCompletionWins(t *testing.T) {
	mkWorkloadClass(t, "tc-race", nil)
	pod := provisionRunningTask(t, "t-race", "tc-race", nil)
	// Completion lands, then the Pod is lost before the reconciler settles.
	writeMailbox(t, "t-race", map[string]string{"status": "success"})
	forceDeletePod(t, pod)
	expectTaskPhase(t, "t-race", kaalmv1beta1.TaskSucceeded)
}

func TestTask_PodLossEmptyMailboxFails(t *testing.T) {
	mkWorkloadClass(t, "tc-loss", nil)
	pod := provisionRunningTask(t, "t-loss", "tc-loss", nil)
	forceDeletePod(t, pod)
	expectTaskPhase(t, "t-loss", kaalmv1beta1.TaskFailed)
}

// ---- exitCode mode ----

func TestTask_ExitCodeSuccessAndNoMailbox(t *testing.T) {
	mkWorkloadClass(t, "tc-exit", nil)
	pod := provisionRunningTask(t, "t-exit", "tc-exit", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
	})

	// exitCode tasks get no mailbox and no per-task RBAC.
	var cm corev1.ConfigMap
	err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-exit-completion"}, &cm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("exitCode task must not get a completion mailbox: %v", err)
	}
	var role rbacv1.Role
	err = testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "kaalm-task-t-exit-completion"}, &role)
	if !apierrors.IsNotFound(err) {
		t.Errorf("exitCode task must not get a completion Role: %v", err)
	}
	if uid := getTask(t, "t-exit").Status.CurrentPodUID; uid != "" {
		t.Errorf("exitCode task must not set currentPodUID, got %q", uid)
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
	expectTaskPhase(t, "t-exit", kaalmv1beta1.TaskSucceeded)
}

func TestTask_ExitCodeNonZeroFails(t *testing.T) {
	mkWorkloadClass(t, "tc-exit2", nil)
	pod := provisionRunningTask(t, "t-exit2", "tc-exit2", func(task *kaalmv1beta1.AgentTask) {
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
	expectTaskPhase(t, "t-exit2", kaalmv1beta1.TaskFailed)
	c := condition(getTask(t, "t-exit2").Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("Completed should be False: %+v", c)
	}
}

// ---- Timeouts ----

func TestTask_TimeoutToTimedOut(t *testing.T) {
	mkWorkloadClass(t, "tc-to", nil)
	provisionRunningTask(t, "t-to", "tc-to", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.Timeout = metav1.Duration{Duration: time.Second}
	})
	expectTaskPhase(t, "t-to", kaalmv1beta1.TaskTimedOut)
	// TimedOut is exempt from retries.
	if r := getTask(t, "t-to").Status.Retries; r != 0 {
		t.Errorf("timeout must not consume retries, got %d", r)
	}
}

func TestTask_TimeoutOnTimeoutSucceed(t *testing.T) {
	mkWorkloadClass(t, "tc-tos", nil)
	provisionRunningTask(t, "t-tos", "tc-tos", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.Timeout = metav1.Duration{Duration: time.Second}
		task.Spec.Completion.OnTimeout = "Succeed"
	})
	expectTaskPhase(t, "t-tos", kaalmv1beta1.TaskSucceeded)
}

func TestTask_ClassDefaultTimeoutTimesOut(t *testing.T) {
	mkWorkloadClass(t, "tc-cto", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTaskTimeout = metav1.Duration{Duration: time.Second}
	})
	provisionRunningTask(t, "t-cto", "tc-cto", nil)
	expectTaskPhase(t, "t-cto", kaalmv1beta1.TaskTimedOut)
	c := condition(getTask(t, "t-cto").Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Reason != "TimeoutExceeded" || !strings.Contains(c.Message, "1s") {
		t.Errorf("Completed should name the class default timeout: %+v", c)
	}
}

func TestTask_TimeoutClampedToClassMax(t *testing.T) {
	mkWorkloadClass(t, "tc-mto", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.MaxTaskTimeout = metav1.Duration{Duration: time.Second}
	})
	provisionRunningTask(t, "t-mto", "tc-mto", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.Timeout = metav1.Duration{Duration: time.Hour}
	})
	expectTaskPhase(t, "t-mto", kaalmv1beta1.TaskTimedOut)
}

// ---- TTL ----

func TestTask_TTLDeletesFinishedTask(t *testing.T) {
	mkWorkloadClass(t, "tc-ttl", nil)
	// A few seconds of TTL leaves a reliable margin to observe the Succeeded
	// phase before the task is reaped, without slowing the suite noticeably.
	ttl := int32(3)
	pod := provisionRunningTask(t, "t-ttl", "tc-ttl", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
		task.Spec.TTLSecondsAfterFinished = &ttl
	})
	pod.Status.Phase = corev1.PodSucceeded
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-ttl", kaalmv1beta1.TaskSucceeded)
	expectTaskTTLDeleted(t, "t-ttl")
}

// expectTaskTTLDeleted waits for the TTL to delete the task. The finalizer
// needs the Pod's termination finished, which envtest never does on its own.
func expectTaskTTLDeleted(t *testing.T, name string) {
	t.Helper()
	eventually(t, func() error {
		var got kaalmv1beta1.AgentTask
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var pods corev1.PodList
		if err := testAPIReader.List(ctxT(), &pods, client.InNamespace("default"),
			client.MatchingLabels(map[string]string{"kaalm.io/task": name})); err != nil {
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

func TestTask_ClassDefaultTTLDeletesFinishedTask(t *testing.T) {
	ttl := int32(3)
	mkWorkloadClass(t, "tc-cttl", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTTLSecondsAfterFinished = &ttl
	})
	pod := provisionRunningTask(t, "t-cttl", "tc-cttl", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
	})
	pod.Status.Phase = corev1.PodSucceeded
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-cttl", kaalmv1beta1.TaskSucceeded)
	expectTaskTTLDeleted(t, "t-cttl")
}

// ---- Class snapshot of the effective timeout and TTL ----

// editClass applies mutate to the named class, retrying on conflict.
func editClass(t *testing.T, name string, mutate func(*kaalmv1beta1.AgentClass)) {
	t.Helper()
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: name}, &ac); err != nil {
			return err
		}
		mutate(&ac)
		return testClient.Update(ctxT(), &ac)
	})
}

// holdTaskPhase asserts the task stays in phase for d, giving a class edit
// time to reach the reconciler.
func holdTaskPhase(t *testing.T, name string, phase kaalmv1beta1.AgentTaskPhase, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		var got kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &got); err != nil {
			t.Fatalf("task %s: %v", name, err)
		}
		if got.Status.Phase != phase {
			t.Fatalf("task %s left %s for %s", name, phase, got.Status.Phase)
		}
	}
}

func TestTask_RecordsClassBoundsAtPodCreation(t *testing.T) {
	ttl, maxTTL := int32(600), int32(300)
	mkWorkloadClass(t, "tc-bounds", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTaskTimeout = metav1.Duration{Duration: time.Hour}
		c.Spec.Lifecycle.MaxTaskTimeout = metav1.Duration{Duration: 2 * time.Hour}
		c.Spec.Lifecycle.DefaultTTLSecondsAfterFinished = &ttl
		c.Spec.Lifecycle.MaxTTLSecondsAfterFinished = &maxTTL
	})
	provisionRunningTask(t, "t-bounds", "tc-bounds", nil)
	b := getTask(t, "t-bounds").Status.ClassBounds
	if b == nil || b.DefaultTaskTimeout.Duration != time.Hour || b.MaxTaskTimeout.Duration != 2*time.Hour ||
		b.DefaultTTLSecondsAfterFinished == nil || *b.DefaultTTLSecondsAfterFinished != 600 ||
		b.MaxTTLSecondsAfterFinished == nil || *b.MaxTTLSecondsAfterFinished != 300 {
		t.Errorf("classBounds = %+v, want the class's four task bounds", b)
	}
}

func TestTask_ClassTimeoutEditSkipsRunningTask(t *testing.T) {
	mkWorkloadClass(t, "tc-snapto", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTaskTimeout = metav1.Duration{Duration: time.Hour}
	})
	provisionRunningTask(t, "t-snapto", "tc-snapto", nil)
	expectTaskPhase(t, "t-snapto", kaalmv1beta1.TaskRunning)
	editClass(t, "tc-snapto", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTaskTimeout = metav1.Duration{Duration: time.Second}
		c.Spec.Lifecycle.MaxTaskTimeout = metav1.Duration{Duration: time.Second}
	})
	holdTaskPhase(t, "t-snapto", kaalmv1beta1.TaskRunning, 3*time.Second)
	if b := getTask(t, "t-snapto").Status.ClassBounds; b == nil || b.DefaultTaskTimeout.Duration != time.Hour {
		t.Errorf("classBounds = %+v, want the 1h snapshot", b)
	}
}

// The owner's own edit applies at once, within the bounds captured at Pod
// creation.
func TestTask_OwnerTimeoutEditAppliesToRunningTask(t *testing.T) {
	mkWorkloadClass(t, "tc-ownto", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTaskTimeout = metav1.Duration{Duration: time.Hour}
	})
	provisionRunningTask(t, "t-ownto", "tc-ownto", nil)
	eventually(t, func() error {
		task := getTask(t, "t-ownto")
		task.Spec.Completion.Timeout = metav1.Duration{Duration: time.Second}
		return testClient.Update(ctxT(), task)
	})
	expectTaskPhase(t, "t-ownto", kaalmv1beta1.TaskTimedOut)
}

func TestTask_ClassTTLEditSkipsFinishedTask(t *testing.T) {
	mkWorkloadClass(t, "tc-snapttl", nil)
	pod := provisionRunningTask(t, "t-snapttl", "tc-snapttl", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
	})
	pod.Status.Phase = corev1.PodSucceeded
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-snapttl", kaalmv1beta1.TaskSucceeded)
	ttl := int32(0)
	editClass(t, "tc-snapttl", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTTLSecondsAfterFinished = &ttl
	})
	holdTaskPhase(t, "t-snapttl", kaalmv1beta1.TaskSucceeded, 3*time.Second)
}

func TestTask_RetryRecordsBoundsFromEditedClass(t *testing.T) {
	mkWorkloadClass(t, "tc-rebounds", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTaskTimeout = metav1.Duration{Duration: time.Hour}
	})
	oldPod := provisionRunningTask(t, "t-rebounds", "tc-rebounds", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.BackoffLimit = 1
	})
	editClass(t, "tc-rebounds", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTaskTimeout = metav1.Duration{Duration: 2 * time.Hour}
	})
	writeMailbox(t, "t-rebounds", map[string]string{"status": "failure", "message": "boom"})
	eventually(t, func() error {
		var got corev1.Pod
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: oldPod.Name}, &got)
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
	eventually(t, func() error {
		newPod := taskPod(t, "t-rebounds")
		if newPod == nil || newPod.Name == oldPod.Name {
			return errString("no replacement pod yet")
		}
		if b := getTask(t, "t-rebounds").Status.ClassBounds; b == nil || b.DefaultTaskTimeout.Duration != 2*time.Hour {
			return errString("classBounds not rewritten to the 2h default")
		}
		return nil
	})
}

// A task that settles before any Pod exists records the class bounds in the
// settling write, so the class default TTL still cleans it up.
func TestTask_PrePodFailureRecordsBoundsAndExpires(t *testing.T) {
	ttl := int32(2)
	mkWorkloadClass(t, "tc-prepod", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTTLSecondsAfterFinished = &ttl
	})
	mkTask(t, "t-prepod", "tc-prepod", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Image = "evil.example/not-allowed:v1"
	})
	eventually(t, func() error {
		var got kaalmv1beta1.AgentTask
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-prepod"}, &got)
		if apierrors.IsNotFound(err) {
			return nil // already expired; the recorded bounds are checked by the deletion
		}
		if err != nil {
			return err
		}
		if got.Status.Phase != kaalmv1beta1.TaskFailed {
			return errString("not yet Failed")
		}
		if b := got.Status.ClassBounds; b == nil || b.DefaultTTLSecondsAfterFinished == nil ||
			*b.DefaultTTLSecondsAfterFinished != 2 {
			t.Fatalf("classBounds = %+v, want the class's 2s default TTL", b)
		}
		return nil
	})
	expectTaskTTLDeleted(t, "t-prepod")
}

// A task that settled before classBounds existed is never settled again, so
// it keeps no classBounds and a class default TTL never reaches it.
func TestTask_PreUpgradeSettledTaskIsKept(t *testing.T) {
	mkTask(t, "t-preupg", "tc-preupg", nil) // class absent: InvalidReference, no Pod
	eventually(t, func() error {
		var got kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-preupg"}, &got); err != nil {
			return err
		}
		done := metav1.NewTime(time.Now().Add(-time.Hour))
		got.Status.Phase = kaalmv1beta1.TaskFailed
		got.Status.CompletionTime = &done
		got.Status.ClassBounds = nil
		return testClient.Status().Update(ctxT(), &got)
	})
	ttl := int32(0)
	mkWorkloadClass(t, "tc-preupg", func(c *kaalmv1beta1.AgentClass) {
		c.Spec.Lifecycle.DefaultTTLSecondsAfterFinished = &ttl
	})
	holdTaskPhase(t, "t-preupg", kaalmv1beta1.TaskFailed, 3*time.Second)
	if b := getTask(t, "t-preupg").Status.ClassBounds; b != nil {
		t.Errorf("a pre-upgrade settled task must keep no classBounds, got %+v", b)
	}
}

// The timeout is derived every pass from the current spec and the recorded
// bounds; nil bounds (a task that predates them) leave the spec unbounded.
func TestTimedOut_SpecWithinRecordedBounds(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-time.Minute))
	task := &kaalmv1beta1.AgentTask{Status: kaalmv1beta1.AgentTaskStatus{StartTime: &started}}
	if timedOut(task) || runningRequeue(task).RequeueAfter != 0 {
		t.Error("nil bounds and no spec timeout: must not time out or requeue")
	}
	task.Spec.Completion.Timeout = metav1.Duration{Duration: time.Second}
	if !timedOut(task) {
		t.Error("nil bounds: the spec timeout must apply")
	}
	task.Spec.Completion.Timeout = metav1.Duration{Duration: 5 * time.Hour}
	if timedOut(task) || runningRequeue(task).RequeueAfter < 4*time.Hour {
		t.Error("nil bounds: the spec timeout must apply unclamped")
	}

	task.Status.ClassBounds = &kaalmv1beta1.AgentTaskClassBounds{
		DefaultTaskTimeout: metav1.Duration{Duration: 30 * time.Second},
		MaxTaskTimeout:     metav1.Duration{Duration: 2 * time.Minute},
	}
	// The owner raised the timeout to 5h: clamped to the recorded 2m max.
	if timedOut(task) {
		t.Error("a minute into a 2m clamped timeout must not time out")
	}
	if got := runningRequeue(task).RequeueAfter; got > time.Minute+time.Second {
		t.Errorf("requeue %v must follow the clamped 2m timeout", got)
	}
	// With no spec value, the recorded default applies.
	task.Spec.Completion.Timeout = metav1.Duration{}
	if !timedOut(task) {
		t.Error("a minute past the recorded 30s default must time out")
	}
}

func TestHandleTTL_SpecWithinRecordedBounds(t *testing.T) {
	ctx := context.Background()
	i32 := func(v int32) *int32 { return &v }
	settled := func(name string, spec *int32, b *kaalmv1beta1.AgentTaskClassBounds) *kaalmv1beta1.AgentTask {
		done := metav1.NewTime(time.Now().Add(-time.Minute))
		return &kaalmv1beta1.AgentTask{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: kaalmv1beta1.AgentTaskSpec{
				AgentClassRef:           kaalmv1beta1.LocalObjectReference{Name: "ttl-now"},
				TTLSecondsAfterFinished: spec,
			},
			Status: kaalmv1beta1.AgentTaskStatus{
				Phase: kaalmv1beta1.TaskSucceeded, CompletionTime: &done, ClassBounds: b,
			},
		}
	}
	// The class now sets a zero TTL; no task may read it.
	class := &kaalmv1beta1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ttl-now"},
		Spec: kaalmv1beta1.AgentClassSpec{Lifecycle: kaalmv1beta1.AgentClassLifecycle{
			DefaultTTLSecondsAfterFinished: i32(0),
		}},
	}
	recordedDefault := &kaalmv1beta1.AgentTaskClassBounds{DefaultTTLSecondsAfterFinished: i32(30)}
	recordedMax := &kaalmv1beta1.AgentTaskClassBounds{MaxTTLSecondsAfterFinished: i32(30)}
	kept := settled("kept", nil, nil)
	ownSpec := settled("own-spec", i32(30), nil)
	byDefault := settled("by-default", nil, recordedDefault)
	raised := settled("raised", i32(3600), recordedDefault)
	clamped := settled("clamped", i32(3600), recordedMax)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(class, kept, ownSpec, byDefault, raised, clamped).
		WithStatusSubresource(&kaalmv1beta1.AgentTask{}).Build()
	r := &AgentTaskReconciler{Client: c}
	exists := func(task *kaalmv1beta1.AgentTask) bool {
		return c.Get(ctx, client.ObjectKeyFromObject(task), &kaalmv1beta1.AgentTask{}) == nil
	}

	if res, err := r.handleTTL(ctx, kept); err != nil || res.RequeueAfter != 0 || !exists(kept) {
		t.Errorf("nil bounds, no spec TTL: want kept with no requeue, got %+v, %v", res, err)
	}
	if _, err := r.handleTTL(ctx, ownSpec); err != nil || exists(ownSpec) {
		t.Errorf("nil bounds: the spec TTL of 30s, a minute past, must delete the task (err %v)", err)
	}
	if _, err := r.handleTTL(ctx, byDefault); err != nil || exists(byDefault) {
		t.Errorf("no spec TTL: the recorded 30s default must delete the task (err %v)", err)
	}
	// The owner raised the TTL on a finished task, the S9 escape hatch.
	if res, err := r.handleTTL(ctx, raised); err != nil || res.RequeueAfter <= 0 || !exists(raised) {
		t.Errorf("an owner-raised TTL must win over the recorded default: got %+v, %v", res, err)
	}
	if _, err := r.handleTTL(ctx, clamped); err != nil || exists(clamped) {
		t.Errorf("an owner-raised TTL above the recorded 30s max must be clamped (err %v)", err)
	}
}

// A status write lost after the Pod's creation leaves podName behind; the
// next pass repairs the snapshot from the observed Pod. A Pod created before
// the snapshot fields existed has a matching podName and keeps no classBounds.
func TestDriveProvisioning_RepairsLostSnapshotOnly(t *testing.T) {
	ctx := context.Background()
	eff := effectiveTaskSpec{}
	class := &kaalmv1beta1.AgentClass{Spec: kaalmv1beta1.AgentClassSpec{Lifecycle: kaalmv1beta1.AgentClassLifecycle{
		DefaultTaskTimeout: metav1.Duration{Duration: time.Hour},
	}}}
	for _, tc := range []struct {
		name, recorded string
		wantBounds     bool
	}{
		{"lost-write", "", true},
		{"pre-upgrade", "p-pre-upgrade", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &kaalmv1beta1.AgentTask{
				ObjectMeta: metav1.ObjectMeta{Name: "t-" + tc.name, Namespace: "default"},
				Spec: kaalmv1beta1.AgentTaskSpec{
					Completion: kaalmv1beta1.AgentTaskCompletion{Condition: completionExitCode},
				},
				Status: kaalmv1beta1.AgentTaskStatus{Phase: kaalmv1beta1.TaskProvisioning, PodName: tc.recorded},
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "p-" + tc.name, Namespace: "default", CreationTimestamp: metav1.Now(),
			}}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task, pod).
				WithStatusSubresource(&kaalmv1beta1.AgentTask{}).Build()
			r := &AgentTaskReconciler{Client: c}
			if _, err := r.driveProvisioning(ctx, task, class, eff, pod); err != nil {
				t.Fatalf("driveProvisioning: %v", err)
			}
			var got kaalmv1beta1.AgentTask
			if err := c.Get(ctx, client.ObjectKeyFromObject(task), &got); err != nil {
				t.Fatal(err)
			}
			recorded := got.Status.ClassBounds != nil
			if recorded != tc.wantBounds || got.Status.PodName != pod.Name {
				t.Errorf("classBounds=%+v podName=%q, want recorded=%v podName=%q",
					got.Status.ClassBounds, got.Status.PodName, tc.wantBounds, pod.Name)
			}
		})
	}
}

// ---- AgentTask pre-Pod violations (taskViolation) ----

func TestTask_ImageNotAllowedIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-img", nil) // allows registry.test/agents/*
	mkTask(t, "t-img", "tc-img", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Image = "evil.example/x:v1"
	})
	expectTaskPhase(t, "t-img", kaalmv1beta1.TaskFailed)
	c := condition(getTask(t, "t-img").Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Reason != kaalmv1beta1.ReasonClassConstraintViolation {
		t.Errorf("Completed condition wrong: %+v", c)
	}
}

func TestTask_ProviderNotAllowedIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-prov", nil) // empty allowedProviders => none allowed
	mkTask(t, "t-prov", "tc-prov", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Providers = []kaalmv1beta1.AgentProviderReference{
			{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "nope"}},
		}
	})
	expectTaskPhase(t, "t-prov", kaalmv1beta1.TaskFailed)
}

func TestTask_ProviderNamespaceDeniedIsTerminalFailed(t *testing.T) {
	mkSecret(t, "t-provns-key")
	mkProvider(t, "t-provns", func(mp *kaalmv1beta1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1beta1.SecretKeyReference{Name: "t-provns-key", Key: "token"}
		mp.Spec.AllowedNamespaces = []string{"team-*"}
	})
	mkWorkloadClass(t, "tc-provns", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedProviders = []kaalmv1beta1.LocalObjectReference{{Name: "t-provns"}}
	})
	mkTask(t, "t-provns-task", "tc-provns", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Providers = []kaalmv1beta1.AgentProviderReference{
			{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "t-provns"}},
		}
	})
	// The task's namespace (default) does not match team-*.
	expectTaskPhase(t, "t-provns-task", kaalmv1beta1.TaskFailed)
}

// Rule 47: a class whose allowedNamespaces does not admit the task's
// namespace settles it terminal Failed with NamespaceNotAllowed, no Pod.
func TestTask_ClassNamespaceDeniedIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-ns-deny", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedNamespaces = []string{"team-*"}
	})
	mkTask(t, "t-ns-deny", "tc-ns-deny", nil)
	// The task's namespace (default) does not match team-*.
	expectTaskPhase(t, "t-ns-deny", kaalmv1beta1.TaskFailed)
	task := getTask(t, "t-ns-deny")
	c := condition(task.Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Reason != kaalmv1beta1.ReasonNamespaceNotAllowed {
		t.Errorf("Completed condition wrong: %+v", c)
	}
	if taskPod(t, "t-ns-deny") != nil {
		t.Error("no Pod may be created for a namespace the class does not admit")
	}
}

// Rule 47: a matching pattern and an unset field both admit the namespace.
func TestTask_ClassNamespaceAllowedProvisions(t *testing.T) {
	mkWorkloadClass(t, "tc-ns-exact", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedNamespaces = []string{"default"}
	})
	mkWorkloadClass(t, "tc-ns-unset", nil)
	for _, c := range []struct{ task, class string }{
		{"t-ns-exact", "tc-ns-exact"},
		{"t-ns-unset", "tc-ns-unset"},
	} {
		mkTask(t, c.task, c.class, nil)
		eventually(t, func() error { return markCertReadyErr(c.task) })
		eventually(t, func() error {
			if taskPod(t, c.task) == nil {
				return errString(c.task + ": no pod yet")
			}
			return nil
		})
		if got := getTask(t, c.task).Status.Phase; got == kaalmv1beta1.TaskFailed {
			t.Errorf("%s Failed under class %s", c.task, c.class)
		}
	}
}

func TestTask_ImagePullSecretMissingBlocks(t *testing.T) {
	mkWorkloadClass(t, "tc-pull", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Image.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "tc-pull-creds"}}
	})
	mkTask(t, "t-pull", "tc-pull", nil)
	readyReason := func() (string, error) {
		var task kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-pull"}, &task); err != nil {
			return "", err
		}
		c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil {
			return "", errString("no Ready condition yet")
		}
		return c.Reason, nil
	}
	eventually(t, func() error {
		r, err := readyReason()
		if err != nil {
			return err
		}
		if r != kaalmv1beta1.ReasonImagePullSecretMissing {
			return errString("ImagePullSecretMissing not set, got " + r)
		}
		return nil
	})
	// Creating the Secret clears Ready=False and provisioning proceeds.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tc-pull-creds", Namespace: "default"}}
	if err := testClient.Create(ctxT(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	eventually(t, func() error { return markCertReadyErr("t-pull") })
	eventually(t, func() error {
		r, err := readyReason()
		if err != nil {
			return err
		}
		if r == kaalmv1beta1.ReasonImagePullSecretMissing {
			return errString("still waiting on the pull secret")
		}
		return nil
	})
}

// ---- AgentTask exitCode: Pod succeeds before ever becoming Ready ----

func TestTask_ExitCodeSucceedsBeforeReady(t *testing.T) {
	mkWorkloadClass(t, "tc-early", nil)
	mkTask(t, "t-early", "tc-early", func(task *kaalmv1beta1.AgentTask) {
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
	expectTaskPhase(t, "t-early", kaalmv1beta1.TaskSucceeded)
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
	expectTaskPhase(t, "t-badimg", kaalmv1beta1.TaskFailed)
}

// ---- AgentTask ensureTaskChildren: persistence provisions a task PVC ----

func TestTask_PersistenceProvisionsPVC(t *testing.T) {
	mkWorkloadClass(t, "tc-pvc", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
	})
	mkTask(t, "t-pvc", "tc-pvc", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Persistence.Enabled = true
	})
	eventually(t, func() error { return markCertReadyErr("t-pvc") })
	eventually(t, func() error {
		var pvc corev1.PersistentVolumeClaim
		return testAPIReader.Get(ctxT(),
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
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got)
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
		var got kaalmv1beta1.AgentTask
		err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-del"}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errString("task not yet finalized")
	})
}

// ---- AgentTask: missing class and empty image are terminal or not Ready ----

func TestTask_MissingClassIsNotReady(t *testing.T) {
	mkTask(t, "t-noclass", "ghost-class", nil)
	eventually(t, func() error {
		var task kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-noclass"}, &task); err != nil {
			return err
		}
		c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != kaalmv1beta1.ReasonInvalidReference {
			return errString("InvalidReference not set")
		}
		return nil
	})
}

// A malformed allowedCIDRs entry (rule 19) holds a new task with a
// non-terminal condition, and fixing the class lets it provision.
func TestTask_InvalidClassCIDRBlocksAndRecovers(t *testing.T) {
	mkWorkloadClass(t, "tc-cidr", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Network.Egress.AllowedCIDRs = []string{"not-a-cidr"}
	})
	mkTask(t, "t-cidr", "tc-cidr", nil)
	readyReason := func() (string, kaalmv1beta1.AgentTaskPhase, error) {
		var task kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-cidr"}, &task); err != nil {
			return "", "", err
		}
		c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil {
			return "", task.Status.Phase, errString("no Ready condition yet")
		}
		return c.Reason, task.Status.Phase, nil
	}
	eventually(t, func() error {
		r, phase, err := readyReason()
		if err != nil {
			return err
		}
		if r != kaalmv1beta1.ReasonInvalidReference || phase == kaalmv1beta1.TaskFailed {
			return errString("want non-terminal InvalidReference, got " + r + " in " + string(phase))
		}
		return nil
	})
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Name: "tc-cidr"}, &ac); err != nil {
			return err
		}
		ac.Spec.Network.Egress.AllowedCIDRs = []string{"10.0.0.0/8"}
		return testClient.Update(ctxT(), &ac)
	})
	eventually(t, func() error { return markCertReadyErr("t-cidr") })
	eventually(t, func() error {
		if pod := taskPod(t, "t-cidr"); pod == nil {
			return errString("no task Pod yet")
		}
		return nil
	})
}

func TestTask_EmptyImageIsNotReady(t *testing.T) {
	mkWorkloadClass(t, "tc-noimg", nil) // no defaultImage
	mkTask(t, "t-noimg", "tc-noimg", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Image = ""
	})
	eventually(t, func() error {
		var task kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-noimg"}, &task); err != nil {
			return err
		}
		c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != kaalmv1beta1.ReasonInvalidReference {
			return errString("InvalidReference not set")
		}
		return nil
	})
}

// ---- AgentTask: a provider in the allowlist whose CR is absent fails ----

func TestTask_ProviderCRMissingIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-provghost", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedProviders = []kaalmv1beta1.LocalObjectReference{{Name: "task-ghost-prov"}}
	})
	mkTask(t, "t-provghost", "tc-provghost", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Providers = []kaalmv1beta1.AgentProviderReference{
			{ProviderRef: kaalmv1beta1.LocalObjectReference{Name: "task-ghost-prov"}},
		}
	})
	expectTaskPhase(t, "t-provghost", kaalmv1beta1.TaskFailed)
}

// ---- AgentTask: the provisioning deadline fails a Pod that never starts ----

func TestTask_ProvisioningDeadlineFails(t *testing.T) {
	mkWorkloadClass(t, "tc-deadline", nil)
	mkTask(t, "t-deadline", "tc-deadline", func(task *kaalmv1beta1.AgentTask) {
		withShortDeadline(task)
		task.Spec.Completion.Condition = completionExitCode
	})
	eventually(t, func() error { return markCertReadyErr("t-deadline") })
	eventually(t, func() error {
		if taskPod(t, "t-deadline") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	// A non-fatal waiting reason exercises the container-status scan without
	// short-circuiting; the Pod never becomes Ready, so the deadline fails it.
	pod := taskPod(t, "t-deadline")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "agent",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-deadline", kaalmv1beta1.TaskFailed)
}

// TestTask_ExitCodeFailsBeforeReady covers driveProvisioning's terminal-before-
// Ready branch for an exitCode task whose Pod fails outright.
func TestTask_ExitCodeFailsBeforeReady(t *testing.T) {
	mkWorkloadClass(t, "tc-failearly", nil)
	mkTask(t, "t-failearly", "tc-failearly", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.Condition = completionExitCode
	})
	eventually(t, func() error { return markCertReadyErr("t-failearly") })
	eventually(t, func() error {
		if taskPod(t, "t-failearly") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	pod := taskPod(t, "t-failearly")
	pod.Status.Phase = corev1.PodFailed
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectTaskPhase(t, "t-failearly", kaalmv1beta1.TaskFailed)
}

// TestTask_CrashInterruptedRetryResumes covers the Reconcile entry that
// finishes a counted retry left half done (Failed with no completionTime):
// the old Pod is deleted and a new one created, and the retry is not counted
// again.
func TestTask_CrashInterruptedRetryResumes(t *testing.T) {
	mkWorkloadClass(t, "tc-resume", nil)
	oldPod := provisionRunningTask(t, "t-resume", "tc-resume", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.BackoffLimit = 2
	})
	// A finalizer keeps the old Pod terminating once deleted, so the hold
	// on it can be observed.
	oldPod.Finalizers = append(oldPod.Finalizers, "kaalm.io/test-hold")
	if err := testClient.Update(ctxT(), oldPod); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}

	// The state a retry leaves after its counting write: Failed, not settled,
	// the retry counted, and currentPodUID cleared.
	eventually(t, func() error {
		task := getTask(t, "t-resume")
		task.Status.Phase = kaalmv1beta1.TaskFailed
		task.Status.CompletionTime = nil
		task.Status.Retries = 1
		task.Status.CurrentPodUID = ""
		return testClient.Status().Update(ctxT(), task)
	})
	eventually(t, func() error {
		var got corev1.Pod
		if err := testAPIReader.Get(ctxT(), client.ObjectKeyFromObject(oldPod), &got); err != nil {
			return err
		}
		if got.DeletionTimestamp.IsZero() {
			return errString("old Pod not deleted yet")
		}
		task := getTask(t, "t-resume")
		c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
		if task.Status.Phase != kaalmv1beta1.TaskProvisioning || c == nil || c.Reason != "PodTerminating" {
			return errString("task not holding on the terminating Pod yet")
		}
		if task.Status.Retries != 1 || task.Status.CurrentPodUID != "" {
			return fmt.Errorf("retries=%d currentPodUID=%q, want 1 and empty",
				task.Status.Retries, task.Status.CurrentPodUID)
		}
		return nil
	})

	var got corev1.Pod
	if err := testAPIReader.Get(ctxT(), client.ObjectKeyFromObject(oldPod), &got); err != nil {
		t.Fatal(err)
	}
	got.Finalizers = nil
	if err := testClient.Update(ctxT(), &got); err != nil {
		t.Fatalf("remove finalizer: %v", err)
	}
	eventually(t, func() error {
		newPod := taskPod(t, "t-resume")
		if newPod == nil || newPod.UID == oldPod.UID {
			return errString("no replacement pod yet")
		}
		if r := getTask(t, "t-resume").Status.Retries; r != 1 {
			return fmt.Errorf("retries = %d, want 1", r)
		}
		return nil
	})
}

// TestReadMailbox_NotFoundIsEmpty covers readMailbox's NotFound path directly.
func TestReadMailbox_NotFoundIsEmpty(t *testing.T) {
	r := &AgentTaskReconciler{Client: testClient}
	task := &kaalmv1beta1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "no-mailbox", Namespace: "default"}}
	payload, err := r.readMailbox(ctxT(), task)
	if err != nil {
		t.Fatalf("missing mailbox must not error: %v", err)
	}
	if payload.Status != "" {
		t.Errorf("absent mailbox must parse to an empty payload: %+v", payload)
	}
}

func TestPodExitMessage(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "agent",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
	}}}}
	if got := podExitMessage(pod); got != "container agent exited 3" {
		t.Errorf("podExitMessage = %q", got)
	}
	// No terminated container -> fallback.
	empty := &corev1.Pod{}
	if got := podExitMessage(empty); got != "task Pod failed without a container exit code" {
		t.Errorf("podExitMessage fallback = %q", got)
	}
}

// A retry holds while the old Pod is still terminating: currentPodUID stays
// empty, no backoff unit is spent on the old Pod's terminal state, and
// the replacement is created once it is gone.
func TestTask_RetryHoldsWhileOldPodTerminates(t *testing.T) {
	mkWorkloadClass(t, "tc-hold", nil)
	oldPod := provisionRunningTask(t, "t-hold", "tc-hold", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.BackoffLimit = 1
	})
	// A finalizer keeps the old Pod terminating: the apiserver deletes an
	// unscheduled Pod at once, and kubelet-less envtest never finishes a
	// scheduled one, so this pins the state the test is about.
	oldPod.Finalizers = append(oldPod.Finalizers, "kaalm.io/test-hold")
	if err := testClient.Update(ctxT(), oldPod); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	writeMailbox(t, "t-hold", map[string]string{"status": "failure", "message": "boom"})

	eventually(t, func() error {
		task := getTask(t, "t-hold")
		if task.Status.Retries != 1 {
			return errString("retries not incremented")
		}
		c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
		if c == nil || c.Reason != "PodTerminating" {
			return errString("Ready reason is not PodTerminating yet")
		}
		return nil
	})
	// The old Pod is terminating (kubelet-less envtest never finishes it), and
	// several passes later nothing has moved: no UID rewrite, no second failure,
	// no replacement.
	time.Sleep(2 * time.Second)
	var got corev1.Pod
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: oldPod.Name}, &got); err != nil ||
		got.DeletionTimestamp.IsZero() {
		t.Fatalf("old pod should still be terminating: %v", err)
	}
	task := getTask(t, "t-hold")
	if task.Status.CurrentPodUID != "" {
		t.Errorf("currentPodUID set again for the old Pod: currentPodUID=%q", task.Status.CurrentPodUID)
	}
	if task.Status.Retries != 1 || task.Status.CompletionTime != nil {
		t.Errorf("old Pod's terminal state spent a backoff unit: retries=%d completionTime=%v",
			task.Status.Retries, task.Status.CompletionTime)
	}
	if taskPod(t, "t-hold") != nil {
		t.Error("replacement Pod created while the old one terminates")
	}

	got.Finalizers = nil
	if err := testClient.Update(ctxT(), &got); err != nil {
		t.Fatalf("remove finalizer: %v", err)
	}
	eventually(t, func() error {
		newPod := taskPod(t, "t-hold")
		if newPod == nil || newPod.Name == oldPod.Name {
			return errString("no replacement pod yet")
		}
		task := getTask(t, "t-hold")
		if task.Status.CurrentPodUID != string(newPod.UID) {
			return errString("UID not set to the new pod")
		}
		if task.Status.Retries != 1 {
			return errString("retries moved during the hold")
		}
		return nil
	})
}

// The AgentTask's transition events wait for the status write: a pass that
// loses its write to a conflict emits nothing, and the retry emits once.
func TestAgentTask_EventsFollowTheStatusWrite(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		step   func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error
		steady bool // a third pass over the stored state must stay quiet
	}{{
		name: "ChildConflict", prefix: "Warning " + kaalmv1beta1.ReasonChildConflict, steady: true,
		step: func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error {
			_, err := r.childBlocked(ctxT(), task, nil, false,
				&ChildConflictError{Kind: "Service", Name: task.Name, OwnerKind: "AgentTask"})
			return err
		},
	}, {
		name: "ChildWriteRejected", prefix: "Warning " + kaalmv1beta1.ReasonChildWriteRejected, steady: true,
		step: func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error {
			_, err := r.childBlocked(ctxT(), task, nil, false, &ChildWriteRejectedError{
				Op: "creating", Kind: "NetworkPolicy", Name: task.Name,
				Err: apierrors.NewForbidden(schema.GroupResource{Resource: "networkpolicies"}, task.Name,
					errors.New("denied by policy webhook")),
			})
			return err
		},
	}, {
		name: "a terminal phase", prefix: "Warning TimeoutExceeded",
		step: func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error {
			return r.settle(ctxT(), task, kaalmv1beta1.TaskTimedOut, "TimeoutExceeded", "task exceeded its timeout")
		},
	}, {
		name: "a retry", prefix: "Warning PodFailed",
		step: func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error {
			return r.retry(ctxT(), task, "PodFailed", "container exited 1")
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &kaalmv1beta1.AgentTask{
				ObjectMeta: metav1.ObjectMeta{Name: "ev-task", Namespace: "default"},
				Spec: kaalmv1beta1.AgentTaskSpec{Completion: kaalmv1beta1.AgentTaskCompletion{
					Condition: completionExitCode, BackoffLimit: 3,
				}},
				Status: kaalmv1beta1.AgentTaskStatus{Phase: kaalmv1beta1.TaskRunning},
			}
			conflicts := &statusConflicts{}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task).
				WithStatusSubresource(task).WithInterceptorFuncs(conflicts.funcs()).Build()
			rec := record.NewFakeRecorder(16)
			r := &AgentTaskReconciler{Client: c, Recorder: rec}
			pass := func() error {
				var got kaalmv1beta1.AgentTask
				if err := c.Get(ctxT(), client.ObjectKeyFromObject(task), &got); err != nil {
					t.Fatal(err)
				}
				return tc.step(r, &got)
			}

			conflicts.failNext()
			if err := pass(); !apierrors.IsConflict(err) {
				t.Fatalf("err = %v, want a conflict", err)
			}
			if got := drainEvents(rec); len(got) != 0 {
				t.Fatalf("a pass whose status write failed emitted %q", got)
			}
			if err := pass(); err != nil {
				t.Fatal(err)
			}
			if got := withPrefix(drainEvents(rec), tc.prefix); len(got) != 1 {
				t.Fatalf("the retry emitted %d %q events, want 1", len(got), tc.prefix)
			}
			if !tc.steady {
				return
			}
			if err := pass(); err != nil {
				t.Fatal(err)
			}
			if got := withPrefix(drainEvents(rec), tc.prefix); len(got) != 0 {
				t.Fatalf("a pass over the stored state emitted %q again", got)
			}
		})
	}
}

// A task Certificate that already exists keeps its spec.secretName:
// ensureTaskCertificate returns that name without updating the Certificate.
func TestEnsureTaskCertificate_KeepsExistingSecretName(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	task := &kaalmv1beta1.AgentTask{ObjectMeta: metav1.ObjectMeta{
		Name: "legacy-task", Namespace: "default", UID: "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d",
	}}
	cert := desiredTaskCertificate(task, CertLifetime{})
	cert.Spec.SecretName = "legacy-task-tls"
	cert.Status.Conditions = []cmapi.CertificateCondition{{
		Type: cmapi.CertificateConditionReady, Status: cmmeta.ConditionTrue, Reason: "Issued",
	}}
	if err := controllerutil.SetControllerReference(task, cert, scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cert).WithStatusSubresource(cert).Build()
	key := client.ObjectKeyFromObject(cert)
	var before cmapi.Certificate
	if err := c.Get(ctx, key, &before); err != nil {
		t.Fatal(err)
	}

	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system"}
	name, ready, err := r.ensureTaskCertificate(ctx, task)
	if err != nil || !ready || name != "legacy-task-tls" {
		t.Fatalf("ensureTaskCertificate = (%q, %v, %v), want (legacy-task-tls, true, nil)", name, ready, err)
	}
	var after cmapi.Certificate
	if err := c.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion || !equality.Semantic.DeepEqual(after.Spec, before.Spec) {
		t.Errorf("ensureTaskCertificate changed the existing Certificate: %+v", after.Spec)
	}
}

// ---- Rule 48: workload env Secrets opt in ----

func taskReadyReason(name string) (string, error) {
	var task kaalmv1beta1.AgentTask
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &task); err != nil {
		return "", err
	}
	c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
	if c == nil {
		return "", errString("no Ready condition yet")
	}
	return c.Reason, nil
}

func expectTaskReadyReason(t *testing.T, name, reason string) {
	t.Helper()
	eventually(t, func() error {
		r, err := taskReadyReason(name)
		if err != nil {
			return err
		}
		if r != reason {
			return errString("reason=" + r + " want " + reason)
		}
		return nil
	})
}

func TestTask_EnvSecretNotOptedInBlocks(t *testing.T) {
	mkWorkloadClass(t, "tc-envsec", nil)
	mkEnvSecret(t, "t-envsec-creds", false)
	mkTask(t, "t-envsec", "tc-envsec", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Env = []corev1.EnvVar{
			envFromSecret("TOKEN", "t-envsec-creds", false),
			envFromSecret("OTHER", "t-envsec-other", false),
		}
	})
	expectTaskReadyReason(t, "t-envsec", kaalmv1beta1.ReasonSecretNotOptedIn)
	expectEvent(t, "AgentTask", "default", "t-envsec", kaalmv1beta1.ReasonSecretNotOptedIn,
		corev1.EventTypeWarning, `Secret "t-envsec-creds"`)
	consistently(t, 2*time.Second, func() error {
		if taskPod(t, "t-envsec") != nil {
			return errString("a Pod was created while Ready=False held it")
		}
		if p := getTask(t, "t-envsec").Status.Phase; p == kaalmv1beta1.TaskFailed {
			return errString("a missing opt-in is not terminal, phase=" + string(p))
		}
		return nil
	})

	key := types.NamespacedName{Namespace: "default", Name: taskEnvSecretRoleName("t-envsec")}
	eventually(t, func() error {
		var role rbacv1.Role
		if err := testAPIReader.Get(ctxT(), key, &role); err != nil {
			return err
		}
		if len(role.Rules) != 1 ||
			!equality.Semantic.DeepEqual(role.Rules[0].ResourceNames, []string{"t-envsec-creds", "t-envsec-other"}) {
			return fmt.Errorf("role rules = %+v", role.Rules)
		}
		return nil
	})

	// Labeling both Secrets lets provisioning proceed.
	setEnvSecretLabel(t, "t-envsec-creds", true)
	mkEnvSecret(t, "t-envsec-other", true)
	eventually(t, func() error { return markCertReadyErr("t-envsec") })
	eventually(t, func() error {
		if taskPod(t, "t-envsec") == nil {
			return errString("no pod yet")
		}
		return nil
	})
}

func TestTask_EnvSecretMissingBlocks(t *testing.T) {
	mkWorkloadClass(t, "tc-envmiss", nil)
	mkTask(t, "t-envmiss", "tc-envmiss", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Env = []corev1.EnvVar{envFromSecret("TOKEN", "t-envmiss-creds", false)}
	})
	expectTaskReadyReason(t, "t-envmiss", kaalmv1beta1.ReasonSecretNotOptedIn)
	if taskPod(t, "t-envmiss") != nil {
		t.Fatal("a Pod was created for a missing env Secret")
	}
}
