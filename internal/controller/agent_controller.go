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
	"strings"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

const (
	// certWaitRequeue is the backoff while waiting for cert-manager to issue
	// the per-Agent Certificate (first issuance typically takes seconds).
	certWaitRequeue = 5 * time.Second
	// certWaitMsg is the Ready message while the Certificate is not Ready,
	// and certWaitServingMsg the one when the Agent's current Pod is Ready.
	certWaitMsg        = "waiting for cert-manager to issue the agent certificate"
	certWaitServingMsg = certWaitMsg + "; the current Pod keeps serving"
	// crashLoopThreshold is the restart count at which a CrashLoopBackOff
	// container marks the Agent Failed.
	crashLoopThreshold = 5
)

// gateRequeue is the retry interval for Ready=False gates that depend on
// unwatched resources (imagePullSecrets, existingClaim PVCs, a child name
// taken by an object the workload does not own, and a Pod create the API
// server rejected): without it a Secret created after the gate fired would
// never be observed. A variable so tests can
// shorten it.
var gateRequeue = 30 * time.Second

// AgentReconciler owns the full child-resource tree for a persistent agent:
// Certificate, ServiceAccount, Service, PVC, NetworkPolicy, and the Pod. Pod
// creation and replacement wait until the Certificate is Ready; an existing
// Pod keeps running meanwhile. It drives the whole Agent state machine:
// Pending -> Provisioning -> Running, the Idle and Hibernation cycle from the
// gateway's activity data, wake handling, and Degraded, Failed, and
// Terminating. See docs/src/controller/reconcilers/agent.md.
type AgentReconciler struct {
	client.Client
	// claimsWarned holds the ResourceClaimsIgnored rising edge (rule 53).
	claimsWarned claimsWarnings
	Recorder     record.EventRecorder
	// OperatorNamespace hosts the gateway and controller (kaalm-system).
	// Agents in this namespace are rejected to protect SAN integrity.
	OperatorNamespace string
	// SecretReader reads Secrets in user namespaces, which the manager's
	// cache does not hold. In production it is a secretwatch.Reader: one
	// name-filtered watch per referenced Secret, so repeated reads are cache
	// hits. nil falls back to the embedded client.
	SecretReader client.Reader
	// MaxConcurrentReconciles is the number of reconciles that may run at
	// once; controller-runtime still serializes per object. 0 means one.
	MaxConcurrentReconciles int
	// DNS selects the peers of the DNS egress rule on every Agent NetworkPolicy.
	DNS DNSSelector
	// Activity fetches per-namespace gateway activity for idle detection.
	// nil disables idle and hibernation transitions (no data, no evidence).
	Activity ActivityClient
	// CertLifetime sets the duration and renewBefore of each Agent's
	// Certificate. The zero value takes the defaults.
	CertLifetime CertLifetime
	// Clock is injectable for tests; nil means time.Now.
	Clock func() time.Time
	// FQDNSupport reports whether the CNI can enforce FQDN egress policies;
	// production passes the FQDNProbe shared with the AgentClassReconciler.
	// nil means unsupported: no CiliumNetworkPolicy is synthesized.
	FQDNSupport func() (bool, error)

	// driftSlots bounds concurrent spec-drift replacements per class
	// (maxUnavailableOnDrift). The zero value is ready to use.
	driftSlots driftSlots
	// events holds the events a pass derives from a status change until the
	// write that persists it succeeds. The zero value is ready to use.
	events heldEvents
}

func (r *AgentReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=kaalm.io,resources=agents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete;patch
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// The FQDN egress policy (allowedHosts). A rule on an API group the cluster
// lacks grants nothing and does no harm, so it ships on every CNI.
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;create;update;patch;delete

// Reconcile runs one pass of the Agent state machine.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var agent kaalmv1beta1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	statusBefore := agent.Status.DeepCopy()
	// Events held for a status write that never happened (no change, or an
	// error) are dropped: the next pass derives them again from what the
	// apiserver holds.
	defer r.events.take(&agent)

	if !agent.DeletionTimestamp.IsZero() {
		r.claimsWarned.forget(agent.UID)
		return ctrl.Result{}, r.reconcileDelete(ctx, &agent)
	}

	if controllerutil.AddFinalizer(&agent, kaalmv1beta1.AgentFinalizer) {
		if err := r.Update(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	agent.Status.ObservedGeneration = agent.Generation
	// Rule 53: advisory, with no condition. The Agent warns about its own
	// claims only; claims inherited from class defaults warn on the class.
	r.claimsWarned.note(r.Recorder, &agent, "spec.resources", agent.Spec.Resources.Claims)
	if agent.Status.Phase == "" {
		r.setPhase(&agent, kaalmv1beta1.AgentPending, "")
	}

	// Step 1: wake-annotation handling, with phase-dependent removal so a
	// failed reconcile cannot silently drop the wake. A wake that lands while
	// Hibernating stays on the Agent: driveHibernating finishes the Pod
	// deletion, settles Hibernated, and requeues so this step then commits
	// Resuming.
	if handled, res, err := r.reconcileWakeAnnotations(ctx, &agent); handled {
		return res, err
	}

	// Step 2: the system namespace is forbidden (SAN-integrity guard).
	if agent.Namespace == r.OperatorNamespace {
		r.setReadyGate(&agent, kaalmv1beta1.ReasonSystemNamespaceForbidden,
			fmt.Sprintf("Agents may not run in the operator namespace %q", r.OperatorNamespace))
		return ctrl.Result{}, r.updateStatusIfChanged(ctx, &agent, statusBefore)
	}

	// Step 2 continued: resolve the AgentClass.
	var class kaalmv1beta1.AgentClass
	if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.AgentClassRef.Name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			r.setReadyGate(&agent, kaalmv1beta1.ReasonInvalidReference,
				fmt.Sprintf("AgentClass %q does not exist", agent.Spec.AgentClassRef.Name))
			return ctrl.Result{}, r.updateStatusIfChanged(ctx, &agent, statusBefore)
		}
		return ctrl.Result{}, err
	}

	eff := deriveEffectiveSpec(&agent, &class)
	// Rule 9: the gateway reads the class-resolved wakeTimeout from status.
	// Every path below persists status, so the value lands on this pass.
	agent.Status.EffectiveWakeTimeout = nil
	if eff.WakeTimeout > 0 {
		agent.Status.EffectiveWakeTimeout = &metav1.Duration{Duration: eff.WakeTimeout}
	}

	// IdleDetection says when no idle timeout applies, which turns the
	// idle and hibernation cycle off.
	setIdleDetection(&agent, class.Name, eff.IdleTimeout)
	// GatewayReachable lives only while the activity step evaluates the
	// Agent. Removing it here covers the passes that end before that step;
	// the step itself covers a phase change later in the pass.
	r.dropGatewayReachableUnlessEvaluated(&agent, eff.IdleTimeout)

	// ProvidersReady mirrors the Ready state of every referenced
	// ModelProvider. A status condition only: it never moves the phase, and
	// every status write below persists it.
	r.reconcileProvidersCondition(ctx, &agent, &class)

	// Step 3 (scenario S10): surface budget exhaustion as a Degraded
	// condition without a phase transition. Runs before the degrade branch so
	// a Degraded agent re-evaluates it on every pass; the mutation is
	// persisted by whichever Status().Update the reconcile path below hits.
	r.reconcileBudgetCondition(ctx, &agent)

	// Step 4: the Degraded-triggering class-versus-spec cross-checks. All
	// outstanding reasons are evaluated together so recovery can be
	// per-condition.
	if handled, res, err := r.reconcileDegraded(ctx, &agent, &class, eff); handled {
		return res, err
	}

	// Step 5: Hibernating drives the Pod down; Hibernated holds with no Pod
	// (PVC, Service, Certificate, SA, and NetworkPolicy persist).
	switch agent.Status.Phase {
	case kaalmv1beta1.AgentHibernating:
		return r.driveHibernating(ctx, &agent)
	case kaalmv1beta1.AgentHibernated:
		return ctrl.Result{}, r.updateStatusIfChanged(ctx, &agent, statusBefore)
	}

	// Step 6: Ready=False gates that block Pod creation without degrading,
	// the rule 48 env-Secret gate among them.
	gated, gateResult, err := r.readyGates(ctx, &agent, &class, eff)
	if err != nil {
		return r.childBlocked(ctx, &agent, statusBefore, err)
	}
	if gated {
		if err := r.updateStatusIfChanged(ctx, &agent, statusBefore); err != nil {
			return ctrl.Result{}, err
		}
		return gateResult, nil
	}

	// Step 7: ensure the Certificate and hold Pod work on its readiness. With
	// no Pod, the pass ends and waits. With a Pod, the Pod keeps the
	// certificate it mounted, so the children and the activity step still
	// converge, and only Pod creation, replacement, and hibernation wait.
	tlsSecret, certReady, err := r.ensureCertificate(ctx, &agent)
	if err != nil {
		return r.childBlocked(ctx, &agent, statusBefore, err)
	}
	certHeld, noPod, err := r.awaitCertificate(ctx, &agent, statusBefore, certReady)
	if err != nil {
		return ctrl.Result{}, err
	}
	if noPod {
		return ctrl.Result{RequeueAfter: certWaitRequeue}, nil
	}

	// Step 8: converge the non-Pod children.
	if err := r.ensureChildren(ctx, &agent, &class, eff); err != nil {
		return r.childBlocked(ctx, &agent, statusBefore, err)
	}

	// Step 9: converge the Pod and derive the phase from it.
	driftWaiting, createRejected, err := r.convergePod(ctx, &agent, &class, eff, tlsSecret, certHeld)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Step 10: activity evaluation for Running and Idle agents drives the
	// idle and hibernation transitions. An Agent the step skips, including
	// one the Pod step just moved out of Running, loses GatewayReachable.
	res := ctrl.Result{}
	if r.activityStepRuns(&agent, eff.IdleTimeout) {
		res = r.evaluateActivity(ctx, &agent, eff, certHeld)
	} else {
		apimeta.RemoveStatusCondition(&agent.Status.Conditions, kaalmv1beta1.ConditionGatewayReachable)
	}

	if driftWaiting {
		res = withDriftRetry(res)
	}
	// A rejected Pod create depends on objects the controller does not
	// watch (RuntimeClasses, quotas, webhooks): re-check on gateRequeue.
	if createRejected && !res.Requeue && (res.RequeueAfter == 0 || res.RequeueAfter > gateRequeue) {
		res.RequeueAfter = gateRequeue
	}
	if certHeld {
		res = withCertWaitRetry(res)
	}

	// Step 11: write status only when the pass changed it.
	if err := r.updateStatusIfChanged(ctx, &agent, statusBefore); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("reconciled Agent", "phase", agent.Status.Phase)
	return res, nil
}

// awaitCertificate decides how a pass goes on while the Agent's Certificate
// is not Ready. With no Pod, the Agent waits (waitForCertificateWithoutPod),
// the status is written, and noPod tells the caller to end the pass. With a
// Pod, held tells the caller to go on and hold only Pod creation,
// replacement, and hibernation.
func (r *AgentReconciler) awaitCertificate(
	ctx context.Context, agent *kaalmv1beta1.Agent, before *kaalmv1beta1.AgentStatus, certReady bool,
) (held, noPod bool, err error) {
	if certReady {
		return false, false, nil
	}
	pod, err := r.ownedPod(ctx, agent)
	if err != nil {
		return false, false, err
	}
	if pod != nil {
		return true, false, nil
	}
	r.waitForCertificateWithoutPod(agent)
	return false, true, r.updateStatusIfChanged(ctx, agent, before)
}

// withCertWaitRetry makes sure a pass held on the Certificate is retried
// within certWaitRequeue when nothing else requeues it. The Certificate
// watch is the normal trigger; this retry is the fallback.
func withCertWaitRetry(res ctrl.Result) ctrl.Result {
	if !res.Requeue && res.RequeueAfter == 0 {
		res.RequeueAfter = certWaitRequeue
	}
	return res
}

// waitForCertificateWithoutPod holds an Agent that has no Pod until its
// Certificate is Ready: a woken Agent stays Resuming, every other Agent is
// Provisioning, and the Pod is created once the Certificate is Ready.
func (r *AgentReconciler) waitForCertificateWithoutPod(agent *kaalmv1beta1.Agent) {
	r.setPhase(agent, podPendingPhase(agent), "waiting for the agent certificate")
	agent.Status.PodName = ""
	r.setReady(agent, false, kaalmv1beta1.ReasonCertificateNotReady, certWaitMsg)
}

// childBlocked reports a child the pass cannot converge, and passes any
// other error through. A ChildConflictError (the name is taken by an object
// the Agent does not control) gives Ready=False ChildConflict; a
// ChildWriteRejectedError (the API server refused a create, update, or
// delete of the child) gives Ready=False ChildWriteRejected. Either way a
// Warning event reports it, the phase is kept, and the pass requeues after
// gateRequeue. Neither is a reconcile error: nothing the controller retries
// can clear it, so backoff retries would only fill the log. The pass ends
// before the Pod is converged, so no Pod is created, and a running Pod is
// left alone. The cause (a conflicting object with no owner reference, a
// quota, a webhook) raises no watch event, so the requeue is what notices it
// clearing.
func (r *AgentReconciler) childBlocked(
	ctx context.Context, agent *kaalmv1beta1.Agent, before *kaalmv1beta1.AgentStatus, err error,
) (ctrl.Result, error) {
	if cr, ok := asChildWriteRejected(err); ok {
		// The event fires only when the reason first appears: the message
		// carries quota counts that change between passes.
		r.setReadyGate(agent, kaalmv1beta1.ReasonChildWriteRejected, cr.Error())
		if err := r.updateStatusIfChanged(ctx, agent, before); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: gateRequeue}, nil
	}
	cc, ok := asChildConflict(err)
	if !ok {
		return ctrl.Result{}, err
	}
	msg := cc.Error()
	if prev := apimeta.FindStatusCondition(before.Conditions, kaalmv1beta1.ConditionReady); prev == nil ||
		prev.Reason != kaalmv1beta1.ReasonChildConflict || prev.Message != msg {
		r.events.add(agent, corev1.EventTypeWarning, kaalmv1beta1.ReasonChildConflict, msg)
	}
	r.setReady(agent, false, kaalmv1beta1.ReasonChildConflict, msg)
	if err := r.updateStatusIfChanged(ctx, agent, before); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: gateRequeue}, nil
}

// reconcileWakeAnnotations runs step 1: a wake request on an Agent that is
// not Hibernating is handled and ends the pass. A wake-trigger annotation
// with no wake request is left over (a controller that predates the trigger
// annotation consumed the wake and kept it); it is removed and the pass ends,
// so a later manual wake is not counted as a channel wake.
func (r *AgentReconciler) reconcileWakeAnnotations(
	ctx context.Context, agent *kaalmv1beta1.Agent,
) (bool, ctrl.Result, error) {
	_, wake := wakeTrigger(agent)
	if wake && agent.Status.Phase != kaalmv1beta1.AgentHibernating {
		res, err := r.handleWake(ctx, agent)
		return true, res, err
	}
	if _, trigger := agent.Annotations[kaalmv1beta1.AnnotationWakeTrigger]; trigger && !wake {
		delete(agent.Annotations, kaalmv1beta1.AnnotationWakeTrigger)
		if err := r.Update(ctx, agent); err != nil {
			return true, ctrl.Result{}, err
		}
		return true, ctrl.Result{Requeue: true}, nil
	}
	return false, ctrl.Result{}, nil
}

// handleWake implements the phase-dependent wake-annotation protocol: on a
// Hibernated agent the Resuming transition is committed BEFORE the annotation
// is removed, so an apiserver failure between the two leaves the wake intent
// observable; on any other phase the annotation is removed immediately, with
// a WakeIgnored warning except in Resuming (a benign idempotent re-attempt).
// Reconcile never calls it while Hibernating: that wake waits for Hibernated.
func (r *AgentReconciler) handleWake(ctx context.Context, agent *kaalmv1beta1.Agent) (ctrl.Result, error) {
	trigger, _ := wakeTrigger(agent)
	if agent.Status.Phase == kaalmv1beta1.AgentHibernated {
		r.setPhase(agent, kaalmv1beta1.AgentResuming, "wake requested ("+trigger+")")
		agent.Status.HibernatedAt = nil
		// The message that woke the agent is activity. The gateway records it
		// only once delivery succeeds, which is after the Pod is Ready, and
		// the controller's activity read is cached per namespace, so the
		// first Running pass would otherwise see only the pre-sleep record
		// and send the agent straight back through Idle to Hibernating.
		// evaluateActivity floors the gateway's record with this time.
		woke := metav1.NewTime(r.now())
		agent.Status.LastActivityTime = &woke
		r.setReady(agent, false, kaalmv1beta1.ReasonWoken, "wake requested; recreating the Pod")
		if err := r.writeStatus(ctx, agent); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(agent, corev1.EventTypeNormal, kaalmv1beta1.ReasonWoken, "waking from hibernation")
		wakesTotal.WithLabelValues(agent.Namespace, trigger).Inc()
		clearWake(agent)
		if err := r.Update(ctx, agent); err != nil {
			// The next reconcile observes the annotation on a Resuming agent
			// and removes it silently.
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	phase := agent.Status.Phase
	clearWake(agent)
	if err := r.Update(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	if phase != kaalmv1beta1.AgentResuming {
		r.Recorder.Event(agent, corev1.EventTypeWarning, kaalmv1beta1.ReasonWakeIgnored,
			"wake annotation observed on non-Hibernated agent; ignored")
	}
	return ctrl.Result{Requeue: true}, nil
}

// driveHibernating deletes the Pod (gracefully) and settles Hibernated once
// it is gone. Everything else survives for wake-on-demand.
func (r *AgentReconciler) driveHibernating(ctx context.Context, agent *kaalmv1beta1.Agent) (ctrl.Result, error) {
	pod, err := r.ownedPod(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod != nil {
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, r.writeStatus(ctx, agent)
	}
	r.setPhase(agent, kaalmv1beta1.AgentHibernated, "Pod deleted, state retained")
	// With no Pod there is nothing to replace: an Agent that waited for a
	// drift slot stops waiting, and its wake creates the Pod from the
	// current spec.
	apimeta.RemoveStatusCondition(&agent.Status.Conditions, kaalmv1beta1.ConditionPodUpToDate)
	now := metav1.NewTime(r.now())
	agent.Status.HibernatedAt = &now
	agent.Status.PodName = ""
	r.setReady(agent, false, kaalmv1beta1.ReasonHibernated, "Pod deleted; PVC retained for wake")
	r.events.add(agent, corev1.EventTypeNormal, kaalmv1beta1.ReasonHibernated,
		"hibernated: Pod deleted, state retained")
	if err := r.writeStatus(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	hibernationsTotal.WithLabelValues(agent.Namespace).Inc()
	// A wake requested while Hibernating is honored now that the Pod is gone.
	_, wake := wakeTrigger(agent)
	return ctrl.Result{Requeue: wake}, nil
}

// evaluateActivity reads the gateway activity data and drives Running <->
// Idle and Idle -> Hibernating. Absence of data is not evidence of
// inactivity: unreachable gateways preserve the phase, and silence counts
// only once a replica has been up for idleTimeout. While the Agent's
// Certificate is not Ready (hibernationHeld), an Idle Agent keeps its Pod,
// because a wake could not create one; it hibernates on the first pass after
// the Certificate is Ready.
func (r *AgentReconciler) evaluateActivity(
	ctx context.Context, agent *kaalmv1beta1.Agent, eff effectiveAgentSpec, hibernationHeld bool,
) ctrl.Result {
	now := r.now()
	reachable, total, err := r.Activity.NamespaceActivity(ctx, agent.Namespace)
	if err != nil || total == 0 || len(reachable) == 0 {
		// The transition time comes from the reconciler clock: the outage
		// backoff measures from it. SetStatusCondition keeps the stored
		// time while the condition stays False.
		apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
			Type: kaalmv1beta1.ConditionGatewayReachable, Status: metav1.ConditionFalse,
			Reason: "GatewayUnavailable", Message: "no gateway activity data; idle transitions deferred",
			LastTransitionTime: metav1.NewTime(now),
		})
		return ctrl.Result{RequeueAfter: gatewayOutageRequeue(agent.Status.Conditions, now, outageJitter())}
	}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionGatewayReachable, Status: metav1.ConditionTrue,
		Reason: "GatewayReady", Message: "gateway activity data available",
	})

	last := mergedActivity(reachable, agent.Name, eff.ActivitySource)
	synthetic := false
	if last == nil {
		// No recorded activity: genuine silence only if some replica has
		// been watching for at least idleTimeout; otherwise a restart wiped
		// the store and the data is unknown.
		qualified := false
		for _, replica := range reachable {
			if now.Sub(replica.StartedAt) >= eff.IdleTimeout {
				qualified = true
				break
			}
		}
		if !qualified || agent.Status.PhaseTransitionTime == nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}
		}
		// Fall back to PhaseTransitionTime as the silence marker, but remember
		// it is synthetic: it advances on every setPhase, so it must not be
		// read as fresh activity below (that would oscillate Idle<->Running).
		t := agent.Status.PhaseTransitionTime.Time
		last = &t
		synthetic = true
	}

	switch agent.Status.Phase {
	case kaalmv1beta1.AgentRunning:
		// A wake sets LastActivityTime to the wake time (handleWake). Floor the gateway's
		// record with it, so a woken agent gets a full idleTimeout while the
		// gateway still reports only the activity that preceded its sleep.
		marker := *last
		if agent.Status.LastActivityTime != nil && agent.Status.LastActivityTime.After(marker) {
			marker = agent.Status.LastActivityTime.Time
		}
		if now.Sub(marker) > eff.IdleTimeout {
			r.setPhase(agent, kaalmv1beta1.AgentIdle, fmt.Sprintf("no activity for %s", eff.IdleTimeout))
			lt := metav1.NewTime(marker)
			agent.Status.LastActivityTime = &lt
			// Re-enter promptly: an already-elapsed hibernationDelay should
			// carry straight on to Hibernating on the next pass.
			return ctrl.Result{Requeue: true}
		}
	case kaalmv1beta1.AgentIdle:
		// Only real recorded activity returns an Idle agent to Running. The
		// synthetic fallback marker advances on every transition, so treating
		// it as fresh activity would flip Idle->Running->Idle forever and never
		// let a trafficless agent hibernate.
		//
		// Compare at second resolution: the stored timestamp lost its
		// nanoseconds in the apiserver round-trip (metav1.Time marshals to
		// RFC3339 seconds), and a raw comparison would misread the same
		// instant as fresh activity on every pass.
		if !synthetic && agent.Status.LastActivityTime != nil &&
			last.Truncate(time.Second).After(agent.Status.LastActivityTime.Time) {
			r.setPhase(agent, kaalmv1beta1.AgentRunning, "new activity")
			lt := metav1.NewTime(*last)
			agent.Status.LastActivityTime = &lt
			return ctrl.Result{Requeue: true}
		}
		// Measure the hibernation window from a stable silence marker. The
		// synthetic path uses LastActivityTime (set once at the Idle
		// transition), not the advancing PhaseTransitionTime, so the delay is
		// counted from when silence began rather than reset on every pass.
		// The real path floors the gateway's record with LastActivityTime for
		// the same reason as the Running case: after a wake the record still
		// predates the sleep, and the delay must count from the wake.
		silenceStart := last
		if agent.Status.LastActivityTime != nil &&
			(synthetic || agent.Status.LastActivityTime.After(*silenceStart)) {
			silenceStart = &agent.Status.LastActivityTime.Time
		}
		if eff.HibernationEnabled && !hibernationHeld && now.Sub(*silenceStart) > eff.IdleTimeout+eff.HibernationDelay {
			r.setPhase(agent, kaalmv1beta1.AgentHibernating,
				fmt.Sprintf("idle for %s past the idle timeout", eff.HibernationDelay))
			return ctrl.Result{Requeue: true}
		}
	}
	return ctrl.Result{RequeueAfter: activityCacheWindow}
}

// readyGates evaluates the Ready=False conditions that block Pod creation
// without degrading: a malformed class allowedCIDRs entry (rule 19), a missing
// image, a missing existingClaim, missing imagePullSecrets, an env Secret
// that is missing or lacks the workload label (rule 48), and a missing
// handler ConfigMap (rule 31). It sets the condition on the Agent and reports
// whether the pass is gated; Secrets, PVCs, and handler ConfigMaps are
// unwatched, so gated results carry a requeue interval. Class spec changes are
// watched, so the class gates carry none.
func (r *AgentReconciler) readyGates(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec,
) (bool, ctrl.Result, error) {
	// Rule 19: the class is Ready=False, and the NetworkPolicy built from its
	// entries would fail the apiserver write on every pass.
	if bad := invalidCIDRs(class); len(bad) > 0 {
		r.setReadyGate(agent, kaalmv1beta1.ReasonInvalidReference,
			fmt.Sprintf("AgentClass %q is not usable: %s", class.Name, strings.Join(bad, "; ")))
		return true, ctrl.Result{}, nil
	}
	if eff.Image == "" {
		r.setReadyGate(agent, kaalmv1beta1.ReasonInvalidReference,
			"no image: Agent.spec.image is empty and the AgentClass sets no defaultImage")
		return true, ctrl.Result{}, nil
	}
	if eff.PersistenceOn && eff.ExistingClaim != "" {
		var pvc corev1.PersistentVolumeClaim
		err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: eff.ExistingClaim}, &pvc)
		if apierrors.IsNotFound(err) {
			r.setReadyGate(agent, kaalmv1beta1.ReasonExistingClaimNotFound,
				fmt.Sprintf("existingClaim %q not found in namespace %q", eff.ExistingClaim, agent.Namespace))
			return true, ctrl.Result{RequeueAfter: gateRequeue}, nil
		} else if err != nil {
			return false, ctrl.Result{}, err
		}
	}
	// Rule 23 reads Secrets in the Agent's namespace, where the operator
	// holds no standing read: the scoped Role comes first.
	if err := ensureControllerSecretAccess(ctx, r.Client, r.Scheme(), agent, agentPullSecretRoleName(agent.Name),
		r.OperatorNamespace, eff.ImagePullSecrets); err != nil {
		return false, ctrl.Result{}, err
	}
	for _, ref := range eff.ImagePullSecrets {
		var sec corev1.Secret
		err := getSecretLive(ctx, liveSecretReader(r.SecretReader, r.Client),
			types.NamespacedName{Namespace: agent.Namespace, Name: ref.Name}, &sec)
		if apierrors.IsNotFound(err) {
			r.setReadyGate(agent, kaalmv1beta1.ReasonImagePullSecretMissing,
				fmt.Sprintf("imagePullSecret %q missing in namespace %q", ref.Name, agent.Namespace))
			return true, ctrl.Result{RequeueAfter: gateRequeue}, nil
		} else if err != nil {
			return false, ctrl.Result{}, err
		}
	}
	// Rule 48: every Secret the env reads must opt in to workload use. The
	// read runs under its own scoped Role, kept on every pass so a removed
	// reference drops its grant. A running Pod is left in place; the gate
	// blocks any replacement.
	if err := ensureControllerSecretAccess(ctx, r.Client, r.Scheme(), agent, agentEnvSecretRoleName(agent.Name),
		r.OperatorNamespace, envSecretRefs(eff.Env)); err != nil {
		return false, ctrl.Result{}, err
	}
	reason, msg, err := checkEnvSecrets(ctx, liveSecretReader(r.SecretReader, r.Client), agent.Namespace, eff.Env)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if reason != "" {
		r.setReadyGate(agent, reason, msg)
		return true, ctrl.Result{RequeueAfter: gateRequeue}, nil
	}
	// Rule 31: the handler ConfigMap must exist where the Agent runs. Checked
	// pre-Pod so a bad reference surfaces as a condition, not a Pod wedged in
	// ContainerCreating on a missing volume source. The ConfigMap is
	// developer-owned: no ownerRef is added and content is never tracked.
	if eff.HandlerConfigMap != "" {
		var cm corev1.ConfigMap
		err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: eff.HandlerConfigMap}, &cm)
		if apierrors.IsNotFound(err) {
			r.setReadyGate(agent, kaalmv1beta1.ReasonHandlerConfigMapNotFound,
				fmt.Sprintf("handler ConfigMap %q not found in namespace %q", eff.HandlerConfigMap, agent.Namespace))
			return true, ctrl.Result{RequeueAfter: gateRequeue}, nil
		} else if err != nil {
			return false, ctrl.Result{}, err
		}
	}
	return false, ctrl.Result{}, nil
}

// reconcileDegraded runs the cross-checks and drives the Degraded enter/leave
// protocol. handled=true means the pass ends here (either the agent is
// Degraded, or it just recovered and requeues).
func (r *AgentReconciler) reconcileDegraded(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec,
) (bool, ctrl.Result, error) {
	reasons := r.degradedReasons(ctx, agent, class, eff)
	if len(reasons) > 0 {
		return true, ctrl.Result{}, r.enterOrStayDegraded(ctx, agent, reasons)
	}
	if agent.Status.Phase != kaalmv1beta1.AgentDegraded {
		return false, ctrl.Result{}, nil
	}
	// Every Degraded-triggering condition has cleared: restore the prior
	// phase and null preDegradedPhase atomically in the same write.
	restored := agent.Status.PreDegradedPhase
	if restored == "" {
		restored = kaalmv1beta1.AgentPending
	}
	r.setPhase(agent, restored, "every Degraded check has cleared")
	agent.Status.PreDegradedPhase = ""
	return true, ctrl.Result{Requeue: true}, r.writeStatus(ctx, agent)
}

// degradedReasons evaluates every Degraded-triggering cross-check and returns
// the outstanding reasons in a stable order (first entry becomes the reported
// reason).
func (r *AgentReconciler) degradedReasons(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec,
) []metav1.Condition {
	var out []metav1.Condition
	add := func(reason, msg string) {
		out = append(out, metav1.Condition{Reason: reason, Message: msg})
	}

	// Rule 47: the class admits the Agent's namespace. First, so it is the
	// reported reason when several mismatches exist.
	if !class.AdmitsNamespace(agent.Namespace) {
		add(kaalmv1beta1.ReasonNamespaceNotAllowed,
			fmt.Sprintf("namespace %q is not in AgentClass %q allowedNamespaces", agent.Namespace, class.Name))
	}
	// Rule 2: image allowlist.
	if eff.Image != "" && !imageAllowed(eff.Image, class.Spec.Image.AllowedImages) {
		add(kaalmv1beta1.ReasonClassConstraintViolation,
			fmt.Sprintf("image %q does not match AgentClass %q allowedImages", eff.Image, class.Name))
	}
	// Rules 4, 5: provider resolution, allowlist, and namespace admission.
	for _, p := range agent.Spec.Providers {
		name := p.ProviderRef.Name
		// An empty allowedProviders list allows none (docs/src/resources/agentclass.md).
		allowed := false
		for _, ap := range class.Spec.AllowedProviders {
			if ap.Name == name {
				allowed = true
				break
			}
		}
		if !allowed {
			add(kaalmv1beta1.ReasonClassConstraintViolation,
				fmt.Sprintf("provider %q is not in AgentClass %q allowedProviders", name, class.Name))
			continue
		}
		var mp kaalmv1beta1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				add(kaalmv1beta1.ReasonClassConstraintViolation,
					fmt.Sprintf("provider %q does not exist", name))
			}
			continue
		}
		if !namespaceAllowed(agent.Namespace, mp.Spec.AllowedNamespaces) {
			add(kaalmv1beta1.ReasonClassConstraintViolation,
				fmt.Sprintf("provider %q does not allow namespace %q", name, agent.Namespace))
		}
	}
	// Rules 35 to 38: tool grant resolution, class allowlist, namespace
	// admission, and catalog membership.
	out = append(out, toolGrantViolations(ctx, r.Client, agent.Namespace, agent.Spec.Tools, class)...)
	// Rule 24: persistence must be class-permitted.
	if agent.Spec.Persistence.Enabled && !class.Spec.Persistence.Enabled {
		add(kaalmv1beta1.ReasonPersistenceNotAllowed,
			fmt.Sprintf("persistence requested but AgentClass %q has persistence.enabled=false", class.Name))
	}
	// Rule 26: hibernation must be class-permitted.
	if agent.Spec.Lifecycle.HibernationEnabled && !class.Spec.Lifecycle.HibernationAllowed {
		add(kaalmv1beta1.ReasonHibernationNotAllowed,
			fmt.Sprintf("hibernation requested but AgentClass %q has lifecycle.hibernationAllowed=false", class.Name))
	}
	// Rule 29: hibernation requires persistence (spec-internal).
	if agent.Spec.Lifecycle.HibernationEnabled && !agent.Spec.Persistence.Enabled {
		add(kaalmv1beta1.ReasonHibernationRequiresPersist,
			"lifecycle.hibernationEnabled=true requires spec.persistence.enabled=true")
	}
	// Rule 30: a handler mount must be class-permitted. Drift-sensitive like
	// rules 24 and 26: flipping allowHandlerMounts off on a live class
	// degrades existing handler-mounting Agents.
	if agent.Spec.Handler != nil && !class.Spec.Image.AllowHandlerMounts {
		add(kaalmv1beta1.ReasonHandlerMountNotAllowed,
			fmt.Sprintf("spec.handler set but AgentClass %q has image.allowHandlerMounts=false", class.Name))
	}
	return out
}

// enterOrStayDegraded transitions into Degraded (recording preDegradedPhase on
// first entry only) or refreshes reason/message while already Degraded.
func (r *AgentReconciler) enterOrStayDegraded(
	ctx context.Context, agent *kaalmv1beta1.Agent, reasons []metav1.Condition,
) error {
	first := reasons[0]
	if agent.Status.Phase != kaalmv1beta1.AgentDegraded {
		agent.Status.PreDegradedPhase = agent.Status.Phase
		r.setPhase(agent, kaalmv1beta1.AgentDegraded, first.Reason)
		r.events.add(agent, corev1.EventTypeWarning, first.Reason, first.Message)
	}
	r.setReady(agent, false, first.Reason, first.Message)
	return r.writeStatus(ctx, agent)
}

// reconcileBudgetCondition mirrors provider budget state onto the Agent (S10).
// If any referenced ModelProvider reports the Agent's namespace as budget
// Blocked in status.budgetUsage, the Degraded condition is set (reason
// BudgetExhausted); otherwise it is removed. This never touches status.phase:
// budget exhaustion is a recoverable runtime state, not a lifecycle transition,
// so the agent keeps running and the signal clears on its own when the provider
// reports the namespace unblocked (period reset, budget increase, spend
// drop, or the budget is turned off), driven by the ModelProvider watch. When the condition first appears a
// BudgetExhausted Warning event is emitted, so `kubectl describe agent` shows
// it. Provider Get errors are tolerated (a missing or unreadable provider is
// the degrade path's concern, not this one): the condition reflects what could
// be read.
func (r *AgentReconciler) reconcileBudgetCondition(ctx context.Context, agent *kaalmv1beta1.Agent) {
	var blocking []string
	for _, p := range agent.Spec.Providers {
		var mp kaalmv1beta1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: p.ProviderRef.Name}, &mp); err != nil {
			continue
		}
		for _, u := range mp.Status.BudgetUsage {
			if u.Namespace == agent.Namespace && u.State == kaalmv1beta1.BudgetStateBlocked {
				blocking = append(blocking, mp.Name)
				break
			}
		}
	}
	if len(blocking) == 0 {
		apimeta.RemoveStatusCondition(&agent.Status.Conditions, kaalmv1beta1.ConditionDegraded)
		return
	}
	msg := fmt.Sprintf("namespace %q budget exhausted on provider %s", agent.Namespace, strings.Join(blocking, ", "))
	// The Warning fires on the transition into the condition only: a
	// blocked namespace stays blocked until the period resets, and every
	// pass in between re-reads the same state.
	if prev := apimeta.FindStatusCondition(agent.Status.Conditions, kaalmv1beta1.ConditionDegraded); prev == nil ||
		prev.Status != metav1.ConditionTrue || prev.Reason != kaalmv1beta1.ReasonBudgetExhausted {
		r.events.add(agent, corev1.EventTypeWarning, kaalmv1beta1.ReasonBudgetExhausted, msg)
	}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:               kaalmv1beta1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: agent.Generation,
		Reason:             kaalmv1beta1.ReasonBudgetExhausted,
		Message:            msg,
	})
}

// reconcileProvidersCondition sets ProvidersReady. It is True with
// AllProvidersHealthy when every provider in spec.providers is in the class
// allowlist, exists, and reports Ready=True. Otherwise it is False with the
// reason of the first problem in spec order: ClassConstraintViolation for a
// provider outside the allowlist or missing (the reason Ready carries for the
// same problem), ProviderUnhealthy for one that is not Ready. Provider Get
// errors other than NotFound are skipped, as in reconcileBudgetCondition.
func (r *AgentReconciler) reconcileProvidersCondition(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass,
) {
	var reason string
	var problems []string
	add := func(rsn, msg string) {
		if reason == "" {
			reason = rsn
		}
		problems = append(problems, msg)
	}
	for _, p := range agent.Spec.Providers {
		name := p.ProviderRef.Name
		allowed := false
		for _, ap := range class.Spec.AllowedProviders {
			if ap.Name == name {
				allowed = true
				break
			}
		}
		if !allowed {
			add(kaalmv1beta1.ReasonClassConstraintViolation,
				fmt.Sprintf("provider %q is not in AgentClass %q allowedProviders", name, class.Name))
			continue
		}
		var mp kaalmv1beta1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				add(kaalmv1beta1.ReasonClassConstraintViolation, fmt.Sprintf("provider %q does not exist", name))
			}
			continue
		}
		ready := apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionReady)
		switch {
		case ready == nil:
			add(kaalmv1beta1.ReasonProviderUnhealthy, fmt.Sprintf("provider %q reports no Ready condition yet", name))
		case ready.Status != metav1.ConditionTrue:
			add(kaalmv1beta1.ReasonProviderUnhealthy,
				fmt.Sprintf("provider %q is not Ready (%s)", name, ready.Reason))
		}
	}
	cond := metav1.Condition{
		Type:               kaalmv1beta1.ConditionProvidersReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: agent.Generation,
		Reason:             kaalmv1beta1.ReasonAllProvidersHealthy,
		Message:            "every referenced provider is Ready",
	}
	if reason != "" {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reason
		cond.Message = strings.Join(problems, "; ")
	}
	apimeta.SetStatusCondition(&agent.Status.Conditions, cond)
}

// ensureCertificate creates the Agent's Certificate when it is missing and
// reports whether it is Ready. Once the Certificate exists and the Agent
// controls it, it also returns the Secret name the Certificate writes
// (spec.secretName), which the Pod mounts. An existing Certificate is never
// updated, so it keeps the Secret name it was created with.
func (r *AgentReconciler) ensureCertificate(ctx context.Context, agent *kaalmv1beta1.Agent) (string, bool, error) {
	var cert cmapi.Certificate
	key := types.NamespacedName{Namespace: agent.Namespace, Name: agentCertificateName(agent.Name)}
	if err := r.Get(ctx, key, &cert); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", false, err
		}
		desired := desiredCertificate(agent, r.CertLifetime)
		if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
			return "", false, err
		}
		return "", false, createControlled(ctx, r.Client, agent, desired)
	}
	if err := requireControlled(r.Scheme(), agent, &cert); err != nil {
		return "", false, err
	}
	return cert.Spec.SecretName, certificateReady(&cert), nil
}

// certificateReady reports whether the Certificate's Ready condition is True.
func certificateReady(cert *cmapi.Certificate) bool {
	for _, c := range cert.Status.Conditions {
		if c.Type == cmapi.CertificateConditionReady && c.Status == cmmeta.ConditionTrue {
			return true
		}
	}
	return false
}

// ensureChildren converges the ServiceAccount, Service, PVC, NetworkPolicy,
// and FQDN policy (everything except the Certificate and the Pod).
func (r *AgentReconciler) ensureChildren(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec,
) error {
	if err := r.ensureServiceAccount(ctx, agent); err != nil {
		return err
	}
	if eff.ServiceEnabled {
		if err := r.ensureService(ctx, agent, eff); err != nil {
			return err
		}
	}
	if eff.PersistenceOn && eff.ExistingClaim == "" {
		if err := r.ensurePVC(ctx, agent, class, eff); err != nil {
			return err
		}
	}
	if err := r.ensureNetworkPolicy(ctx, agent, class, eff); err != nil {
		return err
	}
	hosts := class.Spec.Network.Egress.AllowedHosts
	supported, err := fqdnSupported(r.FQDNSupport)
	if err != nil {
		// With no hosts to enforce, a discovery failure must not block the
		// pass; the probe is retried once a class lists hosts.
		if len(hosts) > 0 {
			return err
		}
		supported = false
	}
	return ensureFQDNPolicy(ctx, r.Client, r.Scheme(), agent, agentPodLabels(agent), hosts, r.DNS, supported)
}

func (r *AgentReconciler) ensureServiceAccount(ctx context.Context, agent *kaalmv1beta1.Agent) error {
	desired := desiredServiceAccount(agent)
	if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
		return err
	}
	// Read from the informer before writing: a create that is expected to
	// fail AlreadyExists is still a POST the apiserver has to reject, once
	// per agent per pass (#174).
	var current corev1.ServiceAccount
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &current)
	if err == nil {
		return requireControlled(r.Scheme(), agent, &current)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return createControlled(ctx, r.Client, agent, desired)
}

func (r *AgentReconciler) ensureService(ctx context.Context, agent *kaalmv1beta1.Agent, eff effectiveAgentSpec) error {
	desired := desiredService(agent, eff)
	if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
		return err
	}
	var current corev1.Service
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &current); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := createControlled(ctx, r.Client, agent, desired); err != nil {
			return err
		}
	} else if err := requireControlled(r.Scheme(), agent, &current); err != nil {
		return err
	} else if len(current.Spec.Ports) != 1 ||
		current.Spec.Ports[0].Port != desired.Spec.Ports[0].Port ||
		current.Spec.Ports[0].TargetPort != desired.Spec.Ports[0].TargetPort {
		current.Spec.Ports = desired.Spec.Ports
		if err := r.Update(ctx, &current); err != nil {
			return rejectedWrite("updating", r.Scheme(), &current, err)
		}
	}
	agent.Status.Endpoint = fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", agent.Name, agent.Namespace, eff.ServicePort)
	return nil
}

func (r *AgentReconciler) ensurePVC(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec,
) error {
	desired := desiredPVC(agent, class, eff)
	if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
		return err
	}
	var current corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &current); err == nil {
		// A PVC with no controller is memory a deleted Agent of the same name
		// left under pvcRetention: Retain, and it is reused as it was left.
		// Mounting it grants nothing existingClaim does not; a PVC another
		// object controls is still a conflict.
		if metav1.GetControllerOf(&current) == nil {
			return nil
		}
		return requireControlled(r.Scheme(), agent, &current)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if err := createControlled(ctx, r.Client, agent, desired); err != nil {
		return err
	}
	agent.Status.PVCName = desired.Name
	return nil
}

func (r *AgentReconciler) ensureNetworkPolicy(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec,
) error {
	desired := desiredNetworkPolicy(agent, class, eff, r.OperatorNamespace, r.DNS)
	if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
		return err
	}
	var current networkingv1.NetworkPolicy
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &current); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		return createControlled(ctx, r.Client, agent, desired)
	}
	if err := requireControlled(r.Scheme(), agent, &current); err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(current.Spec, desired.Spec) {
		return nil
	}
	current.Spec = desired.Spec
	return rejectedWrite("updating", r.Scheme(), &current, r.Update(ctx, &current))
}

// convergePod implements Pod convergence: create when missing, replace when
// terminal (involuntary disruption) or when the spec hash drifts and the
// class has a drift slot free, mark the Agent Failed on a persistent crash
// loop, and derive Running from readiness. Until the Pod is Ready, a woken
// Agent stays Resuming and any other Agent is Provisioning (podPendingPhase).
// A Pod create the API server rejects (isWriteRejection) sets that phase
// with Ready=False PodCreateRejected and clears podName; any other creation
// error returns without a phase change, so the pass retries with backoff. It
// reports whether the Agent has drifted and waits for a slot, and whether the
// create was rejected, so the caller can schedule a retry. A Pod it creates
// mounts tlsSecret, the Secret the Agent's Certificate names. Drift is a spec
// hash mismatch or a TLS volume that names a Secret other than tlsSecret; both
// kinds share the maxUnavailableOnDrift slots. The Secret name stays out of
// the hash, so a Certificate that kept its pre-v1.1.0 name replaces no Pod on
// upgrade. While the Certificate is not Ready (certHeld), it reads the Pod as
// usual but never creates or replaces one, takes no drift slot, and reports
// Ready=False CertificateNotReady.
func (r *AgentReconciler) convergePod(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, eff effectiveAgentSpec,
	tlsSecret string, certHeld bool,
) (driftWaiting, createRejected bool, err error) {
	pod, err := r.ownedPod(ctx, agent)
	if err != nil {
		return false, false, err
	}
	ready := func(ok bool, reason, msg string) { r.setPodReady(agent, certHeld, ok, reason, msg) }

	if pod == nil {
		if certHeld {
			// The Pod went away after Reconcile's read.
			r.waitForCertificateWithoutPod(agent)
			return false, false, nil
		}
		desired := desiredPod(agent, eff, r.OperatorNamespace, tlsSecret)
		if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
			return false, false, err
		}
		if err := r.Create(ctx, desired); err != nil {
			if !isWriteRejection(err) {
				return false, false, err
			}
			// A rejection holds like a gate: the cause is fixed outside the
			// Agent, and the caller's timed requeue notices the fix. The
			// drift slot (PodUpToDate=Replacing) is kept.
			r.setPhase(agent, podPendingPhase(agent), "Pod create rejected")
			r.setReadyGate(agent, kaalmv1beta1.ReasonPodCreateRejected, err.Error())
			agent.Status.PodName = ""
			return false, true, nil
		}
		r.setPhase(agent, podPendingPhase(agent), "Pod created")
		r.setReady(agent, false, "PodProvisioning", "agent Pod created, waiting for readiness")
		agent.Status.PodName = desired.Name
		return false, false, nil
	}

	// A Pod already being deleted is a replacement in progress: wait for the
	// owned-Pod watch to fire when it is gone.
	if !pod.DeletionTimestamp.IsZero() {
		r.setPhase(agent, podPendingPhase(agent), "previous Pod terminating")
		ready(false, "PodProvisioning", "previous Pod terminating")
		return false, false, nil
	}

	// Involuntary disruption: a terminal Pod is never resurrected by the
	// kubelet under restartPolicy Always, so delete it and re-provision.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		r.Recorder.Event(agent, corev1.EventTypeWarning, "PodDisrupted",
			fmt.Sprintf("Pod %s is terminal (%s); re-provisioning", pod.Name, pod.Status.Phase))
		r.setPhase(agent, podPendingPhase(agent), "replacing a terminal Pod")
		ready(false, "PodDisrupted", "replacing a terminal Pod")
		return false, false, r.Delete(ctx, pod)
	}

	// A Pod hashed by an older formula that is current under that formula
	// is not drift: rewrite its hash annotations in place so an upgrade that changes
	// the formula replaces no Pod.
	if err := r.rewriteLegacyHash(ctx, pod, eff); err != nil {
		return false, false, err
	}

	// Drift: compare the hash in the Pod's annotation against the re-derived
	// one, never the live Pod object, then the Secret the Pod's TLS volume
	// names against the one the Certificate names. Both read the Pod already
	// listed, so neither costs an API call. A Pod mounting another Secret
	// stops receiving renewals, so it is replaced like spec drift. The check
	// runs before the crash-loop check, so a change replaces a crash-looping
	// Pod; that is how a rollout halted by a failed replacement recovers.
	waiting := false
	cause, mounted := podDriftCause(pod, eff, tlsSecret)
	if cause != "" && !certHeld {
		replace, err := r.admitDriftReplacement(ctx, agent, class, cause)
		if err != nil {
			return false, false, err
		}
		if replace {
			r.Recorder.Event(agent, corev1.EventTypeNormal, "SpecDrift", cause+"; replacing the Pod")
			return false, false, r.Delete(ctx, pod)
		}
		waiting = true
	} else if cause != "" {
		// The replacement waits for the Certificate without a drift slot. A
		// Replacing Agent whose Pod is not Ready keeps its slot, which counts
		// that unavailability.
		if podUpToDateReason(agent) != kaalmv1beta1.ReasonReplacing || podReady(pod) {
			r.setPodUpToDate(agent, metav1.ConditionFalse, kaalmv1beta1.ReasonCertificateNotReady,
				cause+"; the replacement waits for the agent certificate, and the current Pod keeps running")
		}
	}

	// Persistent crash loop marks the Agent Failed (any -> Failed).
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil &&
			(cs.State.Waiting.Reason == "CrashLoopBackOff" && cs.RestartCount >= crashLoopThreshold ||
				cs.State.Waiting.Reason == "ImagePullBackOff") {
			r.setPhase(agent, kaalmv1beta1.AgentFailed,
				fmt.Sprintf("container %s: %s", cs.Name, cs.State.Waiting.Reason))
			ready(false, cs.State.Waiting.Reason,
				fmt.Sprintf("container %s: %s", cs.Name, cs.State.Waiting.Message))
			return waiting, false, nil
		}
	}

	agent.Status.PodName = pod.Name
	if podReady(pod) {
		// Idle is a pod-bearing phase: a ready Pod does not promote an Idle
		// agent back to Running; only fresh activity does.
		if agent.Status.Phase != kaalmv1beta1.AgentIdle {
			r.setPhase(agent, kaalmv1beta1.AgentRunning, "Pod is Ready")
		}
		ready(true, kaalmv1beta1.ReasonPodRunning, "agent Pod is ready")
		if cause == "" {
			// A Ready Pod on the current spec that mounts the Certificate's
			// Secret frees the drift slot. The message names the Secret only
			// when the TLS check compared one.
			msg := "agent Pod matches the derived spec"
			if mounted != "" && mounted == tlsSecret {
				msg += " and mounts the Certificate's TLS Secret " + tlsSecret
			}
			r.setPodUpToDate(agent, metav1.ConditionTrue, kaalmv1beta1.ReasonPodCurrent, msg)
		}
	} else {
		r.setPhase(agent, podPendingPhase(agent), "Pod is not Ready")
		ready(false, "PodNotReady", "agent Pod is not ready")
	}
	return waiting, false, nil
}

// podDriftCause says why pod no longer matches the Agent, or "" when it does,
// and returns the Secret the Pod's TLS volume names. The spec hash is checked
// first; the TLS Secret counts only when the Pod has a TLS volume and the
// Certificate names a Secret.
func podDriftCause(pod *corev1.Pod, eff effectiveAgentSpec, tlsSecret string) (cause, mounted string) {
	mounted = tlsSecretOf(pod)
	switch {
	case pod.Annotations[annotationPodSpecHash] != podSpecHash(eff):
		return "derived Pod spec changed", mounted
	case mounted != "" && tlsSecret != "" && mounted != tlsSecret:
		return fmt.Sprintf("the Agent's Certificate names TLS Secret %s and the Pod mounts %s", tlsSecret, mounted), mounted
	}
	return "", mounted
}

// withDriftRetry makes sure an Agent waiting for a drift slot is retried
// within driftPendingRequeue. The watch that fires when another Agent of the
// class leaves Replacing is the normal trigger; this retry is the fallback.
func withDriftRetry(res ctrl.Result) ctrl.Result {
	if !res.Requeue && (res.RequeueAfter == 0 || res.RequeueAfter > driftPendingRequeue) {
		res.RequeueAfter = driftPendingRequeue
	}
	return res
}

// admitDriftReplacement decides whether a drifted Agent replaces its Pod on
// this pass. An Agent that already holds a drift slot (Replacing) keeps it,
// so a failed replacement is fixed at once; any other Agent asks the class
// for a slot (maxUnavailableOnDrift). On a grant it sets Replacing and
// persists the status before the caller deletes the Pod, so the slot counts
// even if the controller stops right after the delete. On a refusal it sets
// ReplacementPending and leaves the Pod running. The caller passes the drift
// cause, which names what changed in every event and condition message.
func (r *AgentReconciler) admitDriftReplacement(
	ctx context.Context, agent *kaalmv1beta1.Agent, class *kaalmv1beta1.AgentClass, cause string,
) (bool, error) {
	if podUpToDateReason(agent) != kaalmv1beta1.ReasonReplacing {
		granted, err := r.driftSlots.acquire(ctx, r.Client, agent, class, r.now())
		if err != nil {
			return false, err
		}
		if !granted {
			if podUpToDateReason(agent) != kaalmv1beta1.ReasonReplacementPending {
				r.events.add(agent, corev1.EventTypeNormal, kaalmv1beta1.ReasonSpecDriftPending,
					cause+"; waiting for a free maxUnavailableOnDrift slot")
			}
			r.setPodUpToDate(agent, metav1.ConditionFalse, kaalmv1beta1.ReasonReplacementPending,
				cause+"; waiting for a free maxUnavailableOnDrift slot, the current Pod keeps running")
			return false, nil
		}
	}
	msg := cause + "; replacing the Pod"
	r.setPodUpToDate(agent, metav1.ConditionFalse, kaalmv1beta1.ReasonReplacing, msg)
	r.setPhase(agent, podPendingPhase(agent), msg)
	r.setReady(agent, false, "SpecDrift", msg)
	if err := r.writeStatus(ctx, agent); err != nil {
		return false, err
	}
	return true, nil
}

// setPodReady writes the Ready condition convergePod derives from the Pod.
// While the Certificate is held it is always False CertificateNotReady, so
// Ready never flips within a pass, and ok only picks the message.
func (r *AgentReconciler) setPodReady(agent *kaalmv1beta1.Agent, certHeld, ok bool, reason, msg string) {
	if !certHeld {
		r.setReady(agent, ok, reason, msg)
		return
	}
	held := certWaitMsg
	if ok {
		held = certWaitServingMsg
	}
	r.setReady(agent, false, kaalmv1beta1.ReasonCertificateNotReady, held)
}

func (r *AgentReconciler) setPodUpToDate(agent *kaalmv1beta1.Agent, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionPodUpToDate, Status: status, Reason: reason, Message: msg,
	})
}

// podPendingPhase is the phase an Agent holds while it waits for a Pod to be
// created, replaced, or become Ready. A woken Agent stays Resuming until its
// Pod is Ready; every other Agent is Provisioning.
func podPendingPhase(agent *kaalmv1beta1.Agent) kaalmv1beta1.AgentPhase {
	if agent.Status.Phase == kaalmv1beta1.AgentResuming {
		return kaalmv1beta1.AgentResuming
	}
	return kaalmv1beta1.AgentProvisioning
}

// rewriteLegacyHash rewrites the hash annotations of a Pod whose hash came
// from formula 1 (no hash-version annotation) and still matches that
// formula: it merge-patches only the two annotations, so the Pod keeps
// running. A formula-1 Pod whose
// hash does not match is real drift and is left for the caller to replace.
func (r *AgentReconciler) rewriteLegacyHash(ctx context.Context, pod *corev1.Pod, eff effectiveAgentSpec) error {
	if _, ok := pod.Annotations[annotationPodSpecHashVersion]; ok {
		return nil
	}
	if pod.Annotations[annotationPodSpecHash] != podSpecHashV1(eff) {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	pod.Annotations[annotationPodSpecHash] = podSpecHash(eff)
	pod.Annotations[annotationPodSpecHashVersion] = podSpecHashVersion
	if err := r.Patch(ctx, pod, patch); err != nil {
		return err
	}
	log.FromContext(ctx).Info("rewrote Pod spec hash for the current formula; Pod kept",
		"pod", pod.Name, "hashVersion", podSpecHashVersion)
	return nil
}

// ownedPod returns the Agent's live Pod, preferring a non-terminating one when
// a replacement overlaps a termination.
func (r *AgentReconciler) ownedPod(ctx context.Context, agent *kaalmv1beta1.Agent) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(agent.Namespace),
		client.MatchingLabels(agentPodLabels(agent))); err != nil {
		return nil, err
	}
	var candidate *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if !metav1.IsControlledBy(p, agent) {
			continue
		}
		if p.DeletionTimestamp.IsZero() {
			return p, nil
		}
		candidate = p
	}
	return candidate, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// reconcileDelete implements the agent finalizer: gracefully terminate the Pod
// if one exists, apply pvcRetention by rewriting the PVC's ownerRef, then
// release the finalizer. See docs/src/controller/finalizers.md (Agent).
func (r *AgentReconciler) reconcileDelete(ctx context.Context, agent *kaalmv1beta1.Agent) error {
	if !controllerutil.ContainsFinalizer(agent, kaalmv1beta1.AgentFinalizer) {
		return nil
	}

	if agent.Status.Phase != kaalmv1beta1.AgentTerminating {
		agent.Status.PreDegradedPhase = ""
		r.setPhase(agent, kaalmv1beta1.AgentTerminating, "the Agent is being deleted")
		if err := r.writeStatus(ctx, agent); err != nil {
			return err
		}
	}

	// Terminate the Pod gracefully and wait for it to go away; the owned-Pod
	// watch re-enqueues us when it does.
	pod, err := r.ownedPod(ctx, agent)
	if err != nil {
		return err
	}
	if pod != nil {
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		return nil
	}

	// pvcRetention: Retain strips the PVC's ownerRef before the finalizer is
	// removed, so cascade GC finds no owner and leaves the PVC in place. The
	// ownerRef edit must land before the finalizer removal: once the finalizer
	// entry is gone the Agent can vanish at any moment. existingClaim PVCs
	// never carried an ownerRef and are untouched.
	var class kaalmv1beta1.AgentClass
	retention := "Delete"
	if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.AgentClassRef.Name}, &class); err == nil {
		if class.Spec.Persistence.PVCRetention != "" {
			retention = class.Spec.Persistence.PVCRetention
		}
	}
	if retention == "Retain" {
		var pvc corev1.PersistentVolumeClaim
		key := types.NamespacedName{Namespace: agent.Namespace, Name: agentPVCName(agent.Name)}
		if err := r.Get(ctx, key, &pvc); err == nil {
			var kept []metav1.OwnerReference
			for _, ref := range pvc.OwnerReferences {
				if ref.UID != agent.UID {
					kept = append(kept, ref)
				}
			}
			if len(kept) != len(pvc.OwnerReferences) {
				pvc.OwnerReferences = kept
				if err := r.Update(ctx, &pvc); err != nil {
					return err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}

	controllerutil.RemoveFinalizer(agent, kaalmv1beta1.AgentFinalizer)
	return r.Update(ctx, agent)
}

// setPhase is the only writer of status.phase and phaseTransitionTime, and
// so the one place PhaseChanged is emitted: a Normal event naming the old and
// new phase, with why appended when it is set. Setting the current phase is
// not a transition, and neither is an Agent's first phase (Pending, from
// none), so neither emits. The event is held until the status write that
// persists the new phase succeeds (writeStatus), so a write lost to a
// conflict does not report a transition that did not happen, and the retry
// does not report it twice.
func (r *AgentReconciler) setPhase(agent *kaalmv1beta1.Agent, phase kaalmv1beta1.AgentPhase, why string) {
	old := agent.Status.Phase
	if old == phase {
		return
	}
	agent.Status.Phase = phase
	now := metav1.Now()
	agent.Status.PhaseTransitionTime = &now
	if old == "" {
		return
	}
	msg := fmt.Sprintf("phase changed from %s to %s", old, phase)
	if why != "" {
		msg += ": " + why
	}
	r.events.add(agent, corev1.EventTypeNormal, kaalmv1beta1.ReasonPhaseChanged, msg)
}

// setReadyGate sets Ready=False for a reconcile-time validation failure and
// emits a Warning event with the same reason and message when the reason
// first appears, once the status write succeeds. Gates on unwatched objects
// requeue every gateRequeue, so an event per pass would repeat for as long as
// the problem lasts.
func (r *AgentReconciler) setReadyGate(agent *kaalmv1beta1.Agent, reason, msg string) {
	if readyFalseIsNew(agent.Status.Conditions, reason) {
		r.events.add(agent, corev1.EventTypeWarning, reason, msg)
	}
	r.setReady(agent, false, reason, msg)
}

// Wake triggers, the kaalm_wakes_total trigger label values.
const (
	wakeTriggerChannel    = "channel"    // the activator, on a channel message
	wakeTriggerAnnotation = "annotation" // anyone else who set kaalm.io/wake=true
)

// wakeTrigger reports whether the Agent carries a wake request
// (kaalm.io/wake=true) and who made it. The activator writes
// kaalm.io/wake-trigger=channel in the same patch as the wake; with no
// trigger annotation the wake is manual (kubectl annotate). The wake value
// stays "true" for every trigger, so a controller that predates the trigger
// annotation still honors an activator wake during a rollout.
func wakeTrigger(agent *kaalmv1beta1.Agent) (string, bool) {
	if agent.Annotations[kaalmv1beta1.AnnotationWake] != kaalmv1beta1.AnnotationTrue {
		return "", false
	}
	if agent.Annotations[kaalmv1beta1.AnnotationWakeTrigger] == kaalmv1beta1.AnnotationWakeTriggerChannel {
		return wakeTriggerChannel, true
	}
	return wakeTriggerAnnotation, true
}

// clearWake removes the wake request and its trigger together, so a trigger
// never outlives the wake it labeled.
func clearWake(agent *kaalmv1beta1.Agent) {
	delete(agent.Annotations, kaalmv1beta1.AnnotationWake)
	delete(agent.Annotations, kaalmv1beta1.AnnotationWakeTrigger)
}

func (r *AgentReconciler) setReady(agent *kaalmv1beta1.Agent, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionReady, Status: status, Reason: reason, Message: msg,
	})
}

// SetupWithManager wires the reconciler, its owned children, and the
// platform-level map-func watches.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&kaalmv1beta1.Agent{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&cmapi.Certificate{}).
		// The platform-level watches fan out to every Agent that references
		// the changed object, so they must fire only for changes an Agent
		// consumes: the spec of a class or tool provider (their status is
		// bookkeeping the Agent never reads), and for a model provider the
		// spec or the set of namespaces its budget blocks. Without the
		// predicates every in-use count the class reconciler wrote and every
		// ten-second budget publish re-enqueued the whole fleet (#174).
		Watches(&kaalmv1beta1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(r.agentsForClass),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&kaalmv1beta1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.agentsForProvider),
			builder.WithPredicates(providerChangeMatters())).
		Watches(&kaalmv1beta1.ToolProvider{}, handler.EnqueueRequestsFromMapFunc(r.agentsForToolProvider),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// A freed drift slot re-queues the class's Agents that wait for one.
		Watches(&kaalmv1beta1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.driftWaitersOfClass),
			builder.WithPredicates(driftSlotFreed())).
		// A gateway Pod turning Ready re-queues the Agents waiting on the
		// gateway, so the outage backoff does not delay recovery. The Pod
		// informer is already cluster-wide for the owned Pods.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.agentsWaitingOnGateway),
			builder.WithPredicates(gatewayTurnedReady(r.OperatorNamespace))).
		Complete(r)
}

// driftSlotFreed admits an Agent event that frees a drift slot: the Agent's
// PodUpToDate condition leaves Replacing, or a Replacing Agent is deleted.
func driftSlotFreed() predicate.Predicate {
	replacing := func(o client.Object) bool {
		ag, ok := o.(*kaalmv1beta1.Agent)
		return ok && podUpToDateReason(ag) == kaalmv1beta1.ReasonReplacing
	}
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return replacing(e.ObjectOld) && !replacing(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool { return replacing(e.Object) },
	}
}

// driftWaitersOfClass maps an Agent to the Agents of its class that wait for
// a drift slot (ReplacementPending).
func (r *AgentReconciler) driftWaitersOfClass(ctx context.Context, obj client.Object) []reconcile.Request {
	ag, ok := obj.(*kaalmv1beta1.Agent)
	if !ok {
		return nil
	}
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexAgentClassRef: ag.Spec.AgentClassRef.Name}); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range agents.Items {
		a := &agents.Items[i]
		if podUpToDateReason(a) == kaalmv1beta1.ReasonReplacementPending {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		}
	}
	return reqs
}

// providerChangeMatters admits a ModelProvider update to the Agent fan-out
// when its spec changed, when the set of namespaces in the Blocked budget
// state changed, or when its Ready status or reason changed, which is all an
// Agent reads from a provider's status. Spend counters and the Healthy
// condition change on their own cadence and affect no Agent.
func providerChangeMatters() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMP, ok1 := e.ObjectOld.(*kaalmv1beta1.ModelProvider)
			newMP, ok2 := e.ObjectNew.(*kaalmv1beta1.ModelProvider)
			if !ok1 || !ok2 {
				return true
			}
			if oldMP.Generation != newMP.Generation || readyState(oldMP) != readyState(newMP) {
				return true
			}
			return !equality.Semantic.DeepEqual(blockedNamespaces(oldMP), blockedNamespaces(newMP))
		},
	}
}

// readyState is a provider's Ready status and reason, the part of the
// condition ProvidersReady reads.
func readyState(mp *kaalmv1beta1.ModelProvider) string {
	c := apimeta.FindStatusCondition(mp.Status.Conditions, kaalmv1beta1.ConditionReady)
	if c == nil {
		return ""
	}
	return string(c.Status) + "/" + c.Reason
}

// blockedNamespaces returns the namespaces a provider's status reports as
// budget-blocked, sorted, so two statuses compare by content.
func blockedNamespaces(mp *kaalmv1beta1.ModelProvider) []string {
	var out []string
	for _, u := range mp.Status.BudgetUsage {
		if u.State == kaalmv1beta1.BudgetStateBlocked {
			out = append(out, u.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

func (r *AgentReconciler) agentsForClass(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexAgentClassRef: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(agents.Items))
	for _, a := range agents.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return reqs
}

func (r *AgentReconciler) agentsForProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexProviderRef: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(agents.Items))
	for _, a := range agents.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return reqs
}

func (r *AgentReconciler) agentsForToolProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents kaalmv1beta1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexToolProviderRef: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(agents.Items))
	for _, a := range agents.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return reqs
}

// updateStatusIfChanged writes the Agent's status only when a pass changed
// it: every Agent reconciles on its periodic requeue and on every event
// from its children, and a status write per pass was one of the three
// per-agent writes a settled fleet paid for every pass (#174).
func (r *AgentReconciler) updateStatusIfChanged(
	ctx context.Context, agent *kaalmv1beta1.Agent, before *kaalmv1beta1.AgentStatus,
) error {
	if equality.Semantic.DeepEqual(before, &agent.Status) {
		return nil
	}
	return r.writeStatus(ctx, agent)
}

// writeStatus writes the Agent's status and, when the write succeeds, emits
// the events the pass held for it (phase transitions, gate warnings, entry to
// Degraded, ChildConflict, Hibernated, SpecDriftPending, and budget
// exhaustion). A failed write drops them.
func (r *AgentReconciler) writeStatus(ctx context.Context, agent *kaalmv1beta1.Agent) error {
	err := r.Status().Update(ctx, agent)
	r.events.flush(r.Recorder, agent, err == nil)
	return err
}
