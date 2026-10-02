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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// resolveProviderCredential reads a ModelProvider or ToolProvider credential
// from the operator namespace only, never from a tenant namespace, and
// returns the value plus the Ready reason and message. The checks run in this
// order: the Secret must exist (CredentialsMissing), carry the rule 49 label
// (SecretNotOptedIn), list the endpoint host in its rule 50 annotation
// (EndpointHostNotApproved), and hold a non-empty key (CredentialsMissing).
// The rule 49 and 50 messages name the Secret and the host, never a key. Any
// reason other than CredentialsValid ends the reconcile pass before the
// probe, so a credential never goes to a host its Secret does not approve.
func resolveProviderCredential(
	ctx context.Context, c client.Reader, namespace, endpoint string, ref kaalmv1beta1.SecretKeyReference,
) (value, reason, message string) {
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	if err := c.Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", kaalmv1beta1.ReasonCredentialsMissing, fmt.Sprintf("Secret %s not found", key)
		}
		return "", kaalmv1beta1.ReasonCredentialsMissing, err.Error()
	}
	if !kaalmv1beta1.ProviderCredentialOptedIn(sec.Labels) {
		return "", kaalmv1beta1.ReasonSecretNotOptedIn, fmt.Sprintf(
			"Secret %q does not carry the label %s: %q; a provider may use only Secrets with that label",
			ref.Name, kaalmv1beta1.LabelProviderCredential, kaalmv1beta1.AnnotationTrue)
	}
	host := kaalmv1beta1.EndpointHost(endpoint)
	if host == "" {
		return "", kaalmv1beta1.ReasonEndpointHostNotApproved, fmt.Sprintf(
			"spec.endpoint %q has no hostname that Secret %q could approve", endpoint, ref.Name)
	}
	if !kaalmv1beta1.ProviderHostApproved(sec.Annotations, host) {
		return "", kaalmv1beta1.ReasonEndpointHostNotApproved, fmt.Sprintf(
			"Secret %q does not list the endpoint host %q in its %s annotation",
			ref.Name, host, kaalmv1beta1.AnnotationProviderHosts)
	}
	val, ok := sec.Data[ref.Key]
	if !ok || len(val) == 0 {
		return "", kaalmv1beta1.ReasonCredentialsMissing,
			fmt.Sprintf("key %q missing or empty in Secret %s", ref.Key, key)
	}
	return string(val), kaalmv1beta1.ReasonCredentialsValid, ""
}
