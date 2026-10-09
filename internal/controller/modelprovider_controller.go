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
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

const defaultHealthInterval = 60 * time.Second

// ModelProviderReconciler validates a ModelProvider's credentials, fallback tree,
// degrade targets, and allowedNamespaces patterns, reduces its budget partials, mirrors GatewayReachable
// from gateway Pod readiness, probes it for liveness, and holds it in
// Terminating while referenced. When it releases the finalizer it deletes the
// provider's budget and agent-spend ConfigMaps. See
// docs/src/controller/reconcilers/modelprovider.md.
type ModelProviderReconciler struct {
	client.Client
	Recorder record.EventRecorder
	// OperatorNamespace is where credential Secrets live (kaalm-system).
	OperatorNamespace string
	// Health probes provider liveness. Injected so tests need no real provider.
	Health ProviderHealthChecker
	// Clock is injectable for tests; nil means time.Now.
	Clock func() time.Time

	// probes records each provider's last probe, so only a pass whose probe
	// is due dials the upstream. The zero value is ready to use.
	probes probeGate[ProviderProbeResult]
	// events holds the state events a pass derives (a Ready=False reason or
	// an advisory condition turning True) until finish writes the status
	// that records them. The zero value is ready to use.
	events heldEvents
}

// +kubebuilder:rbac:groups=kaalm.io,resources=modelproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=modelproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=modelproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups=kaalm.io,resources=agents;agenttasks;agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",namespace=kaalm-system,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ModelProviderReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// Reconcile validates and probes the provider and reconciles its status.
func (r *ModelProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mp kaalmv1beta1.ModelProvider
	if err := r.Get(ctx, req.NamespacedName, &mp); err != nil {
		if apierrors.IsNotFound(err) {
			// A provider can disappear without our finalizer pass (the
			// finalizer stripped by hand); its series must not freeze.
			dropBudgetCanonical(req.Name)
			r.probes.forget(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Events held for a status write that never happened (an error before
	// finish) are dropped: the next pass derives them again.
	defer r.events.take(&mp)
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

	// The gateway-reachability mirror and the budget and agent-spend
	// reducers need neither a valid credential nor a valid spec, and the
	// gateway keeps counting spend while the provider is Ready=False, so
	// they run before the checks that can end the pass.
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

	// Credentials.
	credential, credReason, credMsg := r.credential(ctx, &mp)
	if credReason != kaalmv1beta1.ReasonCredentialsValid {
		r.setReadyFalse(&mp, credReason, credMsg)
		setHealthyNotProbed(&mp.Status.Conditions, "Ready is False with reason "+credReason)
		r.probes.forget(mp.Name)
		return r.finish(ctx, &mp, budgetRequeue(&mp, ctrl.Result{}))
	}

	// Config validation: fallback tree, degrade targets, hard pricing, and
	// allowedNamespaces patterns.
	var problems []validationProblem
	problems = append(problems, r.validateFallback(ctx, &mp)...)
	problems = append(problems, validateDegradeTargets(&mp)...)
	problems = append(problems, validateHardPricing(&mp)...)
	problems = append(problems, validateNamespacePatterns(&mp)...)
	r.costSanity(&mp)
	if len(problems) > 0 {
		reason, msg := readyFalseFromProblems(problems)
		r.setReadyFalse(&mp, reason, msg)
		setHealthyNotProbed(&mp.Status.Conditions, "Ready is False with reason "+reason)
		r.probes.forget(mp.Name)
		return r.finish(ctx, &mp, budgetRequeue(&mp, ctrl.Result{}))
	}
	if err := r.scanFallbackEligibility(ctx, &mp); err != nil {
		return ctrl.Result{}, err
	}

	// Liveness probe. Only a pass whose probe is due dials the upstream (see
	// probeGate); any other pass reapplies the recorded result.
	requeue := ctrl.Result{}
	if healthCheckEnabled(&mp) {
		now := r.now()
		key := newProbeKey(&mp, credential)
		res, wait, cached := r.probes.cached(mp.Name, key, now)
		if !cached {
			res = r.Health.Probe(ctx, &mp, credential)
		}
		// delay is the wait before the next probe: what is left of the
		// recorded one, or the interval or backoff from this probe.
		delay := func(next time.Duration) time.Duration {
			if cached {
				return wait
			}
			r.probes.record(mp.Name, key, res, now.Add(next))
			return next
		}
		switch {
		case res.AuthFailed:
			msg := "provider rejected the credential"
			if res.Err != nil {
				msg += ": " + res.Err.Error()
			}
			r.setHealthy(&mp, false, kaalmv1beta1.ReasonCredentialsInvalid, msg)
			r.setReadyFalse(&mp, kaalmv1beta1.ReasonCredentialsInvalid, msg)
			return r.finish(ctx, &mp, ctrl.Result{RequeueAfter: delay(r.probeRequeue(&mp))})
		case res.Err != nil:
			// A failed probe is an occurrence, not a state: it is reported on
			// every failing probe, and the recorder folds the repeats into
			// one event with a count, which keeps it visible through a long
			// outage. A pass that reuses the recorded failure sent nothing
			// new to report.
			r.setHealthy(&mp, false, kaalmv1beta1.ReasonProviderUnhealthy, res.Err.Error())
			if !cached {
				r.Recorder.Event(&mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonProviderUnhealthy, res.Err.Error())
			}
			requeue = ctrl.Result{RequeueAfter: delay(r.probeRequeue(&mp))}
		default: // Healthy
			r.setHealthy(&mp, true, kaalmv1beta1.ReasonUpstreamReachable, "provider is reachable")
			requeue = ctrl.Result{RequeueAfter: delay(r.interval(&mp))}
		}
	} else {
		r.probes.forget(mp.Name)
		setHealthyNotProbed(&mp.Status.Conditions, "healthCheck.enabled is false")
	}

	r.setReady(&mp, true, kaalmv1beta1.ReasonCredentialsValid, "provider is valid")
	requeue = budgetRequeue(&mp, requeue)
	logger.V(1).Info("reconciled ModelProvider", "type", mp.Spec.Type)
	return r.finish(ctx, &mp, requeue)
}

func (r *ModelProviderReconciler) reconcileDelete(
	ctx context.Context, mp *kaalmv1beta1.ModelProvider,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mp, kaalmv1beta1.ProviderFinalizer) {
		return ctrl.Result{}, nil
	}
	refs, err := r.referrers(ctx, mp.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(refs) > 0 {
		return r.holdDelete(ctx, mp, refs)
	}
	// A provider recreated under the same name must start at zero spend, so
	// the spend ConfigMaps go before the finalizer. They stay while the
	// delete is held: referrers still spend, and the gateway still counts it.
	if err := r.deleteSpendConfigMaps(ctx, mp.Name); err != nil {
		return ctrl.Result{}, err
	}
	// No budget pass runs once the delete has started, so nothing sets the
	// series again before the finalizer goes.
	dropBudgetCanonical(mp.Name)
	r.probes.forget(mp.Name)
	controllerutil.RemoveFinalizer(mp, kaalmv1beta1.ProviderFinalizer)
	return ctrl.Result{}, r.Update(ctx, mp)
}

// holdDelete holds the delete while any Agent, AgentTask, or AgentClass
// references the provider, and says so on Ready. Their watches re-enqueue
// the provider when a referrer goes away. The referrers still route through
// the provider and the gateway keeps counting its spend, so the
// gateway-reachability mirror and the budget and agent-spend reducers run
// as on every other pass; the probe does not. finish writes only a changed
// status and sends the DeletionBlocked Warning only after that write.
func (r *ModelProviderReconciler) holdDelete(
	ctx context.Context, mp *kaalmv1beta1.ModelProvider, refs []string,
) (ctrl.Result, error) {
	liveGateways, readyGateways, err := r.gatewayPods(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.setGatewayReachable(mp, readyGateways)
	if err := r.reconcileBudget(ctx, mp, liveGateways); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileAgentSpend(ctx, mp, liveGateways); err != nil {
		return ctrl.Result{}, err
	}
	setHealthyNotProbed(&mp.Status.Conditions, "deletion is held")
	r.probes.forget(mp.Name)
	if msg, first := setDeletionBlocked(&mp.Status.Conditions, refs); first {
		r.events.add(mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonDeletionBlocked, msg)
	}
	return r.finish(ctx, mp, budgetRequeue(mp, ctrl.Result{}))
}

// credential resolves the provider's credential through
// resolveProviderCredential: the Secret must exist, carry the rule 49 label,
// list spec.endpoint's host in its rule 50 annotation, and hold the key, in
// that order. It returns the value plus the Ready reason and message.
func (r *ModelProviderReconciler) credential(
	ctx context.Context, mp *kaalmv1beta1.ModelProvider,
) (string, string, string) {
	return resolveProviderCredential(ctx, r.Client, r.OperatorNamespace, mp.Spec.Endpoint, mp.Spec.CredentialsRef)
}

// validationProblem is one failed config check: the Ready=False reason the
// check reports and the message that describes the problem.
type validationProblem struct {
	reason  string
	message string
}

// validationReasonPrecedence orders the Ready=False reasons of the config
// checks. When several checks fail, the reason earliest in this list wins.
var validationReasonPrecedence = []string{
	kaalmv1beta1.ReasonInvalidDegradeTarget,
	kaalmv1beta1.ReasonHardBudgetUnpriced,
	kaalmv1beta1.ReasonInvalidModelMap,
	kaalmv1beta1.ReasonFallbackIneligible,
	kaalmv1beta1.ReasonInvalidNamespacePattern,
}

// validateNamespacePatterns reports each malformed allowedNamespaces entry
// (rule 51). The gateway still routes the valid entries; the malformed one
// matches nothing.
func validateNamespacePatterns(mp *kaalmv1beta1.ModelProvider) []validationProblem {
	var problems []validationProblem
	for _, msg := range invalidNamespacePatterns(mp.Spec.AllowedNamespaces) {
		problems = append(problems, validationProblem{reason: kaalmv1beta1.ReasonInvalidNamespacePattern, message: msg})
	}
	return problems
}

// readyFalseFromProblems picks the Ready=False reason for a non-empty set of
// problems by validationReasonPrecedence, and joins every problem's message,
// sorted, into one message. A reason missing from the precedence list ranks
// after every listed one.
func readyFalseFromProblems(problems []validationProblem) (reason, message string) {
	rank := func(reason string) int {
		if i := slices.Index(validationReasonPrecedence, reason); i >= 0 {
			return i
		}
		return len(validationReasonPrecedence)
	}
	msgs := make([]string, 0, len(problems))
	for _, p := range problems {
		if reason == "" || rank(p.reason) < rank(reason) {
			reason = p.reason
		}
		msgs = append(msgs, p.message)
	}
	sort.Strings(msgs)
	return reason, strings.Join(msgs, "; ")
}

// validateFallback walks the fallback tree detecting cycles (rule 11),
// format incompatibility (rule 12), and model maps naming models that do not
// exist on either end (rule 41). A cycle is a provider that reappears among
// its own ancestors; a provider reached twice on different branches (a
// shared backup) is not one, so its edges are checked on every branch but its
// own fallbacks are walked once, as the gateway's walk skips the repeat. A
// crossing into anthropic whose mapped models declare no maxOutputTokens sets
// the advisory MaxOutputTokensUnset condition on the primary: it stays valid,
// but a request without max_tokens cannot cross it.
func (r *ModelProviderReconciler) validateFallback(
	ctx context.Context, primary *kaalmv1beta1.ModelProvider,
) []validationProblem {
	var problems []validationProblem
	ineligible := func(format string, args ...any) {
		problems = append(problems, validationProblem{
			reason: kaalmv1beta1.ReasonFallbackIneligible, message: fmt.Sprintf(format, args...),
		})
	}
	badMap := func(format string, args ...any) {
		problems = append(problems, validationProblem{
			reason: kaalmv1beta1.ReasonInvalidModelMap, message: fmt.Sprintf(format, args...),
		})
	}
	unsetMax := map[string]bool{}
	// fetched caches each provider read; nil records one that does not exist.
	fetched := map[string]*kaalmv1beta1.ModelProvider{}
	onPath := map[string]bool{primary.Name: true}
	walked := map[string]bool{primary.Name: true}
	var walk func(parent *kaalmv1beta1.ModelProvider)
	walk = func(parent *kaalmv1beta1.ModelProvider) {
		for _, ref := range parent.Spec.Fallback {
			if onPath[ref.Name] {
				ineligible("fallback chain is circular at %q", ref.Name)
				continue
			}
			if _, seen := fetched[ref.Name]; !seen {
				var got kaalmv1beta1.ModelProvider
				if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &got); err != nil {
					if apierrors.IsNotFound(err) {
						ineligible("fallback provider %q does not exist", ref.Name)
						fetched[ref.Name] = nil
					}
					continue
				}
				fetched[ref.Name] = &got
			}
			child := fetched[ref.Name]
			if child == nil {
				continue
			}
			if !kaalmv1beta1.FallbackFormatCompatible(parent.Spec.Type, child.Spec.Type) {
				ineligible("fallback provider %q has type %q, which cannot follow type %q (rule 12)",
					ref.Name, child.Spec.Type, parent.Spec.Type)
			}
			parentModels := modelSet(parent)
			childModels := modelSet(child)
			for key, value := range ref.ModelMap {
				if !parentModels[key] {
					badMap("modelMap on fallback %q: key %q is not a model of %q", ref.Name, key, parent.Name)
				}
				if !childModels[value] {
					badMap("modelMap on fallback %q: value %q is not a model of %q", ref.Name, value, ref.Name)
				}
			}
			if kaalmv1beta1.FallbackCrossesFormat(parent.Spec.Type, child.Spec.Type) &&
				child.Spec.Type == kaalmv1beta1.ProviderTypeAnthropic {
				for _, m := range parent.Spec.Models {
					target := m.ID
					if mapped, ok := ref.ModelMap[m.ID]; ok {
						target = mapped
					}
					if cm := findModel(child, target); cm != nil && cm.MaxOutputTokens == nil {
						unsetMax[fmt.Sprintf("%s/%s", child.Name, target)] = true
					}
				}
			}
			if walked[ref.Name] {
				continue
			}
			walked[ref.Name] = true
			onPath[ref.Name] = true
			walk(child)
			delete(onPath, ref.Name)
		}
	}
	walk(primary)
	names := make([]string, 0, len(unsetMax))
	for n := range unsetMax {
		names = append(names, n)
	}
	sort.Strings(names)
	r.setMaxOutputTokensUnset(primary, names)
	return problems
}

// setMaxOutputTokensUnset records the anthropic models a crossing reaches
// without a declared maxOutputTokens in the advisory MaxOutputTokensUnset
// condition, and holds a Warning event for finish to send when the condition
// turns True. A steady finding, even with a changed list, does not warn
// again; clearing it sets the condition False without an event, and a
// provider that never had a finding gets no condition. Ready is not touched.
func (r *ModelProviderReconciler) setMaxOutputTokensUnset(mp *kaalmv1beta1.ModelProvider, names []string) {
	was := apimeta.IsStatusConditionTrue(mp.Status.Conditions, kaalmv1beta1.ConditionMaxOutputTokensUnset)
	if len(names) == 0 {
		if apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionMaxOutputTokensUnset) != nil {
			apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
				Type:   kaalmv1beta1.ConditionMaxOutputTokensUnset,
				Status: metav1.ConditionFalse,
				Reason: kaalmv1beta1.ReasonMaxOutputTokensDeclared,
			})
		}
		return
	}
	msg := "a request without max_tokens cannot cross into these anthropic models until they declare " +
		"maxOutputTokens: " + strings.Join(names, ", ")
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type:    kaalmv1beta1.ConditionMaxOutputTokensUnset,
		Status:  metav1.ConditionTrue,
		Reason:  kaalmv1beta1.ReasonMaxOutputTokensUnset,
		Message: msg,
	})
	if !was {
		r.events.add(mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonMaxOutputTokensUnset, msg)
	}
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
func validateDegradeTargets(mp *kaalmv1beta1.ModelProvider) []validationProblem {
	models := map[string]bool{}
	for _, m := range mp.Spec.Models {
		models[m.ID] = true
	}
	var problems []validationProblem
	for _, p := range mp.Spec.Budget.Policies {
		if p.Action == "degrade" {
			if p.DegradeTo == nil || !models[*p.DegradeTo] {
				target := "(unset)"
				if p.DegradeTo != nil {
					target = *p.DegradeTo
				}
				problems = append(problems, validationProblem{
					reason:  kaalmv1beta1.ReasonInvalidDegradeTarget,
					message: fmt.Sprintf("degradeTo %q is not a model in this provider", target),
				})
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
func validateHardPricing(mp *kaalmv1beta1.ModelProvider) []validationProblem {
	if mp.Spec.Budget.Enforcement != kaalmv1beta1.BudgetEnforcementHard {
		return nil
	}
	var problems []validationProblem
	for _, m := range mp.Spec.Models {
		_, errIn := strconv.ParseFloat(m.CostPer1MInputTokens, 64)
		_, errOut := strconv.ParseFloat(m.CostPer1MOutputTokens, 64)
		if errIn != nil || errOut != nil {
			problems = append(problems, validationProblem{
				reason: kaalmv1beta1.ReasonHardBudgetUnpriced,
				message: fmt.Sprintf(
					"model %q is unpriced; hard budget enforcement requires a fully priced catalog (rule 33)", m.ID),
			})
		}
	}
	return problems
}

// setBoundaryMargin surfaces the gateway's _marginExceeded flag as the
// BoundaryMarginRaised condition, with a Warning event on the rising edge
// once finish writes it:
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
			r.events.add(mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonBoundaryMarginRaised,
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

// costSanity records the degrade targets that cost more than some priced
// model in the advisory DegradeTargetNotCheapest condition, one finding per
// distinct target joined into its message, and holds one Warning event with
// that message for finish to send when the condition turns True. A target
// tied for the lowest cost is not a finding, and neither is a target without
// both prices, whose cost the check cannot judge (rule 33 covers pricing where
// budgets need it). A steady finding, even with a changed list, does not warn
// again, the same as setMaxOutputTokensUnset. It never blocks readiness.
func (r *ModelProviderReconciler) costSanity(mp *kaalmv1beta1.ModelProvider) {
	was := apimeta.IsStatusConditionTrue(mp.Status.Conditions, kaalmv1beta1.ConditionDegradeTargetNotCheapest)
	var findings []string
	if cheapest, cheapestAvg, ok := cheapestModel(mp); ok {
		seen := map[string]bool{}
		for _, p := range mp.Spec.Budget.Policies {
			if p.Action != "degrade" || p.DegradeTo == nil || seen[*p.DegradeTo] {
				continue
			}
			seen[*p.DegradeTo] = true
			avg, priced := modelAverageCost(mp, *p.DegradeTo)
			if !priced || avg.Cmp(cheapestAvg) <= 0 {
				continue
			}
			findings = append(findings, fmt.Sprintf("degradeTo %q ($%s average) costs more than %q ($%s)",
				*p.DegradeTo, formatUSD(avg), cheapest, formatUSD(cheapestAvg)))
		}
	}
	if len(findings) > 0 {
		msg := strings.Join(findings, "; ")
		apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
			Type:    kaalmv1beta1.ConditionDegradeTargetNotCheapest,
			Status:  metav1.ConditionTrue,
			Reason:  kaalmv1beta1.ReasonCheaperModelAvailable,
			Message: msg,
		})
		if !was {
			r.events.add(mp, corev1.EventTypeWarning, kaalmv1beta1.ReasonDegradeTargetNotCheapest, msg)
		}
		return
	}
	if was || apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionDegradeTargetNotCheapest) != nil {
		apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
			Type:   kaalmv1beta1.ConditionDegradeTargetNotCheapest,
			Status: metav1.ConditionFalse,
			Reason: kaalmv1beta1.ReasonDegradeTargetCheapest,
		})
	}
}

// cheapestModel returns the priced model with the lowest average of its input
// and output token costs, and that average. Among tied models it returns the
// first in spec.models, so the finding that names it is stable across passes.
// ok is false when no model is priced.
func cheapestModel(mp *kaalmv1beta1.ModelProvider) (string, *big.Rat, bool) {
	best := ""
	var bestAvg *big.Rat
	for _, m := range mp.Spec.Models {
		avg, ok := averageCost(m)
		if ok && (bestAvg == nil || avg.Cmp(bestAvg) < 0) {
			best, bestAvg = m.ID, avg
		}
	}
	return best, bestAvg, bestAvg != nil
}

// modelAverageCost returns the average cost of the first catalog entry with
// the id; ok is false when there is none or it is unpriced.
func modelAverageCost(mp *kaalmv1beta1.ModelProvider, id string) (*big.Rat, bool) {
	for _, m := range mp.Spec.Models {
		if m.ID == id {
			return averageCost(m)
		}
	}
	return nil, false
}

// averageCost is (costPer1MInputTokens + costPer1MOutputTokens) / 2 as an
// exact decimal, so prices that are equal in decimal compare equal. ok is
// false unless both prices parse.
func averageCost(m kaalmv1beta1.ModelProviderModel) (*big.Rat, bool) {
	in, okIn := parsePrice(m.CostPer1MInputTokens)
	out, okOut := parsePrice(m.CostPer1MOutputTokens)
	if !okIn || !okOut {
		return nil, false
	}
	sum := new(big.Rat).Add(in, out)
	return sum.Quo(sum, big.NewRat(2, 1)), true
}

// parsePrice reads a decimal price string exactly. A price counts only when
// the gateway ledger can also parse it (strconv.ParseFloat, as in
// validateHardPricing), which rules out the fractions big.Rat alone accepts.
func parsePrice(s string) (*big.Rat, bool) {
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return nil, false
	}
	return new(big.Rat).SetString(s)
}

// formatUSD prints an average as an exact decimal with at least two fraction
// digits: 3 is "3.00", 0.0025 is "0.0025". Averages of decimal prices always
// terminate, so the loop ends well before its cap.
func formatUSD(r *big.Rat) string {
	for prec := 2; prec < 40; prec++ {
		s := r.FloatString(prec)
		if v, ok := new(big.Rat).SetString(s); ok && v.Cmp(r) == 0 {
			return s
		}
	}
	return r.FloatString(40)
}

// referrers lists the objects that hold the provider's delete: Agents and
// AgentTasks naming it in spec.providers, and AgentClasses listing it in
// allowedProviders.
func (r *ModelProviderReconciler) referrers(ctx context.Context, name string) ([]string, error) {
	return listReferrers(ctx, r.Client, name,
		referrerIndexes{agent: IndexProviderRef, task: IndexProviderRef, class: IndexAllowedProviders})
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

// probeRequeue is the delay before the next probe: the interval, backed off
// while the Healthy condition is False (see probeRequeue).
func (r *ModelProviderReconciler) probeRequeue(mp *kaalmv1beta1.ModelProvider) time.Duration {
	return probeRequeue(mp.Status.Conditions, r.interval(mp), r.now())
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

// setReadyFalse sets Ready=False for a reason a person must fix, and holds a
// Warning event with the same reason and message for finish to send when the
// reason first appears.
func (r *ModelProviderReconciler) setReadyFalse(mp *kaalmv1beta1.ModelProvider, reason, msg string) {
	if readyFalseIsNew(mp.Status.Conditions, reason) {
		r.events.add(mp, corev1.EventTypeWarning, reason, msg)
	}
	r.setReady(mp, false, reason, msg)
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

// finish writes the provider's status only when the pass changed it against
// what the informer holds: the reconciler runs on every budget ConfigMap
// event, and a status write per pass is an update event for every watcher
// whether or not anything in it moved. The events the pass held go
// out only when its write succeeds. A pass that finds its status already
// stored drops them: the pass that stored it sent them.
func (r *ModelProviderReconciler) finish(
	ctx context.Context, mp *kaalmv1beta1.ModelProvider, res ctrl.Result,
) (ctrl.Result, error) {
	var current kaalmv1beta1.ModelProvider
	if err := r.Get(ctx, client.ObjectKeyFromObject(mp), &current); err == nil &&
		equality.Semantic.DeepEqual(current.Status, mp.Status) {
		r.events.flush(r.Recorder, mp, false)
		return res, nil
	}
	err := r.Status().Update(ctx, mp)
	r.events.flush(r.Recorder, mp, err == nil)
	return res, err
}

// SetupWithManager wires the reconciler and its reference watches.
func (r *ModelProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kaalmv1beta1.ModelProvider{}).
		Watches(&kaalmv1beta1.Agent{}, handler.EnqueueRequestsFromMapFunc(providersForWorkload),
			builder.WithPredicates(providerRefsChanged())).
		Watches(&kaalmv1beta1.AgentTask{}, handler.EnqueueRequestsFromMapFunc(providersForWorkload),
			builder.WithPredicates(providerRefsChanged())).
		Watches(&kaalmv1beta1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(providersForClass),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.providerForBudgetCM)).
		// The fallback checks read other providers' spec and existence only,
		// so a provider's status write (a spend fold) wakes no chain.
		Watches(&kaalmv1beta1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.providersWithFallback),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.providersForSecret)).
		// GatewayReachable follows gateway Pod readiness event-driven.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.allModelProviders),
			builder.WithPredicates(gatewayReadinessChanged(r.OperatorNamespace))).
		Complete(r)
}

// providersWithFallback re-enqueues every provider that declares a fallback
// when any provider is created, deleted, or has its spec edited. The fallback tree is validated transitively, so
// a provider created or fixed after its parent must wake the whole chain.
func (r *ModelProviderReconciler) providersWithFallback(ctx context.Context, obj client.Object) []reconcile.Request {
	var list kaalmv1beta1.ModelProviderList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		mp := &list.Items[i]
		if len(mp.Spec.Fallback) == 0 || mp.Name == obj.GetName() {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: mp.Name}})
	}
	return reqs
}

// providersForSecret re-enqueues the providers whose credentialsRef names a
// changed Secret in the operator namespace, so a credential created or
// rotated after the provider takes effect without another event.
func (r *ModelProviderReconciler) providersForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.OperatorNamespace {
		return nil
	}
	var list kaalmv1beta1.ModelProviderList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.CredentialsRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
		}
	}
	return reqs
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

// providersForWorkload re-enqueues the providers an Agent or AgentTask names,
// on its create and delete and when its provider references change
// (providerRefsChanged), so the delete hold and the eligibility scan follow.
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

// providersForClass re-enqueues the providers an AgentClass allows, on its
// create and delete and when its spec changes, so the delete hold follows.
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
