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

**Activator handler (served on every replica).** The `POST /v1/activate/{namespace}/{agentName}` handler is served on every controller replica, not only the leader. The handler authenticates the caller (see [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api)), loads the target Agent, and patches `agentry.io/wake=true` onto it via the apiserver. The leader's existing Agent watch fires and runs the manual-wake path in `AgentReconciler` step 8 below, which transitions the Agent to `Resuming`. This avoids any leader-aware Service endpoint plumbing: a non-leader replica that handles the POST still drives the wake correctly. Non-leader replicas do not run reconcilers; they only need apiserver patch access for Agents, which the controller ServiceAccount already has.

**No admission webhooks.** Field-level validation uses CEL expressions in CRD schemas (`x-kubernetes-validations`). Cross-resource validation runs at reconcile time and is surfaced as `Ready=False` status conditions with descriptive messages. This eliminates the availability risk of a webhook server on the apiserver request path.

**Controller TLS.** The activator / activity-API / health endpoints on port 9443 serve HTTPS using a cert-manager-issued `Certificate` named `agentry-controller-tls` in `agentry-system`. The chart installs this `Certificate` with `issuerRef` → `agentry-ca-issuer` (the same `ClusterIssuer` that signs the gateway cert). Its SAN set covers `agentry-controller.agentry-system.svc.cluster.local`, `agentry-controller.agentry-system.svc`, and `localhost`. Usages are `server auth` **and** `client auth` — the controller serves TLS for inbound activator/health traffic, and it also presents the same cert as a client when dialing the gateway's `/v1/activity` endpoint (see [AgentReconciler](#agentreconciler) step 7). The gateway and controller mutually verify against the Agentry CA (`agentry-ca`). cert-manager rotates this cert continuously — no operator code is involved in its lifecycle. See [GATEWAY_LLM.md § TLS on the LLM Gateway Listener](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener) for the full trust chain.

---

## Reconciler Responsibilities

### AgentClassReconciler

Watches: `AgentClass`.

AgentClass has no owned child resources. Its reconciliation is lightweight:

1. Validate that all referenced `allowedProviders` exist (emit a `Ready=False` condition if any are missing).
2. Validate `network.egress.allowedCIDRs`: every entry must parse as a valid CIDR block (IPv4 or IPv6). Invalid entries set `Ready=False, reason=InvalidCIDR` with a message naming the offending entry.
3. Validate `network.egress.allowedHosts`: every entry must be a valid DNS name (RFC 1123). If `allowedHosts` is non-empty, check the cached result of the startup **CNI FQDN-policy probe** (see below). If the cluster's CNI does not support FQDN egress policies, emit a `Warning` event (`reason=FQDNPolicyUnsupported`, message naming the AgentClass and the unsupported entries) on the AgentClass and mark the condition `FQDNPolicySupported=False` on status. `allowedHosts` is **ignored** when the AgentClassReconciler (or any dependent reconciler) synthesizes per-agent NetworkPolicy — only `allowedCIDRs` is applied. The AgentClass itself still becomes `Ready=True`; the warning is actionable for the platform engineer but not blocking.
4. Count `Agent` and `AgentTask` resources currently referencing this class; populate `status.agentsInUse` and `status.tasksInUse`.
5. Update `status.conditions` accordingly.

**CNI FQDN-policy probe** (runs once at controller startup, result cached for the process lifetime): the reconciler checks the apiserver's discovery API for CRDs that indicate FQDN egress support:

- Cilium: presence of `ciliumnetworkpolicies.cilium.io` (v2) — supports `toFQDNs`.
- Calico Enterprise: presence of `networkpolicies.crd.projectcalico.org` **with** Enterprise licensing CRDs (`licensekeys.crd.projectcalico.org`) — open-source Calico does not support FQDN egress.

If neither is present, FQDN policy is unsupported. The probe is intentionally conservative — unknown CNIs are treated as unsupported to avoid silently generating NetworkPolicy that the CNI cannot enforce.

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

Watches: `Agent`, plus owned `Pod`, `PVC`, `Service`, `ConfigMap`, `cert-manager.io/v1/Certificate`. The cert-manager-managed `Secret` (`spec.secretName` output) is **not** owned by the reconciler — cert-manager owns it and populates it from the `Certificate`. Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc` — when an AgentClass changes (e.g., `allowedProviders` updated, `maxLimits` lowered), re-queue all Agents referencing that class (via indexed lookup on `agentClassRef.name`).

This is the most complex reconciler. It implements the persistent [Agent State Machine](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode). Each reconciliation pass:

1. Resolve `agentClassRef` and fetch the AgentClass. If missing, set `Ready=False` with a clear reason.
2. If `spec.providers` is present, resolve all `providerRef`s. If any are missing or the Agent's namespace is not allowed, set `Degraded` with details.
3. Determine the desired phase based on the state machine.
4. Converge child resources (Pod, PVC, Service, ConfigMap) to match the desired phase.
5. When creating a Pod, inject controller-managed environment variables:
   - `$AGENTRY_HEALTH_PORT` (always): the port the agent serves its HTTPS health/message endpoint on (default 8080).
   - `$AGENTRY_GATEWAY_ENDPOINT` (always): HTTPS URL pointing to the gateway Service in `agentry-system` on port 8443. This is the base URL for all agent→gateway calls — LLM requests, heartbeats (`POST /v1/agent/heartbeat`), and task completion (`POST /v1/task/complete`). Always injected regardless of whether `spec.providers` is set, so provider-less agents (e.g., AgentTasks that report completion but make no LLM calls) can still reach the gateway.
   - `$AGENTRY_CA_CERT` (always): path to the Agentry CA trust bundle projected into the Pod.
   - `$AGENTRY_TLS_CERT` and `$AGENTRY_TLS_KEY` (always): paths to the agent's TLS serving/client certificate and key (cert-manager-managed; see step 6).

   The controller also sets `httpGet.scheme: HTTPS` on the injected liveness and readiness probes — the agent serves TLS on `$AGENTRY_HEALTH_PORT`, and Kubernetes httpGet probes do not verify TLS certificates, so no additional CA configuration is required on the probe itself.
6. Create a cert-manager `Certificate` resource for the Agent, named `{agentName}-tls` in the Agent's namespace. The `Certificate` is owned by the Agent via `ownerReferences` so it is garbage-collected on Agent deletion. Key fields:
   - `spec.issuerRef`: `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`. A `ClusterIssuer` is used because cert-manager does not resolve a namespaced `Issuer` across namespace boundaries; the chart installs `agentry-ca-issuer` as a `ClusterIssuer` sourcing from the `agentry-ca` Secret in `agentry-system`.
   - `spec.secretName`: `{agentName}-tls` (the output Secret cert-manager creates in the Agent's namespace).
   - `spec.dnsNames`: `{agentName}.{namespace}.svc.cluster.local`, `{agentName}.{namespace}.svc`, `{agentName}.{namespace}`.
   - `spec.duration`: `2160h` (90d), `spec.renewBefore`: `720h` (30d) — chart defaults.
   - `spec.usages`: `server auth`, `client auth` (the same cert acts as the agent's serving cert and as its mTLS client cert when calling the gateway).

   The reconciler mounts the cert-manager-managed Secret into the Pod as a projected volume at `/var/run/agentry/` (`tls.crt`, `tls.key`). It does **not** create the Secret itself — cert-manager does — and it does not track cert rotation state. cert-manager rotates continuously per `renewBefore`; kubelet propagates the new Secret contents into the Pod's projected volume; the agent reloads via the file-watch pattern (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)).

   CA rotation requires no reconciler participation — cert-manager re-issues leaves as the root rotates and a trust-bundle overlap window covers leaves signed by either CA. See [TLS on the LLM Gateway Listener](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener).
7. For agents in `Running` or `Idle` phase: query the activity API in a **per-namespace batch**, not per individual agent reconcile. Because each agent's reconcile is an independent controller-runtime invocation, a naive implementation would issue O(agents × replicas) gateway HTTP calls per reconcile cycle — at 1000 agents and 3 replicas, that is 3000+ calls per cycle. Instead, the reconciler caches the activity fan-out result for a namespace for a short window (e.g., 15 seconds): on the first reconcile of any agent in a namespace during this window, it fans out to all gateway Pod IPs in parallel (`GET /v1/activity?namespace={ns}`, one call per replica), takes the most recent timestamp per agent across all responses, and stores the result in a reconciler-local namespace-keyed cache. Subsequent agent reconciles in the same namespace within the window read from the cache rather than issuing new HTTP calls. This reduces gateway query load to O(namespaces × replicas) per window. Skips unreachable replicas. Uses the result to evaluate idle and hibernation transitions. See [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) for the full logic.
8. On every reconcile pass, check for the `agentry.io/wake=true` annotation. Removal is phase-dependent so a failed reconcile cannot silently drop the wake:
   - **Non-`Hibernated` agent with the annotation**: remove the annotation immediately and emit a Warning event (`reason=WakeIgnored, message="wake annotation observed on non-Hibernated agent; ignored"`). No phase change. This prevents stale annotations from triggering spurious wakes on a future hibernation cycle.
   - **`Hibernated` agent with the annotation**: transition to `Resuming` and recreate the Pod (see [Wake trigger](./CONTROLLER_LIFECYCLE.md#wake-trigger)). Remove the annotation **only after the transition to `Resuming` has been committed** (successful status update). If the status update or the subsequent Pod recreation fails and the reconcile is requeued, leave the annotation in place so the next reconcile observes it again. This ensures a wake signal from the activator (or from a manual `kubectl annotate`) cannot be lost to a transient apiserver error between the moment the reconciler sees the annotation and the moment it commits the phase change.
9. Update status and emit events for phase transitions.

Owner references are set on all child resources pointing back to the Agent, so cascade deletion works naturally.

### AgentTaskReconciler

Watches: `AgentTask`, plus owned `Pod`, `PVC`, `ConfigMap` (for artifacts), and `cert-manager.io/v1/Certificate`. The cert-manager-managed `Secret` output is not owned by the reconciler — cert-manager owns it. Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc` — when an AgentClass changes, re-queue all AgentTasks referencing that class (via indexed lookup on `agentClassRef.name`).

Reconciliation steps:

1. Resolve AgentClass and ModelProviders (same validation as Agent).
1a. Ensure a per-task `Role` and `RoleBinding` exist in the task's namespace, granting the gateway ServiceAccount (`agentry-system/agentry-gateway`) `create, update` on the ConfigMap named `{taskName}-completion`. Both resources are owned by the AgentTask via ownerRef and cascade-delete when the task is cleaned up. This mirrors the per-channel Role pattern in [AgentChannelReconciler](#agentchannelreconciler) and avoids granting the gateway blanket ConfigMap write access. See [Gateway ServiceAccount](./SECURITY.md#gateway-serviceaccount) for the security rationale.
1b. Inject controller-managed environment variables on Pod creation — the same set as [AgentReconciler step 5](#agentreconciler): `$AGENTRY_HEALTH_PORT`, `$AGENTRY_GATEWAY_ENDPOINT`, `$AGENTRY_CA_CERT`, `$AGENTRY_TLS_CERT`, `$AGENTRY_TLS_KEY`. `$AGENTRY_GATEWAY_ENDPOINT` is always injected so the task image can call `POST /v1/task/complete` and emit heartbeats even if the task makes no LLM calls. Readiness and liveness probes are **not** injected by the controller for AgentTasks: tasks have no Service and typically do not serve a message endpoint, so probe configuration is left to the task image (the image may declare probes in its own Pod spec fields, which the reconciler preserves).
1c. Create a cert-manager `Certificate` resource for the AgentTask, named `{taskName}-tls` in the task's namespace, owner-referenced to the AgentTask so it is garbage-collected on deletion. Key fields:
   - `spec.issuerRef`: `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }` — the same `ClusterIssuer` used by Agents.
   - `spec.secretName`: `{taskName}-tls`.
   - `spec.dnsNames`: `{taskName}.{namespace}.task.agentry.io` (single SAN). A non-Service shape is used deliberately — AgentTasks have no Service, so the Service-DNS shape used by Agents would be misleading. The shape is recognized by the gateway's SAN parser as an AgentTask identity — see [Namespace Identification](./GATEWAY_LLM.md#namespace-identification).
   - `spec.usages`: `client auth` only (no `server auth` — tasks have no inbound TLS listener).
   - `spec.duration`: `2160h` (90d), `spec.renewBefore`: `720h` (30d) — matches Agent defaults.

   The reconciler mounts the cert-manager-managed Secret into the Pod as a projected volume at `/var/run/agentry/` (`tls.crt`, `tls.key`, `ca.crt` from the `trust-manager` bundle). cert-manager owns and rotates the Secret; the reconciler does not track rotation state. The task image uses the same cert-reload pattern as Agents (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)).
2. Drive the [AgentTask State Machine](./CONTROLLER_LIFECYCLE.md#agenttask).
3. On `Completing`: artifact values are read from a ConfigMap created by the gateway. When the agent calls `POST /v1/task/complete`, the gateway writes the completion payload to a ConfigMap named `{taskName}-completion` in the task's namespace. The ConfigMap is owned by the AgentTask (via ownerRef) for cascade deletion. The reconciler watches for this ConfigMap, reads artifact values, and populates `status.artifactValues`. No exec into the container is required, and the completion data survives Pod crashes or eviction.
4. Honor `ttlSecondsAfterFinished` by scheduling deletion.

### AgentChannelReconciler

Watches: `AgentChannel`, plus the referenced Agent and its Service.

Reconciliation steps:

1. Resolve `agentRef` — if the referenced Agent does not exist, set `Ready=False, reason=AgentNotFound`. Note: `agentRef` must reference an `Agent`, not an `AgentTask`. Tasks are ephemeral and lack a stable Service endpoint.
2. Verify the Agent has `spec.service.enabled: true`. If not, set `Ready=False, reason=AgentServiceDisabled`.
3. Validate that the webhook auth Secret exists in the AgentChannel's namespace. The Secret location depends on `spec.webhook.auth.type`: for `bearer`, it is `spec.webhook.auth.secretRef`; for `hmac`, it is `spec.webhook.auth.hmac.secretRef`. If the Secret is missing, set `Ready=False, reason=CredentialsMissing`. Ensure a Role named `agentry-channel-{channelName}-creds` exists in the AgentChannel's namespace, granting the gateway ServiceAccount `get, watch` on the specific Secret referenced by the active auth type. If the auth type or Secret reference has changed since the last reconcile (detectable by comparing the current Role's resource name rule against the new Secret name), the reconciler deletes the old Role before creating the new one — this prevents the gateway from retaining read access to a Secret it no longer needs. The Role name is deterministic so that successive reconciles are idempotent.
4. Poll channel health status from the gateway's `GET /v1/channels/health?namespace={ns}` endpoint (see [Channel Health Endpoint](./API_ENDPOINTS.md#get-v1channelshealth-internal--controller-use-only)), authenticating via mTLS with the controller's `agentry-controller-tls` client cert. The response maps channel paths to `{ phase, platformConnected, lastError }`. Update `status.conditions[type=PlatformConnected]` accordingly.
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

## Observability

The controller exposes Prometheus metrics on `:8080/metrics` (standard controller-runtime port).

Standard controller-runtime metrics (reconcile counts, duration, queue depth) are emitted automatically. The following Agentry-specific metrics are added:

- `agentry_agents_total{phase,namespace}` — gauge of Agent count by phase and namespace
- `agentry_tasks_total{phase,namespace}` — gauge of AgentTask count by phase and namespace
- `agentry_hibernations_total{namespace}` — counter of hibernation events
- `agentry_wakes_total{namespace,trigger}` — counter of wake events (trigger = `channel` | `annotation`)
- `agentry_budget_threshold_events_total{provider,namespace,action}` — counter of budget policy triggers (action = `degrade` | `block` | `warn`)

For gateway metrics (LLM and channel), see [GATEWAY_LLM.md](./GATEWAY_LLM.md#observability) and [GATEWAY_USER.md](./GATEWAY_USER.md#observability).

---

## Testing Strategy Notes

While detailed test guidance lives in the (deferred) contribution guide, the design assumes:

- Each reconciler is unit-testable by injecting a fake client.
- State machine transitions are table-testable.
- Integration tests use `envtest` for API server + etcd in-memory.
- End-to-end tests run against a kind cluster with a stubbed LLM provider (an HTTP server that responds with canned completions and reports fake token counts).

The controller should not hardcode assumptions about real LLM providers — testability depends on the gateway being swappable with a mock.
