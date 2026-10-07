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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
	time.Sleep(3 * notReadyRecheck)
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
	const ns, name = "pvc-reject-deadline-task", "pvc-deadline-task"
	mkPVCQuota(t, ns)
	mkPersistentTaskIn(t, ns, name, "wc-pvc-reject-deadline", func(task *kaalmv1beta1.AgentTask) {
		withShortDeadline(task)
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

// retryRejecter is a fake client whose targeted write fails Forbidden while
// on is set: the Delete of a Pod, or the Update of a ConfigMap.
func retryRejecter(t *testing.T, on *bool, objs ...client.Object) client.Client {
	t.Helper()
	forbidden := func(resource, name string) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, name, errors.New("denied by webhook"))
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.AgentTask{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok && *on {
					return forbidden("pods", obj.GetName())
				}
				return c.Delete(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && *on {
					return forbidden("configmaps", obj.GetName())
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
}

// expectHeldRetry checks a stored task waits mid-retry: Failed with no
// completionTime, one retry counted, Ready=False ChildWriteRejected.
func expectHeldRetry(t *testing.T, c client.Client, task *kaalmv1beta1.AgentTask) {
	t.Helper()
	got := storedTask(t, c, task)
	if got.Status.Phase != kaalmv1beta1.TaskFailed || got.Status.CompletionTime != nil || got.Status.Retries != 1 {
		t.Errorf("phase=%s completionTime=%v retries=%d, want Failed, nil, 1",
			got.Status.Phase, got.Status.CompletionTime, got.Status.Retries)
	}
	expectStoredReady(t, got, metav1.ConditionFalse, kaalmv1beta1.ReasonChildWriteRejected)
}

// A retry whose delete of the old Pod is rejected holds in Failed with
// Ready=False ChildWriteRejected, then finishes once the delete goes
// through, without counting the retry again.
func TestTaskRetry_PodDeleteRejectedHoldsThenResumes(t *testing.T) {
	ctx := context.Background()
	task := restoreTask("retry-del", kaalmv1beta1.TaskProvisioning, false, "PodProvisioning")
	task.Finalizers = []string{kaalmv1beta1.TaskFinalizer}
	task.Spec.Completion.BackoffLimit = 2
	pod := restorePod(t, task, corev1.PodFailed, false)
	on := true
	c := retryRejecter(t, &on, task, pod)
	rec := record.NewFakeRecorder(16)
	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: rec}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}

	err := r.retry(ctx, storedTask(t, c, task), "PodStartFailed", "pod failed")
	cr, ok := asChildWriteRejected(err)
	if !ok || cr.Op != "deleting" || cr.Kind != "Pod" {
		t.Fatalf("retry error = %v, want a rejected Pod delete", err)
	}
	if _, err := r.childBlocked(ctx, storedTask(t, c, task), nil, false, err); err != nil {
		t.Fatal(err)
	}
	expectHeldRetry(t, c, task)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	expectHeldRetry(t, c, task)
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Errorf("old Pod gone while its delete is rejected: %v", err)
	}

	on = false
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("old Pod still present after the hold cleared: %v", err)
	}
	got := storedTask(t, c, task)
	if got.Status.Phase != kaalmv1beta1.TaskProvisioning || got.Status.Retries != 1 {
		t.Errorf("phase=%s retries=%d, want Provisioning and 1", got.Status.Phase, got.Status.Retries)
	}
	events := drainEvents(rec)
	if got := withPrefix(events, "Warning PodStartFailed"); len(got) != 1 || !strings.Contains(got[0], "retrying (1/2)") {
		t.Errorf("retry events = %q, want one retrying (1/2)", got)
	}
	if got := withPrefix(events, "Warning "+kaalmv1beta1.ReasonChildWriteRejected); len(got) != 1 {
		t.Errorf("ChildWriteRejected events = %q, want 1", got)
	}
}

// A retry whose mailbox reset is rejected holds the same way; the next
// attempt never starts with the old attempt's payload in the mailbox.
func TestTaskRetry_MailboxResetRejectedHoldsThenResumes(t *testing.T) {
	ctx := context.Background()
	task := restoreTask("retry-mbox", kaalmv1beta1.TaskProvisioning, false, "PodProvisioning")
	task.Finalizers = []string{kaalmv1beta1.TaskFinalizer}
	task.Spec.Completion = kaalmv1beta1.AgentTaskCompletion{Condition: completionAgentReported, BackoffLimit: 2}
	task.Status.CurrentPodUID = "old-pod-uid"
	cm := desiredCompletionConfigMap(task)
	cm.Data = map[string]string{"status": "failure"}
	if err := controllerutil.SetControllerReference(task, cm, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	on := true
	c := retryRejecter(t, &on, task, cm)
	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(16)}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}

	err := r.retry(ctx, storedTask(t, c, task), "PodStartFailed", "pod failed")
	if cr, ok := asChildWriteRejected(err); !ok ||
		!strings.Contains(cr.Error(), fmt.Sprintf("updating ConfigMap %q", taskCompletionCMName(task.Name))) {
		t.Fatalf("retry error = %v, want a rejected mailbox update", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	expectHeldRetry(t, c, task)
	var mbox corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &mbox); err != nil || mbox.Data["status"] != "failure" {
		t.Errorf("mailbox = %v (%v), want the old payload kept during the hold", mbox.Data, err)
	}

	on = false
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &mbox); err != nil || len(mbox.Data) != 0 {
		t.Errorf("mailbox = %v (%v), want it reset", mbox.Data, err)
	}
	got := storedTask(t, c, task)
	if got.Status.Phase != kaalmv1beta1.TaskProvisioning || got.Status.Retries != 1 || got.Status.CurrentPodUID != "" {
		t.Errorf("phase=%s retries=%d currentPodUID=%q, want Provisioning, 1, empty",
			got.Status.Phase, got.Status.Retries, got.Status.CurrentPodUID)
	}
}

// A rejected write on a task mid-retry keeps the Failed phase that marks
// the steps still to finish; it never starts the provisioning deadline.
func TestChildBlocked_FailedTaskKeepsPhase(t *testing.T) {
	task := restoreTask("failed-held", kaalmv1beta1.TaskFailed, false, "PodStartFailed")
	task.Status.StartTime = nil
	r, c := restoreReconciler(t, task)
	r.notReadyRecheckOverride = 30 * time.Second
	rejected := &ChildWriteRejectedError{Op: "updating", Kind: "ConfigMap", Name: "failed-held-completion",
		Err: apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "failed-held-completion", errors.New("denied"))}
	res, err := r.childBlocked(context.Background(), storedTask(t, c, task), &kaalmv1beta1.AgentClass{}, true, rejected)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want 30s", res.RequeueAfter)
	}
	got := storedTask(t, c, task)
	if got.Status.Phase != kaalmv1beta1.TaskFailed || got.Status.CreateRejectedTime != nil {
		t.Errorf("phase=%s createRejectedTime=%v, want Failed and nil", got.Status.Phase, got.Status.CreateRejectedTime)
	}
	expectStoredReady(t, got, metav1.ConditionFalse, kaalmv1beta1.ReasonChildWriteRejected)
}
