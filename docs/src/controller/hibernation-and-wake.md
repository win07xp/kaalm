# Hibernation and wake

A persistent Agent that nobody is talking to still costs a Pod. Hibernation reclaims that cost: after a period of inactivity the controller deletes the Agent's Pod but keeps its PVC, and recreates the Pod on the next inbound message. The agent's state survives; the compute does not.

Three things have to work for this to be safe. The controller must know when an Agent was last active, it must tear the Pod down without losing state, and something must be able to wake the Agent when its Service has no endpoints to route to. This page covers all three.

Five Agent phases participate: `Running` -> `Idle` -> `Hibernating` -> `Hibernated` -> `Resuming` -> `Running`. The transition triggers themselves are tabulated in the [Agent state machine](agent-lifecycle.md).

## Timing knobs

Three `Agent.spec.lifecycle` durations govern the cycle. Each defaults from the referenced AgentClass and is capped by it (see [validation rules 8, 9, and 10](../resources/validation-and-defaulting.md#cross-resource-validation)):

| Field | Meaning | Class default | Class cap |
|---|---|---|---|
| `idleTimeout` | Inactivity before `Running` -> `Idle` | `defaultIdleTimeout` (`30m` in the chart's `standard` class) | `maxIdleTimeout` (`24h`) |
| `hibernationDelay` | Time spent `Idle` before `Idle` -> `Hibernating` | `defaultHibernationDelay` (`30m`) | `maxHibernationDelay` (`2h`) |
| `wakeTimeout` | How long the gateway waits for a woken Agent's Service to become reachable | `defaultWakeTimeout` (`2m`) | `maxWakeTimeout` (`5m`) |

Hibernation only happens at all when `spec.lifecycle.hibernationEnabled` is `true` and the class permits it with `lifecycle.hibernationAllowed`.

The effective `idleTimeout` is the Agent's value, else the class `defaultIdleTimeout`, capped by `maxIdleTimeout`; a class `maxIdleTimeout` alone supplies no value. When it is zero, the controller skips the activity check, so the Agent never goes `Idle` or `Hibernating` on its own, and sets `IdleDetection=False, reason=Disabled` on the Agent. Once a nonzero idle timeout applies, the controller removes the condition instead of setting it `True`, so the common case carries no extra condition. The condition follows the spec in every phase and emits no event.

## Activity detection

### Activity lives in the gateway, not in etcd

Activity timestamps are kept **in memory in the gateway**, not in etcd. Writing an annotation on every request would not scale with the Agent count: at the design target of 1,000 or more Agents and AgentTasks per cluster, per-request etcd writes would dominate the API server.

The reconciler therefore does not keep the activity clock. It reads the clock from the gateway and writes `status.lastActivityTime` on the Agent **only when a phase transition is warranted**, which keeps the etcd write rate proportional to transitions rather than to traffic.

### Two signal sources

Two signals feed the gateway's in-memory activity store (see [Activity tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api)):

- **Gateway traffic**: the gateway records a timestamp for an Agent on every LLM request it forwards for that Agent and on every channel message it delivers to it. AgentTask traffic is not recorded.
- **Agent heartbeat**: the agent calls [`POST /v1/agent/heartbeat`](../gateways/api/agent-endpoints.md#post-v1agentheartbeat) on the gateway; the gateway updates the agent's timestamp in its in-memory store.

### Fan-out and merge

Each gateway replica keeps its own store, so the controller queries every gateway Pod IP and takes the most recent timestamp per agent and source across the replicas that answered; the fan-out, its cache, and its TLS handling are specified under [Multi-replica fan-out](../gateways/user/activation-and-activity.md#multi-replica-fan-out).

### The activitySource filter

The `/v1/activity` response returns both signal sources separately per agent. The reconciler applies the Agent's `spec.lifecycle.activitySource` filter **after** merging across all gateway replicas:

- `gatewayTraffic` (default): only the `gatewayTraffic` timestamp field is considered.
- `agentHeartbeat`: only the `heartbeat` timestamp field is considered.
- `both`: the most recent timestamp from either field is used.

The gateway returns both signal sources unconditionally, and the controller, which already holds the Agent spec, makes the filtering decision. This keeps the gateway from needing a watch on Agents.

### When activity data is missing

Two situations leave the controller without trustworthy activity data. In both, the controller's rule is the same: absence of data is not evidence of inactivity, so it refuses to make an idle or hibernation transition.

**Gateway unavailability.** If no gateway replica answers the activity fan-out for a `Running` or `Idle` Agent with a nonzero effective `idleTimeout`, the controller preserves the Agent's current phase and sets `GatewayReachable=False, reason=GatewayUnavailable`. No idle or hibernation transitions are made without activity data. If only some replicas are unreachable, the controller uses the data available from the reachable replicas.

The `GatewayReachable` condition exists only while the controller evaluates activity for the Agent: the Agent is `Running` or `Idle` and its effective `idleTimeout` is above zero. On any other pass the controller removes the condition, so a stale value never outlives the evaluation. That covers an effective `idleTimeout` of zero (`IdleDetection=False`, `reason=Disabled`) and every other phase, including an Agent the Pod step moved out of `Running`. A `Running` or `Idle` Agent whose pass ends early at a Ready gate or the certificate wait keeps its last value.

While the outage lasts, the requeue backs off. The delay is 30 seconds plus the time the `GatewayReachable` condition has been `False`, so each pass doubles the wait: 30 seconds, 1 minute, 2 minutes, 4 minutes, then the cap of 5 minutes. The outage start is read from the condition's `lastTransitionTime`, so a controller restart keeps the backoff. Jitter of 10 percent either way keeps Agents that lost the gateway together from requeueing together, and the result never exceeds 5 minutes.

A recovery kick keeps the backoff from delaying recovery. When a gateway Pod turns Ready, the controller re-enqueues every `Running` or `Idle` Agent whose `GatewayReachable` is `False`, except an Agent with `IdleDetection=False`. It also drops the activity client's cached per-namespace answers that carried no activity data (the 15-second [activity cache](../gateways/user/activation-and-activity.md#multi-replica-fan-out)), so the re-enqueued passes dial the gateway. A gateway Pod that leaves Ready or is deleted enqueues nothing: Agents find the outage on their own next pass.

**Gateway restart.** Activity data is in memory, so a restarted replica comes back with an empty store, which looks like silence. Each replica's `/v1/activity` response carries its `replicaStartedAt`, which the controller reads only when no reachable replica has a record for the Agent. In that case it proceeds only if some reachable replica has been up for at least `idleTimeout` and the Agent has a `status.phaseTransitionTime`, and then counts the silence from that timestamp (set by the AgentReconciler on every phase change, see [Agent](../resources/agent.md)); otherwise it defers and requeues in a fixed 30 seconds, with no backoff. Any recorded activity, from any replica, is used as is. A wake sets `status.lastActivityTime` to the wake time, and both the idle and the hibernation windows are measured from no earlier than that time, so a woken Agent gets a full `idleTimeout` even while the gateway still reports only the activity that preceded its sleep (the reason is under [Manual wake](#manual-wake)).

![Flowchart of the controller's decision after an activity fan-out, as two rows. Evidence: no replica reachable keeps the phase, sets GatewayReachable=False, and requeues with backoff; any replica with recorded activity for the Agent yields the newest timestamp per source, then activitySource; otherwise, with no replica up for at least idleTimeout, the controller defers and requeues in 30 seconds; otherwise silence is counted from phaseTransitionTime. Transitions: a Running Agent idle past idleTimeout becomes Idle; an Idle Agent with real activity newer than lastActivityTime becomes Running; an Idle Agent with hibernation enabled and silent past idleTimeout plus hibernationDelay becomes Hibernating; otherwise no change.](../diagrams/activity-data-missing.svg)

The rule "absence of data is not evidence of inactivity" is the first row. Unreachable means the controller has no answer at all. A missing record is evidence of silence only once a replica has been watching for a whole `idleTimeout`; before that the store may have been wiped by a restart, and the controller defers.

**Operational consequence:** a synchronized gateway restart (rollout, image deploy, chart upgrade) defers all idle and hibernation transitions for `idleTimeout`, since no replica satisfies the "up for `idleTimeout`" condition until that long has elapsed post-restart. Operators choosing multi-hour `idleTimeout` values should expect a corresponding window of deferred hibernation after every gateway restart.

## Hibernation mechanics

Hibernation scales the Pod to zero by deleting the Pod and keeping the PVC. On wake, the controller recreates the Pod with the same PVC mount. An `Idle` Agent whose Certificate is not Ready waits to hibernate until it is, because its wake could not create a Pod ([Certificate wait with a running Pod](reconcilers/agent.md#certificate-wait-with-a-running-pod)). The Service remains (with no endpoints) while the Agent is hibernated. Wake is triggered by the [User Gateway](../gateways/user/activation-and-activity.md#the-activator) (on channel message arrival) or manual annotation, not by traffic to the Service.

Hibernation presupposes the PVC: `spec.lifecycle.hibernationEnabled: true` with `spec.persistence.enabled: false` is refused at reconcile time (`Degraded, reason=HibernationRequiresPersistence`, [rule 29](../resources/validation/class-policy.md)). Without a PVC there is nothing to carry state, including the dedup buffer that [the runtime contract](../runtime/contract.md) item 7 requires, across the delete and recreate cycle.

## Wake trigger

When an Agent is `Hibernated`, its ClusterIP Service has no endpoints, so traffic is not routed to it. Nothing in the data path can wake the Agent. The gateway therefore calls the controller's activator: on a channel message for a `Hibernated` or `Hibernating` Agent it calls `POST /v1/activate/{namespace}/{agentName}` on the controller over mTLS, waits up to `wakeTimeout` for the Agent's Service to become reachable, then delivers the message. The activator client, its TLS, and the failure table are under [The activator](../gateways/user/activation-and-activity.md#the-activator). The full sequence, including the two orderings that surprise readers (the `202` precedes the wake, and the replica that receives the call is not necessarily the one that performs the reconcile), is drawn in [The wake sequence](../gateways/user/activation-and-activity.md#the-wake-sequence).

While waiting, the gateway holds the message. Sync callers block, and async callers already hold their `202`. The Discord bot shows as thinking until the reply lands; the WhatsApp adapter has no progress signal.

### From activator call to Resuming

On the controller, that call becomes one merge patch that any replica can make, so the handler does not need to run on the leader ([Activator handler](overview.md#activator-handler-served-on-every-replica)). The patch sets `kaalm.io/wake=true` and `kaalm.io/wake-trigger=channel` on the Agent. The wake value is `true` for every trigger, so a controller replica that does not read the trigger annotation still honors the wake during a rolling upgrade; only `kaalm.io/wake-trigger` marks this wake as the activator's. The leader's `AgentReconciler` handles the annotations as its first step, transitioning the Agent to `Resuming` and requeueing; the next pass creates the Pod.

### Wake-failure state machine

`wakeTimeout` is a gateway-side, caller-facing deadline (504 sync, `wake_timeout` async, see [The activator](../gateways/user/activation-and-activity.md#the-activator)). The controller has no wake deadline of its own. The Agent stays `Resuming` from the wake until its Pod reports Ready, then moves to `Running`, and the committed `Resuming` phase carries the wake intent across a failed Pod creation. Two things can intervene. A Pod-creation error never moves the Agent to `Failed`; it stays in `Resuming`. A create the API server rejects sets `Ready=False, reason=PodCreateRejected` and re-checks every 30 seconds, while any other creation error retries with backoff ([Error handling](operations.md#error-handling)). A rejected child write also leaves the Agent in `Resuming`, with `Ready=False, reason=ChildWriteRejected` and the same re-check ([A rejected child write](operations.md#a-rejected-child-write)). And the class cross-checks run before the Pod is touched, so a class that no longer admits the Agent's stored spec moves it to `Degraded` with `preDegradedPhase` set to the phase it was in, recovering through the standard [Degraded](agent-lifecycle.md#degraded) path once the developer aligns the spec.

A gateway-side `wakeTimeout` exhaustion does not interrupt this: the caller gets its error, the controller keeps working. The next channel message triggers another activator call, which is idempotent.

### Manual wake

Manual wake sets the same `kaalm.io/wake=true` annotation the activator writes, but with no `kaalm.io/wake-trigger` annotation:

```
kubectl annotate agent foo kaalm.io/wake=true --overwrite
```

The reconciler treats any Agent carrying `kaalm.io/wake=true` as a wake request. It tells a channel wake from a manual one by `kaalm.io/wake-trigger`: `channel` when the activator's patch set it, absent for a manual wake. `kaalm_wakes_total{trigger}` is labeled from that distinction ([Observability](operations.md#observability)).

Operational uses include pre-warming an agent before expected traffic or forcing a wake when no AgentChannel is configured. The AgentReconciler handles the wake with phase-dependent removal so a failed reconcile cannot silently drop it:

- If the agent is `Hibernating`, the annotations are kept. The reconciler finishes deleting the Pod, settles `Hibernated`, and requeues, so the next pass takes the `Hibernated` branch below. A wake requested while `Hibernating`, by hand or by a channel message, is therefore not lost; this is why the gateway calls the activator for a `Hibernating` Agent (see [The activator](../gateways/user/activation-and-activity.md#the-activator)).
- If the agent is in any other non-`Hibernated` phase, `kaalm.io/wake` and `kaalm.io/wake-trigger` are removed together at once, and the phase is unchanged. A `Warning` event (`reason=WakeIgnored`) is emitted **unless the agent is in `Resuming`**, where a wake is a benign idempotent re-attempt and the annotations are removed silently. Elsewhere the Warning surfaces a misfire: a stale cached phase on the gateway, or a hand-set annotation on an Agent that is not asleep.
- If the agent is `Hibernated`, the reconciler transitions it to `Resuming`, sets `status.lastActivityTime` to the wake time in the same status write, and requeues; the next pass creates the Pod. Both annotations are removed only after the `Resuming` status write has committed, so a failed write leaves them in place for the next pass to observe.

The wake sets `status.lastActivityTime` because the message that woke the agent is activity, but the gateway records it only once delivery succeeds, after the Pod is Ready, and the controller's activity read is cached per namespace. Without the write, an agent that slept longer than `idleTimeout + hibernationDelay` would go straight back through `Idle` to `Hibernating` after answering one message.

A `kaalm.io/wake-trigger` annotation left on an Agent with no `kaalm.io/wake=true` request is stale: the reconciler removes it on its next pass, before it can label a later manual wake as a channel wake.
