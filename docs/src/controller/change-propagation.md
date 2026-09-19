# Change propagation

Two kinds of change reach an Agent's provisioned child resources: edits to the Agent's own spec, and edits to the AgentClass, ModelProvider, or ToolProvider it references. This page defines both paths. It is the canonical description of class-change propagation; the [AgentReconciler](reconcilers.md#agentreconciler) and the [child resources](../runtime/child-resources.md) page both defer here.

## Spec change handling

When a developer edits an Agent's spec, the controller detects drift by hash comparison. It hashes the Pod spec it would derive from the current Agent spec and class, and compares the result with the hash stamped as the `kaalm.io/pod-spec-hash` annotation on the existing Pod at creation time, the Deployment `pod-template-hash` idiom. The comparison is never made against the live Pod object: the apiserver defaults and injects fields on admission (`serviceAccountName`, `nodeName`, tolerations, `imagePullPolicy`), so a deep-equal against the live object would report drift on every pass and recreate the Pod in a loop.

The hash covers the image, command, args, env, resources, the provider names, and the handler ConfigMap name (never its content). Kaalm replaces the Pod on any change to these, for a clean process restart, even where Kubernetes would allow an in-place change such as an image or, on clusters with in-place Pod resize, resources. On a hash mismatch the Agent transitions to `Provisioning`, the Pod is deleted with its `terminationGracePeriodSeconds`, and a new Pod is created from the new spec. The PVC, Service, Certificate, ServiceAccount, and NetworkPolicy are preserved.

Fields outside the hash are never applied to a live Pod: the reconciler creates, deletes, and reads Pods, and never patches one. A change to such a field takes effect when the Pod is next replaced for another reason. The Service and the NetworkPolicy are converged in place, so a `spec.service.port` change or a class egress change reaches a running Agent without a restart.

### Effect by phase

| Phase during edit | Effect |
|---|---|
| `Running`, `Idle` | The Pod is replaced on the next pass. |
| `Provisioning`, `Resuming` | A Pod that already exists is replaced like any other, since the hash check runs before the readiness check. An edit that lands before the Pod is created is absorbed into it. |
| `Hibernating`, `Hibernated` | Applied on the next wake, which creates the Pod from the new spec. The in-progress hibernation completes first. |
| `Degraded` | The cross-checks re-run on every pass, so aligning the Agent or class spec is the way out of `Degraded`; see [Degraded](agent-lifecycle.md#degraded). |

A spec edit's effect is guaranteed visible only after `status.phase` next settles at `Running`, `Hibernated`, or, for class recovery, the restored `preDegradedPhase`.

## AgentClass change handling

When an AgentClass, ModelProvider, or ToolProvider spec changes, the [AgentReconciler](reconcilers.md#agentreconciler) re-enqueues every Agent referencing it through three indexed watches (`agentClassRef.name`, `providers[].providerRef.name`, `tools[].providerRef.name`), so propagation is event-driven rather than waiting for a periodic requeue. The [AgentTaskReconciler](reconcilers.md#agenttaskreconciler) watches AgentClass only. Each watch is gated so that status-only writes never fan out: a class or tool provider fires on a spec generation change, and a model provider on a spec change or a change to the set of namespaces its budget blocks.

A change propagates along one of three paths, decided in this order: does it exclude the Agent's stored spec (bucket 2), does it change the Pod spec hash (bucket 1), or neither (bucket 3, or an in-place child update).

![Flowchart of what a class or provider edit does to a provisioned Agent, as one cascade. If the stored spec is no longer admitted by the class and providers, the Agent becomes Degraded with the Pod untouched. Otherwise, if the Pod spec hash is unchanged, the children are converged in place with no restart. Otherwise a Hibernated Agent applies the change on its next wake, and any other Agent goes to Provisioning, where the Pod is deleted and created from the new spec.](../diagrams/agentclass-propagation.svg)

The exclusion test runs first, so bucket membership follows what the change does to the stored spec, not the field's nominal category: a restrictive `allowedNamespaces` edit is bucket 2 even though `allowedNamespaces` is listed under bucket 3.

### Bucket 1: recreate-and-clamp (default)

A class change that reaches the Pod spec hash replaces the Pod: `maxLimits` lowered (the Agent's `resources.limits` are clamped to it), `defaultImage` changed for an Agent with no image of its own, or `defaultResources` changed for an Agent with none. The reconciler transitions the Agent to `Provisioning`, deletes the Pod gracefully, and creates a new one from the clamped spec; a `Hibernated` Agent applies the change on its next wake. Agents must tolerate restart.

Class fields outside the hash reach a running Agent by other routes or not at all:

| Class change | Effect on a provisioned Agent |
|---|---|
| `network.egress.allowedCIDRs`, `allowSameNamespaceIngress` | the NetworkPolicy is updated in place; no restart |
| `image.allowedImages` narrowed but still admitting the image | none |
| `security`, `runtime.runtimeClassName`, `image.pullPolicy`, `image.imagePullSecrets`, `lifecycle.terminationGracePeriodSeconds`, `podMetadata` | as shipped, none until the Pod is next replaced for another reason: these fields are derived into a new Pod but not hashed |
| `lifecycle` defaults and caps | applied on the next activity evaluation; no restart |

### Bucket 2: degrade-when-irreconcilable

Some changes exclude the Agent's spec rather than constrain its derived Pod spec. The reconciler does not touch the Pod for these; it moves the Agent to `phase=Degraded` with a `reason` naming the mismatch and a message naming the offending field. A class or provider change can newly introduce any of these:

1. The Agent's `spec.image` no longer matches `image.allowedImages` (rule 2).
2. A `spec.providers` entry is no longer in `allowedProviders`, has been deleted, or its own `allowedNamespaces` no longer includes the Agent's namespace (rules 3 to 5). Rule 4, a provider dropping the namespace, is the canonical case ([scenario S5](../appendix/scenarios.md)).
3. A `spec.tools` entry is no longer in `allowedToolProviders`, its ToolProvider no longer admits the namespace or has been deleted, or a granted tool has left the declared catalog (rules 35 to 38).
4. `spec.persistence.enabled: true` while the class has `persistence.enabled: false` (rule 24).
5. `spec.lifecycle.hibernationEnabled: true` while the class has `lifecycle.hibernationAllowed: false` (rule 26).
6. `spec.handler` set while the class has `image.allowHandlerMounts: false` (rule 30).

The reasons, the `preDegradedPhase` bookkeeping, per-mismatch recovery, and what happens to the Pod meanwhile are specified under [Degraded](agent-lifecycle.md#degraded). Either side of the mismatch can be aligned, the workload spec or the class or provider, and the controller restores the prior phase on the next pass after every mismatch has cleared. Recoverable runtime issues (a transient provider outage, budget exhaustion) are a different bucket: they set a `Degraded` condition without changing the phase, see [Error handling](operations.md#error-handling).

### Bucket 3: routing-concern changes

Routing-concern fields propagate through the gateway's CRD and Secret watches with no Agent-side effect: `spec.models` shrinkage, fallback-chain edits, credential rotations on `credentialsRef`, and additive changes to `allowedNamespaces` or `allowedProviders` that exclude no bound provider or namespace. These take effect on the next routed call without any Pod-level transition. See [LLM Gateway](../gateways/llm/overview.md) for the per-request routing and credential mechanics.

### AgentTask handling (no Degraded phase)

AgentTask has no `Degraded` phase. Where an Agent would degrade, the task settles terminal `Failed` with the same reason.

| Task state at the edit | Effect |
|---|---|
| has a Pod (`Provisioning` with a Pod, `Running`, `Completing`) | none; the task finishes under the class snapshot its Pod was created from |
| no Pod yet (`Pending`, or `Provisioning` before creation), or retrying from `Failed` | the pre-Pod class check runs against the new class; a violation settles the task `Failed` at once, whatever `backoffLimit` remains |
| terminal (`Succeeded`, `Failed`, `TimedOut`) | none; the task proceeds to TTL cleanup |

A retry spends its `status.retries` increment before the check runs and does not get it back. To retry against a class you have since aligned, delete and recreate the task; a `kubectl apply` of the same spec does not reset `status.retries`, since status is controller-owned and apply patches only `spec`. Only an AgentClass change re-enqueues tasks; a ModelProvider or ToolProvider change is picked up at the task's next pass.

### Bulk impact

Tightening a class with many Agents re-enqueues every one of them at once, and the restarts run concurrently up to `controller.maxConcurrentReconciles` (default 4). There is no ordering, pacing, or max-unavailable bound. Platform teams that need a staged rollout split the tightening across classes (`standard-v2`, say) and migrate Agents incrementally rather than editing an in-use class.
