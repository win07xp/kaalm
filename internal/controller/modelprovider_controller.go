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

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/gateway"
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
	// OperatorNamespace is where credential Secrets live (kaalm-system).
	OperatorNamespace string
	// Health probes provider liveness. Injected so tests need no real provider.
	Health ProviderHealthChecker
}

// +kubebuilder:rbac:groups=kaalm.io,resources=modelproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=modelproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=modelproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups=kaalm.io,resources=agents;agenttasks;agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates and probes the provider and reconciles its status.
func (r *ModelProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mp kaalmv1beta1.ModelProvider
	if err := r.Get(ctx, req.NamespacedName, &mp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !mp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &mp)
	}

	if controllerutil.AddFinalizer(&mp, kaalmv1beta1.ProviderFinalizer) {
		if err := r.Update(ctx, &mp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	mp.Status.ObservedGeneration = mp.Generation

	// Credentials.
	credential, credReason, credMsg := r.credential(ctx, &mp)
	if credReason != kaalmv1beta1.ReasonCredentialsValid {
		r.setReady(&mp, false, credReason, credMsg)
		return r.finish(ctx, &mp, ctrl.Result{})
	}

	// Config validation: fallback tree and degrade targets.
	var problems []string
	problems = append(problems, r.validateFallback(ctx, &mp)...)
	problems = append(problems, validateDegradeTargets(&mp)...)
	problems = append(problems, validateHardPricing(&mp)...)
	r.costSanity(&mp)
	if len(problems) > 0 {
		sort.Strings(problems)
		reason := kaalmv1beta1.ReasonFallbackIneligible
		for _, p := range problems {
			if strings.Contains(p, "degradeTo") {
				reason = kaalmv1beta1.ReasonInvalidDegradeTarget
				break
			}
			if strings.Contains(p, "unpriced") {
				reason = kaalmv1beta1.ReasonHardBudgetUnpriced
				break
			}
			if strings.Contains(p, "modelMap") {
				reason = kaalmv1beta1.ReasonInvalidModelMap
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
	if err := r.reconcileAgentSpend(ctx, &mp, liveGateways); err != nil {
		return ctrl.Result{}, err
	}

	// Liveness probe.
	requeue := ctrl.Result{}
	if healthCheckEnabled(&mp) {
		res := r.Health.Probe(ctx, &mp, credential)
		switch {
		case res.AuthFailed:
			r.setHealthy(&mp, false, kaalmv1beta1.ReasonCredentialsInvalid, "provider rejected the credential")
			r.setReady(&mp, false, kaalmv1beta1.ReasonCredentialsInvalid, "provider rejected the credential")
			return r.finish(ctx, &mp, ctrl.Result{RequeueAfter: r.interval(&mp)})
		case res.Skipped:
			apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
				Type: kaalmv1beta1.ConditionHealthy, Status: metav1.ConditionUnknown,
				Reason: "ProbeSkipped", Message: "no liveness probe implemented for this provider type yet",
			})
		case res.Err != nil:
			r.setHealthy(&mp, false, kaalmv1beta1.ReasonProviderUnhealthy, res.Err.Error())
			r.Recorder.Event(&mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonProviderUnhealthy, res.Err.Error())
			requeue = ctrl.Result{RequeueAfter: r.interval(&mp)}
		default: // Healthy
			r.setHealthy(&mp, true, kaalmv1beta1.ReasonUpstreamReachable, "provider is reachable")
			requeue = ctrl.Result{RequeueAfter: r.interval(&mp)}
		}
	}

	r.setReady(&mp, true, kaalmv1beta1.ReasonCredentialsValid, "provider is valid")
	// Budget-tracked providers re-reconcile on a short cadence so the spend
	// roll-up and rollover stay fresh even without ConfigMap events.
	if requeue.RequeueAfter == 0 && gateway.PeriodKey(mp.Spec.Budget.Period, time.Now()) != "" {
		requeue = ctrl.Result{RequeueAfter: time.Minute}
	}
	logger.V(1).Info("reconciled ModelProvider", "type", mp.Spec.Type)
	return r.finish(ctx, &mp, requeue)
}

func (r *ModelProviderReconciler) reconcileDelete(
	ctx context.Context, mp *kaalmv1beta1.ModelProvider,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mp, kaalmv1beta1.ProviderFinalizer) {
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
	controllerutil.RemoveFinalizer(mp, kaalmv1beta1.ProviderFinalizer)
	return ctrl.Result{}, r.Update(ctx, mp)
}

// credential resolves the referenced Secret key and returns the credential value
// plus the condition reason.
func (r *ModelProviderReconciler) credential(
	ctx context.Context, mp *kaalmv1beta1.ModelProvider,
) (string, string, string) {
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: mp.Spec.CredentialsRef.Name}
	if err := r.Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", kaalmv1beta1.ReasonCredentialsMissing,
				fmt.Sprintf("Secret %s not found", key)
		}
		return "", kaalmv1beta1.ReasonCredentialsMissing, err.Error()
	}
	val, ok := sec.Data[mp.Spec.CredentialsRef.Key]
	if !ok || len(val) == 0 {
		return "", kaalmv1beta1.ReasonCredentialsMissing,
			fmt.Sprintf("key %q missing or empty in Secret %s", mp.Spec.CredentialsRef.Key, key)
	}
	return string(val), kaalmv1beta1.ReasonCredentialsValid, ""
}

// validateFallback walks the fallback tree detecting cycles (rule 11),
// format incompatibility (rule 12), and model maps naming models that do not
// exist on either end (rule 41). A crossing into anthropic whose mapped
// models declare no maxOutputTokens gets a Warning event on the primary: it
// stays valid, but a request without max_tokens cannot cross it.
func (r *ModelProviderReconciler) validateFallback(
	ctx context.Context, primary *kaalmv1beta1.ModelProvider,
) []string {
	type edge struct {
		parent *kaalmv1beta1.ModelProvider
		ref    kaalmv1beta1.FallbackReference
	}
	var problems []string
	var unsetMax []string
	visited := map[string]bool{primary.Name: true}
	var queue []edge
	for _, ref := range primary.Spec.Fallback {
		queue = append(queue, edge{parent: primary, ref: ref})
	}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if visited[e.ref.Name] {
			problems = append(problems, fmt.Sprintf("fallback chain is circular at %q", e.ref.Name))
			continue
		}
		visited[e.ref.Name] = true
		var child kaalmv1beta1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: e.ref.Name}, &child); err != nil {
			if apierrors.IsNotFound(err) {
				problems = append(problems, fmt.Sprintf("fallback provider %q does not exist", e.ref.Name))
			}
			continue
		}
		if !kaalmv1beta1.FallbackFormatCompatible(e.parent.Spec.Type, child.Spec.Type) {
			problems = append(problems, fmt.Sprintf(
				"fallback provider %q has type %q, which cannot follow type %q (rule 12)",
				e.ref.Name, child.Spec.Type, e.parent.Spec.Type))
		}
		parentModels := modelSet(e.parent)
		childModels := modelSet(&child)
		for key, value := range e.ref.ModelMap {
			if !parentModels[key] {
				problems = append(problems, fmt.Sprintf(
					"modelMap on fallback %q: key %q is not a model of %q", e.ref.Name, key, e.parent.Name))
			}
			if !childModels[value] {
				problems = append(problems, fmt.Sprintf(
					"modelMap on fallback %q: value %q is not a model of %q", e.ref.Name, value, e.ref.Name))
			}
		}
		if kaalmv1beta1.FallbackCrossesFormat(e.parent.Spec.Type, child.Spec.Type) &&
			child.Spec.Type == kaalmv1beta1.ProviderTypeAnthropic {
			for _, m := range e.parent.Spec.Models {
				target := m.ID
				if mapped, ok := e.ref.ModelMap[m.ID]; ok {
					target = mapped
				}
				if cm := findModel(&child, target); cm != nil && cm.MaxOutputTokens == nil {
					unsetMax = append(unsetMax, fmt.Sprintf("%s/%s", child.Name, target))
				}
			}
		}
		for _, ref := range child.Spec.Fallback {
			queue = append(queue, edge{parent: child.DeepCopy(), ref: ref})
		}
	}
	if len(unsetMax) > 0 && r.Recorder != nil {
		sort.Strings(unsetMax)
		r.Recorder.Event(primary, corev1.EventTypeWarning, kaalmv1beta1.ReasonMaxOutputTokensUnset,
			"a request without max_tokens cannot cross into these anthropic models until they declare "+
				"maxOutputTokens: "+strings.Join(unsetMax, ", "))
	}
	return problems
}

func modelSet(mp *kaalmv1beta1.ModelProvider) map[string]bool {
	set := map[string]bool{}
	for _, m := range mp.Spec.Models {
		set[m.ID] = true
	}
	return set
}

func findModel(mp *kaalmv1beta1.ModelProvider, id string) *kaalmv1beta1.ModelProviderModel {
	for i := range mp.Spec.Models {
		if mp.Spec.Models[i].ID == id {
			return &mp.Spec.Models[i]
		}
	}
	return nil
}

// validateDegradeTargets checks that every degrade policy names a real model in
// the same provider's catalog (rule 18).
func validateDegradeTargets(mp *kaalmv1beta1.ModelProvider) []string {
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

// validateHardPricing checks rule 33: hard budget enforcement requires every
// catalog model priced with values the gateway ledger can parse. An unpriced
// model costs zero in the ledger, so a cap over it is silently vacuous. The
// check must match the ledger's decimal parsing exactly, which is why it is
// reconcile-time rather than CRD CEL.
func validateHardPricing(mp *kaalmv1beta1.ModelProvider) []string {
	if mp.Spec.Budget.Enforcement != kaalmv1beta1.BudgetEnforcementHard {
		return nil
	}
	var problems []string
	for _, m := range mp.Spec.Models {
		_, errIn := strconv.ParseFloat(m.CostPer1MInputTokens, 64)
		_, errOut := strconv.ParseFloat(m.CostPer1MOutputTokens, 64)
		if errIn != nil || errOut != nil {
			problems = append(problems, fmt.Sprintf("model %q is unpriced; hard budget enforcement requires a fully priced catalog (rule 33)", m.ID))
		}
	}
	return problems
}

// setBoundaryMargin surfaces the gateway's _marginExceeded flag as the
// BoundaryMarginRaised condition, with a Warning event on the rising edge:
// observed traffic required a wider boundary margin than
// budget.hard.boundaryMarginPercent configures. The guarantee held; the knob
// is undersized for the deployment.
func (r *ModelProviderReconciler) setBoundaryMargin(mp *kaalmv1beta1.ModelProvider, raised bool) {
	was := apimeta.IsStatusConditionTrue(mp.Status.Conditions, kaalmv1beta1.ConditionBoundaryMarginRaised)
	if raised {
		apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
			Type:   kaalmv1beta1.ConditionBoundaryMarginRaised,
			Status: metav1.ConditionTrue,
			Reason: kaalmv1beta1.ReasonBoundaryMarginRaised,
			Message: "a gateway replica widened the effective boundary margin beyond " +
				"budget.hard.boundaryMarginPercent to uphold the hard-enforcement guarantee",
		})
		if !was {
			r.Recorder.Event(mp, corev1.EventTypeWarning, kaalmv1beta1.ConditionBoundaryMarginRaised,
				"observed traffic exceeded the configured boundary margin; size the knob from the overspend-bound formula")
		}
		return
	}
	if was || apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionBoundaryMarginRaised) != nil {
		apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
			Type:   kaalmv1beta1.ConditionBoundaryMarginRaised,
			Status: metav1.ConditionFalse,
			Reason: kaalmv1beta1.ReasonBoundaryMarginOK,
		})
	}
}

// costSanity emits an advisory Warning when a degrade target is not the cheapest
// model. It never blocks readiness.
func (r *ModelProviderReconciler) costSanity(mp *kaalmv1beta1.ModelProvider) {
	cheapest, ok := cheapestModel(mp)
	if !ok {
		return
	}
	for _, p := range mp.Spec.Budget.Policies {
		if p.Action == "degrade" && p.DegradeTo != nil && *p.DegradeTo != cheapest {
			r.Recorder.Event(mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonDegradeTargetNotCheapest,
				fmt.Sprintf("degradeTo %q is not the cheapest model (%q)", *p.DegradeTo, cheapest))
		}
	}
}

// cheapestModel returns the id of the model with the lowest average of its input
// and output token costs. ok is false when no model has parseable costs.
func cheapestModel(mp *kaalmv1beta1.ModelProvider) (string, bool) {
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
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexProviderRef: name}); err != nil {
		return false, err
	}
	if len(agents.Items) > 0 {
		return true, nil
	}
	var tasks kaalmv1beta1.AgentTaskList
	if err := r.List(ctx, &tasks, client.MatchingFields{IndexProviderRef: name}); err != nil {
		return false, err
	}
	if len(tasks.Items) > 0 {
		return true, nil
	}
	var classes kaalmv1beta1.AgentClassList
	if err := r.List(ctx, &classes, client.MatchingFields{IndexAllowedProviders: name}); err != nil {
		return false, err
	}
	return len(classes.Items) > 0, nil
}

// healthCheckEnabled reports whether the periodic upstream probe should run. A
// nil HealthCheck block defaults to enabled; an explicit enabled=false disables
// it (the field carries no omitempty so a false survives the wire).
func healthCheckEnabled(mp *kaalmv1beta1.ModelProvider) bool {
	return mp.Spec.HealthCheck == nil || mp.Spec.HealthCheck.Enabled
}

func (r *ModelProviderReconciler) interval(mp *kaalmv1beta1.ModelProvider) time.Duration {
	if hc := mp.Spec.HealthCheck; hc != nil && hc.IntervalSeconds > 0 {
		return time.Duration(hc.IntervalSeconds) * time.Second
	}
	return defaultHealthInterval
}

func (r *ModelProviderReconciler) setReady(mp *kaalmv1beta1.ModelProvider, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionReady, Status: status, Reason: reason, Message: msg,
	})
}

func (r *ModelProviderReconciler) setHealthy(mp *kaalmv1beta1.ModelProvider, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionHealthy, Status: status, Reason: reason, Message: msg,
	})
}

func (r *ModelProviderReconciler) finish(
	ctx context.Context, mp *kaalmv1beta1.ModelProvider, res ctrl.Result,
) (ctrl.Result, error) {
	return res, r.Status().Update(ctx, mp)
}

// SetupWithManager wires the reconciler and its reference watches.
func (r *ModelProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kaalmv1beta1.ModelProvider{}).
		Watches(&kaalmv1beta1.Agent{}, handler.EnqueueRequestsFromMapFunc(providersForWorkload)).
		Watches(&kaalmv1beta1.AgentTask{}, handler.EnqueueRequestsFromMapFunc(providersForWorkload)).
		Watches(&kaalmv1beta1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(providersForClass)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.providerForBudgetCM)).
		Complete(r)
}

// providerForBudgetCM re-enqueues the ModelProvider owning an
// kaalm-budget-{name} ConfigMap in the operator namespace, so replica
// partial writes drive the reducer event-driven.
func (r *ModelProviderReconciler) providerForBudgetCM(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.OperatorNamespace {
		return nil
	}
	name, ok := strings.CutPrefix(obj.GetName(), "kaalm-budget-")
	if !ok || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

func providersForWorkload(_ context.Context, obj client.Object) []reconcile.Request {
	var refs []kaalmv1beta1.AgentProviderReference
	switch w := obj.(type) {
	case *kaalmv1beta1.Agent:
		refs = w.Spec.Providers
	case *kaalmv1beta1.AgentTask:
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
	ac, ok := obj.(*kaalmv1beta1.AgentClass)
	if !ok {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(ac.Spec.AllowedProviders))
	for _, ref := range ac.Spec.AllowedProviders {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: ref.Name}})
	}
	return reqs
}
