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
	"errors"
	"fmt"
	"strings"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// provisioningDeadline bounds how long an attempt may go without a Ready Pod
// before it fails: an image-pull or scheduling failure, counted from Pod
// creation, and a rejected Pod create or child write, counted from the
// attempt's first rejection (status.createRejectedTime). A documented
// constant, not a spec field (docs/src/controller/task-lifecycle.md).
const provisioningDeadline = 5 * time.Minute

// AgentTaskReconciler drives the run-to-completion state machine: Pending ->
// Provisioning -> Running -> Completing -> Succeeded/Failed/TimedOut, with
// backoffLimit retries bracketed by the currentPodUID check (the gateway
// records a completion only from that Pod) and the completion mailbox. See
// docs/src/controller/reconcilers/agenttask.md and
// docs/src/controller/task-lifecycle.md.
type AgentTaskReconciler struct {
	client.Client
	// claimsWarned holds the ResourceClaimsIgnored rising edge (rule 53).
	claimsWarned      claimsWarnings
	Recorder          record.EventRecorder
	OperatorNamespace string
	// DNS selects the peers of the DNS egress rule on every task NetworkPolicy.
	DNS DNSSelector
	// SecretReader reads Secrets in user namespaces, which the manager's
	// cache does not hold. In production it is a secretwatch.Reader: one
	// name-filtered watch per referenced Secret, so repeated reads are cache
	// hits. nil falls back to the embedded client.
	SecretReader client.Reader
	// MaxConcurrentReconciles is the number of reconciles that may run at
	// once; controller-runtime still serializes per object. 0 means one.
	MaxConcurrentReconciles int
	// CertLifetime sets the duration and renewBefore of each AgentTask's
	// Certificate. The zero value takes the defaults.
	CertLifetime CertLifetime
	// FQDNSupport reports whether the CNI can enforce FQDN egress policies;
	// production passes the FQDNProbe shared with the AgentClassReconciler.
	// nil means unsupported: no CiliumNetworkPolicy is synthesized.
	FQDNSupport func() (bool, error)
	// notReadyRecheckOverride overrides notReadyRecheck for childBlocked's
	// hold. Zero means notReadyRecheck; tests set it so they need not change
	// the package variable the envtest manager reads.
	notReadyRecheckOverride time.Duration
	// deadlineFor shortens the provisioning deadline for chosen tasks in
	// tests. nil, or a zero return, means provisioningDeadline. It is set
	// before the manager starts and only read after.
	deadlineFor func(*kaalmv1beta1.AgentTask) time.Duration
}

// deadline is the provisioning deadline for task.
func (r *AgentTaskReconciler) deadline(task *kaalmv1beta1.AgentTask) time.Duration {
	if r.deadlineFor != nil {
		if d := r.deadlineFor(task); d > 0 {
			return d
		}
	}
	return provisioningDeadline
}

// notReadyRecheckInterval is the re-check interval childBlocked holds a task
// for.
func (r *AgentTaskReconciler) notReadyRecheckInterval() time.Duration {
	if r.notReadyRecheckOverride > 0 {
		return r.notReadyRecheckOverride
	}
	return notReadyRecheck
}

// +kubebuilder:rbac:groups=kaalm.io,resources=agenttasks,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=kaalm.io,resources=agenttasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kaalm.io,resources=agenttasks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=escalate;bind

// Reconcile runs one pass of the AgentTask state machine.
func (r *AgentTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var task kaalmv1beta1.AgentTask
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !task.DeletionTimestamp.IsZero() {
		r.claimsWarned.forget(task.UID)
		return r.reconcileDelete(ctx, &task)
	}
	if controllerutil.AddFinalizer(&task, kaalmv1beta1.TaskFinalizer) {
		if err := r.Update(ctx, &task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	task.Status.ObservedGeneration = task.Generation
	if task.Status.Phase == "" {
		task.Status.Phase = kaalmv1beta1.TaskPending
	}

	// A Failed phase with no completionTime is a counted retry whose old-Pod
	// delete, mailbox reset, or Provisioning write has not finished: after a
	// controller restart, an error, or a write the API server rejected.
	// Finish it; nothing is counted again. A Failed phase with
	// completionTime set is terminal.
	if task.Status.Phase == kaalmv1beta1.TaskFailed && task.Status.CompletionTime == nil {
		if err := r.finishRetry(ctx, &task); err != nil {
			return r.childBlocked(ctx, &task, nil, false, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Terminal phases only wait out their TTL.
	// A settled task's claims no longer matter, so it does not warn.
	if isTerminalTaskPhase(task.Status.Phase) {
		r.claimsWarned.forget(task.UID)
		return r.handleTTL(ctx, &task)
	}
	// Rule 53: advisory, with no condition.
	r.claimsWarned.note(r.Recorder, &task, "spec.resources", task.Spec.Resources.Claims)

	// System-namespace guard (same SAN-integrity rule as Agents).
	if task.Namespace == r.OperatorNamespace {
		return ctrl.Result{}, r.markTaskNotReady(ctx, &task, kaalmv1beta1.ReasonSystemNamespaceForbidden,
			fmt.Sprintf("AgentTasks may not run in the operator namespace %q", r.OperatorNamespace))
	}

	var class kaalmv1beta1.AgentClass
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.AgentClassRef.Name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.markTaskNotReady(ctx, &task, kaalmv1beta1.ReasonInvalidReference,
				fmt.Sprintf("AgentClass %q does not exist", task.Spec.AgentClassRef.Name))
		}
		return ctrl.Result{}, err
	}
	eff := deriveEffectiveTaskSpec(&task, &class)

	pod, err := r.ownedTaskPod(ctx, &task)
	if err != nil {
		return ctrl.Result{}, err
	}
	// prePod: the attempt has no Pod yet, so a rejected child write is
	// bounded by the provisioning deadline.
	prePod := pod == nil && (task.Status.Phase == kaalmv1beta1.TaskPending ||
		task.Status.Phase == kaalmv1beta1.TaskProvisioning)

	// Pre-Pod validation runs only when provisioning a new Pod (initial
	// provisioning and the Provisioning re-entry of a backoff retry).
	// In-flight tasks continue under the class snapshot taken at Pod creation.
	if prePod {
		if reason, msg := r.taskViolation(ctx, &task, &class, eff); reason != "" {
			// Terminal: AgentTask has no Degraded phase. A task that never had
			// a Pod has no class bounds yet; record them in the settling
			// write so the class default TTL still reaches it.
			if task.Status.ClassBounds == nil && task.Status.PodName == "" {
				task.Status.ClassBounds = classTaskBounds(&class)
			}
			return ctrl.Result{}, r.settle(ctx, &task, kaalmv1beta1.TaskFailed, reason, msg)
		}
		// Rule 23 reads Secrets in the task's namespace, where the operator
		// holds no standing read: the scoped Role comes first.
		if err := ensureControllerSecretAccess(ctx, r.Client, r.Scheme(), &task, taskPullSecretRoleName(task.Name),
			r.OperatorNamespace, eff.ImagePullSecrets); err != nil {
			return r.childBlocked(ctx, &task, &class, true, err)
		}
		for _, ref := range eff.ImagePullSecrets {
			var sec corev1.Secret
			err := getSecretLive(ctx, liveSecretReader(r.SecretReader, r.Client),
				types.NamespacedName{Namespace: task.Namespace, Name: ref.Name}, &sec)
			if apierrors.IsNotFound(err) {
				if err := r.markTaskNotReady(ctx, &task, kaalmv1beta1.ReasonImagePullSecretMissing,
					fmt.Sprintf("imagePullSecret %q missing in namespace %q", ref.Name, task.Namespace)); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: notReadyRecheck}, nil
			} else if err != nil {
				return ctrl.Result{}, err
			}
		}
		if handled, res, err := r.enforceEnvSecretOptIn(ctx, &task, &class, eff); handled {
			return res, err
		}
		if eff.Image == "" {
			return ctrl.Result{}, r.markTaskNotReady(ctx, &task, kaalmv1beta1.ReasonInvalidReference,
				"no image: AgentTask.spec.image is empty and the AgentClass sets no defaultImage")
		}
		// Rule 19: the class is Ready=False, and the NetworkPolicy built from
		// its entries would fail the apiserver write on every pass.
		if bad := invalidCIDRs(&class); len(bad) > 0 {
			return ctrl.Result{}, r.markTaskNotReady(ctx, &task, kaalmv1beta1.ReasonInvalidReference,
				fmt.Sprintf("AgentClass %q is not usable: %s", class.Name, strings.Join(bad, "; ")))
		}
	}

	// A Failed task reaching this point is in the retry path: the eligibility
	// was decided when Failed was entered (settle vs retry), so a lingering
	// Failed phase with a live retry has already transitioned to Provisioning.
	res, err := r.drive(ctx, &task, &class, eff, pod)
	if err != nil {
		return r.childBlocked(ctx, &task, &class, prePod, err)
	}
	logger.V(1).Info("reconciled AgentTask", "phase", task.Status.Phase)
	return res, nil
}

// drive advances the non-terminal state machine given the current Pod.
func (r *AgentTaskReconciler) drive(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass,
	eff effectiveTaskSpec, pod *corev1.Pod,
) (ctrl.Result, error) {
	switch task.Status.Phase {
	case kaalmv1beta1.TaskPending, kaalmv1beta1.TaskProvisioning:
		return r.driveProvisioning(ctx, task, class, eff, pod)
	case kaalmv1beta1.TaskRunning:
		return r.driveRunning(ctx, task, class, eff, pod)
	case kaalmv1beta1.TaskCompleting:
		return ctrl.Result{}, r.driveCompleting(ctx, task, pod)
	}
	return ctrl.Result{}, nil
}

// driveProvisioning creates the child tree, waits for the Certificate, creates
// the Pod, and watches it to Ready or an early failure. A pass still waiting
// on the Pod re-creates a missing child after the state checks, so it never
// delays them.
func (r *AgentTaskReconciler) driveProvisioning(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass,
	eff effectiveTaskSpec, pod *corev1.Pod,
) (ctrl.Result, error) {
	if pod == nil {
		tlsSecret, certReady, err := r.ensureTaskCertificate(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !certReady {
			if task.Status.Phase == kaalmv1beta1.TaskPending {
				r.setTaskPhase(task, kaalmv1beta1.TaskProvisioning)
			}
			r.setTaskReady(task, false, kaalmv1beta1.ReasonCertificateNotReady, "waiting for cert-manager to issue the task certificate")
			if err := r.updateStatusIfChanged(ctx, task); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: certWaitRequeue}, nil
		}
		if err := r.ensureTaskChildren(ctx, task, class, eff); err != nil {
			return ctrl.Result{}, err
		}
		desired := desiredTaskPod(task, eff, r.OperatorNamespace, tlsSecret)
		if err := controllerutil.SetControllerReference(task, desired, r.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desired); err != nil {
			if !isWriteRejection(err) {
				return ctrl.Result{}, err
			}
			return r.createRejected(ctx, task, class, kaalmv1beta1.ReasonPodCreateRejected,
				"Pod create rejected", err.Error())
		}
		r.setTaskPhase(task, kaalmv1beta1.TaskProvisioning)
		r.setTaskReady(task, false, "PodProvisioning", "task Pod created, waiting for readiness")
		task.Status.PodName = desired.Name
		task.Status.CreateRejectedTime = nil
		// The class snapshot for this attempt: a retry's new Pod copies the
		// bounds and the egress lists again from the class as it then
		// stands, the lists the children were just brought to.
		task.Status.ClassBounds = classTaskBounds(class)
		task.Status.ClassEgress = classTaskEgress(class)
		if isAgentReported(task) {
			task.Status.CurrentPodUID = string(desired.UID)
		}
		return ctrl.Result{}, r.Status().Update(ctx, task)
	}

	// A retry's old Pod may still be terminating. Hold until it is gone: the
	// Pod UID check rejects its completions, its terminal state spends no
	// backoff unit, and the replacement is created only once no task Pod
	// remains.
	if !pod.DeletionTimestamp.IsZero() {
		r.setTaskReady(task, false, "PodTerminating", "waiting for the previous task Pod to terminate")
		if err := r.updateStatusIfChanged(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: certWaitRequeue}, nil
	}

	// Repair the observed Pod's identity when the status write after its
	// creation was lost (the Pod UID check accepts the new Pod again after a
	// retry). A podName that does not match shows the loss, so the class
	// bounds and egress lists are written with it. A Pod created before
	// status.classBounds or status.classEgress existed has a matching podName
	// and keeps no record.
	lost := task.Status.PodName != pod.Name
	if lost || (isAgentReported(task) && task.Status.CurrentPodUID != string(pod.UID)) {
		if lost {
			task.Status.ClassBounds = classTaskBounds(class)
			task.Status.ClassEgress = classTaskEgress(class)
		}
		if isAgentReported(task) {
			task.Status.CurrentPodUID = string(pod.UID)
		}
		task.Status.PodName = pod.Name
		task.Status.CreateRejectedTime = nil
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
	}

	if podReady(pod) {
		r.setTaskPhase(task, kaalmv1beta1.TaskRunning)
		now := metav1.Now()
		task.Status.StartTime = &now
		r.setTaskReady(task, true, "PodRunning", "task Pod is running")
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return runningRequeue(task), nil
	}

	// Terminal before Ready: under restartPolicy Never one crash is final.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		// An exitCode task whose container ran to completion before the Ready
		// condition ever flipped is still a completion, not a provisioning
		// failure: fall through to Completing.
		if !isAgentReported(task) && pod.Status.Phase == corev1.PodSucceeded {
			r.setTaskPhase(task, kaalmv1beta1.TaskCompleting)
			if err := r.Status().Update(ctx, task); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.driveCompleting(ctx, task, pod)
		}
		return ctrl.Result{}, r.failOrRetry(ctx, task, "PodStartFailed",
			fmt.Sprintf("Pod %s reached %s before becoming Ready", pod.Name, pod.Status.Phase))
	}

	// Fatal config errors fail immediately; pull/scheduling failures fail
	// after the provisioning deadline.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		switch cs.State.Waiting.Reason {
		case "InvalidImageName", "ErrImageNeverPull":
			return ctrl.Result{}, r.failOrRetry(ctx, task, cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
	}
	if time.Since(pod.CreationTimestamp.Time) > r.deadline(task) {
		return ctrl.Result{}, r.failOrRetry(ctx, task, "ProvisioningDeadlineExceeded",
			fmt.Sprintf("Pod %s not Ready within %s", pod.Name, r.deadline(task)))
	}
	return r.awaitPodReady(ctx, task, class, eff, pod)
}

// awaitPodReady ends a Provisioning pass whose Pod is not Ready yet, after
// every state check: it re-creates a missing child, keeps Ready at
// PodProvisioning (clearing a Ready=False reason the pass no longer finds),
// and re-checks soon.
func (r *AgentTaskReconciler) awaitPodReady(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass,
	eff effectiveTaskSpec, pod *corev1.Pod,
) (ctrl.Result, error) {
	if err := r.restoreTaskChildren(ctx, task, class, eff, pod, false); err != nil {
		return ctrl.Result{}, err
	}
	r.setTaskReady(task, false, "PodProvisioning", "task Pod created, waiting for readiness")
	return ctrl.Result{RequeueAfter: certWaitRequeue}, r.updateStatusIfChanged(ctx, task)
}

// updateStatusIfChanged writes the task's status only when it differs from
// what the informer holds. A Provisioning task re-checks every
// certWaitRequeue while it waits, and an unchanged write per re-check is a
// PUT that moves nothing but is still an update event for every watcher.
// A cache that lags an earlier write in the same pass costs at most one
// redundant write, never a missed one.
func (r *AgentTaskReconciler) updateStatusIfChanged(ctx context.Context, task *kaalmv1beta1.AgentTask) error {
	var current kaalmv1beta1.AgentTask
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), &current); err == nil &&
		equality.Semantic.DeepEqual(current.Status, task.Status) {
		return nil
	}
	return r.Status().Update(ctx, task)
}

// createRejected holds a task whose Pod create, or a write of another
// child it needs before the Pod, the API server rejected: Provisioning with
// Ready=False reason, re-checked on notReadyRecheck because the cause (a
// RuntimeClass, a quota, a webhook) is not watched. The provisioning
// deadline counts from the attempt's first rejection, recorded in status so
// a controller restart keeps it; past the deadline the attempt fails or
// retries like a Pod that never became Ready, with a message of what and
// msg.
func (r *AgentTaskReconciler) createRejected(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass, reason, what, msg string,
) (ctrl.Result, error) {
	now := metav1.Now()
	if task.Status.CreateRejectedTime == nil {
		task.Status.CreateRejectedTime = &now
	}
	task.Status.PodName = ""
	if now.Sub(task.Status.CreateRejectedTime.Time) > r.deadline(task) {
		// A task that settles without ever having a Pod has no class bounds
		// yet; record them so the class default TTL still reaches it.
		if task.Status.Retries >= task.Spec.Completion.BackoffLimit && task.Status.ClassBounds == nil {
			task.Status.ClassBounds = classTaskBounds(class)
		}
		return ctrl.Result{}, r.failOrRetry(ctx, task, "ProvisioningDeadlineExceeded",
			fmt.Sprintf("%s for longer than %s: %s", what, r.deadline(task), msg))
	}
	r.setTaskPhase(task, kaalmv1beta1.TaskProvisioning)
	if err := r.markTaskNotReady(ctx, task, reason, msg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: notReadyRecheck}, nil
}

// driveRunning watches for completion, timeout, and mid-run Pod loss, in that
// precedence order: a completion already in the mailbox beats a lost Pod. A
// pass that finds the task still running re-creates a missing child after
// those checks, so it never delays them.
func (r *AgentTaskReconciler) driveRunning(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass,
	eff effectiveTaskSpec, pod *corev1.Pod,
) (ctrl.Result, error) {
	// agentReported: the mailbox is the completion signal.
	if isAgentReported(task) {
		payload, err := r.readMailbox(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if payload.Status != "" {
			r.setTaskPhase(task, kaalmv1beta1.TaskCompleting)
			if err := r.Status().Update(ctx, task); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.driveCompleting(ctx, task, pod)
		}
	}

	// exitCode: a terminal Pod is the completion signal.
	if !isAgentReported(task) && pod != nil &&
		(pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) {
		r.setTaskPhase(task, kaalmv1beta1.TaskCompleting)
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.driveCompleting(ctx, task, pod)
	}

	// Timeout: measured from startTime, so scheduling never counts.
	if timedOut(task) {
		r.setTaskPhase(task, kaalmv1beta1.TaskCompleting)
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.driveCompleting(ctx, task, pod)
	}

	// Mid-run Pod loss with an empty mailbox is a retryable failure.
	if pod == nil || (isAgentReported(task) && pod.Status.Phase == corev1.PodFailed) {
		return ctrl.Result{}, r.failOrRetry(ctx, task, "PodDisrupted", "task Pod was lost mid-run")
	}
	return r.continueRunning(ctx, task, class, eff, pod)
}

// continueRunning ends a Running pass whose Pod runs on, after every state
// check: it re-creates a missing child and restores Ready=True PodRunning
// when a failed check or a conflict left it otherwise. A terminating Pod needs
// neither; the next pass handles its loss.
func (r *AgentTaskReconciler) continueRunning(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass,
	eff effectiveTaskSpec, pod *corev1.Pod,
) (ctrl.Result, error) {
	if !pod.DeletionTimestamp.IsZero() {
		return runningRequeue(task), nil
	}
	if err := r.restoreTaskChildren(ctx, task, class, eff, pod, true); err != nil {
		return ctrl.Result{}, err
	}
	if !apimeta.IsStatusConditionTrue(task.Status.Conditions, kaalmv1beta1.ConditionReady) {
		r.setTaskReady(task, true, "PodRunning", "task Pod is running")
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
	}
	return runningRequeue(task), nil
}

// driveCompleting settles the terminal phase. The outcome is re-derived from
// the mailbox, the Pod, and the clock rather than stored: mailbox payload
// first, then container exit, then timeout.
func (r *AgentTaskReconciler) driveCompleting(
	ctx context.Context, task *kaalmv1beta1.AgentTask, pod *corev1.Pod,
) error {
	if isAgentReported(task) {
		payload, err := r.readMailbox(ctx, task)
		if err != nil {
			return err
		}
		if payload.Status != "" {
			if msg := validateArtifactNames(payload, task.Spec.Artifacts); msg != "" {
				// The gateway enforces the same rule synchronously, so this
				// firing means something drifted.
				return r.failOrRetry(ctx, task, kaalmv1beta1.ReasonTaskFailed, "artifact validation failed: "+msg)
			}
			task.Status.ArtifactValues = payload.Artifacts
			task.Status.AgentReportedStatus = payload.Status
			task.Status.AgentReportedMessage = payload.Message
			if payload.Status == completionStatusSuccess {
				return r.settle(ctx, task, kaalmv1beta1.TaskSucceeded,
					kaalmv1beta1.ReasonTaskSucceeded, payload.Message)
			}
			return r.failOrRetry(ctx, task, kaalmv1beta1.ReasonTaskFailed,
				"agent reported failure: "+payload.Message)
		}
		// No payload: this Completing pass was timeout-triggered.
		if timedOut(task) {
			return r.settleTimeout(ctx, task)
		}
		return r.failOrRetry(ctx, task, "PodDisrupted", "task reached Completing with no completion payload")
	}

	// exitCode mode.
	if pod != nil && pod.Status.Phase == corev1.PodSucceeded {
		return r.settle(ctx, task, kaalmv1beta1.TaskSucceeded,
			kaalmv1beta1.ReasonTaskSucceeded, "container exited 0")
	}
	if pod != nil && pod.Status.Phase == corev1.PodFailed {
		return r.failOrRetry(ctx, task, kaalmv1beta1.ReasonTaskFailed, podExitMessage(pod))
	}
	if timedOut(task) {
		return r.settleTimeout(ctx, task)
	}
	// Pod vanished between Running and Completing.
	return r.failOrRetry(ctx, task, "PodDisrupted", "task Pod was lost before completion settled")
}

// settleTimeout applies spec.completion.onTimeout: Fail (default) settles
// TimedOut, Succeed settles Succeeded. TimedOut is exempt from backoffLimit.
func (r *AgentTaskReconciler) settleTimeout(ctx context.Context, task *kaalmv1beta1.AgentTask) error {
	if task.Spec.Completion.OnTimeout == onTimeoutSucceed {
		return r.settle(ctx, task, kaalmv1beta1.TaskSucceeded, "TimeoutSucceeded",
			"timeout reached with onTimeout: Succeed")
	}
	return r.settle(ctx, task, kaalmv1beta1.TaskTimedOut, "TimeoutExceeded",
		fmt.Sprintf("task exceeded its %s completion timeout", taskTimeout(task)))
}

// failOrRetry either executes the retry sequence (backoffLimit permitting) or
// settles the task in terminal Failed.
func (r *AgentTaskReconciler) failOrRetry(
	ctx context.Context, task *kaalmv1beta1.AgentTask, reason, msg string,
) error {
	if task.Status.Retries < task.Spec.Completion.BackoffLimit {
		return r.retry(ctx, task, reason, msg)
	}
	return r.settle(ctx, task, kaalmv1beta1.TaskFailed, reason, msg)
}

// retry runs the documented sequence in order: increment retries, clear the
// UID (the old Pod's completions are now rejected), delete the old Pod, reset
// the mailbox, transition back to Provisioning. The next pass creates the new
// Pod and writes its UID from the Create response. The counting write (steps 1
// and 2) comes first, so a lost write never counts a retry twice; finishRetry
// does the rest, and Reconcile calls it again for a retry that stopped
// partway. The clear-before-reset ordering is load-bearing: resetting the
// mailbox first would let an in-flight stale write land on the fresh mailbox.
func (r *AgentTaskReconciler) retry(ctx context.Context, task *kaalmv1beta1.AgentTask, reason, msg string) error {
	retrying := fmt.Sprintf("%s; retrying (%d/%d)", msg, task.Status.Retries+1, task.Spec.Completion.BackoffLimit)

	// Steps 1 and 2 in one status write: the counter moves and the old Pod's
	// completions are rejected from here on.
	task.Status.Retries++
	task.Status.CurrentPodUID = ""
	task.Status.StartTime = nil
	task.Status.ArtifactValues = nil
	task.Status.AgentReportedStatus = ""
	task.Status.AgentReportedMessage = ""
	task.Status.CreateRejectedTime = nil
	r.setTaskPhase(task, kaalmv1beta1.TaskFailed)
	r.setTaskReady(task, false, reason, msg)
	if err := r.Status().Update(ctx, task); err != nil {
		return err
	}
	// After the write that counts the retry, so a pass that lost its write
	// to a conflict does not report the same retry twice.
	r.Recorder.Event(task, corev1.EventTypeWarning, reason, retrying)
	return r.finishRetry(ctx, task)
}

// finishRetry finishes a counted retry: it deletes the old Pod, resets the
// mailbox, and writes Provisioning. Every step is safe to repeat: a Pod
// already deleting is skipped, and an empty mailbox is not written. It runs
// right after the counting write and also from Reconcile for a Failed task
// with no completionTime, so a delete or reset that fails is finished on a
// later pass without counting the retry again. A delete or reset the API
// server rejects comes back as a ChildWriteRejectedError, and the task stays
// in Failed until it goes through.
func (r *AgentTaskReconciler) finishRetry(ctx context.Context, task *kaalmv1beta1.AgentTask) error {
	// Step 3: delete the old Pod if any remains.
	pod, err := r.ownedTaskPod(ctx, task)
	if err != nil {
		return err
	}
	if pod != nil && pod.DeletionTimestamp.IsZero() {
		if err := rejectedWrite("deleting", r.Scheme(), pod, client.IgnoreNotFound(r.Delete(ctx, pod))); err != nil {
			return err
		}
	}

	// Step 4: reset the mailbox in place, preserving ownerRef and the scoped
	// Role's validity.
	if isAgentReported(task) {
		var cm corev1.ConfigMap
		key := types.NamespacedName{Namespace: task.Namespace, Name: taskCompletionCMName(task.Name)}
		if err := r.Get(ctx, key, &cm); err == nil {
			if err := requireControlled(r.Scheme(), task, &cm); err != nil {
				return err
			}
			if len(cm.Data) > 0 {
				cm.Data = map[string]string{}
				if err := rejectedWrite("updating", r.Scheme(), &cm, r.Update(ctx, &cm)); err != nil {
					return err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}

	// Step 6: back to Provisioning. Pod recreation happens on the next pass
	// once the old Pod is gone.
	r.setTaskPhase(task, kaalmv1beta1.TaskProvisioning)
	return r.Status().Update(ctx, task)
}

// settle commits a terminal phase with its condition and completion time, and
// reports it in an event once the write succeeds.
func (r *AgentTaskReconciler) settle(
	ctx context.Context, task *kaalmv1beta1.AgentTask, phase kaalmv1beta1.AgentTaskPhase, reason, msg string,
) error {
	r.setTaskPhase(task, phase)
	now := metav1.Now()
	task.Status.CompletionTime = &now
	task.Status.CreateRejectedTime = nil
	completed := metav1.ConditionFalse
	if phase == kaalmv1beta1.TaskSucceeded {
		completed = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionCompleted, Status: completed, Reason: reason, Message: msg,
	})
	r.setTaskReady(task, false, reason, msg)
	eventType := corev1.EventTypeNormal
	if phase != kaalmv1beta1.TaskSucceeded {
		eventType = corev1.EventTypeWarning
	}
	if err := r.Status().Update(ctx, task); err != nil {
		return err
	}
	r.Recorder.Event(task, eventType, reason, msg)
	return nil
}

// handleTTL deletes a terminal task once its effective ttlSecondsAfterFinished
// has elapsed: the current spec within the class bounds copied at Pod
// creation, so a class edit after that never reaches the task.
func (r *AgentTaskReconciler) handleTTL(ctx context.Context, task *kaalmv1beta1.AgentTask) (ctrl.Result, error) {
	ttl := taskTTL(task)
	if ttl == nil {
		return ctrl.Result{}, nil
	}
	finished := task.Status.CompletionTime
	if finished == nil {
		return ctrl.Result{}, nil
	}
	expiry := finished.Add(time.Duration(*ttl) * time.Second)
	if remaining := time.Until(expiry); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	r.setTaskPhase(task, kaalmv1beta1.TaskTerminating)
	if err := r.Status().Update(ctx, task); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, task))
}

// taskViolation runs the pre-Pod cross-checks. AgentTask has no Degraded
// phase, so any violation is a terminal Failed.
func (r *AgentTaskReconciler) taskViolation(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass, eff effectiveTaskSpec,
) (string, string) {
	// Rule 47: the class admits the task's namespace.
	if !class.AdmitsNamespace(task.Namespace) {
		return kaalmv1beta1.ReasonNamespaceNotAllowed,
			fmt.Sprintf("namespace %q is not in AgentClass %q allowedNamespaces", task.Namespace, class.Name)
	}
	if eff.Image != "" && !imageAllowed(eff.Image, class.Spec.Image.AllowedImages) {
		return kaalmv1beta1.ReasonClassConstraintViolation,
			fmt.Sprintf("image %q does not match AgentClass %q allowedImages", eff.Image, class.Name)
	}
	for _, p := range task.Spec.Providers {
		name := p.ProviderRef.Name
		allowed := false
		for _, ap := range class.Spec.AllowedProviders {
			if ap.Name == name {
				allowed = true
				break
			}
		}
		if !allowed {
			return kaalmv1beta1.ReasonClassConstraintViolation,
				fmt.Sprintf("provider %q is not in AgentClass %q allowedProviders", name, class.Name)
		}
		var mp kaalmv1beta1.ModelProvider
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &mp); err != nil {
			if apierrors.IsNotFound(err) {
				return kaalmv1beta1.ReasonClassConstraintViolation,
					fmt.Sprintf("provider %q does not exist", name)
			}
			continue
		}
		if !namespaceAllowed(task.Namespace, mp.Spec.AllowedNamespaces) {
			return kaalmv1beta1.ReasonClassConstraintViolation,
				fmt.Sprintf("provider %q does not allow namespace %q", name, task.Namespace)
		}
	}
	// Rules 35 to 38 for the task's tool grants; the first violation wins,
	// terminal like every other task violation.
	if v := toolGrantViolations(ctx, r.Client, task.Namespace, task.Spec.Tools, class); len(v) > 0 {
		return v[0].Reason, v[0].Message
	}
	if task.Spec.Persistence.Enabled && !class.Spec.Persistence.Enabled {
		return kaalmv1beta1.ReasonPersistenceNotAllowed,
			fmt.Sprintf("persistence requested but AgentClass %q has persistence.enabled=false", class.Name)
	}
	return "", ""
}

// ensureTaskChildren converges the SA, PVC, NetworkPolicy, the completion
// mailbox with its scoped RBAC (agentReported tasks only), and the FQDN
// policy before the Pod is created; restoreTaskChildren covers the time
// after. The children are brought to the class as it now stands, the lists
// the new Pod records: a drifted NetworkPolicy, Role, or RoleBinding is
// updated, while the ServiceAccount, PVC, and ConfigMap are only created.
// Each child is read from the cache first, so one that matches costs no API
// call.
func (r *AgentTaskReconciler) ensureTaskChildren(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass, eff effectiveTaskSpec,
) error {
	// A name taken by an object the task does not control is a
	// ChildConflictError: adopting it would run the Pod under a policy or
	// grant Kaalm did not write.
	children := desiredTaskChildren(task, class, eff, class.Spec.Network.Egress.AllowedCIDRs, r.OperatorNamespace, r.DNS)
	for _, obj := range children {
		if err := controllerutil.SetControllerReference(task, obj, r.Scheme()); err != nil {
			return err
		}
		if err := convergeTaskChild(ctx, r.Client, task, obj); err != nil {
			return err
		}
	}
	hosts := class.Spec.Network.Egress.AllowedHosts
	supported, err := fqdnSupported(r.FQDNSupport)
	if err != nil {
		// As for Agents: no hosts to enforce, so a discovery failure must not
		// block the pass.
		if len(hosts) > 0 {
			return err
		}
		supported = false
	}
	return ensureFQDNPolicy(ctx, r.Client, r.Scheme(), task, taskPodLabels(task), hosts, r.DNS, supported)
}

// restoreTaskChildren keeps the children of a task that has a live Pod as
// the reconciler wrote them: the Certificate and the children
// desiredTaskChildren lists. A missing child is re-created, and an edited
// NetworkPolicy, CiliumNetworkPolicy, completion Role, or RoleBinding is
// reverted; the ServiceAccount, PVC, and ConfigMap are only re-created. The
// two policies are built from status.classEgress, so a later class edit does
// not reach the task; for a legacy task (no record, its Pod predates the
// field) they are only re-created, from the class as it now stands. It runs
// only after the pass's state checks (completion, timeout, Pod loss, the
// provisioning deadline), so it never delays them, and it never waits on
// readiness: the Pod keeps what it already mounted. The PVC comes back only
// when the Pod mounts it. The FQDN policy is read live, so it is touched
// only when withFQDN is set (Running passes, which are event-driven and
// few). The Secret-access Roles are not restored: only pre-Pod checks read
// through them. Every child is tried, and the failures come back joined, so
// a conflict on one child does not stop the others. In steady state it
// costs only cache reads and makes no writes.
func (r *AgentTaskReconciler) restoreTaskChildren(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass,
	eff effectiveTaskSpec, pod *corev1.Pod, withFQDN bool,
) error {
	var errs []error
	eff.PersistenceOn = podMountsClaim(pod, taskPVCName(task.Name))
	egress, revert := taskPolicyEgress(task, class)
	for _, obj := range desiredTaskChildren(task, class, eff, egress.AllowedCIDRs, r.OperatorNamespace, r.DNS) {
		if err := controllerutil.SetControllerReference(task, obj, r.Scheme()); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, isPolicy := obj.(*networkingv1.NetworkPolicy); isPolicy && !revert {
			errs = append(errs, createIfMissing(ctx, r.Client, task, obj))
			continue
		}
		errs = append(errs, convergeTaskChild(ctx, r.Client, task, obj))
	}
	_, _, err := r.ensureTaskCertificate(ctx, task)
	errs = append(errs, err)
	if hosts := egress.AllowedHosts; withFQDN && len(hosts) > 0 {
		supported, err := fqdnSupported(r.FQDNSupport)
		if err == nil {
			if revert {
				err = ensureFQDNPolicy(ctx, r.Client, r.Scheme(), task, taskPodLabels(task), hosts, r.DNS, supported)
			} else {
				err = restoreFQDNPolicy(ctx, r.Client, r.Scheme(), task, taskPodLabels(task), hosts, r.DNS, supported)
			}
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// taskPolicyEgress returns the egress lists a task with a Pod keeps its
// policies to: status.classEgress, recorded at Pod creation, with revert
// true. A task whose Pod predates the record gets the class as it now
// stands, with revert false: its policies are only re-created, never
// updated, so an upgrade changes no running task's egress.
func taskPolicyEgress(
	task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass,
) (egress kaalmv1beta1.AgentClassEgress, revert bool) {
	if task.Status.ClassEgress != nil {
		return *task.Status.ClassEgress, true
	}
	return class.Spec.Network.Egress, false
}

// convergeTaskChild is the update-in-place sibling of createIfMissing for a
// task's children. It reads the child from the cache and creates it when
// missing. A NetworkPolicy, Role, or RoleBinding whose content differs from
// desired is updated; any other kind (the ServiceAccount, the PVC, the
// completion ConfigMap) is never updated. A RoleBinding's roleRef is
// immutable, so one that differs is deleted, with a UID precondition so a
// stale cache never deletes a binding already replaced, and created again.
// An object the owner does not control is a ChildConflictError, with no
// write; one being deleted counts as present. A rejected write comes back as
// a ChildWriteRejectedError. The update carries the cached resourceVersion,
// so a stale cache gets a conflict that is retried with backoff rather than
// a write loop.
func convergeTaskChild(ctx context.Context, c client.Client, owner, desired client.Object) error {
	current, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a client.Object", desired)
	}
	err := c.Get(ctx, types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}, current)
	if apierrors.IsNotFound(err) {
		return createControlled(ctx, c, owner, desired)
	}
	if err != nil {
		return err
	}
	if err := requireControlled(c.Scheme(), owner, current); err != nil {
		return err
	}
	if !current.GetDeletionTimestamp().IsZero() {
		return nil
	}
	switch cur := current.(type) {
	case *networkingv1.NetworkPolicy:
		d := desired.(*networkingv1.NetworkPolicy)
		if equality.Semantic.DeepEqual(cur.Spec, d.Spec) {
			return nil
		}
		cur.Spec = d.Spec
	case *rbacv1.Role:
		d := desired.(*rbacv1.Role)
		if equality.Semantic.DeepEqual(cur.Rules, d.Rules) {
			return nil
		}
		cur.Rules = d.Rules
	case *rbacv1.RoleBinding:
		d := desired.(*rbacv1.RoleBinding)
		if cur.RoleRef != d.RoleRef {
			uid := cur.UID
			err := c.Delete(ctx, cur, client.Preconditions{UID: &uid})
			if err := rejectedWrite("deleting", c.Scheme(), cur, client.IgnoreNotFound(err)); err != nil {
				return err
			}
			return createControlled(ctx, c, owner, desired)
		}
		if equality.Semantic.DeepEqual(cur.Subjects, d.Subjects) {
			return nil
		}
		cur.Subjects = d.Subjects
	default:
		return nil
	}
	return rejectedWrite("updating", c.Scheme(), current, c.Update(ctx, current))
}

// podMountsClaim reports whether pod mounts the PersistentVolumeClaim claim.
func podMountsClaim(pod *corev1.Pod, claim string) bool {
	if pod == nil {
		return false
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// ensureTaskCertificate creates the task's Certificate when it is missing and
// reports whether it is Ready. Once the Certificate exists and the task
// controls it, it also returns the Secret name the Certificate writes
// (spec.secretName), which the Pod mounts. An existing Certificate is never
// updated, so it keeps the Secret name it was created with. While the task
// has a live Pod, restoreTaskChildren calls it too and ignores readiness.
func (r *AgentTaskReconciler) ensureTaskCertificate(ctx context.Context, task *kaalmv1beta1.AgentTask) (string, bool, error) {
	var cert cmapi.Certificate
	key := types.NamespacedName{Namespace: task.Namespace, Name: taskCertificateName(task.Name)}
	if err := r.Get(ctx, key, &cert); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", false, err
		}
		desired := desiredTaskCertificate(task, r.CertLifetime)
		if err := controllerutil.SetControllerReference(task, desired, r.Scheme()); err != nil {
			return "", false, err
		}
		return "", false, createControlled(ctx, r.Client, task, desired)
	}
	if err := requireControlled(r.Scheme(), task, &cert); err != nil {
		return "", false, err
	}
	return cert.Spec.SecretName, certificateReady(&cert), nil
}

func (r *AgentTaskReconciler) readMailbox(ctx context.Context, task *kaalmv1beta1.AgentTask) (completionPayload, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: task.Namespace, Name: taskCompletionCMName(task.Name)}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return completionPayload{}, nil
		}
		return completionPayload{}, err
	}
	if err := requireControlled(r.Scheme(), task, &cm); err != nil {
		return completionPayload{}, err
	}
	return parseCompletion(cm.Data), nil
}

func (r *AgentTaskReconciler) ownedTaskPod(ctx context.Context, task *kaalmv1beta1.AgentTask) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(task.Namespace),
		client.MatchingLabels(taskPodLabels(task))); err != nil {
		return nil, err
	}
	var candidate *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if !metav1.IsControlledBy(p, task) {
			continue
		}
		if p.DeletionTimestamp.IsZero() {
			return p, nil
		}
		candidate = p
	}
	return candidate, nil
}

// enforceEnvSecretOptIn applies rule 48 before a task Pod is made: every
// Secret the env reads must opt in to workload use, read under the task's own
// scoped env-Secret Role. A failure is not terminal: labeling the Secret lets
// provisioning continue on a later pass. handled reports whether the pass
// ends here, with res and err as its result.
func (r *AgentTaskReconciler) enforceEnvSecretOptIn(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass, eff effectiveTaskSpec,
) (handled bool, res ctrl.Result, err error) {
	if err := ensureControllerSecretAccess(ctx, r.Client, r.Scheme(), task, taskEnvSecretRoleName(task.Name),
		r.OperatorNamespace, envSecretRefs(eff.Env)); err != nil {
		res, err := r.childBlocked(ctx, task, class, true, err)
		return true, res, err
	}
	reason, msg, err := checkEnvSecrets(ctx, liveSecretReader(r.SecretReader, r.Client), task.Namespace, eff.Env)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if reason == "" {
		return false, ctrl.Result{}, nil
	}
	if err := r.markTaskNotReady(ctx, task, reason, msg); err != nil {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{RequeueAfter: notReadyRecheck}, nil
}

// childBlocked reports a child the pass cannot converge, and passes any
// other error through. A ChildConflictError (the name is taken by an object
// the task does not control) gives Ready=False ChildConflict; a
// ChildWriteRejectedError (the API server refused a create, update, or
// delete of the child) gives Ready=False ChildWriteRejected. Either way a
// Warning event reports it and the phase is kept. Before a Pod exists the
// task waits, and a rejected write starts the provisioning deadline the way
// a rejected Pod create does (prePod; class must be set then). A task in
// Failed is mid-retry and keeps that phase, which marks the steps still to
// finish, so a rejected write there only sets Ready, as with a Pod. With a Pod,
// either only sets Ready: completion, timeout, and Pod loss are still acted
// on each pass, since those checks run before any child write, and the
// other missing children are still re-created on the same pass. The cause
// (a conflicting object this workload does not control: one with no owner
// reference, or one controlled by a same-named Agent or AgentTask; a quota;
// a webhook) raises no watch event for this workload, so the notReadyRecheck
// requeue is what notices it clearing; a running task is requeued sooner
// when its timeout comes first.
func (r *AgentTaskReconciler) childBlocked(
	ctx context.Context, task *kaalmv1beta1.AgentTask, class *kaalmv1beta1.AgentClass, prePod bool, err error,
) (ctrl.Result, error) {
	if cr, ok := asChildWriteRejected(err); ok {
		if prePod && task.Status.Phase != kaalmv1beta1.TaskFailed {
			return r.createRejected(ctx, task, class, kaalmv1beta1.ReasonChildWriteRejected,
				"child write rejected", cr.Error())
		}
		if err := r.markTaskNotReady(ctx, task, kaalmv1beta1.ReasonChildWriteRejected, cr.Error()); err != nil {
			return ctrl.Result{}, err
		}
		return heldRequeue(task, r.notReadyRecheckInterval()), nil
	}
	cc, ok := asChildConflict(err)
	if !ok {
		return ctrl.Result{}, err
	}
	msg := cc.Error()
	if prev := apimeta.FindStatusCondition(task.Status.Conditions, kaalmv1beta1.ConditionReady); prev == nil ||
		prev.Reason != kaalmv1beta1.ReasonChildConflict || prev.Message != msg {
		r.setTaskReady(task, false, kaalmv1beta1.ReasonChildConflict, msg)
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(task, corev1.EventTypeWarning, kaalmv1beta1.ReasonChildConflict, msg)
	}
	return heldRequeue(task, r.notReadyRecheckInterval()), nil
}

// heldRequeue is the requeue for a task held by a child it cannot write:
// interval (childBlocked passes notReadyRecheckInterval), or a running task's
// timeout deadline when that comes first, so the hold never delays the
// timeout.
func heldRequeue(task *kaalmv1beta1.AgentTask, interval time.Duration) ctrl.Result {
	if task.Status.Phase == kaalmv1beta1.TaskRunning {
		if d := runningRequeue(task).RequeueAfter; d > 0 && d < interval {
			return ctrl.Result{RequeueAfter: d}
		}
	}
	return ctrl.Result{RequeueAfter: interval}
}

// runningRequeue schedules the next pass at the deadline of the effective
// timeout, when there is one.
func runningRequeue(task *kaalmv1beta1.AgentTask) ctrl.Result {
	d := taskTimeout(task)
	if d <= 0 || task.Status.StartTime == nil {
		return ctrl.Result{}
	}
	remaining := time.Until(task.Status.StartTime.Add(d))
	if remaining < time.Second {
		remaining = time.Second
	}
	return ctrl.Result{RequeueAfter: remaining}
}

// taskTimeout is the effective completion timeout: the current spec within
// the class bounds copied when the current Pod was created, so an owner's
// edit applies at once and a class edit never reaches the task.
func taskTimeout(task *kaalmv1beta1.AgentTask) time.Duration {
	return effectiveTaskTimeout(task, task.Status.ClassBounds)
}

// taskTTL is taskTimeout for ttlSecondsAfterFinished.
func taskTTL(task *kaalmv1beta1.AgentTask) *int32 {
	return effectiveTaskTTL(task, task.Status.ClassBounds)
}

// timedOut reports whether a started task has run past its effective
// timeout.
func timedOut(task *kaalmv1beta1.AgentTask) bool {
	d := taskTimeout(task)
	return d > 0 && task.Status.StartTime != nil && time.Since(task.Status.StartTime.Time) > d
}

func podExitMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return fmt.Sprintf("container %s exited %d", cs.Name, cs.State.Terminated.ExitCode)
		}
	}
	return "task Pod failed without a container exit code"
}

// isTerminalTaskPhase covers the phases that only wait for TTL. Failed is
// terminal too once settle() has set completionTime; an unfinished retry
// (Failed with no completionTime) is filtered before this check in Reconcile.
func isTerminalTaskPhase(p kaalmv1beta1.AgentTaskPhase) bool {
	switch p {
	case kaalmv1beta1.TaskSucceeded, kaalmv1beta1.TaskFailed,
		kaalmv1beta1.TaskTimedOut, kaalmv1beta1.TaskTerminating:
		return true
	}
	return false
}

// reconcileDelete implements the task finalizer: gracefully terminate the Pod
// if one exists; everything else is owner-referenced and cascade-GCed.
func (r *AgentTaskReconciler) reconcileDelete(ctx context.Context, task *kaalmv1beta1.AgentTask) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(task, kaalmv1beta1.TaskFinalizer) {
		return ctrl.Result{}, nil
	}
	pod, err := r.ownedTaskPod(ctx, task)
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
	controllerutil.RemoveFinalizer(task, kaalmv1beta1.TaskFinalizer)
	return ctrl.Result{}, r.Update(ctx, task)
}

func (r *AgentTaskReconciler) setTaskPhase(task *kaalmv1beta1.AgentTask, phase kaalmv1beta1.AgentTaskPhase) {
	task.Status.Phase = phase
}

func (r *AgentTaskReconciler) setTaskReady(task *kaalmv1beta1.AgentTask, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type: kaalmv1beta1.ConditionReady, Status: status, Reason: reason, Message: msg,
	})
}

// markTaskNotReady sets Ready=False for a reconcile-time validation failure,
// writes the status, and emits a Warning event with the same reason when the
// reason first appears, not on each pass that finds the problem again. The
// event follows a successful write, so a pass that lost its write to a
// conflict does not report the reason twice.
func (r *AgentTaskReconciler) markTaskNotReady(
	ctx context.Context, task *kaalmv1beta1.AgentTask, reason, msg string,
) error {
	first := readyFalseIsNew(task.Status.Conditions, reason)
	r.setTaskReady(task, false, reason, msg)
	if err := r.Status().Update(ctx, task); err != nil {
		return err
	}
	if first && r.Recorder != nil {
		r.Recorder.Event(task, corev1.EventTypeWarning, reason, msg)
	}
	return nil
}

// SetupWithManager wires the reconciler, its owned children (including the
// completion mailbox and its RBAC pair), and the AgentClass map-func watch.
func (r *AgentTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&kaalmv1beta1.AgentTask{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&cmapi.Certificate{}).
		// Tasks read a class's spec only; its in-use counts must not fan out.
		Watches(&kaalmv1beta1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(r.tasksForClass),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

func (r *AgentTaskReconciler) tasksForClass(ctx context.Context, obj client.Object) []reconcile.Request {
	var tasks kaalmv1beta1.AgentTaskList
	if err := r.List(ctx, &tasks, client.MatchingFields{IndexAgentClassRef: obj.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(tasks.Items))
	for _, t := range tasks.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: t.Namespace, Name: t.Name}})
	}
	return reqs
}
