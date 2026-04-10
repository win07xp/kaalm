# Agentry — Agent Lifecycle & State Machines

This document describes the state machines for Agent and AgentTask resources, including transition triggers, activity detection, hibernation mechanics, and finalizers.

For reconciler responsibilities (what each reconciler does and how it converges child resources), see [CONTROLLER_RECONCILERS.md](./CONTROLLER_RECONCILERS.md).

---

## Agent (persistent mode)

```
                 ┌──────────┐
                 │ Pending  │  initial
                 └────┬─────┘
                      │ references validated, class resolved
                      ▼
                 ┌─────────────┐
                 │Provisioning │  creating Pod/PVC/Service
                 └────┬────────┘
                      │ Pod becomes Ready
                      ▼
          ┌────────▶┌─────────┐
          │         │ Running │◀──────┐
          │         └────┬────┘       │
          │              │            │ activity observed
          │  idleTimeout │            │
          │   elapsed    │            │
          │              ▼            │
          │         ┌──────┐          │
          │         │ Idle │──────────┘
          │         └──┬───┘
          │            │ hibernationEnabled=true
          │            │ AND idle for hibernationDelay
          │            ▼
          │      ┌─────────────┐
          │      │ Hibernating │   scaling pod to zero
          │      └──────┬──────┘
          │             │ pod scaled to 0, PVC retained
          │             ▼
          │      ┌────────────┐
          │      │ Hibernated │──── channel message via User Gateway
          │      └─────┬──────┘     OR manual wake annotation
          │            │
          │            ▼
          │      ┌──────────┐
          │      │ Resuming │   scaling pod back up
          │      └─────┬────┘
          │            │ pod Ready
          │            └───────────┐
          │                        │
          │                        ▼
          │   (back to Running) ───┘
          │
          │  irrecoverable error at any point
          ▼
     ┌────────┐
     │ Failed │
     └────────┘

     deletion requested at any point
         │
         ▼
   ┌─────────────┐
   │ Terminating │ → (resource removed after finalizers run)
   └─────────────┘

     transient provider error
         │
         ▼
    ┌──────────┐
    │ Degraded │ → (re-enters Running when provider recovers)
    └──────────┘
```

**Transition triggers:**

| From -> To | Trigger |
|---|---|
| Pending -> Provisioning | References validated, AgentClass resolved |
| Provisioning -> Running | Pod reports Ready, Service endpoint populated |
| Running -> Idle | `lastActivityTime` older than `idleTimeout` |
| Idle -> Running | Activity observed (see [Activity Detection](#activity-detection)) |
| Idle -> Hibernating | Idle for `hibernationDelay` (defaults from AgentClass) AND `hibernationEnabled` |
| Hibernating -> Hibernated | Pod scaled to 0, PVC retained, Service remains |
| Hibernated -> Resuming | Gateway [Activator](./GATEWAY_USER.md#activator) calls `POST /v1/activate/{namespace}/{name}` on the controller (triggered by a channel message arriving via the User Gateway for this Agent), OR `agentry.io/wake: "true"` annotation (manual override) |
| Resuming -> Running | Pod becomes Ready |
| any -> Degraded | Provider unavailable, quota exhausted, other recoverable issue |
| Degraded -> Running | Underlying condition resolved |
| any -> Failed | Unrecoverable error (image pull failure after retries, invalid config, persistent crash loop) |
| any -> Terminating | Deletion requested |

### Activity Detection

Activity timestamps are maintained **in-memory in the gateway**, not in etcd. This is critical for scale — at hundreds of thousands of agents, per-request annotation writes would overwhelm the API server. Two signal sources feed the gateway's in-memory activity store — see [Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api):
- **Gateway traffic**: the LLM Gateway and User Gateway record the timestamp of each request for an Agent in-memory.
- **Agent heartbeat**: the agent calls `POST /v1/agent/heartbeat` on the gateway; the gateway updates the agent's timestamp in its in-memory store.

The gateway exposes `GET /v1/activity?namespace={ns}` returning a map of agent names to last-activity timestamps. Because each gateway replica maintains its own in-memory store (updated only by the traffic it handles), the controller fans out this query to **all gateway Pod IPs in parallel** (enumerating them via its Pod informer) rather than hitting the ClusterIP Service, which would round-robin to one replica and miss activity recorded by the others. The controller takes the **most recent timestamp per agent** across all responses. Replicas that are unreachable are skipped; data from the remaining replicas is used. The `startedAt` field in each response is evaluated per-replica for restart detection. See [Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api) for the full fan-out protocol.

The reconciler evaluates `lastActivityTime` based on the Agent's `spec.lifecycle.activitySource` setting:
- `providerTraffic` (default): only gateway-observed LLM and channel traffic timestamps are considered.
- `agentHeartbeat`: only heartbeat timestamps are considered.
- `both`: the most recent timestamp from either source is used.

The reconciler updates `status.lastActivityTime` on the Agent only when a phase transition is warranted, avoiding unnecessary etcd writes.

**Gateway unavailability**: if all gateway replicas are unreachable, the controller preserves the Agent's current phase — no idle or hibernation transitions are made without activity data. The reconciler sets a `GatewayReachable=False` condition on affected Agents and requeues with backoff until the gateway recovers. If only some replicas are unreachable, the controller uses the data available from the reachable replicas.

**Gateway restart**: each replica's `/v1/activity` response includes a `startedAt` timestamp. If a replica's `startedAt` is more recent than the Agent's last known phase transition time (i.e., that replica restarted while the agent was Running), the controller treats that replica's missing activity data as "unknown" — it uses data from other replicas, or defers if all replicas have restarted. No idle or hibernation transitions are made until at least one replica has been running for `idleTimeout`, at which point missing activity data from that replica can be interpreted as genuine inactivity.

### Hibernation mechanics

Hibernation scales the Pod to zero by deleting the Pod and keeping the PVC. On wake, the controller recreates the Pod with the same PVC mount. The Service remains (with no endpoints) while the Agent is hibernated. Wake is triggered by the [User Gateway](./GATEWAY_USER.md#activator) (on channel message arrival) or manual annotation, not by traffic to the Service.

### Wake trigger

When an Agent is `Hibernated`, its ClusterIP Service has no endpoints — traffic is not routed. The gateway serves as the activator:
1. A channel message arrives at the User Gateway targeting a hibernated Agent (via AgentChannel).
2. The gateway calls `POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service.
3. The controller transitions the Agent to `Resuming` and creates the Pod.
4. The gateway holds the message and sends a "typing" or "processing" indicator to the channel platform while waiting. Once the Pod is Ready (bounded by `spec.lifecycle.wakeTimeout`, which defaults from AgentClass), the gateway delivers the message. If the timeout is exceeded, the gateway returns an appropriate error to the channel platform.

Manual wake is also supported via annotation: `kubectl annotate agent foo agentry.io/wake=true`.

### Spec change handling (Running Agent)

When a developer updates an Agent's spec while it is in `Running` or `Idle` phase, the controller detects spec drift by comparing the desired Pod spec (derived from the current Agent spec and AgentClass) against the existing Pod's spec. If drift is detected in immutable Pod fields (image, resources, command, args, env, providers), the controller recreates the Pod:

1. The Agent transitions to `Provisioning`.
2. The existing Pod is deleted (SIGTERM is sent; the agent has `terminationGracePeriodSeconds` to shut down).
3. The controller creates a new Pod with the updated spec.
4. Once the new Pod is Ready, the Agent transitions back to `Running`.

The PVC, Service, and ConfigMap are preserved — only the Pod is replaced. This is intentionally disruptive: the agent process restarts, but its persistent state (PVC) is retained.

Changes to mutable fields (labels, annotations, non-structural metadata) are patched in-place without Pod recreation.

If the Agent is `Hibernated`, spec changes are applied on the next wake — no Pod exists to recreate.

---

## AgentTask

```
       ┌─────────┐
       │ Pending │  initial
       └────┬────┘
            │ references validated
            ▼
       ┌─────────────┐
       │Provisioning │  creating Pod/PVC
       └────┬────────┘
            │ Pod Ready
            ▼
       ┌─────────┐
       │ Running │
       └────┬────┘
            │ completion signal OR timeout OR exit
            ▼
       ┌────────────┐
       │ Completing │  collecting artifacts, scheduling teardown
       └────┬───────┘
            │
            ├──── success reported ──▶ ┌───────────┐
            │                          │ Succeeded │
            │                          └───────────┘
            │
            ├──── failure reported ──▶ ┌────────┐
            │                          │ Failed │
            │                          └────────┘
            │
            └──── timeout hit ──▶ ┌──────────┐
                                  │ TimedOut │
                                  └──────────┘

Any state → Terminating (on delete or after TTL)
```

**Transition triggers:**

| From -> To | Trigger |
|---|---|
| Pending -> Provisioning | References valid |
| Provisioning -> Running | Pod Ready |
| Running -> Completing | Agent reports completion, container exits, or timeout hits |
| Completing -> Succeeded | Completion reported success AND artifacts collected |
| Completing -> Failed | Completion reported failure OR artifact collection failed OR container exited non-zero |
| Failed -> Provisioning | `backoffLimit > 0` AND `status.retries < backoffLimit` (retry — see below) |
| Completing -> TimedOut | Timeout hit before completion reported |
| Succeeded/Failed/TimedOut -> Terminating | TTL expired OR deletion requested |

**Completion detection** depends on `spec.completion.condition`:
- `agentReported`: the gateway receives [POST /v1/task/complete](./API_ENDPOINTS.md#post-v1taskcomplete-agenttask-only) from the agent container. The gateway writes the completion payload (status, message, and artifact key-values) to a ConfigMap named `{taskName}-completion` in the task's namespace. The reconciler watches for this ConfigMap and transitions to `Completing`. Using a ConfigMap (rather than a Pod annotation) ensures completion data survives Pod crashes or eviction between the agent's completion call and the reconciler's next pass.
- `exitCode`: the reconciler watches Pod phase; exit 0 -> Succeeded, non-zero -> Failed.

**Artifact collection** in `agentReported` mode: artifact values are embedded in the completion payload written by the agent. The reconciler reads them from the `{taskName}-completion` ConfigMap and writes them to `status.artifactValues`. No exec into the container is required. Artifacts exceeding the inline size limit (4 KiB per artifact, 32 KiB total) are stored in a separate auto-created ConfigMap and referenced by name in `status.artifactRefs`.

**Retry mechanics**: when `spec.completion.backoffLimit > 0` and the task transitions to `Failed` with `status.retries` below the limit:
1. The reconciler increments `status.retries`.
2. The existing Pod is deleted (it has already exited or will be terminated).
3. The `{taskName}-completion` ConfigMap is deleted to clear stale completion data.
4. The PVC is retained — the retry runs with the same scratch storage.
5. The task transitions back to `Provisioning` and a new Pod is created.
6. If the retry also fails and `status.retries` equals `backoffLimit`, the task remains in `Failed` as a terminal state.

---

## Finalizers

Each reconciler adds a finalizer to its resource on first reconciliation:

- `agentry.io/agent-finalizer` on Agent
- `agentry.io/task-finalizer` on AgentTask
- `agentry.io/provider-finalizer` on ModelProvider
- `agentry.io/class-finalizer` on AgentClass
- `agentry.io/channel-finalizer` on AgentChannel

**Finalizer duties:**

- **Agent**: on delete, gracefully terminate the Pod (send SIGTERM, wait up to `terminationGracePeriodSeconds`), optionally delete PVC per AgentClass reclaim policy.
- **AgentTask**: on delete, terminate the Pod, clean up ConfigMaps.
- **ModelProvider**: on delete, reject if any Agent or AgentTask still references it (reference resolution rules in [Cross-Resource Validation](./API_RESOURCES.md#cross-resource-validation)); otherwise remove gateway credential configuration.
- **AgentClass**: on delete, reject if any Agent or AgentTask still references it.
- **AgentChannel**: on delete, the reconciler coordinates with the gateway (see [AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler)):
  1. The reconciler sets `status.phase = Terminating` on the AgentChannel.
  2. The gateway sees the phase change via its watch and drops the platform connection.
  3. The gateway writes an `agentry.io/channel-disconnected: "true"` annotation on the AgentChannel to confirm disconnection.
  4. The reconciler watches for this annotation. Once observed (or after a bounded timeout of 30s if the gateway is unavailable), the reconciler removes the finalizer and the resource is deleted. The timeout prevents indefinite blocking if the gateway is down.

Finalizers prevent accidental deletion of cluster-scoped resources that would break running workloads.
