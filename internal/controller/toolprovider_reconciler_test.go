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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	// namespace must never satisfy the ref, even one that passes rules 49
	// and 50, so the namespace is what makes it miss.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tp-tenant"}}
	if err := testClient.Create(ctxT(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: providerCredentialMeta("tp-sneaky-key", "tp-tenant"),
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
		ObjectMeta: providerCredentialMeta("tp-empty-key", testOperatorNamespace),
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
	expectHealthyNotProbed(t, get, "healthCheck.enabled is false")
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

// A pass that changes nothing skips the status write, so the object's
// resourceVersion holds still.
func TestToolProvider_UnchangedPassSkipsStatusWrite(t *testing.T) {
	ctx := context.Background()
	tp := &kaalmv1beta1.ToolProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tp-quiet", Generation: 1,
			Finalizers: []string{kaalmv1beta1.ToolProviderFinalizer},
		},
		Spec: kaalmv1beta1.ToolProviderSpec{Type: "mcp", Endpoint: "https://mcp.example.com"},
	}
	writes := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(tp).WithStatusSubresource(tp).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				writes++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ToolProviderReconciler{
		Client: c, Recorder: record.NewFakeRecorder(10),
		OperatorNamespace: testOperatorNamespace, Health: newFakeToolHealth(),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "tp-quiet"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var settled kaalmv1beta1.ToolProvider
	if err := c.Get(ctx, req.NamespacedName, &settled); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("first pass made %d status writes, want 1", writes)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var after kaalmv1beta1.ToolProvider
	if err := c.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || after.ResourceVersion != settled.ResourceVersion {
		t.Errorf("an unchanged pass rewrote status: writes=%d, resourceVersion %s -> %s",
			writes, settled.ResourceVersion, after.ResourceVersion)
	}
}

// drainEvents returns every event the fake recorder has buffered.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// A rejected credential records one Warning CredentialsInvalid event when
// Healthy first enters that reason, not one per probe pass, and records
// another when the credential is rejected again after a recovery.
func TestToolProvider_AuthFailedEmitsCredentialsInvalidOnEntry(t *testing.T) {
	ctx := context.Background()
	tp := &kaalmv1beta1.ToolProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tp-authev", Generation: 1,
			Finalizers: []string{kaalmv1beta1.ToolProviderFinalizer},
		},
		Spec: kaalmv1beta1.ToolProviderSpec{Type: "mcp", Endpoint: "https://mcp.example.com"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(tp).WithStatusSubresource(tp).Build()
	health := newFakeToolHealth()
	rec := record.NewFakeRecorder(10)
	r := &ToolProviderReconciler{
		Client: c, Recorder: rec,
		OperatorNamespace: testOperatorNamespace, Health: health,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "tp-authev"}}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	credEvents := func() int {
		n := 0
		for _, e := range drainEvents(rec) {
			if strings.HasPrefix(e, corev1.EventTypeWarning+" "+kaalmv1beta1.ReasonCredentialsInvalid+" ") {
				n++
			}
		}
		return n
	}

	health.set("tp-authev", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{AuthFailed: true}})
	reconcile()
	if n := credEvents(); n != 1 {
		t.Fatalf("first rejected probe recorded %d CredentialsInvalid events, want 1", n)
	}
	reconcile()
	reconcile()
	if n := credEvents(); n != 0 {
		t.Fatalf("repeat rejected probes recorded %d CredentialsInvalid events, want 0", n)
	}

	health.set("tp-authev", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Healthy: true}})
	reconcile()
	health.set("tp-authev", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{AuthFailed: true}})
	reconcile()
	if n := credEvents(); n != 1 {
		t.Fatalf("a rejection after recovery recorded %d CredentialsInvalid events, want 1", n)
	}
}

// eventsToolProvider is a ToolProvider that already carries its finalizer,
// with a credential reference and the probe on.
func eventsToolProvider(name string) *kaalmv1beta1.ToolProvider {
	return &kaalmv1beta1.ToolProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Generation: 1,
			Finalizers: []string{kaalmv1beta1.ToolProviderFinalizer},
		},
		Spec: kaalmv1beta1.ToolProviderSpec{
			Type: "mcp", Endpoint: "https://mcp.example.com",
			CredentialsRef: &kaalmv1beta1.SecretKeyReference{Name: name + "-key", Key: "token"},
			HealthCheck:    &kaalmv1beta1.ToolProviderHealthCheck{Enabled: true},
		},
	}
}

// The ToolProvider's Ready=False reasons are states: a Warning on the rising
// edge, sent only after the status write that records it succeeds.
func TestToolProvider_ReadyFalseWarningsFollowTheStatusWrite(t *testing.T) {
	cases := []struct {
		name   string
		key    bool
		probe  ToolProbeResult
		reason string
	}{
		{"credentials missing", false, ToolProbeResult{}, kaalmv1beta1.ReasonCredentialsMissing},
		{"the probe's rejected credential", true,
			ToolProbeResult{ProviderProbeResult: ProviderProbeResult{AuthFailed: true}},
			kaalmv1beta1.ReasonCredentialsInvalid},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "ev-tp-" + string(rune('a'+i))
			tp := eventsToolProvider(name)
			objs := []client.Object{tp}
			if tc.key {
				objs = append(objs, providerKey(name))
			}
			conflicts := &statusConflicts{}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(objs...).WithStatusSubresource(tp).
				WithInterceptorFuncs(conflicts.funcs()).Build()
			health := newFakeToolHealth()
			health.set(name, tc.probe)
			rec := record.NewFakeRecorder(16)
			r := &ToolProviderReconciler{
				Client: c, Recorder: rec, OperatorNamespace: testOperatorNamespace, Health: health,
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
			expectEventOnceAcrossConflict(t, r, rec, conflicts, req, "Warning "+tc.reason)
		})
	}
}

// A failing probe is an occurrence: every failing pass emits
// ProviderUnhealthy.
func TestToolProvider_ProviderUnhealthyOnEveryFailingProbe(t *testing.T) {
	tp := eventsToolProvider("ev-tp-down")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(tp, providerKey("ev-tp-down")).WithStatusSubresource(tp).Build()
	health := newFakeToolHealth()
	health.set("ev-tp-down", ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Err: errString("connection refused")}})
	rec := record.NewFakeRecorder(16)
	r := &ToolProviderReconciler{Client: c, Recorder: rec, OperatorNamespace: testOperatorNamespace, Health: health}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ev-tp-down"}}
	for range 3 {
		if _, err := r.Reconcile(ctxT(), req); err != nil {
			t.Fatal(err)
		}
	}
	if got := withPrefix(drainEvents(rec), "Warning "+kaalmv1beta1.ReasonProviderUnhealthy); len(got) != 3 {
		t.Fatalf("three failing probes emitted %d ProviderUnhealthy events, want 3", len(got))
	}
}

// Rule 49 for a ToolProvider with credentialsRef: an unlabeled Secret keeps
// it not Ready, and the probe never carries the credential until the label is
// set; removing the label takes it out again.
func TestToolProvider_UnlabeledSecretIsNotOptedIn(t *testing.T) {
	const name = "tp-optin"
	mkBareSecret(t, name+"-key", nil,
		map[string]string{kaalmv1beta1.AnnotationProviderHosts: "mcp.example.com"})
	mkToolProvider(t, name, nil)
	get := toolProviderConditions(name)
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
	want := `Secret "tp-optin-key" does not carry the label kaalm.io/provider-credential: "true"; ` +
		`a provider may use only Secrets with that label`
	if c := condition(get(), kaalmv1beta1.ConditionReady); c.Message != want {
		t.Fatalf("Ready message = %q, want %q", c.Message, want)
	}
	expectEvent(t, "ToolProvider", "", name, kaalmv1beta1.ReasonSecretNotOptedIn, corev1.EventTypeWarning, name+"-key")
	if n := fakeToolHealth.count(name); n != 0 {
		t.Fatalf("probe ran %d times for a provider whose Secret is not opted in", n)
	}

	patchSecretMeta(t, name+"-key", map[string]any{kaalmv1beta1.LabelProviderCredential: "true"}, nil)
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
	eventually(t, func() error {
		if got := fakeToolHealth.credential(name); got != "sk-test" {
			return errString("probe has not carried the credential yet")
		}
		return nil
	})

	patchSecretMeta(t, name+"-key", map[string]any{kaalmv1beta1.LabelProviderCredential: nil}, nil)
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
}

// Rule 50 for a ToolProvider with credentialsRef: the Secret must list the
// endpoint host; the probe never carries the credential before it does.
func TestToolProvider_EndpointHostMustBeApproved(t *testing.T) {
	const name = "tp-hosts"
	mkBareSecret(t, name+"-key", map[string]string{kaalmv1beta1.LabelProviderCredential: "true"},
		map[string]string{kaalmv1beta1.AnnotationProviderHosts: "other.example.com"})
	mkToolProvider(t, name, nil)
	get := toolProviderConditions(name)
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonEndpointHostNotApproved)
	want := `Secret "tp-hosts-key" does not list the endpoint host "mcp.example.com" ` +
		`in its kaalm.io/provider-hosts annotation`
	if c := condition(get(), kaalmv1beta1.ConditionReady); c.Message != want {
		t.Fatalf("Ready message = %q, want %q", c.Message, want)
	}
	expectEvent(t, "ToolProvider", "", name, kaalmv1beta1.ReasonEndpointHostNotApproved,
		corev1.EventTypeWarning, "mcp.example.com")
	if n := fakeToolHealth.count(name); n != 0 {
		t.Fatalf("probe ran %d times for a provider whose endpoint host is not approved", n)
	}

	patchSecretMeta(t, name+"-key", nil,
		map[string]any{kaalmv1beta1.AnnotationProviderHosts: "other.example.com, MCP.example.com"})
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
	eventually(t, func() error {
		if got := fakeToolHealth.credential(name); got != "sk-test" {
			return errString("probe has not carried the credential yet")
		}
		return nil
	})
}

// A ToolProvider pass that ends without probing must not keep a Healthy
// value from an earlier probe: it sets Healthy=Unknown with NotProbed.
func TestToolProvider_HealthyNotProbedWhenPassEndsEarly(t *testing.T) {
	secret := func(name string, labels, annotations map[string]string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: name + "-key", Namespace: testOperatorNamespace,
				Labels: labels, Annotations: annotations,
			},
			Data: map[string][]byte{"token": []byte("sk-test")},
		}
	}
	optIn := map[string]string{kaalmv1beta1.LabelProviderCredential: kaalmv1beta1.AnnotationTrue}
	cases := []struct {
		name    string
		objects func(name string) []client.Object
		mutate  func(tp *kaalmv1beta1.ToolProvider)
		ready   metav1.ConditionStatus
		reason  string
		why     string
	}{
		{
			name:    "credentials missing",
			objects: func(string) []client.Object { return nil },
			ready:   metav1.ConditionFalse, reason: kaalmv1beta1.ReasonCredentialsMissing,
		},
		{
			name: "secret not opted in",
			objects: func(name string) []client.Object {
				return []client.Object{secret(name, nil,
					map[string]string{kaalmv1beta1.AnnotationProviderHosts: testProviderHosts})}
			},
			ready: metav1.ConditionFalse, reason: kaalmv1beta1.ReasonSecretNotOptedIn,
		},
		{
			name: "endpoint host not approved",
			objects: func(name string) []client.Object {
				return []client.Object{secret(name, optIn,
					map[string]string{kaalmv1beta1.AnnotationProviderHosts: "other.example.com"})}
			},
			ready: metav1.ConditionFalse, reason: kaalmv1beta1.ReasonEndpointHostNotApproved,
		},
		{
			name:    "probe disabled",
			objects: func(string) []client.Object { return nil },
			mutate: func(tp *kaalmv1beta1.ToolProvider) {
				tp.Spec.CredentialsRef = nil
				tp.Spec.HealthCheck = &kaalmv1beta1.ToolProviderHealthCheck{Enabled: false}
			},
			ready: metav1.ConditionTrue, reason: kaalmv1beta1.ReasonCredentialsValid,
			why: "healthCheck.enabled is false",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			name := "tp-np-" + string(rune('a'+i))
			tp := &kaalmv1beta1.ToolProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Generation: 1,
					Finalizers: []string{kaalmv1beta1.ToolProviderFinalizer},
				},
				Spec: kaalmv1beta1.ToolProviderSpec{
					Type: "mcp", Endpoint: "https://mcp.example.com",
					CredentialsRef: &kaalmv1beta1.SecretKeyReference{Name: name + "-key", Key: "token"},
				},
				Status: kaalmv1beta1.ToolProviderStatus{
					Conditions: healthyCond(metav1.ConditionTrue, time.Now().Add(-time.Hour)),
				},
			}
			if tc.mutate != nil {
				tc.mutate(tp)
			}
			objs := append([]client.Object{tp}, tc.objects(name)...)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(objs...).WithStatusSubresource(tp).Build()
			health := newFakeToolHealth()
			r := &ToolProviderReconciler{
				Client: c, Recorder: record.NewFakeRecorder(10),
				OperatorNamespace: testOperatorNamespace, Health: health,
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			var got kaalmv1beta1.ToolProvider
			if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
				t.Fatal(err)
			}
			ready := condition(got.Status.Conditions, kaalmv1beta1.ConditionReady)
			if ready == nil || ready.Status != tc.ready || ready.Reason != tc.reason {
				t.Fatalf("Ready = %+v, want %s/%s", ready, tc.ready, tc.reason)
			}
			why := tc.why
			if why == "" {
				why = tc.reason
			}
			expectNotProbed(t, got.Status.Conditions, why)
			if n := health.count(name); n != 0 {
				t.Fatalf("probe ran %d times on a pass that should not probe", n)
			}
		})
	}
}
