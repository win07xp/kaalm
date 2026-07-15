# Agent Lifecycle

An Agent is a persistent workload: the controller provisions a Pod for it and keeps that Pod alive until the Agent goes idle, hibernates, or is deleted. `status.phase` is the single field that says where an Agent is in that story, and it takes one of ten values: `Pending`, `Provisioning`, `Running`, `Idle`, `Hibernating`, `Hibernated`, `Resuming`, `Degraded`, `Failed`, `Terminating`.

The happy path is a straight line: an Agent is admitted (`Pending`), its child resources are built (`Provisioning`), and its Pod reports Ready (`Running`). Everything else in this page is a branch off that line, and the branches fall into three groups:

- **The idle/hibernate cycle.** An Agent with no traffic drops to `Idle`, then optionally to `Hibernated` (Pod deleted, PVC kept), and comes back through `Resuming` when a message arrives.
- **Re-provisioning.** Spec drift or an involuntary Pod disruption sends a live Agent back through `Provisioning` for a Pod replacement.
- **Trouble.** `Degraded` for a mismatch the developer can fix, `Failed` for one they cannot, `Terminating` for deletion.

This page is the state machine itself: the diagram, and the canonical table of every transition and its trigger. The mechanics that hang off individual transitions live on their own pages: [Activity Detection](hibernation-and-wake.md#activity-detection) (how the controller knows an Agent is idle), [Hibernation mechanics](hibernation-and-wake.md#hibernation-mechanics), [Wake trigger](hibernation-and-wake.md#wake-trigger), and [AgentClass change handling](change-propagation.md#agentclass-change-handling) (how a class edit propagates to already-provisioned Agents). For what the reconciler actually does on each pass, see [AgentReconciler](reconcilers.md#agentreconciler).

## State diagram

![Agent state machine: Pending to Provisioning to Running, with an idle and hibernate cycle through Idle, Hibernating, Hibernated, and Resuming back to Running. Spec drift or involuntary Pod disruption sends Running or Idle back to Provisioning, and from any phase an Agent can move to Degraded, Failed, or Terminating.](../diagrams/agent-lifecycle.svg)

## Transition triggers

This table is the canonical list. Rows whose behavior needs more than a sentence are expanded in the sections below it.

| From -> To | Trigger |
|---|---|
| Pending -> Provisioning | References validated, AgentClass resolved, per-Agent `Certificate` created |
| Provisioning -> Running | Per-Agent `Certificate` reaches `Ready=True`, Pod is created and reports Ready, Service endpoint populated. See [Waiting on the Certificate](#waiting-on-the-certificate). |
| Running -> Idle | `lastActivityTime` older than `idleTimeout` |
| Idle -> Running | Activity observed (see [Activity Detection](hibernation-and-wake.md#activity-detection)) |
| Idle -> Hibernating | Idle for `hibernationDelay` (defaults from AgentClass) AND `hibernationEnabled` |
| Hibernating -> Hibernated | Pod scaled to 0, PVC retained, Service remains |
| Hibernated -> Resuming | Gateway [Activator](../gateways/user/activation-and-activity.md#the-activator) calls `POST /v1/activate/{namespace}/{agentName}` on the controller (triggered by a channel message arriving via the User Gateway for this Agent), OR `agentry.io/wake: "true"` annotation (manual override) |
| Resuming -> Running | Pod becomes Ready |
| Running -> Provisioning | Spec drift (Agent or AgentClass) re-derives a Pod spec that differs in replacement-triggering fields. See [AgentClass change handling](change-propagation.md#agentclass-change-handling). |
| Running/Idle -> Provisioning | **Involuntary Pod disruption**: the Pod was deleted out-of-band, or is present but terminal without kubelet recovery. See [Involuntary Pod disruption](#involuntary-pod-disruption). |
| any -> Degraded | **AgentClass-vs-Agent-spec mismatch**, present at initial provisioning or introduced by class or ModelProvider drift on an already-running Agent. See [Entering Degraded](#entering-degraded). |
| Degraded -> {pre-degradation phase} | Underlying condition resolved. The controller restores the phase recorded in `status.preDegradedPhase`. See [Leaving Degraded](#leaving-degraded). |
| any -> Failed | Unrecoverable error (image pull failure after retries, invalid config, persistent crash loop) |
| any -> Terminating | Deletion requested |

### Waiting on the Certificate

Provisioning waits on the per-Agent `Certificate` before creating the Pod so the Pod never hangs on a missing projected Secret. See [AgentReconciler](reconcilers.md#agentreconciler) step 4.

### Involuntary Pod disruption

An Agent in `Running` or `Idle` returns to `Provisioning` when its Pod goes away or dies without the kubelet bringing it back. Two shapes:

- **Deleted out-of-band**: node drain / eviction API, manual `kubectl delete`, or node loss followed by Pod GC.
- **Present but terminal without kubelet recovery**: node-pressure eviction leaves the Pod object at `status.phase: Failed`, `reason: Evicted`. `restartPolicy: Always` does **not** resurrect it, because the kubelet restarts containers inside a live Pod, never a dead Pod. The eviction increments no `restartCount`, so crash-loop detection never sees it either.

Agent Pods are bare Pods, so there is no Deployment or ReplicaSet standing behind them: the reconciler *is* the self-healing loop. It deletes the dead Pod object if one remains and re-enters `Provisioning`, preserving the PVC, Service, and Certificate exactly as in the spec-drift recreate. Detection is event-driven via the owned-Pod watch.

Two phases need no handling here. `Hibernated` is unaffected because no Pod exists. `Resuming` already requeues on failed Pod creation.

### Entering Degraded

`Degraded` is the phase for a mismatch between the Agent's spec and the AgentClass that admits it. The developer, not the controller, is the one who can fix it. Four class-vs-spec mismatches trigger it:

- `spec.image` not in `image.allowedImages`.
- `spec.providers` references a ModelProvider not in `allowedProviders`, or one whose `allowedNamespaces` no longer includes the Agent's namespace.
- `spec.persistence.enabled: true` while the class has `persistence.enabled: false`.
- `spec.lifecycle.hibernationEnabled: true` while the class has `lifecycle.hibernationAllowed: false`.

Plus one spec-internal coupling handled the same way: `spec.lifecycle.hibernationEnabled: true` while the Agent's own `spec.persistence.enabled` is `false` ([rule 29](../resources/validation-and-defaulting.md#cross-resource-validation)).

The mismatch may be present at initial provisioning (a developer applies an Agent that already violates its class) or introduced later by class or ModelProvider drift on an already-running Agent. Both are the same transition.

The `reason` names the specific mismatch:

| `reason` | Mismatch |
|---|---|
| `ClassConstraintViolation` | image / provider / namespace |
| `PersistenceNotAllowed` | persistence |
| `HibernationNotAllowed` | hibernation |
| `HibernationRequiresPersistence` | hibernation without persistence |

**`preDegradedPhase` bookkeeping.** On the first transition into `Degraded` from a non-Degraded phase, the controller records the current phase in `status.preDegradedPhase`. If a new Degraded-triggering condition arises while the Agent is *already* in `Degraded`, only `reason` and `message` are updated: `preDegradedPhase` is preserved, so the Agent still remembers where it came from.

**Not everything bad is a phase change.** Recoverable runtime issues (transient provider unhealthy, budget exhaustion) set a `Degraded` *condition* on the Agent without touching `status.phase`. See [Error Handling](operations.md#error-handling). For the phase-transition path, see [Degrade-when-irreconcilable](change-propagation.md#agentclass-change-handling).

### Leaving Degraded

When the underlying condition resolves, the controller restores the phase the Agent was in before it degraded, tracked in `status.preDegradedPhase`.

Recovery is **per-condition**. When multiple Degraded-triggering conditions have arisen during a single Degraded period, the controller clears the current `reason` when its condition resolves and, if others remain outstanding, swaps `reason` to the next one without leaving `Degraded`. The Agent exits only once every outstanding condition has cleared.

The exit is a single atomic status write: `status.phase` is restored from `preDegradedPhase` and `preDegradedPhase` is set to `null` in the same write, so a subsequent `any -> Degraded` transition cannot reuse a stale value.

The idle clock is **not** reset. The controller evaluates idleness against the gateway's activity timestamp, which runs continuously through the Degraded period. So if the pre-degradation phase was `Idle` and `hibernationDelay` has since elapsed, the Agent transitions straight to `Hibernating` on the next reconcile rather than sitting `Idle` again.
