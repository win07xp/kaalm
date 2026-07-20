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
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
)

const (
	// certWaitRequeue is the backoff while waiting for cert-manager to issue
	// the per-Agent Certificate (first issuance typically takes seconds).
	certWaitRequeue = 5 * time.Second
	// crashLoopThreshold is the restart count at which a CrashLoopBackOff
	// container marks the Agent Failed.
	crashLoopThreshold = 5
)

// gateRequeue is the retry interval for Ready=False gates that depend on
// unwatched resources (imagePullSecrets and existingClaim PVCs): without it a
// Secret created after the gate fired would never be observed. A variable so
// tests can shorten it.
var gateRequeue = 30 * time.Second

// AgentReconciler owns the full child-resource tree for a persistent agent:
// Certificate, ServiceAccount, Service, PVC, NetworkPolicy, and the Pod, with
// Pod creation gated on certificate readiness. It drives the Pending ->
// Provisioning -> Running path of the Agent state machine plus Degraded,
// Failed, and Terminating. The Idle/Hibernation cycle, activity fan-out, and
// wake handling are gateway-coupled and land in a later phase. See
// docs/src/controller/reconcilers.md (AgentReconciler).
type AgentReconciler struct {
	client.Client
	Recorder record.EventRecorder
	// OperatorNamespace hosts the gateway and controller (agentry-system).
	// Agents in this namespace are rejected to protect SAN integrity.
	OperatorNamespace string
}

// +kubebuilder:rbac:groups=agentry.io,resources=agents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentry.io,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentry.io,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// Reconcile runs one pass of the Agent state machine.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var agent agentryv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !agent.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &agent)
	}

	if controllerutil.AddFinalizer(&agent, agentryv1alpha1.AgentFinalizer) {
		if err := r.Update(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	agent.Status.ObservedGeneration = agent.Generation
	if agent.Status.Phase == "" {
		r.setPhase(&agent, agentryv1alpha1.AgentPending)
	}

	// Step 1: the system namespace is forbidden (SAN-integrity guard).
	if agent.Namespace == r.OperatorNamespace {
		r.setReady(&agent, false, agentryv1alpha1.ReasonSystemNamespaceForbidden,
			fmt.Sprintf("Agents may not run in the operator namespace %q", r.OperatorNamespace))
		return ctrl.Result{}, r.Status().Update(ctx, &agent)
	}

	// Step 1 continued: resolve the AgentClass.
	var class agentryv1alpha1.AgentClass
	if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.AgentClassRef.Name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			r.setReady(&agent, false, agentryv1alpha1.ReasonInvalidReference,
				fmt.Sprintf("AgentClass %q does not exist", agent.Spec.AgentClassRef.Name))
			return ctrl.Result{}, r.Status().Update(ctx, &agent)
		}
		return ctrl.Result{}, err
	}

	eff := deriveEffectiveSpec(&agent, &class)

	// Steps 2 and 5: the Degraded-triggering cross-checks (rules 2, 4, 5, 24,
	// 26, 29). All outstanding reasons are evaluated together so recovery can
	// be per-condition.
	degradedReasons := r.degradedReasons(ctx, &agent, &class, eff)
	if len(degradedReasons) > 0 {
		return ctrl.Result{}, r.enterOrStayDegraded(ctx, &agent, degradedReasons)
	}
	if agent.Status.Phase == agentryv1alpha1.AgentDegraded {
		// Every Degraded-triggering condition has cleared: restore the prior
		// phase and null preDegradedPhase atomically in the same write.
		restored := agent.Status.PreDegradedPhase
		if restored == "" {
			restored = agentryv1alpha1.AgentPending
		}
		r.setPhase(&agent, restored)
		agent.Status.PreDegradedPhase = ""
		r.Recorder.Event(&agent, corev1.EventTypeNormal, agentryv1alpha1.ReasonPhaseChanged,
			fmt.Sprintf("recovered from Degraded to %s", restored))
		return ctrl.Result{Requeue: true}, r.Status().Update(ctx, &agent)
	}

	// Step 5: Ready=False gates that block Pod creation without degrading.
	gated, gateResult, err := r.readyGates(ctx, &agent, eff)
	if err != nil {
		return ctrl.Result{}, err
	}
	if gated {
		if err := r.Status().Update(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
		return gateResult, nil
	}

	// Step 4: ensure the Certificate and gate Pod creation on its readiness.
	certReady, err := r.ensureCertificate(ctx, &agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !certReady {
		if agent.Status.Phase == agentryv1alpha1.AgentPending {
			r.setPhase(&agent, agentryv1alpha1.AgentProvisioning)
		}
		r.setReady(&agent, false, "CertificateNotReady", "waiting for cert-manager to issue the agent certificate")
		if err := r.Status().Update(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: certWaitRequeue}, nil
	}

	// Step 6: converge the non-Pod children.
	if err := r.ensureServiceAccount(ctx, &agent); err != nil {
		return ctrl.Result{}, err
	}
	if eff.ServiceEnabled {
		if err := r.ensureService(ctx, &agent, eff); err != nil {
			return ctrl.Result{}, err
		}
	}
	if eff.PersistenceOn && eff.ExistingClaim == "" {
		if err := r.ensurePVC(ctx, &agent, &class, eff); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.ensureNetworkPolicy(ctx, &agent, &class, eff); err != nil {
		return ctrl.Result{}, err
	}

	// Steps 3, 6, 7: converge the Pod and derive the phase from it.
	if err := r.convergePod(ctx, &agent, eff); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Status().Update(ctx, &agent); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("reconciled Agent", "phase", agent.Status.Phase)
	return ctrl.Result{}, nil
}

// readyGates evaluates the Ready=False conditions that block Pod creation
// without degrading: a missing image, a missing existingClaim, and missing
// imagePullSecrets. It sets the condition on the Agent and reports whether the
// pass is gated; Secrets and PVCs are unwatched, so gated results carry a
// requeue interval.
func (r *AgentReconciler) readyGates(
	ctx context.Context, agent *agentryv1alpha1.Agent, eff effectiveAgentSpec,
) (bool, ctrl.Result, error) {
	if eff.Image == "" {
		r.setReady(agent, false, agentryv1alpha1.ReasonInvalidReference,
			"no image: Agent.spec.image is empty and the AgentClass sets no defaultImage")
		return true, ctrl.Result{}, nil
	}
	if eff.PersistenceOn && eff.ExistingClaim != "" {
		var pvc corev1.PersistentVolumeClaim
		err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: eff.ExistingClaim}, &pvc)
		if apierrors.IsNotFound(err) {
			r.setReady(agent, false, agentryv1alpha1.ReasonExistingClaimNotFound,
				fmt.Sprintf("existingClaim %q not found in namespace %q", eff.ExistingClaim, agent.Namespace))
			return true, ctrl.Result{RequeueAfter: gateRequeue}, nil
		} else if err != nil {
			return false, ctrl.Result{}, err
		}
	}
	for _, ref := range eff.ImagePullSecrets {
		var sec corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: ref.Name}, &sec)
		if apierrors.IsNotFound(err) {
			r.setReady(agent, false, agentryv1alpha1.ReasonImagePullSecretMissing,
				fmt.Sprintf("imagePullSecret %q missing in namespace %q", ref.Name, agent.Namespace))
			return true, ctrl.Result{RequeueAfter: gateRequeue}, nil
		} else if err != nil {
			return false, ctrl.Result{}, err
		}
	}
	return false, ctrl.Result{}, nil
}

// degradedReasons evaluates every Degraded-triggering cross-check and returns
// the outstanding reasons in a stable order (first entry becomes the reported
// reason).
func (r *AgentReconciler) degradedReasons(
	ctx context.Context, agent *agentryv1alpha1.Agent, class *agentryv1alpha1.AgentClass, eff effectiveAgentSpec,
) []metav1.Condition {
	var out []metav1.Condition
	add := func(reason, msg string) {
		out = append(out, metav1.Condition{Reason: reason, Message: msg})
	}

	// Rule 2: image allowlist.
	if eff.Image != "" && !imageAllowed(eff.Image, class.Spec.Image.AllowedImages) {
		add(agentryv1alpha1.ReasonClassConstraintViolation,
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
			add(agentryv1alpha1.ReasonClassConstraintViolation,
				fmt.Sprintf("provider %q is not in AgentClass %q allowedProviders", name, class.Name))
			continue
		}
		var mp agentryv1alpha1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				add(agentryv1alpha1.ReasonClassConstraintViolation,
					fmt.Sprintf("provider %q does not exist", name))
			}
			continue
		}
		if !namespaceAllowed(agent.Namespace, mp.Spec.AllowedNamespaces) {
			add(agentryv1alpha1.ReasonClassConstraintViolation,
				fmt.Sprintf("provider %q does not allow namespace %q", name, agent.Namespace))
		}
	}
	// Rule 24: persistence must be class-permitted.
	if agent.Spec.Persistence.Enabled && !class.Spec.Persistence.Enabled {
		add(agentryv1alpha1.ReasonPersistenceNotAllowed,
			fmt.Sprintf("persistence requested but AgentClass %q has persistence.enabled=false", class.Name))
	}
	// Rule 26: hibernation must be class-permitted.
	if agent.Spec.Lifecycle.HibernationEnabled && !class.Spec.Lifecycle.HibernationAllowed {
		add(agentryv1alpha1.ReasonHibernationNotAllowed,
			fmt.Sprintf("hibernation requested but AgentClass %q has lifecycle.hibernationAllowed=false", class.Name))
	}
	// Rule 29: hibernation requires persistence (spec-internal).
	if agent.Spec.Lifecycle.HibernationEnabled && !agent.Spec.Persistence.Enabled {
		add(agentryv1alpha1.ReasonHibernationRequiresPersist,
			"lifecycle.hibernationEnabled=true requires spec.persistence.enabled=true")
	}
	return out
}

// enterOrStayDegraded transitions into Degraded (recording preDegradedPhase on
// first entry only) or refreshes reason/message while already Degraded.
func (r *AgentReconciler) enterOrStayDegraded(
	ctx context.Context, agent *agentryv1alpha1.Agent, reasons []metav1.Condition,
) error {
	first := reasons[0]
	if agent.Status.Phase != agentryv1alpha1.AgentDegraded {
		agent.Status.PreDegradedPhase = agent.Status.Phase
		r.setPhase(agent, agentryv1alpha1.AgentDegraded)
		r.Recorder.Event(agent, corev1.EventTypeWarning, first.Reason, first.Message)
	}
	r.setReady(agent, false, first.Reason, first.Message)
	return r.Status().Update(ctx, agent)
}

func (r *AgentReconciler) ensureCertificate(ctx context.Context, agent *agentryv1alpha1.Agent) (bool, error) {
	var cert cmapi.Certificate
	key := types.NamespacedName{Namespace: agent.Namespace, Name: agentCertificateName(agent.Name)}
	if err := r.Get(ctx, key, &cert); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		desired := desiredCertificate(agent)
		if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
			return false, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, err
		}
		return false, nil
	}
	for _, c := range cert.Status.Conditions {
		if c.Type == cmapi.CertificateConditionReady && c.Status == cmmeta.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

func (r *AgentReconciler) ensureServiceAccount(ctx context.Context, agent *agentryv1alpha1.Agent) error {
	desired := desiredServiceAccount(agent)
	if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
		return err
	}
	err := r.Create(ctx, desired)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (r *AgentReconciler) ensureService(ctx context.Context, agent *agentryv1alpha1.Agent, eff effectiveAgentSpec) error {
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
		if err := r.Create(ctx, desired); err != nil {
			return err
		}
	} else if len(current.Spec.Ports) != 1 ||
		current.Spec.Ports[0].Port != desired.Spec.Ports[0].Port ||
		current.Spec.Ports[0].TargetPort != desired.Spec.Ports[0].TargetPort {
		current.Spec.Ports = desired.Spec.Ports
		if err := r.Update(ctx, &current); err != nil {
			return err
		}
	}
	agent.Status.Endpoint = fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", agent.Name, agent.Namespace, eff.ServicePort)
	return nil
}

func (r *AgentReconciler) ensurePVC(
	ctx context.Context, agent *agentryv1alpha1.Agent, class *agentryv1alpha1.AgentClass, eff effectiveAgentSpec,
) error {
	desired := desiredPVC(agent, class, eff)
	if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
		return err
	}
	err := r.Create(ctx, desired)
	if apierrors.IsAlreadyExists(err) {
		err = nil
	}
	if err == nil {
		agent.Status.PVCName = desired.Name
	}
	return err
}

func (r *AgentReconciler) ensureNetworkPolicy(
	ctx context.Context, agent *agentryv1alpha1.Agent, class *agentryv1alpha1.AgentClass, eff effectiveAgentSpec,
) error {
	desired := desiredNetworkPolicy(agent, class, eff, r.OperatorNamespace)
	if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
		return err
	}
	var current networkingv1.NetworkPolicy
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &current); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		return r.Create(ctx, desired)
	}
	current.Spec = desired.Spec
	return r.Update(ctx, &current)
}

// convergePod implements Pod convergence: create when missing, replace when
// terminal (involuntary disruption) or when the spec hash drifts, mark the
// Agent Failed on a persistent crash loop, and derive Running from readiness.
func (r *AgentReconciler) convergePod(
	ctx context.Context, agent *agentryv1alpha1.Agent, eff effectiveAgentSpec,
) error {
	pod, err := r.ownedPod(ctx, agent)
	if err != nil {
		return err
	}

	if pod == nil {
		desired := desiredPod(agent, eff, r.OperatorNamespace)
		if err := controllerutil.SetControllerReference(agent, desired, r.Scheme()); err != nil {
			return err
		}
		if err := r.Create(ctx, desired); err != nil {
			return err
		}
		r.setPhase(agent, agentryv1alpha1.AgentProvisioning)
		r.setReady(agent, false, "PodProvisioning", "agent Pod created, waiting for readiness")
		agent.Status.PodName = desired.Name
		return nil
	}

	// A Pod already being deleted is a replacement in progress: wait for the
	// owned-Pod watch to fire when it is gone.
	if !pod.DeletionTimestamp.IsZero() {
		r.setPhase(agent, agentryv1alpha1.AgentProvisioning)
		r.setReady(agent, false, "PodProvisioning", "previous Pod terminating")
		return nil
	}

	// Involuntary disruption: a terminal Pod is never resurrected by the
	// kubelet under restartPolicy Always, so delete it and re-provision.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		r.Recorder.Event(agent, corev1.EventTypeWarning, "PodDisrupted",
			fmt.Sprintf("Pod %s is terminal (%s); re-provisioning", pod.Name, pod.Status.Phase))
		r.setPhase(agent, agentryv1alpha1.AgentProvisioning)
		r.setReady(agent, false, "PodDisrupted", "replacing a terminal Pod")
		return r.Delete(ctx, pod)
	}

	// Persistent crash loop marks the Agent Failed (any -> Failed).
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil &&
			(cs.State.Waiting.Reason == "CrashLoopBackOff" && cs.RestartCount >= crashLoopThreshold ||
				cs.State.Waiting.Reason == "ImagePullBackOff") {
			r.setPhase(agent, agentryv1alpha1.AgentFailed)
			r.setReady(agent, false, cs.State.Waiting.Reason,
				fmt.Sprintf("container %s: %s", cs.Name, cs.State.Waiting.Message))
			return nil
		}
	}

	// Spec drift: compare the stamped hash against the re-derived one, never
	// the live Pod object.
	if pod.Annotations[annotationPodSpecHash] != podSpecHash(eff) {
		r.Recorder.Event(agent, corev1.EventTypeNormal, "SpecDrift",
			"derived Pod spec changed; replacing the Pod")
		r.setPhase(agent, agentryv1alpha1.AgentProvisioning)
		r.setReady(agent, false, "SpecDrift", "replacing Pod for updated spec")
		return r.Delete(ctx, pod)
	}

	agent.Status.PodName = pod.Name
	if podReady(pod) {
		r.setPhase(agent, agentryv1alpha1.AgentRunning)
		r.setReady(agent, true, agentryv1alpha1.ReasonPodRunning, "agent Pod is ready")
	} else {
		r.setPhase(agent, agentryv1alpha1.AgentProvisioning)
		r.setReady(agent, false, "PodNotReady", "agent Pod is not ready")
	}
	return nil
}

// ownedPod returns the Agent's live Pod, preferring a non-terminating one when
// a replacement overlaps a termination.
func (r *AgentReconciler) ownedPod(ctx context.Context, agent *agentryv1alpha1.Agent) (*corev1.Pod, error) {
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
func (r *AgentReconciler) reconcileDelete(ctx context.Context, agent *agentryv1alpha1.Agent) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(agent, agentryv1alpha1.AgentFinalizer) {
		return ctrl.Result{}, nil
	}

	if agent.Status.Phase != agentryv1alpha1.AgentTerminating {
		agent.Status.PreDegradedPhase = ""
		r.setPhase(agent, agentryv1alpha1.AgentTerminating)
		if err := r.Status().Update(ctx, agent); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Terminate the Pod gracefully and wait for it to go away; the owned-Pod
	// watch re-enqueues us when it does.
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
		return ctrl.Result{}, nil
	}

	// pvcRetention: Retain strips the PVC's ownerRef before the finalizer is
	// removed, so cascade GC finds no owner and leaves the PVC in place. The
	// ownerRef edit must land before the finalizer removal: once the finalizer
	// entry is gone the Agent can vanish at any moment. existingClaim PVCs
	// never carried an ownerRef and are untouched.
	var class agentryv1alpha1.AgentClass
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
					return ctrl.Result{}, err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(agent, agentryv1alpha1.AgentFinalizer)
	return ctrl.Result{}, r.Update(ctx, agent)
}

func (r *AgentReconciler) setPhase(agent *agentryv1alpha1.Agent, phase agentryv1alpha1.AgentPhase) {
	if agent.Status.Phase == phase {
		return
	}
	agent.Status.Phase = phase
	now := metav1.Now()
	agent.Status.PhaseTransitionTime = &now
}

func (r *AgentReconciler) setReady(agent *agentryv1alpha1.Agent, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: agentryv1alpha1.ConditionReady, Status: status, Reason: reason, Message: msg,
	})
}

// SetupWithManager wires the reconciler, its owned children, and the
// platform-level map-func watches.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentryv1alpha1.Agent{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&cmapi.Certificate{}).
		Watches(&agentryv1alpha1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(r.agentsForClass)).
		Watches(&agentryv1alpha1.ModelProvider{}, handler.EnqueueRequestsFromMapFunc(r.agentsForProvider)).
		Complete(r)
}

func (r *AgentReconciler) agentsForClass(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents agentryv1alpha1.AgentList
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
	var agents agentryv1alpha1.AgentList
	if err := r.List(ctx, &agents, client.MatchingFields{IndexProviderRef: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(agents.Items))
	for _, a := range agents.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return reqs
}
