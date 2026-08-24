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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// ToolProviderReconciler validates a ToolProvider's credentials, probes it
// for liveness over MCP, and holds it in Terminating while referenced by an
// Agent, AgentTask, or AgentClass. The grant checks of rules 35 to 38 run on
// the workload reconcilers (toolGrantViolations). See
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
// +kubebuilder:rbac:groups=kaalm.io,resources=toolproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups=kaalm.io,resources=agents;agenttasks;agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates and probes the tool provider and reconciles its status.
func (r *ToolProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var tp kaalmv1beta1.ToolProvider
	if err := r.Get(ctx, req.NamespacedName, &tp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &tp)
	}
	if controllerutil.AddFinalizer(&tp, kaalmv1beta1.ToolProviderFinalizer) {
		if err := r.Update(ctx, &tp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	tp.Status.ObservedGeneration = tp.Generation

	// Credentials. The ref is optional: an unauthenticated server needs no
	// Secret, and the probe then sends no Authorization header.
	credential := ""
	readyMsg := "provider is valid (no credential configured)"
	if tp.Spec.CredentialsRef != nil {
		var reason, msg string
		credential, reason, msg = r.credential(ctx, &tp)
		if reason != kaalmv1beta1.ReasonCredentialsValid {
			r.setCondition(&tp, kaalmv1beta1.ConditionReady, false, reason, msg)
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
			r.setCondition(&tp, kaalmv1beta1.ConditionHealthy, false,
				kaalmv1beta1.ReasonCredentialsInvalid, "server rejected the credential")
			r.setCondition(&tp, kaalmv1beta1.ConditionReady, false,
				kaalmv1beta1.ReasonCredentialsInvalid, "server rejected the credential")
			return r.finish(ctx, &tp, ctrl.Result{RequeueAfter: r.interval(&tp)})
		case res.Err != nil:
			r.setCondition(&tp, kaalmv1beta1.ConditionHealthy, false,
				kaalmv1beta1.ReasonProviderUnhealthy, res.Err.Error())
			r.Recorder.Event(&tp, corev1.EventTypeWarning, kaalmv1beta1.ReasonProviderUnhealthy, res.Err.Error())
			requeue = ctrl.Result{RequeueAfter: r.interval(&tp)}
		default: // Healthy
			r.setCondition(&tp, kaalmv1beta1.ConditionHealthy, true,
				kaalmv1beta1.ReasonUpstreamReachable, "server is reachable")
			// The era is a property of the server, cached on status so the
			// operator sees which revision each server speaks. A failed
			// probe keeps the last negotiated value.
			tp.Status.MCPRevision = res.MCPRevision
			requeue = ctrl.Result{RequeueAfter: r.interval(&tp)}
		}
	}

	r.setCondition(&tp, kaalmv1beta1.ConditionReady, true, kaalmv1beta1.ReasonCredentialsValid, readyMsg)
	logger.V(1).Info("reconciled ToolProvider", "type", tp.Spec.Type)
	return r.finish(ctx, &tp, requeue)
}

func (r *ToolProviderReconciler) reconcileDelete(
	ctx context.Context, tp *kaalmv1beta1.ToolProvider,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(tp, kaalmv1beta1.ToolProviderFinalizer) {
		return ctrl.Result{}, nil
	}
	referenced, err := r.isReferenced(ctx, tp.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if referenced {
		// Hold in Terminating while any Agent, AgentTask, or AgentClass
		// references it. Their watches re-enqueue us when a referrer goes away.
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(tp, kaalmv1beta1.ToolProviderFinalizer)
	return ctrl.Result{}, r.Update(ctx, tp)
}

func (r *ToolProviderReconciler) isReferenced(ctx context.Context, name string) (bool, error) {
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexToolProviderRef: name}); err != nil {
		return false, err
	}
	if len(agents.Items) > 0 {
		return true, nil
	}
	var tasks kaalmv1beta1.AgentTaskList
	if err := r.List(ctx, &tasks, client.MatchingFields{IndexToolProviderRef: name}); err != nil {
		return false, err
	}
	if len(tasks.Items) > 0 {
		return true, nil
	}
	var classes kaalmv1beta1.AgentClassList
	if err := r.List(ctx, &classes, client.MatchingFields{IndexAllowedToolProviders: name}); err != nil {
		return false, err
	}
	return len(classes.Items) > 0, nil
}

// credential resolves the referenced Secret key from the operator namespace
// only, never from a tenant namespace, and returns the credential value plus
// the condition reason.
func (r *ToolProviderReconciler) credential(
	ctx context.Context, tp *kaalmv1beta1.ToolProvider,
) (string, string, string) {
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: tp.Spec.CredentialsRef.Name}
	if err := r.Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", kaalmv1beta1.ReasonCredentialsMissing,
				fmt.Sprintf("Secret %s not found", key)
		}
		return "", kaalmv1beta1.ReasonCredentialsMissing, err.Error()
	}
	val, ok := sec.Data[tp.Spec.CredentialsRef.Key]
	if !ok || len(val) == 0 {
		return "", kaalmv1beta1.ReasonCredentialsMissing,
			fmt.Sprintf("key %q missing or empty in Secret %s", tp.Spec.CredentialsRef.Key, key)
	}
	return string(val), kaalmv1beta1.ReasonCredentialsValid, ""
}

func (r *ToolProviderReconciler) interval(tp *kaalmv1beta1.ToolProvider) time.Duration {
	if hc := tp.Spec.HealthCheck; hc != nil && hc.IntervalSeconds > 0 {
		return time.Duration(hc.IntervalSeconds) * time.Second
	}
	return defaultHealthInterval
}

func (r *ToolProviderReconciler) setCondition(
	tp *kaalmv1beta1.ToolProvider, condType string, ok bool, reason, msg string,
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
	ctx context.Context, tp *kaalmv1beta1.ToolProvider, res ctrl.Result,
) (ctrl.Result, error) {
	return res, r.Status().Update(ctx, tp)
}

// SetupWithManager wires the reconciler, the credential-Secret watch, and the
// reference watches that release the deletion hold when a referrer goes away.
func (r *ToolProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kaalmv1beta1.ToolProvider{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.toolProvidersForSecret)).
		Watches(&kaalmv1beta1.Agent{}, handler.EnqueueRequestsFromMapFunc(toolProvidersForWorkload)).
		Watches(&kaalmv1beta1.AgentTask{}, handler.EnqueueRequestsFromMapFunc(toolProvidersForWorkload)).
		Watches(&kaalmv1beta1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(toolProvidersForClass)).
		Complete(r)
}

func toolProvidersForWorkload(_ context.Context, obj client.Object) []reconcile.Request {
	var grants []kaalmv1beta1.AgentToolGrant
	switch w := obj.(type) {
	case *kaalmv1beta1.Agent:
		grants = w.Spec.Tools
	case *kaalmv1beta1.AgentTask:
		grants = w.Spec.Tools
	default:
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(grants))
	for _, g := range grants {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: g.ProviderRef.Name}})
	}
	return reqs
}

func toolProvidersForClass(_ context.Context, obj client.Object) []reconcile.Request {
	ac, ok := obj.(*kaalmv1beta1.AgentClass)
	if !ok {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(ac.Spec.AllowedToolProviders))
	for _, ref := range ac.Spec.AllowedToolProviders {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: ref.Name}})
	}
	return reqs
}

// toolProvidersForSecret re-enqueues every ToolProvider whose credentialsRef
// names the changed Secret in the operator namespace, so a credential created
// after its provider recovers event-driven. ToolProviders are cluster-scoped
// and few; a list-and-filter needs no index.
func (r *ToolProviderReconciler) toolProvidersForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.OperatorNamespace {
		return nil
	}
	var tps kaalmv1beta1.ToolProviderList
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
