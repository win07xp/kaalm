/*
Copyright 2026.

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
	"sort"
	"strconv"
	"strings"
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

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
	"github.com/win07xp/kubeclaw/internal/gateway"
)

const defaultHealthInterval = 60 * time.Second

// ModelProviderReconciler validates a ModelProvider's credentials, fallback tree,
// and degrade targets, probes it for liveness, and holds it in Terminating while
// referenced. Budget reconciliation and GatewayReachable depend on the gateway and
// are deferred to a later phase. See docs/src/controller/reconcilers.md
// (ModelProviderReconciler).
type ModelProviderReconciler struct {
	client.Client
	Recorder record.EventRecorder
	// OperatorNamespace is where credential Secrets live (agentry-system).
	OperatorNamespace string
	// Health probes provider liveness. Injected so tests need no real provider.
	Health ProviderHealthChecker
}

// +kubebuilder:rbac:groups=agentry.io,resources=modelproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentry.io,resources=modelproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentry.io,resources=modelproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentry.io,resources=agents;agenttasks;agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates and probes the provider and reconciles its status.
func (r *ModelProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mp agentryv1alpha1.ModelProvider
	if err := r.Get(ctx, req.NamespacedName, &mp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !mp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &mp)
	}

	if controllerutil.AddFinalizer(&mp, agentryv1alpha1.ProviderFinalizer) {
		if err := r.Update(ctx, &mp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	mp.Status.ObservedGeneration = mp.Generation

	// Credentials.
	credential, credReason, credMsg := r.credential(ctx, &mp)
	if credReason != agentryv1alpha1.ReasonCredentialsValid {
		r.setReady(&mp, false, credReason, credMsg)
		return r.finish(ctx, &mp, ctrl.Result{})
	}

	// Config validation: fallback tree and degrade targets.
	var problems []string
	problems = append(problems, r.validateFallback(ctx, &mp)...)
	problems = append(problems, validateDegradeTargets(&mp)...)
	r.costSanity(&mp)
	if len(problems) > 0 {
		sort.Strings(problems)
		reason := agentryv1alpha1.ReasonFallbackIneligible
		for _, p := range problems {
			if strings.Contains(p, "degradeTo") {
				reason = agentryv1alpha1.ReasonInvalidDegradeTarget
				break
			}
		}
		r.setReady(&mp, false, reason, strings.Join(problems, "; "))
		return r.finish(ctx, &mp, ctrl.Result{})
	}

	// Budget reconciliation (the reducer over gateway partials) and the
	// gateway-reachability mirror.
	liveGateways, readyGateways, err := r.gatewayPods(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.setGatewayReachable(&mp, readyGateways)
	if err := r.reconcileBudget(ctx, &mp, liveGateways); err != nil {
		return ctrl.Result{}, err
	}

	// Liveness probe.
	requeue := ctrl.Result{}
	if healthCheckEnabled(&mp) {
		res := r.Health.Probe(ctx, &mp, credential)
		switch {
		case res.AuthFailed:
			r.setHealthy(&mp, false, agentryv1alpha1.ReasonCredentialsInvalid, "provider rejected the credential")
			r.setReady(&mp, false, agentryv1alpha1.ReasonCredentialsInvalid, "provider rejected the credential")
			return r.finish(ctx, &mp, ctrl.Result{RequeueAfter: r.interval(&mp)})
		case res.Skipped:
			apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
				Type: agentryv1alpha1.ConditionHealthy, Status: metav1.ConditionUnknown,
				Reason: "ProbeSkipped", Message: "no liveness probe implemented for this provider type yet",
			})
		case res.Err != nil:
			r.setHealthy(&mp, false, agentryv1alpha1.ReasonProviderUnhealthy, res.Err.Error())
			r.Recorder.Event(&mp, corev1.EventTypeWarning, agentryv1alpha1.ReasonProviderUnhealthy, res.Err.Error())
			requeue = ctrl.Result{RequeueAfter: r.interval(&mp)}
		default: // Healthy
			r.setHealthy(&mp, true, agentryv1alpha1.ReasonUpstreamReachable, "provider is reachable")
			requeue = ctrl.Result{RequeueAfter: r.interval(&mp)}
		}
	}

	r.setReady(&mp, true, agentryv1alpha1.ReasonCredentialsValid, "provider is valid")
	// Budget-tracked providers re-reconcile on a short cadence so the spend
	// roll-up and rollover stay fresh even without ConfigMap events.
	if requeue.RequeueAfter == 0 && gateway.PeriodKey(mp.Spec.Budget.Period, time.Now()) != "" {
		requeue = ctrl.Result{RequeueAfter: time.Minute}
	}
	logger.V(1).Info("reconciled ModelProvider", "type", mp.Spec.Type)
	return r.finish(ctx, &mp, requeue)
}

func (r *ModelProviderReconciler) reconcileDelete(
	ctx context.Context, mp *agentryv1alpha1.ModelProvider,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mp, agentryv1alpha1.ProviderFinalizer) {
		return ctrl.Result{}, nil
	}
	referenced, err := r.isReferenced(ctx, mp.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if referenced {
		// Hold in Terminating while any Agent, AgentTask, or AgentClass references
		// it. Their watches re-enqueue us when a referrer goes away.
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(mp, agentryv1alpha1.ProviderFinalizer)
	return ctrl.Result{}, r.Update(ctx, mp)
}

// credential resolves the referenced Secret key and returns the credential value
// plus the condition reason.
func (r *ModelProviderReconciler) credential(
	ctx context.Context, mp *agentryv1alpha1.ModelProvider,
) (string, string, string) {
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: mp.Spec.CredentialsRef.Name}
	if err := r.Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", agentryv1alpha1.ReasonCredentialsMissing,
				fmt.Sprintf("Secret %s not found", key)
		}
		return "", agentryv1alpha1.ReasonCredentialsMissing, err.Error()
	}
	val, ok := sec.Data[mp.Spec.CredentialsRef.Key]
	if !ok || len(val) == 0 {
		return "", agentryv1alpha1.ReasonCredentialsMissing,
			fmt.Sprintf("key %q missing or empty in Secret %s", mp.Spec.CredentialsRef.Key, key)
	}
	return string(val), agentryv1alpha1.ReasonCredentialsValid, ""
}

// validateFallback walks the fallback tree detecting cycles (rule 11) and type
// mismatches (rule 12).
func (r *ModelProviderReconciler) validateFallback(
	ctx context.Context, primary *agentryv1alpha1.ModelProvider,
) []string {
	var problems []string
	visited := map[string]bool{primary.Name: true}
	queue := append([]agentryv1alpha1.LocalObjectReference(nil), primary.Spec.Fallback...)
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		if visited[ref.Name] {
			problems = append(problems, fmt.Sprintf("fallback chain is circular at %q", ref.Name))
			continue
		}
		visited[ref.Name] = true
		var child agentryv1alpha1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &child); err != nil {
			if apierrors.IsNotFound(err) {
				problems = append(problems, fmt.Sprintf("fallback provider %q does not exist", ref.Name))
			}
			continue
		}
		if child.Spec.Type != primary.Spec.Type {
			problems = append(problems, fmt.Sprintf(
				"fallback provider %q has type %q, must match primary type %q",
				ref.Name, child.Spec.Type, primary.Spec.Type))
		}
		queue = append(queue, child.Spec.Fallback...)
	}
	return problems
}

// validateDegradeTargets checks that every degrade policy names a real model in
// the same provider's catalog (rule 18).
func validateDegradeTargets(mp *agentryv1alpha1.ModelProvider) []string {
	models := map[string]bool{}
	for _, m := range mp.Spec.Models {
		models[m.ID] = true
	}
	var problems []string
	for _, p := range mp.Spec.Budget.Policies {
		if p.Action == "degrade" {
			if p.DegradeTo == nil || !models[*p.DegradeTo] {
				target := "(unset)"
				if p.DegradeTo != nil {
					target = *p.DegradeTo
				}
				problems = append(problems, fmt.Sprintf("degradeTo %q is not a model in this provider", target))
			}
		}
	}
	return problems
}

// costSanity emits an advisory Warning when a degrade target is not the cheapest
// model. It never blocks readiness.
func (r *ModelProviderReconciler) costSanity(mp *agentryv1alpha1.ModelProvider) {
	cheapest, ok := cheapestModel(mp)
	if !ok {
		return
	}
	for _, p := range mp.Spec.Budget.Policies {
		if p.Action == "degrade" && p.DegradeTo != nil && *p.DegradeTo != cheapest {
			r.Recorder.Event(mp, corev1.EventTypeWarning, agentryv1alpha1.ReasonDegradeTargetNotCheapest,
				fmt.Sprintf("degradeTo %q is not the cheapest model (%q)", *p.DegradeTo, cheapest))
		}
	}
}

// cheapestModel returns the id of the model with the lowest average of its input
// and output token costs. ok is false when no model has parseable costs.
func cheapestModel(mp *agentryv1alpha1.ModelProvider) (string, bool) {
	best := ""
	var bestCost float64
	found := false
	for _, m := range mp.Spec.Models {
		in, errIn := strconv.ParseFloat(m.CostPer1MInputTokens, 64)
		out, errOut := strconv.ParseFloat(m.CostPer1MOutputTokens, 64)
		if errIn != nil || errOut != nil {
			continue
		}
		avg := (in + out) / 2
		if !found || avg < bestCost {
			best, bestCost, found = m.ID, avg, true
		}
	}
	return best, found
}

func (r *ModelProviderReconciler) isReferenced(ctx context.Context, name string) (bool, error) {
	var agents agentryv1alpha1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexProviderRef: name}); err != nil {
		return false, err
	}
	if len(agents.Items) > 0 {
		return true, nil
	}
	var tasks agentryv1alpha1.AgentTaskList
	if err := r.List(ctx, &tasks, client.MatchingFields{IndexProviderRef: name}); err != nil {
		return false, err
	}
	if len(tasks.Items) > 0 {
		return true, nil
	}
	var classes agentryv1alpha1.AgentClassList
	if err := r.List(ctx, &classes, client.MatchingFields{IndexAllowedProviders: name}); err != nil {
		return false, err
	}
	return len(classes.Items) > 0, nil
}

// healthCheckEnabled reports whether the periodic upstream probe should run. A
// nil HealthCheck block defaults to enabled; an explicit enabled=false disables
// it (the field carries no omitempty so a false survives the wire).
func healthCheckEnabled(mp *agentryv1alpha1.ModelProvider) bool {
	return mp.Spec.HealthCheck == nil || mp.Spec.HealthCheck.Enabled
}

func (r *ModelProviderReconciler) interval(mp *agentryv1alpha1.ModelProvider) time.Duration {
	if hc := mp.Spec.HealthCheck; hc != nil && hc.IntervalSeconds > 0 {
		return time.Duration(hc.IntervalSeconds) * time.Second
	}
	return defaultHealthInterval
}

func (r *ModelProviderReconciler) setReady(mp *agentryv1alpha1.ModelProvider, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type: agentryv1alpha1.ConditionReady, Status: status, Reason: reason, Message: msg,
	})
}

func (r *ModelProviderReconciler) setHealthy(mp *agentryv1alpha1.ModelProvider, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type: agentryv1alpha1.ConditionHealthy, Status: status, Reason: reason, Message: msg,
	})
}

func (r *ModelProviderReconciler) finish(
	ctx context.Context, mp *agentryv1alpha1.ModelProvider, res ctrl.Result,
) (ctrl.Result, error) {
	return res, r.Status().Update(ctx, mp)
}

// SetupWithManager wires the reconciler and its reference watches.
func (r *ModelProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentryv1alpha1.ModelProvider{}).
		Watches(&agentryv1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(providersForWorkload)).
		Watches(&agentryv1alpha1.AgentTask{}, handler.EnqueueRequestsFromMapFunc(providersForWorkload)).
		Watches(&agentryv1alpha1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(providersForClass)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.providerForBudgetCM)).
		Complete(r)
}

// providerForBudgetCM re-enqueues the ModelProvider owning an
// agentry-budget-{name} ConfigMap in the operator namespace, so replica
// partial writes drive the reducer event-driven.
func (r *ModelProviderReconciler) providerForBudgetCM(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.OperatorNamespace {
		return nil
	}
	name, ok := strings.CutPrefix(obj.GetName(), "agentry-budget-")
	if !ok || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

func providersForWorkload(_ context.Context, obj client.Object) []reconcile.Request {
	var refs []agentryv1alpha1.AgentProviderReference
	switch w := obj.(type) {
	case *agentryv1alpha1.Agent:
		refs = w.Spec.Providers
	case *agentryv1alpha1.AgentTask:
		refs = w.Spec.Providers
	default:
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(refs))
	for _, ref := range refs {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: ref.ProviderRef.Name}})
	}
	return reqs
}

func providersForClass(_ context.Context, obj client.Object) []reconcile.Request {
	ac, ok := obj.(*agentryv1alpha1.AgentClass)
	if !ok {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(ac.Spec.AllowedProviders))
	for _, ref := range ac.Spec.AllowedProviders {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: ref.Name}})
	}
	return reqs
}
