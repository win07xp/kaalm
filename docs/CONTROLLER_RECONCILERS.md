# Agentry — Controller & Reconciler Design

This document describes how the Agentry operator is structured and what each reconciler does. It covers reconciliation steps, error handling, events, and performance in enough detail that an implementer does not need to make architectural decisions.

For the state machines, finalizers, and lifecycle transitions driven by these reconcilers, see [CONTROLLER_LIFECYCLE.md](./CONTROLLER_LIFECYCLE.md). This document implements the CRDs defined in [API_RESOURCES.md](./API_RESOURCES.md).

## Operator Structure

The operator is a single binary built with `controller-runtime` (kubebuilder scaffolding is fine but not required). It hosts:

- Five reconcilers: `AgentClassReconciler`, `ModelProviderReconciler`, `AgentReconciler`, `AgentTaskReconciler`, `AgentChannelReconciler`.
- An activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) called by the gateway to trigger hibernated agent wake-up. This endpoint is exposed via a ClusterIP Service (`agentry-controller.agentry-system.svc.cluster.local`, default port 9443).
- A health/readiness endpoint (on the same Service).
- A metrics endpoint (Prometheus format) exposing controller internals (reconcile counts, errors, queue depth).

It runs as a Deployment in `agentry-system` with leader election enabled. Two replicas are recommended for availability; only the leader actively reconciles.

**No admission webhooks.** Field-level validation uses CEL expressions in CRD schemas (`x-kubernetes-validations`). Cross-resource validation runs at reconcile time and is surfaced as `Ready=False` status conditions with descriptive messages. This eliminates the cert-manager dependency and the availability risk of a webhook server.

---

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
3. Reconcile budget state: read per-replica partial spend counters from the gateway's budget ConfigMap in `agentry-system` — see [Budget State Management](./GATEWAY_LLM.md#budget-state-management) for the ConfigMap format. Before summing, cross-reference ConfigMap keys against the current set of gateway Pod names (from the gateway Deployment's Pods) and delete stale entries left by scaled-down or replaced replicas. Sum the remaining per-replica partials, write the canonical total to the `_canonical` key, and update `status.budgetUsage` per namespace. On budget period rollover (midnight UTC), archive the previous period's totals to ModelProvider status, delete all per-replica keys from the ConfigMap, and write a fresh `_canonical: {}`.
4. Health-check the gateway: the reconciler periodically (every 30s) probes the gateway Service's health endpoint. If unreachable, set `GatewayReachable=False` on the ModelProvider's status conditions. This signals that LLM traffic may be disrupted for all agents using this provider. When the gateway recovers, the condition is set back to `True`.
5. Validate fallback chain: walk the full fallback chain (following each provider's `spec.fallback` recursively up to `maxFallbackDepth`) and confirm no circular references, all referenced providers exist, and all providers in the chain have the same `spec.type` as the primary. Emit `Ready=False` if invalid. The depth cap is a gateway-level setting; the reconciler validates the chain structure regardless of depth cap, since the cap may change without re-reconciling providers.

The ModelProviderReconciler is **not** responsible for credential distribution to agent pods. Credentials are held in `agentry-system` Secrets and read directly by the gateway.

### AgentReconciler

Watches: `Agent`, plus owned `Pod`, `PVC`, `Service`, `ConfigMap`, `Secret` (TLS cert). Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc` — when an AgentClass changes (e.g., `allowedProviders` updated, `maxLimits` lowered), re-queue all Agents referencing that class (via indexed lookup on `agentClassRef.name`).

This is the most complex reconciler. It implements the persistent [Agent State Machine](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode). Each reconciliation pass:

1. Resolve `agentClassRef` and fetch the AgentClass. If missing, set `Ready=False` with a clear reason.
2. If `spec.providers` is present, resolve all `providerRef`s. If any are missing or the Agent's namespace is not allowed, set `Degraded` with details.
3. Determine the desired phase based on the state machine.
4. Converge child resources (Pod, PVC, Service, ConfigMap) to match the desired phase.
5. When creating a Pod, inject controller-managed environment variables: `$AGENTRY_HEALTH_PORT` (always), `$AGENTRY_PROVIDER_ENDPOINT` (only when `spec.providers` is non-empty, pointing at the gateway Service in `agentry-system`), `$AGENTRY_CA_CERT` (path to the operator CA certificate), `$AGENTRY_TLS_CERT` and `$AGENTRY_TLS_KEY` (paths to the agent's TLS serving certificate and key).
6. Create a TLS serving certificate Secret for the agent (signed by the operator-managed CA), owned by the Agent resource. Mount the cert and key into the Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The certificate's SAN includes the agent's Service DNS name (`{name}.{namespace}.svc.cluster.local`). Certificate lifetime and rotation follow the same policy as the gateway serving cert (90-day lifetime, rotate at 60 days) — see [TLS on LLM Gateway](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener). The controller watches the Secret and recreates it before expiry. During CA rotation, the controller re-issues agent certificates signed by the new CA in a rolling fashion — agents are re-queued and their certs updated over multiple reconcile cycles to avoid a thundering herd of certificate re-issuance. The CA bundle (containing both old and new CAs) ensures no TLS disruption during this rollout.
7. Update status and emit events for phase transitions.

Owner references are set on all child resources pointing back to the Agent, so cascade deletion works naturally.

### AgentTaskReconciler

Watches: `AgentTask`, plus owned `Pod`, `PVC`, `ConfigMap` (for artifacts). Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc` — when an AgentClass changes, re-queue all AgentTasks referencing that class (via indexed lookup on `agentClassRef.name`).

Reconciliation steps:

1. Resolve AgentClass and ModelProviders (same validation as Agent).
2. Drive the [AgentTask State Machine](./CONTROLLER_LIFECYCLE.md#agenttask).
3. On `Completing`: artifact values are read from a ConfigMap created by the gateway. When the agent calls `POST /v1/task/complete`, the gateway writes the completion payload to a ConfigMap named `{taskName}-completion` in the task's namespace. The ConfigMap is owned by the AgentTask (via ownerRef) for cascade deletion. The reconciler watches for this ConfigMap, reads artifact values, and populates `status.artifactValues`. No exec into the container is required, and the completion data survives Pod crashes or eviction.
4. Honor `ttlSecondsAfterFinished` by scheduling deletion.

### AgentChannelReconciler

Watches: `AgentChannel`, plus the referenced Agent and its Service.

Reconciliation steps:

1. Resolve `agentRef` — if the referenced Agent does not exist, set `Ready=False, reason=AgentNotFound`. Note: `agentRef` must reference an `Agent`, not an `AgentTask`. Tasks are ephemeral and lack a stable Service endpoint.
2. Verify the Agent has `spec.service.enabled: true`. If not, set `Ready=False, reason=AgentServiceDisabled`.
3. Validate that the `credentialsRef` Secret exists in the AgentChannel's namespace. If not, set `Ready=False, reason=CredentialsMissing`.
4. Poll channel health status from the gateway's internal status endpoint and update `status.conditions[type=PlatformConnected]`.
5. On Agent phase changes (e.g., Agent transitions to `Failed`), update `status.phase` to `Degraded` with a clear reason.

The AgentChannelReconciler does not own Pod resources. The gateway watches `AgentChannel` resources directly, reads the referenced credentials from user namespaces, and manages the live platform connections — see [User Gateway Request Flow](./GATEWAY_USER.md#user-gateway--request-flow). The reconciler's role is validation and status reporting.

---

## Error Handling

Errors are classified into three categories:

**Transient** (retry with backoff):
- API server conflicts (409)
- Transient Pod failures (crashloop with recent start)
- Network errors talking to ModelProvider for health checks

Handled by returning a `Requeue` result with exponential backoff (250ms -> 30s max).

**Recoverable** (set Degraded condition, continue reconciling):
- Referenced ModelProvider becomes unhealthy
- Budget exhaustion
- Namespace removed from provider allowlist

The Agent remains in its current phase with `Degraded` condition set. Reconciles continue on relevant resource events.

**Terminal** (set Failed phase, stop reconciling except on spec change):
- Image pull failure after max retries
- PVC provisioning failure that exceeds retry budget
- Invalid configuration that cannot be corrected

---

## Event Emission

The controller emits Kubernetes Events for:

- Phase transitions (`Normal`, reason=`PhaseChanged`, message includes old->new).
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
