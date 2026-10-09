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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// A Provisioning task re-checks every certWaitRequeue while it waits for a
// previous Pod to terminate or for its Pod to turn Ready. A re-check that finds nothing new writes no status.
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
