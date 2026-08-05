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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1alpha1 "github.com/win07xp/kaalm/api/v1alpha1"
)

// ToolProviderReconciler validates a ToolProvider's credentials and probes it
// for liveness over MCP. The grant chain (rules 35 to 38), the finalizer, and
// the reference watches land with the rest of the tool plane. See
// docs/src/controller/reconcilers.md (ToolProviderReconciler).
type ToolProviderReconciler struct {
	client.Client
	Recorder record.EventRecorder
	// OperatorNamespace is where credential Secrets live (kaalm-system).
	OperatorNamespace string
	// Health probes tool server liveness. Injected so tests need no real server.
	Health ToolHealthChecker
}

// +kubebuilder:rbac:groups=kaalm.io,resources=toolproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=toolproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates and probes the tool provider and reconciles its status.
func (r *ToolProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var tp kaalmv1alpha1.ToolProvider
	if err := r.Get(ctx, req.NamespacedName, &tp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tp.DeletionTimestamp.IsZero() {
		// No finalizer yet: it arrives with the grant chain, which creates the
		// references a deletion would need to wait on.
		return ctrl.Result{}, nil
	}

	tp.Status.ObservedGeneration = tp.Generation

	// Credentials. The ref is optional: an unauthenticated server needs no
	// Secret, and the probe then sends no Authorization header.
	credential := ""
	readyMsg := "provider is valid (no credential configured)"
	if tp.Spec.CredentialsRef != nil {
		var reason, msg string
		credential, reason, msg = r.credential(ctx, &tp)
		if reason != kaalmv1alpha1.ReasonCredentialsValid {
			r.setCondition(&tp, kaalmv1alpha1.ConditionReady, false, reason, msg)
			return r.finish(ctx, &tp, ctrl.Result{})
		}
		readyMsg = "provider is valid"
	}

	// Liveness probe.
	requeue := ctrl.Result{}
	if tp.Spec.HealthCheck == nil || tp.Spec.HealthCheck.Enabled {
		res := r.Health.Probe(ctx, &tp, credential)
		switch {
		case res.AuthFailed:
			r.setCondition(&tp, kaalmv1alpha1.ConditionHealthy, false,
				kaalmv1alpha1.ReasonCredentialsInvalid, "server rejected the credential")
			r.setCondition(&tp, kaalmv1alpha1.ConditionReady, false,
				kaalmv1alpha1.ReasonCredentialsInvalid, "server rejected the credential")
			return r.finish(ctx, &tp, ctrl.Result{RequeueAfter: r.interval(&tp)})
		case res.Err != nil:
			r.setCondition(&tp, kaalmv1alpha1.ConditionHealthy, false,
				kaalmv1alpha1.ReasonProviderUnhealthy, res.Err.Error())
			r.Recorder.Event(&tp, corev1.EventTypeWarning, kaalmv1alpha1.ReasonProviderUnhealthy, res.Err.Error())
			requeue = ctrl.Result{RequeueAfter: r.interval(&tp)}
		default: // Healthy
			r.setCondition(&tp, kaalmv1alpha1.ConditionHealthy, true,
				kaalmv1alpha1.ReasonUpstreamReachable, "server is reachable")
			requeue = ctrl.Result{RequeueAfter: r.interval(&tp)}
		}
	}

	r.setCondition(&tp, kaalmv1alpha1.ConditionReady, true, kaalmv1alpha1.ReasonCredentialsValid, readyMsg)
	logger.V(1).Info("reconciled ToolProvider", "type", tp.Spec.Type)
	return r.finish(ctx, &tp, requeue)
}

// credential resolves the referenced Secret key from the operator namespace
// only, never from a tenant namespace, and returns the credential value plus
// the condition reason.
func (r *ToolProviderReconciler) credential(
	ctx context.Context, tp *kaalmv1alpha1.ToolProvider,
) (string, string, string) {
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: tp.Spec.CredentialsRef.Name}
	if err := r.Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", kaalmv1alpha1.ReasonCredentialsMissing,
				fmt.Sprintf("Secret %s not found", key)
		}
		return "", kaalmv1alpha1.ReasonCredentialsMissing, err.Error()
	}
	val, ok := sec.Data[tp.Spec.CredentialsRef.Key]
	if !ok || len(val) == 0 {
		return "", kaalmv1alpha1.ReasonCredentialsMissing,
			fmt.Sprintf("key %q missing or empty in Secret %s", tp.Spec.CredentialsRef.Key, key)
	}
	return string(val), kaalmv1alpha1.ReasonCredentialsValid, ""
}

func (r *ToolProviderReconciler) interval(tp *kaalmv1alpha1.ToolProvider) time.Duration {
	if hc := tp.Spec.HealthCheck; hc != nil && hc.IntervalSeconds > 0 {
		return time.Duration(hc.IntervalSeconds) * time.Second
	}
	return defaultHealthInterval
}

func (r *ToolProviderReconciler) setCondition(
	tp *kaalmv1alpha1.ToolProvider, condType string, ok bool, reason, msg string,
) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&tp.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: msg,
	})
}

func (r *ToolProviderReconciler) finish(
	ctx context.Context, tp *kaalmv1alpha1.ToolProvider, res ctrl.Result,
) (ctrl.Result, error) {
	return res, r.Status().Update(ctx, tp)
}

// SetupWithManager wires the reconciler and the credential-Secret watch.
// Reference watches arrive with the grant chain.
func (r *ToolProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kaalmv1alpha1.ToolProvider{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.toolProvidersForSecret)).
		Complete(r)
}

// toolProvidersForSecret re-enqueues every ToolProvider whose credentialsRef
// names the changed Secret in the operator namespace, so a credential created
// after its provider recovers event-driven. ToolProviders are cluster-scoped
// and few; a list-and-filter needs no index.
func (r *ToolProviderReconciler) toolProvidersForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.OperatorNamespace {
		return nil
	}
	var tps kaalmv1alpha1.ToolProviderList
	if err := r.List(ctx, &tps); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, tp := range tps.Items {
		if tp.Spec.CredentialsRef != nil && tp.Spec.CredentialsRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: tp.Name}})
		}
	}
	return reqs
}
