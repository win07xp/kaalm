# Finalizers

A finalizer is a marker on a resource that blocks the apiserver from deleting it. When you delete a resource that carries one, the apiserver sets a deletion timestamp and stops there. The object stays visible and readable until the controller responsible for it does its cleanup work and removes its finalizer entry. Only then does the object disappear.

Kaalm uses finalizers for two jobs: to run teardown that the garbage collector cannot express (terminating a Pod, changing what cascade GC does, sweeping resources in another namespace), and to hold a resource while something still depends on it.

Each reconciler adds its finalizer on the first reconcile of a resource:

| Resource | Finalizer | On delete |
|---|---|---|
| Agent | `kaalm.io/agent-finalizer` | terminates the Pod, applies `pvcRetention`, releases |
| AgentTask | `kaalm.io/task-finalizer` | terminates the Pod, releases |
| AgentChannel | `kaalm.io/channel-finalizer` | runs the gateway handshake, sweeps the async records, releases |
| ModelProvider | `kaalm.io/provider-finalizer` | holds while referenced |
| ToolProvider | `kaalm.io/toolprovider-finalizer` | holds while referenced |
| AgentClass | `kaalm.io/class-finalizer` | holds while referenced |

## Agent

The finalizer runs in this order, each step on its own pass:

1. The phase is set to `Terminating` and `status.preDegradedPhase` is cleared, in one status write.
2. If a Pod exists, it is deleted with its own `terminationGracePeriodSeconds`, and the pass ends; the owned-Pod watch re-enqueues the Agent when the Pod is gone. `Hibernated`, `Pending`, and `Failed`-before-first-Pod Agents have nothing to terminate.
3. [`AgentClass.spec.persistence.pvcRetention`](../resources/agentclass.md) is applied. With `Retain`, the finalizer rewrites the PVC's `metadata.ownerReferences` to drop the Agent, so cascade GC finds no owner and leaves the PVC in place. With `Delete`, the default, the ownerRef stays and cascade GC removes the PVC with the Agent.
4. The finalizer is removed and the apiserver deletes the Agent.

![Flowchart of Agent deletion under the finalizer. The deletion timestamp is set and the finalizer holds the Agent; the phase becomes Terminating with preDegradedPhase cleared. Pod row: if a Pod exists it is deleted and the pass waits for the watch. PVC row: a PVC that Kaalm did not provision is left alone; a Kaalm-provisioned PVC under pvcRetention Retain has the Agent's ownerRef removed; under Delete it is left as is. Then the finalizer is removed and the apiserver deletes the Agent.](../diagrams/agent-finalizer-pvc.svg)

Retention is not a flag the garbage collector reads. It is implemented by rewriting the ownership graph while the finalizer still holds the Agent, so that cascade GC reaches a different conclusion on its own. The order of the two writes is the point: once the finalizer entry is gone the object can vanish at any moment, so the ownerRef edit lands first.

`pvcRetention` governs the per-Agent PVC only. A PVC named by [`spec.persistence.existingClaim`](../resources/agent.md) never received an ownerRef and is untouched under either setting, and `PersistentVolume.persistentVolumeReclaimPolicy`, which governs the PV when a PVC is deleted, is independent of it.

## AgentTask

The finalizer deletes the Pod if one exists, waits for it to go, and releases. Nothing else is swept: every other child (the Certificate, ServiceAccount, and NetworkPolicy always, the PVC when persistence is enabled, and the completion ConfigMap with its Role and RoleBinding for `agentReported` tasks) is owner-referenced to the AgentTask and removed by cascade GC; see [Child resources](../runtime/child-resources.md).

## Cluster-scoped resources

ModelProvider, ToolProvider, and AgentClass are cluster-scoped and carry no phase. On delete, the reconciler keeps the finalizer while a referrer exists, so the object keeps its deletion timestamp and stays readable, and releases it on the pass after the last reference clears (the referrers' watches re-enqueue the resource).

| Resource | Pinned by | Fields checked |
|---|---|---|
| ModelProvider | any Agent or AgentTask, or any AgentClass | `spec.providers[].providerRef`, `spec.allowedProviders` |
| ToolProvider | any Agent or AgentTask, or any AgentClass | `spec.tools[].providerRef`, `spec.allowedToolProviders` |
| AgentClass | any Agent or AgentTask | `spec.agentClassRef` |

The hold is by reference, not by validity: a workload whose reference violates a validation rule still pins its provider or class. As shipped the hold is silent: no condition, event, or phase says that a delete is blocked, so a hung `kubectl delete` is diagnosed by listing the referrers.

No gateway-side teardown is needed for a provider. The gateway's own watch drops it from its routing table, and its credential Secret is an independent resource the platform team deletes separately. Gateway-only-tier callers hold no Agent or AgentTask reference and never block a delete; their next request to a deleted provider fails with `400 invalid_request`.

## AgentChannel

Deleting a channel is a handshake with the gateway, because the channel's async response records live in `kaalm-system`, carry no ownerRef to the channel, and are invisible to cascade GC; see [Response persistence](../gateways/api/async-responses.md#response-persistence). The finalizer's sweep is their only cleanup, and the handshake exists so no gateway replica writes a new record after the sweep.

![Sequence diagram of AgentChannel deletion with two gateway replicas. The reconciler sets status.phase to Terminating; both replicas see it in their watch and answer 401 on the channel's path; the first replica to see it writes the channel-disconnected annotation. The reconciler proceeds when it sees the annotation, or 30 seconds after the deletion timestamp without it, then deletes the kaalm-async-* ConfigMaps by the channel's labels and removes the finalizer.](../diagrams/channel-delete-handshake.svg)

1. The reconciler sets `status.phase: Terminating`.
2. Every gateway replica sees the phase in its watch and rejects further inbound requests on the channel's path with `401`, so it creates no new `kaalm-async-*` record. The gate is the intake handler's, so it costs nothing extra.
3. The first replica to see the phase writes the `kaalm.io/channel-disconnected: "true"` annotation on the channel.
4. The reconciler proceeds when it sees the annotation, checking every two seconds, or 30 seconds after the deletion timestamp without it, so a dead gateway cannot wedge the delete.
5. It deletes every `kaalm-async-*` ConfigMap in `kaalm-system` carrying the channel's `kaalm.io/channel-namespace` and `kaalm.io/channel-name` labels, expired or not.
6. It removes the finalizer and the apiserver deletes the channel.

With the confirmation the sweep is final. As shipped the confirmation is one annotation written by whichever replica saw the phase first, not one per replica, so a second replica that has not seen `Terminating` can still write a record after the sweep; the same holds on the timeout branch. Such a record lingers until nothing reaps it, since the expiry prune runs only for live channels.

The Role and RoleBinding the reconciler created for the channel are owner-referenced and cascade-delete with it; see [Per-channel credential Role](reconcilers.md#per-channel-credential-role).
