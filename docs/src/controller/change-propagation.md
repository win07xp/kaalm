# Change propagation

Two kinds of change reach an Agent's provisioned child resources: edits to the Agent's own spec, and edits to the AgentClass, ModelProvider, or ToolProvider it references. This page defines both paths and is the canonical description of class-change propagation.

## Spec change handling

When a developer edits an Agent's spec, the controller detects drift by hash comparison. It hashes the Pod spec it would derive from the current Agent spec and class, and compares the result with the hash it wrote to the existing Pod's `kaalm.io/pod-spec-hash` annotation when it created the Pod. The comparison is never made against the live Pod object: the apiserver defaults and injects fields on admission, so a deep-equal against the live object would report drift on every pass and recreate the Pod in a loop.

The hash covers the image, command, args, env, resources, the provider names, and the handler ConfigMap name (never its content). It also covers every Pod input the AgentClass controls, as derived for the Pod: the Pod and container security contexts with the `restricted` baseline merged in, `automountServiceAccountToken`, `runtimeClassName`, the image pull policy and pull Secrets, the termination grace period, and the `podMetadata` labels and annotations. Kaalm replaces the Pod on any change to a hashed input, for a clean process restart, even where Kubernetes would allow an in-place change such as an image or resources. On a hash mismatch the Agent transitions to `Provisioning`, the Pod is deleted with its `terminationGracePeriodSeconds`, and a new Pod is created from the new spec. The PVC, Service, Certificate, ServiceAccount, and NetworkPolicy are preserved.

Fields outside the hash are never applied to a live Pod: the reconciler patches only the two hash annotations of a live Pod ([An upgrade that changes the hash formula replaces no Pod](#an-upgrade-that-changes-the-hash-formula-replaces-no-pod)). A change to such a field takes effect when the Pod is next replaced for another reason. The exception is the name of the TLS Secret the Pod mounts ([A Pod that mounts another TLS Secret is replaced](#a-pod-that-mounts-another-tls-secret-is-replaced)). The Service and the NetworkPolicy are converged in place, so a `spec.service.port` change or a class egress change reaches a running Agent without a restart. If the API server rejects the in-place update, such as a policy webhook refusing the new NetworkPolicy, the old object stays and the Agent shows `Ready=False, reason=ChildWriteRejected` with its Pod kept ([A rejected child write](operations.md#a-rejected-child-write)).

### Effect by phase

| Phase during edit | Effect |
|---|---|
| `Running`, `Idle` | The Pod is replaced on the next pass. |
| `Provisioning`, `Resuming` | A Pod that already exists is replaced like any other, since the hash check runs before the readiness check. An edit that lands before the Pod is created is absorbed into it. |
| `Hibernating`, `Hibernated` | Applied on the next wake, which creates the Pod from the new spec. The in-progress hibernation completes first. |
| `Degraded` | The cross-checks re-run on every pass, so aligning the Agent or class spec is the way out of `Degraded`; see [Degraded](agent-lifecycle.md#degraded). |

A spec edit's effect is guaranteed visible only after `status.phase` next settles at `Running`, `Hibernated`, or, for class recovery, the restored `preDegradedPhase`.

### Drift replacements are capped per class

A drifted Agent does not replace its Pod the moment it detects drift. It first asks its class for one of the `lifecycle.maxUnavailableOnDrift` slots: an integer of at least 1, or a percentage of the class's Agents, hibernated ones included, from 1% to 100%, rounded up and never below 1. A class with no `lifecycle.maxUnavailableOnDrift`, including one with no `lifecycle` block at all, gets 25% ([rule 44](../resources/validation/class-policy.md)).

A granted Agent's `PodUpToDate` condition becomes `False, reason=Replacing`, its old Pod is deleted, and a new one is created from the current spec. The slot frees after the new Pod is Ready on that spec (`PodUpToDate` turns `True, reason=Current`), or the Agent is deleted. A refused Agent's `PodUpToDate` condition becomes `False, reason=ReplacementPending`, and its current Pod keeps serving: `status.phase` and the `Ready` condition still come from that Pod. A refused Agent is re-queued as soon as another Agent of the class leaves `Replacing`, with a 30-second fallback retry. When both an Idle and a non-Idle Agent of the class wait for a slot, the Idle one is replaced first.

The cap applies to every drift replacement alike: an edit to an Agent's own spec, a class or provider edit that reaches the Pod spec hash ([Bucket 1](#bucket-1-recreate-and-clamp-default)), and a [TLS Secret mismatch](#a-pod-that-mounts-another-tls-secret-is-replaced) compete for the same slots. `AgentClass.status.agentsReplacing` and `status.agentsPendingReplacement` count the class's Agents on each side of the wait ([AgentClass](../resources/agentclass.md)). A drift held on the Agent's Certificate takes no slot and is in neither count until the Certificate is Ready ([Certificate wait with a running Pod](reconcilers/agent.md#certificate-wait-with-a-running-pod)). An Agent that reaches `Hibernated` while waiting for a slot stops waiting and drops out of `agentsPendingReplacement`, since hibernation clears its `PodUpToDate` condition; its wake creates the Pod from the current spec.

A Pod that never becomes Ready after a grant, because it crash-loops or cannot pull its image, sets the Agent `Failed` but keeps its slot, so a bad rollout halts at the cap instead of failing the whole class. A further spec change replaces that Agent's Pod at once, on the slot it already holds, and the rollout resumes from there.

A replacement Pod create that the API server rejects, such as a class edit that names a missing `RuntimeClass` or a Pod create over a ResourceQuota, halts the rollout at the cap in the same way. The Agent keeps its slot and shows `Provisioning` with `Ready=False, reason=PodCreateRejected`, not `Failed`, so a bad `runtimeClassName` stops at `maxUnavailableOnDrift` Agents. Fixing the cause creates the Pod within 30 seconds ([Timing](reconcilers/agent.md#timing)), and a further spec change retries the create at once on the held slot.

### A Pod that mounts another TLS Secret is replaced

cert-manager renews a certificate into the Secret that the Certificate's `spec.secretName` names. A Pod whose TLS volume names a different Secret stops receiving renewals and fails once its certificate expires. On each pass that reaches the Pod, the Agent reconciler compares the Secret the Pod's TLS volume names with the Certificate's `spec.secretName` and replaces the Pod when they differ.

This happens when someone deletes a Certificate whose Secret is named `{name}-tls`, the name earlier releases gave it. The controller re-creates the Certificate with the UID-suffixed name ([Agent certificate](reconcilers/agent.md#agent-certificate)). The old Pod keeps serving while the new certificate issues, and the Agent shows `Ready=False, reason=CertificateNotReady`. The replacement waits for the Certificate without taking a slot (`PodUpToDate` shows `CertificateNotReady`), then asks for one once the Certificate is Ready ([Certificate wait with a running Pod](reconcilers/agent.md#certificate-wait-with-a-running-pod)).

The replacement is a drift replacement. It takes a `maxUnavailableOnDrift` slot, sets `PodUpToDate` to `Replacing` or `ReplacementPending`, and emits `SpecDrift` or `SpecDriftPending`. The event and condition messages name both Secrets, so you can tell why a Pod restarted with no spec edit. The Pod keeps its mounted certificate until it expires, so the replacement has no reason to bypass the cap.

The Secret name stays out of the Pod spec hash, so an upgrade that keeps a Certificate's existing name replaces no Pod. AgentTask Pods are not compared: a task Pod is short-lived, and a retry's Pod mounts the name the current Certificate holds. A deleted task Certificate is re-created while the Pod keeps the certificate it mounted ([AgentTask certificate](reconcilers/agenttask.md#agenttask-certificate)).

### An upgrade that changes the hash formula replaces no Pod

Each Pod carries a second annotation, `kaalm.io/pod-spec-hash-version`, which records the version of the formula that produced its hash. A Pod without this annotation has a version 1 hash. When the reconciler finds such a Pod, it recomputes the hash with the version 1 formula and compares:

- If the hashes match, the Pod is current under the rules it was created with. The reconciler patches both annotations in place with the current formula's hash and version, and the Pod keeps running.
- If the hashes differ, the Pod has drifted, and the reconciler replaces it as usual.

As a result, an AgentClass edit made before the upgrade to a field that version 1 does not hash (`security`, `runtime.runtimeClassName`, the pull settings, the termination grace period, or `podMetadata`) does not reach a running Pod at upgrade time. The edit takes effect when the Pod is next replaced. An edit made after the upgrade replaces the Pod, as [Bucket 1](#bucket-1-recreate-and-clamp-default) describes.

The version 1 hash is recomputed from the spec as the earlier release derived it, including container `claims` that release passed through when the class set no `maxLimits`. A running Pod whose Agent sets `claims` therefore keeps running through the upgrade.

## AgentClass change handling

When an AgentClass, ModelProvider, or ToolProvider spec changes, the [AgentReconciler](reconcilers/agent.md) re-enqueues every Agent referencing it so propagation is event-driven rather than waiting for a periodic requeue. The [AgentTaskReconciler](reconcilers/agenttask.md) watches AgentClass only. Each watch fires only on a change that can affect an Agent, so most status writes never fan out: a class or tool provider triggers on a spec generation change, and a model provider on a spec change, a change to its `Ready` status or reason, or a change to the set of namespaces its budget blocks.

A change propagates along one of three paths, decided in this order: does it exclude the Agent's stored spec (bucket 2), does it change the Pod spec hash (bucket 1), or neither (bucket 3, or an in-place child update).

![Flowchart of what a class or provider edit does to a provisioned Agent, as one cascade. If the stored spec is no longer admitted by the class and providers, the Agent becomes Degraded with the Pod untouched. Otherwise, if the Pod spec hash is unchanged, the children are converged in place with no restart. Otherwise a Hibernated Agent applies the change on its next wake, and any other Agent goes to Provisioning, where the Pod is deleted and created from the new spec.](../diagrams/agentclass-propagation.svg)

The exclusion test runs first, so bucket membership follows what the change does to the stored spec, not the field's nominal category: a restrictive `allowedNamespaces` edit is bucket 2 even though `allowedNamespaces` is listed under bucket 3.

### Bucket 1: recreate-and-clamp (default)

A class change that reaches the Pod spec hash replaces the Pod, subject to the class's `maxUnavailableOnDrift` cap ([Drift replacements are capped per class](#drift-replacements-are-capped-per-class)): `maxLimits` lowered (the Agent's `resources.limits` are clamped to it), `defaultImage` changed for an Agent with no image of its own, `defaultResources` changed for an Agent with none, or any change to `security`, `runtime.runtimeClassName`, `image.pullPolicy`, `image.imagePullSecrets`, `lifecycle.terminationGracePeriodSeconds`, or `podMetadata`. Once an Agent holds a slot, its Pod is replaced from the clamped spec as in [Spec change handling](#spec-change-handling); a `Hibernated` Agent applies the change on its next wake. Agents must tolerate restart.

Class fields outside the hash reach a running Agent by other routes or not at all:

| Class change | Effect on a provisioned Agent |
|---|---|
| `network.egress.allowedCIDRs`, `allowSameNamespaceIngress` | the NetworkPolicy is updated in place; no restart |
| `image.allowedImages` narrowed but still admitting the image | none |
| `lifecycle` defaults and caps | applied on the next activity evaluation; no restart |
| `lifecycle.maxUnavailableOnDrift` | paces how many Pods the hash changes listed before this table replace at once; see [Drift replacements are capped per class](#drift-replacements-are-capped-per-class) |

### Bucket 2: degrade-when-irreconcilable

Some changes exclude the Agent rather than constrain its derived Pod spec. The reconciler does not touch the Pod for these; it moves the Agent to `phase=Degraded` with a `reason` naming the mismatch. A class or provider change can newly introduce any of these:

1. The Agent's `spec.image` no longer matches `image.allowedImages` (rule 2).
2. A `spec.providers` entry is no longer in `allowedProviders`, has been deleted, or its own `allowedNamespaces` no longer includes the Agent's namespace (rules 3 to 5). Rule 4, a provider dropping the namespace, is the canonical case ([scenario S5](../appendix/scenarios.md)).
3. A `spec.tools` entry is no longer in `allowedToolProviders`, its ToolProvider no longer admits the namespace or has been deleted, or a granted tool has left the declared catalog (rules 35 to 38).
4. `spec.persistence.enabled: true` while the class has `persistence.enabled: false` (rule 24).
5. `spec.lifecycle.hibernationEnabled: true` while the class has `lifecycle.hibernationAllowed: false` (rule 26).
6. `spec.handler` set while the class has `image.allowHandlerMounts: false` (rule 30).
7. The class sets `allowedNamespaces` and none of its patterns matches the Agent's namespace (rule 47). The Pod keeps running, but the gateway refuses the Agent's LLM and tool calls, as it does when a provider drops the namespace. Adding the namespace back, or removing the field, restores the prior phase. Rule 47 is the first check, so it is the reported reason when several mismatches exist.

The reasons, the `preDegradedPhase` bookkeeping, per-mismatch recovery, and what happens to the Pod meanwhile are specified under [Degraded](agent-lifecycle.md#degraded). The controller restores the prior phase on the next pass after every mismatch has cleared, whichever side is aligned. A provider budget that blocks the namespace is a different bucket: it sets the `Degraded` condition with `reason=BudgetExhausted` and leaves the phase alone. An unhealthy provider shows on the `ProvidersReady` condition instead, see [Error handling](operations.md#error-handling).

### Bucket 3: routing-concern changes

Routing-concern fields propagate through the gateway's CRD and Secret watches with no Agent-side effect: `spec.models` shrinkage, fallback-chain edits, credential rotations on `credentialsRef`, and additive changes to `allowedNamespaces` or `allowedProviders` that exclude no bound provider or namespace. These take effect on the next routed call without any Pod-level transition. See [LLM Gateway](../gateways/llm/overview.md) for the per-request routing and credential mechanics.

### AgentTask handling (no Degraded phase)

AgentTask has no `Degraded` phase. Where an Agent would degrade, the task settles terminal `Failed` with the same reason.

| Task state at the edit | Effect |
|---|---|
| has a Pod (`Provisioning` with a Pod, `Running`, `Completing`) | none; the task finishes under the class snapshot its Pod was created from. A child the reconciler re-creates after it was deleted is built from the class as it now stands ([Task child-resource convergence](reconcilers/agenttask.md#task-child-resource-convergence)). If the edit removes the task's namespace from the class's `allowedNamespaces` (rule 47) or from a referenced provider's `allowedNamespaces`, the gateway refuses the task's LLM and tool calls |
| no Pod yet (`Pending`, or `Provisioning` before creation), or retrying from `Failed` | the pre-Pod class check runs against the new class; a violation settles the task `Failed` at once, whatever `backoffLimit` remains |
| terminal (`Succeeded`, `Failed`, `TimedOut`) | none; the task proceeds to TTL cleanup |

The class's task timeout and TTL bounds follow the same table. The reconciler records them in `status.classBounds` when it creates the task's Pod, or when a task settles before any Pod exists, so a class edit reaches a task only if the reconciler records the bounds after the edit. A task's own timeout and TTL stay editable within the recorded bounds ([The class bounds timeout and retention](../resources/agenttask.md#the-class-bounds-timeout-and-retention)).

A retry spends its `status.retries` increment before the check runs; [Retry mechanics](task-lifecycle.md#retry-mechanics) says how to retry against a class you have since aligned. Only an AgentClass change re-enqueues tasks; a ModelProvider or ToolProvider change is picked up at the task's next pass.

### Bulk impact

Tightening a class with many Agents re-enqueues every one of them at once, and the reconciles run concurrently up to `controller.maxConcurrentReconciles` (default 4). A class edit that changes the Pod spec hash reaches every Agent of the class, but [`maxUnavailableOnDrift`](#drift-replacements-are-capped-per-class) paces the Pod replacements: at most that many Agents restart at a time.
