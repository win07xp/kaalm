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
	"slices"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
// Secret name, and the task keeps its phase, Ready, and Pod.
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
	if _, err := r.driveRunning(context.Background(), storedTask(t, c, task), &kaalmv1beta1.AgentClass{}, effectiveTaskSpec{}, pod); err != nil {
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

	_, err := r.driveRunning(context.Background(), storedTask(t, c, task), &kaalmv1beta1.AgentClass{}, effectiveTaskSpec{}, pod)
	if _, ok := asChildConflict(err); !ok {
		t.Fatalf("driveRunning = %v, want a ChildConflictError", err)
	}
	if p := storedTask(t, c, task).Status.Phase; p != kaalmv1beta1.TaskRunning {
		t.Errorf("phase = %s, want Running", p)
	}

	pod.Status.Phase = corev1.PodSucceeded
	if _, err := r.driveRunning(context.Background(), storedTask(t, c, task), &kaalmv1beta1.AgentClass{}, effectiveTaskSpec{}, pod); err != nil {
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

// A child the task cannot write holds a running task on the held requeue,
// so neither a conflict nor a rejected write pushes the timeout back. The
// reconciler's gate is 30s here (the package's gateRequeue is shortened for
// envtest), so a return of the bare gate interval fails the 5s bound.
func TestChildBlocked_HeldRequeueKeepsTimeout(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{"conflict", &ChildConflictError{Kind: "Certificate", Name: "held-tls", OwnerKind: "AgentTask"},
			kaalmv1beta1.ReasonChildConflict},
		{"rejected write", &ChildWriteRejectedError{Op: "creating", Kind: "Certificate", Name: "held-tls",
			Err: apierrors.NewForbidden(schema.GroupResource{Resource: "certificates"}, "held-tls", errors.New("exceeded quota"))},
			kaalmv1beta1.ReasonChildWriteRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, timeout := range []time.Duration{10 * time.Second, 0} {
				task := restoreTask("held-requeue", kaalmv1beta1.TaskRunning, true, "PodRunning")
				task.Spec.Completion.Timeout = metav1.Duration{Duration: timeout}
				started := metav1.NewTime(time.Now().Add(-5 * time.Second))
				task.Status.StartTime = &started
				r, c := restoreReconciler(t, task)
				r.gateInterval = 30 * time.Second
				res, err := r.childBlocked(context.Background(), storedTask(t, c, task), nil, false, tc.err)
				if err != nil {
					t.Fatal(err)
				}
				if timeout > 0 {
					if d := res.RequeueAfter; d <= 0 || d > 5*time.Second {
						t.Errorf("timeout 10s, 5s left: RequeueAfter = %v, want at most 5s", d)
					}
				} else if res.RequeueAfter != 30*time.Second {
					t.Errorf("no timeout: RequeueAfter = %v, want 30s", res.RequeueAfter)
				}
				stored := storedTask(t, c, task)
				expectStoredReady(t, stored, metav1.ConditionFalse, tc.reason)
				if stored.Status.Phase != kaalmv1beta1.TaskRunning {
					t.Errorf("phase = %s, want Running", stored.Status.Phase)
				}
			}
		})
	}
}

// A running task whose AgentClass was deleted and came back drops the stale
// Ready=False InvalidReference the class gate left.
func TestDriveRunning_RestoresReadyAfterClassGate(t *testing.T) {
	task := restoreTask("run-class-back", kaalmv1beta1.TaskRunning, false, kaalmv1beta1.ReasonInvalidReference)
	pod := restorePod(t, task, corev1.PodRunning, true)
	cert := desiredTaskCertificate(task, CertLifetime{})
	if err := controllerutil.SetControllerReference(task, cert, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	r, c := restoreReconciler(t, task, pod, cert)
	if _, err := r.driveRunning(context.Background(), storedTask(t, c, task), &kaalmv1beta1.AgentClass{}, effectiveTaskSpec{}, pod); err != nil {
		t.Fatalf("driveRunning: %v", err)
	}
	expectStoredReady(t, storedTask(t, c, task), metav1.ConditionTrue, "PodRunning")
}

// agentReportedChildren are every child desiredTaskChildren lists for an
// agentReported task without persistence, each controlled by task.
func agentReportedChildren(t *testing.T, task *kaalmv1beta1.AgentTask) []client.Object {
	t.Helper()
	objs := []client.Object{
		desiredTaskServiceAccount(task),
		desiredTaskNetworkPolicy(task, nil, "kaalm-system", DNSSelector{}),
		desiredCompletionConfigMap(task),
		desiredCompletionRole(task),
		desiredCompletionRoleBinding(task, "kaalm-system"),
	}
	for _, obj := range objs {
		if err := controllerutil.SetControllerReference(task, obj, testScheme(t)); err != nil {
			t.Fatal(err)
		}
	}
	return objs
}

// The pre-Pod pass reads each child before creating it, so children that
// exist cost no create (createIfMissing's read-before-create rule, applied
// to tasks).
func TestEnsureTaskChildren_ReadsBeforeCreating(t *testing.T) {
	task := restoreTask("reads-first", kaalmv1beta1.TaskProvisioning, false, "CertificateNotReady")
	task.Spec.Completion.Condition = ""
	var creates int
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agentReportedChildren(t, task)...).
		WithInterceptorFuncs(countingCreates(&creates, nil)).Build()
	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system"}
	if err := r.ensureTaskChildren(context.Background(), task, &kaalmv1beta1.AgentClass{}, effectiveTaskSpec{}); err != nil {
		t.Fatal(err)
	}
	if creates != 0 {
		t.Errorf("creates = %d, want 0 when every child exists", creates)
	}
}

// A running task's deleted children come back with new UIDs while its Pod
// keeps running, and the task still completes through the re-created
// mailbox.
func TestTask_DeletedChildrenRecreatedWhileRunning(t *testing.T) {
	mkWorkloadClass(t, "wc-child-restore", nil)
	pod := provisionRunningTask(t, "child-restore", "wc-child-restore", nil)
	children := []client.Object{
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "child-restore"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: taskServiceAccountName("child-restore")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: taskCompletionCMName("child-restore")}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: taskCompletionRoleName("child-restore")}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: taskCompletionRoleName("child-restore")}},
	}
	old := map[string]types.UID{}
	for _, obj := range children {
		obj.SetNamespace("default")
		if err := testClient.Get(ctxT(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("%T %s: %v", obj, obj.GetName(), err)
		}
		old[fmt.Sprintf("%T", obj)] = obj.GetUID()
	}
	for _, obj := range children {
		if err := testClient.Delete(ctxT(), obj); err != nil {
			t.Fatalf("delete %T: %v", obj, err)
		}
	}
	task := getTask(t, "child-restore")
	for _, obj := range children {
		eventually(t, func() error {
			got := obj.DeepCopyObject().(client.Object)
			if err := testClient.Get(ctxT(), client.ObjectKeyFromObject(obj), got); err != nil {
				return err
			}
			if got.GetUID() == old[fmt.Sprintf("%T", obj)] {
				return fmt.Errorf("%T %s not re-created yet", obj, obj.GetName())
			}
			if !metav1.IsControlledBy(got, task) {
				return fmt.Errorf("%T %s is not controlled by the task", obj, obj.GetName())
			}
			return nil
		})
	}
	var role rbacv1.Role
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default",
		Name: taskCompletionRoleName("child-restore")}, &role); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(role.Rules, desiredCompletionRole(task).Rules) {
		t.Errorf("re-created Role rules = %+v, want the desired rules", role.Rules)
	}
	if p := getTask(t, "child-restore").Status.Phase; p != kaalmv1beta1.TaskRunning {
		t.Errorf("phase = %s, want Running", p)
	}
	expectTaskReadyReason(t, "child-restore", "PodRunning")
	if p := taskPod(t, "child-restore"); p == nil || p.UID != pod.UID {
		t.Errorf("pod = %v, want the running Pod %s kept", p, pod.UID)
	}
	writeMailbox(t, "child-restore", map[string]string{"status": "success"})
	expectTaskPhase(t, "child-restore", kaalmv1beta1.TaskSucceeded)
}

// A NetworkPolicy deleted while the task waits on a Pod that is not Ready
// comes back, and the task keeps waiting on that Pod.
func TestTask_NetworkPolicyDeletedWhileProvisioningIsRecreated(t *testing.T) {
	mkWorkloadClass(t, "wc-np-restore-prov", nil)
	mkTask(t, "np-restore-prov", "wc-np-restore-prov", nil)
	eventually(t, func() error { return markCertReadyErr("np-restore-prov") })
	eventually(t, func() error {
		if taskPod(t, "np-restore-prov") == nil {
			return errString("no pod yet")
		}
		return nil
	})
	pod := taskPod(t, "np-restore-prov")
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "np-restore-prov", Namespace: "default"}}
	if err := testClient.Get(ctxT(), client.ObjectKeyFromObject(np), np); err != nil {
		t.Fatal(err)
	}
	oldUID := np.UID
	if err := testClient.Delete(ctxT(), np); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		var got networkingv1.NetworkPolicy
		if err := testClient.Get(ctxT(), client.ObjectKeyFromObject(np), &got); err != nil {
			return err
		}
		if got.UID == oldUID {
			return errString("NetworkPolicy not re-created yet")
		}
		if !metav1.IsControlledBy(&got, getTask(t, "np-restore-prov")) {
			return errString("the task does not control the new NetworkPolicy")
		}
		return nil
	})
	expectTaskReadyReason(t, "np-restore-prov", "PodProvisioning")
	if p := taskPod(t, "np-restore-prov"); p == nil || p.UID != pod.UID {
		t.Errorf("pod = %v, want the Pod %s kept", p, pod.UID)
	}
}

// setFQDNHosts overwrites the hosts of the task's FQDN policy, the way an
// edit by hand would.
func setFQDNHosts(t *testing.T, name string, hosts ...string) {
	t.Helper()
	eventually(t, func() error {
		u, err := getFQDNPolicy(name)
		if err != nil {
			return err
		}
		egress, _, _ := unstructured.NestedSlice(u.Object, "spec", "egress")
		var fqdns []any
		for _, h := range hosts {
			fqdns = append(fqdns, map[string]any{"matchName": h})
		}
		egress[1].(map[string]any)["toFQDNs"] = fqdns
		if err := unstructured.SetNestedSlice(u.Object, egress, "spec", "egress"); err != nil {
			return err
		}
		return testClient.Update(ctxT(), u)
	})
}

// A running task's FQDN policy keeps the hosts the class listed when the
// Pod was created: a class edit does not reach it, an edit to the policy is
// reverted at the next Running pass, and a deleted policy comes back with
// those hosts.
func TestTask_FQDNPolicyKeptToClassAtPodCreation(t *testing.T) {
	mkWorkloadClass(t, "wc-fqdn-restore", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Network.Egress.AllowedHosts = []string{"a.example.com"}
	})
	provisionRunningTask(t, "fqdn-restore", "wc-fqdn-restore", nil)
	expectFQDNHosts(t, "fqdn-restore", "AgentTask", "a.example.com")

	setClassHosts(t, "wc-fqdn-restore", []string{"b.example.com"})
	consistently(t, 2*time.Second, func() error {
		u, err := getFQDNPolicy("fqdn-restore")
		if err != nil {
			return err
		}
		egress, _, _ := unstructured.NestedSlice(u.Object, "spec", "egress")
		if fmt.Sprint(egress[1].(map[string]any)["toFQDNs"]) != "[map[matchName:a.example.com]]" {
			return errString("a running task's FQDN policy was updated")
		}
		return nil
	})

	// The policy kind is not watched: a class edit brings the Running pass.
	setFQDNHosts(t, "fqdn-restore", "evil.example.com")
	setClassHosts(t, "wc-fqdn-restore", []string{"c.example.com"})
	expectFQDNHosts(t, "fqdn-restore", "AgentTask", "a.example.com")

	u, err := getFQDNPolicy("fqdn-restore")
	if err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctxT(), u); err != nil {
		t.Fatal(err)
	}
	setClassHosts(t, "wc-fqdn-restore", []string{"d.example.com"})
	expectFQDNHosts(t, "fqdn-restore", "AgentTask", "a.example.com")

	setClassHosts(t, "wc-fqdn-restore", nil)
	consistently(t, 2*time.Second, func() error {
		_, err := getFQDNPolicy("fqdn-restore")
		return err
	})
}

// setClassCIDRs sets a class's network.egress.allowedCIDRs.
func setClassCIDRs(t *testing.T, class string, cidrs []string) {
	t.Helper()
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: class}, &ac); err != nil {
			return err
		}
		ac.Spec.Network.Egress.AllowedCIDRs = cidrs
		return testClient.Update(ctxT(), &ac)
	})
}

// npCIDRs lists the ipBlock CIDRs of a task's NetworkPolicy.
func npCIDRs(name string) ([]string, error) {
	var np networkingv1.NetworkPolicy
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &np); err != nil {
		return nil, err
	}
	var out []string
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				out = append(out, peer.IPBlock.CIDR)
			}
		}
	}
	return out, nil
}

// expectKeptCIDRs checks the np-keep task's NetworkPolicy CIDRs.
func expectKeptCIDRs(want ...string) func() error {
	return func() error {
		got, err := npCIDRs("np-keep")
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("NetworkPolicy CIDRs = %v, want %v", got, want)
		}
		return nil
	}
}

// writtenSince reports whether obj is a copy written after the given
// resourceVersions. testClient reads from the manager's cache, which can
// still hold the copy from before an edit or the edit itself; a revert
// check passes only on a later write.
func writtenSince(obj client.Object, rvs ...string) error {
	if slices.Contains(rvs, obj.GetResourceVersion()) {
		return fmt.Errorf("%T %s not rewritten yet", obj, obj.GetName())
	}
	return nil
}

// editObject applies edit to the named object and updates it, retrying on
// conflicts. It returns the resourceVersions of the copy it edited and of
// the edit.
func editObject(t *testing.T, key types.NamespacedName, obj client.Object, edit func()) (before, after string) {
	t.Helper()
	eventually(t, func() error {
		if err := testClient.Get(ctxT(), key, obj); err != nil {
			return err
		}
		before = obj.GetResourceVersion()
		edit()
		if err := testClient.Update(ctxT(), obj); err != nil {
			return err
		}
		after = obj.GetResourceVersion()
		return nil
	})
	return before, after
}

// Edits to a running task's NetworkPolicy, completion Role, and RoleBinding
// are put back, and the task keeps its Pod and phase.
func TestTask_EditedChildrenRevertedWhileRunning(t *testing.T) {
	mkWorkloadClass(t, "wc-child-revert", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Network.Egress.AllowedCIDRs = []string{"10.0.0.0/8"}
	})
	pod := provisionRunningTask(t, "child-revert", "wc-child-revert", nil)
	task := getTask(t, "child-revert")
	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: "default", Name: name} }

	var np networkingv1.NetworkPolicy
	var role rbacv1.Role
	var rb rbacv1.RoleBinding
	if err := testClient.Get(ctxT(), key("child-revert"), &np); err != nil {
		t.Fatal(err)
	}
	wantNP := np.Spec.DeepCopy()
	npBefore, npEdit := editObject(t, key("child-revert"), &np, func() {
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
		})
	})
	roleBefore, roleEdit := editObject(t, key(taskCompletionRoleName("child-revert")), &role, func() {
		role.Rules[0].Verbs = append(role.Rules[0].Verbs, "get", "create")
	})
	rbBefore, rbEdit := editObject(t, key(taskCompletionRoleName("child-revert")), &rb, func() {
		rb.Subjects = append(rb.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "intruder", Namespace: "default"})
	})

	eventually(t, func() error {
		var got networkingv1.NetworkPolicy
		if err := testClient.Get(ctxT(), key("child-revert"), &got); err != nil {
			return err
		}
		if err := writtenSince(&got, npBefore, npEdit); err != nil {
			return err
		}
		if !equality.Semantic.DeepEqual(got.Spec, *wantNP) {
			return errString("NetworkPolicy edit not reverted yet")
		}
		var gotRole rbacv1.Role
		if err := testClient.Get(ctxT(), key(taskCompletionRoleName("child-revert")), &gotRole); err != nil {
			return err
		}
		if err := writtenSince(&gotRole, roleBefore, roleEdit); err != nil {
			return err
		}
		if !equality.Semantic.DeepEqual(gotRole.Rules, desiredCompletionRole(task).Rules) {
			return errString("Role edit not reverted yet")
		}
		var gotRB rbacv1.RoleBinding
		if err := testClient.Get(ctxT(), key(taskCompletionRoleName("child-revert")), &gotRB); err != nil {
			return err
		}
		if err := writtenSince(&gotRB, rbBefore, rbEdit); err != nil {
			return err
		}
		if len(gotRB.Subjects) != 1 {
			return errString("RoleBinding edit not reverted yet")
		}
		return nil
	})
	if p := getTask(t, "child-revert").Status.Phase; p != kaalmv1beta1.TaskRunning {
		t.Errorf("phase = %s, want Running", p)
	}
	expectTaskReadyReason(t, "child-revert", "PodRunning")
	if p := taskPod(t, "child-revert"); p == nil || p.UID != pod.UID {
		t.Errorf("pod = %v, want the running Pod %s kept", p, pod.UID)
	}
}

// A running task's NetworkPolicy keeps the CIDRs the class listed when the
// Pod was created: a class edit does not reach it, an edit is reverted to
// those CIDRs, and a deleted policy comes back with them.
func TestTask_NetworkPolicyKeptToClassAtPodCreation(t *testing.T) {
	mkWorkloadClass(t, "wc-np-keep", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.Network.Egress.AllowedCIDRs = []string{"10.1.0.0/16"}
	})
	provisionRunningTask(t, "np-keep", "wc-np-keep", nil)
	if eg := getTask(t, "np-keep").Status.ClassEgress; eg == nil || !slices.Equal(eg.AllowedCIDRs, []string{"10.1.0.0/16"}) {
		t.Fatalf("status.classEgress = %+v, want allowedCIDRs [10.1.0.0/16]", eg)
	}

	setClassCIDRs(t, "wc-np-keep", []string{"10.2.0.0/16"})
	consistently(t, 2*time.Second, expectKeptCIDRs("10.1.0.0/16"))

	npKey := types.NamespacedName{Namespace: "default", Name: "np-keep"}
	var edited networkingv1.NetworkPolicy
	before, edit := editObject(t, npKey, &edited, func() {
		edited.Spec.Egress[len(edited.Spec.Egress)-1].To[0].IPBlock.CIDR = "0.0.0.0/0"
	})
	eventually(t, func() error {
		var np networkingv1.NetworkPolicy
		if err := testClient.Get(ctxT(), npKey, &np); err != nil {
			return err
		}
		if err := writtenSince(&np, before, edit); err != nil {
			return err
		}
		return expectKeptCIDRs("10.1.0.0/16")()
	})

	// testClient reads from the manager's cache, which can still hold the
	// deleted policy for a moment; wait for the restored one by its UID.
	var old networkingv1.NetworkPolicy
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "np-keep"}, &old); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctxT(), &old); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		var np networkingv1.NetworkPolicy
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "np-keep"}, &np); err != nil {
			return err
		}
		if np.UID == old.UID {
			return fmt.Errorf("NetworkPolicy np-keep not re-created yet")
		}
		return expectKeptCIDRs("10.1.0.0/16")()
	})
	consistently(t, time.Second, expectKeptCIDRs("10.1.0.0/16"))
}

// countingWrites is an interceptor that counts Create, Update, Patch, and
// Delete calls, and Get calls on unstructured objects.
func countingWrites(writes, unstructuredGets *int) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			*writes++
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			*writes++
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch,
			opts ...client.PatchOption) error {
			*writes++
			return c.Patch(ctx, obj, p, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*writes++
			return c.Delete(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*unstructured.Unstructured); ok {
				*unstructuredGets++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

// The still-running pass re-creates only what is missing, and a pass with
// nothing missing writes nothing. A PVC comes back only when the Pod mounts
// it.
func TestDriveRunning_RestoresMissingChildrenOnly(t *testing.T) {
	persistent := effectiveTaskSpec{PersistenceOn: true, PVCSizeGi: 1}
	cases := []struct {
		name      string
		eff       effectiveTaskSpec
		mountsPVC bool
		drop      []string // kinds left out of the store
		creates   int
		wantPVC   bool
	}{
		{"all present", effectiveTaskSpec{}, false, nil, 0, false},
		{"NetworkPolicy and completion Role missing", effectiveTaskSpec{}, false,
			[]string{"*v1.NetworkPolicy", "*v1.Role"}, 2, false},
		{"mounted PVC missing", persistent, true, nil, 1, true},
		{"unmounted PVC not created", persistent, false, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := restoreTask("run-children", kaalmv1beta1.TaskRunning, true, "PodRunning")
			task.Spec.Completion.Condition = ""
			pod := restorePod(t, task, corev1.PodRunning, true)
			if tc.mountsPVC {
				pod.Spec.Volumes = []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: taskPVCName(task.Name)},
				}}}
			}
			cert := desiredTaskCertificate(task, CertLifetime{})
			if err := controllerutil.SetControllerReference(task, cert, testScheme(t)); err != nil {
				t.Fatal(err)
			}
			objs := []client.Object{task, pod, cert}
			for _, obj := range agentReportedChildren(t, task) {
				if !slices.Contains(tc.drop, fmt.Sprintf("%T", obj)) {
					objs = append(objs, obj)
				}
			}
			var writes, ugets int
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
				WithStatusSubresource(&kaalmv1beta1.AgentTask{}).
				WithInterceptorFuncs(countingWrites(&writes, &ugets)).Build()
			r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(8)}
			res, err := r.driveRunning(context.Background(), storedTask(t, c, task), &kaalmv1beta1.AgentClass{}, tc.eff, pod)
			if err != nil {
				t.Fatal(err)
			}
			if res != runningRequeue(task) {
				t.Errorf("result = %+v, want the running requeue", res)
			}
			if writes != tc.creates {
				t.Errorf("writes = %d, want %d", writes, tc.creates)
			}
			err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: taskPVCName(task.Name)},
				&corev1.PersistentVolumeClaim{})
			if gotPVC := err == nil; gotPVC != tc.wantPVC {
				t.Errorf("PVC exists = %v, want %v", gotPVC, tc.wantPVC)
			}
			if p := storedTask(t, c, task).Status.Phase; p != kaalmv1beta1.TaskRunning {
				t.Errorf("phase = %s, want Running", p)
			}
		})
	}
}

// A conflict on one child does not stop the others from coming back on the
// same pass; the task keeps running with Ready=False ChildConflict.
func TestTaskReconcile_ConflictDoesNotStopOtherChildren(t *testing.T) {
	task := restoreTask("run-mixed", kaalmv1beta1.TaskRunning, true, "PodRunning")
	task.Finalizers = []string{kaalmv1beta1.TaskFinalizer}
	class := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "restore-class"}}
	pod := restorePod(t, task, corev1.PodRunning, true)
	foreign := desiredTaskCertificate(task, CertLifetime{})
	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task, class, pod, foreign).
		WithStatusSubresource(&kaalmv1beta1.AgentTask{}).Build()
	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: rec}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != gateRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, gateRequeue)
	}
	got := storedTask(t, c, task)
	expectStoredReady(t, got, metav1.ConditionFalse, kaalmv1beta1.ReasonChildConflict)
	if got.Status.Phase != kaalmv1beta1.TaskRunning {
		t.Errorf("phase = %s, want Running", got.Status.Phase)
	}
	for _, obj := range []client.Object{
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: task.Name}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: taskServiceAccountName(task.Name)}},
	} {
		obj.SetNamespace("default")
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Errorf("%T: %v", obj, err)
		} else if !metav1.IsControlledBy(obj, task) {
			t.Errorf("%T is not controlled by the task", obj)
		}
	}
	var cert cmapi.Certificate
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &cert); err != nil ||
		metav1.GetControllerOf(&cert) != nil {
		t.Errorf("the foreign Certificate was changed: %+v, %v", cert.OwnerReferences, err)
	}
	if got := withPrefix(drainEvents(rec), "Warning "+kaalmv1beta1.ReasonChildConflict); len(got) != 1 {
		t.Errorf("ChildConflict events = %q, want 1", got)
	}
}

// The Provisioning tail restores children from the cache only: it never
// reads the unwatched FQDN policy, which only Running passes restore.
func TestDriveProvisioning_TailRestoresWithoutFQDNRead(t *testing.T) {
	task := restoreTask("prov-nofqdn", kaalmv1beta1.TaskProvisioning, false, "PodProvisioning")
	task.Status.StartTime = nil
	pod := restorePod(t, task, corev1.PodPending, false)
	class := &kaalmv1beta1.AgentClass{}
	class.Spec.Network.Egress.AllowedHosts = []string{"a.example.com"}
	var writes, ugets int
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task, pod).
		WithStatusSubresource(&kaalmv1beta1.AgentTask{}).
		WithInterceptorFuncs(countingWrites(&writes, &ugets)).Build()
	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system",
		FQDNSupport: func() (bool, error) { return true, nil }}
	res, err := r.driveProvisioning(context.Background(), storedTask(t, c, task), class, effectiveTaskSpec{}, pod)
	if err != nil || res.RequeueAfter != certWaitRequeue {
		t.Fatalf("driveProvisioning = (%+v, %v), want a certWaitRequeue requeue", res, err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: task.Name}, &np); err != nil {
		t.Fatalf("NetworkPolicy not re-created: %v", err)
	}
	if ugets != 0 {
		t.Errorf("unstructured reads = %d, want 0 on the Provisioning tail", ugets)
	}
}

// runningAgentReportedTask is an agentReported Running task whose Pod
// recorded the empty class's egress, with its Pod and Certificate.
func runningAgentReportedTask(t *testing.T, name string) (*kaalmv1beta1.AgentTask, *corev1.Pod, *cmapi.Certificate) {
	t.Helper()
	task := restoreTask(name, kaalmv1beta1.TaskRunning, true, "PodRunning")
	task.Spec.Completion.Condition = ""
	task.Status.ClassEgress = &kaalmv1beta1.AgentClassEgress{}
	pod := restorePod(t, task, corev1.PodRunning, true)
	cert := desiredTaskCertificate(task, CertLifetime{})
	if err := controllerutil.SetControllerReference(task, cert, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	return task, pod, cert
}

// The still-running pass reverts a drifted NetworkPolicy, Role, or
// RoleBinding with one write, leaves the ConfigMap's data alone, and a
// second pass writes nothing.
func TestDriveRunning_RevertsDriftedChildrenOnly(t *testing.T) {
	cases := []struct {
		name   string
		drift  func(obj client.Object)
		writes int
	}{
		{"all present and equal", func(client.Object) {}, 0},
		{"NetworkPolicy spec drifted", func(obj client.Object) {
			if np, ok := obj.(*networkingv1.NetworkPolicy); ok {
				np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
					To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
				})
			}
		}, 1},
		{"Role rules drifted", func(obj client.Object) {
			if role, ok := obj.(*rbacv1.Role); ok {
				role.Rules[0].Verbs = append(role.Rules[0].Verbs, "get")
			}
		}, 1},
		{"RoleBinding subjects drifted", func(obj client.Object) {
			if rb, ok := obj.(*rbacv1.RoleBinding); ok {
				rb.Subjects = append(rb.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "x", Namespace: "default"})
			}
		}, 1},
		{"RoleBinding roleRef differs", func(obj client.Object) {
			if rb, ok := obj.(*rbacv1.RoleBinding); ok {
				rb.RoleRef.Name = "cluster-admin"
			}
		}, 2},
		{"ConfigMap data changed", func(obj client.Object) {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == taskCompletionCMName("run-drift") {
				cm.Data = map[string]string{"status": "success"}
			}
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task, pod, cert := runningAgentReportedTask(t, "run-drift")
			objs := []client.Object{task, pod, cert}
			for _, obj := range agentReportedChildren(t, task) {
				tc.drift(obj)
				objs = append(objs, obj)
			}
			var writes, ugets int
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
				WithStatusSubresource(&kaalmv1beta1.AgentTask{}).
				WithInterceptorFuncs(countingWrites(&writes, &ugets)).Build()
			r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(8)}
			drive := func() {
				t.Helper()
				if _, err := r.driveRunning(context.Background(), storedTask(t, c, task),
					&kaalmv1beta1.AgentClass{}, effectiveTaskSpec{}, pod); err != nil {
					t.Fatal(err)
				}
			}
			drive()
			if writes != tc.writes {
				t.Errorf("writes = %d, want %d", writes, tc.writes)
			}
			for _, want := range agentReportedChildren(t, task) {
				got := want.DeepCopyObject().(client.Object)
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(want), got); err != nil {
					t.Fatalf("%T: %v", want, err)
				}
				if !metav1.IsControlledBy(got, task) {
					t.Errorf("%T is not controlled by the task", got)
				}
				switch g := got.(type) {
				case *networkingv1.NetworkPolicy:
					if !equality.Semantic.DeepEqual(g.Spec, want.(*networkingv1.NetworkPolicy).Spec) {
						t.Errorf("NetworkPolicy spec = %+v, want the desired spec", g.Spec)
					}
				case *rbacv1.Role:
					if !equality.Semantic.DeepEqual(g.Rules, want.(*rbacv1.Role).Rules) {
						t.Errorf("Role rules = %+v", g.Rules)
					}
				case *rbacv1.RoleBinding:
					w := want.(*rbacv1.RoleBinding)
					if g.RoleRef != w.RoleRef || !equality.Semantic.DeepEqual(g.Subjects, w.Subjects) {
						t.Errorf("RoleBinding = %+v %+v", g.RoleRef, g.Subjects)
					}
				case *corev1.ConfigMap:
					if tc.name == "ConfigMap data changed" && g.Data["status"] != "success" {
						t.Errorf("ConfigMap data = %v, want the payload kept", g.Data)
					}
				}
			}
			writes = 0
			drive()
			if writes != 0 {
				t.Errorf("second pass writes = %d, want 0", writes)
			}
		})
	}
}

// A task whose Pod predates status.classEgress keeps edits to its
// NetworkPolicy; its Role is still reverted.
func TestDriveRunning_LegacyTaskKeepsPolicyEdits(t *testing.T) {
	task, pod, cert := runningAgentReportedTask(t, "run-legacy")
	task.Status.ClassEgress = nil
	objs := []client.Object{task, pod, cert}
	for _, obj := range agentReportedChildren(t, task) {
		switch o := obj.(type) {
		case *networkingv1.NetworkPolicy:
			o.Spec.Egress = o.Spec.Egress[:1]
		case *rbacv1.Role:
			o.Rules[0].Verbs = append(o.Rules[0].Verbs, "get")
		}
		objs = append(objs, obj)
	}
	var writes, ugets int
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.AgentTask{}).
		WithInterceptorFuncs(countingWrites(&writes, &ugets)).Build()
	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(8)}
	if _, err := r.driveRunning(context.Background(), storedTask(t, c, task),
		&kaalmv1beta1.AgentClass{}, effectiveTaskSpec{}, pod); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Errorf("writes = %d, want 1 (the Role)", writes)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: task.Name}, &np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Egress) != 1 {
		t.Errorf("legacy NetworkPolicy egress = %+v, want the edit kept", np.Spec.Egress)
	}
}

// Before a Pod exists, the NetworkPolicy is brought to the class as it now
// stands, so the new Pod's record matches the policy it runs under.
func TestEnsureTaskChildren_ConvergesToCurrentClass(t *testing.T) {
	task := restoreTask("converge-np", kaalmv1beta1.TaskProvisioning, false, "CertificateNotReady")
	classA := &kaalmv1beta1.AgentClass{}
	classA.Spec.Network.Egress.AllowedCIDRs = []string{"10.1.0.0/16"}
	classB := &kaalmv1beta1.AgentClass{}
	classB.Spec.Network.Egress.AllowedCIDRs = []string{"10.2.0.0/16"}
	old := desiredTaskNetworkPolicy(task, classA.Spec.Network.Egress.AllowedCIDRs, "kaalm-system", DNSSelector{})
	if err := controllerutil.SetControllerReference(task, old, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	var writes, ugets int
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(old).
		WithInterceptorFuncs(countingWrites(&writes, &ugets)).Build()
	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system"}
	if err := r.ensureTaskChildren(context.Background(), task, classB, effectiveTaskSpec{}); err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(old), &np); err != nil {
		t.Fatal(err)
	}
	want := desiredTaskNetworkPolicy(task, classB.Spec.Network.Egress.AllowedCIDRs, "kaalm-system", DNSSelector{})
	if !equality.Semantic.DeepEqual(np.Spec, want.Spec) {
		t.Errorf("NetworkPolicy spec = %+v, want the current class's", np.Spec)
	}
	if writes != 2 { // the ServiceAccount create and the NetworkPolicy update
		t.Errorf("writes = %d, want 2", writes)
	}
}

// readyTaskCert is a task Certificate that cert-manager reports Ready.
func readyTaskCert(t *testing.T, task *kaalmv1beta1.AgentTask) *cmapi.Certificate {
	t.Helper()
	cert := desiredTaskCertificate(task, CertLifetime{})
	cert.Status.Conditions = []cmapi.CertificateCondition{{
		Type: cmapi.CertificateConditionReady, Status: cmmeta.ConditionTrue, Reason: "Issued",
	}}
	if err := controllerutil.SetControllerReference(task, cert, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	return cert
}

// The status write that records a new Pod records the class's egress lists
// too, as a copy; so does the repair of a lost Pod name.
func TestDriveProvisioning_RecordsClassEgress(t *testing.T) {
	ctx := context.Background()
	t.Run("Pod creation", func(t *testing.T) {
		task := restoreTask("rec-egress", kaalmv1beta1.TaskProvisioning, false, "CertificateNotReady")
		task.Status.PodName, task.Status.StartTime = "", nil
		class := &kaalmv1beta1.AgentClass{}
		class.Spec.Network.Egress = kaalmv1beta1.AgentClassEgress{
			AllowedCIDRs: []string{"10.0.0.0/8"}, AllowedHosts: []string{"a.example.com"},
		}
		r, c := restoreReconciler(t, task, readyTaskCert(t, task))
		if _, err := r.driveProvisioning(ctx, storedTask(t, c, task), class, effectiveTaskSpec{Image: "x"}, nil); err != nil {
			t.Fatal(err)
		}
		class.Spec.Network.Egress.AllowedCIDRs[0] = "0.0.0.0/0"
		got := storedTask(t, c, task).Status.ClassEgress
		if got == nil || !slices.Equal(got.AllowedCIDRs, []string{"10.0.0.0/8"}) ||
			!slices.Equal(got.AllowedHosts, []string{"a.example.com"}) {
			t.Errorf("classEgress = %+v, want a copy of the class's lists", got)
		}
	})
	t.Run("empty class", func(t *testing.T) {
		task := restoreTask("rec-egress-empty", kaalmv1beta1.TaskProvisioning, false, "CertificateNotReady")
		task.Status.PodName, task.Status.StartTime = "", nil
		r, c := restoreReconciler(t, task, readyTaskCert(t, task))
		if _, err := r.driveProvisioning(ctx, storedTask(t, c, task), &kaalmv1beta1.AgentClass{},
			effectiveTaskSpec{Image: "x"}, nil); err != nil {
			t.Fatal(err)
		}
		if got := storedTask(t, c, task).Status.ClassEgress; got == nil {
			t.Error("classEgress = nil, want an empty record")
		}
	})
	t.Run("lost Pod name", func(t *testing.T) {
		task := restoreTask("rec-egress-lost", kaalmv1beta1.TaskProvisioning, false, "PodProvisioning")
		task.Status.PodName, task.Status.StartTime = "", nil
		pod := restorePod(t, task, corev1.PodPending, false)
		class := &kaalmv1beta1.AgentClass{}
		class.Spec.Network.Egress.AllowedCIDRs = []string{"10.3.0.0/16"}
		r, c := restoreReconciler(t, task, pod)
		if _, err := r.driveProvisioning(ctx, storedTask(t, c, task), class, effectiveTaskSpec{}, pod); err != nil {
			t.Fatal(err)
		}
		got := storedTask(t, c, task).Status.ClassEgress
		if got == nil || !slices.Equal(got.AllowedCIDRs, []string{"10.3.0.0/16"}) {
			t.Errorf("classEgress = %+v, want the class's CIDRs", got)
		}
	})
}

// A Running pass reverts a drifted FQDN policy to the hosts recorded when
// the Pod was created.
func TestDriveRunning_RevertsFQDNPolicyToRecord(t *testing.T) {
	task, pod, cert := runningAgentReportedTask(t, "run-fqdn-drift")
	task.Spec.Completion.Condition = completionExitCode
	task.Status.ClassEgress = &kaalmv1beta1.AgentClassEgress{AllowedHosts: []string{"a.example.com"}}
	drifted := desiredFQDNPolicy(task, taskPodLabels(task), []string{"evil.example.com"}, DNSSelector{})
	if err := controllerutil.SetControllerReference(task, drifted, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	objs := []client.Object{task, pod, cert, drifted}
	np := desiredTaskNetworkPolicy(task, nil, "kaalm-system", DNSSelector{})
	sa := desiredTaskServiceAccount(task)
	for _, obj := range []client.Object{np, sa} {
		if err := controllerutil.SetControllerReference(task, obj, testScheme(t)); err != nil {
			t.Fatal(err)
		}
		objs = append(objs, obj)
	}
	var writes, ugets int
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&kaalmv1beta1.AgentTask{}).
		WithInterceptorFuncs(countingWrites(&writes, &ugets)).Build()
	r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(8),
		FQDNSupport: func() (bool, error) { return true, nil }}
	// The class now lists other hosts; the record wins.
	class := &kaalmv1beta1.AgentClass{}
	class.Spec.Network.Egress.AllowedHosts = []string{"b.example.com"}
	if _, err := r.driveRunning(context.Background(), storedTask(t, c, task), class, effectiveTaskSpec{}, pod); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Errorf("writes = %d, want 1", writes)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ciliumPolicyGVK)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(drifted), u); err != nil {
		t.Fatal(err)
	}
	want := desiredFQDNPolicy(task, taskPodLabels(task), []string{"a.example.com"}, DNSSelector{})
	if !equality.Semantic.DeepEqual(u.Object["spec"], want.Object["spec"]) {
		t.Errorf("FQDN policy spec = %v, want the recorded hosts", u.Object["spec"])
	}
}
