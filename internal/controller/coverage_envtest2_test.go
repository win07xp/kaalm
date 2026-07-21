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
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// ---- AgentTask pre-Pod violations (taskViolation) ----

func TestTask_ImageNotAllowedIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-img", nil) // allows registry.test/agents/*
	mkTask(t, "t-img", "tc-img", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Image = "evil.example/x:v1"
	})
	expectTaskPhase(t, "t-img", agentryv1alpha1.TaskFailed)
	c := condition(getTask(t, "t-img").Status.Conditions, agentryv1alpha1.ConditionCompleted)
	if c == nil || c.Reason != agentryv1alpha1.ReasonClassConstraintViolation {
		t.Errorf("Completed condition wrong: %+v", c)
	}
}

func TestTask_ProviderNotAllowedIsTerminalFailed(t *testing.T) {
	mkWorkloadClass(t, "tc-prov", nil) // empty allowedProviders => none allowed
	mkTask(t, "t-prov", "tc-prov", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Providers = []agentryv1alpha1.AgentProviderReference{
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "nope"}},
		}
	})
	expectTaskPhase(t, "t-prov", agentryv1alpha1.TaskFailed)
}

func TestTask_ProviderNamespaceDeniedIsTerminalFailed(t *testing.T) {
	mkSecret(t, "t-provns-key")
	mkProvider(t, "t-provns", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "t-provns-key", Key: "token"}
		mp.Spec.AllowedNamespaces = []string{"team-*"}
	})
	mkWorkloadClass(t, "tc-provns", func(ac *agentryv1alpha1.AgentClass) {
		ac.Spec.AllowedProviders = []agentryv1alpha1.LocalObjectReference{{Name: "t-provns"}}
	})
	mkTask(t, "t-provns-task", "tc-provns", func(task *agentryv1alpha1.AgentTask) {
		task.Spec.Providers = []agentryv1alpha1.AgentProviderReference{
			{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: "t-provns"}},
		}
	})
	// The task's namespace (default) does not match team-*.
	expectTaskPhase(t, "t-provns-task", agentryv1alpha1.TaskFailed)
}

func TestTask_ImagePullSecretMissingGates(t *testing.T) {
	mkWorkloadClass(t, "tc-pull", func(ac *agentryv1alpha1.AgentClass) {
		ac.Spec.Image.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "tc-pull-creds"}}
	})
	mkTask(t, "t-pull", "tc-pull", nil)
	readyReason := func() (string, error) {
		var task agentryv1alpha1.AgentTask
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "t-pull"}, &task); err != nil {
			return "", err
		}
		c := condition(task.Status.Conditions, agentryv1alpha1.ConditionReady)
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
		if r != agentryv1alpha1.ReasonImagePullSecretMissing {
			return errString("ImagePullSecretMissing not set, got " + r)
		}
		return nil
	})
	// Creating the Secret recovers the gate and provisioning proceeds.
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
		if r == agentryv1alpha1.ReasonImagePullSecretMissing {
			return errString("still gated on pull secret")
		}
		return nil
	})
}

// ---- Agent convergePod: crash loop -> Failed ----

func TestAgent_CrashLoopMarksFailed(t *testing.T) {
	mkWorkloadClass(t, "wc-crash", nil)
	pod := provisionRunningAgent(t, "crash-agent", "wc-crash")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "agent",
		RestartCount: crashLoopThreshold,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff", Message: "back-off restarting",
		}},
	}}
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	expectAgentPhase(t, "crash-agent", agentryv1alpha1.AgentFailed)
	expectAgentReadyReason(t, "crash-agent", "CrashLoopBackOff")
}

// ---- Agent convergePod: terminal Pod is re-provisioned ----

func TestAgent_TerminalPodReprovisions(t *testing.T) {
	mkWorkloadClass(t, "wc-term", nil)
	pod := provisionRunningAgent(t, "term-agent", "wc-term")
	pod.Status.Phase = corev1.PodFailed
	if err := testClient.Status().Update(ctxT(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	// The reconciler deletes the terminal Pod; finish its termination.
	eventually(t, func() error {
		var got corev1.Pod
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: pod.Name}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !got.DeletionTimestamp.IsZero() {
			forceDeletePod(t, &got)
		}
		return errString("terminal pod still present")
	})
	// A fresh Pod is provisioned.
	eventually(t, func() error {
		newPod := agentPod(t, "term-agent")
		if newPod == nil || newPod.Name == pod.Name {
			return errString("no replacement pod yet")
		}
		return nil
	})
}

// ---- AgentChannel ensureCredentialRole: secret refs change -> Role updated ----

func TestChannel_CredentialRoleGrowsWithSecretRefs(t *testing.T) {
	mkWorkloadClass(t, "chc-role", nil)
	mkWorkloadAgent(t, "ch-agent-role", "chc-role", nil)
	mkChannelSecret(t, "ch-role-secret")
	mkChannel(t, "ch-role", "ch-agent-role", "/channels/default/ch-role", nil)
	expectChannelReady(t, "ch-role", metav1.ConditionTrue, "")

	roleName := "agentry-channel-ch-role-creds"
	eventually(t, func() error {
		var role rbacv1.Role
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Namespace: "default", Name: roleName}, &role); err != nil {
			return err
		}
		if len(role.Rules) != 1 || len(role.Rules[0].ResourceNames) != 1 {
			return errString("initial role not scoped to one secret")
		}
		return nil
	})

	// Add an HMAC secret ref: the Role's resourceNames must grow to include it.
	mkChannelSecret(t, "ch-role-hmac")
	eventually(t, func() error {
		var ch agentryv1alpha1.AgentChannel
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-role"}, &ch); err != nil {
			return err
		}
		ch.Spec.Webhook.Auth.HMAC = &agentryv1alpha1.ChannelHMAC{
			Header:    "X-Sig",
			SecretRef: agentryv1alpha1.SecretKeyReference{Name: "ch-role-hmac", Key: "token"},
		}
		return testClient.Update(ctxT(), &ch)
	})
	eventually(t, func() error {
		var role rbacv1.Role
		if err := testClient.Get(ctxT(),
			types.NamespacedName{Namespace: "default", Name: roleName}, &role); err != nil {
			return err
		}
		names := append([]string(nil), role.Rules[0].ResourceNames...)
		sort.Strings(names)
		if len(names) != 2 || names[0] != "ch-role-hmac" || names[1] != "ch-role-secret" {
			return errString("role resourceNames did not grow to both secrets")
		}
		return nil
	})
}

// ---- ModelProvider credential: key present but missing/empty ----

func TestModelProvider_CredentialKeyMissing(t *testing.T) {
	// Secret exists but lacks the referenced key.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mp-wrongkey-secret", Namespace: testOperatorNamespace},
		Data:       map[string][]byte{"other": []byte("x")},
	}
	if err := testClient.Create(ctxT(), sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create secret: %v", err)
	}
	mkProvider(t, "mp-wrongkey", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "mp-wrongkey-secret", Key: "token"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-wrongkey"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonCredentialsMissing)
}
