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
2. If health checks are enabled, dispatch a **lightweight provider-specific liveness probe** against the provider endpoint — not a real inference request. The probe uses the provider's model-list endpoint, which requires authentication but consumes no tokens: Anthropic and OpenAI/OpenAI-compatible adapters send `GET /v1/models`; Google Vertex uses the provider-specific health URL TBD per adapter. A 2xx response sets `Healthy=True`. A 4xx response (including 401 Unauthorized) sets `Healthy=False` and, for 401 specifically, also sets `Ready=False, reason=CredentialsInvalid` — this is the signal for a valid-but-wrong credential. A network error or 5xx sets `Healthy=False` without changing `Ready`. Track result in `status.conditions[type=Healthy]` with exponential backoff on failures.
3. Reconcile budget state: read per-replica partial spend counters from the gateway's budget ConfigMap in `agentry-system` — see [Budget State Management](./GATEWAY_LLM.md#budget-state-management) for the ConfigMap format. Before summing, cross-reference ConfigMap keys against the current set of gateway Pod names (from the gateway Deployment's Pods) and delete stale entries left by scaled-down or replaced replicas. Sum the remaining per-replica partials, write the canonical total to the `_canonical` key, and update `status.budgetUsage` per namespace. On budget period rollover (midnight UTC), archive the previous period's totals to ModelProvider status, delete all per-replica keys from the ConfigMap, and write a fresh `_canonical: {}`.
4. Health-check the gateway: the reconciler periodically (every 30s) probes the gateway Service's health endpoint. If unreachable, set `GatewayReachable=False` on the ModelProvider's status conditions. This signals that LLM traffic may be disrupted for all agents using this provider. When the gateway recovers, the condition is set back to `True`.
5. Validate fallback chain: walk the full fallback chain (following each provider's `spec.fallback` recursively up to `maxFallbackDepth`) and confirm no circular references, all referenced providers exist, and all providers in the chain have the same `spec.type` as the primary. Emit `Ready=False` if invalid. The depth cap is a gateway-level setting; the reconciler validates the chain structure regardless of depth cap, since the cap may change without re-reconciling providers.
6. Validate budget policies: confirm that every `budget.policies[x].degradeTo` value matches a model `id` in `spec.models`. If not, set `Ready=False, reason=InvalidDegradeTarget`. This catches misconfigured degrade targets before they silently fail at runtime when a budget threshold is crossed.

The ModelProviderReconciler is **not** responsible for credential distribution to agent pods. Credentials are held in `agentry-system` Secrets and read directly by the gateway.

### AgentReconciler

Watches: `Agent`, plus owned `Pod`, `PVC`, `Service`, `ConfigMap`, `Secret` (TLS cert). Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc` — when an AgentClass changes (e.g., `allowedProviders` updated, `maxLimits` lowered), re-queue all Agents referencing that class (via indexed lookup on `agentClassRef.name`).

This is the most complex reconciler. It implements the persistent [Agent State Machine](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode). Each reconciliation pass:

1. Resolve `agentClassRef` and fetch the AgentClass. If missing, set `Ready=False` with a clear reason.
2. If `spec.providers` is present, resolve all `providerRef`s. If any are missing or the Agent's namespace is not allowed, set `Degraded` with details.
3. Determine the desired phase based on the state machine.
4. Converge child resources (Pod, PVC, Service, ConfigMap) to match the desired phase.
5. When creating a Pod, inject controller-managed environment variables:
   - `$AGENTRY_HEALTH_PORT` (always): the port the agent serves its HTTPS health/message endpoint on (default 8080).
   - `$AGENTRY_GATEWAY_ENDPOINT` (always): HTTPS URL pointing to the gateway Service in `agentry-system` on port 8443. This is the base URL for all agent→gateway calls — LLM requests, heartbeats (`POST /v1/agent/heartbeat`), and task completion (`POST /v1/task/complete`). Always injected regardless of whether `spec.providers` is set, so provider-less agents (e.g., AgentTasks that report completion but make no LLM calls) can still reach the gateway.
   - `$AGENTRY_CA_CERT` (always): path to the operator CA certificate.
   - `$AGENTRY_TLS_CERT` and `$AGENTRY_TLS_KEY` (always): paths to the agent's TLS serving certificate and key.

   The controller also sets `httpGet.scheme: HTTPS` on the injected liveness and readiness probes — the agent serves TLS on `$AGENTRY_HEALTH_PORT`, and Kubernetes httpGet probes do not verify TLS certificates, so no additional CA configuration is required on the probe itself.
6. Create a TLS serving certificate Secret for the agent (signed by the operator-managed CA), owned by the Agent resource. Mount the cert and key into the Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The certificate's SAN includes the agent's Service DNS name (`{name}.{namespace}.svc.cluster.local`). Certificate lifetime and rotation follow the same policy as the gateway serving cert (90-day lifetime, rotate at 60 days) — see [TLS on LLM Gateway](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener). The controller watches the Secret and recreates it before expiry. During CA rotation, the controller re-issues agent certificates signed by the new CA in a **rate-limited rolling fashion**: at most 50 agents per reconcile cycle (configurable), tracked via a ConfigMap in `agentry-system` (`agentry-ca-rotation-state`). The CA bundle (containing both old and new CAs) ensures no TLS disruption during the rollout — the old CA is not removed from the bundle until the reconciler has confirmed that every agent cert Secret is signed by the new CA. See [CA Rotation](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener) for the full 4-step rotation sequence.
7. For agents in `Running` or `Idle` phase: query the activity API in a **per-namespace batch**, not per individual agent reconcile. Because each agent's reconcile is an independent controller-runtime invocation, a naive implementation would issue O(agents × replicas) gateway HTTP calls per reconcile cycle — at 1000 agents and 3 replicas, that is 3000+ calls per cycle. Instead, the reconciler caches the activity fan-out result for a namespace for a short window (e.g., 15 seconds): on the first reconcile of any agent in a namespace during this window, it fans out to all gateway Pod IPs in parallel (`GET /v1/activity?namespace={ns}`, one call per replica), takes the most recent timestamp per agent across all responses, and stores the result in a reconciler-local namespace-keyed cache. Subsequent agent reconciles in the same namespace within the window read from the cache rather than issuing new HTTP calls. This reduces gateway query load to O(namespaces × replicas) per window. Skips unreachable replicas. Uses the result to evaluate idle and hibernation transitions. See [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) for the full logic.
8. On every reconcile pass, check for the `agentry.io/wake=true` annotation **unconditionally**. If present on a `Hibernated` agent, transition to `Resuming` and recreate the Pod (see [Wake trigger](./CONTROLLER_LIFECYCLE.md#wake-trigger)). If present on an agent in any other phase, remove the annotation immediately and emit a Warning event (`reason=WakeIgnored, message="wake annotation observed on non-Hibernated agent; ignored"`). The annotation is always removed after being observed — it must never be left on a resource after a reconcile pass, regardless of phase. This prevents stale annotations from triggering spurious wakes on a future hibernation cycle.
9. Update status and emit events for phase transitions.

Owner references are set on all child resources pointing back to the Agent, so cascade deletion works naturally.

### AgentTaskReconciler

Watches: `AgentTask`, plus owned `Pod`, `PVC`, `ConfigMap` (for artifacts). Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc` — when an AgentClass changes, re-queue all AgentTasks referencing that class (via indexed lookup on `agentClassRef.name`).

Reconciliation steps:

1. Resolve AgentClass and ModelProviders (same validation as Agent).
1a. Ensure a per-task `Role` and `RoleBinding` exist in the task's namespace, granting the gateway ServiceAccount (`agentry-system/agentry-gateway`) `create, update` on the ConfigMap named `{taskName}-completion`. Both resources are owned by the AgentTask via ownerRef and cascade-delete when the task is cleaned up. This mirrors the per-channel Role pattern in [AgentChannelReconciler](#agentchannelreconciler) and avoids granting the gateway blanket ConfigMap write access. See [Gateway ServiceAccount](./SECURITY.md#gateway-serviceaccount) for the security rationale.
2. Drive the [AgentTask State Machine](./CONTROLLER_LIFECYCLE.md#agenttask).
3. On `Completing`: artifact values are read from a ConfigMap created by the gateway. When the agent calls `POST /v1/task/complete`, the gateway writes the completion payload to a ConfigMap named `{taskName}-completion` in the task's namespace. The ConfigMap is owned by the AgentTask (via ownerRef) for cascade deletion. The reconciler watches for this ConfigMap, reads artifact values, and populates `status.artifactValues`. No exec into the container is required, and the completion data survives Pod crashes or eviction.
4. Honor `ttlSecondsAfterFinished` by scheduling deletion.

### AgentChannelReconciler

Watches: `AgentChannel`, plus the referenced Agent and its Service.

Reconciliation steps:

1. Resolve `agentRef` — if the referenced Agent does not exist, set `Ready=False, reason=AgentNotFound`. Note: `agentRef` must reference an `Agent`, not an `AgentTask`. Tasks are ephemeral and lack a stable Service endpoint.
2. Verify the Agent has `spec.service.enabled: true`. If not, set `Ready=False, reason=AgentServiceDisabled`.
3. Validate that the `credentialsRef` Secret exists in the AgentChannel's namespace. If not, set `Ready=False, reason=CredentialsMissing`. Ensure a Role named `agentry-channel-{channelName}-creds` exists in the AgentChannel's namespace, granting the gateway ServiceAccount `get, watch` on the specific Secret named in `credentialsRef`. If `credentialsRef` has changed since the last reconcile (detectable by comparing the current Role's resource name rule against the new Secret name), the reconciler deletes the old Role before creating the new one — this prevents the gateway from retaining read access to a Secret it no longer needs. The Role name is deterministic so that successive reconciles are idempotent.
4. Poll channel health status from the gateway's `GET /v1/channels/health?namespace={ns}` endpoint (see [Channel Health Endpoint](./API_ENDPOINTS.md#get-v1channelshealth-internal--controller-use-only)) using the shared HMAC key (`agentry-activator-key` Secret). The response maps channel paths to `{ phase, platformConnected, lastError }`. Update `status.conditions[type=PlatformConnected]` accordingly.
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
