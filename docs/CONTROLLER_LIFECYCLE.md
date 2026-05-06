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
| Pending -> Provisioning | References validated, AgentClass resolved, per-Agent `Certificate` created |
| Provisioning -> Running | Per-Agent `Certificate` reaches `Ready=True`, Pod is created and reports Ready, Service endpoint populated. Provisioning waits on the `Certificate` before creating the Pod so the Pod never hangs on a missing projected Secret — see [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) step 4. |
| Running -> Idle | `lastActivityTime` older than `idleTimeout` |
| Idle -> Running | Activity observed (see [Activity Detection](#activity-detection)) |
| Idle -> Hibernating | Idle for `hibernationDelay` (defaults from AgentClass) AND `hibernationEnabled` |
| Hibernating -> Hibernated | Pod scaled to 0, PVC retained, Service remains |
| Hibernated -> Resuming | Gateway [Activator](./GATEWAY_USER.md#activator) calls `POST /v1/activate/{namespace}/{name}` on the controller (triggered by a channel message arriving via the User Gateway for this Agent), OR `agentry.io/wake: "true"` annotation (manual override) |
| Resuming -> Running | Pod becomes Ready |
| Running -> Provisioning | Spec drift (Agent or AgentClass) re-derives a Pod spec that differs in immutable Pod fields. See [Spec change handling](#spec-change-handling-running-agent) and [AgentClass change handling](#agentclass-change-handling-running-agent). |
| any -> Degraded | Provider unavailable, quota exhausted, other recoverable issue, OR AgentClass change introduces a constraint that the Agent's spec violates (image no longer in `allowedImages`, provider no longer in `allowedProviders`). The controller records the current phase in `status.preDegradedPhase` before transitioning. |
| Degraded -> {pre-degradation phase} | Underlying condition resolved. The controller restores the phase the Agent was in before entering Degraded (tracked in `status.preDegradedPhase`). The idle clock is not reset — the controller evaluates idleness against the gateway's activity timestamp, which is continuous through the Degraded period. If the pre-degradation phase was `Idle` and `hibernationDelay` has since elapsed, the agent transitions to `Hibernating` on the next reconcile. |
| any -> Failed | Unrecoverable error (image pull failure after retries, invalid config, persistent crash loop) |
| any -> Terminating | Deletion requested |

### Activity Detection

Activity timestamps are maintained **in-memory in the gateway**, not in etcd. Per-request annotation writes would not scale as the Agent count grows: v1 targets 1000 Agents/AgentTasks per cluster, and the in-memory activity store is designed so future versions can reach an order of magnitude higher without a design change (at which point per-request etcd writes would dominate the API server). Two signal sources feed the gateway's in-memory activity store — see [Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api):
- **Gateway traffic**: the LLM Gateway and User Gateway record the timestamp of each request for an Agent in-memory.
- **Agent heartbeat**: the agent calls [`POST /v1/agent/heartbeat`](./API_ENDPOINTS.md#post-v1agentheartbeat-agent-only) on the gateway; the gateway updates the agent's timestamp in its in-memory store.

The gateway exposes `GET /v1/activity?namespace={ns}` returning a map of agent names to last-activity timestamps. Because each gateway replica maintains its own in-memory store (updated only by the traffic it handles), the controller fans out this query to **all gateway Pod IPs in parallel** (enumerating them via its Pod informer) rather than hitting the ClusterIP Service, which would round-robin to one replica and miss activity recorded by the others. The controller takes the **most recent timestamp per agent** across all responses. Replicas that are unreachable are skipped; data from the remaining replicas is used. The `replicaStartedAt` field in each response is evaluated per-replica for restart detection. See [Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api) for the full fan-out protocol.

The `/v1/activity` response returns both signal sources separately per agent (see [Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api)). The reconciler applies the `activitySource` filter from the Agent's `spec.lifecycle.activitySource` setting after merging results across all gateway replicas:
- `gatewayTraffic` (default): only the `gatewayTraffic` timestamp field is considered.
- `agentHeartbeat`: only the `heartbeat` timestamp field is considered.
- `both`: the most recent timestamp from either field is used.

The gateway returns both signal sources unconditionally — the controller (which already holds the Agent spec) owns the filtering decision. This avoids a dependency on the gateway watching Agent resources.

The reconciler updates `status.lastActivityTime` on the Agent only when a phase transition is warranted, avoiding unnecessary etcd writes.

**Gateway unavailability**: if all gateway replicas are unreachable, the controller preserves the Agent's current phase — no idle or hibernation transitions are made without activity data. The reconciler sets a `GatewayReachable=False` condition on affected Agents and requeues with backoff until the gateway recovers. If only some replicas are unreachable, the controller uses the data available from the reachable replicas.

**Gateway restart**: each replica's `/v1/activity` response includes a `replicaStartedAt` timestamp. The controller compares this against the Agent's `status.phaseTransitionTime` (set by the AgentReconciler on every phase change — see the [Agent CRD design notes](./API_RESOURCES.md#agent)). If a replica's `replicaStartedAt` is more recent than `status.phaseTransitionTime` (i.e., that replica restarted while the agent was Running), the controller treats that replica's missing activity data as "unknown" — it uses data from other replicas, or defers if all replicas have restarted. No idle or hibernation transitions are made until at least one replica has been running for `idleTimeout`, at which point missing activity data from that replica can be interpreted as genuine inactivity.

**Operational consequence:** a synchronized gateway restart (rollout, image deploy, chart upgrade) defers all idle and hibernation transitions for `idleTimeout`, since no replica satisfies the "up for `idleTimeout`" condition until that long has elapsed post-restart. Operators choosing multi-hour `idleTimeout` values should expect a corresponding window of deferred hibernation after every gateway restart.

### Hibernation mechanics

Hibernation scales the Pod to zero by deleting the Pod and keeping the PVC. On wake, the controller recreates the Pod with the same PVC mount. The Service remains (with no endpoints) while the Agent is hibernated. Wake is triggered by the [User Gateway](./GATEWAY_USER.md#activator) (on channel message arrival) or manual annotation, not by traffic to the Service.

### Wake trigger

When an Agent is `Hibernated`, its ClusterIP Service has no endpoints — traffic is not routed. The gateway serves as the activator:
1. A channel message arrives at the User Gateway targeting a hibernated Agent (via AgentChannel).
2. The gateway calls `POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service over HTTPS (the controller's activator endpoint is TLS-protected using a cert-manager-issued `Certificate` signed by the Agentry `ClusterIssuer`; the gateway verifies it against the Agentry CA). See [CONTROLLER_RECONCILERS.md § Controller TLS](./CONTROLLER_RECONCILERS.md#operator-structure).
3. The activator handler (served on every controller replica) patches `agentry.io/wake=true` on the target Agent via the apiserver. The leader's existing Agent watch fires, and the leader's `AgentReconciler` runs the manual-wake path (step 9) to transition the Agent to `Resuming` and recreate the Pod. The Service round-robins the POST across replicas, but any replica that receives it can drive the wake because the signal is an annotation on the resource rather than an in-memory call on the leader. See [CONTROLLER_RECONCILERS.md § Operator Structure](./CONTROLLER_RECONCILERS.md#operator-structure).
4. The gateway holds the message and sends a "typing" or "processing" indicator to the channel platform while waiting. Once the Pod is Ready (bounded by `spec.lifecycle.wakeTimeout`, which defaults from AgentClass), the gateway delivers the message. If the timeout is exceeded, the gateway returns an appropriate error to the channel platform.
5. **Controller unreachable**: if the gateway cannot reach the controller's activator endpoint at all, the wake is not attempted and the Agent remains `Hibernated`. The gateway surfaces this to the caller as an HTTP `504` (sync) or a `controller_unavailable` async error — see [GATEWAY_USER.md § Failure Modes](./GATEWAY_USER.md#failure-modes) for the full behavior.

Manual wake is also supported via annotation: `kubectl annotate agent foo agentry.io/wake=true`. Operational uses include pre-warming an agent before expected traffic or forcing a wake when no AgentChannel is configured. The AgentReconciler handles this annotation with phase-dependent removal so a failed reconcile cannot silently drop the wake:
- If the agent is in any non-`Hibernated` phase, the annotation is removed immediately and a Warning event (`reason=WakeIgnored`) is emitted without changing phase.
- If the agent is `Hibernated`, the reconciler transitions it to `Resuming` and recreates the Pod. The annotation is removed **only after** the transition to `Resuming` has been committed. If the status update or the subsequent Pod recreation fails and the reconcile is requeued, the annotation is left in place so the next reconcile pass can re-observe the wake intent.

See [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) step 9 for the implementation detail.

### Spec change handling (Running Agent)

When a developer updates an Agent's spec while it is in `Running` or `Idle` phase, the controller detects spec drift by comparing the desired Pod spec (derived from the current Agent spec and AgentClass) against the existing Pod's spec. If drift is detected in immutable Pod fields (image, resources, command, args, env, providers), the controller recreates the Pod:

1. The Agent transitions to `Provisioning`.
2. The existing Pod is deleted (SIGTERM is sent; the agent has `terminationGracePeriodSeconds` to shut down).
3. The controller creates a new Pod with the updated spec.
4. Once the new Pod is Ready, the Agent transitions back to `Running`.

The PVC, Service, and ConfigMap are preserved — only the Pod is replaced. This is intentionally disruptive: the agent process restarts, but its persistent state (PVC) is retained.

Changes to mutable fields (labels, annotations, non-structural metadata) are patched in-place without Pod recreation.

If the Agent is `Hibernated`, spec changes are applied on the next wake — no Pod exists to recreate.

### AgentClass change handling (Running Agent)

When an AgentClass field that affects already-provisioned child resources is changed (e.g., `resources.maxLimits` lowered, `security.podSecurityContext` tightened, `network.egress.allowedCIDRs` reduced, `image.allowedImages` narrowed, `allowedProviders` reduced), the AgentReconciler and AgentTaskReconciler re-queue every Agent and AgentTask referencing the class via their existing `EnqueueRequestsFromMapFunc` watches (see [CONTROLLER_RECONCILERS.md § AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) and [§ AgentTaskReconciler](./CONTROLLER_RECONCILERS.md#agenttaskreconciler)). For each affected **Agent** the reconciler chooses one of two paths (AgentTask handling is described below):

**Recreate-and-clamp (default).** If the new AgentClass invariants can be applied by re-deriving the desired Pod spec — for example, clamping `resources.limits` down to the new `maxLimits`, applying tighter `securityContext`, regenerating the per-Agent `NetworkPolicy` from the new egress rules — the reconciler transitions the Agent to `Provisioning`, deletes the existing Pod (graceful SIGTERM honoring `terminationGracePeriodSeconds`), and creates a new Pod with the adjusted spec. The PVC, Service, ConfigMap, and Certificate are preserved. This mirrors the existing Agent spec-drift behavior; agents must tolerate restart.

**Degrade-when-irreconcilable.** If the new invariants exclude the Agent's spec rather than just constrain its derived Pod spec — the workload's `spec.image` no longer matches `image.allowedImages`, or its `spec.providers` references a ModelProvider no longer in `allowedProviders` — the reconciler does not recreate the Pod. It transitions the Agent to `Degraded` with `reason=ClassConstraintViolation` and a message naming the offending field. The developer must update the Agent spec to comply; the controller resumes normal operation on the next reconcile after the Agent spec is reconciled with the class. The pre-Degraded phase is preserved in `status.preDegradedPhase` per the standard Degraded handling.

Hibernated Agents apply the new invariants on their next wake — recreation happens automatically as the wake path provisions a new Pod from the (now-clamped) desired spec. AgentTasks are not subject to mid-execution recreation: in-flight tasks (`Running` or `Provisioning`) finish under the class snapshot in effect at task-Pod creation time, and the new invariants take effect only when the task next provisions a Pod — either a backoff retry from `Failed` (subject to standard validation against the new class) or a subsequent AgentTask CR. Tasks already in `Succeeded`, `Failed`, or `TimedOut` are unaffected.

Bulk impact: tightening AgentClass policy on a class with many Agents triggers a rolling Pod restart of every affected Agent that falls into the recreate-and-clamp path (in-flight AgentTasks are not part of this rollout). Platform teams that need staged rollouts should split tightening across multiple AgentClasses (e.g., `standard-v2`) and migrate Agents incrementally rather than mutating an in-use class.

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
- `agentReported`: the gateway receives [POST /v1/task/complete](./API_ENDPOINTS.md#post-v1taskcomplete-agenttask-only) from the agent container. The gateway updates the pre-existing `{taskName}-completion` ConfigMap in the task's namespace (created by the AgentTaskReconciler at provisioning time) with the completion payload (status, message, and artifact key-values). The reconciler watches the ConfigMap for changes and transitions to `Completing` once the payload is populated. Using a ConfigMap (rather than a Pod annotation) ensures completion data survives Pod crashes or eviction between the agent's completion call and the reconciler's next pass.
- `exitCode`: the reconciler watches Pod phase; exit 0 -> Succeeded, non-zero -> Failed.

**Artifact collection** in `agentReported` mode: artifact values are embedded in the completion payload written by the agent. The reconciler reads them from the `{taskName}-completion` ConfigMap and writes them to `status.artifactValues`. No exec into the container is required. Oversize artifacts (>4 KiB per artifact or >32 KiB total) are rejected at the gateway with HTTP 413; agents must externalize large outputs (object storage, Git, etc.) and pass a reference URL inline. There is no auto-spill mechanism and no `status.artifactRefs` field.

**Retry mechanics**: when [`spec.completion.backoffLimit`](./API_RESOURCES.md#agenttask) `> 0` and the task transitions to `Failed` with `status.retries` below the limit:
1. The reconciler increments `status.retries`.
2. The existing Pod is deleted (it has already exited or will be terminated).
3. The `{taskName}-completion` ConfigMap is reset to `data: {}` (the reconciler patches it back to empty rather than deleting and re-creating, so the existing ownerRef and the gateway's name-scoped `update, patch` Role remain valid for the retry).
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

- **Agent**: on delete, gracefully terminate the Pod (send SIGTERM, wait up to `terminationGracePeriodSeconds`), then delete or retain the PVC per [`AgentClass.spec.persistence.pvcRetention`](./API_RESOURCES.md#agentclass). This field controls what happens to the per-Agent PVC when the Agent is deleted; it is distinct from `PersistentVolume.persistentVolumeReclaimPolicy` (which governs PV fate on PVC deletion) and the two operate independently.
- **AgentTask**: on delete, terminate the Pod, clean up ConfigMaps.
- **ModelProvider**: on delete, reject if any Agent or AgentTask still references it (reference resolution rules in [Cross-Resource Validation](./API_RESOURCES.md#cross-resource-validation)); otherwise remove [gateway credential configuration](./GATEWAY_LLM.md#credential-handling).
- **AgentClass**: on delete, reject if any Agent or AgentTask still references it.
- **AgentChannel**: on delete, the reconciler coordinates with the gateway (see [AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler)):
  1. The reconciler sets `status.phase = Terminating` on the AgentChannel.
  2. The gateway sees the phase change via its watch and drops the platform connection.
  3. The gateway writes an `agentry.io/channel-disconnected: "true"` annotation on the AgentChannel to confirm disconnection.
  4. The reconciler deletes all `agentry-async-*` ConfigMaps in `agentry-system` matching label selector `agentry.io/channel-namespace={ns},agentry.io/channel-name={name}`. This is explicit because cross-namespace ownerRefs do not trigger Kubernetes GC, so without this sweep the channel's stored async responses would be orphaned until their 1-hour annotation expiry.
  5. The reconciler watches for the disconnect annotation from step 3. Once observed (or after a bounded timeout of 30s if the gateway is unavailable), the reconciler removes the finalizer and the resource is deleted. The timeout prevents indefinite blocking if the gateway is down.

Finalizers prevent accidental deletion of cluster-scoped resources that would break running workloads.
