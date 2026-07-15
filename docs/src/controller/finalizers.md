# Finalizers

A finalizer is a marker on a resource that blocks the apiserver from actually deleting it. When you delete a resource that carries one, the apiserver sets a deletion timestamp and stops there. The object stays visible and readable until the owning controller does its cleanup work and removes its finalizer entry. Only then does the object disappear.

Agentry uses finalizers for two jobs: to run teardown that the garbage collector cannot express (terminating a Pod gracefully, changing what cascade GC will do, sweeping resources in another namespace), and to hold a resource in `Terminating` while something still depends on it.

Each reconciler adds a finalizer to its resource on first reconciliation:

| Resource | Finalizer |
|---|---|
| Agent | `agentry.io/agent-finalizer` |
| AgentTask | `agentry.io/task-finalizer` |
| ModelProvider | `agentry.io/provider-finalizer` |
| AgentClass | `agentry.io/class-finalizer` |
| AgentChannel | `agentry.io/channel-finalizer` |

## Agent

On delete, if a Pod exists, gracefully terminate it (send SIGTERM, wait up to `terminationGracePeriodSeconds`). If no Pod exists, skip this step. That covers three cases: `Hibernated`, `Pending` before Pod creation, and `Failed` before the first Pod ever started.

Then apply [`AgentClass.spec.persistence.pvcRetention`](../resources/agentclass.md).

The per-Agent PVC carries an ownerRef back to the Agent like other [child resources](../runtime/child-resources.md), so the default cascade GC removes it on Agent deletion. The finalizer toggles that outcome by mutating the ownerRef *before* removing its own finalizer entry:

- `pvcRetention: Retain`: the finalizer **strips the PVC's ownerRef** (apiserver `Patch`) and then removes the Agent finalizer. The apiserver completes Agent deletion, but the PVC has no remaining owner, so cascade GC leaves it in place.
- `pvcRetention: Delete` (default): the finalizer leaves the ownerRef intact and removes the Agent finalizer. Cascade GC subsequently removes the PVC alongside the Agent.

The ordering matters: once the finalizer entry is gone the object can vanish at any moment, so the ownerRef edit has to land first.

Two clarifications on scope:

- `pvcRetention` controls what happens to the per-Agent PVC when the Agent is deleted. It is distinct from `PersistentVolume.persistentVolumeReclaimPolicy`, which governs PV fate on PVC deletion. The two operate independently.
- A PVC referenced via [`Agent.spec.persistence.existingClaim`](../resources/agent.md) was never given an ownerRef by the reconciler and is untouched by the finalizer under either setting. `pvcRetention` governs Agentry-provisioned PVCs only.

![Activity diagram of Agent deletion under the agent-finalizer. The apiserver sets a deletionTimestamp and the finalizer blocks the delete while the object stays readable. If a Pod exists it receives SIGTERM and a wait of up to terminationGracePeriodSeconds; if none exists, covering Hibernated, Pending before Pod creation, and Failed before the first Pod started, there is nothing to terminate. A PVC supplied via existingClaim never received an ownerRef and is left untouched, so the finalizer is simply removed. For an Agentry-provisioned PVC the pvcRetention setting decides: under Retain the finalizer patches the PVC to strip its ownerRef, then removes the finalizer, and cascade GC finds no owner so the PVC survives; under Delete, the default, the ownerRef is left intact and cascade GC removes the PVC alongside the Agent.](../diagrams/agent-finalizer-pvc.svg)

Reading the diagram: retention is not a flag the garbage collector reads. It is implemented by **rewriting the ownership graph** while the finalizer still holds the Agent in place, so that when cascade GC eventually runs it reaches a different conclusion on its own. That is what makes the ordering load-bearing rather than stylistic: the ownerRef patch and the finalizer removal are two separate writes, and the finalizer removal is the point of no return.

## AgentTask

On delete, if a Pod exists, gracefully terminate it (send SIGTERM, wait up to `terminationGracePeriodSeconds`). If no Pod exists, skip this step: that is the `Pending` case before Pod creation, and the `Pending -> Failed` case where the first Pod never started.

Nothing else needs sweeping. All other child resources (PVC, Certificate, NetworkPolicy, ServiceAccount, the per-task `{taskName}-completion` ConfigMap, and the per-task `Role`/`RoleBinding`) are owner-referenced to the AgentTask per [AgentTaskReconciler](reconcilers.md#agenttaskreconciler) steps 2-4 and are removed by cascade GC. The finalizer does not sweep them explicitly.

## ModelProvider

On delete, hold the resource in `Terminating` (the finalizer is not removed) while any Agent or AgentTask still references it. Reference resolution rules are in [Cross-Resource Validation](../resources/validation-and-defaulting.md#cross-resource-validation). Once references clear, the finalizer is removed and deletion completes.

No gateway-side teardown is required. The gateway's own ModelProvider watch drops the provider from its routing table, and the [credential Secret](../gateways/llm/provider-routing.md#credential-handling) is an independent resource the platform team deletes separately.

Gateway-only-tier callers hold no Agent/AgentTask reference, so they do not block deletion. Their next request to the deleted provider fails with `400 invalid_request` (unknown provider).

## AgentClass

On delete, hold the resource in `Terminating` (finalizer retained) while any Agent or AgentTask still references it. Deletion completes when the last reference is removed.

## AgentChannel

On delete, the reconciler coordinates with the gateway (see [AgentChannelReconciler](reconcilers.md#agentchannelreconciler)). The handshake has six steps:

1. The reconciler sets `status.phase = Terminating` on the AgentChannel.
2. The gateway sees the phase change via its watch and drops the platform connection.
3. The gateway writes an `agentry.io/channel-disconnected: "true"` annotation on the AgentChannel to confirm disconnection.
4. The reconciler watches for the disconnect annotation from step 3. Once observed (or after a bounded timeout of 30s if the gateway is unavailable), the reconciler proceeds to step 5. The timeout prevents indefinite blocking if the gateway is down.
5. The reconciler deletes all `agentry-async-*` ConfigMaps in `agentry-system` matching label selector `agentry.io/channel-namespace={ns},agentry.io/channel-name={name}`. Running the sweep after step 4's disconnect confirmation eliminates the race where in-flight async-response writes from gateway replicas could orphan ConfigMaps after the sweep. The sweep is explicit because these ConfigMaps cannot carry an ownerRef back to the channel and so are invisible to cascade GC (see [async webhook response](../gateways/api/async-responses.md) for why); without this sweep the channel's stored async responses would be orphaned until their 1-hour annotation expiry.
6. The reconciler removes the finalizer and the resource is deleted.

![Sequence diagram of AgentChannel deletion across two gateway replicas. kubectl deletes the channel, the apiserver sets a deletionTimestamp, and the channel-finalizer blocks the delete. The reconciler sets status.phase to Terminating; both gateway replicas see the phase change via their watch and drop their platform connections, and the gateway then writes the agentry.io/channel-disconnected annotation to confirm. The reconciler either observes that annotation or, if the gateway is unavailable, falls through a bounded 30s timeout. Only then does it delete the agentry-async-* ConfigMaps in agentry-system by label selector and remove the finalizer, at which point the apiserver deletes the object.](../diagrams/channel-delete-handshake.svg)

Reading the diagram: the second gateway replica is on stage to show what step 4 buys. Steps 1 through 4 are what make this a handshake rather than a plain delete: the reconciler announces intent, waits for the gateway to acknowledge that it has stopped writing, and only then cleans up. Without that wait, a replica still holding its connection can complete an async-response write after the sweep has already run, leaving a ConfigMap that no sweep will revisit and no ownerRef will collect. The 30s timeout is the escape hatch that keeps a dead gateway from wedging channel deletion forever.

Finalizers prevent accidental deletion of cluster-scoped resources that would break running workloads.
