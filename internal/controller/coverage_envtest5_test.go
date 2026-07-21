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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// TestTask_ExitCodeFailsBeforeReady covers driveProvisioning's terminal-before-
// Ready branch for an exitCode task whose Pod fails outright.
func TestTask_ExitCodeFailsBeforeReady(t *testing.T) {
	mkWorkloadClass(t, "tc-failearly", nil)
	mkTask(t, "t-failearly", "tc-failearly", func(task *agentryv1alpha1.AgentTask) {
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
	expectTaskPhase(t, "t-failearly", agentryv1alpha1.TaskFailed)
}

// TestTask_CrashInterruptedRetryResumes covers the Reconcile entry that resumes
// a Failed task with no completionTime (a retry interrupted mid-sequence).
func TestTask_CrashInterruptedRetryResumes(t *testing.T) {
	mkWorkloadClass(t, "tc-resume", nil)
	provisionRunningTask(t, "t-resume", "tc-resume", nil)

	// Simulate the crash-interrupted state: Failed, but not yet settled.
	eventually(t, func() error {
		task := getTask(t, "t-resume")
		task.Status.Phase = agentryv1alpha1.TaskFailed
		task.Status.CompletionTime = nil
		return testClient.Status().Update(ctxT(), task)
	})
	// The reconciler resumes it; the still-ready Pod carries it back to Running.
	expectTaskPhase(t, "t-resume", agentryv1alpha1.TaskRunning)
}

// TestReadMailbox_NotFoundIsEmpty covers readMailbox's NotFound path directly.
func TestReadMailbox_NotFoundIsEmpty(t *testing.T) {
	r := &AgentTaskReconciler{Client: testClient}
	task := &agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "no-mailbox", Namespace: "default"}}
	payload, err := r.readMailbox(ctxT(), task)
	if err != nil {
		t.Fatalf("missing mailbox must not error: %v", err)
	}
	if payload.Status != "" {
		t.Errorf("absent mailbox must parse to an empty payload: %+v", payload)
	}
}

// TestChannel_PruneSkipsNonAsyncConfigMap covers the prune loop's skip of a
// labeled ConfigMap that is not an async-response record.
func TestChannel_PruneSkipsNonAsyncConfigMap(t *testing.T) {
	mkWorkloadClass(t, "chc-skip", nil)
	mkWorkloadAgent(t, "ch-agent-skip", "chc-skip", nil)
	mkChannelSecret(t, "ch-skip-secret")

	// A ConfigMap carrying this channel's labels but NOT the agentry-async- name
	// prefix: the prune must leave it untouched.
	other := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unrelated-config", Namespace: testSystemNamespace,
			Labels: map[string]string{
				agentryv1alpha1.LabelChannelNamespace: "default",
				agentryv1alpha1.LabelChannelName:      "ch-skip",
			},
			Annotations: map[string]string{
				agentryv1alpha1.AnnotationExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}
	if err := testClient.Create(ctxT(), other); err != nil {
		t.Fatalf("create configmap: %v", err)
	}

	mkChannel(t, "ch-skip", "ch-agent-skip", "/channels/default/ch-skip", nil)
	expectChannelReady(t, "ch-skip", metav1.ConditionTrue, "")

	// Give the reconciler time to run a prune pass, then confirm survival.
	time.Sleep(500 * time.Millisecond)
	var got corev1.ConfigMap
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: testSystemNamespace, Name: "unrelated-config"}, &got); err != nil {
		t.Errorf("a non-async ConfigMap must not be pruned: %v", err)
	}
}
