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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// mkPVCQuota creates namespace ns with a "persistentvolumeclaims" quota
// that admits no claim.
func mkPVCQuota(t *testing.T, ns string) {
	t.Helper()
	mkResourceQuota(t, ns, corev1.ResourcePersistentVolumeClaims, func() client.Object {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "quota-probe-", Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
	})
}

// mkPersistentTaskIn creates a persistent task in ns under a class that
// allows persistence.
func mkPersistentTaskIn(t *testing.T, ns, name, className string, mutate func(*kaalmv1beta1.AgentTask)) {
	t.Helper()
	mkWorkloadClass(t, className, func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Persistence.Enabled = true
		ac.Spec.Persistence.DefaultSizeGi = 1
	})
	task := &kaalmv1beta1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kaalmv1beta1.AgentTaskSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: className},
			Image:         "registry.test/agents/demo:v1",
		},
	}
	task.Spec.Persistence.Enabled = true
	if mutate != nil {
		mutate(task)
	}
	if err := testClient.Create(ctxT(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
}

func getTaskIn(t *testing.T, ns, name string) *kaalmv1beta1.AgentTask {
	t.Helper()
	var got kaalmv1beta1.AgentTask
	if err := testAPIReader.Get(ctxT(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get task: %v", err)
	}
	return &got
}

// A task whose PVC create the namespace quota refuses waits in
// Provisioning with Ready=False ChildWriteRejected and the deadline clock
// started, with no Pod and one Warning, and gets its Pod once the quota is
// raised.
func TestTask_PVCCreateRejectedByQuota(t *testing.T) {
	const ns, name = "pvc-reject-quota-task", "pvc-quota-task"
	mkPVCQuota(t, ns)
	mkPersistentTaskIn(t, ns, name, "wc-pvc-reject-quota", nil)
	eventually(t, func() error { return markCertReadyIn(ns, name) })

	wantName := `creating PersistentVolumeClaim "` + name + `-workspace"`
	eventually(t, func() error {
		got := getTaskIn(t, ns, name)
		if got.Status.Phase != kaalmv1beta1.TaskProvisioning {
			return fmt.Errorf("phase = %s, want Provisioning", got.Status.Phase)
		}
		if got.Status.CreateRejectedTime == nil {
			return errString("createRejectedTime unset")
		}
		return expectReadyWriteRejected(got.Status.Conditions, wantName, "exceeded quota")
	})
	if p := podIn(t, ns, "kaalm.io/task", name); p != nil {
		t.Fatalf("pod %s exists, want none", p.Name)
	}
	expectEvent(t, "AgentTask", ns, name, kaalmv1beta1.ReasonChildWriteRejected, corev1.EventTypeWarning, wantName)
	time.Sleep(3 * gateRequeue)
	if n := eventCount(objectEvents(t, "AgentTask", ns, name, kaalmv1beta1.ReasonChildWriteRejected)); n != 1 {
		t.Fatalf("ChildWriteRejected events = %d, want 1", n)
	}

	setQuota(t, ns, corev1.ResourcePersistentVolumeClaims, 1)
	eventually(t, func() error {
		if podIn(t, ns, "kaalm.io/task", name) == nil {
			return errString("no pod yet")
		}
		return nil
	})
	eventually(t, func() error {
		if getTaskIn(t, ns, name).Status.CreateRejectedTime != nil {
			return errString("createRejectedTime still set")
		}
		return nil
	})
}

// A child write rejected past the provisioning deadline fails the attempt
// with ProvisioningDeadlineExceeded: retried while backoffLimit allows, then
// settled Failed with the class bounds recorded.
func TestTask_ChildWriteRejectedDeadline(t *testing.T) {
	old := provisioningDeadline
	provisioningDeadline = 1 * time.Second
	defer func() { provisioningDeadline = old }()

	const ns, name = "pvc-reject-deadline-task", "pvc-deadline-task"
	mkPVCQuota(t, ns)
	mkPersistentTaskIn(t, ns, name, "wc-pvc-reject-deadline", func(task *kaalmv1beta1.AgentTask) {
		task.Spec.Completion.BackoffLimit = 1
	})
	eventually(t, func() error { return markCertReadyIn(ns, name) })

	expectEvent(t, "AgentTask", ns, name, "ProvisioningDeadlineExceeded",
		corev1.EventTypeWarning, "retrying (1/1)")
	eventually(t, func() error {
		task := getTaskIn(t, ns, name)
		if task.Status.Phase != kaalmv1beta1.TaskFailed || task.Status.CompletionTime == nil {
			return fmt.Errorf("phase = %s completionTime = %v, want settled Failed",
				task.Status.Phase, task.Status.CompletionTime)
		}
		return nil
	})
	task := getTaskIn(t, ns, name)
	c := condition(task.Status.Conditions, kaalmv1beta1.ConditionCompleted)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ProvisioningDeadlineExceeded" ||
		!strings.Contains(c.Message, "child write rejected for longer than") ||
		!strings.Contains(c.Message, "PersistentVolumeClaim") {
		t.Errorf("Completed = %+v, want False ProvisioningDeadlineExceeded naming the PersistentVolumeClaim", c)
	}
	if task.Status.ClassBounds == nil {
		t.Error("classBounds unset, want the class bounds recorded at settle")
	}
	if task.Status.CreateRejectedTime != nil {
		t.Errorf("createRejectedTime = %v, want cleared", task.Status.CreateRejectedTime)
	}
	if p := podIn(t, ns, "kaalm.io/task", name); p != nil {
		t.Errorf("pod %s exists, want none", p.Name)
	}
}
