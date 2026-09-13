# Hibernation and wake

A persistent Agent that nobody is talking to still costs a Pod. Hibernation reclaims that cost: after a period of inactivity the controller deletes the Agent's Pod but keeps its PVC, and recreates the Pod on the next inbound message. The agent's state survives; the compute does not.

Three things have to work for this to be safe. The controller must know when an Agent was last active, it must tear the Pod down without losing state, and something must be able to wake the Agent when its Service has no endpoints to route to. This page covers all three.

Four Agent phases participate: `Running` -> `Idle` -> `Hibernating` -> `Hibernated` -> `Resuming` -> `Running`. The transition triggers themselves are tabulated in the [Agent state machine](agent-lifecycle.md).

## Timing knobs

Three `Agent.spec.lifecycle` durations govern the cycle. Each defaults from the referenced AgentClass and is capped by it (see [validation rules 8, 9, and 10](../resources/validation-and-defaulting.md#cross-resource-validation)):

| Field | Meaning | Class default | Class cap |
|---|---|---|---|
| `idleTimeout` | Inactivity before `Running` -> `Idle` | `defaultIdleTimeout` (`30m` in the chart's `standard` class) | `maxIdleTimeout` (`24h`) |
| `hibernationDelay` | Time spent `Idle` before `Idle` -> `Hibernating` | `defaultHibernationDelay` (`30m`) | `maxHibernationDelay` (`2h`) |
| `wakeTimeout` | How long the gateway waits for a woken Agent's Service to become reachable | `defaultWakeTimeout` (`2m`); not applied as shipped, the gateway uses 2m when the Agent leaves `wakeTimeout` unset | `maxWakeTimeout` (`5m`); not enforced as shipped |

Hibernation only happens at all when `spec.lifecycle.hibernationEnabled` is `true` and the class permits it with `lifecycle.hibernationAllowed`.

## Activity detection

### Activity lives in the gateway, not in etcd

Activity timestamps are maintained **in-memory in the gateway**, not in etcd. Writing an annotation on every request would not scale as the Agent count grows: the design target is 1000 Agents and AgentTasks per cluster, and the in-memory store scales an order of magnitude past that without a design change. At that scale, per-request etcd writes would dominate the API server.

The reconciler therefore does not keep the activity clock. It reads the clock from the gateway and writes `status.lastActivityTime` on the Agent **only when a phase transition is warranted**, which keeps the etcd write rate proportional to transitions rather than to traffic.

### Two signal sources

Two signals feed the gateway's in-memory activity store (see [Activity tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api)):

- **Gateway traffic**: the gateway records a timestamp for an Agent on every LLM request it forwards for that Agent and on every channel message it delivers to it. AgentTask traffic is not recorded.
- **Agent heartbeat**: the agent calls [`POST /v1/agent/heartbeat`](../gateways/api/agent-endpoints.md#post-v1agentheartbeat) on the gateway; the gateway updates the agent's timestamp in its in-memory store.

### Fan-out and merge

Each gateway replica keeps its own store, so the controller queries every gateway Pod IP and takes the most recent timestamp per agent and source across the replicas that answered; the fan-out, its cache, and its TLS handling are specified under [Multi-replica fan-out](../gateways/user/activation-and-activity.md#multi-replica-fan-out).

### The activitySource filter

The `/v1/activity` response returns both signal sources separately per agent. The reconciler applies the `activitySource` filter from the Agent's `spec.lifecycle.activitySource` setting **after** merging results across all gateway replicas:

- `gatewayTraffic` (default): only the `gatewayTraffic` timestamp field is considered.
- `agentHeartbeat`: only the `heartbeat` timestamp field is considered.
- `both`: the most recent timestamp from either field is used.

The order matters: merge first across replicas, then filter. The gateway returns both signal sources unconditionally, and the controller (which already holds the Agent spec) makes the filtering decision. This avoids a dependency on the gateway watching Agent resources.

### When activity data is missing

Two situations leave the controller without trustworthy activity data. In both, the controller's rule is the same: absence of data is not evidence of inactivity, so it refuses to make an idle or hibernation transition.

**Gateway unavailability.** If all gateway replicas are unreachable, the controller preserves the Agent's current phase. No idle or hibernation transitions are made without activity data. The reconciler sets a `GatewayReachable=False` condition on affected Agents and requeues in 30 seconds until the gateway recovers. If only some replicas are unreachable, the controller uses the data available from the reachable replicas.

**Gateway restart.** Activity data is in memory, so a restarted replica comes back with an empty store, which looks like silence. Each replica's `/v1/activity` response carries its `replicaStartedAt`, and the controller reads it only when no reachable replica has a record for the Agent. In that case it proceeds only if some reachable replica has been up for at least `idleTimeout` and the Agent has a `status.phaseTransitionTime`, and then counts the silence from that timestamp (set by the AgentReconciler on every phase change, see [Agent](../resources/agent.md)); otherwise it defers and requeues in 30 seconds. Any recorded activity, from any replica, is used as is. A wake stamps `status.lastActivityTime`, and both the idle and the hibernation windows are measured from no earlier than that stamp, so a woken Agent gets a full `idleTimeout` even while the gateway still reports only the activity that preceded its sleep.

![Flowchart of the controller's decision after an activity fan-out, as two rows. Evidence: no replica reachable keeps the phase and sets GatewayReachable=False; any replica with recorded activity for the Agent yields the newest timestamp per source, then activitySource; otherwise, with no replica up for at least idleTimeout, the controller defers and requeues in 30 seconds; otherwise silence is counted from phaseTransitionTime. Transitions: a Running Agent idle past idleTimeout becomes Idle; an Idle Agent with real activity newer than lastActivityTime becomes Running; an Idle Agent with hibernation enabled and silent past idleTimeout plus hibernationDelay becomes Hibernating; otherwise no change.](../diagrams/activity-data-missing.svg)

The rule "absence of data is not evidence of inactivity" is the first row. Unreachable means the controller has no answer at all. A missing record is evidence of silence only once a replica has been watching for a whole `idleTimeout`; before that the store may have been wiped by a restart, and the controller defers.

**Operational consequence:** a synchronized gateway restart (rollout, image deploy, chart upgrade) defers all idle and hibernation transitions for `idleTimeout`, since no replica satisfies the "up for `idleTimeout`" condition until that long has elapsed post-restart. Operators choosing multi-hour `idleTimeout` values should expect a corresponding window of deferred hibernation after every gateway restart.

## Hibernation mechanics

Hibernation scales the Pod to zero by deleting the Pod and keeping the PVC. On wake, the controller recreates the Pod with the same PVC mount. The Service remains (with no endpoints) while the Agent is hibernated. Wake is triggered by the [User Gateway](../gateways/user/activation-and-activity.md#the-activator) (on channel message arrival) or manual annotation, not by traffic to the Service.

Hibernation presupposes the PVC: `spec.lifecycle.hibernationEnabled: true` with `spec.persistence.enabled: false` is refused at reconcile time (`Degraded, reason=HibernationRequiresPersistence`, [rule 29](../resources/validation-and-defaulting.md#cross-resource-validation)). Without a PVC there is nothing to carry state, including the dedup buffer that [the runtime contract](../runtime/contract.md) item 7 requires, across the delete and recreate cycle.

## Wake trigger

When an Agent is `Hibernated`, its ClusterIP Service has no endpoints, so traffic is not routed to it. Nothing in the data path can wake the Agent. The gateway therefore serves as the activator: on a channel message for a `Hibernated` Agent it calls `POST /v1/activate/{namespace}/{agentName}` on the controller over mTLS, waits up to `wakeTimeout` for the Agent's Service to become reachable, then delivers the message. The activator client, its TLS, and the failure table are under [The activator](../gateways/user/activation-and-activity.md#the-activator).

The full sequence, including the two orderings that surprise readers (the `202` precedes the wake, and the replica that receives the activator call is not necessarily the replica that performs the reconcile), is drawn in [The wake sequence](../gateways/user/activation-and-activity.md#the-wake-sequence).

While waiting, the gateway holds the message. The generic webhook adapter has no side channel to signal progress on: sync callers block, and async callers already hold their `202`. The Discord adapter's deferred acknowledgment is the platform's own progress signal (the bot shows as thinking until the reply lands); the WhatsApp adapter has none.

### From activator call to Resuming

The controller side of that call is thin. The activator handler is served on every controller replica, and all it does is patch `kaalm.io/wake=true` on the target Agent through the apiserver. The leader's existing Agent watch fires, and the leader's `AgentReconciler` handles the annotation as its first step, transitioning the Agent to `Resuming` and requeueing; the next pass creates the Pod.

This is why the handler does not need to run on the leader. The Service round-robins the POST across replicas, but any replica that receives it can drive the wake, because the signal is an annotation on the resource rather than an in-memory call on the leader. See [Operator structure](overview.md).

### Wake-failure state machine

`wakeTimeout` is a gateway-side, caller-facing deadline (504 sync, `wake_timeout` async, see [The activator](../gateways/user/activation-and-activity.md#the-activator)). The controller has no wake deadline of its own: a failed Pod creation returns an error and requeues the pass, and the `Resuming` phase, already committed, carries the wake intent across it.

As shipped, `Resuming` lasts one reconcile: the next pass creates the Pod and the phase follows it through `Provisioning` to `Running` when the Pod reports Ready. Two things can intervene. A Pod-creation error returns an error and requeues the pass, with the Agent still in `Resuming` or `Provisioning`; it never reaches `Failed`. And the class cross-checks run before the Pod is touched, so a class that no longer admits the Agent's stored spec moves it to `Degraded` with `preDegradedPhase` set to the phase it was in, recovering through the standard [Degraded](agent-lifecycle.md#degraded) path once the developer aligns the spec.

A gateway-side `wakeTimeout` exhaustion does not interrupt this: the caller gets its error, the controller keeps working. The next channel message triggers another activator call, which is idempotent.

### Manual wake

Manual wake is also supported by annotation:

```
kubectl annotate agent foo kaalm.io/wake=true
```

Operational uses include pre-warming an agent before expected traffic or forcing a wake when no AgentChannel is configured. The AgentReconciler handles this annotation with phase-dependent removal so a failed reconcile cannot silently drop the wake:

- If the agent is in any non-`Hibernated` phase, the annotation is removed immediately. A `Warning` event (`reason=WakeIgnored`) is emitted **unless the agent is in `Resuming`**, where the annotation is removed silently: a wake observed during `Resuming` is a benign idempotent re-attempt, not the misfire case the Warning is meant to surface. Phase is unchanged in either branch. Outside `Resuming`, the Warning surfaces a misfire: a stale cached phase on the gateway, or a hand-set annotation on an Agent that is not asleep.
- If the agent is `Hibernated`, the reconciler transitions it to `Resuming`, stamps `status.lastActivityTime`, and requeues; the next pass creates the Pod. The annotation is removed only after the `Resuming` status write has committed, so a failed write leaves the annotation in place for the next pass to observe. Once `Resuming` is committed, the phase itself carries the wake intent across a failed Pod creation.
- The wake also stamps `status.lastActivityTime`, because the message that woke the agent is activity. The gateway records that message only once delivery succeeds, after the Pod is Ready, and the controller's activity read is cached per namespace, so the first `Running` reconcile after a wake can see only the record that preceded the sleep. The idle and hibernation windows after a wake are therefore measured from no earlier than the wake stamp; without it, an agent that slept longer than `idleTimeout + hibernationDelay` would go straight back through `Idle` to `Hibernating` after answering one message.

An operator manually annotating during the transient `Hibernating` phase will see `WakeIgnored` per the first bullet (it is a non-`Hibernated` phase); they should re-annotate after observing `status.phase = Hibernated`. Channel-driven wakes recover on the next inbound message, which calls the activator again once the phase is `Hibernated`; the message that arrives during `Hibernating` itself fails delivery as shipped, see [The activator](../gateways/user/activation-and-activity.md#the-activator).

See [AgentReconciler](reconcilers.md#agentreconciler) step 1 for the implementation detail.
