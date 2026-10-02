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
	"encoding/json"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// mkBareSecret creates a provider credential Secret in the operator
// namespace with a valid key and the given labels and annotations.
func mkBareSecret(t *testing.T, name string, labels, annotations map[string]string) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testOperatorNamespace, Labels: labels, Annotations: annotations,
		},
		Data: map[string][]byte{"token": []byte("sk-test")},
	}
	if err := testClient.Create(ctxT(), s); err != nil {
		t.Fatalf("create secret %s: %v", name, err)
	}
}

// patchSecretMeta merge-patches the Secret's labels and annotations; a nil
// value in either map removes that key.
func patchSecretMeta(t *testing.T, name string, labels, annotations map[string]any) {
	t.Helper()
	meta := map[string]any{}
	if labels != nil {
		meta["labels"] = labels
	}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	patch, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		t.Fatal(err)
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testOperatorNamespace}}
	if err := testClient.Patch(ctxT(), s, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatalf("patch secret %s: %v", name, err)
	}
}

func modelProviderConditions(name string) func() []metav1.Condition {
	return func() []metav1.Condition {
		var mp kaalmv1beta1.ModelProvider
		_ = testClient.Get(ctxT(), types.NamespacedName{Name: name}, &mp)
		return mp.Status.Conditions
	}
}

// Rule 49: a ModelProvider whose Secret lacks kaalm.io/provider-credential is
// not Ready and never probes; labeling the Secret recovers it with no spec
// touch, and removing the label takes it out again.
func TestModelProvider_UnlabeledSecretIsNotOptedIn(t *testing.T) {
	const name = "mp-optin"
	mkBareSecret(t, name+"-key", nil,
		map[string]string{kaalmv1beta1.AnnotationProviderHosts: "api.example.com"})
	mkProvider(t, name, nil)
	get := modelProviderConditions(name)
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
	want := fmt.Sprintf(`Secret %q does not carry the label kaalm.io/provider-credential: "true"; `+
		`a provider may use only Secrets with that label`, name+"-key")
	if c := condition(get(), kaalmv1beta1.ConditionReady); c.Message != want {
		t.Fatalf("Ready message = %q, want %q", c.Message, want)
	}
	expectEvent(t, "ModelProvider", "", name, kaalmv1beta1.ReasonSecretNotOptedIn, corev1.EventTypeWarning, name+"-key")
	if n := fakeHealth.count(name); n != 0 {
		t.Fatalf("probe ran %d times for a provider whose Secret is not opted in", n)
	}

	patchSecretMeta(t, name+"-key", map[string]any{kaalmv1beta1.LabelProviderCredential: "true"}, nil)
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)

	patchSecretMeta(t, name+"-key", map[string]any{kaalmv1beta1.LabelProviderCredential: nil}, nil)
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonSecretNotOptedIn)
}

// Rule 50: a labeled Secret that does not list the endpoint host keeps the
// provider not Ready and unprobed; annotating it recovers the provider, and
// moving spec.endpoint to an unlisted host takes it out before any probe.
func TestModelProvider_EndpointHostMustBeApproved(t *testing.T) {
	const name = "mp-hosts"
	mkBareSecret(t, name+"-key", map[string]string{kaalmv1beta1.LabelProviderCredential: "true"}, nil)
	mkProvider(t, name, nil)
	get := modelProviderConditions(name)
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonEndpointHostNotApproved)
	want := fmt.Sprintf(`Secret %q does not list the endpoint host "api.example.com" `+
		`in its kaalm.io/provider-hosts annotation`, name+"-key")
	if c := condition(get(), kaalmv1beta1.ConditionReady); c.Message != want {
		t.Fatalf("Ready message = %q, want %q", c.Message, want)
	}
	expectEvent(t, "ModelProvider", "", name, kaalmv1beta1.ReasonEndpointHostNotApproved,
		corev1.EventTypeWarning, "api.example.com")
	if n := fakeHealth.count(name); n != 0 {
		t.Fatalf("probe ran %d times for a provider whose endpoint host is not approved", n)
	}

	patchSecretMeta(t, name+"-key", nil, map[string]any{kaalmv1beta1.AnnotationProviderHosts: "api.example.com"})
	expectReady(t, get, metav1.ConditionTrue, kaalmv1beta1.ReasonCredentialsValid)
	before := fakeHealth.count(name)

	var mp kaalmv1beta1.ModelProvider
	if err := testClient.Get(ctxT(), types.NamespacedName{Name: name}, &mp); err != nil {
		t.Fatal(err)
	}
	mp.Spec.Endpoint = "https://other.example.com"
	if err := testClient.Update(ctxT(), &mp); err != nil {
		t.Fatalf("update endpoint: %v", err)
	}
	expectReady(t, get, metav1.ConditionFalse, kaalmv1beta1.ReasonEndpointHostNotApproved)
	if n := fakeHealth.count(name); n != before {
		t.Fatalf("probe count grew from %d to %d after the endpoint moved to an unapproved host", before, n)
	}
}
