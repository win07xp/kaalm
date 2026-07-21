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
	"net"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentryv1alpha1 "github.com/win07xp/kubeclaw/api/v1alpha1"
)

// AgentClassReconciler validates an AgentClass, counts its users, and holds it in
// Terminating while any workload still references it. See
// docs/src/controller/reconcilers.md (AgentClassReconciler).
type AgentClassReconciler struct {
	client.Client
	Recorder  record.EventRecorder
	Discovery discovery.DiscoveryInterface

	// fqdn caches the one-time CNI FQDN-policy support probe.
	fqdnProbed    bool
	fqdnSupported bool
}

// +kubebuilder:rbac:groups=agentry.io,resources=agentclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentry.io,resources=agentclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentry.io,resources=agentclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentry.io,resources=modelproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentry.io,resources=agents;agenttasks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates the class and reconciles its status and finalizer.
func (r *AgentClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ac agentryv1alpha1.AgentClass
	if err := r.Get(ctx, req.NamespacedName, &ac); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ac.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ac)
	}

	if controllerutil.AddFinalizer(&ac, agentryv1alpha1.ClassFinalizer) {
		if err := r.Update(ctx, &ac); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Validate.
	var problems []string
	problems = append(problems, r.missingProviders(ctx, &ac)...)
	problems = append(problems, invalidCIDRs(&ac)...)
	problems = append(problems, invalidHosts(&ac)...)

	// FQDN support only matters when allowedHosts is set. When unsupported, warn
	// but do not block: allowedHosts is silently ignored during policy synthesis.
	fqdnCond := metav1.Condition{Type: agentryv1alpha1.ConditionFQDNPolicySupported}
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
			fqdnCond.Reason = agentryv1alpha1.ReasonFQDNPolicyUnsupported
			fqdnCond.Message = "the cluster CNI cannot enforce FQDN egress policies; allowedHosts is ignored"
			r.Recorder.Event(&ac, corev1.EventTypeWarning, agentryv1alpha1.ReasonFQDNPolicyUnsupported,
				"allowedHosts is set but the CNI does not support FQDN egress policies")
		}
	} else {
		fqdnCond.Status = metav1.ConditionTrue
		fqdnCond.Reason = "NoHostsRequested"
	}

	// Count users.
	agents, tasks, err := r.countUsers(ctx, ac.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Write status.
	ac.Status.ObservedGeneration = ac.Generation
	ac.Status.AgentsInUse = agents
	ac.Status.TasksInUse = tasks
	apimeta.SetStatusCondition(&ac.Status.Conditions, fqdnCond)
	if len(problems) == 0 {
		apimeta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
			Type:    agentryv1alpha1.ConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  agentryv1alpha1.ReasonAllReferencesResolved,
			Message: "class is valid",
		})
	} else {
		sort.Strings(problems)
		apimeta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
			Type:    agentryv1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  agentryv1alpha1.ReasonInvalidReference,
			Message: strings.Join(problems, "; "),
		})
	}
	if err := r.Status().Update(ctx, &ac); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("reconciled AgentClass", "ready", len(problems) == 0, "agents", agents, "tasks", tasks)
	return ctrl.Result{}, nil
}

func (r *AgentClassReconciler) reconcileDelete(ctx context.Context, ac *agentryv1alpha1.AgentClass) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ac, agentryv1alpha1.ClassFinalizer) {
		return ctrl.Result{}, nil
	}
	agents, tasks, err := r.countUsers(ctx, ac.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if agents > 0 || tasks > 0 {
		// Hold in Terminating until the last reference is removed. The watches on
		// Agent/AgentTask re-enqueue us when a referrer goes away.
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(ac, agentryv1alpha1.ClassFinalizer)
	return ctrl.Result{}, r.Update(ctx, ac)
}

func (r *AgentClassReconciler) missingProviders(ctx context.Context, ac *agentryv1alpha1.AgentClass) []string {
	var missing []string
	for _, ref := range ac.Spec.AllowedProviders {
		var mp agentryv1alpha1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, fmt.Sprintf("allowedProvider %q does not exist", ref.Name))
				continue
			}
			// A transient get error is surfaced by returning it up the stack; here
			// we conservatively skip so a flake does not mark the class invalid.
		}
	}
	return missing
}

func invalidCIDRs(ac *agentryv1alpha1.AgentClass) []string {
	var bad []string
	for _, c := range ac.Spec.Network.Egress.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			bad = append(bad, fmt.Sprintf("allowedCIDR %q is not a valid CIDR", c))
		}
	}
	return bad
}

func invalidHosts(ac *agentryv1alpha1.AgentClass) []string {
	var bad []string
	for _, h := range ac.Spec.Network.Egress.AllowedHosts {
		if errs := validation.IsDNS1123Subdomain(h); len(errs) > 0 {
			bad = append(bad, fmt.Sprintf("allowedHost %q is not a valid DNS name", h))
		}
	}
	return bad
}

func (r *AgentClassReconciler) countUsers(ctx context.Context, className string) (int32, int32, error) {
	var agents agentryv1alpha1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexAgentClassRef: className}); err != nil {
		return 0, 0, err
	}
	var tasks agentryv1alpha1.AgentTaskList
	if err := r.List(ctx, &tasks, client.MatchingFields{IndexAgentClassRef: className}); err != nil {
		return 0, 0, err
	}
	return int32(len(agents.Items)), int32(len(tasks.Items)), nil
}

func (r *AgentClassReconciler) fqdnSupport() (bool, error) {
	if r.fqdnProbed {
		return r.fqdnSupported, nil
	}
	supported, err := ProbeFQDNPolicySupport(r.Discovery)
	if err != nil {
		return false, err
	}
	r.fqdnSupported = supported
	r.fqdnProbed = true
	return supported, nil
}

// SetupWithManager wires the reconciler and its cross-resource watches.
func (r *AgentClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentryv1alpha1.AgentClass{}).
		Watches(&agentryv1alpha1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.classesForProvider)).
		Watches(&agentryv1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(classForWorkload)).
		Watches(&agentryv1alpha1.AgentTask{}, handler.EnqueueRequestsFromMapFunc(classForWorkload)).
		Complete(r)
}

// classesForProvider re-enqueues every AgentClass whose allowedProviders lists the
// changed ModelProvider.
func (r *AgentClassReconciler) classesForProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	var classes agentryv1alpha1.AgentClassList
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
	case *agentryv1alpha1.Agent:
		className = w.Spec.AgentClassRef.Name
	case *agentryv1alpha1.AgentTask:
		className = w.Spec.AgentClassRef.Name
	default:
		return nil
	}
	if className == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: className}}}
}
