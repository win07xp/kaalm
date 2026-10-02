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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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

	// events holds the Warning a pass derives from a new Ready=False reason
	// until finish writes the status that records it. The zero value is
	// ready to use.
	events heldEvents
}

// +kubebuilder:rbac:groups=kaalm.io,resources=toolproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=toolproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=toolproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups=kaalm.io,resources=agents;agenttasks;agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",namespace=kaalm-system,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates and probes the tool provider and reconciles its status.
func (r *ToolProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var tp kaalmv1beta1.ToolProvider
	if err := r.Get(ctx, req.NamespacedName, &tp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tp.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, &tp)
	}
	// Events held for a status write that never happened are dropped: the
	// next pass derives them again.
	defer r.events.take(&tp)
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
			r.setReadyFalse(&tp, reason, msg, msg)
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
			// A state: the event fires when Ready enters CredentialsInvalid,
			// not on every probe pass while the credential stays rejected.
			r.setCondition(&tp, kaalmv1beta1.ConditionHealthy, false,
				kaalmv1beta1.ReasonCredentialsInvalid, "server rejected the credential")
			r.setReadyFalse(&tp, kaalmv1beta1.ReasonCredentialsInvalid, "server rejected the credential",
				"tool server rejected the credential; credential rotation may be needed")
			return r.finish(ctx, &tp, ctrl.Result{RequeueAfter: r.probeRequeue(&tp)})
		case res.Err != nil:
			// An occurrence: reported on every failing pass, and the recorder
			// folds the repeats into one event with a count.
			r.setCondition(&tp, kaalmv1beta1.ConditionHealthy, false,
				kaalmv1beta1.ReasonProviderUnhealthy, res.Err.Error())
			r.Recorder.Event(&tp, corev1.EventTypeWarning, kaalmv1beta1.ReasonProviderUnhealthy, res.Err.Error())
			requeue = ctrl.Result{RequeueAfter: r.probeRequeue(&tp)}
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
) error {
	if !controllerutil.ContainsFinalizer(tp, kaalmv1beta1.ToolProviderFinalizer) {
		return nil
	}
	refs, err := r.referrers(ctx, tp.Name)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		// Hold while any Agent, AgentTask, or AgentClass references it, and
		// say so on Ready. Their watches re-enqueue us when a referrer goes
		// away.
		return holdDeletion(ctx, r.Client, r.Recorder, tp, &tp.Status.Conditions, refs)
	}
	controllerutil.RemoveFinalizer(tp, kaalmv1beta1.ToolProviderFinalizer)
	return r.Update(ctx, tp)
}

// referrers lists the objects that hold the provider's delete: Agents and
// AgentTasks granting its tools in spec.tools, and AgentClasses listing it in
// allowedToolProviders.
func (r *ToolProviderReconciler) referrers(ctx context.Context, name string) ([]string, error) {
	return listReferrers(ctx, r.Client, name,
		referrerIndexes{agent: IndexToolProviderRef, task: IndexToolProviderRef, class: IndexAllowedToolProviders})
}

// credential resolves the provider's credential from the operator namespace
// only, never from a tenant namespace, through resolveProviderCredential: the
// Secret must exist, carry the rule 49 label, list spec.endpoint's host in its
// rule 50 annotation, and hold the key, in that order. It returns the value
// plus the Ready reason and message. The caller runs it only when
// credentialsRef is set.
func (r *ToolProviderReconciler) credential(
	ctx context.Context, tp *kaalmv1beta1.ToolProvider,
) (string, string, string) {
	return resolveProviderCredential(ctx, r.Client, r.OperatorNamespace, tp.Spec.Endpoint, *tp.Spec.CredentialsRef)
}

func (r *ToolProviderReconciler) interval(tp *kaalmv1beta1.ToolProvider) time.Duration {
	if hc := tp.Spec.HealthCheck; hc != nil && hc.IntervalSeconds > 0 {
		return time.Duration(hc.IntervalSeconds) * time.Second
	}
	return defaultHealthInterval
}

// probeRequeue is the delay before the next probe: the interval, backed off
// while the Healthy condition is False (see probeRequeue).
func (r *ToolProviderReconciler) probeRequeue(tp *kaalmv1beta1.ToolProvider) time.Duration {
	return probeRequeue(tp.Status.Conditions, r.interval(tp), time.Now())
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

// finish writes status only when it differs from the stored object, so a
// pass that changes nothing does not bump resourceVersion. The events the
// pass held go out only when its write succeeds; a pass that finds its
// status already stored drops them, because the pass that stored it sent
// them.
func (r *ToolProviderReconciler) finish(
	ctx context.Context, tp *kaalmv1beta1.ToolProvider, res ctrl.Result,
) (ctrl.Result, error) {
	var current kaalmv1beta1.ToolProvider
	if err := r.Get(ctx, client.ObjectKeyFromObject(tp), &current); err == nil &&
		equality.Semantic.DeepEqual(current.Status, tp.Status) {
		r.events.flush(r.Recorder, tp, false)
		return res, nil
	}
	err := r.Status().Update(ctx, tp)
	r.events.flush(r.Recorder, tp, err == nil)
	return res, err
}

// setReadyFalse sets Ready=False with reason and msg, and holds a Warning
// event with the reason and eventMsg for finish to send when the reason
// first appears.
func (r *ToolProviderReconciler) setReadyFalse(tp *kaalmv1beta1.ToolProvider, reason, msg, eventMsg string) {
	if readyFalseIsNew(tp.Status.Conditions, reason) {
		r.events.add(tp, corev1.EventTypeWarning, reason, eventMsg)
	}
	r.setCondition(tp, kaalmv1beta1.ConditionReady, false, reason, msg)
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
