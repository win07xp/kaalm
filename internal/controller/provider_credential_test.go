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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// providerSecret is a Secret in the operator namespace with the given labels,
// annotations, and data, named "cred".
func providerSecret(namespace string, labels, annotations map[string]string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cred", Namespace: namespace, Labels: labels, Annotations: annotations,
		},
		Data: data,
	}
}

// Rules 49 and 50, in order: a missing Secret first, then the opt-in label,
// then the endpoint host, then the key. The refusal messages name the Secret
// and the host, never a key.
func TestResolveProviderCredential(t *testing.T) {
	optedIn := map[string]string{kaalmv1beta1.LabelProviderCredential: "true"}
	approved := map[string]string{kaalmv1beta1.AnnotationProviderHosts: "api.example.com"}
	token := map[string][]byte{"token": []byte("sk-test")}
	const endpoint = "https://api.example.com"
	const notOptedMsg = `Secret "cred" does not carry the label kaalm.io/provider-credential: "true"; ` +
		`a provider may use only Secrets with that label`
	const notApprovedMsg = `Secret "cred" does not list the endpoint host "api.example.com" ` +
		`in its kaalm.io/provider-hosts annotation`

	cases := []struct {
		name       string
		secret     *corev1.Secret
		endpoint   string
		wantValue  string
		wantReason string
		wantMsg    string
	}{
		{name: "missing Secret", endpoint: endpoint,
			wantReason: kaalmv1beta1.ReasonCredentialsMissing,
			wantMsg:    "Secret " + testOperatorNamespace + "/cred not found"},
		{name: "unlabeled, unannotated, no key", endpoint: endpoint,
			secret:     providerSecret(testOperatorNamespace, nil, nil, nil),
			wantReason: kaalmv1beta1.ReasonSecretNotOptedIn, wantMsg: notOptedMsg},
		{name: "unlabeled with a valid key", endpoint: endpoint,
			secret:     providerSecret(testOperatorNamespace, nil, approved, token),
			wantReason: kaalmv1beta1.ReasonSecretNotOptedIn, wantMsg: notOptedMsg},
		{name: "channel label only", endpoint: endpoint,
			secret: providerSecret(testOperatorNamespace,
				map[string]string{kaalmv1beta1.LabelChannelCredential: "true"}, approved, token),
			wantReason: kaalmv1beta1.ReasonSecretNotOptedIn, wantMsg: notOptedMsg},
		{name: "labeled without annotation", endpoint: endpoint,
			secret:     providerSecret(testOperatorNamespace, optedIn, nil, token),
			wantReason: kaalmv1beta1.ReasonEndpointHostNotApproved, wantMsg: notApprovedMsg},
		{name: "annotation lists another host", endpoint: endpoint,
			secret: providerSecret(testOperatorNamespace, optedIn,
				map[string]string{kaalmv1beta1.AnnotationProviderHosts: "other.example.com"}, token),
			wantReason: kaalmv1beta1.ReasonEndpointHostNotApproved, wantMsg: notApprovedMsg},
		{name: "host only in callback-hosts", endpoint: endpoint,
			secret: providerSecret(testOperatorNamespace, optedIn,
				map[string]string{kaalmv1beta1.AnnotationCallbackHosts: "api.example.com"}, token),
			wantReason: kaalmv1beta1.ReasonEndpointHostNotApproved, wantMsg: notApprovedMsg},
		{name: "unapproved and key missing", endpoint: endpoint,
			secret:     providerSecret(testOperatorNamespace, optedIn, nil, nil),
			wantReason: kaalmv1beta1.ReasonEndpointHostNotApproved, wantMsg: notApprovedMsg},
		{name: "endpoint with no hostname", endpoint: "https://",
			secret:     providerSecret(testOperatorNamespace, optedIn, approved, token),
			wantReason: kaalmv1beta1.ReasonEndpointHostNotApproved,
			wantMsg:    `spec.endpoint "https://" has no hostname that Secret "cred" could approve`},
		{name: "endpoint with port and path", endpoint: "https://API.example.com:8443/v1",
			secret:    providerSecret(testOperatorNamespace, optedIn, approved, token),
			wantValue: "sk-test", wantReason: kaalmv1beta1.ReasonCredentialsValid},
		{name: "approved", endpoint: endpoint,
			secret:    providerSecret(testOperatorNamespace, optedIn, approved, token),
			wantValue: "sk-test", wantReason: kaalmv1beta1.ReasonCredentialsValid},
		{name: "approved, key missing", endpoint: endpoint,
			secret:     providerSecret(testOperatorNamespace, optedIn, approved, nil),
			wantReason: kaalmv1beta1.ReasonCredentialsMissing,
			wantMsg:    `key "token" missing or empty in Secret ` + testOperatorNamespace + "/cred"},
		{name: "approved, key empty", endpoint: endpoint,
			secret: providerSecret(testOperatorNamespace, optedIn, approved,
				map[string][]byte{"token": {}}),
			wantReason: kaalmv1beta1.ReasonCredentialsMissing,
			wantMsg:    `key "token" missing or empty in Secret ` + testOperatorNamespace + "/cred"},
		{name: "labeled, approved Secret in another namespace", endpoint: endpoint,
			secret:     providerSecret("tenant", optedIn, approved, token),
			wantReason: kaalmv1beta1.ReasonCredentialsMissing,
			wantMsg:    "Secret " + testOperatorNamespace + "/cred not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(testScheme(t))
			if c.secret != nil {
				b = b.WithObjects(c.secret)
			}
			ref := kaalmv1beta1.SecretKeyReference{Name: "cred", Key: "token"}
			value, reason, msg := resolveProviderCredential(
				context.Background(), b.Build(), testOperatorNamespace, c.endpoint, ref)
			if value != c.wantValue || reason != c.wantReason || msg != c.wantMsg {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)",
					value, reason, msg, c.wantValue, c.wantReason, c.wantMsg)
			}
			if (reason == kaalmv1beta1.ReasonSecretNotOptedIn || reason == kaalmv1beta1.ReasonEndpointHostNotApproved) &&
				strings.Contains(msg, "token") {
				t.Fatalf("refusal message %q names a key", msg)
			}
		})
	}
}
