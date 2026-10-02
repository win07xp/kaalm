# Change propagation

Two kinds of change reach an Agent's provisioned child resources: edits to the Agent's own spec, and edits to the AgentClass, ModelProvider, or ToolProvider it references. This page defines both paths. It is the canonical description of class-change propagation; the [AgentReconciler](reconcilers.md#agentreconciler) and the [child resources](../runtime/child-resources.md) page both defer here.

## Spec change handling

When a developer edits an Agent's spec, the controller detects drift by hash comparison. It hashes the Pod spec it would derive from the current Agent spec and class, and compares the result with the hash the controller wrote to the existing Pod's `kaalm.io/pod-spec-hash` annotation when it created the Pod, the Deployment `pod-template-hash` idiom. The comparison is never made against the live Pod object: the apiserver defaults and injects fields on admission (`serviceAccountName`, `nodeName`, tolerations, `imagePullPolicy`), so a deep-equal against the live object would report drift on every pass and recreate the Pod in a loop.

The hash covers the image, command, args, env, resources, the provider names, and the handler ConfigMap name (never its content). It also covers every Pod input the AgentClass controls, as derived for the Pod: the Pod and container security contexts with the `restricted` baseline merged in, `automountServiceAccountToken`, `runtimeClassName`, the image pull policy and pull Secrets, the termination grace period, and the `podMetadata` labels and annotations. The hash leaves out the controller's own labels and the two hash annotations, because the controller sets them on every Pod whatever the class says. Kaalm replaces the Pod on any change to a hashed input, for a clean process restart, even where Kubernetes would allow an in-place change such as an image or, on clusters with in-place Pod resize, resources. On a hash mismatch the Agent transitions to `Provisioning`, the Pod is deleted with its `terminationGracePeriodSeconds`, and a new Pod is created from the new spec. The PVC, Service, Certificate, ServiceAccount, and NetworkPolicy are preserved.

Fields outside the hash are never applied to a live Pod: the reconciler creates, deletes, and reads Pods, and patches only the two hash annotations ([An upgrade that changes the hash formula replaces no Pod](#an-upgrade-that-changes-the-hash-formula-replaces-no-pod)). A change to such a field takes effect when the Pod is next replaced for another reason. The Service and the NetworkPolicy are converged in place, so a `spec.service.port` change or a class egress change reaches a running Agent without a restart.

### Effect by phase

| Phase during edit | Effect |
|---|---|
| `Running`, `Idle` | The Pod is replaced on the next pass. |
| `Provisioning`, `Resuming` | A Pod that already exists is replaced like any other, since the hash check runs before the readiness check. An edit that lands before the Pod is created is absorbed into it. |
| `Hibernating`, `Hibernated` | Applied on the next wake, which creates the Pod from the new spec. The in-progress hibernation completes first. |
| `Degraded` | The cross-checks re-run on every pass, so aligning the Agent or class spec is the way out of `Degraded`; see [Degraded](agent-lifecycle.md#degraded). |

A spec edit's effect is guaranteed visible only after `status.phase` next settles at `Running`, `Hibernated`, or, for class recovery, the restored `preDegradedPhase`.

### Drift replacements are capped per class

A drifted Agent does not replace its Pod the moment it detects drift. It first asks its class for one of the `lifecycle.maxUnavailableOnDrift` slots: an integer of at least 1, or a percentage of the class's Agents, hibernated ones included, from 1% to 100%, rounded up and never below 1. A class with no `lifecycle.maxUnavailableOnDrift`, including one with no `lifecycle` block at all, gets 25% ([rule 44](../resources/validation-and-defaulting.md#the-rules)).

A granted Agent's `PodUpToDate` condition becomes `False, reason=Replacing`, its old Pod is deleted, and a new one is created from the current spec. The slot frees after the new Pod is Ready on that spec (`PodUpToDate` turns `True, reason=Current`), or the Agent is deleted. A refused Agent's `PodUpToDate` condition becomes `False, reason=ReplacementPending`, and its current Pod keeps serving: `status.phase` and the `Ready` condition still come from that Pod. A refused Agent is re-queued as soon as another Agent of the class leaves `Replacing`, with a 30-second fallback retry. When both an Idle and a non-Idle Agent of the class wait for a slot, the Idle one is replaced first.

The cap applies to every drift replacement alike: an edit to an Agent's own spec and a class or provider edit that reaches the Pod spec hash ([Bucket 1](#bucket-1-recreate-and-clamp-default)) compete for the same slots. `AgentClass.status.agentsReplacing` and `status.agentsPendingReplacement` count the class's Agents on each side of the wait ([AgentClass](../resources/agentclass.md)). An Agent that reaches `Hibernated` while waiting for a slot stops waiting and drops out of `agentsPendingReplacement`, since hibernation clears its `PodUpToDate` condition; its wake creates the Pod from the current spec.

A Pod that never becomes Ready after a grant, because it crash-loops or cannot pull its image, sets the Agent `Failed` but keeps its slot, so a bad rollout halts at the cap instead of failing the whole class. A further spec change replaces that Agent's Pod at once, on the slot it already holds, and the rollout resumes from there.

### An upgrade that changes the hash formula replaces no Pod

Each Pod carries a second annotation, `kaalm.io/pod-spec-hash-version`, which records the version of the formula that produced its hash. A Pod without this annotation has a version 1 hash. When the reconciler finds such a Pod, it recomputes the hash with the version 1 formula and compares:

- If the hashes match, the Pod is current under the rules it was created with. The reconciler patches both annotations in place with the current formula's hash and version, and the Pod keeps running.
- If the hashes differ, the Pod has drifted, and the reconciler replaces it as usual.

As a result, an AgentClass edit made before the upgrade to a field that version 1 does not hash (`security`, `runtime.runtimeClassName`, the pull settings, the termination grace period, or `podMetadata`) does not reach a running Pod at upgrade time. The edit takes effect when the Pod is next replaced. An edit made after the upgrade replaces the Pod, as [Bucket 1](#bucket-1-recreate-and-clamp-default) describes.

## AgentClass change handling

When an AgentClass, ModelProvider, or ToolProvider spec changes, the [AgentReconciler](reconcilers.md#agentreconciler) re-enqueues every Agent referencing it through three indexed watches (`agentClassRef.name`, `providers[].providerRef.name`, `tools[].providerRef.name`), so propagation is event-driven rather than waiting for a periodic requeue. The [AgentTaskReconciler](reconcilers.md#agenttaskreconciler) watches AgentClass only. Each watch is gated so that status-only writes never fan out: a class or tool provider fires on a spec generation change, and a model provider on a spec change, a change to its `Ready` status or reason, or a change to the set of namespaces its budget blocks.

A change propagates along one of three paths, decided in this order: does it exclude the Agent's stored spec (bucket 2), does it change the Pod spec hash (bucket 1), or neither (bucket 3, or an in-place child update).

![Flowchart of what a class or provider edit does to a provisioned Agent, as one cascade. If the stored spec is no longer admitted by the class and providers, the Agent becomes Degraded with the Pod untouched. Otherwise, if the Pod spec hash is unchanged, the children are converged in place with no restart. Otherwise a Hibernated Agent applies the change on its next wake, and any other Agent goes to Provisioning, where the Pod is deleted and created from the new spec.](../diagrams/agentclass-propagation.svg)

The exclusion test runs first, so bucket membership follows what the change does to the stored spec, not the field's nominal category: a restrictive `allowedNamespaces` edit is bucket 2 even though `allowedNamespaces` is listed under bucket 3.

### Bucket 1: recreate-and-clamp (default)

A class change that reaches the Pod spec hash replaces the Pod, subject to the class's `maxUnavailableOnDrift` cap ([Drift replacements are capped per class](#drift-replacements-are-capped-per-class)): `maxLimits` lowered (the Agent's `resources.limits` are clamped to it), `defaultImage` changed for an Agent with no image of its own, `defaultResources` changed for an Agent with none, or any change to `security`, `runtime.runtimeClassName`, `image.pullPolicy`, `image.imagePullSecrets`, `lifecycle.terminationGracePeriodSeconds`, or `podMetadata`. Once an Agent holds a slot, the reconciler transitions it to `Provisioning`, deletes the Pod gracefully, and creates a new one from the clamped spec; a `Hibernated` Agent applies the change on its next wake. Agents must tolerate restart.

Class fields outside the hash reach a running Agent by other routes or not at all:

| Class change | Effect on a provisioned Agent |
|---|---|
| `network.egress.allowedCIDRs`, `allowSameNamespaceIngress` | the NetworkPolicy is updated in place; no restart |
| `image.allowedImages` narrowed but still admitting the image | none |
| `lifecycle` defaults and caps | applied on the next activity evaluation; no restart |
| `lifecycle.maxUnavailableOnDrift` | paces how many Pods the changes above replace at once; see [Drift replacements are capped per class](#drift-replacements-are-capped-per-class) |

### Bucket 2: degrade-when-irreconcilable

Some changes exclude the Agent rather than constrain its derived Pod spec. The reconciler does not touch the Pod for these; it moves the Agent to `phase=Degraded` with a `reason` naming the mismatch and a message naming the offending field. A class or provider change can newly introduce any of these:

1. The Agent's `spec.image` no longer matches `image.allowedImages` (rule 2).
2. A `spec.providers` entry is no longer in `allowedProviders`, has been deleted, or its own `allowedNamespaces` no longer includes the Agent's namespace (rules 3 to 5). Rule 4, a provider dropping the namespace, is the canonical case ([scenario S5](../appendix/scenarios.md)).
3. A `spec.tools` entry is no longer in `allowedToolProviders`, its ToolProvider no longer admits the namespace or has been deleted, or a granted tool has left the declared catalog (rules 35 to 38).
4. `spec.persistence.enabled: true` while the class has `persistence.enabled: false` (rule 24).
5. `spec.lifecycle.hibernationEnabled: true` while the class has `lifecycle.hibernationAllowed: false` (rule 26).
6. `spec.handler` set while the class has `image.allowHandlerMounts: false` (rule 30).
7. The class sets `allowedNamespaces` and none of its patterns matches the Agent's namespace (rule 47). The Pod keeps running, but the gateway refuses the Agent's LLM and tool calls, as it does when a provider drops the namespace. Adding the namespace back, or removing the field, restores the prior phase. Rule 47 is the first check, so it is the reported reason when several mismatches exist.

The reasons, the `preDegradedPhase` bookkeeping, per-mismatch recovery, and what happens to the Pod meanwhile are specified under [Degraded](agent-lifecycle.md#degraded). Either side of the mismatch can be aligned, the workload spec or the class or provider, and the controller restores the prior phase on the next pass after every mismatch has cleared. Recoverable runtime issues (a transient provider outage, budget exhaustion) are a different bucket: they set a `Degraded` condition without changing the phase, see [Error handling](operations.md#error-handling).

### Bucket 3: routing-concern changes

Routing-concern fields propagate through the gateway's CRD and Secret watches with no Agent-side effect: `spec.models` shrinkage, fallback-chain edits, credential rotations on `credentialsRef`, and additive changes to `allowedNamespaces` or `allowedProviders` that exclude no bound provider or namespace. These take effect on the next routed call without any Pod-level transition. See [LLM Gateway](../gateways/llm/overview.md) for the per-request routing and credential mechanics.

### AgentTask handling (no Degraded phase)

AgentTask has no `Degraded` phase. Where an Agent would degrade, the task settles terminal `Failed` with the same reason.

| Task state at the edit | Effect |
|---|---|
| has a Pod (`Provisioning` with a Pod, `Running`, `Completing`) | none; the task finishes under the class snapshot its Pod was created from. If the edit removes the task's namespace from the class's `allowedNamespaces` (rule 47) or from a referenced provider's `allowedNamespaces`, the gateway refuses the task's LLM and tool calls |
| no Pod yet (`Pending`, or `Provisioning` before creation), or retrying from `Failed` | the pre-Pod class check runs against the new class; a violation settles the task `Failed` at once, whatever `backoffLimit` remains |
| terminal (`Succeeded`, `Failed`, `TimedOut`) | none; the task proceeds to TTL cleanup |

The class's task timeout and TTL bounds follow the same table. The reconciler records them in `status.classBounds` when it creates the task's Pod, or when a task settles before any Pod exists, so a class edit reaches a task only if the reconciler records the bounds after the edit. A task's own timeout and TTL stay editable within the recorded bounds ([The class bounds timeout and retention](../resources/agenttask.md#the-class-bounds-timeout-and-retention)).

A retry spends its `status.retries` increment before the check runs and does not get it back. To retry against a class you have since aligned, delete and recreate the task; a `kubectl apply` of the same spec does not reset `status.retries`, since status is controller-owned and apply patches only `spec`. Only an AgentClass change re-enqueues tasks; a ModelProvider or ToolProvider change is picked up at the task's next pass.

### Bulk impact

Tightening a class with many Agents re-enqueues every one of them at once, and the reconciles run concurrently up to `controller.maxConcurrentReconciles` (default 4). A class edit that changes the Pod spec hash reaches every Agent of the class, but [`maxUnavailableOnDrift`](#drift-replacements-are-capped-per-class) paces the Pod replacements themselves: at most that many Agents restart at a time, and the rest wait their turn as slots free.
