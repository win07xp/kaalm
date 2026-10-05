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

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// restoreTask is an exitCode task with a UID in phase, with Ready set to
// ready/reason.
func restoreTask(name string, phase kaalmv1beta1.AgentTaskPhase, ready bool, reason string) *kaalmv1beta1.AgentTask {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	now := metav1.Now()
	return &kaalmv1beta1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-0123456789")},
		Spec: kaalmv1beta1.AgentTaskSpec{
			AgentClassRef: kaalmv1beta1.LocalObjectReference{Name: "restore-class"},
			Image:         "registry.test/agents/demo:v1",
			Completion:    kaalmv1beta1.AgentTaskCompletion{Condition: completionExitCode},
		},
		Status: kaalmv1beta1.AgentTaskStatus{
			Phase: phase, PodName: name + "-pod", StartTime: &now,
			Conditions: []metav1.Condition{{
				Type: kaalmv1beta1.ConditionReady, Status: status, Reason: reason, Message: reason,
				LastTransitionTime: now,
			}},
		},
	}
}

// restorePod is a Pod the task controls, in phase, Ready when ready.
func restorePod(t *testing.T, task *kaalmv1beta1.AgentTask, phase corev1.PodPhase, ready bool) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: task.Name + "-pod", Namespace: "default", Labels: taskPodLabels(task),
			CreationTimestamp: metav1.Now(),
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	if err := controllerutil.SetControllerReference(task, pod, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	return pod
}

// restoreReconciler is a task reconciler on a fake client holding objs.
func restoreReconciler(t *testing.T, objs ...client.Object) (*AgentTaskReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.AgentTask{}).Build()
	return &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(16)}, c
}

func storedTask(t *testing.T, c client.Client, task *kaalmv1beta1.AgentTask) *kaalmv1beta1.AgentTask {
	t.Helper()
	var got kaalmv1beta1.AgentTask
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(task), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func expectStoredReady(t *testing.T, task *kaalmv1beta1.AgentTask, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := apimeta.FindStatusCondition(task.Status.Conditions, kaalmv1beta1.ConditionReady)
	if c == nil || c.Status != status || c.Reason != reason {
		t.Errorf("Ready = %+v, want %s %s", c, status, reason)
	}
}

// expectTaskCertificate checks Certificate {name}-tls exists, is controlled
// by the task, and names the task's UID-suffixed Secret.
func expectTaskCertificate(t *testing.T, c client.Client, task *kaalmv1beta1.AgentTask) {
	t.Helper()
	var cert cmapi.Certificate
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: taskCertificateName(task.Name)}, &cert); err != nil {
		t.Fatalf("task Certificate: %v", err)
	}
	if !metav1.IsControlledBy(&cert, task) {
		t.Error("the task does not control its Certificate")
	}
	if want := certificateSecretName(task.Name, task.UID); cert.Spec.SecretName != want {
		t.Errorf("secretName = %q, want %q", cert.Spec.SecretName, want)
	}
}

// A task Certificate deleted while the task runs is re-created with the same
// Secret name, and the task keeps its phase, Ready, and Pod (#424).
func TestTask_CertificateDeletedWhileRunningIsRecreated(t *testing.T) {
	mkWorkloadClass(t, "wc-cert-restore", nil)
	pod := provisionRunningTask(t, "cert-restore", "wc-cert-restore", nil)
	key := types.NamespacedName{Namespace: "default", Name: "cert-restore-tls"}
	var old cmapi.Certificate
	if err := testClient.Get(ctxT(), key, &old); err != nil {
		t.Fatal(err)
	}
	if tlsSecretOf(pod) != old.Spec.SecretName {
		t.Fatalf("Pod mounts %q, Certificate names %q", tlsSecretOf(pod), old.Spec.SecretName)
	}
	if err := testClient.Delete(ctxT(), &old); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		var cert cmapi.Certificate
		if err := testClient.Get(ctxT(), key, &cert); err != nil {
			return err
		}
		if cert.UID == old.UID {
			return errString("old Certificate still there")
		}
		if cert.Spec.SecretName != old.Spec.SecretName {
			return fmt.Errorf("secretName = %q, want %q", cert.Spec.SecretName, old.Spec.SecretName)
		}
		if !metav1.IsControlledBy(&cert, getTask(t, "cert-restore")) {
			return errString("the task does not control the new Certificate")
		}
		return nil
	})
	if p := getTask(t, "cert-restore").Status.Phase; p != kaalmv1beta1.TaskRunning {
		t.Errorf("phase = %s, want Running", p)
	}
	expectTaskReadyReason(t, "cert-restore", "PodRunning")
	if p := taskPod(t, "cert-restore"); p == nil || p.UID != pod.UID {
		t.Errorf("pod = %v, want the running Pod %s kept", p, pod.UID)
	}
}

// With a Pod that is not Ready yet, a missing Certificate is re-created and
// does not hold the task: Ready stays PodProvisioning and the Pod turning
// Ready still moves the task to Running.
func TestDriveProvisioning_RecreatesCertificateWithoutGating(t *testing.T) {
	ctx := context.Background()
	task := restoreTask("prov-restore", kaalmv1beta1.TaskProvisioning, false, "CertificateNotReady")
	task.Status.StartTime = nil
	pod := restorePod(t, task, corev1.PodPending, false)
	r, c := restoreReconciler(t, task, pod)
	class := &kaalmv1beta1.AgentClass{}

	res, err := r.driveProvisioning(ctx, storedTask(t, c, task), class, effectiveTaskSpec{}, pod)
	if err != nil || res.RequeueAfter != certWaitRequeue {
		t.Fatalf("driveProvisioning = (%+v, %v), want a certWaitRequeue requeue", res, err)
	}
	expectTaskCertificate(t, c, task)
	expectStoredReady(t, storedTask(t, c, task), metav1.ConditionFalse, "PodProvisioning")

	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := r.driveProvisioning(ctx, storedTask(t, c, task), class, effectiveTaskSpec{}, pod); err != nil {
		t.Fatal(err)
	}
	if p := storedTask(t, c, task).Status.Phase; p != kaalmv1beta1.TaskRunning {
		t.Errorf("phase = %s, want Running while the new Certificate is not Ready", p)
	}
}

// A running task re-creates a missing Certificate and restores Ready.
func TestDriveRunning_RecreatesCertificateAndRestoresReady(t *testing.T) {
	task := restoreTask("run-restore", kaalmv1beta1.TaskRunning, false, kaalmv1beta1.ReasonChildConflict)
	pod := restorePod(t, task, corev1.PodRunning, true)
	r, c := restoreReconciler(t, task, pod)
	if _, err := r.driveRunning(context.Background(), storedTask(t, c, task), pod); err != nil {
		t.Fatalf("driveRunning: %v", err)
	}
	expectTaskCertificate(t, c, task)
	expectStoredReady(t, storedTask(t, c, task), metav1.ConditionTrue, "PodRunning")
}

// A Certificate name held by another object reports a conflict on a running
// task, and does not stop the task from completing.
func TestDriveRunning_CertificateConflictDoesNotBlockCompletion(t *testing.T) {
	task := restoreTask("run-conflict", kaalmv1beta1.TaskRunning, true, "PodRunning")
	pod := restorePod(t, task, corev1.PodRunning, true)
	foreign := desiredTaskCertificate(task, CertLifetime{})
	r, c := restoreReconciler(t, task, pod, foreign)

	_, err := r.driveRunning(context.Background(), storedTask(t, c, task), pod)
	if _, ok := asChildConflict(err); !ok {
		t.Fatalf("driveRunning = %v, want a ChildConflictError", err)
	}
	if p := storedTask(t, c, task).Status.Phase; p != kaalmv1beta1.TaskRunning {
		t.Errorf("phase = %s, want Running", p)
	}

	pod.Status.Phase = corev1.PodSucceeded
	if _, err := r.driveRunning(context.Background(), storedTask(t, c, task), pod); err != nil {
		t.Fatalf("driveRunning with the Pod done: %v", err)
	}
	if p := storedTask(t, c, task).Status.Phase; p != kaalmv1beta1.TaskSucceeded {
		t.Errorf("phase = %s, want Succeeded", p)
	}
}

// A running task held by a child it cannot write is requeued for its
// timeout when that comes before the gate re-check.
func TestHeldRequeue(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-5 * time.Second))
	withTimeout := &kaalmv1beta1.AgentTask{
		Spec: kaalmv1beta1.AgentTaskSpec{Completion: kaalmv1beta1.AgentTaskCompletion{
			Timeout: metav1.Duration{Duration: 10 * time.Second},
		}},
		Status: kaalmv1beta1.AgentTaskStatus{Phase: kaalmv1beta1.TaskRunning, StartTime: &started},
	}
	if d := heldRequeue(withTimeout, 30*time.Second).RequeueAfter; d <= 0 || d > 5*time.Second {
		t.Errorf("running with a timeout: RequeueAfter = %v, want at most 5s", d)
	}
	if d := heldRequeue(withTimeout, time.Second).RequeueAfter; d != time.Second {
		t.Errorf("gate sooner than the timeout: RequeueAfter = %v, want 1s", d)
	}
	noTimeout := withTimeout.DeepCopy()
	noTimeout.Spec.Completion.Timeout = metav1.Duration{}
	if d := heldRequeue(noTimeout, 30*time.Second).RequeueAfter; d != 30*time.Second {
		t.Errorf("running without a timeout: RequeueAfter = %v, want 30s", d)
	}
	provisioning := withTimeout.DeepCopy()
	provisioning.Status.Phase = kaalmv1beta1.TaskProvisioning
	if d := heldRequeue(provisioning, 30*time.Second).RequeueAfter; d != 30*time.Second {
		t.Errorf("provisioning: RequeueAfter = %v, want 30s", d)
	}
}

// A conflict on a running task uses the held requeue, so it never pushes the
// timeout back.
func TestChildBlocked_ConflictKeepsTimeoutRequeue(t *testing.T) {
	task := restoreTask("held-conflict", kaalmv1beta1.TaskRunning, true, "PodRunning")
	task.Spec.Completion.Timeout = metav1.Duration{Duration: 10 * time.Second}
	r, c := restoreReconciler(t, task)
	stored := storedTask(t, c, task)
	res, err := r.childBlocked(context.Background(), stored, nil, false,
		&ChildConflictError{Kind: "Certificate", Name: task.Name + "-tls", OwnerKind: "AgentTask"})
	if err != nil {
		t.Fatal(err)
	}
	if want := heldRequeue(stored, gateRequeue); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	expectStoredReady(t, storedTask(t, c, task), metav1.ConditionFalse, kaalmv1beta1.ReasonChildConflict)
}
