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
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

func mkChannel(t *testing.T, name, agentName, path string, mutate func(*agentryv1alpha1.AgentChannel)) {
	t.Helper()
	ch := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agentryv1alpha1.AgentChannelSpec{
			AgentRef: agentryv1alpha1.LocalObjectReference{Name: agentName},
			Webhook: agentryv1alpha1.AgentChannelWebhook{
				Path: path,
				Auth: agentryv1alpha1.ChannelAuth{
					Type:      "bearer",
					SecretRef: &agentryv1alpha1.SecretKeyReference{Name: name + "-secret", Key: "token"},
				},
			},
		},
	}
	if mutate != nil {
		mutate(ch)
	}
	if err := testClient.Create(ctxT(), ch); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
}

func mkChannelSecret(t *testing.T, name string) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("hook-token")},
	}
	if err := testClient.Create(ctxT(), sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create channel secret: %v", err)
	}
}

func expectChannelReady(t *testing.T, name string, want metav1.ConditionStatus, reason string) {
	t.Helper()
	eventually(t, func() error {
		var ch agentryv1alpha1.AgentChannel
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: name}, &ch); err != nil {
			return err
		}
		c := condition(ch.Status.Conditions, agentryv1alpha1.ConditionReady)
		if c == nil {
			return errString("no Ready condition yet")
		}
		if c.Status != want {
			return errString("Ready=" + string(c.Status) + " want " + string(want) + " reason=" + c.Reason)
		}
		if reason != "" && c.Reason != reason {
			return errString("reason=" + c.Reason + " want " + reason)
		}
		return nil
	})
}

func TestChannel_ValidBecomesReady(t *testing.T) {
	mkWorkloadClass(t, "chc-ok", nil)
	mkWorkloadAgent(t, "ch-agent-ok", "chc-ok", nil)
	mkChannelSecret(t, "ch-ok-secret")
	mkChannel(t, "ch-ok", "ch-agent-ok", "/channels/default/ch-ok", nil)

	expectChannelReady(t, "ch-ok", metav1.ConditionTrue, agentryv1alpha1.ReasonAgentReachable)

	// The scoped Role exists with exactly the auth Secret, get+watch only.
	var role rbacv1.Role
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "agentry-channel-ch-ok-creds"}, &role); err != nil {
		t.Fatalf("credential Role missing: %v", err)
	}
	rule := role.Rules[0]
	if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "ch-ok-secret" {
		t.Errorf("Role not scoped to the auth Secret: %v", rule.ResourceNames)
	}
	for _, v := range rule.Verbs {
		if v == "list" || v == "create" || v == "delete" {
			t.Errorf("Role must grant get/watch only, found %q", v)
		}
	}
	var rb rbacv1.RoleBinding
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: "default", Name: "agentry-channel-ch-ok-creds-gateway"}, &rb); err != nil {
		t.Errorf("gateway RoleBinding missing: %v", err)
	}
	// Phase reduces from the Agent (Pending and transients are Active).
	eventually(t, func() error {
		var ch agentryv1alpha1.AgentChannel
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-ok"}, &ch); err != nil {
			return err
		}
		if ch.Status.Phase != agentryv1alpha1.ChannelActive {
			return errString("phase=" + string(ch.Status.Phase))
		}
		return nil
	})
}

func TestChannel_AgentNotFound(t *testing.T) {
	mkChannelSecret(t, "ch-noagent-secret")
	mkChannel(t, "ch-noagent", "no-such-agent", "/channels/default/ch-noagent", nil)
	expectChannelReady(t, "ch-noagent", metav1.ConditionFalse, agentryv1alpha1.ReasonAgentNotFound)
	eventually(t, func() error {
		var ch agentryv1alpha1.AgentChannel
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-noagent"}, &ch); err != nil {
			return err
		}
		if ch.Status.Phase != agentryv1alpha1.ChannelFailed {
			return errString("phase=" + string(ch.Status.Phase) + " want Failed")
		}
		return nil
	})
}

func TestChannel_ServiceDisabled(t *testing.T) {
	mkWorkloadClass(t, "chc-svc", nil)
	mkWorkloadAgent(t, "ch-agent-svc", "chc-svc", func(ag *agentryv1alpha1.Agent) {
		ag.Spec.Service = &agentryv1alpha1.AgentService{Enabled: false}
	})
	mkChannelSecret(t, "ch-svc-secret")
	mkChannel(t, "ch-svc", "ch-agent-svc", "/channels/default/ch-svc", nil)
	expectChannelReady(t, "ch-svc", metav1.ConditionFalse, agentryv1alpha1.ReasonAgentServiceDisabled)
}

func TestChannel_InvalidPathPrefix(t *testing.T) {
	mkWorkloadClass(t, "chc-path", nil)
	mkWorkloadAgent(t, "ch-agent-path", "chc-path", nil)
	mkChannelSecret(t, "ch-path-secret")
	// Wrong namespace segment: rule 15.
	mkChannel(t, "ch-path", "ch-agent-path", "/channels/other-ns/ch-path", nil)
	expectChannelReady(t, "ch-path", metav1.ConditionFalse, agentryv1alpha1.ReasonInvalidPath)
}

func TestChannel_PathConflictNewerLoses(t *testing.T) {
	mkWorkloadClass(t, "chc-conf", nil)
	mkWorkloadAgent(t, "ch-agent-conf", "chc-conf", nil)
	mkChannelSecret(t, "ch-conf-a-secret")
	mkChannelSecret(t, "ch-conf-b-secret")
	mkChannel(t, "ch-conf-a", "ch-agent-conf", "/channels/default/shared-path", nil)
	expectChannelReady(t, "ch-conf-a", metav1.ConditionTrue, "")
	time.Sleep(1100 * time.Millisecond) // distinct creationTimestamps (1s resolution)
	mkChannel(t, "ch-conf-b", "ch-agent-conf", "/channels/default/shared-path", nil)

	expectChannelReady(t, "ch-conf-b", metav1.ConditionFalse, agentryv1alpha1.ReasonPathConflict)
	expectChannelReady(t, "ch-conf-a", metav1.ConditionTrue, "")
}

func TestChannel_CredentialsMissing(t *testing.T) {
	mkWorkloadClass(t, "chc-cred", nil)
	mkWorkloadAgent(t, "ch-agent-cred", "chc-cred", nil)
	mkChannel(t, "ch-cred", "ch-agent-cred", "/channels/default/ch-cred", nil) // secret never created
	expectChannelReady(t, "ch-cred", metav1.ConditionFalse, agentryv1alpha1.ReasonCredentialsMissing)
}

func TestChannel_InvalidCallbackURL(t *testing.T) {
	mkWorkloadClass(t, "chc-cb", nil)
	mkWorkloadAgent(t, "ch-agent-cb", "chc-cb", nil)
	mkChannelSecret(t, "ch-cb-secret")
	badURL := "http://example.com/hook" // not https
	mkChannel(t, "ch-cb", "ch-agent-cb", "/channels/default/ch-cb", func(ch *agentryv1alpha1.AgentChannel) {
		ch.Spec.Webhook.CallbackURL = &badURL
		ch.Spec.Webhook.CallbackAuth = &agentryv1alpha1.ChannelAuth{
			Type:      "bearer",
			SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "ch-cb-secret", Key: "token"},
		}
	})
	expectChannelReady(t, "ch-cb", metav1.ConditionFalse, agentryv1alpha1.ReasonInvalidCallbackURL)
}

func TestChannel_DegradedWhenAgentDegraded(t *testing.T) {
	mkWorkloadClass(t, "chc-deg", nil)
	mkWorkloadAgent(t, "ch-agent-deg", "chc-deg", func(ag *agentryv1alpha1.Agent) {
		// Image outside the allowlist degrades the agent.
		ag.Spec.Image = "evil.example/x:v1"
	})
	expectAgentPhase(t, "ch-agent-deg", agentryv1alpha1.AgentDegraded)
	mkChannelSecret(t, "ch-deg-secret")
	mkChannel(t, "ch-deg", "ch-agent-deg", "/channels/default/ch-deg", nil)
	eventually(t, func() error {
		var ch agentryv1alpha1.AgentChannel
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-deg"}, &ch); err != nil {
			return err
		}
		if ch.Status.Phase != agentryv1alpha1.ChannelDegraded {
			return errString("phase=" + string(ch.Status.Phase) + " want Degraded")
		}
		return nil
	})
}

func TestChannel_PruneExpiredAsyncConfigMaps(t *testing.T) {
	mkWorkloadClass(t, "chc-prune", nil)
	mkWorkloadAgent(t, "ch-agent-prune", "chc-prune", nil)
	mkChannelSecret(t, "ch-prune-secret")

	mkAsyncCM := func(name string, expired bool) {
		expiry := time.Now().Add(time.Hour)
		if expired {
			expiry = time.Now().Add(-time.Hour)
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: testSystemNamespace,
				Labels: map[string]string{
					agentryv1alpha1.LabelChannelNamespace: "default",
					agentryv1alpha1.LabelChannelName:      "ch-prune",
				},
				Annotations: map[string]string{
					agentryv1alpha1.AnnotationExpiresAt: expiry.UTC().Format(time.RFC3339),
				},
			},
			Data: map[string]string{},
		}
		if err := testClient.Create(ctxT(), cm); err != nil {
			t.Fatalf("create async cm: %v", err)
		}
	}
	mkAsyncCM("agentry-async-expired-1", true)
	mkAsyncCM("agentry-async-live-1", false)

	mkChannel(t, "ch-prune", "ch-agent-prune", "/channels/default/ch-prune", nil)
	expectChannelReady(t, "ch-prune", metav1.ConditionTrue, "")

	eventually(t, func() error {
		var cm corev1.ConfigMap
		err := testClient.Get(ctxT(),
			types.NamespacedName{Namespace: testSystemNamespace, Name: "agentry-async-expired-1"}, &cm)
		if !apierrors.IsNotFound(err) {
			return errString("expired record not pruned")
		}
		return nil
	})
	var live corev1.ConfigMap
	if err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: testSystemNamespace, Name: "agentry-async-live-1"}, &live); err != nil {
		t.Errorf("live record must survive the prune: %v", err)
	}
}

func TestChannel_DeleteHandshake(t *testing.T) {
	mkWorkloadClass(t, "chc-del", nil)
	mkWorkloadAgent(t, "ch-agent-del", "chc-del", nil)
	mkChannelSecret(t, "ch-del-secret")
	mkChannel(t, "ch-del", "ch-agent-del", "/channels/default/ch-del", nil)
	expectChannelReady(t, "ch-del", metav1.ConditionTrue, "")

	// A live async record that only the finalizer sweep may remove.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agentry-async-del-1", Namespace: testSystemNamespace,
			Labels: map[string]string{
				agentryv1alpha1.LabelChannelNamespace: "default",
				agentryv1alpha1.LabelChannelName:      "ch-del",
			},
			Annotations: map[string]string{
				agentryv1alpha1.AnnotationExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}
	if err := testClient.Create(ctxT(), cm); err != nil {
		t.Fatalf("create async cm: %v", err)
	}

	var ch agentryv1alpha1.AgentChannel
	if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-del"}, &ch); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctxT(), &ch); err != nil {
		t.Fatalf("delete channel: %v", err)
	}

	// Step 1: the reconciler announces Terminating and holds.
	eventually(t, func() error {
		var got agentryv1alpha1.AgentChannel
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-del"}, &got); err != nil {
			return err
		}
		if got.Status.Phase != agentryv1alpha1.ChannelTerminating {
			return errString("phase=" + string(got.Status.Phase) + " want Terminating")
		}
		return nil
	})

	// Steps 2-3: play the gateway and confirm disconnection.
	eventually(t, func() error {
		var got agentryv1alpha1.AgentChannel
		if err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-del"}, &got); err != nil {
			return err
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations[agentryv1alpha1.AnnotationChannelDisconnected] = agentryv1alpha1.AnnotationTrue
		return testClient.Update(ctxT(), &got)
	})

	// Steps 5-6: sweep and release.
	eventually(t, func() error {
		var got agentryv1alpha1.AgentChannel
		err := testClient.Get(ctxT(), types.NamespacedName{Namespace: "default", Name: "ch-del"}, &got)
		if !apierrors.IsNotFound(err) {
			return errString("channel not yet finalized")
		}
		return nil
	})
	var sweptCM corev1.ConfigMap
	err := testClient.Get(ctxT(),
		types.NamespacedName{Namespace: testSystemNamespace, Name: "agentry-async-del-1"}, &sweptCM)
	if !apierrors.IsNotFound(err) {
		t.Error("finalizer sweep must remove the channel's async records")
	}
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

// ---- AgentChannel reconcileDelete: no finalizer is a no-op ----

func TestChannel_DeleteBeforeFinalizerIsNoop(t *testing.T) {
	// A channel with the deletion timestamp already set but no finalizer must
	// short-circuit reconcileDelete without touching status.
	r := &AgentChannelReconciler{Client: testClient, OperatorNamespace: testSystemNamespace}
	now := metav1.Now()
	ch := &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ephemeral", Namespace: "default", DeletionTimestamp: &now},
	}
	res, err := r.reconcileDelete(context.Background(), ch)
	if err != nil || res.RequeueAfter != 0 || res.Requeue {
		t.Errorf("no-finalizer delete should be a clean no-op: res=%+v err=%v", res, err)
	}
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

// ---- validateCallbackURL (rule 22, reconcile-time half) ----

func TestValidateCallbackURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantBad bool
	}{
		{"not https", "http://example.com/hook", true},
		{"parse error", "://no-scheme", true},
		{"empty host", "https://", true},
		{"loopback literal", "https://127.0.0.1/hook", true},
		{"public literal ok", "https://8.8.8.8/hook", false},
		{"unresolvable deferred", "https://nonexistent.invalid/hook", false},
	}
	for _, c := range cases {
		reason, _ := validateCallbackURL(c.url)
		bad := reason != ""
		if bad != c.wantBad {
			t.Errorf("%s: validateCallbackURL(%q) bad=%v, want %v (reason=%q)", c.name, c.url, bad, c.wantBad, reason)
		}
		if bad && reason != agentryv1alpha1.ReasonInvalidCallbackURL {
			t.Errorf("%s: reason=%q, want InvalidCallbackURL", c.name, reason)
		}
	}
}

// ---- channel health reduction ----

// fakeChannelHealth serves canned per-replica channel health.
type fakeChannelHealth struct {
	reachable []ReplicaChannelHealth
	total     int
	err       error
}

func (f fakeChannelHealth) NamespaceChannelHealth(
	context.Context, string,
) ([]ReplicaChannelHealth, int, error) {
	return f.reachable, f.total, f.err
}

// chTestPath is the webhook path shared across the channel-health unit tests.
const chTestPath = "/channels/default/x"

func newChannelAt() *agentryv1alpha1.AgentChannel {
	return &agentryv1alpha1.AgentChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default"},
		Spec: agentryv1alpha1.AgentChannelSpec{
			Webhook: agentryv1alpha1.AgentChannelWebhook{Path: chTestPath},
		},
	}
}

func platformCond(ch *agentryv1alpha1.AgentChannel) *metav1.Condition {
	return condition(ch.Status.Conditions, agentryv1alpha1.ConditionPlatformConnected)
}

func TestReduceChannelHealth_NoDataPreservesCondition(t *testing.T) {
	// total==0 is rule 4: no condition written.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{total: 0}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatalf("rule 4: no PlatformConnected condition expected, got %+v", platformCond(ch))
	}

	// err also preserves.
	r = &AgentChannelReconciler{Health: fakeChannelHealth{
		reachable: []ReplicaChannelHealth{{}}, total: 1, err: errString("boom"),
	}}
	ch = newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatal("error result must preserve the existing condition")
	}
}

func TestReduceChannelHealth_Success(t *testing.T) {
	path := chTestPath
	// Two success replicas with different timestamps: the newer wins (newerHealth).
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 2,
		reachable: []ReplicaChannelHealth{
			{Channels: map[string]ChannelHealthState{path: {State: healthStateSuccess, Timestamp: strptr("2026-01-01T00:00:00Z")}}},
			{Channels: map[string]ChannelHealthState{path: {State: healthStateSuccess, Timestamp: strptr("2026-02-01T00:00:00Z")}}},
		},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != agentryv1alpha1.ReasonWebhookReady {
		t.Fatalf("rule 1 success expected True/WebhookReady, got %+v", c)
	}
}

func TestReduceChannelHealth_Failure(t *testing.T) {
	path := chTestPath
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			Channels: map[string]ChannelHealthState{path: {
				State:     healthStateFailure,
				Reason:    strptr("Timeout"),
				LastError: strptr("dial tcp: i/o timeout"),
				Timestamp: strptr("2026-01-01T00:00:00Z"),
			}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Timeout" ||
		c.Message != "dial tcp: i/o timeout" {
		t.Fatalf("rule 2 failure expected False/Timeout, got %+v", c)
	}
}

func TestReduceChannelHealth_NoRecentTraffic(t *testing.T) {
	path := chTestPath
	// A replica up longer than its window, with only an empty state: rule 3.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			StartedAt:     time.Now().Add(-time.Hour),
			WindowSeconds: 60,
			Channels:      map[string]ChannelHealthState{path: {State: healthStateEmpty}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	c := platformCond(ch)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != agentryv1alpha1.ReasonNoRecentTraffic {
		t.Fatalf("rule 3 expected Unknown/NoRecentTraffic, got %+v", c)
	}
}

func TestReduceChannelHealth_DefaultReturnsNoCondition(t *testing.T) {
	path := chTestPath
	// Window not yet full and all empty: rule 4 default -> no condition.
	r := &AgentChannelReconciler{Health: fakeChannelHealth{
		total: 1,
		reachable: []ReplicaChannelHealth{{
			StartedAt:     time.Now(),
			WindowSeconds: 3600,
			Channels:      map[string]ChannelHealthState{path: {State: healthStateEmpty}},
		}},
	}}
	ch := newChannelAt()
	r.reduceChannelHealth(context.Background(), ch)
	if platformCond(ch) != nil {
		t.Fatal("rule 4 default must leave the condition unset")
	}
}

func TestNewerHealth(t *testing.T) {
	cases := []struct {
		name string
		a, b ChannelHealthState
		want bool
	}{
		{"both nil", ChannelHealthState{}, ChannelHealthState{}, true},
		{"a nil b set", ChannelHealthState{}, ChannelHealthState{Timestamp: strptr("x")}, false},
		{"a set b nil", ChannelHealthState{Timestamp: strptr("x")}, ChannelHealthState{}, true},
		{"a newer", ChannelHealthState{Timestamp: strptr("2026-02")}, ChannelHealthState{Timestamp: strptr("2026-01")}, true},
		{"a older", ChannelHealthState{Timestamp: strptr("2026-01")}, ChannelHealthState{Timestamp: strptr("2026-02")}, false},
	}
	for _, c := range cases {
		if got := newerHealth(c.a, c.b); got != c.want {
			t.Errorf("%s: newerHealth = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAuthSecretNames_DedupAndHMAC(t *testing.T) {
	cb := "https://example.com/hook"
	ch := &agentryv1alpha1.AgentChannel{
		Spec: agentryv1alpha1.AgentChannelSpec{
			Webhook: agentryv1alpha1.AgentChannelWebhook{
				Auth: agentryv1alpha1.ChannelAuth{
					SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "inbound", Key: "t"},
					HMAC:      &agentryv1alpha1.ChannelHMAC{SecretRef: agentryv1alpha1.SecretKeyReference{Name: "hmac-sec", Key: "s"}},
				},
				CallbackURL: &cb,
				CallbackAuth: &agentryv1alpha1.ChannelAuth{
					SecretRef: &agentryv1alpha1.SecretKeyReference{Name: "inbound", Key: "t"}, // duplicate name
				},
			},
		},
	}
	names := authSecretNames(ch)
	// Sorted, deduped: hmac-sec, inbound.
	if len(names) != 2 || names[0] != "hmac-sec" || names[1] != "inbound" {
		t.Errorf("authSecretNames = %v, want [hmac-sec inbound]", names)
	}
}
