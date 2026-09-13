# Agent lifecycle

An Agent is a persistent workload: the controller provisions a Pod for it and keeps that Pod alive until the Agent goes idle, hibernates, or is deleted. `status.phase` is the single field that says where an Agent is in that lifecycle, and it takes one of ten values: `Pending`, `Provisioning`, `Running`, `Idle`, `Hibernating`, `Hibernated`, `Resuming`, `Degraded`, `Failed`, `Terminating`.

The happy path is a straight line: an Agent is admitted (`Pending`), its child resources are built (`Provisioning`), and its Pod reports Ready (`Running`). Everything else on this page is a branch off that line, in three groups:

- **The idle and hibernate cycle.** An Agent with no traffic drops to `Idle`, then optionally to `Hibernated` (Pod deleted, PVC kept), and comes back through `Resuming` when a message arrives.
- **Re-provisioning.** Spec drift or an involuntary Pod disruption sends a live Agent back through `Provisioning` for a Pod replacement.
- **Trouble.** `Degraded` for a mismatch the developer can fix, `Failed` for a Pod that cannot run, `Terminating` for deletion.

This page is the state machine itself: the figure, the table of every transition and its trigger, and the mechanics of the Degraded and Failed phases. The mechanics behind the other transitions live on their own pages: [Activity detection](hibernation-and-wake.md#activity-detection) (how the controller knows an Agent is idle), [Hibernation mechanics](hibernation-and-wake.md#hibernation-mechanics), [Wake trigger](hibernation-and-wake.md#wake-trigger), and [AgentClass change handling](change-propagation.md#agentclass-change-handling) (how a class edit propagates to provisioned Agents). For what the reconciler does on each pass, see [AgentReconciler](reconcilers.md#agentreconciler).

## State diagram

The other lifecycles in the system are indexed on [Lifecycles at a glance](../appendix/lifecycles.md).

![Agent state machine. Pending to Provisioning on Certificate created, Provisioning to Running on Pod Ready, Running to Idle when idleTimeout elapses, Idle back to Running on activity observed, Idle to Hibernating when hibernationDelay elapses, Hibernating to Hibernated when the Pod is gone, Hibernated to Resuming on the wake annotation, and Resuming to Provisioning when the Pod is created. Running and Idle return to Provisioning on spec drift or Pod disruption. From any phase: Degraded on a class mismatch, returning to that phase when the mismatch clears; Failed on a crash loop or image pull failure, returning to Provisioning when the Pod recovers or is replaced; Terminating when deleted.](../diagrams/agent-lifecycle.svg)

## Transition triggers

The table lists every transition in the order the reconciler evaluates them. Rows whose behavior needs more than a sentence are expanded in the sections below it.

| From | To | Trigger |
|---|---|---|
| `Pending` | `Pending` (holds) | A Ready gate blocks: the operator namespace, a missing AgentClass, no image, a missing `existingClaim`, a missing `imagePullSecret`, or a missing handler ConfigMap. `Ready=False` names the gate. |
| `Pending` | `Provisioning` | The per-Agent `Certificate` exists and the controller is waiting for it, or the Pod has been created. See [Waiting on the Certificate](#waiting-on-the-certificate). |
| `Provisioning` | `Running` | The Pod reports Ready. |
| `Running` | `Idle` | No activity for `idleTimeout`, measured from the later of the gateway's activity record and `status.lastActivityTime`. See [Activity detection](hibernation-and-wake.md#activity-detection). |
| `Idle` | `Running` | Activity newer than `status.lastActivityTime` is observed. A Ready Pod alone does not promote an Idle Agent. |
| `Idle` | `Hibernating` | `hibernationEnabled` and silence for `idleTimeout` plus `hibernationDelay`. |
| `Hibernating` | `Hibernated` | The Pod is deleted and gone. The PVC, Service, Certificate, ServiceAccount, and NetworkPolicy remain; `status.podName` is cleared and `status.hibernatedAt` set. |
| `Hibernated` | `Resuming` | The `kaalm.io/wake: "true"` annotation, set by the gateway [activator](../gateways/user/activation-and-activity.md#the-activator) on a channel message, or by hand. The annotation on an Agent in any other phase is removed with a `WakeIgnored` warning, silently in `Resuming`. |
| `Resuming` | `Provisioning` | As shipped, the Pod is created on the next pass and the phase follows it: `Resuming` lasts one reconcile, and the Agent reaches `Running` through `Provisioning`. |
| `Running`, `Idle` | `Provisioning` | Spec drift: the stamped Pod spec hash differs from the re-derived one, so the Pod is replaced. Drift is detected in both phases, since an idle Agent still has a Pod. See [Spec change handling](change-propagation.md#spec-change-handling). |
| `Running`, `Idle` | `Provisioning` | Involuntary Pod disruption: the Pod was deleted out of band, or is present but terminal. See [Involuntary Pod disruption](#involuntary-pod-disruption). |
| any | `Degraded` | A class-versus-spec mismatch, present at first provisioning or introduced by class, ModelProvider, or ToolProvider drift. The check runs before every other step, so `Hibernated` and `Pending` Agents degrade too. See [Degraded](#degraded). |
| `Degraded` | the phase recorded in `status.preDegradedPhase` | Every outstanding mismatch has cleared. |
| any | `Failed` | A container in `CrashLoopBackOff` with five or more restarts, or in `ImagePullBackOff`. See [Failed](#failed). |
| `Failed` | `Provisioning` or `Running` | The Pod recovers, or a spec change replaces it. |
| any | `Terminating` | Deletion requested. `status.preDegradedPhase` is cleared; the finalizer is specified under [Finalizers](finalizers.md). |

### Waiting on the Certificate

Provisioning waits on the per-Agent `Certificate` before creating the Pod, so the Pod never hangs on a missing projected Secret. While cert-manager issues it the phase is `Provisioning` with `Ready=False, reason=CertificateNotReady`, requeued every five seconds. See [AgentReconciler](reconcilers.md#agentreconciler) step 7.

### Involuntary Pod disruption

An Agent in `Running` or `Idle` returns to `Provisioning` when its Pod goes away or dies without the kubelet bringing it back. Two cases:

- **Deleted out of band**: node drain or the eviction API, a manual `kubectl delete`, or node loss followed by Pod garbage collection. The owned-Pod watch fires and the reconciler creates a replacement.
- **Present but terminal**: node-pressure eviction leaves the Pod object at `status.phase: Failed`, `reason: Evicted`. `restartPolicy: Always` does not resurrect it, because the kubelet restarts containers inside a live Pod, never a dead Pod, and the eviction increments no `restartCount`, so crash-loop detection never sees it. The reconciler emits a `PodDisrupted` warning, deletes the dead Pod, and re-enters `Provisioning`.

Agent Pods are bare Pods with no Deployment or ReplicaSet behind them: the reconciler is the self-healing loop. The PVC, Service, and Certificate are preserved exactly as in the spec-drift replacement. `Hibernated` needs no handling here because no Pod exists.

### Degraded

`Degraded` is the phase for a mismatch between the Agent's spec and the AgentClass and providers that admit it. The developer, not the controller, can fix it. The reconciler evaluates every mismatch together at the top of each pass, before the hibernation branch, the Ready gates, and the Pod, so a Degraded Agent's Pod is neither created nor deleted while it is Degraded: a Running Agent keeps running, and a Hibernated one stays asleep.

| `reason` | Rule | Mismatch |
|---|---|---|
| `ClassConstraintViolation` | 2 | `spec.image` not in the class's `image.allowedImages` |
| `ClassConstraintViolation` | 4, 5 | a `spec.providers` entry not in `allowedProviders`, not existing, or not admitting the Agent's namespace |
| `ClassConstraintViolation` | 35 to 37 | a `spec.tools` entry not resolving, not admitting the namespace, or not in `allowedToolProviders` |
| `ToolNotInCatalog` | 38 | a granted tool name outside the ToolProvider's declared catalog |
| `PersistenceNotAllowed` | 24 | `spec.persistence.enabled: true` while the class has `persistence.enabled: false` |
| `HibernationNotAllowed` | 26 | `spec.lifecycle.hibernationEnabled: true` while the class has `lifecycle.hibernationAllowed: false` |
| `HibernationRequiresPersistence` | 29 | `spec.lifecycle.hibernationEnabled: true` while the Agent's own `spec.persistence.enabled` is `false` |
| `HandlerMountNotAllowed` | 30 | `spec.handler` set while the class has `image.allowHandlerMounts: false` |

The rules are specified under [Cross-resource validation](../resources/validation-and-defaulting.md#cross-resource-validation). A mismatch may be present at first provisioning (a developer applies an Agent that already violates its class) or introduced later by class, ModelProvider, or ToolProvider drift on a provisioned Agent; both are the same transition, and [AgentClass change handling](change-propagation.md#agentclass-change-handling) covers which class edits cause it. Rule 29 is spec-internal, so no class edit can introduce it.

**Entering.** On the first transition into `Degraded` the controller records the current phase in `status.preDegradedPhase`, emits a `Warning` event with the first outstanding reason, and sets `Ready=False` with that reason and message. If a further mismatch arises while the Agent is already `Degraded`, only `reason` and `message` are updated; `preDegradedPhase` is preserved, so the Agent still remembers where it came from.

**Leaving.** Recovery is per mismatch. When the reported mismatch clears and others remain, `reason` swaps to the next one without leaving `Degraded`. Once every mismatch has cleared, the controller restores `status.phase` from `preDegradedPhase` and clears `preDegradedPhase` in the same status write, so a later entry cannot reuse a stale value. An empty `preDegradedPhase` restores `Pending`.

The idle clock is not reset. Idleness is evaluated against the gateway's activity record, which runs through the Degraded period, so an Agent restored to `Idle` whose `hibernationDelay` has elapsed moves to `Hibernating` on the next pass.

**Not everything bad is a phase change.** Recoverable runtime issues (a transient provider outage, budget exhaustion) set a `Degraded` condition on the Agent without touching `status.phase`. See [Error handling](operations.md#error-handling).

### Failed

`Failed` is the phase for a Pod that cannot run. The reconciler sets it when a container status shows `CrashLoopBackOff` with a `restartCount` of five or more, or `ImagePullBackOff` at any count, and `Ready=False` carries the kubelet's reason and message. A missing image pull Secret never reaches `Failed`: it is caught as a Ready gate before the Pod is created.

`Failed` is derived from the Pod on every pass, so it clears itself: a container that stops crashing, or an image that becomes pullable, returns the Agent to `Running` when the Pod reports Ready. A spec change that alters the Pod spec replaces the Pod through `Provisioning` in the usual way.
