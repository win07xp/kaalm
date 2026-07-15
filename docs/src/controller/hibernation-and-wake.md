# Hibernation and Wake

A persistent Agent that nobody is talking to still costs a Pod. Hibernation reclaims that cost: after a period of inactivity the controller deletes the Agent's Pod but keeps its PVC, and recreates the Pod on the next inbound message. The agent's state survives; the compute does not.

Three things have to work for this to be safe. The controller must know when an Agent was last active, it must tear the Pod down without losing state, and something must be able to wake the Agent when its Service has no endpoints to route to. This page covers all three.

Four Agent phases participate: `Running` -> `Idle` -> `Hibernating` -> `Hibernated` -> `Resuming` -> `Running`. The transition triggers themselves are tabulated in the [Agent state machine](agent-lifecycle.md).

## Timing knobs

Three `Agent.spec.lifecycle` durations govern the cycle. Each defaults from the referenced AgentClass and is capped by it (see [validation rules 8, 9, and 10](../resources/validation-and-defaulting.md#cross-resource-validation)):

| Field | Meaning | Class default | Class cap |
|---|---|---|---|
| `idleTimeout` | Inactivity before `Running` -> `Idle` | `defaultIdleTimeout` (`30m` in the chart's `standard` class) | `maxIdleTimeout` (`24h`) |
| `hibernationDelay` | Time spent `Idle` before `Idle` -> `Hibernating` | `defaultHibernationDelay` (`30m`) | `maxHibernationDelay` (`2h`) |
| `wakeTimeout` | How long the gateway waits for a woken Pod to become Ready | `defaultWakeTimeout` (`2m`, i.e. 120s) | `maxWakeTimeout` (`5m`) |

Hibernation only happens at all when `spec.lifecycle.hibernationEnabled` is `true` and the class permits it via `lifecycle.hibernationAllowed`.

## Activity Detection

### Activity lives in the gateway, not in etcd

Activity timestamps are maintained **in-memory in the gateway**, not in etcd. Writing an annotation on every request would not scale as the Agent count grows: v1 targets 1000 Agents/AgentTasks per cluster, and the in-memory activity store is deliberately designed so future versions can reach an order of magnitude higher without a design change. At that scale, per-request etcd writes would dominate the API server.

The reconciler therefore does not own the activity clock. It reads the clock from the gateway and writes `status.lastActivityTime` on the Agent **only when a phase transition is warranted**, which keeps the etcd write rate proportional to transitions rather than to traffic.

### Two signal sources

Two signals feed the gateway's in-memory activity store (see [Activity Tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api)):

- **Gateway traffic**: the LLM Gateway and User Gateway record the timestamp of each request for an Agent in-memory.
- **Agent heartbeat**: the agent calls [`POST /v1/agent/heartbeat`](../gateways/api/agent-endpoints.md#post-v1agentheartbeat) on the gateway; the gateway updates the agent's timestamp in its in-memory store.

### Fan-out and merge

The gateway exposes `GET /v1/activity?namespace={ns}`, returning a map of agent names to last-activity timestamps. Because each gateway replica maintains its own in-memory store, updated only by the traffic that replica handled, the controller fans the query out to **all gateway Pod IPs in parallel**, enumerating them via its Pod informer. Querying the ClusterIP Service instead would round-robin to a single replica and miss activity recorded by the others, making busy agents look idle.

The controller takes the **most recent timestamp per agent** across all responses. Replicas that are unreachable are skipped, and data from the remaining replicas is used. The `replicaStartedAt` field in each response is evaluated per-replica for restart detection. See [Activity Tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api) for the full fan-out protocol.

### The activitySource filter

The `/v1/activity` response returns both signal sources separately per agent. The reconciler applies the `activitySource` filter from the Agent's `spec.lifecycle.activitySource` setting **after** merging results across all gateway replicas:

- `gatewayTraffic` (default): only the `gatewayTraffic` timestamp field is considered.
- `agentHeartbeat`: only the `heartbeat` timestamp field is considered.
- `both`: the most recent timestamp from either field is used.

The order matters: merge first across replicas, then filter. The gateway returns both signal sources unconditionally, and the controller (which already holds the Agent spec) owns the filtering decision. This avoids a dependency on the gateway watching Agent resources.

### When activity data is missing

Two situations leave the controller without trustworthy activity data. In both, the controller's rule is the same: absence of data is not evidence of inactivity, so it refuses to make an idle or hibernation transition.

**Gateway unavailability.** If all gateway replicas are unreachable, the controller preserves the Agent's current phase. No idle or hibernation transitions are made without activity data. The reconciler sets a `GatewayReachable=False` condition on affected Agents and requeues with backoff until the gateway recovers. If only some replicas are unreachable, the controller uses the data available from the reachable replicas.

**Gateway restart.** Activity data is in-memory, so a restarted replica comes back with an empty store, which is indistinguishable from genuine silence unless the controller can tell the replica is new. Each replica's `/v1/activity` response includes a `replicaStartedAt` timestamp. The controller compares this against the Agent's `status.phaseTransitionTime` (set by the AgentReconciler on every phase change, see the [Agent CRD design notes](../resources/agent.md)). If a replica's `replicaStartedAt` is more recent than `status.phaseTransitionTime`, that replica restarted while the agent was Running, so the controller treats that replica's missing activity data as "unknown": it uses data from other replicas, or defers if all replicas have restarted. No idle or hibernation transitions are made until at least one replica has been running for `idleTimeout`, at which point missing activity data from that replica can be interpreted as genuine inactivity.

**Operational consequence:** a synchronized gateway restart (rollout, image deploy, chart upgrade) defers all idle and hibernation transitions for `idleTimeout`, since no replica satisfies the "up for `idleTimeout`" condition until that long has elapsed post-restart. Operators choosing multi-hour `idleTimeout` values should expect a corresponding window of deferred hibernation after every gateway restart.

## Hibernation Mechanics

Hibernation scales the Pod to zero by deleting the Pod and keeping the PVC. On wake, the controller recreates the Pod with the same PVC mount. The Service remains (with no endpoints) while the Agent is hibernated. Wake is triggered by the [User Gateway](../gateways/user/activation-and-activity.md#the-activator) (on channel message arrival) or manual annotation, not by traffic to the Service.

Hibernation presupposes the PVC: `spec.lifecycle.hibernationEnabled: true` with `spec.persistence.enabled: false` is refused at reconcile time (`Degraded, reason=HibernationRequiresPersistence`, [rule 29](../resources/validation-and-defaulting.md#cross-resource-validation)). Without a PVC there is nothing to carry state, including the [The Runtime Contract](../runtime/contract.md) item 7 dedup buffer, across the delete-Pod/recreate-Pod cycle.

## Wake Trigger

When an Agent is `Hibernated`, its ClusterIP Service has no endpoints, so traffic is not routed to it. Nothing in the data path can wake the Agent. The gateway therefore serves as the activator: on a channel message for a hibernated Agent it calls `POST /v1/activate/{namespace}/{agentName}` on the controller over mTLS, waits up to `wakeTimeout` for the Pod to become Ready, then delivers the message, surfacing `504` (sync) or a `wake_timeout` / `controller_unavailable` error payload (async) if it cannot. The full activator flow, its TLS setup, and its failure responses are documented in [Activator](../gateways/user/activation-and-activity.md#the-activator) and [§ Failure Modes](../gateways/user/operations.md#failure-modes).

While waiting, the gateway holds the message. The v1 generic webhook adapter has no side channel to signal progress on: sync callers simply block, and async callers already hold their `202`. v1.1 platform adapters (Discord, WhatsApp) may surface a "typing" indicator here.

### From activator call to Resuming

The controller side of that call is deliberately thin. The activator handler is served on **every** controller replica, and all it does is patch `agentry.io/wake=true` on the target Agent via the apiserver. The leader's existing Agent watch fires, and the leader's `AgentReconciler` runs the manual-wake path (step 9) to transition the Agent to `Resuming` and recreate the Pod.

This is why the handler does not need to run on the leader. The Service round-robins the POST across replicas, but any replica that receives it can drive the wake, because the signal is an annotation on the resource rather than an in-memory call on the leader. See [Operator Structure](overview.md).

### Wake-failure state machine

`wakeTimeout` is purely a gateway-side caller-facing deadline (504 sync / `wake_timeout` async, see [Activator](../gateways/user/activation-and-activity.md#the-activator)). The controller has no wake deadline of its own: per [AgentReconciler step 9](reconcilers.md#agentreconciler), a failed Pod recreation simply requeues the reconcile with the wake annotation still in place.

An Agent stays in `Resuming` until one of three outcomes:

1. The Pod becomes Ready (-> `Running`).
2. An unrecoverable Pod-creation error trips `any -> Failed`.
3. The pre-Pod cross-check in [AgentReconciler step 5](reconcilers.md#agentreconciler) finds the class no longer admits the Agent's stored spec (-> `Degraded` with `preDegradedPhase = Resuming`, recovering through the standard Degraded path when the developer aligns the spec).

A gateway-side `wakeTimeout` exhaustion does not interrupt this: the caller gets its error, the controller keeps working. The next channel message simply triggers another activator call, which is idempotent.

### Manual wake

Manual wake is also supported via annotation:

```
kubectl annotate agent foo agentry.io/wake=true
```

Operational uses include pre-warming an agent before expected traffic or forcing a wake when no AgentChannel is configured. The AgentReconciler handles this annotation with phase-dependent removal so a failed reconcile cannot silently drop the wake:

- If the agent is in any non-`Hibernated` phase, the annotation is removed immediately. A `Warning` event (`reason=WakeIgnored`) is emitted **unless the agent is in `Resuming`**, where the annotation is removed silently: a wake observed during `Resuming` is a benign idempotent re-attempt, not the misfire case the Warning is meant to surface. Phase is unchanged in either branch. (Outside `Resuming`, the Warning is the defense against the gateway's lazy hibernation detection misfiring on transient network failures, see [Activator](../gateways/user/activation-and-activity.md#the-activator).)
- If the agent is `Hibernated`, the reconciler transitions it to `Resuming` and recreates the Pod. The annotation is removed **only after** the transition to `Resuming` has been committed. If the status update or the subsequent Pod recreation fails and the reconcile is requeued, the annotation is left in place so the next reconcile pass can re-observe the wake intent.

An operator manually annotating during the transient `Hibernating` phase will see `WakeIgnored` per the first bullet (it is a non-`Hibernated` phase); they should re-annotate after observing `status.phase = Hibernated`. Channel-driven wakes are unaffected: the gateway re-attempts the activator call on the next inbound message, so a wake racing an in-progress hibernation is recovered automatically when the next message arrives.

See [AgentReconciler](reconcilers.md#agentreconciler) step 9 for the implementation detail.
