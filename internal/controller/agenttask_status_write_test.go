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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// A Provisioning task re-checks every certWaitRequeue while it waits: for
// its Certificate, for a previous Pod to terminate, or for its Pod to turn
// Ready. A re-check that finds nothing new writes no status.
func TestDriveProvisioning_WaitingRecheckSkipsUnchangedStatus(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, task *kaalmv1beta1.AgentTask) (*corev1.Pod, []client.Object)
		ready string
	}{
		{"Pod not Ready yet", func(t *testing.T, task *kaalmv1beta1.AgentTask) (*corev1.Pod, []client.Object) {
			pod := restorePod(t, task, corev1.PodPending, false)
			return pod, []client.Object{pod}
		}, "PodProvisioning"},
		{"previous Pod terminating", func(t *testing.T, task *kaalmv1beta1.AgentTask) (*corev1.Pod, []client.Object) {
			pod := restorePod(t, task, corev1.PodRunning, false)
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			return pod, nil
		}, "PodTerminating"},
		{"Certificate not Ready", func(t *testing.T, task *kaalmv1beta1.AgentTask) (*corev1.Pod, []client.Object) {
			task.Status.PodName = ""
			return nil, nil
		}, kaalmv1beta1.ReasonCertificateNotReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			task := restoreTask("wait-write", kaalmv1beta1.TaskProvisioning, false, "seeded")
			task.Status.StartTime = nil
			pod, objs := tc.setup(t, task)
			writes := &statusWrites{}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(append(objs, task)...).
				WithStatusSubresource(&kaalmv1beta1.AgentTask{}).
				WithInterceptorFuncs(writes.funcs()).Build()
			r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: record.NewFakeRecorder(16)}
			class := &kaalmv1beta1.AgentClass{}

			for range 2 {
				res, err := r.driveProvisioning(ctx, storedTask(t, c, task), class, effectiveTaskSpec{Image: "x"}, pod)
				if err != nil || res.RequeueAfter != certWaitRequeue {
					t.Fatalf("driveProvisioning = (%+v, %v), want a certWaitRequeue requeue", res, err)
				}
			}
			if n := writes.count(); n != 1 {
				t.Errorf("two re-checks made %d status writes, want 1", n)
			}
			expectStoredReady(t, storedTask(t, c, task), metav1.ConditionFalse, tc.ready)
		})
	}
}

// A task held at Ready=False re-checks every notReadyRecheck until its cause
// clears. A re-check that finds the same cause writes no status and emits
// no second event.
func TestAgentTask_HeldRecheckSkipsUnchangedStatus(t *testing.T) {
	rejected := &ChildWriteRejectedError{
		Op: "creating", Kind: "NetworkPolicy", Name: "held-write",
		Err: apierrors.NewForbidden(schema.GroupResource{Resource: "networkpolicies"}, "held-write",
			errors.New("denied by policy webhook")),
	}
	cases := []struct {
		name   string
		reason string
		step   func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error
	}{{
		name: "ImagePullSecretMissing", reason: kaalmv1beta1.ReasonImagePullSecretMissing,
		step: func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error {
			return r.markTaskNotReady(context.Background(), task, kaalmv1beta1.ReasonImagePullSecretMissing,
				`imagePullSecret "pull" missing in namespace "default"`)
		},
	}, {
		name: "SecretNotOptedIn", reason: kaalmv1beta1.ReasonSecretNotOptedIn,
		step: func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error {
			return r.markTaskNotReady(context.Background(), task, kaalmv1beta1.ReasonSecretNotOptedIn,
				`Secret "creds" is not opted in`)
		},
	}, {
		name: "ChildWriteRejected", reason: kaalmv1beta1.ReasonChildWriteRejected,
		step: func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error {
			_, err := r.childBlocked(context.Background(), task, nil, false, rejected)
			return err
		},
	}, {
		name: "ChildConflict", reason: kaalmv1beta1.ReasonChildConflict,
		step: func(r *AgentTaskReconciler, task *kaalmv1beta1.AgentTask) error {
			_, err := r.childBlocked(context.Background(), task, nil, false,
				&ChildConflictError{Kind: "Service", Name: task.Name, OwnerKind: "AgentTask"})
			return err
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := restoreTask("held-write", kaalmv1beta1.TaskRunning, true, "PodReady")
			writes := &statusWrites{}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task).
				WithStatusSubresource(&kaalmv1beta1.AgentTask{}).
				WithInterceptorFuncs(writes.funcs()).Build()
			rec := record.NewFakeRecorder(16)
			r := &AgentTaskReconciler{Client: c, OperatorNamespace: "kaalm-system", Recorder: rec}

			for range 3 {
				if err := tc.step(r, storedTask(t, c, task)); err != nil {
					t.Fatal(err)
				}
			}
			if n := writes.count(); n != 1 {
				t.Errorf("three passes made %d status writes, want 1", n)
			}
			close(rec.Events)
			var events []string
			for e := range rec.Events {
				events = append(events, e)
			}
			if got := withPrefix(events, "Warning "+tc.reason); len(got) != 1 {
				t.Errorf("events = %q, want one %s warning", events, tc.reason)
			}
			expectStoredReady(t, storedTask(t, c, task), metav1.ConditionFalse, tc.reason)
		})
	}
}
