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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// A class naming a RuntimeClass the cluster lacks: the API server rejects
// the task's Pod create. Within the provisioning deadline the task stays
// Provisioning with no Pod, spends no backoffLimit retry, never starts the
// completion timeout, and reports the rejection in Ready and one Warning.
// The timed re-check creates the Pod once the RuntimeClass exists.
func TestTask_MissingRuntimeClassLeavesNoPod(t *testing.T) {
	rcName := "absent-sandbox-task"
	wantMsg := `RuntimeClass "` + rcName + `" not found`
	mkWorkloadClass(t, "wc-missing-rc-task", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Runtime.RuntimeClassName = &rcName
	})
	mkTask(t, "missing-rc-task", "wc-missing-rc-task", nil)
	eventually(t, func() error { return markCertReadyErr("missing-rc-task") })

	eventually(t, func() error {
		return expectReadyRejected(getTask(t, "missing-rc-task").Status.Conditions, wantMsg)
	})
	consistently(t, time.Second, func() error {
		task := getTask(t, "missing-rc-task")
		if task.Status.Phase != kaalmv1beta1.TaskProvisioning {
			return fmt.Errorf("phase = %s, want Provisioning", task.Status.Phase)
		}
		if task.Status.Retries != 0 {
			return fmt.Errorf("retries = %d, want 0", task.Status.Retries)
		}
		if task.Status.StartTime != nil {
			return fmt.Errorf("startTime = %v, want unset", task.Status.StartTime)
		}
		if task.Status.PodName != "" {
			return fmt.Errorf("podName = %q, want empty", task.Status.PodName)
		}
		if task.Status.PodCreateRejectedTime == nil {
			return errString("podCreateRejectedTime unset")
		}
		if p := taskPod(t, "missing-rc-task"); p != nil {
			return fmt.Errorf("pod %s exists, want none", p.Name)
		}
		return expectReadyRejected(task.Status.Conditions, wantMsg)
	})
	expectEvent(t, "AgentTask", "default", "missing-rc-task", kaalmv1beta1.ReasonPodCreateRejected,
		corev1.EventTypeWarning, wantMsg)
	if n := eventCount(objectEvents(t, "AgentTask", "default", "missing-rc-task",
		kaalmv1beta1.ReasonPodCreateRejected)); n != 1 {
		t.Fatalf("PodCreateRejected events = %d, want 1", n)
	}

	// Recovery: the re-check notices the RuntimeClass without an edit.
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: rcName}, Handler: "runsc"}
	if err := testClient.Create(ctxT(), rc); err != nil {
		t.Fatalf("create runtimeclass: %v", err)
	}
	eventually(t, func() error {
		p := taskPod(t, "missing-rc-task")
		if p == nil {
			return errString("no pod yet")
		}
		if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != rcName {
			t.Fatalf("pod runtimeClassName = %v, want %s", p.Spec.RuntimeClassName, rcName)
		}
		return nil
	})
	eventually(t, func() error {
		task := getTask(t, "missing-rc-task")
		if task.Status.PodCreateRejectedTime != nil {
			return errString("podCreateRejectedTime still set")
		}
		if c := condition(task.Status.Conditions, kaalmv1beta1.ConditionReady); c == nil || c.Reason != "PodProvisioning" {
			return fmt.Errorf("Ready = %+v, want PodProvisioning", c)
		}
		return nil
	})
}

// A task in a namespace over quota reports the rejection and gets its Pod
// once the quota is raised.
func TestTask_PodCreateRejectedByQuota(t *testing.T) {
	const ns, name = "pod-reject-quota-task", "quota-task"
	mkQuotaNamespace(t, ns)
	mkWorkloadClass(t, "wc-reject-quota-task", nil)
	task := &kaalmv1beta1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kaalmv1beta1.AgentTaskSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "wc-reject-quota-task"},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	if err := testClient.Create(ctxT(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	eventually(t, func() error { return markCertReadyIn(ns, name) })
	eventually(t, func() error {
		var got kaalmv1beta1.AgentTask
		if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			return err
		}
		return expectReadyRejected(got.Status.Conditions, "exceeded quota")
	})

	setPodQuota(t, ns, 1)
	eventually(t, func() error {
		if podIn(t, ns, "kaalm.io/task", name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
}

// A Pod create rejected past the provisioning deadline fails the attempt
// with ProvisioningDeadlineExceeded: retried while backoffLimit allows, then
// settled Failed with the class bounds recorded.
func TestTask_PodCreateRejectedDeadline(t *testing.T) {
	old := provisioningDeadline
	provisioningDeadline = 1 * time.Second
	defer func() { provisioningDeadline = old }()

	missing := "absent-sandbox-deadline"
	mkWorkloadClass(t, "wc-reject-deadline", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Runtime.RuntimeClassName = &missing
	})
	mkTask(t, "reject-deadline", "wc-reject-deadline", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.BackoffLimit = 1
	})
	eventually(t, func() error { return markCertReadyErr("reject-deadline") })

	expectEvent(t, "AgentTask", "default", "reject-deadline", "ProvisioningDeadlineExceeded",
		corev1.EventTypeWarning, "retrying (1/1)")
	eventually(t, func() error {
		task := getTask(t, "reject-deadline")
		if task.Status.Phase != kaalmv1beta1.TaskFailed || task.Status.CompletionTime == nil {
			return fmt.Errorf("phase = %s completionTime = %v, want settled Failed",
				task.Status.Phase, task.Status.CompletionTime)
		}
		return nil
	})
	task := getTask(t, "reject-deadline")
	c := condition(task.Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ProvisioningDeadlineExceeded" ||
		!strings.Contains(c.Message, "RuntimeClass") {
		t.Errorf("Completed = %+v, want False ProvisioningDeadlineExceeded naming the RuntimeClass", c)
	}
	if task.Status.Retries != 1 {
		t.Errorf("retries = %d, want 1", task.Status.Retries)
	}
	if task.Status.ClassBounds == nil {
		t.Error("classBounds unset, want the class bounds recorded at settle")
	}
	if task.Status.PodCreateRejectedTime != nil {
		t.Errorf("podCreateRejectedTime = %v, want cleared", task.Status.PodCreateRejectedTime)
	}
	if p := taskPod(t, "reject-deadline"); p != nil {
		t.Errorf("pod %s exists, want none", p.Name)
	}
}

// The deadline reads the persisted first-rejection time, not process
// memory: a stored time older than the deadline fails the attempt on the
// next re-check.
func TestTask_PodCreateRejectedDeadlineFromStatus(t *testing.T) {
	missing := "absent-sandbox-persisted"
	mkWorkloadClass(t, "wc-reject-persisted", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Runtime.RuntimeClassName = &missing
	})
	mkTask(t, "reject-persisted", "wc-reject-persisted", nil)
	eventually(t, func() error { return markCertReadyErr("reject-persisted") })
	eventually(t, func() error {
		if getTask(t, "reject-persisted").Status.PodCreateRejectedTime == nil {
			return errString("podCreateRejectedTime unset")
		}
		return nil
	})

	eventually(t, func() error {
		task := getTask(t, "reject-persisted")
		past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		task.Status.PodCreateRejectedTime = &past
		return testClient.Status().Update(ctxT(), task)
	})
	eventually(t, func() error {
		task := getTask(t, "reject-persisted")
		c := condition(task.Status.Conditions, kaalmv1beta1.ConditionCompleted)
		if task.Status.Phase != kaalmv1beta1.TaskFailed || c == nil || c.Reason != "ProvisioningDeadlineExceeded" {
			return fmt.Errorf("phase = %s Completed = %+v, want Failed ProvisioningDeadlineExceeded",
				task.Status.Phase, c)
		}
		return nil
	})
}
