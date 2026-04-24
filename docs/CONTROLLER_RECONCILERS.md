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

**Activator handler (served on every replica).** The `POST /v1/activate/{namespace}/{agentName}` handler is served on every controller replica, not only the leader. The handler authenticates the caller (see [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api)), loads the target Agent, and patches `agentry.io/wake=true` onto it via the apiserver. The leader's existing Agent watch fires and runs the manual-wake path in `AgentReconciler` step 9 below, which transitions the Agent to `Resuming`. This avoids any leader-aware Service endpoint plumbing: a non-leader replica that handles the POST still drives the wake correctly. Non-leader replicas do not run reconcilers; they only need apiserver patch access for Agents, which the controller ServiceAccount already has.

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
5. Validate fallback chain: walk the full fallback chain (following each provider's `spec.fallback` recursively up to `maxFallbackDepth`) and confirm no circular references, all referenced providers exist, and all providers in the chain have the same `spec.type` as the primary. Emit `Ready=False` if invalid. The depth cap is a gateway-level setting; the reconciler validates the chain structure regardless of depth cap, since the cap may change without re-reconciling providers. Beyond the structural check, also scan each fallback candidate for **static eligibility violations** — a fallback whose `spec.allowedNamespaces` does not contain the primary's callers, or whose `spec.models` does not contain the primary's models — and emit a `Warning` event with `reason=FallbackIneligible` naming the offender. This is the reconcile-time counterpart to the gateway's runtime `FallbackIneligible` event (see [GATEWAY_LLM.md § Fallback Logic](./GATEWAY_LLM.md#fallback-logic)); the runtime event is defense in depth for races where config changes between reconcile and the next request.
6. Validate budget policies: confirm that every `budget.policies[x].degradeTo` value matches a model `id` in `spec.models`. If not, set `Ready=False, reason=InvalidDegradeTarget`. This catches misconfigured degrade targets before they silently fail at runtime when a budget threshold is crossed.
7. **Cost sanity on `degradeTo`**: for each policy whose `action: degrade`, compute `avgCost(model) = (costPer1MInputTokens + costPer1MOutputTokens) / 2` for the target model and compare against the same metric for every other model in `spec.models`. If the target is not strictly the cheapest, emit a `Warning` event with `reason=DegradeTargetNotCheapest` on the ModelProvider, naming the cheaper alternative (e.g., `"degradeTo=claude-opus-4-6 is not the cheapest model; claude-haiku-4-5 has a lower average cost"`). This is **advisory only** — it does **not** set `Ready=False`, because platform teams may have non-cost reasons (latency, capability, quality) to prefer a specific degrade target. The warning exists because a "degrade" policy labelled as cost-saving but pointing at a more expensive model is almost always a misconfiguration, and catching it at the reconciler is cheaper than discovering it from a monthly bill. Runs after step 6's existence check so that a missing `degradeTo` surfaces as the more serious `InvalidDegradeTarget` error without a contradictory cost warning.

The ModelProviderReconciler is **not** responsible for credential distribution to agent pods. Credentials are held in `agentry-system` Secrets and read directly by the gateway.

### AgentReconciler

Watches: `Agent`, plus owned `Pod`, `PVC`, `Service`, `ConfigMap`, `NetworkPolicy`, `ServiceAccount`, `cert-manager.io/v1/Certificate`. The cert-manager-managed `Secret` (`spec.secretName` output) is **not** owned by the reconciler — cert-manager owns it and populates it from the `Certificate`. Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc` — when an AgentClass changes (e.g., `allowedProviders` updated, `maxLimits` lowered), re-queue all Agents referencing that class (via indexed lookup on `agentClassRef.name`).

On every requeue triggered by an AgentClass change, the reconciler runs the **AgentClass-drift path** (see [CONTROLLER_LIFECYCLE.md § AgentClass change handling](./CONTROLLER_LIFECYCLE.md#agentclass-change-handling-running-agent)): re-derive the desired Pod spec under the new class invariants and (a) recreate the Pod via the standard Provisioning transition if the new spec differs in immutable Pod fields (recreate-and-clamp), or (b) transition the Agent to `Degraded` with `reason=ClassConstraintViolation` if the new invariants exclude the Agent's spec — `spec.image` no longer matches `image.allowedImages`, or any `providerRef` is no longer in `allowedProviders`. The image and provider checks reuse the existing cross-resource validation rules (rules 2 and 5 in [API_RESOURCES.md § Cross-Resource Validation](./API_RESOURCES.md#cross-resource-validation)) — they fail at reconcile and surface as `Degraded` rather than being silently re-applied to a recreated Pod.

This is the most complex reconciler. It implements the persistent [Agent State Machine](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode). Each reconciliation pass:

1. Resolve `agentClassRef` and fetch the AgentClass. If missing, set `Ready=False` with a clear reason.
2. If `spec.providers` is present, resolve all `providerRef`s. If any are missing or the Agent's namespace is not allowed, set `Degraded` with details.
3. Determine the desired phase based on the state machine.
4. **Create/ensure the cert-manager `Certificate` resource for the Agent before Pod creation.** Named `{agentName}-tls` in the Agent's namespace, owned by the Agent via `ownerReferences` so it is garbage-collected on Agent deletion. Key fields:
   - `spec.issuerRef`: `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`. A `ClusterIssuer` is used because cert-manager does not resolve a namespaced `Issuer` across namespace boundaries; the chart installs `agentry-ca-issuer` as a `ClusterIssuer` sourcing from the `agentry-ca` Secret in `agentry-system`.
   - `spec.secretName`: `{agentName}-tls` (the output Secret cert-manager creates in the Agent's namespace).
   - `spec.dnsNames`: `{agentName}.{namespace}.svc.cluster.local`, `{agentName}.{namespace}.svc`, `{agentName}.{namespace}`.
   - `spec.duration`: `2160h` (90d), `spec.renewBefore`: `720h` (30d) — chart defaults.
   - `spec.usages`: `server auth`, `client auth` (the same cert acts as the agent's serving cert and as its mTLS client cert when calling the gateway).

   **Pod creation is gated on `Certificate.status.conditions[type=Ready].status == True`.** If the cert is not yet ready (first-time issuance typically takes a few seconds), the reconciler requeues with backoff and **does not create the Pod** — otherwise the Pod would hang on its projected Secret mount until cert-manager caught up. Subsequent rotation is transparent: cert-manager rotates per `renewBefore`, kubelet propagates the new Secret contents into the Pod's projected volume, and the agent reloads via the file-watch pattern (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)). CA rotation requires no reconciler participation — cert-manager re-issues leaves as the root rotates and a trust-bundle overlap window covers leaves signed by either CA. See [TLS on the LLM Gateway Listener](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener).
5. **Resolve `imagePullSecrets`**: before creating the Pod, for each entry in the effective `AgentClass.spec.image.imagePullSecrets` (plus any Agent-level additions, once supported), verify that a Secret with that name exists in the **Agent's namespace**. AgentClass is cluster-scoped but the Secret is resolved per-workload in the Agent's namespace — the reconciler does **not** copy Secrets from `agentry-system` into user namespaces. If any referenced Secret is missing, set `Ready=False, reason=ImagePullSecretMissing` with a message naming the namespace and missing Secret (e.g., `"imagePullSecret 'registry-credentials' missing in namespace 'team-support'"`) and skip Pod creation. This check runs *before* the Pod is created so that a missing pull secret surfaces as a clear condition rather than as an `ImagePullBackOff` loop on a live Pod, and so the Pod is not left in a crash state consuming quota. See [API_RESOURCES.md § Cross-Resource Validation rule 23](./API_RESOURCES.md#cross-resource-validation).
6. Converge the remaining child resources to match the desired phase: `Pod`, `PVC`, `Service`, `ConfigMap`, `ServiceAccount`, `NetworkPolicy`. All are owner-referenced to the Agent for cascade GC.
   - **ServiceAccount**: named `agent-{agentName}` in the Agent's namespace, with no RoleBindings by default (the agent has no Kubernetes API access unless the platform team or developer explicitly grants it — see [SECURITY.md § Agent Pod ServiceAccount](./SECURITY.md#agent-pod-serviceaccount)). Created before the Pod so the Pod can reference it via `spec.serviceAccountName`.
   - **NetworkPolicy**: one policy per Agent, synthesized from AgentClass settings. The policy combines: (a) the default egress allow to the gateway Service on port 8443, (b) the DNS egress rule from the Helm value `controller.networkPolicy.dnsSelector`, (c) the default ingress allow from the gateway for `$AGENTRY_HEALTH_PORT`, (d) `AgentClass.spec.network.egress.allowedCIDRs` translated into `NetworkPolicy.egress.to.ipBlock.cidr` rules, (e) `AgentClass.spec.network.egress.allowedHosts` translated into `CiliumNetworkPolicy.toFQDNs` (or the Calico-Enterprise equivalent) **only** when the AgentClassReconciler's startup CNI probe reported FQDN-policy support — otherwise `allowedHosts` is silently ignored and the matching `Warning` event is emitted on the AgentClass, not the Agent, (f) inter-agent ingress only when `AgentClass.spec.network.allowSameNamespaceIngress: true`.
7. When creating a Pod, inject controller-managed environment variables:
   - `$AGENTRY_HEALTH_PORT` (always): the port the agent serves its HTTPS health/message endpoint on (default 8080).
   - `$AGENTRY_GATEWAY_ENDPOINT` (always): HTTPS URL pointing to the gateway Service in `agentry-system` on port 8443. This is the base URL for all agent→gateway calls — LLM requests, heartbeats (`POST /v1/agent/heartbeat`), and task completion (`POST /v1/task/complete`). Always injected regardless of whether `spec.providers` is set, so provider-less agents (e.g., AgentTasks that report completion but make no LLM calls) can still reach the gateway.
   - `$AGENTRY_CA_CERT` (always): path to the Agentry CA trust bundle projected into the Pod.
   - `$AGENTRY_TLS_CERT` and `$AGENTRY_TLS_KEY` (always): paths to the agent's TLS serving/client certificate and key (cert-manager-managed; see step 4).

   The controller also sets `httpGet.scheme: HTTPS` on the injected liveness and readiness probes — the agent serves TLS on `$AGENTRY_HEALTH_PORT`, and Kubernetes httpGet probes do not verify TLS certificates, so no additional CA configuration is required on the probe itself.
8. For agents in `Running` or `Idle` phase: query the activity API in a **per-namespace batch**, not per individual agent reconcile. Because each agent's reconcile is an independent controller-runtime invocation, a naive implementation would issue O(agents × replicas) gateway HTTP calls per reconcile cycle — at 1000 agents and 3 replicas, that is 3000+ calls per cycle. Instead, the reconciler caches the activity fan-out result for a namespace for a short window (e.g., 15 seconds): on the first reconcile of any agent in a namespace during this window, it fans out to all gateway Pod IPs in parallel (`GET /v1/activity?namespace={ns}`, one call per replica), takes the most recent timestamp per agent across all responses, and stores the result in a reconciler-local namespace-keyed cache. Subsequent agent reconciles in the same namespace within the window read from the cache rather than issuing new HTTP calls. This reduces gateway query load to O(namespaces × replicas) per window. Skips unreachable replicas. Uses the result to evaluate idle and hibernation transitions. See [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) for the full logic. (The HTTP client used for these per-Pod-IP dials sets `tls.Config.ServerName` to the gateway Service DNS so SAN verification succeeds against the Pod-IP target — see [GATEWAY_USER.md § Multi-replica fan-out](./GATEWAY_USER.md#activity-tracking-api).)
9. On every reconcile pass, check for the `agentry.io/wake=true` annotation. Removal is phase-dependent so a failed reconcile cannot silently drop the wake:
   - **Non-`Hibernated` agent with the annotation**: remove the annotation immediately and emit a Warning event (`reason=WakeIgnored, message="wake annotation observed on non-Hibernated agent; ignored"`). No phase change. This prevents stale annotations from triggering spurious wakes on a future hibernation cycle.
   - **`Hibernated` agent with the annotation**: transition to `Resuming` and recreate the Pod (see [Wake trigger](./CONTROLLER_LIFECYCLE.md#wake-trigger)). Remove the annotation **only after the transition to `Resuming` has been committed** (successful status update). If the status update or the subsequent Pod recreation fails and the reconcile is requeued, leave the annotation in place so the next reconcile observes it again. This ensures a wake signal from the activator (or from a manual `kubectl annotate`) cannot be lost to a transient apiserver error between the moment the reconciler sees the annotation and the moment it commits the phase change.
10. Update status and emit events for phase transitions.

Owner references are set on all child resources pointing back to the Agent, so cascade deletion works naturally.

### AgentTaskReconciler

Watches: `AgentTask`, plus owned `Pod`, `PVC`, `ConfigMap` (for artifacts), `NetworkPolicy`, `ServiceAccount`, and `cert-manager.io/v1/Certificate`. The cert-manager-managed `Secret` output is not owned by the reconciler — cert-manager owns it. Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc` — when an AgentClass changes, re-queue all AgentTasks referencing that class (via indexed lookup on `agentClassRef.name`).

On every requeue triggered by an AgentClass change, the reconciler runs the same **AgentClass-drift path** as AgentReconciler (see [CONTROLLER_LIFECYCLE.md § AgentClass change handling](./CONTROLLER_LIFECYCLE.md#agentclass-change-handling-running-agent)), scoped to in-flight tasks: AgentTasks in `Running` or `Provisioning` are subject to recreate-and-clamp or `Degraded, reason=ClassConstraintViolation` exactly as Agents are. AgentTasks already in `Succeeded`, `Failed`, or `TimedOut` are unaffected — terminal-state tasks ignore class drift and proceed to TTL-based cleanup.

Reconciliation steps:

1. Resolve AgentClass and ModelProviders (same validation as Agent). This also includes the **`imagePullSecrets` resolution check** from [AgentReconciler step 5](#agentreconciler): each entry in the effective `AgentClass.spec.image.imagePullSecrets` must exist as a Secret in the **AgentTask's namespace**. Missing Secrets set `Ready=False, reason=ImagePullSecretMissing` and skip Pod creation, so a missing pull secret surfaces as a clear condition rather than as an `ImagePullBackOff` loop on a live Pod — which is especially important for AgentTasks, since a failed task Pod counts against the task's `backoffLimit` (default 0) and can mark the task as `Failed` for a reason unrelated to the workload itself.
2. **Create/ensure the cert-manager `Certificate` resource for the AgentTask before Pod creation.** Named `{taskName}-tls` in the task's namespace, owner-referenced to the AgentTask so it is garbage-collected on deletion. Key fields:
   - `spec.issuerRef`: `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }` — the same `ClusterIssuer` used by Agents.
   - `spec.secretName`: `{taskName}-tls`.
   - `spec.dnsNames`: `{taskName}.{namespace}.task.agentry.io` (single SAN). A non-Service shape is used deliberately — AgentTasks have no Service, so the Service-DNS shape used by Agents would be misleading. The shape is recognized by the gateway's SAN parser as an AgentTask identity — see [Namespace Identification](./GATEWAY_LLM.md#namespace-identification).
   - `spec.usages`: `client auth` only (no `server auth` — tasks have no inbound TLS listener).
   - `spec.duration`: `2160h` (90d), `spec.renewBefore`: `720h` (30d) — matches Agent defaults.

   **Pod creation is gated on `Certificate.status.conditions[type=Ready].status == True`.** If the cert is not yet ready, the reconciler requeues with backoff and does not create the Pod — otherwise the Pod would hang on its projected Secret mount until cert-manager caught up, which for AgentTasks additionally risks counting the delay against `backoffLimit`. cert-manager owns and rotates the Secret afterwards; the reconciler does not track rotation state. The task image uses the same cert-reload pattern as Agents (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)).
3. Ensure a per-task `Role` and `RoleBinding` exist in the task's namespace, granting the gateway ServiceAccount (`agentry-system/agentry-gateway`) `create, update` on the ConfigMap named `{taskName}-completion`. Both resources are owned by the AgentTask via ownerRef and cascade-delete when the task is cleaned up. This mirrors the per-channel Role pattern in [AgentChannelReconciler](#agentchannelreconciler) and avoids granting the gateway blanket ConfigMap write access. See [Gateway ServiceAccount](./SECURITY.md#gateway-serviceaccount-permissions) for the security rationale.
4. Converge the remaining child resources in the task's namespace: `Pod`, `PVC`, `ServiceAccount`, `NetworkPolicy`. All are owner-referenced to the AgentTask for cascade GC.
   - **ServiceAccount**: named `task-{taskName}` with no RoleBindings by default (tasks have no Kubernetes API access unless explicitly granted). Created before the Pod so the Pod can reference it.
   - **NetworkPolicy**: synthesized the same way as for Agents (see [AgentReconciler step 6](#agentreconciler)) except that no gateway→task ingress rule is needed (tasks have no Service and are not delivery targets). The egress rules (gateway on 8443, DNS, AgentClass `allowedCIDRs`, optional `allowedHosts` on FQDN-capable CNIs) are identical to the Agent case.
5. Inject controller-managed environment variables on Pod creation — the same set as [AgentReconciler step 7](#agentreconciler): `$AGENTRY_HEALTH_PORT`, `$AGENTRY_GATEWAY_ENDPOINT`, `$AGENTRY_CA_CERT`, `$AGENTRY_TLS_CERT`, `$AGENTRY_TLS_KEY`. `$AGENTRY_GATEWAY_ENDPOINT` is always injected so the task image can call `POST /v1/task/complete` and emit heartbeats even if the task makes no LLM calls. Readiness and liveness probes are **not** injected by the controller for AgentTasks: tasks have no Service and typically do not serve a message endpoint, so probe configuration is left to the task image (the image may declare probes in its own Pod spec fields, which the reconciler preserves).
6. Drive the [AgentTask State Machine](./CONTROLLER_LIFECYCLE.md#agenttask).
7. On `Completing`: artifact values are read from a ConfigMap created by the gateway. When the agent calls `POST /v1/task/complete`, the gateway writes the completion payload to a ConfigMap named `{taskName}-completion` in the task's namespace. The ConfigMap is owned by the AgentTask (via ownerRef) for cascade deletion. The reconciler watches for this ConfigMap, reads artifact values, and populates `status.artifactValues`. No exec into the container is required, and the completion data survives Pod crashes or eviction.
8. Honor `ttlSecondsAfterFinished` by scheduling deletion.

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
