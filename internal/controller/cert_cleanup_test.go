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

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// certificateOwnerRef is the ownerReference cert-manager writes on a
// Certificate's Secret when it runs with --enable-certificate-owner-ref.
func certificateOwnerRef(name string) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{
		APIVersion: "cert-manager.io/v1", Kind: "Certificate", Name: name,
		UID: types.UID("uid-" + name), Controller: &yes, BlockOwnerDeletion: &yes,
	}
}

func controllerTLSSecret(ns string, refs ...metav1.OwnerReference) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: ControllerTLSSecretName, Namespace: ns, OwnerReferences: refs,
	}}
}

func newCertCleanupCheck(objs ...client.Object) *CertCleanupCheck {
	return &CertCleanupCheck{
		Reader: fake.NewClientBuilder().WithObjects(objs...).Build(),
		Secret: types.NamespacedName{Namespace: "kaalm-system", Name: ControllerTLSSecretName},
	}
}

// ---- CertCleanupCheck.Condition ----

func TestCertCleanupCheck_OwnerRefPresentIsTrue(t *testing.T) {
	c := newCertCleanupCheck(controllerTLSSecret("kaalm-system", certificateOwnerRef(ControllerTLSSecretName)))
	cond, err := c.Condition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cond.Type != kaalmv1beta1.ConditionCertificateCleanup || cond.Status != metav1.ConditionTrue ||
		cond.Reason != kaalmv1beta1.ReasonOwnerRefEnabled {
		t.Fatalf("got %s=%s/%s, want CertificateCleanup=True/OwnerRefEnabled", cond.Type, cond.Status, cond.Reason)
	}
}

func TestCertCleanupCheck_OwnerRefMissingIsFalse(t *testing.T) {
	c := newCertCleanupCheck(controllerTLSSecret("kaalm-system"))
	cond, err := c.Condition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != kaalmv1beta1.ReasonOwnerRefDisabled {
		t.Fatalf("got %s/%s, want False/OwnerRefDisabled", cond.Status, cond.Reason)
	}
	for _, want := range []string{"--enable-certificate-owner-ref", "TLS Secret"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q does not name %q", cond.Message, want)
		}
	}
}

// An ownerReference to anything but a cert-manager Certificate does not
// count: only cert-manager's own reference makes the Secret collectable.
func TestCertCleanupCheck_ForeignOwnerRefIsFalse(t *testing.T) {
	foreign := certificateOwnerRef("x")
	foreign.APIVersion = "example.com/v1"
	c := newCertCleanupCheck(controllerTLSSecret("kaalm-system", foreign))
	cond, err := c.Condition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("got %s, want False for a non-cert-manager owner", cond.Status)
	}
}

func TestCertCleanupCheck_MissingSecretIsUnknown(t *testing.T) {
	c := newCertCleanupCheck()
	cond, err := c.Condition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cond.Status != metav1.ConditionUnknown || cond.Reason != kaalmv1beta1.ReasonControllerSecretNotFound {
		t.Fatalf("got %s/%s, want Unknown/ControllerSecretNotFound", cond.Status, cond.Reason)
	}
}

// Every class pass asks the check, so it logs only when the answer changes.
func TestCertCleanupCheck_LogsOncePerTransition(t *testing.T) {
	secret := controllerTLSSecret("kaalm-system")
	cl := fake.NewClientBuilder().WithObjects(secret).Build()
	c := &CertCleanupCheck{Reader: cl, Secret: client.ObjectKeyFromObject(secret)}
	var lines []string
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) {
		lines = append(lines, args)
	}, funcr.Options{}))

	for range 3 {
		if _, err := c.Condition(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d lines over three identical checks, want 1: %q", len(lines), lines)
	}

	secret.OwnerReferences = []metav1.OwnerReference{certificateOwnerRef(secret.Name)}
	if err := cl.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := c.Condition(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("logged %d lines after one transition, want 2: %q", len(lines), lines)
	}
}

// ---- AgentClass integration (envtest) ----

// The class reports the check and follows the controller Secret: adding
// cert-manager's ownerReference flips the condition without a class edit.
func TestAgentClass_CertificateCleanupFollowsControllerSecret(t *testing.T) {
	secret := controllerTLSSecret(testOperatorNamespace)
	if err := testClient.Create(ctxT(), secret); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), secret) })
	ac := &kaalmv1beta1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-certcleanup"}}
	if err := testClient.Create(ctxT(), ac); err != nil {
		t.Fatalf("create class: %v", err)
	}

	expectCertCleanup := func(status metav1.ConditionStatus, reason string) {
		t.Helper()
		eventually(t, func() error {
			var got kaalmv1beta1.AgentClass
			if err := testClient.Get(ctxT(), types.NamespacedName{Name: ac.Name}, &got); err != nil {
				return err
			}
			c := condition(got.Status.Conditions, kaalmv1beta1.ConditionCertificateCleanup)
			if c == nil || c.Status != status || c.Reason != reason {
				return errString("CertificateCleanup should be " + string(status) + "/" + reason)
			}
			return nil
		})
	}
	expectCertCleanup(metav1.ConditionFalse, kaalmv1beta1.ReasonOwnerRefDisabled)

	secret.OwnerReferences = []metav1.OwnerReference{certificateOwnerRef(secret.Name)}
	if err := testClient.Update(ctxT(), secret); err != nil {
		t.Fatalf("add owner ref: %v", err)
	}
	expectCertCleanup(metav1.ConditionTrue, kaalmv1beta1.ReasonOwnerRefEnabled)
}
