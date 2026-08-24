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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/mcp"
)

func mkToolProvider(t *testing.T, name string, mutate func(*kaalmv1beta1.ToolProvider)) {
	t.Helper()
	tp := &kaalmv1beta1.ToolProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kaalmv1beta1.ToolProviderSpec{
			Type:           "mcp",
			Endpoint:       "https://mcp.example.com",
			CredentialsRef: &kaalmv1beta1.SecretKeyReference{Name: name + "-key", Key: "token"},
		},
	}
	if mutate != nil {
		mutate(tp)
	}
	if err := testClient.Create(ctxT(), tp); err != nil {
		t.Fatalf("create toolprovider %s: %v", name, err)
	}
}

func toolProviderConditions(name string) func() []metav1.Condition {
	return func() []metav1.Condition {
		var tp kaalmv1beta1.ToolProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: name}, &tp)
		return tp.Status.Conditions
	}
}

func TestToolProvider_ValidBecomesReadyAndHealthy(t *testing.T) {
	mkSecret(t, "tp-ok-key")
	mkToolProvider(t, "tp-ok", nil)
	get := toolProviderConditions("tp-ok")
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
	eventually(t, func() error {
		c := condition(get(), kaalmv1beta1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionTrue {
			return errString("not yet Healthy")
		}
		return nil
	})
	// The resolved credential value must have reached the probe.
	if got := fakeToolHealth.credential("tp-ok"); got != "sk-test" {
		t.Fatalf("probe saw credential %q, want the resolved Secret value", got)
	}
	// The negotiated MCP revision the probe reported is recorded on status
	// (the fake answers as a 2026-07-28 server by default).
	eventually(t, func() error {
		var tp kaalmv1beta1.ToolProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "tp-ok"}, &tp); err != nil {
			return err
		}
		if tp.Status.MCPRevision != mcp.ModernRevision {
			return errString("status.mcpRevision = " + tp.Status.MCPRevision)
		}
		return nil
	})
}

func TestToolProvider_NoCredentialsRefIsReady(t *testing.T) {
	mkToolProvider(t, "tp-nocred", func(tp *kaalmv1beta1.ToolProvider) {
		tp.Spec.CredentialsRef = nil
	})
	get := toolProviderConditions("tp-nocred")
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
	c := condition(get(), kaalmv1beta1.ConditionReady)
	if !strings.Contains(c.Message, "no credential configured") {
		t.Fatalf("Ready message = %q, want it to note the absent credential", c.Message)
	}
	if got := fakeToolHealth.credential("tp-nocred"); got != "" {
		t.Fatalf("probe saw credential %q, want empty for a credential-less provider", got)
	}
}

func TestToolProvider_MissingSecretRecoversWhenCreated(t *testing.T) {
	mkToolProvider(t, "tp-late", nil)
	get := toolProviderConditions("tp-late")
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)

	// Creating the Secret afterward must recover the provider event-driven,
	// through the credential-Secret watch: no spec touch re-enqueues it here.
	mkSecret(t, "tp-late-key")
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
}

func TestToolProvider_TenantNamespaceSecretDoesNotResolve(t *testing.T) {
	// The credential invariant: a same-named Secret outside the operator
	// namespace must never satisfy the ref.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tp-tenant"}}
	if err := testClient.Create(ctxT(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tp-sneaky-key", Namespace: "tp-tenant"},
		Data:       map[string][]byte{"token": []byte("sk-tenant")},
	}
	if err := testClient.Create(ctxT(), sec); err != nil {
		t.Fatalf("create tenant secret: %v", err)
	}
	mkToolProvider(t, "tp-sneaky", nil)
	expectReady(t, toolProviderConditions("tp-sneaky"),
		metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)
}

func TestToolProvider_EmptySecretKeyIsNotReady(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tp-empty-key", Namespace: testOperatorNamespace},
		Data:       map[string][]byte{"other": []byte("x")},
	}
	if err := testClient.Create(ctxT(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	mkToolProvider(t, "tp-empty", nil)
	expectReady(t, toolProviderConditions("tp-empty"),
		metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsMissing)
}

func TestToolProvider_AuthFailedIsNotReady(t *testing.T) {
	mkSecret(t, "tp-auth-key")
	fakeToolHealth.set("tp-auth", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{AuthFailed: true}})
	// A 1s probe interval so the recovery half of the test happens within the
	// eventually window (the auth-failed path re-probes on the interval).
	mkToolProvider(t, "tp-auth", func(tp *kaalmv1beta1.ToolProvider) {
		tp.Spec.HealthCheck = &kaalmv1beta1.ToolProviderHealthCheck{Enabled: true, IntervalSeconds: 1}
	})
	get := toolProviderConditions("tp-auth")
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonCredentialsInvalid)
	eventually(t, func() error {
		c := condition(get(), kaalmv1beta1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonCredentialsInvalid {
			return errString("Healthy not yet False/CredentialsInvalid")
		}
		return nil
	})

	// The server accepting the credential again recovers both conditions on
	// the next probe interval.
	fakeToolHealth.set("tp-auth", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Healthy: true}})
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
}

func TestToolProvider_ProbeErrorIsUnhealthyButReady(t *testing.T) {
	mkSecret(t, "tp-down-key")
	fakeToolHealth.set("tp-down", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Err: errString("connect: refused")}})
	mkToolProvider(t, "tp-down", nil)
	get := toolProviderConditions("tp-down")
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
	eventually(t, func() error {
		c := condition(get(), kaalmv1beta1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1beta1.ReasonProviderUnhealthy {
			return errString("Healthy not yet False/ProviderUnhealthy")
		}
		return nil
	})
}

func TestToolProvider_HealthCheckDisabledSkipsProbe(t *testing.T) {
	mkSecret(t, "tp-nohc-key")
	// A probe result that WOULD block Ready if the probe ran, so a passing
	// test proves the probe was genuinely skipped rather than merely healthy.
	fakeToolHealth.set("tp-nohc", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{AuthFailed: true}})
	mkToolProvider(t, "tp-nohc", func(tp *kaalmv1beta1.ToolProvider) {
		tp.Spec.HealthCheck = &kaalmv1beta1.ToolProviderHealthCheck{Enabled: false}
	})
	get := toolProviderConditions("tp-nohc")
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
	if n := fakeToolHealth.count("tp-nohc"); n != 0 {
		t.Fatalf("healthCheck.enabled=false: expected probe to be skipped, called %d times", n)
	}
	if c := condition(get(), kaalmv1beta1.ConditionHealthy); c != nil {
		t.Fatalf("Healthy condition present without a probe: %+v", c)
	}
}

func TestToolProvider_NilHealthCheckRunsProbe(t *testing.T) {
	mkSecret(t, "tp-nilhc-key")
	// Leave HealthCheck nil: reconcile-time defaulting must still run the probe.
	mkToolProvider(t, "tp-nilhc", nil)
	expectReady(t, toolProviderConditions("tp-nilhc"),
		metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
	eventually(t, func() error {
		if fakeToolHealth.count("tp-nilhc") == 0 {
			return errString("nil healthCheck: expected probe to run, but it was never called")
		}
		return nil
	})
}

func TestToolProvider_DeleteIsUnblocked(t *testing.T) {
	// Unreferenced by any Agent, AgentTask, or AgentClass: the finalizer
	// releases immediately and deletion completes.
	mkSecret(t, "tp-del-key")
	mkToolProvider(t, "tp-del", nil)
	expectReady(t, toolProviderConditions("tp-del"),
		metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)

	var tp kaalmv1beta1.ToolProvider
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "tp-del"}, &tp); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := testClient.Delete(ctxT(), &tp); err != nil {
		t.Fatalf("delete: %v", err)
	}
	eventually(t, func() error {
		var got kaalmv1beta1.ToolProvider
		if apierrors.IsNotFound(testClient.Get(ctxT(), types.NamespacedName{Name: "tp-del"}, &got)) {
			return nil
		}
		return errString("toolprovider still present")
	})
}

// awaitToolProviderFinalizer waits until the reconciler has installed the
// finalizer, so a subsequent Delete exercises the hold rather than racing it.
func awaitToolProviderFinalizer(t *testing.T, name string) {
	t.Helper()
	eventually(t, func() error {
		var tp kaalmv1beta1.ToolProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: name}, &tp); err != nil {
			return err
		}
		for _, f := range tp.Finalizers {
			if f == kaalmv1beta1.ToolProviderFinalizer {
				return nil
			}
		}
		return errString("finalizer not yet installed")
	})
}

func TestToolProvider_HeldWhileAgentReferences(t *testing.T) {
	mkOpenTP(t, "tp-held", nil)
	// The class deliberately does NOT allowlist the provider: the agent sits
	// Degraded on rule 37, and its grant still holds the deletion, proving
	// the hold is by reference, not by validity. A class allowlist entry
	// would be its own independent hold (covered by the class test below).
	mkWorkloadClass(t, "wc-held", nil)
	mkWorkloadAgent(t, "held-agent", "wc-held", func(ag *kaalmv1beta1.Agent) {
		ag.Spec.Tools = []kaalmv1beta1.AgentToolGrant{grantOf("tp-held")}
	})
	awaitToolProviderFinalizer(t, "tp-held")

	var tp kaalmv1beta1.ToolProvider
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "tp-held"}, &tp); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := testClient.Delete(ctxT(), &tp); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Held in Terminating while the agent references it: the object persists
	// with a deletion timestamp. Polled, because testClient reads the manager
	// cache, which lags the delete by a beat.
	eventually(t, func() error {
		var got kaalmv1beta1.ToolProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "tp-held"}, &got); err != nil {
			return errString("toolprovider was removed while still referenced: " + err.Error())
		}
		if got.DeletionTimestamp.IsZero() {
			return errString("no deletion timestamp yet")
		}
		return nil
	})

	// Removing the referrer releases the hold.
	var ag kaalmv1beta1.Agent
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "held-agent", Namespace: "default"}, &ag); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if err := testClient.Delete(ctxT(), &ag); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	eventually(t, func() error {
		var got kaalmv1beta1.ToolProvider
		if apierrors.IsNotFound(testClient.Get(ctxT(), types.NamespacedName{Name: "tp-held"}, &got)) {
			return nil
		}
		return errString("toolprovider still held after the referrer went away")
	})
}

func TestToolProvider_HeldWhileClassReferences(t *testing.T) {
	mkOpenTP(t, "tp-clsheld", nil)
	mkWorkloadClass(t, "wc-clsheld", func(ac *kaalmv1beta1.AgentClass) {
		ac.Spec.AllowedToolProviders = []kaalmv1beta1.LocalObjectReference{{Name: "tp-clsheld"}}
	})
	awaitToolProviderFinalizer(t, "tp-clsheld")

	var tp kaalmv1beta1.ToolProvider
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "tp-clsheld"}, &tp); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := testClient.Delete(ctxT(), &tp); err != nil {
		t.Fatalf("delete: %v", err)
	}
	eventually(t, func() error {
		var got kaalmv1beta1.ToolProvider
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "tp-clsheld"}, &got); err != nil {
			return errString("toolprovider was removed while a class still allowlists it: " + err.Error())
		}
		if got.DeletionTimestamp.IsZero() {
			return errString("no deletion timestamp yet")
		}
		return nil
	})

	// Dropping the class's allowlist entry releases the hold (the update
	// event maps through the OLD object's references too).
	eventually(t, func() error {
		var ac kaalmv1beta1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "wc-clsheld"}, &ac); err != nil {
			return err
		}
		ac.Spec.AllowedToolProviders = nil
		return testClient.Update(ctxT(), &ac)
	})
	eventually(t, func() error {
		var got kaalmv1beta1.ToolProvider
		if apierrors.IsNotFound(testClient.Get(ctxT(), types.NamespacedName{Name: "tp-clsheld"}, &got)) {
			return nil
		}
		return errString("toolprovider still held after the class dropped it")
	})
}
