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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

const timeout = 10 * time.Second

func ctxT() context.Context { return context.Background() }

func mkProvider(t *testing.T, name string, mutate func(*kaalmv1alpha1.ModelProvider)) {
	t.Helper()
	mp := &kaalmv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: kaalmv1alpha1.ModelProviderSpec{
			Type:           "openai",
			Endpoint:       "https://api.example.com",
			CredentialsRef: kaalmv1alpha1.SecretKeyReference{Name: name + "-key", Key: "token"},
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
	ac := &kaalmv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, p := range allowed {
		ac.Spec.AllowedProviders = append(ac.Spec.AllowedProviders, kaalmv1alpha1.LocalObjectReference{Name: p})
	}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class %s: %v", name, err)
	}
}

func mkAgent(t *testing.T, name, className string, providers ...string) {
	t.Helper()
	ag := &kaalmv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       kaalmv1alpha1.AgentSpec{AgentClassRef: kaalmv1alpha1.LocalObjectReference{Name: className}},
	}
	for _, p := range providers {
		ag.Spec.Providers = append(ag.Spec.Providers,
			kaalmv1alpha1.AgentProviderReference{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: p}})
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
		c := condition(get(), kaalmv1alpha1.ConditionReady)
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
		var ac kaalmv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-valid"}, &ac)
		return ac.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonAllReferencesResolved)
}

func TestAgentClass_MissingProviderIsNotReady(t *testing.T) {
	mkClass(t, "ac-missing", "does-not-exist")
	expectReady(t, func() []metav1.Condition {
		var ac kaalmv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-missing"}, &ac)
		return ac.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonInvalidReference)
}

func TestAgentClass_InvalidCIDRIsNotReady(t *testing.T) {
	ac := &kaalmv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-badcidr"}}
	ac.Spec.Network.Egress.AllowedCIDRs = []string{"not-a-cidr"}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	expectReady(t, func() []metav1.Condition {
		var got kaalmv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-badcidr"}, &got)
		return got.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonInvalidReference)
}

func TestAgentClass_CountsUsers(t *testing.T) {
	mkClass(t, "ac-count")
	mkAgent(t, "count-a", "ac-count")
	mkAgent(t, "count-b", "ac-count")
	eventually(t, func() error {
		var ac kaalmv1alpha1.AgentClass
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
		var ac kaalmv1alpha1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hold"}, &ac); err != nil {
			return err
		}
		if !controllerutil.ContainsFinalizer(&ac, kaalmv1alpha1.ClassFinalizer) {
			return errString("no finalizer yet")
		}
		return nil
	})

	// Delete the class: it should be held (deletionTimestamp set, object remains).
	var ac kaalmv1alpha1.AgentClass
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
	var agent kaalmv1alpha1.Agent
	_ = testClient.Get(ctxT(), types.NamespacedName{Name: "hold-agent", Namespace: "default"}, &agent)
	if err := testClient.Delete(ctxT(), &agent); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	eventually(t, func() error {
		var got kaalmv1alpha1.AgentClass
		err := testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hold"}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errString("class not yet finalized")
	})
}

// ---- ModelProvider ----

func TestModelProvider_CredentialsMissingIsNotReady(t *testing.T) {
	mkProvider(t, "mp-nocred", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "absent-secret", Key: "token"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-nocred"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonCredentialsMissing)
}

func TestModelProvider_ValidBecomesReadyAndHealthy(t *testing.T) {
	mkSecret(t, "mp-ok-key")
	mkProvider(t, "mp-ok", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.HealthCheck = &kaalmv1alpha1.ModelProviderHealthCheck{Enabled: true}
	})
	get := func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-ok"}, &mp)
		return mp.Status.Conditions
	}
	expectReady(t, get, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)
	eventually(t, func() error {
		c := condition(get(), kaalmv1alpha1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionTrue {
			return errString("not yet Healthy")
		}
		return nil
	})
}

func TestModelProvider_AuthFailedIsNotReady(t *testing.T) {
	mkSecret(t, "mp-auth-key")
	fakeHealth.set("mp-auth", ProviderProbeResult{AuthFailed: true})
	mkProvider(t, "mp-auth", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.HealthCheck = &kaalmv1alpha1.ModelProviderHealthCheck{Enabled: true}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-auth"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonCredentialsInvalid)
}

func TestModelProvider_HealthCheckDisabledSkipsProbe(t *testing.T) {
	mkSecret(t, "mp-nohc-key")
	// A probe result that WOULD block Ready if the probe ran, so a passing
	// test proves the probe was genuinely skipped rather than merely healthy.
	fakeHealth.set("mp-nohc", ProviderProbeResult{AuthFailed: true})
	mkProvider(t, "mp-nohc", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.HealthCheck = &kaalmv1alpha1.ModelProviderHealthCheck{Enabled: false}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-nohc"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)
	if n := fakeHealth.count("mp-nohc"); n != 0 {
		t.Fatalf("healthCheck.enabled=false: expected probe to be skipped, called %d times", n)
	}
}

func TestModelProvider_NilHealthCheckRunsProbe(t *testing.T) {
	mkSecret(t, "mp-nilhc-key")
	// Leave HealthCheck nil: reconcile-time defaulting must still run the probe.
	mkProvider(t, "mp-nilhc", nil)
	get := func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-nilhc"}, &mp)
		return mp.Status.Conditions
	}
	expectReady(t, get, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)
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
	mkProvider(t, "mp-cyc-b", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.Fallback = []kaalmv1alpha1.LocalObjectReference{{Name: "mp-cyc-a"}}
	})
	mkProvider(t, "mp-cyc-a", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.Fallback = []kaalmv1alpha1.LocalObjectReference{{Name: "mp-cyc-b"}}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-cyc-a"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonFallbackIneligible)
}

func TestModelProvider_InvalidDegradeTargetIsNotReady(t *testing.T) {
	mkSecret(t, "mp-deg-key")
	mkProvider(t, "mp-deg", func(mp *kaalmv1alpha1.ModelProvider) {
		to := "no-such-model"
		mp.Spec.Models = []kaalmv1alpha1.ModelProviderModel{{ID: "real-model"}}
		mp.Spec.Budget.Policies = []kaalmv1alpha1.ModelProviderBudgetPolicy{
			{AtPercent: 100, Action: "degrade", DegradeTo: &to},
		}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-deg"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonInvalidDegradeTarget)
}

var _ = client.IgnoreNotFound

// testStorageClass is a reusable PVC storage-class name for the builder tests.
const testStorageClass = "fast"

func strptr(s string) *string { return &s }

// ---- isReferenced: Agent and AgentTask branches ----

func TestModelProviderIsReferenced_ByAgentAndTask(t *testing.T) {
	r := &ModelProviderReconciler{Client: testClient}

	// A provider referenced only by an Agent (no class lists it, so the agents
	// branch is the one that reports referenced).
	mkClass(t, "iref-cls")
	mkAgent(t, "iref-agent", "iref-cls", "iref-only-agent-prov")
	eventually(t, func() error {
		ok, err := r.isReferenced(context.Background(), "iref-only-agent-prov")
		if err != nil {
			return err
		}
		if !ok {
			return errString("provider referenced by an Agent should report referenced")
		}
		return nil
	})

	// A provider referenced only by an AgentTask.
	mkWorkloadClass(t, "iref-tcls", nil)
	mkTask(t, "iref-task", "iref-tcls", func(task *kaalmv1alpha1.AgentTask) {
		task.Spec.Providers = []kaalmv1alpha1.AgentProviderReference{
			{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: "iref-task-prov"}},
		}
	})
	eventually(t, func() error {
		ok, err := r.isReferenced(context.Background(), "iref-task-prov")
		if err != nil {
			return err
		}
		if !ok {
			return errString("provider referenced by an AgentTask should report referenced")
		}
		return nil
	})

	// An unreferenced provider.
	if ok, err := r.isReferenced(context.Background(), "nobody-references-me"); err != nil || ok {
		t.Errorf("unreferenced provider: ok=%v err=%v", ok, err)
	}
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
	mkProvider(t, "mp-wrongkey", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-wrongkey-secret", Key: "token"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-wrongkey"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonCredentialsMissing)
}

// ---- ModelProvider delete handshake (reconcileDelete + isReferenced) ----

func TestModelProvider_DeleteHeldWhileReferenced(t *testing.T) {
	mkSecret(t, "mpref-key")
	mkProvider(t, "mpref", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mpref-key", Key: "token"}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mpref"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)

	// An AgentClass listing the provider makes isReferenced true (classes branch).
	mkClass(t, "acref", "mpref")

	// Delete the provider: the finalizer holds it in Terminating while referenced.
	var mp kaalmv1alpha1.ModelProvider
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mpref"}, &mp); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctxT(), &mp); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: "mpref"}, &mp); err != nil {
		t.Fatalf("provider removed while still referenced: %v", err)
	}

	// Remove the referrer; the provider finalizes away.
	var ac kaalmv1alpha1.AgentClass
	_ = testClient.Get(ctxT(), types.NamespacedName{Name: "acref"}, &ac)
	if err := testClient.Delete(ctxT(), &ac); err != nil {
		t.Fatalf("delete class: %v", err)
	}
	eventually(t, func() error {
		var got kaalmv1alpha1.ModelProvider
		err := testClient.Get(ctxT(), types.NamespacedName{Name: "mpref"}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errString("provider not yet finalized")
	})
}

// ---- ModelProvider probe outcomes (Skipped, Err, interval) ----

func TestModelProvider_ProbeSkipped(t *testing.T) {
	mkSecret(t, "mp-skip-key")
	fakeHealth.set("mp-skip", ProviderProbeResult{Skipped: true})
	mkProvider(t, "mp-skip", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-skip-key", Key: "token"}
		mp.Spec.HealthCheck = &kaalmv1alpha1.ModelProviderHealthCheck{Enabled: true}
	})
	get := func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-skip"}, &mp)
		return mp.Status.Conditions
	}
	expectReady(t, get, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)
	eventually(t, func() error {
		c := condition(get(), kaalmv1alpha1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "ProbeSkipped" {
			return errString("Healthy should be Unknown/ProbeSkipped")
		}
		return nil
	})
}

func TestModelProvider_ProbeErrStaysReady(t *testing.T) {
	mkSecret(t, "mp-unhealthy-key")
	fakeHealth.set("mp-unhealthy", ProviderProbeResult{Err: errString("upstream 500")})
	mkProvider(t, "mp-unhealthy", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-unhealthy-key", Key: "token"}
		// IntervalSeconds>0 exercises the interval() configured branch.
		mp.Spec.HealthCheck = &kaalmv1alpha1.ModelProviderHealthCheck{Enabled: true, IntervalSeconds: 5}
	})
	get := func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-unhealthy"}, &mp)
		return mp.Status.Conditions
	}
	// A transient probe error does not flip Ready.
	expectReady(t, get, metav1.ConditionTrue, kaalmv1alpha1.ReasonCredentialsValid)
	eventually(t, func() error {
		c := condition(get(), kaalmv1alpha1.ConditionHealthy)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != kaalmv1alpha1.ReasonProviderUnhealthy {
			return errString("Healthy should be False/ProviderUnhealthy")
		}
		return nil
	})
}

// ---- ModelProvider fallback validation ----

func TestModelProvider_FallbackMissingIsNotReady(t *testing.T) {
	mkSecret(t, "mp-fbmiss-key")
	mkProvider(t, "mp-fbmiss", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-fbmiss-key", Key: "token"}
		mp.Spec.Fallback = []kaalmv1alpha1.LocalObjectReference{{Name: "no-such-provider"}}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-fbmiss"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonFallbackIneligible)
}

func TestModelProvider_FallbackTypeMismatchIsNotReady(t *testing.T) {
	mkSecret(t, "mp-fbtype-a-key")
	mkSecret(t, "mp-fbtype-b-key")
	mkProvider(t, "mp-fbtype-b", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.Type = "anthropic"
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-fbtype-b-key", Key: "token"}
	})
	mkProvider(t, "mp-fbtype-a", func(mp *kaalmv1alpha1.ModelProvider) {
		mp.Spec.Type = "openai"
		mp.Spec.CredentialsRef = kaalmv1alpha1.SecretKeyReference{Name: "mp-fbtype-a-key", Key: "token"}
		mp.Spec.Fallback = []kaalmv1alpha1.LocalObjectReference{{Name: "mp-fbtype-b"}}
	})
	expectReady(t, func() []metav1.Condition {
		var mp kaalmv1alpha1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "mp-fbtype-a"}, &mp)
		return mp.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonFallbackIneligible)
}

// ---- AgentClass FQDN + host validation ----

func TestAgentClass_AllowedHostsUnsupportedByCNI(t *testing.T) {
	ac := &kaalmv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-hosts"}}
	ac.Spec.Network.Egress.AllowedHosts = []string{"api.example.com"}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The envtest apiserver exposes no Cilium/Calico groups, so FQDN policy is
	// unsupported; the class stays Ready (allowedHosts is advisory) but the
	// FQDNPolicySupported condition is False.
	expectReady(t, func() []metav1.Condition {
		var got kaalmv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hosts"}, &got)
		return got.Status.Conditions
	}, metav1.ConditionTrue, kaalmv1alpha1.ReasonAllReferencesResolved)
	eventually(t, func() error {
		var got kaalmv1alpha1.AgentClass
		if err := testClient.Get(ctxT(), types.NamespacedName{Name: "ac-hosts"}, &got); err != nil {
			return err
		}
		c := condition(got.Status.Conditions, kaalmv1alpha1.ConditionFQDNPolicySupported)
		if c == nil || c.Status != metav1.ConditionFalse ||
			c.Reason != kaalmv1alpha1.ReasonFQDNPolicyUnsupported {
			return errString("FQDNPolicySupported should be False/FQDNPolicyUnsupported")
		}
		return nil
	})
}

func TestAgentClass_InvalidHostIsNotReady(t *testing.T) {
	ac := &kaalmv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-badhost"}}
	ac.Spec.Network.Egress.AllowedHosts = []string{"not a valid host"}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	expectReady(t, func() []metav1.Condition {
		var got kaalmv1alpha1.AgentClass
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: "ac-badhost"}, &got)
		return got.Status.Conditions
	}, metav1.ConditionFalse, kaalmv1alpha1.ReasonInvalidReference)
}

// ---- reconcileDelete short-circuits when the finalizer is already gone ----

func TestReconcileDelete_NoFinalizerIsNoop(t *testing.T) {
	ctx := context.Background()
	clean := func(name string, requeue bool, after time.Duration, err error) {
		if err != nil || requeue || after != 0 {
			t.Errorf("%s no-finalizer delete not a clean no-op: requeue=%v after=%v err=%v", name, requeue, after, err)
		}
	}

	mpRes, err := (&ModelProviderReconciler{Client: testClient}).reconcileDelete(ctx,
		&kaalmv1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "x"}})
	clean("ModelProvider", mpRes.Requeue, mpRes.RequeueAfter, err)

	acRes, err := (&AgentClassReconciler{Client: testClient}).reconcileDelete(ctx,
		&kaalmv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "x"}})
	clean("AgentClass", acRes.Requeue, acRes.RequeueAfter, err)

	taskRes, err := (&AgentTaskReconciler{Client: testClient}).reconcileDelete(ctx,
		&kaalmv1alpha1.AgentTask{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}})
	clean("AgentTask", taskRes.Requeue, taskRes.RequeueAfter, err)

	agRes, err := (&AgentReconciler{Client: testClient}).reconcileDelete(ctx,
		&kaalmv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}})
	clean("Agent", agRes.Requeue, agRes.RequeueAfter, err)
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kaalmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := cmapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// newErrListClient returns a client whose List always fails, so the many
// map-func and reference-count error branches can be exercised deterministically.
func newErrListClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return fmt.Errorf("injected list failure")
		},
	}).Build()
}

// newErrGetClient returns a client whose Get always fails with a non-NotFound
// error, to exercise the Get error branches.
func newErrGetClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return fmt.Errorf("injected get failure")
		},
	}).Build()
}

// newErrCreateClient returns a client whose Create always fails with a
// non-AlreadyExists error, to exercise the child-convergence error branches.
func newErrCreateClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return fmt.Errorf("injected create failure")
		},
	}).Build()
}

func TestGetErrorBranches(t *testing.T) {
	ctx := context.Background()
	c := newErrGetClient(t)

	// credential surfaces a non-NotFound Secret Get error as CredentialsMissing.
	r := &ModelProviderReconciler{Client: c, OperatorNamespace: "default"}
	mp := &kaalmv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       kaalmv1alpha1.ModelProviderSpec{CredentialsRef: kaalmv1alpha1.SecretKeyReference{Name: "s", Key: "token"}},
	}
	_, reason, msg := r.credential(ctx, mp)
	if reason != kaalmv1alpha1.ReasonCredentialsMissing || msg == "" {
		t.Errorf("credential Get error: reason=%q msg=%q", reason, msg)
	}

	// reconcileBudget surfaces a non-NotFound ConfigMap Get error.
	budgeted := &kaalmv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       kaalmv1alpha1.ModelProviderSpec{Budget: kaalmv1alpha1.ModelProviderBudget{Period: "monthly"}},
	}
	if err := r.reconcileBudget(ctx, budgeted, map[string]bool{}); err == nil {
		t.Error("reconcileBudget must surface a ConfigMap Get error")
	}
}

func TestMapFuncs_ListErrorReturnsNil(t *testing.T) {
	ctx := context.Background()
	c := newErrListClient(t)

	ar := &AgentReconciler{Client: c}
	if reqs := ar.agentsForClass(ctx, &kaalmv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}}); reqs != nil {
		t.Errorf("agentsForClass on a list error must return nil: %v", reqs)
	}
	if reqs := ar.agentsForProvider(ctx, &kaalmv1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}); reqs != nil {
		t.Errorf("agentsForProvider on a list error must return nil: %v", reqs)
	}

	acr := &AgentClassReconciler{Client: c}
	if reqs := acr.classesForProvider(ctx, &kaalmv1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}); reqs != nil {
		t.Errorf("classesForProvider on a list error must return nil: %v", reqs)
	}

	tr := &AgentTaskReconciler{Client: c}
	if reqs := tr.tasksForClass(ctx, &kaalmv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}}); reqs != nil {
		t.Errorf("tasksForClass on a list error must return nil: %v", reqs)
	}

	chr := &AgentChannelReconciler{Client: c}
	if reqs := chr.channelsForAgent(ctx, &kaalmv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}); reqs != nil {
		t.Errorf("channelsForAgent on a list error must return nil: %v", reqs)
	}
}

func TestReferenceCountsPropagateListErrors(t *testing.T) {
	ctx := context.Background()
	c := newErrListClient(t)

	// gatewayPods surfaces the list error.
	if _, _, err := (&ModelProviderReconciler{Client: c, OperatorNamespace: "default"}).gatewayPods(ctx); err == nil {
		t.Error("gatewayPods must surface a list error")
	}

	// isReferenced (via reconcileDelete on a finalized provider) surfaces it too.
	mp := &kaalmv1alpha1.ModelProvider{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	controllerutil.AddFinalizer(mp, kaalmv1alpha1.ProviderFinalizer)
	if _, err := (&ModelProviderReconciler{Client: c}).reconcileDelete(ctx, mp); err == nil {
		t.Error("provider reconcileDelete must surface the isReferenced list error")
	}

	// countUsers (via reconcileDelete on a finalized class) surfaces it.
	ac := &kaalmv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}}
	controllerutil.AddFinalizer(ac, kaalmv1alpha1.ClassFinalizer)
	if _, err := (&AgentClassReconciler{Client: c}).reconcileDelete(ctx, ac); err == nil {
		t.Error("class reconcileDelete must surface the countUsers list error")
	}
}

func TestClassForWorkload(t *testing.T) {
	ctx := context.Background()

	// An Agent maps to its class.
	ag := &kaalmv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
		Spec:       kaalmv1alpha1.AgentSpec{AgentClassRef: kaalmv1alpha1.LocalObjectReference{Name: "cls"}},
	}
	if reqs := classForWorkload(ctx, ag); len(reqs) != 1 || reqs[0].Name != "cls" {
		t.Errorf("agent should map to its class: %v", reqs)
	}

	// An AgentTask maps to its class.
	task := &kaalmv1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"},
		Spec:       kaalmv1alpha1.AgentTaskSpec{AgentClassRef: kaalmv1alpha1.LocalObjectReference{Name: "tcls"}},
	}
	if reqs := classForWorkload(ctx, task); len(reqs) != 1 || reqs[0].Name != "tcls" {
		t.Errorf("task should map to its class: %v", reqs)
	}

	// An empty class ref yields no requests.
	empty := &kaalmv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "e"}}
	if reqs := classForWorkload(ctx, empty); reqs != nil {
		t.Errorf("empty classRef must map to nil: %v", reqs)
	}

	// An unrelated object type yields no requests.
	if reqs := classForWorkload(ctx, &corev1.Secret{}); reqs != nil {
		t.Errorf("non-workload object must map to nil: %v", reqs)
	}
}

func TestProvidersForWorkload(t *testing.T) {
	ctx := context.Background()

	ag := &kaalmv1alpha1.Agent{Spec: kaalmv1alpha1.AgentSpec{
		Providers: []kaalmv1alpha1.AgentProviderReference{
			{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: "p1"}},
			{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: "p2"}},
		},
	}}
	if reqs := providersForWorkload(ctx, ag); len(reqs) != 2 ||
		reqs[0].Name != "p1" || reqs[1].Name != "p2" {
		t.Errorf("agent providers not mapped: %v", reqs)
	}

	task := &kaalmv1alpha1.AgentTask{Spec: kaalmv1alpha1.AgentTaskSpec{
		Providers: []kaalmv1alpha1.AgentProviderReference{
			{ProviderRef: kaalmv1alpha1.LocalObjectReference{Name: "tp"}},
		},
	}}
	if reqs := providersForWorkload(ctx, task); len(reqs) != 1 || reqs[0].Name != "tp" {
		t.Errorf("task providers not mapped: %v", reqs)
	}

	// A non-workload object yields nil.
	if reqs := providersForWorkload(ctx, &corev1.Secret{}); reqs != nil {
		t.Errorf("non-workload object must map to nil: %v", reqs)
	}
}

func TestProvidersForClass(t *testing.T) {
	ctx := context.Background()
	ac := &kaalmv1alpha1.AgentClass{Spec: kaalmv1alpha1.AgentClassSpec{
		AllowedProviders: []kaalmv1alpha1.LocalObjectReference{{Name: "a"}, {Name: "b"}},
	}}
	if reqs := providersForClass(ctx, ac); len(reqs) != 2 || reqs[0].Name != "a" || reqs[1].Name != "b" {
		t.Errorf("class allowedProviders not mapped: %v", reqs)
	}
	// A wrong type yields nil.
	if reqs := providersForClass(ctx, &kaalmv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "x"}}); reqs != nil {
		t.Errorf("non-AgentClass object must map to nil: %v", reqs)
	}
}

func TestDeref(t *testing.T) {
	if got := deref(strptr("value"), "fb"); got != "value" {
		t.Errorf("deref(non-empty) = %q, want value", got)
	}
	if got := deref(nil, "fb"); got != "fb" {
		t.Errorf("deref(nil) = %q, want fb", got)
	}
	if got := deref(strptr(""), "fb"); got != "fb" {
		t.Errorf("deref(empty) = %q, want fb", got)
	}
}

func TestEqualStrings(t *testing.T) {
	if !equalStrings([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("equal slices must compare equal")
	}
	if equalStrings([]string{"a"}, []string{"a", "b"}) {
		t.Error("different lengths must not be equal")
	}
	if equalStrings([]string{"a", "x"}, []string{"a", "b"}) {
		t.Error("differing elements must not be equal")
	}
}

// ---- cost helpers ----

func TestCheapestModel(t *testing.T) {
	mp := &kaalmv1alpha1.ModelProvider{Spec: kaalmv1alpha1.ModelProviderSpec{
		Models: []kaalmv1alpha1.ModelProviderModel{
			{ID: "bad", CostPer1MInputTokens: "nope", CostPer1MOutputTokens: "x"},
			{ID: "cheap", CostPer1MInputTokens: "1", CostPer1MOutputTokens: "1"},
			{ID: "pricey", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "10"},
		},
	}}
	got, ok := cheapestModel(mp)
	if !ok || got != "cheap" {
		t.Errorf("cheapestModel = %q, %v; want cheap,true", got, ok)
	}
	// No parseable costs -> ok false.
	none := &kaalmv1alpha1.ModelProvider{Spec: kaalmv1alpha1.ModelProviderSpec{
		Models: []kaalmv1alpha1.ModelProviderModel{{ID: "m", CostPer1MInputTokens: "x", CostPer1MOutputTokens: "y"}},
	}}
	if _, ok := cheapestModel(none); ok {
		t.Error("unparseable costs must yield ok=false")
	}
}

func TestCostSanity_WarnsWhenNotCheapest(t *testing.T) {
	rec := record.NewFakeRecorder(4)
	r := &ModelProviderReconciler{Recorder: rec}
	to := "pricey"
	mp := &kaalmv1alpha1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "mp"},
		Spec: kaalmv1alpha1.ModelProviderSpec{
			Models: []kaalmv1alpha1.ModelProviderModel{
				{ID: "cheap", CostPer1MInputTokens: "1", CostPer1MOutputTokens: "1"},
				{ID: "pricey", CostPer1MInputTokens: "10", CostPer1MOutputTokens: "10"},
			},
			Budget: kaalmv1alpha1.ModelProviderBudget{
				Policies: []kaalmv1alpha1.ModelProviderBudgetPolicy{
					{AtPercent: 100, Action: "degrade", DegradeTo: &to},
				},
			},
		},
	}
	r.costSanity(mp)
	select {
	case ev := <-rec.Events:
		if ev == "" {
			t.Error("expected a non-empty warning event")
		}
	default:
		t.Error("costSanity must warn when the degrade target is not the cheapest model")
	}

	// Degrading to the cheapest emits nothing.
	rec2 := record.NewFakeRecorder(4)
	r2 := &ModelProviderReconciler{Recorder: rec2}
	cheap := "cheap"
	mp.Spec.Budget.Policies[0].DegradeTo = &cheap
	r2.costSanity(mp)
	select {
	case <-rec2.Events:
		t.Error("no warning expected when degrading to the cheapest model")
	default:
	}
}
