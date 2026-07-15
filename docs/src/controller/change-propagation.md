# Change Propagation

Two kinds of change reach an Agent's already-provisioned child resources: edits to the Agent's own spec, and edits to the AgentClass or ModelProvider it references. This page defines both paths. It is the canonical description of AgentClass-change propagation; the [AgentReconciler](reconcilers.md#agentreconciler) drift step and the [child-resources overview](../runtime/child-resources.md) both defer here.

## Spec Change Handling

When a developer updates an Agent's spec while it is in `Running` or `Idle` phase, the controller detects spec drift by hash comparison. It hashes the desired Pod spec, derived from the current Agent spec and AgentClass, and compares it against the hash stamped as an annotation on the existing Pod at creation time. This is the Deployment `pod-template-hash` idiom.

The comparison is never made against the live Pod object itself. The apiserver defaults and injects fields on admission (`serviceAccountName`, `nodeName`, tolerations, `imagePullPolicy`), so a naive deep-equal of derived spec vs. live spec always reports drift and would recreate the Pod in a loop.

The hash covers the replacement-triggering fields: image, resources, command, args, env, provider wiring. Not all of these are immutable in Kubernetes. A Pod's `image` is mutable in place, and `resources` is in-place-resizable on newer clusters. Agentry deliberately replaces the Pod on any such change for a clean process restart.

On a hash mismatch, the controller recreates the Pod:

1. The Agent transitions to `Provisioning`.
2. The existing Pod is deleted (SIGTERM is sent; the agent has `terminationGracePeriodSeconds` to shut down).
3. The controller creates a new Pod with the updated spec.
4. Once the new Pod is Ready, the Agent transitions back to `Running`.

The PVC, Service, and Certificate are preserved; only the Pod is replaced. This is intentionally disruptive: the agent process restarts, but its persistent state (PVC) is retained.

Changes to mutable fields (labels, annotations, non-structural metadata) are patched in place without Pod recreation.

If the Agent is `Hibernated`, spec changes are applied on the next wake. No Pod exists to recreate.

### Effect by Phase

For non-`Running`/`Idle` phases, the AgentReconciler's converge pass (see [step 6](reconcilers.md#agentreconciler)) picks up spec edits on every reconcile, but the user-visible effect is bounded by the Agent's phase:

| Phase during edit | Effect |
|---|---|
| `Provisioning`, `Resuming` | Applied to the Pod being created. No recreate is needed since the Pod is not yet `Ready`. |
| `Hibernating` | Applies on the next wake's Pod creation. The in-progress hibernation completes first. |
| `Hibernated` | Applies on the next wake, as above. |
| `Degraded` | Evaluated on every reconcile. That is the recovery path: aligning the Agent or AgentClass spec is the documented way out of `Degraded` (see [AgentReconciler step 5](reconcilers.md#agentreconciler)). |

The user-facing contract is therefore: a spec edit's effect is guaranteed visible only after `status.phase` next stabilizes at `Running`, `Hibernated`, or, for class-recovery, the restored `preDegradedPhase`.

## AgentClass Change Handling

When an AgentClass field that affects already-provisioned child resources is changed (e.g., `resources.maxLimits` lowered, `security.podSecurityContext` tightened, `network.egress.allowedCIDRs` reduced, `image.allowedImages` narrowed, `allowedProviders` reduced), the [AgentReconciler](reconcilers.md#agentreconciler) and [AgentTaskReconciler](reconcilers.md#agenttaskreconciler) re-queue every Agent and AgentTask referencing the class via their existing `EnqueueRequestsFromMapFunc` watches. The AgentClass watch uses an indexed lookup on `agentClassRef.name`; a parallel ModelProvider watch uses an indexed lookup on `providerRef.name`. Propagation is therefore event-driven rather than waiting for the 5-minute periodic requeue.

A change propagates along one of three paths, depending on whether it constrains the derived Pod spec, excludes the Agent's stored spec, or only affects per-request routing.

![Decision tree for an AgentClass or ModelProvider spec edit. The edit re-queues every referencing workload through EnqueueRequestsFromMapFunc, the AgentClass watch indexed on agentClassRef.name and the ModelProvider watch indexed on providerRef.name. For an Agent, the first test asks whether the change excludes the Agent's stored spec; if so the result is bucket 2, phase=Degraded with preDegradedPhase recorded and a reason naming the mismatch. Otherwise a Pod-spec-hash comparison asks whether a replacement-triggering field changed; if not the result is bucket 3, a routing-concern change handled by the gateway's watches with no Agent-side effect. If it did, the result is bucket 1, recreate-and-clamp: a Hibernated Agent defers to its next wake, otherwise the Agent goes to Provisioning, the Pod is deleted with SIGTERM, a new Pod is created from the clamped spec, and the Agent returns to Running. For an AgentTask, terminal tasks ignore class drift and proceed to TTL cleanup, in-flight tasks run to completion under the class snapshot taken at Pod creation, and a task retrying from Failed increments status.retries and re-validates against the new class, reaching Failed with reason=ClassConstraintViolation if it is no longer admitted.](../diagrams/agentclass-propagation.svg)

Reading the diagram: the order of the two Agent-side tests is the whole design. The exclusion test runs first, and only if it passes does the field-category question arise. This is why a restrictive `allowedNamespaces` edit lands in bucket 2 even though `allowedNamespaces` is listed under bucket 3: bucket membership follows what the change does to the stored spec, not what kind of field it is.

### Bucket 1: Recreate-and-Clamp (Default)

If the new AgentClass invariants can be applied by re-deriving the desired Pod spec, the controller does so without any phase excursion beyond a normal restart. Examples: `maxLimits` lowered, `runtime` changed, `security` tightened, `network.egress.allowedCIDRs` reduced, `image.allowedImages` narrowed but still admitting the Agent's `image`.

The reconciler re-derives the [child-resource set](../runtime/child-resources.md) for every Agent referencing the class: clamping `resources.limits` down to the new `maxLimits`, applying the tighter `securityContext`, regenerating the per-Agent `NetworkPolicy` from the new egress rules. It then transitions the Agent to `Provisioning`, deletes the existing Pod (graceful SIGTERM honoring `terminationGracePeriodSeconds`), and creates a new Pod with the adjusted spec. The PVC, Service, and Certificate are preserved. This mirrors the [spec-drift behavior above](#spec-change-handling); agents must tolerate restart.

Drift is detected exactly as for Agent spec edits: by comparing the Pod-spec hash annotation against the hash of the re-derived spec, never by a DeepEqual against the live Pod. Recreation triggers when the new spec differs in the replacement-triggering fields (image, resources, command, args, env, provider wiring); Agentry deliberately replaces the Pod even for fields that are technically mutable in place.

This design makes AgentClass a live policy lever for Agents without disrupting one-shot task work (see [AgentTask handling](#agenttask-handling-no-degraded-phase) below).

Hibernated Agents apply the new invariants on their next wake. Recreation happens automatically as the wake path provisions a new Pod from the now-clamped desired spec.

### Bucket 2: Degrade-When-Irreconcilable

Four kinds of change exclude the Agent's spec rather than just constrain its derived Pod spec:

1. The Agent's `spec.image` no longer matches `image.allowedImages`.
2. Its `spec.providers` references a ModelProvider no longer in `allowedProviders`, or one whose own `allowedNamespaces` no longer includes the Agent's namespace. A referenced ModelProvider that has been deleted outright is handled the same way (see [AgentReconciler step 2](reconcilers.md#agentreconciler)).
3. Its `spec.persistence.enabled: true` while the class has `persistence.enabled: false`.
4. Its `spec.lifecycle.hibernationEnabled: true` while the class has `lifecycle.hibernationAllowed: false`.

In these cases the reconciler does not recreate the Pod. It transitions the Agent to `phase=Degraded` with a `reason` naming the specific mismatch and a message naming the offending field: `ClassConstraintViolation` for image, provider, or namespace mismatches, `PersistenceNotAllowed` for persistence, `HibernationNotAllowed` for hibernation. One spec-internal coupling is handled the same way: `spec.lifecycle.hibernationEnabled: true` while the Agent's own `spec.persistence.enabled` is `false` gets `reason=HibernationRequiresPersistence`. These checks reuse [cross-resource validation](../resources/validation-and-defaulting.md#cross-resource-validation) rules 2, 4, 5, 24, and 26, plus rule 29 for the hibernation-without-persistence coupling. They fail at reconcile and surface as `Degraded` rather than being silently re-applied to a recreated Pod.

The same `Degraded` handling applies whether the conflict is discovered during initial provisioning (a developer applies an Agent that already violates the referenced class) or via class or ModelProvider drift on an already-running Agent. The developer must update the Agent spec to comply; the controller resumes normal operation on the next reconcile after the Agent spec is reconciled with the class.

Note the boundary with the [recoverable error bucket](operations.md#error-handling): recoverable runtime issues (transient provider unhealthy, budget exhaustion) set a `Degraded` *condition* on the Agent without changing `status.phase`. A ModelProvider's `allowedNamespaces` removing the Agent's namespace is not in that recoverable bucket; it is a class-vs-spec mismatch handled via `phase=Degraded` under this bucket.

#### preDegradedPhase Mechanics

On the first transition into `Degraded` from a non-Degraded phase, the controller records the current phase in `status.preDegradedPhase`. If a new Degraded-triggering condition arises while the Agent is already in `Degraded`, only `reason` and `message` are updated and `preDegradedPhase` is preserved.

Recovery is per-condition. The controller clears the current `reason` when its condition resolves and, if other conditions remain outstanding, swaps `reason` to the next one without leaving `Degraded`. The Agent only exits `Degraded` once every outstanding Degraded-triggering condition has cleared. On exit, `status.phase` is restored from `preDegradedPhase` and `preDegradedPhase = null` is set atomically in the same status write, so a subsequent transition into `Degraded` cannot reuse a stale value.

The idle clock is not reset. The controller evaluates idleness against the gateway's activity timestamp, which is continuous through the Degraded period. If the pre-degradation phase was `Idle` and `hibernationDelay` has since elapsed, the agent transitions to `Hibernating` on the next reconcile.

#### Operational Example

[Scenario S5](../appendix/scenarios.md) shows this bucket in production terms. When a platform admin removes a team's namespace from a ModelProvider's `allowedNamespaces`, two things happen: the gateway denies the namespace's next LLM call, and the controller, re-queued event-driven via its ModelProvider watch, transitions the affected Agents to `Degraded, reason=ClassConstraintViolation`, so the revocation is visible in `kubectl get agents`. The Pods keep running, but LLM access is gone.

### Bucket 3: Routing-Concern Changes

Routing-concern fields propagate via the gateway's CRD and Secret watches with no Agent-side effect: `spec.models` shrinkage, fallback-chain edits, credential rotations on `credentialsRef` (ModelProvider), and *additive* changes to `allowedNamespaces` or `allowedProviders` that do not exclude any currently-bound provider or namespace. These take effect on the next routed call without any Pod-level transition. See [LLM Gateway](../gateways/llm/overview.md) for the per-request routing and credential mechanics.

Restrictive changes to `allowedNamespaces` or `allowedProviders` that exclude a currently-bound provider or namespace fall under bucket 2's `Degraded` handling, not this one. Bucket membership depends on whether the change excludes a stored spec value, not on the field's nominal category.

### AgentTask Handling (No Degraded Phase)

AgentTask has no `Degraded` phase. Where an Agent would degrade, the task transitions to `phase=Failed` with the same `reason` instead; the persistence cross-check, for example, yields `Failed, reason=PersistenceNotAllowed`.

AgentTasks are not subject to mid-execution recreation. In-flight tasks (`Running` or `Provisioning`) finish under the class snapshot in effect at task-Pod creation time, and the new invariants take effect only when the task next provisions a Pod. That next event is either a backoff retry from `Failed` (where standard validation against the new class runs; if the spec no longer admits, the retry is rejected and the task is marked `Failed, reason=ClassConstraintViolation`) or a subsequent AgentTask CR. Tasks already in `Succeeded`, `Failed`, or `TimedOut` are unaffected; terminal-state tasks ignore class drift and proceed to TTL-based cleanup.

One retry-accounting nuance: `status.retries` increments at the start of each retry cycle, before the pre-Pod cross-check runs. A retry whose new Pod fails the cross-check therefore consumes one unit of [`backoffLimit`](../resources/agenttask.md) even though the failure cause is admin misconfiguration of the AgentClass or ModelProvider rather than the workload. Operators that have aligned the class spec mid-backoff and want a clean retry should delete and recreate the AgentTask. A `kubectl apply` of the same or modified spec does not reset `status.retries`, since status is controller-owned and Kubernetes apply patches only `spec`. The new AgentTask starts at `status.retries = 0` against the now-aligned class.

### Bulk Impact and Staged Rollouts

Tightening AgentClass policy on a class with many Agents triggers a rolling Pod restart of every affected Agent that falls into the recreate-and-clamp path. In-flight AgentTasks are not part of this rollout. Platform teams that need staged rollouts should split tightening across multiple AgentClasses (e.g., `standard-v2`) and migrate Agents incrementally rather than mutating an in-use class.
