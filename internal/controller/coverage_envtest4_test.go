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
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// ---- reconcileDelete short-circuits when the finalizer is already gone ----

func TestReconcileDelete_NoFinalizerIsNoop(t *testing.T) {
	ctx := context.Background()
	clean := func(name string, requeue bool, after time.Duration, err error) {
		if err != nil || requeue || after != 0 {
			t.Errorf("%s no-finalizer delete not a clean no-op: requeue=%v after=%v err=%v", name, requeue, after, err)
		}
	}

	mpRes, err := (&ModelProviderReconciler{Client: testClient}).reconcileDelete(ctx,
		&agentryv1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "x"}})
	clean("ModelProvider", mpRes.Requeue, mpRes.RequeueAfter, err)

	acRes, err := (&AgentClassReconciler{Client: testClient}).reconcileDelete(ctx,
		&agentryv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "x"}})
	clean("AgentClass", acRes.Requeue, acRes.RequeueAfter, err)

	taskRes, err := (&AgentTaskReconciler{Client: testClient}).reconcileDelete(ctx,
		&agentryv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}})
	clean("AgentTask", taskRes.Requeue, taskRes.RequeueAfter, err)

	agRes, err := (&AgentReconciler{Client: testClient}).reconcileDelete(ctx,
		&agentryv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}})
	clean("Agent", agRes.Requeue, agRes.RequeueAfter, err)
}

// ---- AgentChannel: system-namespace guard ----

func TestChannel_SystemNamespaceForbidden(t *testing.T) {
	ch := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch-sys", Namespace: testSystemNamespace},
		Spec: agentryv1alpha1.AgentChannelSpec{
			AgentRef: agentryv1alpha1.LocalObjectReference{Name: "whatever"},
			Webhook: agentryv1alpha1.AgentChannelWebhook{
				Path: "/channels/" + testSystemNamespace + "/ch-sys",
				Auth: agentryv1alpha1.ChannelAuth{
					Type:      "bearer",
					SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "s", Key: "token"},
				},
			},
		},
	}
	if err := testClient.Create(ctxT(), ch); err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, func() error {
		var got agentryv1alpha1.AgentChannel
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Namespace: testSystemNamespace, Name: "ch-sys"}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, agentryv1alpha1.ConditionReady)
		if c == nil || c.Reason != agentryv1alpha1.ReasonSystemNamespaceForbidden {
			return errString("SystemNamespaceForbidden not set")
		}
		return nil
	})
}

// ---- AgentChannel: HMAC secret missing fails validation ----

func TestChannel_HMACSecretMissing(t *testing.T) {
	mkWorkloadClass(t, "chc-hmac", nil)
	mkWorkloadAgent(t, "ch-agent-hmac", "chc-hmac", nil)
	mkChannelSecret(t, "ch-hmac-inbound")
	mkChannel(t, "ch-hmac", "ch-agent-hmac", "/channels/default/ch-hmac", func(ch *agentryv1alpha1.AgentChannel) {
		ch.Spec.Webhook.Auth.SecretRef = &agentryv1alpha1.SecretKeyReference{Name: "ch-hmac-inbound", Key: "token"}
		ch.Spec.Webhook.Auth.HMAC = &agentryv1alpha1.ChannelHMAC{
			Header:    "X-Sig",
			SecretRef: agentryv1alpha1.SecretKeyReference{Name: "ch-hmac-missing", Key: "token"},
		}
	})
	expectChannelReady(t, "ch-hmac", metav1.ConditionFalse, agentryv1alpha1.ReasonCredentialsMissing)
}

// ---- Agent: a provider outside the class allowlist degrades ----

func TestAgent_ProviderNotAllowedDegrades(t *testing.T) {
	mkWorkloadClass(t, "wc-provdenied", nil) // empty allowedProviders => none allowed
	mkWorkloadAgent(t, "provdenied-agent", "wc-provdenied", func(ag *agentryv1alpha1.Agent) {
		ag.Spec.Providers = []agentryv1alpha1.AgentProviderReference{
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "some-prov"}},
		}
	})
	expectAgentPhase(t, "provdenied-agent", agentryv1alpha1.AgentDegraded)
	expectAgentReadyReason(t, "provdenied-agent", agentryv1alpha1.ReasonClassConstraintViolation)
}

// ---- AgentTask: missing class and empty image are terminal/gated ----

func TestTask_MissingClassIsNotReady(t *testing.T) {
	mkTask(t, "t-noclass", "ghost-class", nil)
	eventually(t, func() error {
		var task agentryv1alpha1.AgentTask
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-noclass"}, &task); err != nil {
			return err
		}
		c := condition(task.Status.Conditions, agentryv1alpha1.ConditionReady)
		if c == nil || c.Reason != agentryv1alpha1.ReasonInvalidReference {
			return errString("InvalidReference not set")
		}
		return nil
	})
}

func TestTask_EmptyImageIsNotReady(t *testing.T) {
	mkWorkloadClass(t, "tc-noimg", nil) // no defaultImage
	mkTask(t, "t-noimg", "tc-noimg", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Image = ""
	})
	eventually(t, func() error {
		var task agentryv1alpha1.AgentTask
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-noimg"}, &task); err != nil {
			return err
		}
		c := condition(task.Status.Conditions, agentryv1alpha1.ConditionReady)
		if c == nil || c.Reason != agentryv1alpha1.ReasonInvalidReference {
			return errString("InvalidReference not set")
		}
		return nil
	})
}

// ---- AgentTask: a provider in the allowlist whose CR is absent fails ----

func TestTask_ProviderCRMissingIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-provghost", func(ac *agentryv1alpha1.AgentClass) {
		ac.Spec.AllowedProviders = []agentryv1alpha1.LocalObjectReference{{Name: "task-ghost-prov"}}
	})
	mkTask(t, "t-provghost", "tc-provghost", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Providers = []agentryv1alpha1.AgentProviderReference{
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "task-ghost-prov"}},
		}
	})
	expectTaskPhase(t, "t-provghost", agentryv1alpha1.TaskFailed)
}

// ---- AgentTask: the provisioning deadline fails a Pod that never starts ----

func TestTask_ProvisioningDeadlineFails(t *testing.T) {
	old := provisioningDeadline
	provisioningDeadline = 1 * time.Second
	defer func() { provisioningDeadline = old }()

	mkWorkloadClass(t, "tc-deadline", nil)
	mkTask(t, "t-deadline", "tc-deadline", func(task *agentryv1alpha1.AgentTask) {
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
	expectTaskPhase(t, "t-deadline", agentryv1alpha1.TaskFailed)
}

// ---- Agent convergePod: a terminating Pod holds the agent in Provisioning ----

func TestAgent_TerminatingPodHoldsProvisioning(t *testing.T) {
	mkWorkloadClass(t, "wc-terming", nil)
	pod := provisionRunningAgent(t, "terming-agent", "wc-terming")

	// A finalizer keeps the Pod present with a deletionTimestamp set (envtest
	// removes unscheduled Pods instantly otherwise), so convergePod observes a
	// replacement in progress and holds the agent in Provisioning.
	const fin = "test.agentry.io/hold"
	eventually(t, func() error {
		var got corev1.Pod
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got); err != nil {
			return err
		}
		got.Finalizers = append(got.Finalizers, fin)
		return testClient.Update(ctxT(), &got)
	})
	if err := testClient.Delete(ctxT(), pod); err != nil {
		t.Fatalf("graceful delete: %v", err)
	}
	touchAgent(t, "terming-agent")
	expectAgentPhase(t, "terming-agent", agentryv1alpha1.AgentProvisioning)
	expectAgentReadyReason(t, "terming-agent", "PodProvisioning")

	// Release the finalizer so the Pod can be reaped.
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		got.Finalizers = nil
		return testClient.Update(ctxT(), &got)
	})
}

// ---- Agent ensureService: a drifted Service is reconciled back ----

func TestAgent_ServicePortDriftReconciled(t *testing.T) {
	mkWorkloadClass(t, "wc-svcdrift", nil)
	provisionRunningAgent(t, "svcdrift-agent", "wc-svcdrift")

	var svc corev1.Service
	key := types.NamespacedName{Namespace: "default", Name: "svcdrift-agent"}
	eventually(t, func() error { return testClient.Get(ctxT(), key, &svc) })
	wantPort := svc.Spec.Ports[0].Port

	// Drift the Service port out of band.
	svc.Spec.Ports[0].Port = 9999
	if err := testClient.Update(ctxT(), &svc); err != nil {
		t.Fatalf("drift service: %v", err)
	}
	touchAgent(t, "svcdrift-agent")

	eventually(t, func() error {
		var got corev1.Service
		if err := testClient.Get(ctxT(), key, &got); err != nil {
			return err
		}
		if got.Spec.Ports[0].Port != wantPort {
			return errString("service port not reconciled back")
		}
		return nil
	})
}

// ---- ModelProvider: budget re-reconcile cadence with the health probe off ----

func TestModelProvider_BudgetRequeueWithoutProbe(t *testing.T) {
	mkSecret(t, "mp-budreq-key")
	mkProvider(t, "mp-budreq", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mp-budreq-key", Key: "token"}
		mp.Spec.HealthCheck = &agentryv1alpha1.ModelProviderHealthCheck{Enabled: false}
		mp.Spec.Budget = agentryv1alpha1.ModelProviderBudget{Period: "monthly", PerNamespaceUSD: "100"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-budreq"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, agentryv1alpha1.ReasonCredentialsValid)
}
