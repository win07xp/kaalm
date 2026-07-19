# Reconcilers

The operator hosts five reconcilers, one per CRD. This page is the implementation spec for each: what it watches, and what every reconciliation pass does, step by step.

Read them in dependency order. `AgentClassReconciler` and `ModelProviderReconciler` validate the platform-level resources that the workload reconcilers depend on. `AgentReconciler` is the most complex: it owns the full child-resource tree for a persistent agent. `AgentTaskReconciler` mirrors it for one-shot work, and `AgentChannelReconciler` owns no Pods at all: it validates, scopes credential access, and reports status.

For the state machines and transitions these reconcilers drive, see [Agent Lifecycle](agent-lifecycle.md). For the CRDs they implement, see [Resource Overview](../resources/overview.md).

## AgentClassReconciler

Watches: `AgentClass`.

AgentClass has no owned child resources, so its reconciliation is lightweight: validate the class, count its users, write status.

1. Validate that all referenced `allowedProviders` exist (emit a `Ready=False` condition if any are missing).
2. Validate `network.egress.allowedCIDRs`: every entry must parse as a valid CIDR block (IPv4 or IPv6). Invalid entries set `Ready=False, reason=InvalidCIDR` with a message naming the offending entry.
3. Validate `network.egress.allowedHosts`: every entry must be a valid DNS name (RFC 1123). If `allowedHosts` is non-empty, check the cached result of the startup **CNI FQDN-policy probe** (see below). If the cluster's CNI does not support FQDN egress policies, emit a `Warning` event (`reason=FQDNPolicyUnsupported`, message naming the AgentClass and the unsupported entries) on the AgentClass and mark the condition `FQDNPolicySupported=False` on status. `allowedHosts` is **ignored** when the AgentClassReconciler (or any dependent reconciler) synthesizes per-agent NetworkPolicy: only `allowedCIDRs` is applied. The AgentClass itself still becomes `Ready=True`; the warning is actionable for the platform engineer but not blocking.
4. Count `Agent` and `AgentTask` resources currently referencing this class; populate `status.agentsInUse` and `status.tasksInUse`.
5. Update `status.conditions` accordingly.

### CNI FQDN-policy probe

The probe runs once at controller startup and its result is cached for the process lifetime. It checks the apiserver's discovery API for CRDs that indicate FQDN egress support:

- Cilium: presence of `ciliumnetworkpolicies.cilium.io` (v2), which supports `toFQDNs`.
- Calico Enterprise: presence of `networkpolicies.crd.projectcalico.org` **with** Enterprise licensing CRDs (`licensekeys.crd.projectcalico.org`). Open-source Calico does not support FQDN egress.

If neither is present, FQDN policy is unsupported. The probe is intentionally conservative: unknown CNIs are treated as unsupported, to avoid silently generating NetworkPolicy that the CNI cannot enforce. Because the probe runs only at startup, a mid-flight CNI upgrade or replacement is not picked up until the controller is restarted, so operators who change their CNI should roll the controller Deployment afterwards.

### Additional watches

AgentClassReconciler must also watch `ModelProvider` (to re-evaluate when providers come and go) and `Agent`/`AgentTask` (to keep usage counts fresh). Use indexed lookups by `agentClassRef.name` for efficient fan-out.

## ModelProviderReconciler

Watches: `ModelProvider`, plus the referenced Secret (via `source.Kind` with a namespace filter).

Reconciliation steps:

1. Validate the referenced Secret exists and contains the expected key. If not, set `Ready=False, reason=CredentialsMissing`.
2. If health checks are enabled, dispatch a **lightweight provider-specific liveness probe** against the provider endpoint, not a real inference request. See [Liveness probe](#liveness-probe) below.
3. Reconcile budget state. See [Budget reconciliation](#budget-reconciliation) below.
4. Set the gateway-reachability condition: read the live count of Ready gateway Pods in `agentry-system` from the controller's existing gateway-Pod informer (the same informer used by [AgentReconciler step 8](#agentreconciler) for activity fan-out and by [AgentChannelReconciler step 4](#agentchannelreconciler) for channel-health fan-out, so no new RBAC). If ≥1 gateway Pod is Ready, set `GatewayReachable=True`; otherwise `False` with a message stating `"no Ready gateway Pods in agentry-system"`. This is a cluster-wide signal mirrored onto each ModelProvider's status for user-facing visibility via `kubectl describe modelprovider`: LLM traffic for every provider depends on the same gateway Deployment, so the duplication is intentional. No active HTTP probe is issued by this reconciler; informer events drive re-evaluation when gateway Pod readiness changes, so the condition reflects current state without a polling cadence.
5. Validate the fallback chain. See [Fallback chain validation](#fallback-chain-validation) below.
6. Validate budget policies: confirm that every `budget.policies[x].degradeTo` value matches a model `id` in `spec.models`. If not, set `Ready=False, reason=InvalidDegradeTarget`. This catches misconfigured degrade targets before they silently fail at runtime when a budget threshold is crossed.
7. Run the **cost sanity check on `degradeTo`**. See [Cost sanity on degradeTo](#cost-sanity-on-degradeto) below.

The ModelProviderReconciler is **not** responsible for credential distribution to agent pods. Credentials are held in `agentry-system` Secrets and read directly by the gateway.

### Liveness probe

The probe uses the provider's model-list endpoint, which requires authentication but consumes no tokens:

- Anthropic and OpenAI/OpenAI-compatible adapters send `GET /v1/models`.
- The Google Vertex adapter lists publisher models (`GET /v1/projects/{project}/locations/{location}/publishers/google/models`), authenticated with an OAuth2 access token the adapter mints from the service-account JSON key in the credential Secret (Vertex does not accept static API keys, see [Credential Handling](../gateways/llm/provider-routing.md#credential-handling)). Token-free, same probe semantics.

Result handling:

- A 2xx response sets `Healthy=True`.
- A 4xx response sets `Healthy=False` and, for 401/403 specifically, also sets `Ready=False, reason=CredentialsInvalid`. That is the signal for a wrong or under-entitled credential, matching the 401/403 credential-problem classification in [Fallback triggers](../gateways/llm/fallback.md#fallback-triggers).
- A network error or 5xx sets `Healthy=False` without changing `Ready`.

Track the result in `status.conditions[type=Healthy]` with exponential backoff on failures.

### Budget reconciliation

Read per-replica partial spend counters from the gateway's budget ConfigMap in `agentry-system`; see [Budget State Management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management) for the ConfigMap format.

Before summing, cross-reference ConfigMap keys against the current set of gateway Pod names (from the controller's gateway-Pod informer, the same informer step 4 reads for the `GatewayReachable` condition) and delete stale entries left by scaled-down or replaced replicas. Sum the remaining per-replica partials, write the canonical total to the `_canonical` key, and update `status.budgetUsage` per namespace.

On budget period rollover (midnight UTC), archive the previous period's totals to ModelProvider status, delete all per-replica keys from the ConfigMap, and write a fresh `_canonical: {}`. Rollover is processed on the next reconcile pass following midnight UTC, so worst-case lag from the period boundary to the archive write is bounded by the default reconcile interval (5 minutes), which is acceptable for the soft-guardrail spend roll-up.

### Fallback chain validation

Walk the full fallback chain (following each provider's `spec.fallback` recursively up to `maxFallbackDepth`) and confirm no circular references, all referenced providers exist, and all providers in the chain have the same `spec.type` as the primary. Emit `Ready=False` if invalid. The depth cap is a gateway-level setting; the reconciler validates the chain structure regardless of depth cap, since the cap may change without re-reconciling providers.

Beyond the structural check, also scan each fallback candidate for **static eligibility violations**: a fallback whose `spec.allowedNamespaces` does not contain the primary's callers, or whose `spec.models` does not contain the primary's models. Emit a `Warning` event with `reason=FallbackIneligible` naming the offender. This is the reconcile-time counterpart to the gateway's runtime `FallbackIneligible` event (see [Fallback Logic](../gateways/llm/fallback.md)); the runtime event is defense in depth for races where config changes between reconcile and the next request.

### Cost sanity on degradeTo

For each policy whose `action: degrade`, compute `avgCost(model) = (costPer1MInputTokens + costPer1MOutputTokens) / 2` for the target model and compare against the same metric for every other model in `spec.models`. If the target is not strictly the cheapest, emit a `Warning` event with `reason=DegradeTargetNotCheapest` on the ModelProvider, naming the cheaper alternative (e.g., `"degradeTo=claude-opus-4-6 is not the cheapest model; claude-haiku-4-5 has a lower average cost"`).

This is **advisory only**: it does **not** set `Ready=False`, because platform teams may have non-cost reasons (latency, capability, quality) to prefer a specific degrade target. The warning exists because a "degrade" policy labelled as cost-saving but pointing at a more expensive model is almost always a misconfiguration, and catching it at the reconciler is cheaper than discovering it from a monthly bill.

It runs after step 6's existence check so that a missing `degradeTo` surfaces as the more serious `InvalidDegradeTarget` error without a contradictory cost warning.

## AgentReconciler

Watches: `Agent`, plus owned `Pod`, `PVC`, `Service`, `ConfigMap`, `NetworkPolicy`, `ServiceAccount`, `cert-manager.io/v1/Certificate`. The cert-manager-managed `Secret` (`spec.secretName` output) is **not** owned by the reconciler: cert-manager owns it and populates it from the `Certificate`.

Two map-func watches make the reconciler react to platform-level changes without waiting for the periodic requeue:

- `AgentClass` via `handler.EnqueueRequestsFromMapFunc`: when an AgentClass changes (e.g., `allowedProviders` updated, `maxLimits` lowered), re-queue all Agents referencing that class (via indexed lookup on `agentClassRef.name`).
- `ModelProvider` via `handler.EnqueueRequestsFromMapFunc`: when a ModelProvider's `allowedNamespaces`, `Healthy` condition, or other spec fields change, re-queue all Agents whose `spec.providers[*].providerRef` references that ModelProvider (via an indexed lookup on `providerRef.name`).

This is what makes the AgentClass-drift path and the [Recoverable error bucket](operations.md#error-handling) fire event-driven rather than waiting for the 5-minute periodic requeue.

### AgentClass-drift path

On every requeue triggered by an AgentClass or ModelProvider change, the reconciler re-derives the desired Pod spec under the new class invariants and either recreates the Pod or degrades the Agent. The propagation mechanics are specified in [AgentClass change handling](change-propagation.md#agentclass-change-handling); what the reconciler does locally is:

**(a) Recreate the Pod** via the standard Provisioning transition if the new spec differs in replacement-triggering fields: image, resources, command, args, env, provider wiring. Image (and, on newer clusters, resources) is technically mutable in place, but Agentry deliberately replaces the Pod for a clean restart (recreate-and-clamp).

Drift is detected by comparing a hash of the derived Pod spec, stamped as a Pod annotation at creation, against the hash of the re-derived spec: the Deployment `pod-template-hash` idiom. It is **never** detected by a DeepEqual against the live Pod object, whose apiserver-defaulted and admission-injected fields (`serviceAccountName`, `nodeName`, `tolerations`, `imagePullPolicy`, …) would report perpetual drift and recreate-loop the Pod.

**(b) Transition the Agent to `phase=Degraded`** if the new invariants exclude the Agent's spec:

- `spec.image` no longer matches `image.allowedImages`, or any `providerRef` is no longer in `allowedProviders` or references a ModelProvider whose own `allowedNamespaces` no longer includes the Agent's namespace: `reason=ClassConstraintViolation`.
- `spec.persistence.enabled: true` against a class with `persistence.enabled: false`: `reason=PersistenceNotAllowed`.
- `spec.lifecycle.hibernationEnabled: true` against a class with `lifecycle.hibernationAllowed: false`: `reason=HibernationNotAllowed`.

The image, provider, persistence, and hibernation checks reuse cross-resource validation rules 2, 4, 5, 24, and 26 in [Cross-Resource Validation](../resources/validation-and-defaulting.md#cross-resource-validation). Rule 4 (a ModelProvider dropping the workload's namespace from `allowedNamespaces`) is the canonical drift case, shown in [scenario S5](../appendix/scenarios.md). These checks fail at reconcile and surface as `Degraded` rather than being silently re-applied to a recreated Pod. Rule 29 (hibernation-requires-persistence) also degrades an Agent, but it is spec-internal, not a class comparison, so a class change can never introduce it; it is enforced in the [pre-Pod-creation checks](#pre-pod-creation-checks) on every reconcile.

### Reconciliation steps

This is the most complex reconciler. It implements the persistent [Agent State Machine](agent-lifecycle.md). Each reconciliation pass:

1. If the Agent's namespace is `agentry-system`, set `Ready=False, reason=SystemNamespaceForbidden` and stop: no child resources are created. This guards SAN integrity: an Agent in `agentry-system` could otherwise be issued a certificate whose SAN collides with the gateway or controller Service identity (see [rule 28](../resources/validation-and-defaulting.md#cross-resource-validation)); the same guard runs first in the AgentTaskReconciler and AgentChannelReconciler. Otherwise, resolve `agentClassRef` and fetch the AgentClass. If missing, set `Ready=False` with a clear reason.
2. If `spec.providers` is present, resolve all `providerRef`s. If any referenced ModelProvider is missing, no longer in `AgentClass.allowedProviders`, or its `allowedNamespaces` does not include the Agent's namespace, transition the Agent to `phase=Degraded, reason=ClassConstraintViolation` with `preDegradedPhase` set (matching the AgentClass-drift path above). Recovers when the developer aligns the Agent or ModelProvider spec.
3. Determine the desired phase based on the state machine.
4. **Create/ensure the cert-manager `Certificate` resource for the Agent before Pod creation.** See [Agent Certificate](#agent-certificate) below.
5. **Resolve `imagePullSecrets`** and run the pre-Pod-creation cross-checks. See [Pre-Pod-creation checks](#pre-pod-creation-checks) below.
6. Converge the remaining child resources to match the desired phase: `Pod`, `PVC`, `Service`, `ServiceAccount`, `NetworkPolicy`. All are owner-referenced to the Agent for cascade GC. See [Child-resource convergence](#child-resource-convergence) below.
7. When creating a Pod, inject controller-managed environment variables and probes. See [Injected environment and probes](#injected-environment-and-probes) below.
8. For agents in `Running` or `Idle` phase: query the activity API in a **per-namespace batch**. See [Activity fan-out](#activity-fan-out) below.
9. On every reconcile pass, check for the `agentry.io/wake=true` annotation. See [Wake annotation handling](#wake-annotation-handling) below.
10. Update status and emit events for phase transitions. On any change to `status.phase`, set `status.phaseTransitionTime = now()` in the same status patch so the field always reflects the moment the new phase was committed. This timestamp is consumed by the gateway-restart-detection logic in step 8 and by the activity API fan-out in [Activity Tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api). Do not update `phaseTransitionTime` on condition-only or label/annotation-only updates.

Owner references are set on all child resources pointing back to the Agent, so cascade deletion works naturally.

![Activity diagram of one AgentReconciler pass. Step 1 exits to Ready=False, reason=SystemNamespaceForbidden for an Agent in agentry-system, and to a second Ready=False if the agentClassRef does not resolve. Step 2 exits to phase=Degraded, reason=ClassConstraintViolation when a providerRef is unresolvable, excluded from allowedProviders, or blocked by the provider's allowedNamespaces. Step 4 loops back on itself, requeueing with backoff without creating the Pod until the cert-manager Certificate reports Ready=True. Step 5 exits to Ready=False, reason=ImagePullSecretMissing when a pull Secret is absent from the Agent's namespace, and to phase=Degraded with one of PersistenceNotAllowed, HibernationNotAllowed, or HibernationRequiresPersistence when the persistence and hibernation cross-checks fail. Only after all five gates pass do steps 6 and 7 converge the child resources and create the Pod, followed by the conditional activity fan-out in step 8, the wake-annotation check in step 9, and the status write in step 10.](../diagrams/agent-reconcile-pass.svg)

Reading the diagram: the numbered list above is flat, but the pass is not. Five exits can end it before a Pod ever exists, spread over three of the ten steps (1 twice, 2, and 5 twice), and step 4 is a loop rather than a step: Pod creation is gated on certificate readiness, so the reconciler requeues rather than proceeding. The two exit shapes are distinct. `Ready=False` (grey) leaves `status.phase` alone and reports a condition; `phase=Degraded` (blue) is a phase transition that records `preDegradedPhase` so the prior phase can be restored on recovery.

### Agent Certificate

The Certificate is named `{agentName}-tls` in the Agent's namespace, owned by the Agent via `ownerReferences` so it is garbage-collected on Agent deletion. Key fields:

- `spec.issuerRef`: `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`. A `ClusterIssuer` is used because cert-manager does not resolve a namespaced `Issuer` across namespace boundaries; the chart installs `agentry-ca-issuer` as a `ClusterIssuer` sourcing from the `agentry-ca` Secret in cert-manager's **cluster resource namespace** (chart value `certManager.clusterResourceNamespace`, default `cert-manager`). A CA `ClusterIssuer` resolves `spec.ca.secretName` only in the namespace cert-manager's `--cluster-resource-namespace` flag points at, never in `agentry-system`.
- `spec.secretName`: `{agentName}-tls` (the output Secret cert-manager creates in the Agent's namespace).
- `spec.dnsNames`: `{agentName}.{namespace}.svc.cluster.local`, `{agentName}.{namespace}.svc`, `{agentName}.{namespace}`.
- `spec.duration`: `2160h` (90d), `spec.renewBefore`: `720h` (30d), chart defaults.
- `spec.usages`: `server auth`, `client auth` (the same cert acts as the agent's serving cert and as its mTLS client cert when calling the gateway).

**Pod creation is gated on `Certificate.status.conditions[type=Ready].status == True`.** If the cert is not yet ready (first-time issuance typically takes a few seconds), the reconciler requeues with backoff and **does not create the Pod**: otherwise the Pod would hang on its projected Secret mount until cert-manager caught up.

Subsequent rotation is transparent: cert-manager rotates per `renewBefore`, kubelet propagates the new Secret contents into the Pod's projected volume, and the agent reloads via the file-watch pattern (see [Starter Templates](../runtime/starter-templates.md)). CA rotation requires no reconciler participation, and a CA re-key is a manual dual-trust runbook; see [In-cluster TLS](../security/tls.md#in-cluster-tls) for the full trust chain, and [TLS on the LLM Gateway Listener](../gateways/llm/listener-tls.md).

### Pre-Pod-creation checks

Before creating the Pod, for each entry in the effective `AgentClass.spec.image.imagePullSecrets` (plus any Agent-level additions, once supported), verify that a Secret with that name exists in the **Agent's namespace**. AgentClass is cluster-scoped but the Secret is resolved per-workload in the Agent's namespace: the reconciler does **not** copy Secrets from `agentry-system` into user namespaces. If any referenced Secret is missing, set `Ready=False, reason=ImagePullSecretMissing` with a message naming the namespace and missing Secret (e.g., `"imagePullSecret 'registry-credentials' missing in namespace 'team-support'"`) and skip Pod creation.

This check runs *before* the Pod is created so that a missing pull secret surfaces as a clear condition rather than as an `ImagePullBackOff` loop on a live Pod, and so the Pod is not left in a crash state consuming quota. See [Cross-Resource Validation rule 23](../resources/validation-and-defaulting.md#cross-resource-validation).

In the same pre-Pod-creation phase, also enforce the **persistence-enabled cross-check**, the **hibernation-allowed cross-check**, and the **hibernation-requires-persistence check**. All follow the standard [Degrade-when-irreconcilable](change-propagation.md#agentclass-change-handling) handling, transitioning the Agent to `phase=Degraded` (with `preDegradedPhase` set) and a specific `reason`:

- If `Agent.spec.persistence.enabled` is `true` but the resolved AgentClass has `spec.persistence.enabled: false`, transition to `phase=Degraded, reason=PersistenceNotAllowed` with a message naming the AgentClass. The Pod and PVC are not created (or not recreated, if class drift introduced the conflict). See [Cross-Resource Validation rule 24](../resources/validation-and-defaulting.md#cross-resource-validation).
- If `Agent.spec.lifecycle.hibernationEnabled` is `true` but the resolved AgentClass has `spec.lifecycle.hibernationAllowed: false`, transition to `phase=Degraded, reason=HibernationNotAllowed` with a message naming the AgentClass. The Pod is not created (or not recreated, if class drift introduced the conflict). See [Cross-Resource Validation rule 26](../resources/validation-and-defaulting.md#cross-resource-validation).
- If `Agent.spec.lifecycle.hibernationEnabled` is `true` but `Agent.spec.persistence.enabled` is `false`, transition to `phase=Degraded, reason=HibernationRequiresPersistence` with a message naming both fields. Hibernation deletes the Pod and relies on the PVC to carry state across the gap, including the [The Runtime Contract](../runtime/contract.md) item 7 message-dedup buffer, so it is meaningless without persistence. See [Cross-Resource Validation rule 29](../resources/validation-and-defaulting.md#cross-resource-validation).

These checks recover normally when the developer aligns the Agent or AgentClass spec: the reconciler restores the prior phase from `preDegradedPhase` on the next reconcile that observes the resolved mismatch. The same status write that restores `status.phase` also sets `status.preDegradedPhase = null` atomically, so a subsequent `any → Degraded` transition cannot reuse a stale value.

### Child-resource convergence

There is no per-Agent configuration ConfigMap. Non-sensitive config is delivered via the env vars injected at Pod creation in step 7, and config changes are Pod-replacing spec drift by design.

Pod convergence explicitly covers involuntary disruption. Both of the following are recreate triggers:

- A *missing* Pod in a Pod-bearing phase.
- A *terminal* Pod (`status.phase` `Succeeded`/`Failed`). For example, node-pressure eviction leaves a `Failed`/`Evicted` Pod that `restartPolicy: Always` never resurrects.

Either one drives the [involuntary-disruption transition](agent-lifecycle.md) back through `Provisioning`. Detection is event-driven via the owned-Pod watch, not polling.

**Service**: ClusterIP, named `{agentName}` in the Agent's namespace, with `port: spec.service.port` (default 8080) and `targetPort` set to the same numeric port the controller injects as `$AGENTRY_HEALTH_PORT` on the Pod (default 8080). `targetPort` is a literal number on the Service: there is no env-var substitution at Service apply time; the controller computes the port once and uses the same value in both the Service spec and the Pod env var. The two are decoupled from `spec.service.port` deliberately so a developer can override the Service-facing port without changing the in-Pod listen port; the agent only ever binds the port given by `$AGENTRY_HEALTH_PORT`. See the [Agent CRD design notes](../resources/agent.md).

**ServiceAccount**: named `agent-{agentName}` in the Agent's namespace, with no RoleBindings by default (the agent has no Kubernetes API access unless the platform team or developer explicitly grants it, see [Agent Pod ServiceAccount](../security/rbac.md#agent-pod-serviceaccount)). Created before the Pod so the Pod can reference it via `spec.serviceAccountName`.

**NetworkPolicy**: one policy per Agent, synthesized from AgentClass settings. The policy combines:

- (a) the default egress allow to the gateway Service on port 8443,
- (b) the DNS egress rule from the Helm value `controller.networkPolicy.dnsSelector`,
- (c) the default ingress allow from the gateway for `$AGENTRY_HEALTH_PORT`,
- (d) `AgentClass.spec.network.egress.allowedCIDRs` translated into `NetworkPolicy.egress.to.ipBlock.cidr` rules,
- (e) `AgentClass.spec.network.egress.allowedHosts` translated into `CiliumNetworkPolicy.toFQDNs` (or the Calico-Enterprise equivalent) **only** when the AgentClassReconciler's startup CNI probe reported FQDN-policy support; otherwise `allowedHosts` is silently ignored and the matching `Warning` event is emitted on the AgentClass, not the Agent,
- (f) inter-agent ingress only when `AgentClass.spec.network.allowSameNamespaceIngress: true`.

### Injected environment and probes

- `$AGENTRY_HEALTH_PORT` (always): the port the agent serves its HTTPS health/message endpoint on (default 8080).
- `$AGENTRY_GATEWAY_ENDPOINT` (always): HTTPS URL pointing to the gateway Service in `agentry-system` on port 8443. This is the base URL for all agent→gateway calls: LLM requests, heartbeats ([`POST /v1/agent/heartbeat`](../gateways/api/agent-endpoints.md#post-v1agentheartbeat)), and task completion ([`POST /v1/task/complete`](../gateways/api/task-complete.md)). Always injected regardless of whether `spec.providers` is set, so provider-less agents (e.g., AgentTasks that report completion but make no LLM calls) can still reach the gateway.
- `$AGENTRY_CA_CERT` (always): path to the Agentry CA trust bundle projected into the Pod.
- `$AGENTRY_TLS_CERT` and `$AGENTRY_TLS_KEY` (always): paths to the agent's TLS serving/client certificate and key (cert-manager-managed; see step 4).

The controller also injects the liveness and readiness probes with `httpGet.scheme: HTTPS`, targeting `GET /livez` (liveness) and `GET /readyz` (readiness) on `$AGENTRY_HEALTH_PORT`: the paths pinned by [The Runtime Contract](../runtime/contract.md) item 1. The agent serves TLS on that port, and Kubernetes httpGet probes do not verify TLS certificates, so no additional CA configuration is required on the probe itself.

Agent Pods are created with `restartPolicy: Always`: the kubelet restarts a crashed container in place, and the persistent-crash-loop detection behind the `any → Failed` transition reads `containerStatuses[].restartCount` / `CrashLoopBackOff`, not Pod phase (see [Agent state machine](agent-lifecycle.md)).

### Activity fan-out

The activity API is queried in a per-namespace batch, not per individual agent reconcile. Because each agent's reconcile is an independent controller-runtime invocation, a naive implementation would issue O(agents × replicas) gateway HTTP calls per reconcile cycle: at 1000 agents and 3 replicas, that is 3000+ calls per cycle.

Instead, the reconciler caches the activity fan-out result for a namespace for a fixed 15-second window. On the first reconcile of any agent in a namespace during this window, it fans out to all gateway Pod IPs in parallel (`GET /v1/activity?namespace={ns}`, one call per replica), takes the most recent timestamp per agent across all responses, and stores the result in a reconciler-local namespace-keyed cache. Subsequent agent reconciles in the same namespace within the window read from the cache rather than issuing new HTTP calls. This reduces gateway query load to O(namespaces × replicas) per window.

15s is well below any practical `idleTimeout` (the value defaults from `AgentClass.spec.lifecycle.defaultIdleTimeout`; the chart's default `standard` class ships 30m), so cache staleness cannot meaningfully delay idle-detection or hibernation transitions. The value is a controller constant, not a Helm tunable, so operators do not need to size it.

The fan-out skips unreachable replicas. The result is used to evaluate idle and hibernation transitions; see [Activity Detection](hibernation-and-wake.md#activity-detection) for the full logic. The HTTP client used for these per-Pod-IP dials sets `tls.Config.ServerName` to the gateway Service DNS so SAN verification succeeds against the Pod-IP target; see [Multi-replica fan-out](../gateways/user/activation-and-activity.md#activity-tracking-api).

### Wake annotation handling

Removal of `agentry.io/wake=true` is phase-dependent so a failed reconcile cannot silently drop the wake:

- **Non-`Hibernated` agent with the annotation**: remove the annotation immediately. Emit a `Warning` event (`reason=WakeIgnored`, message `"wake annotation observed on non-Hibernated agent; ignored"`) **unless the Agent is in `Resuming`**, in which case the annotation is removed silently. The `Resuming` carve-out exists because a wake observed during `Resuming` is a benign idempotent re-attempt by the gateway (or a manual re-annotation racing an in-progress wake), not the misfire case the Warning is meant to surface for `Running`/`Idle`/`Degraded`. No phase change in either branch. Removing the annotation in both branches prevents stale annotations from triggering spurious wakes on a future hibernation cycle.
- **`Hibernated` agent with the annotation**: transition to `Resuming` and recreate the Pod (see [Wake trigger](hibernation-and-wake.md#wake-trigger)). Remove the annotation **only after the transition to `Resuming` has been committed** (successful status update). If the status update or the subsequent Pod recreation fails and the reconcile is requeued, leave the annotation in place so the next reconcile observes it again. This ensures a wake signal from the activator (or from a manual `kubectl annotate`) cannot be lost to a transient apiserver error between the moment the reconciler sees the annotation and the moment it commits the phase change.

## AgentTaskReconciler

Watches: `AgentTask`, plus owned `Pod`, `PVC`, `ConfigMap` (for artifacts), `NetworkPolicy`, `ServiceAccount`, `Role`, `RoleBinding`, and `cert-manager.io/v1/Certificate`. The `Role`/`RoleBinding` watches detect drift on the per-task RBAC objects created in step 3 (only present for `agentReported`-mode tasks); the watch is a no-op for `exitCode`-mode tasks, which create no per-task RBAC. The cert-manager-managed `Secret` output is not owned by the reconciler: cert-manager owns it. Also watches `AgentClass` via `handler.EnqueueRequestsFromMapFunc`: when an AgentClass changes, re-queue all AgentTasks referencing that class (via indexed lookup on `agentClassRef.name`).

On an AgentClass change, the reconciler does **not** disturb in-flight tasks: AgentTasks in `Running` or `Provisioning` continue under the class snapshot in effect at their Pod-creation time and run to completion. The new invariants apply only on the next Pod-provisioning event for that task: a backoff retry from `Failed` (where standard validation against the new class runs; if the spec no longer admits, the retry is rejected and the task is marked `Failed, reason=ClassConstraintViolation`), or a subsequent AgentTask CR. AgentTasks already in `Succeeded`, `Failed`, or `TimedOut` are unaffected; terminal-state tasks ignore class drift and proceed to TTL-based cleanup. See [AgentClass change handling](change-propagation.md#agentclass-change-handling).

Reconciliation steps:

1. Resolve AgentClass and ModelProviders (same validation as Agent). This also includes the **`imagePullSecrets` resolution check** from [AgentReconciler step 5](#agentreconciler): each entry in the effective `AgentClass.spec.image.imagePullSecrets` must exist as a Secret in the **AgentTask's namespace**. Missing Secrets set `Ready=False, reason=ImagePullSecretMissing` and skip Pod creation, so a missing pull secret surfaces as a clear condition rather than as an `ImagePullBackOff` loop on a live Pod. That is especially important for AgentTasks, since a failed task Pod counts against the task's `backoffLimit` (default 0) and can mark the task as `Failed` for a reason unrelated to the workload itself. The same step also enforces the **persistence-enabled cross-check**: if `AgentTask.spec.persistence.enabled` is `true` but the resolved AgentClass has `spec.persistence.enabled: false`, transition the AgentTask to `phase=Failed, reason=PersistenceNotAllowed` with a message naming the AgentClass and skip Pod and PVC creation. AgentTask has no `Degraded` phase, so the terminal `Failed` mirrors the existing class-drift handling for AgentTask backoff retries (see [§ AgentClass change handling](change-propagation.md#agentclass-change-handling)). See [Cross-Resource Validation rule 24](../resources/validation-and-defaulting.md#cross-resource-validation).
2. **Create/ensure the cert-manager `Certificate` resource for the AgentTask before Pod creation.** See [AgentTask Certificate](#agenttask-certificate) below.
3. **For tasks with `spec.completion.condition: agentReported` only**, pre-create the completion mailbox and its scoped RBAC. See [Completion mailbox and per-task Role](#completion-mailbox-and-per-task-role) below.
4. Converge the remaining child resources in the task's namespace: `Pod`, `PVC`, `ServiceAccount`, `NetworkPolicy`. All are owner-referenced to the AgentTask for cascade GC. See [Task child-resource convergence](#task-child-resource-convergence) below.
5. Inject controller-managed environment variables on Pod creation: the same set as [AgentReconciler step 7](#agentreconciler): `$AGENTRY_HEALTH_PORT`, `$AGENTRY_GATEWAY_ENDPOINT`, `$AGENTRY_CA_CERT`, `$AGENTRY_TLS_CERT`, `$AGENTRY_TLS_KEY`. `$AGENTRY_GATEWAY_ENDPOINT` is always injected so the task image can call [`POST /v1/task/complete`](../gateways/api/task-complete.md) even if the task makes no LLM calls. (Heartbeats are **Agent-only**: `/v1/agent/heartbeat` rejects AgentTask callers with `403` at the handler; task liveness is governed by the task timeout, not idle detection. See [The Runtime Contract](../runtime/contract.md) item 5.) Readiness and liveness probes are **not** injected by the controller for AgentTasks: tasks have no Service and typically do not serve a message endpoint, and task Pods carry no probes at all (a container image cannot declare Pod-spec probes, and AgentTask exposes no probe fields). Task liveness is governed by `spec.completion.timeout` and the provisioning deadline, not by kubelet probes.
6. Drive the [AgentTask State Machine](task-lifecycle.md). See [Pod-loss precedence and retry stamping](#pod-loss-precedence-and-retry-stamping) below.
7. On `Completing`: artifact values are read from the pre-existing `{taskName}-completion` ConfigMap that was created in step 3. When the agent calls `POST /v1/task/complete`, the gateway validates the payload's artifact names against `spec.artifacts` (returning `400 invalid_request` synchronously to the agent on mismatch, see [POST /v1/task/complete](../gateways/api/task-complete.md)) and only then updates this ConfigMap (via `update`/`patch` against the resource-name-scoped Role) with the completion payload. The reconciler watches for changes to this ConfigMap, defensively re-validates the artifact names against `spec.artifacts` (a no-op under normal operation, since the gateway has already enforced the same check), reads artifact values, and populates `status.artifactValues`. No exec into the container is required, and the completion data survives Pod crashes or eviction.
8. Honor `ttlSecondsAfterFinished` by scheduling deletion.

### AgentTask Certificate

Named `{taskName}-tls` in the task's namespace, owner-referenced to the AgentTask so it is garbage-collected on deletion. Key fields:

- `spec.issuerRef`: `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`, the same `ClusterIssuer` used by Agents.
- `spec.secretName`: `{taskName}-tls`.
- `spec.dnsNames`: `{taskName}.{namespace}.task.agentry.io` (single SAN). A non-Service shape is used deliberately: AgentTasks have no Service, so the Service-DNS shape used by Agents would be misleading. The shape is recognized by the gateway's SAN parser as an AgentTask identity, see [Namespace Identification](../gateways/llm/workload-identity.md).
- `spec.usages`: `client auth` only (no `server auth`, since tasks have no inbound TLS listener).
- `spec.duration`: `2160h` (90d), `spec.renewBefore`: `720h` (30d), matching Agent defaults.

**Pod creation is gated on `Certificate.status.conditions[type=Ready].status == True`.** If the cert is not yet ready, the reconciler requeues with backoff and does not create the Pod: otherwise the Pod would hang on its projected Secret mount until cert-manager caught up, which for AgentTasks additionally risks counting the delay against `backoffLimit`. cert-manager owns and rotates the Secret afterwards; the reconciler does not track rotation state. The task image uses the same cert-reload pattern as Agents (see [Starter Templates](../runtime/starter-templates.md)).

### Completion mailbox and per-task Role

Pre-create the empty `{taskName}-completion` ConfigMap in the task's namespace with `data: {}`, owned by the AgentTask via `ownerRef` for cascade deletion. Then ensure a per-task `Role` and `RoleBinding` exist granting the gateway ServiceAccount (`agentry-system/agentry-gateway`) `update, patch` on the ConfigMap with that exact name (`resourceNames: ["{taskName}-completion"]`). The Role and RoleBinding are also owned by the AgentTask.

The verb set is deliberately `update, patch` and not `create`: Kubernetes RBAC's `resourceNames` does not constrain `create` requests, so granting `create` on a named ConfigMap would silently broaden the gateway's access to all ConfigMaps in the namespace. By pre-creating the resource here and granting the gateway only name-scoped mutate verbs, the scoping guarantee is enforceable. This mirrors the per-channel Role pattern in [AgentChannelReconciler](#agentchannelreconciler). See [Gateway ServiceAccount](../security/rbac.md#gateway-serviceaccount-permissions) for the security rationale.

Tasks with `completion.condition: exitCode` skip this step entirely: they have no completion endpoint and never write to this ConfigMap, so allocating it would be wasted state and an unused (though scoped) RBAC grant.

### Task child-resource convergence

Task Pods are created with `restartPolicy: Never`. Agentry owns retries via `backoffLimit` (a kubelet in-place restart would bypass `status.retries` accounting and blur the one-run-per-`currentPodUID` gate), and `exitCode`-mode completion depends on it: Pod phase only reaches `Succeeded`/`Failed` under `Never`; with `Always`/`OnFailure` the kubelet restarts the container in place and the phase stays `Running` forever.

For `agentReported`-mode tasks, the `{taskName}-completion` ConfigMap and its scoped Role/RoleBinding were already created in step 3 alongside the Certificate; they are not (re-)created lazily by the gateway. `exitCode`-mode tasks have no equivalent.

**For `agentReported`-mode tasks**, after the Pod is observed via the informer (initial provisioning or `backoffLimit` retry), patch `status.currentPodUID = Pod.UID` and `status.podName = Pod.Name` in the same status update. The UID stamping is what gives the gateway its identity gate at `/v1/task/complete` (see [POST /v1/task/complete](../gateways/api/task-complete.md) 403 cases (c) and (d)); `podName` stays for human-readable debugging. `exitCode`-mode tasks skip this stamping: they have no completion endpoint.

**ServiceAccount**: named `task-{taskName}` with no RoleBindings by default (tasks have no Kubernetes API access unless explicitly granted). Created before the Pod so the Pod can reference it.

**NetworkPolicy**: synthesized the same way as for Agents (see [AgentReconciler step 6](#agentreconciler)) with one structural difference: tasks have no Service and are not delivery targets, so no gateway→task ingress allow rule is emitted. The policy still sets `policyTypes: [Ingress, Egress]` with `ingress: []`, declared explicitly for clarity, not necessity: when `policyTypes` is unset, Kubernetes defaulting always includes `Ingress` (adding `Egress` only when egress rules are present), so this policy would default to `[Ingress, Egress]` anyway. Spelling it out documents the deny-all-ingress intent instead of relying on defaulting behavior. The egress rules (gateway on 8443, DNS, AgentClass `allowedCIDRs`, optional `allowedHosts` on FQDN-capable CNIs) are identical to the Agent case.

### Pod-loss precedence and retry stamping

On observing mid-run Pod loss (evicted, deleted out-of-band, node lost), check the completion mailbox before classifying: a valid completion already in the `{taskName}-completion` ConfigMap wins and the task proceeds through `Completing` normally. Only an empty mailbox makes the loss a retryable `Failed` (see the [precedence note](task-lifecycle.md)).

On `backoffLimit` retry from `Failed`, the reconciler executes the [retry sequence](task-lifecycle.md) in this order:

1. Clear `status.currentPodUID = ""`. This closes the in-flight stale-write window: any `/v1/task/complete` from the terminated old Pod arriving after this point fails the identity gate with `403 StalePodCompletion`.
2. Reset the `{taskName}-completion` ConfigMap to `data: {}`.
3. Trigger Pod recreation.
4. On observing the new Pod, stamp `status.currentPodUID = newPod.UID`, which re-opens the gate.

The clear-before-reset ordering matters: if the ConfigMap were reset first, an in-flight stale write could land on the fresh mailbox before the gate closed.

## AgentChannelReconciler

Watches: `AgentChannel`, plus owned `Role` and `RoleBinding` (created in step 3, used by both the gateway and the operator to read channel auth Secrets; drift on either rebreaks the auth-Secret read path until the next reconcile), plus the referenced Agent and its Service.

Reconciliation steps:

1. Resolve `agentRef`: if the referenced Agent does not exist, set `Ready=False, reason=AgentNotFound`. Note: `agentRef` must reference an `Agent`, not an `AgentTask`. Tasks are ephemeral and lack a stable Service endpoint.
2. Verify the Agent has `spec.service.enabled: true`. If not, set `Ready=False, reason=AgentServiceDisabled`. In the same step, validate that `spec.webhook.path` begins with `/channels/{namespace}/` where `{namespace}` is the AgentChannel's own namespace: violations set `Ready=False, reason=InvalidPath`. This single-resource rule lives at reconcile time because CRD CEL cannot read `metadata.namespace` (see [rule 15](../resources/validation-and-defaulting.md#cross-resource-validation)); the gateway independently refuses to register non-conforming paths, so a violating channel receives no traffic even before this status lands.
3. Ensure the per-channel credential `Role` exists **before** validating Secret contents. See [Per-channel credential Role](#per-channel-credential-role) below.
4. Poll channel health status from the gateway. See [Channel health poll](#channel-health-poll) below.
5. **Maintain `status.phase`** by reducing the referenced Agent's phase to a Channel phase. See [Channel phase reduction](#channel-phase-reduction) below.
6. **Prune expired async response ConfigMaps.** See [Async ConfigMap pruning](#async-configmap-pruning) below.

The AgentChannelReconciler does not own Pod resources. The gateway watches `AgentChannel` resources directly, reads the referenced credentials from user namespaces, and manages the live platform connections; see [User Gateway Request Flow](../gateways/user/overview.md#request-flow). The reconciler's role is validation and status reporting.

### Per-channel credential Role

The Role is named `agentry-channel-{channelName}-creds` and lives in the AgentChannel's namespace. It must exist before the reconciler reads any Secret, because the reconciler does not have blanket Secret-read RBAC in user namespaces: the Role is what gives it (and the gateway) access to the specific Secret(s).

The Role grants `get, watch`, `resourceNames`-scoped to every Secret referenced by the channel's webhook auth config. `list` is deliberately omitted: RBAC `resourceNames` cannot constrain a plain `list` request, so the verb would be dead weight. Name-scoped `watch` requests must set `fieldSelector metadata.name=<secret>` to satisfy the `resourceNames` check.

The scoped Secrets are:

- The inbound `spec.webhook.auth` Secret, always present (`secretRef.name` for `bearer`, `hmac.secretRef.name` for `hmac`).
- When `spec.webhook.callbackUrl` is set, the outbound `spec.webhook.callbackAuth` Secret (same shape: `callbackAuth.secretRef.name` or `callbackAuth.hmac.secretRef.name`).

When both references point to the same Secret, `resourceNames` lists it once; when they differ, the list contains both.

The Role is bound by two RoleBindings: one to the gateway ServiceAccount (`agentry-system/agentry-gateway`) and one to the operator ServiceAccount (`agentry-system/agentry-controller`). If the inbound auth type, the outbound auth type, or any Secret reference has changed since the last reconcile (detectable by comparing the current Role's `resourceNames` against the desired set), the reconciler updates the Role to the new `resourceNames` so neither the gateway nor the operator retains read access to a Secret either no longer needs. The Role name is deterministic so successive reconciles are idempotent.

After the Role exists, the reconciler reads each referenced Secret via this scoped path and validates:

- (a) Every referenced Secret exists. If any is missing, set `Ready=False, reason=CredentialsMissing` with a message naming which Secret.
- (b) The referenced key is present in each Secret's `data`. If absent, set the same `Ready=False, reason=CredentialsMissing` with a message naming both the Secret and the missing key.

The reason code is shared across inbound and outbound to give consumers one stable "channel auth Secret unusable" signal. Per [cross-resource validation rule 25](../resources/validation-and-defaulting.md#cross-resource-validation), `callbackAuth` is required whenever `callbackUrl` is set (CRD CEL rejects the bypass case); the Secret/key check here is the parallel runtime validation, mirroring the inbound `auth` check.

Both Role and RoleBindings are owned by the AgentChannel via `ownerRef` and cascade-delete on AgentChannel deletion.

### Channel health poll

Poll `GET /v1/channels/health?namespace={ns}` (see [Channel Health Endpoint](../gateways/api/internal-endpoints.md#get-v1channelshealth)), authenticating via mTLS with the controller's `agentry-controller-tls` client cert. The fan-out shape mirrors the activity-API path: query every gateway Pod IP in parallel and skip unreachable replicas. It uses the same per-namespace reconciler-local cache with the fixed 15-second window described in [AgentReconciler step 8](#agentreconciler), so a burst of AgentChannel reconciles in one namespace produces a single fan-out per window; load is bounded to O(namespaces × replicas) per window, identical to the activity API.

Each per-replica response carries a top-level `replicaStartedAt` and `windowSeconds` (sourced from `gateway.channelHealthWindow`) plus a `channels` map whose values are `{ phase, state, reason, lastError, timestamp }` per channel path. `state ∈ { success, failure, empty }` is the replica's view of its in-window observation list (see [Channel Health Tracking](../gateways/user/platform-adapters.md#channel-health-tracking) for the per-replica state model).

Reduce per-channel and update `status.conditions[type=PlatformConnected]` using the tri-state rule:

- (a) Any replica `success` → `True` with `reason=WebhookReady` from the most recent success.
- (b) Else any replica `failure` → `False` with the most recent failure's `reason` and `lastError`.
- (c) Else if at least one replica satisfies `now - replicaStartedAt ≥ window` and every reachable replica is `empty` → `Unknown` with `reason=NoRecentTraffic`.
- (d) Else (no full-window coverage anywhere, or all replicas unreachable) preserve the existing condition.

### Channel phase reduction

`status.phase` is orthogonal to `Ready` (which reflects step 1 to 3 validation only). Per [AgentChannel](../resources/agentchannel.md), `status.phase` and `Ready` are deliberately separate axes.

- `Failed`: set in step 1 if `agentRef` does not resolve (`AgentNotFound`). Recovers only when the Agent is created.
- `Degraded`: set when the referenced Agent's `status.phase` is `Failed` or `Degraded`, with a message naming the Agent and its phase. The gateway's webhook delivery to this Channel will fail at the agent-Service connect step until the Agent recovers; `PlatformConnected` reflects that delivery health independently (per [AgentChannel](../resources/agentchannel.md) on the two axes being orthogonal).
- `Active`: the default for every other non-terminal Agent phase: `Running`, `Idle`, `Hibernated`, `Resuming`, and also the transient phases `Pending`, `Provisioning`, and `Hibernating`. The Channel is configured and the gateway's delivery layer covers the gap during transients (wake-on-demand for `Hibernated`; bounded delivery-retry for the connect-fail window during `Pending`/`Provisioning`/`Hibernating`). Transient unavailability surfaces through `PlatformConnected`, not `status.phase`.
- `Terminating`: set by the AgentChannel finalizer (see [Finalizers](finalizers.md)), not by this reconciler.

`Ready` is unaffected by Agent-phase changes: it remains driven by steps 1 to 3 (`AgentNotFound`, `AgentServiceDisabled`, `CredentialsMissing`). The gateway gates webhook **routing admission** on `Ready` and observes Agent-availability through delivery outcomes (which feed `PlatformConnected`); `status.phase` is for `kubectl describe` ergonomics.

### Async ConfigMap pruning

List `agentry-async-*` ConfigMaps in `agentry-system` matching the label selector `agentry.io/channel-namespace={ns},agentry.io/channel-name={name}` for this AgentChannel, and delete any whose `agentry.io/expires-at` annotation is in the past.

The 1-hour TTL on async responses is enforced by this step rather than by Kubernetes GC because ownership cannot be expressed with a cross-namespace ownerRef; the linkage is by labels instead. See [Async webhook response](../gateways/api/async-responses.md) for the full rationale.

Pruning runs on every reconcile pass, so worst-case lingering is bounded by the [reconcile requeue interval](overview.md#reconcile-interval-and-performance) (default 5 minutes) past the annotated expiry. The delete-time finalizer (see [Finalizers](finalizers.md) step 5) is the matching one-shot sweep when the channel itself is removed.
