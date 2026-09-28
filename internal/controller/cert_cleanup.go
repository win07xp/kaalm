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
	"sync"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// ControllerTLSSecretName is the Secret the chart's kaalm-controller-tls
// Certificate issues into the operator namespace. The controller mounts it,
// so it exists whenever the controller runs.
const ControllerTLSSecretName = "kaalm-controller-tls"

// CertCleanupCheck reports whether cert-manager runs with
// --enable-certificate-owner-ref=true, which the per-workload TLS Secret
// cleanup depends on: the Agent or AgentTask owns its Certificate, and only
// the flag makes cert-manager set an ownerReference from the Secret to the
// Certificate, so garbage collection reaches the Secret.
//
// The check reads the controller's own cert-manager-issued Secret from the
// manager cache (the Secret informer already covers the operator namespace),
// so a class pass costs a map lookup. cert-manager reconciles the reference
// onto existing Secrets when the flag changes, without reissuing, so the
// answer follows the flag once cert-manager restarts. Safe for concurrent use.
type CertCleanupCheck struct {
	Reader client.Reader
	Secret types.NamespacedName

	mu     sync.Mutex
	logged string // reason of the last logged result; logs only on change
}

// Condition returns the CertificateCleanup condition for an AgentClass. It
// errors only on a read failure other than NotFound.
func (c *CertCleanupCheck) Condition(ctx context.Context) (metav1.Condition, error) {
	cond := metav1.Condition{Type: kaalmv1beta1.ConditionCertificateCleanup}
	var secret corev1.Secret
	err := c.Reader.Get(ctx, c.Secret, &secret)
	switch {
	case apierrors.IsNotFound(err):
		cond.Status = metav1.ConditionUnknown
		cond.Reason = kaalmv1beta1.ReasonControllerSecretNotFound
		cond.Message = fmt.Sprintf("the controller's cert-manager-issued Secret %s is missing, "+
			"so whether cert-manager runs with --enable-certificate-owner-ref=true is unknown", c.Secret)
	case err != nil:
		return cond, err
	case ownedByCertificate(&secret):
		cond.Status = metav1.ConditionTrue
		cond.Reason = kaalmv1beta1.ReasonOwnerRefEnabled
		cond.Message = "cert-manager runs with --enable-certificate-owner-ref=true; " +
			"deleting an Agent or AgentTask deletes its TLS Secret"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = kaalmv1beta1.ReasonOwnerRefDisabled
		cond.Message = "cert-manager runs without --enable-certificate-owner-ref=true, " +
			"so every deleted Agent or AgentTask leaves its TLS Secret behind; " +
			"set the flag on cert-manager, which then adds the ownerReference to existing Secrets"
	}
	c.logTransition(ctx, cond)
	return cond, nil
}

func (c *CertCleanupCheck) logTransition(ctx context.Context, cond metav1.Condition) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logged == cond.Reason {
		return
	}
	c.logged = cond.Reason
	logger := log.FromContext(ctx).WithValues("secret", c.Secret.String(), "reason", cond.Reason)
	if cond.Status == metav1.ConditionTrue {
		logger.Info("certificate cleanup: " + cond.Message)
		return
	}
	logger.Error(nil, "certificate cleanup: "+cond.Message)
}

// ownedByCertificate reports whether the Secret carries cert-manager's
// ownerReference to a Certificate.
func ownedByCertificate(s *corev1.Secret) bool {
	for _, ref := range s.OwnerReferences {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err == nil && gv.Group == cmapi.SchemeGroupVersion.Group && ref.Kind == cmapi.CertificateKind {
			return true
		}
	}
	return false
}
