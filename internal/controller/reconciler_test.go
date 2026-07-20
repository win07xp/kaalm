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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

const timeout = 10 * time.Second

func ctxT() context.Context { return context.Background() }

func mkProvider(t *testing.T, name string, mutate func(*agentryv1alpha1.ModelProvider)) {
	t.Helper()
	mp := &agentryv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: agentryv1alpha1.ModelProviderSpec{
			Type:           "openai",
			Endpoint:       "https://api.example.com",
			CredentialsRef: agentryv1alpha1.SecretKeyReference{Name: name + "-key", Key: "token"},
		},
	}
	if mutate != nil {
		mutate(mp)
	}
	if err := testClient.Create(ctxT(), mp); err != nil {
		t.Fatalf("create provider %s: %v", name, err)
	}
}

func mkClass(t *testing.T, name string, allowed ...string) {
	t.Helper()
	ac := &agentryv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, p := range allowed {
		ac.Spec.AllowedProviders = append(ac.Spec.AllowedProviders, agentryv1alpha1.LocalObjectReference{Name: p})
	}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class %s: %v", name, err)
	}
}

func mkAgent(t *testing.T, name, className string, providers ...string) {
	t.Helper()
	ag := &agentryv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       agentryv1alpha1.AgentSpec{AgentClassRef: agentryv1alpha1.LocalObjectReference{Name: className}},
	}
	for _, p := range providers {
		ag.Spec.Providers = append(ag.Spec.Providers,
			agentryv1alpha1.AgentProviderReference{ProviderRef: agentryv1alpha1.LocalObjectReference{Name: p}})
	}
	if err := testClient.Create(ctxT(), ag); err != nil {
		t.Fatalf("create agent %s: %v", name, err)
	}
}

func mkSecret(t *testing.T, name string) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testOperatorNamespace},
		Data:       map[string][]byte{"token": []byte("sk-test")},
	}
	if err := testClient.Create(ctxT(), s); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create secret %s: %v", name, err)
	}
}

func expectReady(t *testing.T, get func() []metav1.Condition, want metav1.ConditionStatus, reason string) {
	t.Helper()
	eventually(t, func() error {
		c := condition(get(), agentryv1alpha1.ConditionReady)
		if c == nil {
			return errString("no Ready condition yet")
		}
		if c.Status != want {
			return errString("Ready=" + string(c.Status) + " want " + string(want))
		}
		if reason != "" && c.Reason != reason {
			return errString("reason=" + c.Reason + " want " + reason)
		}
		return nil
	})
}

type errString string

func (e errString) Error() string { return string(e) }

// ---- AgentClass ----

func TestAgentClass_ValidBecomesReady(t *testing.T) {
	mkProvider(t, "ac-valid-prov", nil)
	mkClass(t, "ac-valid", "ac-valid-prov")
	expectReady(t, func() []metav1.Condition {
		var ac agentryv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-valid"}, &ac)
		return ac.Status.Conditions
	}, metav1.ConditionTrue, agentryv1alpha1.ReasonAllReferencesResolved)
}

func TestAgentClass_MissingProviderIsNotReady(t *testing.T) {
	mkClass(t, "ac-missing", "does-not-exist")
	expectReady(t, func() []metav1.Condition {
		var ac agentryv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-missing"}, &ac)
		return ac.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonInvalidReference)
}

func TestAgentClass_InvalidCIDRIsNotReady(t *testing.T) {
	ac := &agentryv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-badcidr"}}
	ac.Spec.Network.Egress.AllowedCIDRs = []string{"not-a-cidr"}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	expectReady(t, func() []metav1.Condition {
		var got agentryv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-badcidr"}, &got)
		return got.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonInvalidReference)
}

func TestAgentClass_CountsUsers(t *testing.T) {
	mkClass(t, "ac-count")
	mkAgent(t, "count-a", "ac-count")
	mkAgent(t, "count-b", "ac-count")
	eventually(t, func() error {
		var ac agentryv1alpha1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "ac-count"}, &ac); err != nil {
			return err
		}
		if ac.Status.AgentsInUse != 2 {
			return errString("agentsInUse not yet 2")
		}
		return nil
	})
}

func TestAgentClass_FinalizerHoldsWhileReferenced(t *testing.T) {
	mkClass(t, "ac-hold")
	mkAgent(t, "hold-agent", "ac-hold")

	// Wait for the finalizer to be added.
	eventually(t, func() error {
		var ac agentryv1alpha1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hold"}, &ac); err != nil {
			return err
		}
		if !controllerutil.ContainsFinalizer(&ac, agentryv1alpha1.ClassFinalizer) {
			return errString("no finalizer yet")
		}
		return nil
	})

	// Delete the class: it should be held (deletionTimestamp set, object remains).
	var ac agentryv1alpha1.AgentClass
	_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hold"}, &ac)
	if err := testClient.Delete(ctxT(), &ac); err != nil {
		t.Fatalf("delete class: %v", err)
	}
	// Give the reconciler a moment; the class must still exist while referenced.
	time.Sleep(500 * time.Millisecond)
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hold"}, &ac); err != nil {
		t.Fatalf("class was removed while still referenced: %v", err)
	}

	// Remove the referrer; the class should now finalize away.
	var agent agentryv1alpha1.Agent
	_ = testClient.Get(ctxT(), types.NamespacedName{Name: "hold-agent", Namespace: "default"}, &agent)
	if err := testClient.Delete(ctxT(), &agent); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	eventually(t, func() error {
		var got agentryv1alpha1.AgentClass
		err := testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hold"}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errString("class not yet finalized")
	})
}

// ---- ModelProvider ----

func TestModelProvider_CredentialsMissingIsNotReady(t *testing.T) {
	mkProvider(t, "mp-nocred", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = agentryv1alpha1.SecretKeyReference{Name: "absent-secret", Key: "token"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-nocred"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonCredentialsMissing)
}

func TestModelProvider_ValidBecomesReadyAndHealthy(t *testing.T) {
	mkSecret(t, "mp-ok-key")
	mkProvider(t, "mp-ok", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.HealthCheck = &agentryv1alpha1.ModelProviderHealthCheck{Enabled: true}
	})
	get := func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-ok"}, &mp)
		return mp.Status.Conditions
	}
	expectReady(t, get, metav1.ConditionTrue, agentryv1alpha1.ReasonCredentialsValid)
	eventually(t, func() error {
		c := condition(get(), agentryv1alpha1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionTrue {
			return errString("not yet Healthy")
		}
		return nil
	})
}

func TestModelProvider_AuthFailedIsNotReady(t *testing.T) {
	mkSecret(t, "mp-auth-key")
	fakeHealth.set("mp-auth", ProviderProbeResult{AuthFailed: true})
	mkProvider(t, "mp-auth", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.HealthCheck = &agentryv1alpha1.ModelProviderHealthCheck{Enabled: true}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-auth"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonCredentialsInvalid)
}

func TestModelProvider_HealthCheckDisabledSkipsProbe(t *testing.T) {
	mkSecret(t, "mp-nohc-key")
	// A probe result that WOULD block Ready if the probe ran, so a passing
	// test proves the probe was genuinely skipped rather than merely healthy.
	fakeHealth.set("mp-nohc", ProviderProbeResult{AuthFailed: true})
	mkProvider(t, "mp-nohc", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.HealthCheck = &agentryv1alpha1.ModelProviderHealthCheck{Enabled: false}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-nohc"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, agentryv1alpha1.ReasonCredentialsValid)
	if n := fakeHealth.count("mp-nohc"); n != 0 {
		t.Fatalf("healthCheck.enabled=false: expected probe to be skipped, called %d times", n)
	}
}

func TestModelProvider_NilHealthCheckRunsProbe(t *testing.T) {
	mkSecret(t, "mp-nilhc-key")
	// Leave HealthCheck nil: reconcile-time defaulting must still run the probe.
	mkProvider(t, "mp-nilhc", nil)
	get := func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-nilhc"}, &mp)
		return mp.Status.Conditions
	}
	expectReady(t, get, metav1.ConditionTrue, agentryv1alpha1.ReasonCredentialsValid)
	eventually(t, func() error {
		if fakeHealth.count("mp-nilhc") == 0 {
			return errString("nil healthCheck: expected probe to run, but it was never called")
		}
		return nil
	})
}

func TestModelProvider_FallbackCycleIsNotReady(t *testing.T) {
	mkSecret(t, "mp-cyc-a-key")
	mkSecret(t, "mp-cyc-b-key")
	mkProvider(t, "mp-cyc-b", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.Fallback = []agentryv1alpha1.LocalObjectReference{{Name: "mp-cyc-a"}}
	})
	mkProvider(t, "mp-cyc-a", func(mp *agentryv1alpha1.ModelProvider) {
		mp.Spec.Fallback = []agentryv1alpha1.LocalObjectReference{{Name: "mp-cyc-b"}}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-cyc-a"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonFallbackIneligible)
}

func TestModelProvider_InvalidDegradeTargetIsNotReady(t *testing.T) {
	mkSecret(t, "mp-deg-key")
	mkProvider(t, "mp-deg", func(mp *agentryv1alpha1.ModelProvider) {
		to := "no-such-model"
		mp.Spec.Models = []agentryv1alpha1.ModelProviderModel{{ID: "real-model"}}
		mp.Spec.Budget.Policies = []agentryv1alpha1.ModelProviderBudgetPolicy{
			{AtPercent: 100, Action: "degrade", DegradeTo: &to},
		}
	})
	expectReady(t, func() []metav1.Condition {
		var mp agentryv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-deg"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, agentryv1alpha1.ReasonInvalidDegradeTarget)
}

var _ = client.IgnoreNotFound
