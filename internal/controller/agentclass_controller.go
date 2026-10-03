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
	"net"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
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

// AgentClassReconciler validates an AgentClass, counts its users, and holds it in
// Terminating while any workload still references it. See
// docs/src/controller/reconcilers/agentclass.md.
type AgentClassReconciler struct {
	client.Client
	Recorder record.EventRecorder
	// FQDNSupport reports whether the CNI can enforce FQDN egress policies;
	// production passes a shared FQDNProbe. nil means unsupported.
	FQDNSupport func() (bool, error)
	// CertCleanup reports whether cert-manager cleans up workload TLS
	// Secrets; production passes one check shared by every class. nil omits
	// the CertificateCleanup condition.
	CertCleanup *CertCleanupCheck
}

// +kubebuilder:rbac:groups=kaalm.io,resources=agentclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agentclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agentclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=kaalm.io,resources=modelproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=kaalm.io,resources=agents;agenttasks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates the class and reconciles its status and finalizer.
func (r *AgentClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ac kaalmv1beta1.AgentClass
	if err := r.Get(ctx, req.NamespacedName, &ac); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ac.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ac)
	}

	if controllerutil.AddFinalizer(&ac, kaalmv1beta1.ClassFinalizer) {
		if err := r.Update(ctx, &ac); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Validate.
	var problems []string
	// The provider reads come first: a failed read fails the pass before
	// anything writes status or queues an event.
	missing, err := r.missingProviders(ctx, &ac)
	if err != nil {
		return ctrl.Result{}, err
	}
	problems = append(problems, missing...)
	missing, err = r.missingToolProviders(ctx, &ac)
	if err != nil {
		return ctrl.Result{}, err
	}
	problems = append(problems, missing...)
	badCIDRs := invalidCIDRs(&ac)
	problems = append(problems, badCIDRs...)
	problems = append(problems, invalidHosts(&ac)...)

	// warnings are the advisory findings that first appear on this pass,
	// emitted after the status write below records them.
	var warnings []heldEvent

	// FQDN support only matters when allowedHosts is set. When unsupported, warn
	// once, when the condition first turns False, but do not block: the Agent
	// and AgentTask reconcilers then synthesize no FQDN policy and allowedHosts
	// is ignored.
	fqdnCond := metav1.Condition{Type: kaalmv1beta1.ConditionFQDNPolicySupported}
	if len(ac.Spec.Network.Egress.AllowedHosts) > 0 {
		supported, err := r.fqdnSupport()
		if err != nil {
			return ctrl.Result{}, err
		}
		if supported {
			fqdnCond.Status = metav1.ConditionTrue
			fqdnCond.Reason = "FQDNPolicySupported"
		} else {
			fqdnCond.Status = metav1.ConditionFalse
			fqdnCond.Reason = kaalmv1beta1.ReasonFQDNPolicyUnsupported
			fqdnCond.Message = "the cluster CNI cannot enforce FQDN egress policies; allowedHosts is ignored"
			if !apimeta.IsStatusConditionFalse(ac.Status.Conditions, kaalmv1beta1.ConditionFQDNPolicySupported) {
				warnings = append(warnings, heldEvent{corev1.EventTypeWarning, kaalmv1beta1.ReasonFQDNPolicyUnsupported,
					"allowedHosts is set but the CNI does not support FQDN egress policies"})
			}
		}
	} else {
		fqdnCond.Status = metav1.ConditionTrue
		fqdnCond.Reason = "NoHostsRequested"
	}

	// The restricted baseline is the default; a class that declares less is
	// allowed, but never silently: the condition records it and the Warning
	// fires when it first appears.
	baseline := metav1.Condition{
		Type: kaalmv1beta1.ConditionSecurityBaseline, Status: metav1.ConditionTrue,
		Reason: kaalmv1beta1.ReasonRestrictedBaseline, Message: "workload Pods meet the restricted Pod Security Standard",
	}
	if deviations := securityBaselineDeviations(ac.Spec.Security); len(deviations) > 0 {
		baseline.Status = metav1.ConditionFalse
		baseline.Reason = kaalmv1beta1.ReasonBelowRestrictedBaseline
		baseline.Message = strings.Join(deviations, "; ")
		if !apimeta.IsStatusConditionFalse(ac.Status.Conditions, kaalmv1beta1.ConditionSecurityBaseline) {
			warnings = append(warnings, heldEvent{corev1.EventTypeWarning, kaalmv1beta1.ReasonBelowRestrictedBaseline,
				"security block is below the restricted Pod Security Standard: " + baseline.Message})
		}
	}

	// A deprecated field is accepted and has no effect, but never silently:
	// DeprecatedFields records it and the Warning fires on its rising edge.
	// A class that never set one gets no condition.
	var deprecated *metav1.Condition
	if findings := deprecatedFields(&ac.Spec); len(findings) > 0 {
		msg := strings.Join(findings, "; ")
		deprecated = &metav1.Condition{
			Type: kaalmv1beta1.ConditionDeprecatedFields, Status: metav1.ConditionTrue,
			Reason: kaalmv1beta1.ReasonDeprecatedFieldSet, Message: msg,
		}
		if !apimeta.IsStatusConditionTrue(ac.Status.Conditions, kaalmv1beta1.ConditionDeprecatedFields) {
			warnings = append(warnings, heldEvent{corev1.EventTypeWarning, kaalmv1beta1.ReasonDeprecatedFieldSet, msg})
		}
	} else if apimeta.FindStatusCondition(ac.Status.Conditions, kaalmv1beta1.ConditionDeprecatedFields) != nil {
		deprecated = &metav1.Condition{
			Type: kaalmv1beta1.ConditionDeprecatedFields, Status: metav1.ConditionFalse,
			Reason: kaalmv1beta1.ReasonNoDeprecatedFields, Message: "the class sets no deprecated field",
		}
	}

	// A cluster capability shown on each class, like FQDNPolicySupported.
	var certCleanup *metav1.Condition
	if r.CertCleanup != nil {
		c, err := r.CertCleanup.Condition(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		certCleanup = &c
	}

	// Count users.
	usage, err := r.countUsers(ctx, ac.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Write status.
	ac.Status.ObservedGeneration = ac.Generation
	ac.Status.AgentsInUse = usage.agents
	ac.Status.TasksInUse = usage.tasks
	ac.Status.AgentsReplacing = usage.replacing
	ac.Status.AgentsPendingReplacement = usage.pending
	apimeta.SetStatusCondition(&ac.Status.Conditions, fqdnCond)
	apimeta.SetStatusCondition(&ac.Status.Conditions, baseline)
	if certCleanup != nil {
		apimeta.SetStatusCondition(&ac.Status.Conditions, *certCleanup)
	}
	if deprecated != nil {
		apimeta.SetStatusCondition(&ac.Status.Conditions, *deprecated)
	}
	var invalid *metav1.Condition
	if len(problems) == 0 {
		apimeta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
			Type:    kaalmv1beta1.ConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  kaalmv1beta1.ReasonAllReferencesResolved,
			Message: "class is valid",
		})
	} else {
		sort.Strings(problems)
		// The reason names the first problem listed. "allowedCIDR" sorts
		// before every other problem, so a malformed CIDR (rule 19) wins.
		reason := kaalmv1beta1.ReasonInvalidReference
		if len(badCIDRs) > 0 {
			reason = kaalmv1beta1.ReasonInvalidCIDR
		}
		msg := strings.Join(problems, "; ")
		// Emitted after the status write below, and only when the reason
		// first appears.
		if readyFalseIsNew(ac.Status.Conditions, reason) {
			invalid = &metav1.Condition{Reason: reason, Message: msg}
		}
		apimeta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
			Type:    kaalmv1beta1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: msg,
		})
	}
	if err := r.Status().Update(ctx, &ac); err != nil {
		return ctrl.Result{}, err
	}
	for _, w := range warnings {
		r.Recorder.Event(&ac, w.eventType, w.reason, w.message)
	}
	if invalid != nil {
		r.Recorder.Event(&ac, corev1.EventTypeWarning, invalid.Reason, invalid.Message)
	}
	logger.V(1).Info("reconciled AgentClass", "ready", len(problems) == 0, "agents", usage.agents, "tasks", usage.tasks)
	return ctrl.Result{}, nil
}

func (r *AgentClassReconciler) reconcileDelete(ctx context.Context, ac *kaalmv1beta1.AgentClass) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ac, kaalmv1beta1.ClassFinalizer) {
		return ctrl.Result{}, nil
	}
	refs, err := listReferrers(ctx, r.Client, ac.Name,
		referrerIndexes{agent: IndexAgentClassRef, task: IndexAgentClassRef})
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(refs) > 0 {
		// Hold until the last reference is removed, and say so on Ready. The
		// watches on Agent/AgentTask re-enqueue us when a referrer goes away.
		return ctrl.Result{}, holdDeletion(ctx, r.Client, r.Recorder, ac, &ac.Status.Conditions, refs)
	}
	controllerutil.RemoveFinalizer(ac, kaalmv1beta1.ClassFinalizer)
	return ctrl.Result{}, r.Update(ctx, ac)
}

// missingProviders lists the allowedProviders entries with no ModelProvider.
// Any other read error is returned, so the pass fails and retries with
// backoff instead of reporting the reference as checked.
func (r *AgentClassReconciler) missingProviders(ctx context.Context, ac *kaalmv1beta1.AgentClass) ([]string, error) {
	var missing []string
	for _, ref := range ac.Spec.AllowedProviders {
		var mp kaalmv1beta1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, fmt.Sprintf("allowedProvider %q does not exist", ref.Name))
				continue
			}
			return nil, fmt.Errorf("get ModelProvider %q: %w", ref.Name, err)
		}
	}
	return missing, nil
}

// missingToolProviders lists the allowedToolProviders entries with no
// ToolProvider. Any other read error is returned, so the pass fails and
// retries with backoff instead of reporting the reference as checked.
func (r *AgentClassReconciler) missingToolProviders(ctx context.Context, ac *kaalmv1beta1.AgentClass) ([]string, error) {
	var missing []string
	for _, ref := range ac.Spec.AllowedToolProviders {
		var tp kaalmv1beta1.ToolProvider
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &tp); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, fmt.Sprintf("allowedToolProvider %q does not exist", ref.Name))
				continue
			}
			return nil, fmt.Errorf("get ToolProvider %q: %w", ref.Name, err)
		}
	}
	return missing, nil
}

// deprecatedFields is the one list of deprecated AgentClass fields. It
// returns a finding for each one the spec sets; the schema keeps them for
// compatibility, and none has an effect.
func deprecatedFields(spec *kaalmv1beta1.AgentClassSpec) []string {
	var found []string
	if spec.Network.AllowHostNetwork {
		found = append(found, "network.allowHostNetwork is deprecated and has no effect: no Pod Kaalm creates uses host networking")
	}
	return found
}

func invalidCIDRs(ac *kaalmv1beta1.AgentClass) []string {
	var bad []string
	for _, c := range ac.Spec.Network.Egress.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			bad = append(bad, fmt.Sprintf("allowedCIDR %q is not a valid CIDR", c))
		}
	}
	return bad
}

func invalidHosts(ac *kaalmv1beta1.AgentClass) []string {
	var bad []string
	for _, h := range ac.Spec.Network.Egress.AllowedHosts {
		if errs := validation.IsDNS1123Subdomain(h); len(errs) > 0 {
			bad = append(bad, fmt.Sprintf("allowedHost %q is not a valid DNS name", h))
		}
	}
	return bad
}

// classUsage counts a class's referrers, and among its Agents those in a
// spec-drift replacement: Replacing (holding a maxUnavailableOnDrift slot)
// and ReplacementPending (waiting for one).
type classUsage struct {
	agents, tasks, replacing, pending int32
}

func (r *AgentClassReconciler) countUsers(ctx context.Context, className string) (classUsage, error) {
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexAgentClassRef: className}); err != nil {
		return classUsage{}, err
	}
	var tasks kaalmv1beta1.AgentTaskList
	if err := r.List(ctx, &tasks, client.MatchingFields{IndexAgentClassRef: className}); err != nil {
		return classUsage{}, err
	}
	u := classUsage{agents: int32(len(agents.Items)), tasks: int32(len(tasks.Items))}
	for i := range agents.Items {
		switch podUpToDateReason(&agents.Items[i]) {
		case kaalmv1beta1.ReasonReplacing:
			u.replacing++
		case kaalmv1beta1.ReasonReplacementPending:
			u.pending++
		}
	}
	return u, nil
}

func (r *AgentClassReconciler) fqdnSupport() (bool, error) {
	return fqdnSupported(r.FQDNSupport)
}

// fqdnSupported calls an optional FQDN support function; nil is unsupported.
func fqdnSupported(fn func() (bool, error)) (bool, error) {
	if fn == nil {
		return false, nil
	}
	return fn()
}

// SetupWithManager wires the reconciler and its cross-resource watches.
func (r *AgentClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&kaalmv1beta1.AgentClass{}).
		Watches(&kaalmv1beta1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.classesForProvider)).
		Watches(&kaalmv1beta1.ToolProvider{}, handler.EnqueueRequestsFromMapFunc(r.classesForToolProvider)).
		Watches(&kaalmv1beta1.Agent{}, handler.EnqueueRequestsFromMapFunc(classForWorkload)).
		Watches(&kaalmv1beta1.AgentTask{}, handler.EnqueueRequestsFromMapFunc(classForWorkload))
	if r.CertCleanup != nil {
		// cert-manager adds or removes the ownerReference on the controller
		// Secret when its flag changes; re-evaluate every class then.
		key := r.CertCleanup.Secret
		b = b.Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.allClasses),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
				return o.GetNamespace() == key.Namespace && o.GetName() == key.Name
			})))
	}
	return b.Complete(r)
}

// allClasses re-enqueues every AgentClass, for a cluster-wide input such as
// the certificate cleanup check.
func (r *AgentClassReconciler) allClasses(ctx context.Context, _ client.Object) []reconcile.Request {
	var classes kaalmv1beta1.AgentClassList
	if err := r.List(ctx, &classes); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(classes.Items))
	for _, c := range classes.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name}})
	}
	return reqs
}

// classesForToolProvider re-enqueues every AgentClass whose
// allowedToolProviders lists the changed ToolProvider.
func (r *AgentClassReconciler) classesForToolProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	var classes kaalmv1beta1.AgentClassList
	if err := r.List(ctx, &classes, client.MatchingFields{IndexAllowedToolProviders: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(classes.Items))
	for _, c := range classes.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name}})
	}
	return reqs
}

// classesForProvider re-enqueues every AgentClass whose allowedProviders lists the
// changed ModelProvider.
func (r *AgentClassReconciler) classesForProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	var classes kaalmv1beta1.AgentClassList
	if err := r.List(ctx, &classes, client.MatchingFields{IndexAllowedProviders: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(classes.Items))
	for _, c := range classes.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name}})
	}
	return reqs
}

// classForWorkload re-enqueues the AgentClass a workload references, so usage
// counts and the delete hold stay fresh.
func classForWorkload(_ context.Context, obj client.Object) []reconcile.Request {
	var className string
	switch w := obj.(type) {
	case *kaalmv1beta1.Agent:
		className = w.Spec.AgentClassRef.Name
	case *kaalmv1beta1.AgentTask:
		className = w.Spec.AgentClassRef.Name
	default:
		return nil
	}
	if className == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: className}}}
}
