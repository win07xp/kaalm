# Agentry — Controller Design

This document describes how the Agentry operator implements the CRDs defined in the API Design doc. It covers reconciliation loops, state machines, error handling, and finalizers in enough detail that an implementer does not need to make architectural decisions.

## Operator Structure

The operator is a single binary built with `controller-runtime` (kubebuilder scaffolding is fine but not required). It hosts:

- Five reconcilers: `AgentClassReconciler`, `ModelProviderReconciler`, `AgentReconciler`, `AgentTaskReconciler`, `AgentChannelReconciler`.
- An activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) called by the gateway to trigger hibernated agent wake-up. This endpoint is exposed via a ClusterIP Service (`agentry-controller.agentry-system.svc.cluster.local`, default port 9443).
- A health/readiness endpoint (on the same Service).
- A metrics endpoint (Prometheus format) exposing controller internals (reconcile counts, errors, queue depth).

It runs as a Deployment in `agentry-system` with leader election enabled. Two replicas are recommended for availability; only the leader actively reconciles.

**No admission webhooks.** Field-level validation uses CEL expressions in CRD schemas (`x-kubernetes-validations`). Cross-resource validation runs at reconcile time and is surfaced as `Ready=False` status conditions with descriptive messages. This eliminates the cert-manager dependency and the availability risk of a webhook server.

## Reconciler Responsibilities

### AgentClassReconciler

Watches: `AgentClass`.

AgentClass has no owned child resources. Its reconciliation is lightweight:

1. Validate that all referenced `allowedProviders` exist (emit a `Ready=False` condition if any are missing).
2. Count `Agent` and `AgentTask` resources currently referencing this class; populate `status.agentsInUse` and `status.tasksInUse`.
3. Update `status.conditions` accordingly.

AgentClassReconciler must also watch `ModelProvider` (to re-evaluate when providers come and go) and `Agent`/`AgentTask` (to keep usage counts fresh). Use indexed lookups by `agentClassRef.name` for efficient fan-out.

### ModelProviderReconciler

Watches: `ModelProvider`, plus the referenced Secret (via `source.Kind` with a namespace filter).

Reconciliation steps:

1. Validate the referenced Secret exists and contains the expected key. If not, set `Ready=False, reason=CredentialsMissing`.
2. If health checks are enabled, dispatch a probe against the provider endpoint. Track result in `status.conditions[type=Healthy]` with exponential backoff on failures.
3. Reconcile budget state: read per-replica partial spend counters from the gateway (via a lightweight scrape of each gateway replica's internal metrics endpoint), sum them, compute percentages, and update `status.budgetUsage` per namespace. Write the canonical total back to a ConfigMap so gateway replicas can refresh their local counters on the next cycle.
4. Validate fallback chain: walk `spec.fallback` and confirm no circular references, all referenced providers exist. Emit `Ready=False` if invalid.

The ModelProviderReconciler is **not** responsible for credential distribution to agent pods. Credentials are held in `agentry-system` Secrets and read directly by the gateway.

### AgentReconciler

Watches: `Agent`, plus owned `Pod`, `PVC`, `Service`, `ConfigMap`.

This is the most complex reconciler. It implements the persistent Agent state machine described below. Each reconciliation pass:

1. Resolve `agentClassRef` and fetch the AgentClass. If missing, set `Ready=False` with a clear reason.
2. If `spec.providers` is present, resolve all `providerRef`s. If any are missing or the Agent's namespace is not allowed, set `Degraded` with details.
3. Determine the desired phase based on the state machine (see below).
4. Converge child resources (Pod, PVC, Service, ConfigMap) to match the desired phase.
5. When creating a Pod, inject controller-managed environment variables: `$AGENTRY_HEALTH_PORT` (always), `$AGENTRY_PROVIDER_ENDPOINT` (only when `spec.providers` is non-empty, pointing at the gateway Service in `agentry-system`).
6. Update status and emit events for phase transitions.

Owner references are set on all child resources pointing back to the Agent, so cascade deletion works naturally.

### AgentTaskReconciler

Watches: `AgentTask`, plus owned `Pod`, `PVC`, `ConfigMap` (for artifacts).

Reconciliation steps:

1. Resolve AgentClass and ModelProviders (same validation as Agent).
2. Drive the AgentTask state machine (see below).
3. On `Completing`: artifact values are read directly from the task completion payload posted by the agent to the gateway (`POST /v1/task/complete`). The gateway writes artifact values to an annotation on the Pod; the reconciler reads that annotation and populates `status.artifactValues`. No exec into the container is required.
4. Honor `ttlSecondsAfterFinished` by scheduling deletion.

### AgentChannelReconciler

Watches: `AgentChannel`, plus the referenced Agent and its Service.

Reconciliation steps:

1. Resolve `agentRef` — if the referenced Agent does not exist, set `Ready=False, reason=AgentNotFound`. Note: `agentRef` must reference an `Agent`, not an `AgentTask`. Tasks are ephemeral and lack a stable Service endpoint.
2. Verify the Agent has `spec.service.enabled: true`. If not, set `Ready=False, reason=AgentServiceDisabled`.
3. Validate that the `credentialsRef` Secret exists in the AgentChannel's namespace. If not, set `Ready=False, reason=CredentialsMissing`.
4. Poll channel health status from the gateway's internal status endpoint and update `status.conditions[type=PlatformConnected]`.
5. On Agent phase changes (e.g., Agent transitions to `Failed`), update `status.phase` to `Degraded` with a clear reason.

The AgentChannelReconciler does not own Pod resources. The gateway watches `AgentChannel` resources directly, reads the referenced credentials from user namespaces, and manages the live platform connections. The reconciler's role is validation and status reporting.

---

## State Machines

### Agent (persistent mode)

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
          │            │ AND idle for grace period
          │            ▼
          │      ┌─────────────┐
          │      │ Hibernating │   scaling pod to zero
          │      └──────┬──────┘
          │             │ pod scaled to 0, PVC retained
          │             ▼
          │      ┌────────────┐
          │      │ Hibernated │──── incoming request on Service
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

| From → To | Trigger |
|---|---|
| Pending → Provisioning | References validated, AgentClass resolved |
| Provisioning → Running | Pod reports Ready, Service endpoint populated |
| Running → Idle | `lastActivityTime` older than `idleTimeout` |
| Idle → Running | Activity observed (see Activity Detection) |
| Idle → Hibernating | Idle for 2x idleTimeout AND `hibernationEnabled` |
| Hibernating → Hibernated | Pod scaled to 0, PVC retained, Service remains |
| Hibernated → Resuming | Gateway activator calls `POST /v1/activate/{namespace}/{name}` on the controller (triggered by a channel message arriving via the User Gateway for this Agent), OR `agentry.io/wake: "true"` annotation (manual override) |
| Resuming → Running | Pod becomes Ready |
| any → Degraded | Provider unavailable, quota exhausted, other recoverable issue |
| Degraded → Running | Underlying condition resolved |
| any → Failed | Unrecoverable error (image pull failure after retries, invalid config, persistent crash loop) |
| any → Terminating | Deletion requested |

**Activity Detection:**

Activity is updated in `status.lastActivityTime` via Pod annotations written by the gateway. Two signal sources exist:
- **Gateway traffic**: the LLM Gateway or User Gateway reports each request for an Agent by writing a timestamp to a controller-managed annotation (`agentry.io/last-traffic`) on the Agent Pod.
- **Agent heartbeat**: the agent calls `POST /v1/agent/heartbeat` on the gateway; the gateway writes a timestamp to a separate annotation (`agentry.io/last-heartbeat`) on the Pod.

The reconciler reads these annotations and updates `lastActivityTime` based on the Agent's `spec.lifecycle.activitySource` setting:
- `providerTraffic` (default): only the `agentry.io/last-traffic` annotation is considered. Agent heartbeats are ignored for idle detection.
- `agentHeartbeat`: only the `agentry.io/last-heartbeat` annotation is considered. Gateway traffic timestamps are ignored.
- `both`: the most recent timestamp from either annotation is used.

This avoids tight coupling to the Agent CRD for a high-frequency field while giving developers control over what constitutes "activity" for their agent.

**Hibernation mechanics:**

Hibernation scales the Pod to zero by deleting the Pod and keeping the PVC. On wake, the controller recreates the Pod with the same PVC mount. The Service remains pointing at the gateway's activator listener while the Agent is hibernated.

**Wake trigger:**

When an Agent is `Hibernated`, its ClusterIP Service has no endpoints — traffic is not routed. The gateway serves as the activator:
1. A channel message arrives at the User Gateway targeting a hibernated Agent (via AgentChannel).
2. The gateway calls `POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service.
3. The controller transitions the Agent to `Resuming` and creates the Pod.
4. The gateway holds the message and sends a "typing" or "processing" indicator to the channel platform while waiting. Once the Pod is Ready (bounded by a configurable timeout), the gateway delivers the message.

Manual wake is also supported via annotation: `kubectl annotate agent foo agentry.io/wake=true`.

### AgentTask

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

| From → To | Trigger |
|---|---|
| Pending → Provisioning | References valid |
| Provisioning → Running | Pod Ready |
| Running → Completing | Agent reports completion, container exits, or timeout hits |
| Completing → Succeeded | Completion reported success AND artifacts collected |
| Completing → Failed | Completion reported failure OR artifact collection failed OR container exited non-zero |
| Completing → TimedOut | Timeout hit before completion reported |
| Succeeded/Failed/TimedOut → Terminating | TTL expired OR deletion requested |

**Completion detection** depends on `spec.completion.condition`:
- `agentReported`: the gateway receives `POST /v1/task/complete` from the agent container. The gateway writes the completion payload (status, message, and artifact key-values) to the Pod annotation `agentry.io/task-status`. The reconciler watches for this annotation and transitions to `Completing`.
- `exitCode`: the reconciler watches Pod phase; exit 0 → Succeeded, non-zero → Failed.

**Artifact collection** in `agentReported` mode: artifact values are embedded in the completion payload written by the agent. The reconciler reads them from the `agentry.io/task-status` Pod annotation and writes them to `status.artifactValues`. No exec into the container is required. Artifacts exceeding the inline size limit (4 KiB per artifact, 32 KiB total) are stored in an auto-created ConfigMap and referenced by name in `status.artifactRefs`.

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
- **ModelProvider**: on delete, reject if any Agent or AgentTask still references it; otherwise remove gateway credential configuration.
- **AgentClass**: on delete, reject if any Agent or AgentTask still references it.
- **AgentChannel**: on delete, the gateway detects the AgentChannel resource removal (via its watch) and drops the platform connection.

Finalizers prevent accidental deletion of cluster-scoped resources that would break running workloads.

---

## Error Handling

Errors are classified into three categories:

**Transient** (retry with backoff):
- API server conflicts (409)
- Transient Pod failures (crashloop with recent start)
- Network errors talking to ModelProvider for health checks

Handled by returning a `Requeue` result with exponential backoff (250ms → 30s max).

**Recoverable** (set Degraded condition, continue reconciling):
- Referenced ModelProvider becomes unhealthy
- Budget exhaustion
- Namespace removed from provider allowlist

The Agent remains in its current phase with `Degraded` condition set. Reconciles continue on relevant resource events.

**Terminal** (set Failed phase, stop reconciling except on spec change):
- AgentClass deleted while Agent still references it
- Image pull failure after max retries
- PVC provisioning failure that exceeds retry budget
- Invalid configuration that cannot be corrected

---

## Event Emission

The controller emits Kubernetes Events for:

- Phase transitions (`Normal`, reason=`PhaseChanged`, message includes old→new).
- Provider errors (`Warning`, reason=`ProviderUnhealthy` or `BudgetExhausted`).
- Validation failures caught at reconcile time (`Warning`, reason=`InvalidReference`).
- Hibernation/wake events (`Normal`, reason=`Hibernated` / `Woken`).
- Task completion (`Normal`, reason=`TaskSucceeded` or `TaskFailed`).

Events are critical for `kubectl describe` usability. Err toward emitting events on every meaningful state change.

---

## Reconcile Interval and Performance

- Default reconcile requeue: 5 minutes (for periodic health/budget re-evaluation when no events trigger).
- Event-driven reconciles: immediate.
- AgentTask timeout checking: requeue at `startTime + timeout + small jitter` when in Running state.
- Idle detection: requeue at `lastActivityTime + idleTimeout` when in Running state.

The operator should handle 1000+ Agents and AgentTasks per cluster without issue. Use indexed caches for all cross-resource lookups.

---

## Testing Strategy Notes

While detailed test guidance lives in the (deferred) contribution guide, the design assumes:

- Each reconciler is unit-testable by injecting a fake client.
- State machine transitions are table-testable.
- Integration tests use `envtest` for API server + etcd in-memory.
- End-to-end tests run against a kind cluster with a stubbed LLM provider (an HTTP server that responds with canned completions and reports fake token counts).

The controller should not hardcode assumptions about real LLM providers — testability depends on the gateway being swappable with a mock.