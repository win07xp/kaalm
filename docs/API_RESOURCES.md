# Agentry — CRD API Reference

This document defines the custom resources Agentry provides, their spec and status schemas, and the rationale for design choices. It is the canonical API reference for implementation.

For the HTTP endpoints that agent containers call (task completion, heartbeat, message delivery, async webhook), see [API_ENDPOINTS.md](./API_ENDPOINTS.md).

API group: `agentry.io`
API version: `v1alpha1` (v1 API stability is not a goal for the initial release)

## Resource Summary

| Kind | Scope | Owner | Purpose |
|---|---|---|---|
| `AgentClass` | Cluster | Platform | Runtime policy template for a category of agents |
| `ModelProvider` | Cluster | Platform | Managed LLM provider with spend tracking and access controls |
| `Agent` | Namespace | Developer | A persistent agent workload |
| `AgentTask` | Namespace | Developer | An ephemeral, goal-driven agent workload |
| `AgentChannel` | Namespace | Developer | A connection between a running Agent and a user-facing channel |

---

## AgentClass

AgentClass is a cluster-scoped policy resource. It describes the runtime configuration, isolation, resource defaults, and allowed providers for a category of agents. It is analogous to StorageClass: developers reference an AgentClass by name in their Agent or AgentTask spec, and the platform team controls what each class permits.

### Spec

```yaml
apiVersion: agentry.io/v1alpha1
kind: AgentClass
metadata:
  name: standard
spec:
  # Runtime backend. Only "pod" is supported in v1.
  # "agentSandbox" backend (Sandbox CRD integration) is planned for v1.1.
  runtime:
    backend: pod
    # Optional. When set, must name a RuntimeClass that exists on the cluster
    # (e.g., gvisor) — Pod admission fails with "RuntimeClass not found"
    # otherwise. Omit (the default) to use the cluster's default container
    # runtime (runc in practice); stock clusters define no RuntimeClass
    # objects at all, so pinning e.g. "runc" here would break scheduling.
    # runtimeClassName: gvisor

  # Image policy
  image:
    # If set, only images matching one of these patterns may be used.
    # Empty list = any image allowed (not recommended).
    allowedImages:
      - "registry.internal.corp/agents/*"
      - "ghcr.io/myorg/agents/*:v*"
    # Default image if the Agent spec does not provide one.
    defaultImage: "registry.internal.corp/agents/base:v1"
    pullPolicy: IfNotPresent
    imagePullSecrets:
      - name: registry-credentials

  # Resource defaults and caps. Agent.spec.resources may override within caps.
  resources:
    defaults:
      requests: { cpu: "500m", memory: "1Gi" }
      limits:   { cpu: "1",    memory: "2Gi" }
    maxLimits:
      cpu: "4"
      memory: "8Gi"

  # Persistence defaults
  persistence:
    # When false, Agents and AgentTasks of this class cannot request a PVC —
    # an Agent with persistence.enabled=true is moved to phase=Degraded
    # (reason=PersistenceNotAllowed), and an AgentTask with the same setting
    # is moved to phase=Failed (reason=PersistenceNotAllowed), at reconcile
    # time. See rule 24 in Cross-Resource Validation.
    enabled: true
    defaultSizeGi: 5
    maxSizeGi: 50
    storageClassName: "standard"   # k8s StorageClass
    # What happens to the per-Agent PVC when the Agent is deleted. Distinct from
    # PersistentVolume.persistentVolumeReclaimPolicy (which governs PV fate on
    # PVC deletion) — this field controls PVC fate on Agent deletion.
    pvcRetention: Delete           # Delete | Retain

  # Provider access: which ModelProviders may agents of this class reference?
  # Empty list = no providers allowed.
  allowedProviders:
    - name: anthropic-shared
    - name: openai-fallback

  # Network policy hints (the controller translates these into NetworkPolicy resources)
  network:
    egress:
      # Allowed external destinations beyond the Agentry gateway, expressed as
      # CIDR blocks. Agent containers call providers through the gateway; this
      # governs other egress (MCP, tools). Enforced on any CNI that implements
      # standard Kubernetes NetworkPolicy.
      allowedCIDRs:
        - "10.42.0.0/16"         # internal MCP subnet
        - "140.82.112.0/20"      # api.github.com (example; pin to actual ranges)
      # Optional DNS-based allowlist. Only enforced on CNIs that support FQDN
      # egress policies (e.g., Cilium, Calico Enterprise). On standard CNIs this
      # field is ignored and AgentClassReconciler emits a Warning event. Use
      # allowedCIDRs for portable enforcement; use allowedHosts in addition only
      # when you have a CNI that supports FQDN-based policy.
      allowedHosts:
        - "mcp.internal.corp"
        - "api.github.com"
    allowHostNetwork: false
    # Allow ingress from other agent Pods in the same namespace.
    # When true, the controller adds a NetworkPolicy ingress rule that allows
    # traffic from any Pod in the same namespace bearing the agentry agent label.
    # Default false (deny all ingress except from the Agentry gateway).
    allowSameNamespaceIngress: false

  # Pod security
  security:
    podSecurityContext:
      runAsNonRoot: true
      runAsUser: 10001
      seccompProfile: { type: RuntimeDefault }
    containerSecurityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: { drop: ["ALL"] }

  # Lifecycle defaults (overridable per-Agent within limits)
  lifecycle:
    defaultIdleTimeout: "30m"
    maxIdleTimeout: "24h"
    # Hard policy lever. When false, Agents of this class cannot opt in to
    # hibernation — an Agent with lifecycle.hibernationEnabled=true is moved
    # to phase=Degraded, reason=HibernationNotAllowed at reconcile time. See
    # rule 26 in Cross-Resource Validation.
    hibernationAllowed: true
    defaultHibernationDelay: "30m"  # how long an agent stays Idle before hibernating
    maxHibernationDelay: "2h"      # cap on per-Agent hibernationDelay overrides
    defaultWakeTimeout: "2m"       # default time gateway waits for Pod Ready on wake
    maxWakeTimeout: "5m"           # cap on per-Agent wakeTimeout overrides
    terminationGracePeriodSeconds: 60

  # Labels and annotations added to every Pod created under this class
  podMetadata:
    labels:
      cost-center: "platform"
    annotations: {}
```

### Status

```yaml
status:
  observedGeneration: 3
  conditions:
    - type: Ready
      status: "True"
      reason: AllReferencesResolved
      message: "All referenced ModelProviders exist and are healthy"
      lastTransitionTime: "2026-04-05T12:00:00Z"
  agentsInUse: 14       # count of Agents currently using this class
  tasksInUse: 2         # count of AgentTasks currently using this class
```

### Design notes

- `allowedImages` is mandatory in practice for real clusters; an empty list means "any image" and validation will emit a warning.
- **`pvcRetention: Retain` mechanism**: AgentClass-derived per-Agent PVCs carry an ownerRef back to the Agent like other [child resources](./ARCHITECTURE.md#per-agent-and-per-task-child-resources), so the default cascade GC removes the PVC on Agent deletion. To honor `Retain`, the Agent finalizer strips the PVC's ownerRef before the Agent's own finalizer is removed; cascade GC then leaves the PVC untouched. When `Delete`, the finalizer leaves the ownerRef in place and cascade GC removes the PVC. See [CONTROLLER_LIFECYCLE.md § Finalizers](./CONTROLLER_LIFECYCLE.md#finalizers).
- **Image pattern glob semantics**: patterns in `allowedImages` use Go's [`path.Match`](https://pkg.go.dev/path#Match) rules. The `*` wildcard matches any sequence of non-`/` characters — it does **not** cross path separators. Examples: `registry.internal.corp/agents/*` matches `registry.internal.corp/agents/foo:latest` but NOT `registry.internal.corp/agents/team/foo:latest`. Use explicit multi-segment patterns (e.g., `registry.internal.corp/agents/team/*`) for nested paths. **Digest references DO match globs**: `*` matches any run of non-`/` characters *including* `@` and `:`, so `registry.internal.corp/agents/*` matches `registry.internal.corp/agents/foo@sha256:…` as well as tagged refs. Where digest exclusion matters, anchor the tag (e.g., `registry.internal.corp/agents/*:v*` — a hex digest contains no `v`, so digest refs cannot match) or list permitted digests explicitly.
- **Network egress — `allowedCIDRs` vs. `allowedHosts`**: `allowedCIDRs` is the portable primitive and maps directly to `NetworkPolicy.egress.to.ipBlock.cidr`, which every CNI implementing Kubernetes NetworkPolicy supports. `allowedHosts` (DNS names) cannot be expressed in standard `NetworkPolicy` — it requires a CNI with FQDN egress policies (Cilium via `CiliumNetworkPolicy`, Calico Enterprise). The AgentClassReconciler detects the cluster CNI on startup; if `allowedHosts` is set but no supported FQDN-policy CRD is present, a `Warning` event is emitted and `allowedHosts` is ignored. Prefer `allowedCIDRs` for egress governance; layer `allowedHosts` on top only when the CNI supports it.
- `allowedProviders` is the primary access control mechanism for LLM providers at the class level. ModelProvider itself has `allowedNamespaces` for namespace-level control. Both must pass for an Agent to use a provider.
- Defaults vs. maxLimits: defaults are applied at reconcile time if the Agent does not specify; maxLimits are enforced regardless and reject manifests that exceed them.
- **`imagePullSecrets` namespace resolution**: AgentClass is cluster-scoped but `imagePullSecrets[*].name` references a Secret that lives in a namespace. The reconciler resolves each entry in the **Agent's (or AgentTask's) namespace** at Pod-creation time, not in `agentry-system`. Secrets are never copied across namespaces. If any referenced Secret is missing from the target namespace, the Agent enters `Ready=False, reason=ImagePullSecretMissing` with a message naming the namespace and secret, and the Pod is not created. See rule 23 in [Cross-Resource Validation](#cross-resource-validation) and the reconcile step in [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) / [AgentTaskReconciler](./CONTROLLER_RECONCILERS.md#agenttaskreconciler).
- `runtime.backend` only accepts `pod` in v1. The `agentSandbox` value (which creates Agent Sandbox `Sandbox` CRs instead of raw Pods) is deferred to v1.1. The CRD schema enforces this via `x-kubernetes-validations: [{rule: "self == 'pod'", message: "agentSandbox backend is not supported in v1; use pod"}]` on the `runtime.backend` field — invalid values are rejected at apply time rather than surfaced as a reconcile error. See [Integration Points](./ARCHITECTURE.md#integration-points) for the planned integration design.

---

## ModelProvider

ModelProvider is a cluster-scoped resource that defines a managed LLM provider. It holds a reference to a Secret with credentials, configures rate limits and budgets, and controls which namespaces may use it.

### Spec

```yaml
apiVersion: agentry.io/v1alpha1
kind: ModelProvider
metadata:
  name: anthropic-shared
spec:
  # Provider type. Built-in: "anthropic" | "openai" | "google-vertex" | "openai-compatible"
  type: anthropic

  # Endpoint override (for self-hosted or custom gateways). Optional for known types.
  # Must use https:// — the gateway forwards provider credentials to this URL;
  # a non-TLS scheme would leak them in cleartext. CRD schema enforces this via
  # x-kubernetes-validations:
  #   - rule: "self.startsWith('https://')"
  #     message: "endpoint must use https"
  endpoint: "https://api.anthropic.com"

  # Credentials: a reference to a Secret in the operator's namespace.
  # The gateway reads this directly from agentry-system; credentials
  # never leave that namespace.
  credentialsRef:
    name: anthropic-api-key
    key: api-key

  # Models offered through this provider. The gateway validates that requested
  # models are in this list; unknown models are rejected.
  # Each entry's `id` must be unique within the provider — the gateway routes
  # by the qualified name `{providerRef}/{modelId}`, so duplicates would silently
  # win-last. Uniqueness is enforced structurally, not via CEL: the CRD schema
  # declares the list as a map keyed by id —
  #   x-kubernetes-list-type: map
  #   x-kubernetes-list-map-keys: ["id"]
  # — so the apiserver rejects duplicate ids natively at apply time and
  # server-side apply merges entries by id. (A quadratic CEL rule such as
  # self.all(m, self.exists_one(n, n.id == m.id)) is not usable here: the
  # apiserver statically budgets worst-case CEL cost at CRD write time, and an
  # O(n²) walk over an unbounded array exceeds the per-rule cost limit,
  # rejecting the CRD itself.)
  models:
    - id: "claude-opus-4-6"
      displayName: "Claude Opus 4.6"
      costPer1MInputTokens:  "15.00"
      costPer1MOutputTokens: "75.00"
    - id: "claude-sonnet-4-6"
      displayName: "Claude Sonnet 4.6"
      costPer1MInputTokens:  "3.00"
      costPer1MOutputTokens: "15.00"

  # Which namespaces may reference this provider.
  # "*" matches all namespaces. Empty list = no namespaces (provider is inert).
  allowedNamespaces:
    - "team-support"
    - "team-ml"
    - "sandbox-*"   # glob supported

  # Budget enforcement. Budgets are tracked per namespace, per calendar period.
  budget:
    # "monthly" (calendar month) | "daily" | "weekly" | "none"
    period: monthly
    perNamespaceUSD: "500.00"
    # Enforcement policy applied as budget is consumed.
    policies:
      - atPercent: 80
        action: degrade
        degradeTo: "claude-sonnet-4-6"   # model to downgrade to
      - atPercent: 100
        action: block    # "block" | "warn" | "degrade"
    # Cluster-wide ceiling (sum across all namespaces). Optional.
    clusterUSD: "10000.00"

  # Rate limits enforced at the gateway (per namespace, cluster-wide ceiling).
  # Buckets are keyed per (namespace, model): each (namespace, model) pair
  # carries the full configured ceiling independently, so a namespace's
  # aggregate throughput against this provider can reach ceiling × the number
  # of models it uses (see GATEWAY_LLM.md § Rate Limiting).
  # Each gateway replica divides these values by the number of active replicas
  # and enforces its share independently. The configured value represents the
  # intended cluster-wide limit regardless of replica count.
  rateLimits:
    requestsPerMinute: 300
    tokensPerMinute: 500000

  # Fallback chain. If this provider is unavailable (network error, 5xx,
  # timeout), the gateway tries the next provider in order. A budget-blocked
  # primary does NOT trigger fallback — the gateway returns 429
  # budget_exhausted immediately. A budget-blocked *fallback* candidate is
  # skipped (an attempt slot IS consumed) and its own `spec.fallback` children
  # are walked. If the entire walk is exhausted by error, budget-block, or
  # maxFallbackDepth, the gateway returns a fallback-exhausted error — never
  # 429: 502 provider_error in the general case, or 503/504 when every
  # attempt failed unreachable / timed out (see GATEWAY_LLM.md § Depth cap
  # semantics for the failure-class mapping). The
  # gateway walks each fallback provider's own fallback chain, up to the
  # gateway-level maxFallbackDepth setting (default 3). Referenced providers
  # must also allow the namespace. See GATEWAY_LLM.md § Fallback Logic for the
  # traversal algorithm.
  fallback:
    - name: openai-fallback

  # Health check configuration.
  healthCheck:
    enabled: true
    intervalSeconds: 60
    timeoutSeconds: 10
```

### Status

```yaml
status:
  observedGeneration: 2
  conditions:
    - type: Ready
      status: "True"
      reason: CredentialsValid
      message: ""
    - type: Healthy
      status: "True"
      reason: UpstreamReachable
      lastProbeTime: "2026-04-05T12:00:00Z"
  budgetUsage:
    - namespace: "team-support"
      period: "2026-04"
      spentUSD: "287.50"
      percentUsed: 57
      state: "Normal"    # "Normal" | "Throttled" | "Blocked"
    - namespace: "team-ml"
      period: "2026-04"
      spentUSD: "412.00"
      percentUsed: 82
      state: "Throttled"
  clusterSpentUSD: "699.50"
```

### Design notes

- **Secret scoping**: credentials are referenced from the operator's namespace and read directly by the gateway in `agentry-system`. They never leave that namespace or reach agent containers.
- **Budget state**: persisted in status is the source of truth for display, but the gateway maintains a local authoritative counter that is synced to status periodically. This matters because status updates are rate-limited and lossy. See [Budget State Management](./GATEWAY_LLM.md#budget-state-management).
- **Budget period boundaries**: budget periods reset at midnight UTC. `monthly` = first day of the UTC calendar month at 00:00; `weekly` = Monday 00:00 UTC; `daily` = 00:00 UTC. Per-replica rollover detection, archival of previous-period totals to status, and the underestimate behavior during rollover are documented in [GATEWAY_LLM.md § Budget State Management](./GATEWAY_LLM.md#budget-state-management). The `Retry-After` header on `429 budget_exhausted` ([API_ENDPOINTS.md § LLM Gateway error responses](./API_ENDPOINTS.md#llm-gateway-error-responses)) is the delta-seconds to the next reset.
- **Budget enforcement hierarchy**: every routed request checks both `clusterUSD` (sum across namespaces) and `perNamespaceUSD` (the calling namespace's share). The request is blocked with `429 budget_exhausted` if either `clusterSpent + cost > clusterUSD` OR `nsSpent + cost > perNamespaceUSD`. `error.message` names which ceiling fired (`"cluster budget exhausted"` vs `"namespace budget exhausted: <ns>"`) so operators can attribute the block. `Retry-After` is the delta-seconds to the next period reset (see the boundary rule above). Setting `clusterUSD` without `perNamespaceUSD` (or vice versa) is supported — the unset ceiling is simply not enforced. See [GATEWAY_LLM.md § Budget State Management](./GATEWAY_LLM.md#budget-state-management) for replica-side accounting.
- **Glob in `allowedNamespaces`**: supports common patterns like `team-*`. Uses Go's [`path.Match`](https://pkg.go.dev/path#Match) rules — `*` matches any sequence of non-`/` characters and does not cross path separators. Since Kubernetes namespace names are DNS labels (no `/`), `*` behaves as expected: `sandbox-*` matches `sandbox-foo` but not `sandbox-foo-bar/sub`. Exact match is preferred where possible.
- **Fallback chains** form a tree (each provider may have its own `spec.fallback` list) that the gateway walks **depth-first in declared order**. The gateway-level `maxFallbackDepth` cap (default 3) bounds the **total number of providers attempted per request, including the primary** — not the nesting depth of the tree. With the default, the gateway tries at most three providers before giving up, regardless of the tree's shape. Circular references are rejected by validation. All providers in the chain must have the **same `spec.type`** as the primary provider (e.g., all `anthropic` or all `openai-compatible`). Cross-format fallback is not supported in v1 — the gateway does not translate between API formats. The depth cap is a gateway-level operational setting (not per-ModelProvider) because it bounds request latency for the entire cluster. See [Fallback Logic](./GATEWAY_LLM.md#fallback-logic) for the traversal pseudocode.
- **Cost fields are strings** (not floats) to avoid precision issues. The gateway parses them as decimals.
- **`degradeTo` model validation**: every `degradeTo` value in `budget.policies` must reference a model `id` in the same provider's `spec.models` list. The ModelProviderReconciler validates this and sets `Ready=False, reason=InvalidDegradeTarget` if violated. See validation rule 18 in [Cross-Resource Validation](#cross-resource-validation).
- **`degradeTo` cost sanity**: after the existence check passes, the reconciler computes `(costPer1MInputTokens + costPer1MOutputTokens) / 2` for the degrade target and compares it against the same metric for every other model in `spec.models`. If the target is not strictly the cheapest, the reconciler emits a `Warning` event (`reason=DegradeTargetNotCheapest`) on the ModelProvider naming the cheaper alternative. This is **advisory only** — it does not block `Ready=True`, since platform teams may have non-cost reasons to prefer a particular degrade target (latency, capability, quality). The check catches the common misconfiguration where a policy labelled "degrade" silently escalates cost at the budget threshold. See [ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler).

---

## Agent

Agent is a namespace-scoped, developer-facing resource representing a persistent agent workload. It is the primary interface developers interact with.

### Spec

```yaml
apiVersion: agentry.io/v1alpha1
kind: Agent
metadata:
  name: support-assistant
  namespace: team-support
spec:
  # Reference to an AgentClass (required).
  agentClassRef:
    name: standard

  # Container spec for the agent itself. Image must match AgentClass.allowedImages.
  image: "registry.internal.corp/agents/support:v2.3.1"
  command: []            # optional override
  args: []               # optional override
  env:                   # optional env vars (merged with controller-injected ones)
    - name: LOG_LEVEL
      value: "info"

  # ModelProviders this agent uses.
  # Optional: omit entirely for agents that do not call LLM providers
  # (e.g., sub-agents, coding agents with IDE integration, pure webhook handlers).
  providers:
    - providerRef: { name: anthropic-shared }

  # Resource overrides (must fit within AgentClass.resources.maxLimits).
  resources:
    requests: { cpu: "500m", memory: "1Gi" }
    limits:   { cpu: "1",    memory: "2Gi" }

  # Persistence: request a PVC mounted into the agent container.
  # Setting enabled=true requires the referenced AgentClass to also have
  # persistence.enabled=true — see rule 24 in Cross-Resource Validation.
  persistence:
    enabled: true
    sizeGi: 10
    mountPath: "/var/agent/memory"
    # Optional: mount a pre-existing PVC instead of provisioning a new one
    # (e.g., a PVC restored from a VolumeSnapshot of a finished AgentTask's
    # workspace — the S9 promotion pattern). Mutually exclusive with sizeGi;
    # CRD CEL enforces: !has(self.sizeGi) || !has(self.existingClaim).
    # The PVC must already exist in the Agent's namespace. See rule 27 in
    # Cross-Resource Validation and the design notes below.
    # existingClaim: "fix-issue-342-workspace-snap"

  lifecycle:
    idleTimeout: "30m"           # transition to Idle after this much inactivity
    # Setting hibernationEnabled=true requires the referenced AgentClass to
    # also have lifecycle.hibernationAllowed=true (rule 26) AND this Agent to
    # have spec.persistence.enabled=true (rule 29) — see Cross-Resource
    # Validation.
    hibernationEnabled: true
    hibernationDelay: "30m"      # how long to stay Idle before hibernating; defaults from AgentClass
    activitySource: gatewayTraffic   # "gatewayTraffic" | "agentHeartbeat" | "both"
    wakeTimeout: "2m"                # max time gateway waits for Pod Ready on wake; defaults from AgentClass

  # Service exposure. Only ClusterIP is supported in v1.
  service:
    enabled: true
    port: 8080

  # Optional: declare MCP servers the agent will connect to.
  # Used to scope NetworkPolicy egress rules. Does not provision the servers.
  mcpServers:
    - name: github-tools
      url: "https://mcp.internal.corp/github"
```

### Status

```yaml
status:
  observedGeneration: 1
  phase: Running       # Pending | Provisioning | Running | Idle | Hibernating | Hibernated | Resuming | Degraded | Failed | Terminating
  conditions:
    - type: Ready
      status: "True"
      reason: PodRunning
    - type: ProvidersReady
      status: "True"
      reason: AllProvidersHealthy
  endpoint: "https://support-assistant.team-support.svc.cluster.local:8080"
  podName: "support-assistant-7d4b9f"
  pvcName: "support-assistant-memory"
  lastActivityTime: "2026-04-05T11:58:22Z"
  phaseTransitionTime: "2026-04-05T08:00:00Z"   # set on every status.phase change
  hibernatedAt: null
  preDegradedPhase: null   # set on entry to Degraded, cleared on recovery
```

### Design notes

- **`metadata.name` must be a DNS-1123 label** — lowercase alphanumerics and `-` only, no dots, starting and ending with an alphanumeric, at most 63 characters. Enforced via a **root-scoped** CRD CEL rule — validation rules are not allowed under `metadata`, and `metadata.name` (with `generateName`) is the only metadata field reachable from the object root: `x-kubernetes-validations: [{rule: "self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63", message: "Agent name must be a DNS-1123 label (no dots, max 63 characters)"}]` at the root of the Agent schema. The length bound matters as much as the charset: Kubernetes allows namespaced resource names up to 253 characters, but the Agent name is used verbatim as a single DNS label in the cert SAN and as the per-Agent Service name (Service names are RFC-1035 labels, max 63) — a longer name would produce an invalid SAN label and fail Service creation. This is required so the gateway's cert-SAN namespace extraction is unambiguous: the gateway parses `{name}.{namespace}.svc.cluster.local` by splitting on `.` and reading label index 1. If a dotted name were allowed, a developer naming their Agent `admin.svc` in namespace `team-a` would produce the SAN `admin.svc.team-a.svc.cluster.local`, and label[1] would be `svc` rather than `team-a` — a namespace-identification bypass. The label-count check in the gateway (see [Namespace Identification § Mode 1](./GATEWAY_LLM.md#namespace-identification)) is defense in depth against this same pattern.
- **Persistent is the only agent mode in v1.** AgentTask serves the ephemeral use case. If future modes (e.g., `scheduled` for cron-style agents) are needed, a `mode` field will be added to the Agent spec.
- **`status.phaseTransitionTime`** is updated by the AgentReconciler on every `status.phase` change, in the same status patch that commits the new phase. It is distinct from the various `conditions[*].lastTransitionTime` fields, which can change on non-phase events (Ready toggling on PodNotReady, ProvidersReady changes, etc.) and so are not a reliable witness for "when did this Agent last change phase." The controller compares this timestamp against the gateway's `replicaStartedAt` to decide whether missing activity data should be treated as "unknown" versus genuine "no activity" — see [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) and [Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api).
- **`providers` is optional**. Agents that do not call LLM providers (sub-agents, coding agents with IDE integration, pure message handlers) omit it entirely. When present, it is a flat list of provider references. All providers are routed through `$AGENTRY_GATEWAY_ENDPOINT`. The agent uses a qualified model name format (`{providerRef}/{modelId}`, e.g., `anthropic-shared/claude-opus-4-6`) in API calls to identify both the provider and model. See [Provider Routing](./GATEWAY_LLM.md#provider-routing) for the full routing chain.
- **`activitySource`**: agents may not always have meaningful LLM traffic (could be polling, waiting on webhooks). Supporting `agentHeartbeat` lets the agent explicitly signal liveness. See [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) for the heartbeat protocol. **`agentHeartbeat` and `both` are intended for custom agent images that gate heartbeat emission on actual work.** Starter-template-based images should leave this at the default `gatewayTraffic` — the templates' unconditional 30s heartbeat (emitted in agent mode only; task-mode runtimes send no heartbeats) would otherwise keep the agent's last-activity timestamp permanently fresh and prevent any `Idle`/`Hibernated` transition. See [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md).
- **Hibernation requires persistence** (rule 29): `lifecycle.hibernationEnabled: true` on an Agent without `spec.persistence.enabled: true` moves the Agent to `phase=Degraded, reason=HibernationRequiresPersistence` at reconcile time. Hibernation is delete-Pod-keep-PVC — with no PVC there is nothing that survives the Pod, and the dedup-buffer persistence required by [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) item 7 would be impossible. See rule 29 in [Cross-Resource Validation](#cross-resource-validation).
- **`persistence.existingClaim`** mounts a pre-existing PVC instead of provisioning one — the enabler for promoting a finished AgentTask's workspace to a persistent Agent ([STORIES.md § S9](./STORIES.md#s9-promote-a-task-agent-to-persistent-for-human-takeover): snapshot the task PVC via standard `VolumeSnapshot` before TTL cleanup, restore it to a PVC, reference it here). Constraints: mutually exclusive with `sizeGi` (CRD CEL, rule 27); the claim must exist in the Agent's namespace at reconcile time, else `Ready=False, reason=ExistingClaimNotFound` and the Pod is not created; `AgentClass.spec.persistence.enabled: true` is still required (rule 24 gates `persistence.enabled` regardless of provisioning source); `maxSizeGi` is not enforced against pre-existing claims — platform teams bound those with namespace ResourceQuota. The reconciler does **not** add an ownerRef to a pre-existing PVC, so `pvcRetention` never applies to it: the Agent finalizer only manages PVCs Agentry provisioned, and an `existingClaim` PVC survives Agent deletion under either `pvcRetention` setting. AgentTask does not support `existingClaim` in v1 — task scratch storage is always task-owned.
- **`service` is always ClusterIP.** `spec.service.port` is the Service-facing port (default 8080) and is the only field a developer can override. The synthesized Service's `targetPort` is **always** the value of `$AGENTRY_HEALTH_PORT` injected into the Pod (default 8080), which is the port the agent process actually binds. The two are decoupled deliberately: the agent only knows about `$AGENTRY_HEALTH_PORT`, and overriding `spec.service.port` to expose a different cluster-facing port (e.g., 80) does not require any agent-side change. Setting them to different values is supported and works correctly. The Agentry User Gateway uses this Service to deliver channel messages over HTTPS (see [User Gateway Request Flow](./GATEWAY_USER.md#user-gateway--request-flow)). Developers who need external exposure create their own Ingress/HTTPRoute pointing at the Service.
- **TLS environment variables**: the controller injects `$AGENTRY_CA_CERT` (path to the Agentry CA trust bundle), `$AGENTRY_TLS_CERT` and `$AGENTRY_TLS_KEY` (paths to the cert-manager-issued per-Agent cert and key) into every agent Pod. These cert/key files serve a dual purpose:
  - **Server TLS** (gateway→agent): the agent serves HTTPS on its health/message port using this cert, which the gateway verifies against `agentry-ca` on message delivery.
  - **Client TLS / mTLS** (agent→gateway): the agent presents this same cert as a client certificate when calling `$AGENTRY_GATEWAY_ENDPOINT` (LLM requests and heartbeats), allowing the gateway to cryptographically identify the agent and its namespace without relying on network-layer source IPs.
  Starter templates handle both uses automatically — see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md). Custom images must configure their HTTP client to present the client cert for all calls to the gateway and must watch the cert/key files for rotation updates.

---

## AgentTask

AgentTask is a namespace-scoped resource representing an ephemeral, goal-driven agent workload. It is analogous to a Kubernetes Job: it runs once, pursues a completion condition, produces artifacts, and terminates.

### Spec

```yaml
apiVersion: agentry.io/v1alpha1
kind: AgentTask
metadata:
  name: fix-issue-342
  namespace: team-support
spec:
  agentClassRef:
    name: sandboxed

  image: "registry.internal.corp/agents/coder:v1.0.0"
  env:
    - name: TASK_GOAL
      value: "Fix GitHub issue #342 in repo acme/widgets and open a PR"
    - name: GITHUB_TOKEN
      valueFrom:
        secretKeyRef: { name: github-bot-token, key: token }

  providers:
    - providerRef: { name: anthropic-shared }

  resources:
    requests: { cpu: "1", memory: "2Gi" }
    limits:   { cpu: "2", memory: "4Gi" }

  # Scratch persistence for the task. Lifecycle is tied to the AgentTask.
  # Setting enabled=true requires the referenced AgentClass to also have
  # persistence.enabled=true — see rule 24 in Cross-Resource Validation.
  persistence:
    enabled: true
    sizeGi: 10
    mountPath: "/workspace"

  # Completion semantics
  completion:
    # How the task signals completion.
    # "agentReported": agent POSTs to gateway /v1/task/complete
    # "exitCode": task is complete when the container exits 0
    # "webhook": external service calls a controller webhook (v1.1+, not in v1)
    condition: agentReported
    timeout: "1h"
    # What to do if timeout is hit before completion. "Fail" (default) settles
    # the task in phase=TimedOut — a failure-class terminal phase kept distinct
    # from Failed so timeouts are attributable and exempt from backoffLimit
    # retries. "Succeed" settles it in phase=Succeeded, keeping any partial
    # agent-reported payload best-effort. See CONTROLLER_LIFECYCLE.md § AgentTask.
    onTimeout: Fail    # "Fail" | "Succeed" (rarely used) | "Retry" (v1.1+)
    # Retry on failure. v1 supports simple count-based retries, no backoff tuning.
    backoffLimit: 0

  # Artifacts to collect on completion. The agent includes values for these
  # names in the POST /v1/task/complete body.
  # Only valid with condition: agentReported. CRD schema enforces:
  # x-kubernetes-validations:
  #   - rule: "!has(self.artifacts) || size(self.artifacts) == 0 || !has(self.completion) || !has(self.completion.condition) || self.completion.condition != 'exitCode'"
  #     message: "artifacts cannot be collected with exitCode completion; use agentReported"
  # The has() guards are load-bearing: in CRD CEL, reading an absent optional
  # field is an evaluation error that FAILS validation. completion.condition
  # is defaulted at reconcile time (see Defaulting), so the stored spec may
  # omit it — an unguarded self.completion.condition would reject every
  # manifest that declares artifacts without an explicit completion block.
  artifacts:
    - name: pr-url
    - name: summary

  # Retention: how long to keep the AgentTask resource after completion.
  ttlSecondsAfterFinished: 3600
```

### Status

```yaml
status:
  observedGeneration: 1
  phase: Succeeded   # Pending | Provisioning | Running | Completing | Succeeded | Failed | TimedOut | Terminating
  conditions:
    - type: Completed
      status: "True"
      reason: AgentReported
      message: "Agent reported completion at 2026-04-05T11:30:42Z"
  # Stamped when the task transitions Provisioning -> Running (Pod Ready), in
  # the same status write as the phase change. spec.completion.timeout measures
  # from startTime, so scheduling and image-pull time never count against it;
  # Provisioning is bounded separately (see CONTROLLER_LIFECYCLE.md § AgentTask).
  startTime: "2026-04-05T11:05:12Z"
  completionTime: "2026-04-05T11:30:42Z"
  podName: "fix-issue-342-xk9p2"
  currentPodUID: "9d3e2c1b-4a5f-6d7e-8c9b-1a2f3e4d5c6b"
  # Incremented at the start of each backoffLimit retry cycle; compared
  # against spec.completion.backoffLimit to decide whether Failed is terminal.
  # See CONTROLLER_LIFECYCLE.md § Retry mechanics.
  retries: 0
  # Artifact values captured inline. Oversize artifacts are rejected by the
  # gateway with HTTP 413; agents must externalize large outputs and pass a
  # reference URL inline (see design notes below).
  artifactValues:
    pr-url: "https://github.com/acme/widgets/pull/587"
    summary: "Fixed null pointer in WidgetService.get(). Added regression test."
  agentReportedStatus: "success"   # "success" | "failure"
  agentReportedMessage: "PR opened successfully"
```

### Design notes

- **`metadata.name` must be a DNS-1123 label** — same constraint and rationale as the [Agent CRD](#agent), including the 63-character bound (the task name becomes a single DNS label in the SAN). Enforced via the same **root-scoped** CRD CEL pattern: `x-kubernetes-validations: [{rule: "self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63", message: "AgentTask name must be a DNS-1123 label (no dots, max 63 characters)"}]` at the root of the AgentTask schema. The gateway extracts namespace from the `{name}.{namespace}.task.agentry.io` SAN shape by splitting on `.` and reading label index 1; a dotted task name would shift the namespace label and defeat identification. See [Namespace Identification § Mode 1](./GATEWAY_LLM.md#namespace-identification).
- **`completion.condition: agentReported` is the v1 default.** The agent container calls the gateway's completion endpoint with a status payload that may include artifact key-value pairs. This is more flexible than exit codes alone because the agent can report structured metadata and artifacts in a single call. See [POST /v1/task/complete](./API_ENDPOINTS.md#post-v1taskcomplete-agenttask-only) for the endpoint spec.
- **Artifact collection via completion payload**: artifacts are declared by name in the spec. The agent includes artifact values (keyed by name) in the `POST /v1/task/complete` body. The gateway validates the payload's artifact names against `spec.artifacts` and returns `400 invalid_request` synchronously on mismatch; the rule splits by `status` — `status: "success"` requires every declared name present and no undeclared names, while `status: "failure"` enforces only the no-undeclared-names half so a failing task can report a subset of declared artifacts (or none) — see [POST /v1/task/complete](./API_ENDPOINTS.md#post-v1taskcomplete-agenttask-only); the AgentTaskReconciler re-validates defensively when reading the ConfigMap. This eliminates race conditions, removes the need for `pods/exec` RBAC, gives the agent a synchronous error it can log and exit non-zero on, and keeps the artifact contract simple. Artifact size limits apply: 4 KiB per artifact, 32 KiB total. Oversize payloads are rejected at the gateway with HTTP 413; agents must externalize large outputs (object storage, Git, etc.) and place a reference URL in the artifact value. There is no auto-spill into ConfigMaps — the inline payload is the only delivery path.
- **Gateway↔reconciler completion protocol**: the per-task `{taskName}-completion` ConfigMap is the **data** channel — the gateway patches it with the completion payload, the reconciler watches it for changes. `status.currentPodUID` is the **identity gate** — the AgentTaskReconciler stamps it with the current Pod's UID on every Pod creation (initial provisioning and `backoffLimit` retries) and clears it (`""`) during the retry-reset window; the gateway resolves the calling Pod's UID at `/v1/task/complete` admission and rejects mismatched callers with `403 access_denied` `reason=StalePodCompletion`. Combined with a terminal-phase rejection (`reason=TaskAlreadyCompleted` when `status.phase ∈ {Succeeded, Failed, TimedOut}`), this prevents stale writes from a terminated Pod (in-flight after retry) and silent drops from a delayed second call against a completed task — both of which the data-channel reset alone cannot close. See [API_ENDPOINTS.md § /v1/task/complete](./API_ENDPOINTS.md#post-v1taskcomplete-agenttask-only) 403 cases (c) and (d) for the wire-level contract and [CONTROLLER_LIFECYCLE.md § Retry mechanics](./CONTROLLER_LIFECYCLE.md#agenttask) for the clear/reset/create/restamp ordering.
- **`onTimeout: Retry` and `completion.condition: webhook` are intentionally deferred.** v1 is simple: one attempt, report or exit, collect artifacts, done. The CRD schema enforces this: `spec.completion.condition` accepts only `agentReported` and `exitCode` in v1 via `x-kubernetes-validations: [{rule: "self in ['agentReported', 'exitCode']", message: "webhook completion condition is not supported in v1"}]` — invalid values are rejected at apply time.
- **`exitCode` does not support artifact collection.** Artifacts are collected via the `POST /v1/task/complete` payload, which is only used by `agentReported` mode. Declaring `spec.artifacts` with `completion.condition: exitCode` is rejected by CRD schema validation. Tasks using `exitCode` that need to produce output should write results to an external system (e.g., a Git repository, object storage) and rely on the container logs for status.
- **`agentReportedStatus`** mirrors the `status` field from the agent's [`POST /v1/task/complete`](./API_ENDPOINTS.md#post-v1taskcomplete-agenttask-only) payload — `"success"` or `"failure"`. The gateway rejects other values with `400 invalid_request` synchronously, so `agentReportedStatus` always settles to one of those two when populated.
- **`ttlSecondsAfterFinished`** mirrors Job semantics. The controller garbage-collects the resource (and its Pod, PVC) after the TTL.
- **Concurrency**: unlike Job, AgentTask is always parallelism=1 in v1. Parallel fan-out tasks would be a separate future resource (`AgentTaskSet`) rather than a field on AgentTask.
- **Runtime-contract guarantees (same as Agent)**: the AgentTaskReconciler injects the full `$AGENTRY_*` environment-variable set on the task Pod (`$AGENTRY_HEALTH_PORT`, `$AGENTRY_GATEWAY_ENDPOINT`, `$AGENTRY_CA_CERT`, `$AGENTRY_TLS_CERT`, `$AGENTRY_TLS_KEY`) and creates a per-task cert-manager `Certificate` (`{taskName}-tls`) with `usages: [client auth]`. The output Secret mounts at `/var/run/agentry/` so the task image presents a valid mTLS client cert on every call to `$AGENTRY_GATEWAY_ENDPOINT` (LLM requests and `POST /v1/task/complete` — tasks send no heartbeats: `/v1/agent/heartbeat` is Agent-only and rejects per-task certs with 403). The Certificate's SAN is `{taskName}.{namespace}.task.agentry.io` — a non-Service shape, since tasks have no Service. See [AgentTaskReconciler](./CONTROLLER_RECONCILERS.md#agenttaskreconciler) and [Namespace Identification](./GATEWAY_LLM.md#namespace-identification) for the full flow.

---

## AgentChannel

AgentChannel is a namespace-scoped resource that connects a running Agent to a user-facing communication channel. In v1, the only supported channel type is **webhook** (generic inbound HTTP POST with configurable auth). Discord, WhatsApp, and other platform-specific adapters are deferred to v1.1 — they require persistent connections and platform-specific reconnection logic that adds significant implementation surface.

### Spec

```yaml
apiVersion: agentry.io/v1alpha1
kind: AgentChannel
metadata:
  name: support-discord
  namespace: team-support
spec:
  # The Agent this channel delivers messages to (required).
  agentRef:
    name: support-assistant

  # Channel platform type. v1 supports: "webhook"
  # Discord and WhatsApp adapters are planned for v1.1.
  type: webhook

  # Webhook-specific configuration.
  webhook:
    # The gateway exposes this path externally (requires an Ingress pointing
    # at the gateway Service in agentry-system).
    # Must begin with /channels/{namespace}/ — enforced at reconcile time by
    # the AgentChannelReconciler (Ready=False, reason=InvalidPath on violation)
    # and independently by the gateway, which refuses to register
    # non-conforming paths. CRD CEL cannot express this rule: metadata.namespace
    # is not reachable from CRD validation rules at any scope (only
    # metadata.name / metadata.generateName are). See rule 15 in
    # Cross-Resource Validation.
    path: /channels/team-support/support-assistant
    # Auth type: "bearer" or "hmac". CRD schema enforces:
    #   x-kubernetes-validations:
    #     - rule: "self.type == 'bearer' ? has(self.secretRef) : true"
    #       message: "secretRef is required when auth type is bearer"
    #     - rule: "self.type == 'hmac' ? has(self.hmac) : true"
    #       message: "hmac block is required when auth type is hmac"
    auth:
      type: bearer                              # "bearer" | "hmac"
      secretRef: { name: webhook-secret, key: token }   # required for bearer
      # For HMAC signature verification (e.g., GitHub, Stripe, Shopify, Twilio):
      # type: hmac
      # hmac:
      #   header: "X-Hub-Signature-256"         # request header containing the signature
      #   algorithm: sha256                      # "sha256" | "sha1"
      #   secretRef: { name: webhook-hmac-secret, key: secret }
      #   # Optional. Literal prefix the gateway strips off the header value
      #   # before decoding the digest (default ""). Set to "sha256=" for
      #   # GitHub's X-Hub-Signature-256: sha256=<hex> shape.
      #   signaturePrefix: "sha256="
      #   # Optional. Digest encoding in the header (default "hex", lowercase
      #   # case-insensitive compare). Use "base64" (standard, not URL-safe)
      #   # for senders like Shopify's X-Shopify-Hmac-Sha256. These two fields
      #   # apply to inbound webhook verification only — the polling endpoint
      #   # always uses bare hex (see API_ENDPOINTS.md § Polling Fallback).
      #   encoding: hex                          # "hex" | "base64"
    # How the webhook adapter extracts the userId for session tracking.
    # At most one of fromHeader / fromBody may be set; both are optional.
    # When omitted, userId defaults to the empty string — all unattributed
    # requests share one session if session.enabled is true.
    # CRD schema enforces the mutex via:
    #   x-kubernetes-validations:
    #     - rule: "!has(self.fromHeader) || !has(self.fromBody)"
    #       message: "at most one of fromHeader or fromBody may be set"
    userId:
      fromHeader: "X-User-Id"       # read userId from this request header
      # fromBody: ".user.id"        # alternative: dotted JSON path into the body (see design notes)
      fallback: "anonymous"         # value when userId cannot be resolved
    # How the webhook adapter extracts the message content for delivery to
    # the agent's /v1/message envelope. At most one of fromHeader / fromBody
    # may be set; both are optional. When neither is set, the gateway uses
    # the raw inbound body, JSON-encoded as a string, as `content` —
    # preserving the generic-webhook story for senders whose body shape
    # Agentry has no a-priori knowledge of. The raw-body path requires
    # valid UTF-8; non-UTF-8 bodies are rejected with 400 invalid_request,
    # and binary senders must configure fromHeader/fromBody explicitly.
    # CRD schema enforces the mutex
    # via the same CEL pattern used for userId above:
    #   x-kubernetes-validations:
    #     - rule: "!has(self.fromHeader) || !has(self.fromBody)"
    #       message: "at most one of fromHeader or fromBody may be set"
    content:
      fromHeader: "X-Message-Text"        # read content from this request header
      # fromBody: ".message.text"         # alternative: dotted JSON path into the body (see design notes)
      # fallback: ""                      # value when extraction fails (header absent or path does not resolve)
    # Response mode: "sync" (default) or "async".
    # sync: gateway blocks until the agent responds and returns the response
    #   as the webhook HTTP response body.
    # async: gateway returns 202 Accepted immediately with a requestId,
    #   delivers the message to the agent, and posts the agent's response
    #   to callbackUrl (if configured) or makes it available for polling.
    responseMode: sync
    # Required when responseMode is "async" and push-based delivery is desired.
    # The gateway POSTs the agent's response to this URL when it becomes available.
    # If omitted in async mode, responses are stored and retrievable via polling
    # at GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}.
    #
    # Constraints (enforced by AgentChannelReconciler; see rules 22 and 25 below):
    #   - scheme must be https://
    #   - the host must not resolve to loopback (127.0.0.0/8, ::1),
    #     link-local (169.254.0.0/16, fe80::/10), RFC1918 private ranges
    #     (10/8, 172.16/12, 192.168/16), unique-local IPv6 (fc00::/7), or
    #     cloud metadata IPs (169.254.169.254, fd00:ec2::254)
    #   - callbackAuth (below) must also be set — every callback POST is signed
    # The check is re-run at each delivery attempt (DNS is re-resolved then to
    # defeat rebinding) — see GATEWAY_USER.md § Request Flow. Platform teams
    # that need callbacks into the cluster can override this deny-internal
    # default with an explicit allowlist via the Helm value
    # gateway.callbackUrl.allowlist (DNS-name suffixes or CIDR blocks).
    # callbackUrl: "https://my-service.example.com/agent-responses"
    #
    # Required when callbackUrl is set (enforced by rule 25 below). Defines the
    # signing material the gateway applies to every callback POST so receivers
    # can reject forged callbacks. Same shape as `auth` above; bearer adds an
    # Authorization header, hmac signs a canonical string of
    # "{requestId}\n{timestamp}\n{sha256(body)}" — see API_ENDPOINTS.md
    # § Callback delivery for the wire contract.
    # callbackAuth:
    #   type: hmac
    #   hmac:
    #     header: "X-Agentry-Signature"
    #     algorithm: sha256
    #     secretRef: { name: callback-signing-secret, key: secret }
    # Maximum number of in-flight async responses before the gateway rejects
    # new async requests with HTTP 503. Bounds ConfigMap creation in agentry-system.
    maxPendingAsyncResponses: 100

  # Optional: session configuration. When enabled, the gateway generates a
  # deterministic sessionId from (channelId, userId) and includes it in the
  # message envelope so the agent can maintain conversation context.
  # Session expiry/rotation is the agent's responsibility using its PVC state.
  session:
    enabled: true
```

### Future platform types (v1.1+)

When Discord and WhatsApp adapters are added in v1.1, the spec will support platform-specific configuration blocks:

```yaml
spec:
  type: discord
  credentialsRef:
    name: discord-bot-credentials
    key: bot-token
  discord:
    guildId: "123456789012345678"
    allowedChannelIds:
      - "987654321098765432"
```

These types require persistent connections (Discord WebSocket, WhatsApp Cloud API registration) and platform-specific reconnection logic, which is why they are deferred.

### Status

```yaml
status:
  observedGeneration: 1
  phase: Active     # Active | Degraded | Failed | Terminating (unset until first reconcile)
  conditions:
    - type: Ready
      status: "True"
      reason: AgentReachable
    - type: PlatformConnected
      status: "True"
      reason: WebhookReady
      message: "1 successful inbound request in the last 5m"
```

`PlatformConnected` is a **tri-state** condition reflecting the gateway's view of the channel's recent inbound delivery health, evaluated over a rolling window (`gateway.channelHealthWindow`, default `5m`):

- `status: "True"`, `reason: WebhookReady` — at least one in-window inbound request succeeded (auth passed + message dispatched to the agent).
- `status: "False"` with `reason` ∈ {`WebhookAuthFailed`, `AgentNotReady`, `DispatchFailed`, `CallbackInvalid`, `CallbackRejected`} — the in-window observations are all failures; the reason reflects the most recent failure. The two `Callback*` reasons come from outbound callback dispatch on async channels: `CallbackInvalid` when the `callbackUrl` fails the pre-dial deny-range / allowlist re-check, `CallbackRejected` when the receiver terminally rejects the POST — see [API_ENDPOINTS.md § Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed).
- `status: "Unknown"`, `reason: NoRecentTraffic` — no in-window observations exist on any replica that has been up the full window. Distinguishes a truly idle channel from one that was last known healthy hours ago.

The reduction across gateway replicas is performed by the `AgentChannelReconciler`. v1.1+ persistent-connection channels will reuse the same tri-state contract, with reasons sourced from gateway-side connection events. See [GATEWAY_USER.md § Channel Health Tracking](./GATEWAY_USER.md#channel-health-tracking) for the per-replica state model and cross-replica reduction.

**`status.phase` vs `conditions[type=PlatformConnected]`** — these signals are orthogonal:

- `phase: Degraded` reflects the **bound Agent's** state. The `AgentChannelReconciler` sets it when the referenced Agent transitions to a non-serving phase (e.g., `Failed`) — see [AgentChannelReconciler step 5](./CONTROLLER_RECONCILERS.md#agentchannelreconciler). A channel whose Agent is broken cannot deliver messages regardless of inbound auth state.
- `conditions[type=PlatformConnected]` reflects **inbound webhook** delivery health (auth pass/fail, dispatch success/failure), evaluated over the rolling window described above.

The two can disagree without contradiction: a channel can be `phase: Degraded` (Agent broken) while `PlatformConnected=Unknown` (no recent inbound to observe), and a channel with a healthy Agent (`phase: Active`) can still report `PlatformConnected=False` if recent inbound requests failed auth or dispatch.

### Design notes

- **v1 supports webhook only.** Discord, WhatsApp, and other platform-specific adapters are planned for v1.1. The webhook type is stateless and covers the core channel integration pattern without requiring persistent platform connections.
- **Webhook auth types**: `bearer` validates a static token from the `Authorization` header against the value in `secretRef`. `hmac` validates a request body signature: the gateway reads the signature from the configured `header` (e.g., `X-Hub-Signature-256`), strips any configured `hmac.signaturePrefix` (e.g., `"sha256="` for GitHub), decodes per `hmac.encoding` (`hex` default; `base64` for senders like Shopify), computes `HMAC(algorithm, secret, request_body)` using the shared secret from `hmac.secretRef`, and constant-time-compares the values. HMAC is preferred for integrations where the calling platform signs payloads (GitHub, Stripe, Shopify, Twilio) — it avoids exposing a static token in every request.
- **Poll-endpoint auth** (async response mode): `GET /v1/channels/responses/{requestId}` reuses the same `webhook.auth` configuration, but the HMAC input differs because poll GETs have no body. For `auth.type: hmac`, the caller computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}"` (unix seconds, LF delimiter, no trailing newline), sends the bare lowercase hex digest in the configured `header` (the `hmac.signaturePrefix` and `hmac.encoding` fields apply to inbound verification only — polling is an Agentry-canonical surface and always uses bare hex with no prefix), and presents the timestamp in `X-Agentry-Timestamp`. The gateway rejects requests whose timestamp differs from its wall clock by more than 300s to bound replay. For `auth.type: bearer`, the poll presents the same bearer token in `Authorization: Bearer …`. See [Async Webhook Response § polling fallback](./API_ENDPOINTS.md#async-webhook-response-gateway-managed).
- **Webhook path namespace scoping**: `spec.webhook.path` must begin with `/channels/{namespace}/` where `{namespace}` is the AgentChannel's own namespace. This is enforced at reconcile time by the AgentChannelReconciler (`Ready=False, reason=InvalidPath`) and re-checked by the gateway at path registration — CRD CEL cannot express the rule, because `metadata.namespace` is not reachable from CRD validation rules at any scope (only `metadata.name`/`metadata.generateName` are). Because the gateway routes only `Ready=True` channels **and** refuses non-conforming paths outright, a violating AgentChannel never receives traffic, so cross-tenant path conflicts remain impossible at the routing layer. Within a namespace, paths must be unique (see validation rules 15-16 in [Cross-Resource Validation](#cross-resource-validation)). The gateway routes webhook traffic only to AgentChannels with `status.conditions[type=Ready].status == True` — `Ready=False` channels (including the `PathConflict` loser) receive no traffic; see [GATEWAY_USER.md § Request Flow](./GATEWAY_USER.md#user-gateway--request-flow).
- **Delivery counts are not in status.** Per-message counters require either a status patch on every delivery (high etcd write pressure at scale) or a separate in-memory accumulation mechanism. Instead, delivery volume is tracked via the Prometheus metric `agentry_channel_messages_total{channel_type,namespace,status}` exposed by the gateway. Status reflects channel health (phase, conditions) — not traffic volume.
- **AgentChannel owns no Pod resources.** The gateway watches AgentChannel resources directly and manages webhook endpoints based on their specs. The reconciler's role is validation and status reporting.
- **Credentials stay in the agent's namespace.** Unlike LLM provider credentials (which live in `agentry-system`), channel credentials (webhook auth tokens, etc.) are stored in the agent's namespace for organizational isolation. They are typically created by the platform team or a provisioning service, not by the developer. The gateway reads them via scoped RBAC and holds them in-process for the channel adapter.
- **One AgentChannel per (Agent, channel) pair.** An Agent may have multiple AgentChannels (e.g., both a Discord channel and a webhook). Each is a separate resource.
- **Session management is opt-in.** When `session.enabled: true`, the gateway generates a **deterministic `sessionId`** from the message's `channelId` and `userId`: `sessionId = UUIDv5(namespace: agentry-session-ns, name: channelId + ":" + userId)`, where `agentry-session-ns` is the fixed constant `f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d` — a purpose-generated UUID published as part of the Agentry API specification, identical across all installations and versions. This ID is stable across gateway replicas and restarts — no gateway-side session state is required. Session expiry and rotation are the agent's responsibility: the agent uses its PVC to track conversation state and decides when a "session" is over. When `session.enabled: false`, no `sessionId` is included in the envelope. **This constant must not change after v1 ships** — any change would invalidate existing session state in agent PVCs.
- **userId extraction**: the webhook adapter resolves `userId` using `webhook.userId` config (`fromHeader` or `fromBody`). At most one may be set — the CRD schema enforces this via CEL (`!has(self.fromHeader) || !has(self.fromBody)`), rejecting invalid combinations at apply time. If both are absent, the adapter uses the empty string. When `session.enabled: true`, this means all requests that cannot be attributed to a user share a single session — set `fallback` explicitly to control this behavior. See the `webhook.userId` spec block above.
- **`fromBody` syntax — dotted JSON path**: `webhook.userId.fromBody` and `webhook.content.fromBody` accept a **strict subset** of jq-style paths, not the full jq language. Supported: object property access (`.foo.bar.baz`, leading dot required, property names match `[a-zA-Z_][a-zA-Z0-9_]*`) and numeric array indexing (`.foo[0]`). **Not supported**: filters, slicing, `[]` flatten, recursive descent (`..`), pipes, comparisons, or any jq expression. Invalid paths are rejected at apply time via CRD CEL on both fields (`x-kubernetes-validations` with `rule: "self.matches('^(\\.[a-zA-Z_][a-zA-Z0-9_]*(\\[[0-9]+\\])?)+$')"`, message `"fromBody must be a dotted JSON path: .foo.bar or .foo[0]"`). The narrow grammar keeps the extraction implementation small and unambiguous across language ports; senders whose payloads require richer extraction should pre-process upstream or configure `fromHeader` instead.
- **content extraction**: the webhook adapter resolves `content` using `webhook.content` config (`fromHeader` or `fromBody`). At most one may be set — the CRD schema enforces this via CEL (`!has(self.fromHeader) || !has(self.fromBody)`), mirroring the `userId` rule, rejecting invalid combinations at apply time. **If neither is configured**, the gateway uses the raw inbound body, JSON-encoded as a string, as `content` — preserving the generic-webhook story for callers whose body shape Agentry has no a-priori knowledge of (this is the structural reason the design supports any third-party webhook sender out of the box). The raw-body path requires valid UTF-8: bodies containing invalid UTF-8 bytes are rejected with `400 Bad Request` and `error.type: invalid_request` (`error.message` names the invalid byte offset). Operators with binary senders (e.g., protobuf-payload webhooks) must configure `webhook.content` explicitly — typically `fromHeader` — so the gateway never tries to UTF-8-decode the body. **If `fromBody` is configured but the inbound body cannot be parsed as JSON**, the gateway rejects the request with `400 Bad Request` and `error.type: invalid_request`; this applies symmetrically to `userId.fromBody`, since both extractions share the parse step. **If `fromBody` is configured and the body parses but the path does not resolve, or `fromHeader` is configured and the header is absent**, the configured `fallback` is used (empty string if omitted). Per-channel `attachments` and `metadata` extraction are out of scope in v1: the generic webhook adapter populates them as `[]` and `{}` respectively; v1.1 platform-specific adapters (Discord, WhatsApp) populate them via adapter code, not per-channel CRD config. See [API_ENDPOINTS.md § POST /channels/{namespace}/{channel-path}](./API_ENDPOINTS.md#post-channelsnamespacechannel-path-external--webhook-caller) for the wire contract.
- **The agent's Service must be enabled** (`spec.service.enabled: true`) for AgentChannel to function — the gateway delivers messages via the ClusterIP Service.
- **AgentChannel references Agent only, not AgentTask.** Tasks are ephemeral and lack a stable Service endpoint. The `agentRef` field must point to an `Agent` resource.
- **Async response mode** (`spec.webhook.responseMode: async`): designed for agents that take minutes to respond (e.g., coding agents, research agents). The async/sync distinction is handled entirely by the gateway. See [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the response schemas.
- **`maxPendingAsyncResponses`** (default: 100) caps the number of in-flight async responses per AgentChannel. The gateway counts a channel's live `agentry-async-{requestId}` ConfigMaps via its `agentry-system` ConfigMap informer (label selector on `agentry.io/channel-namespace` / `agentry.io/channel-name`) and rejects new async requests with HTTP 503 when the count is at the limit — informer-based counting makes the cap replica-agnostic without new coordination state, at the cost of being approximate under concurrent bursts (multiple replicas can admit simultaneously before their informers observe each other's placeholder `Create`s; the overshoot is bounded by in-flight acceptances per informer-lag window, acceptable for an etcd-pressure guardrail). This bounds the number of `agentry-async-{requestId}` ConfigMaps created in `agentry-system` per channel, preventing unbounded etcd pressure from burst traffic or slow agents.
- **Wake-on-demand integration**: if the target Agent is `Hibernated` when a webhook message arrives, the gateway wakes it via the [Activator](./GATEWAY_USER.md#activator) before delivering the message. In sync mode, the webhook caller blocks until the agent is ready and responds, or receives a timeout error if `wakeTimeout` is exceeded. In async mode, the gateway returns 202 immediately and handles wake + delivery asynchronously.

---

## Cross-Resource Validation

The following constraints are enforced via a mix of CRD CEL `x-kubernetes-validations` (apply-time, rejected by the apiserver) and reconcile-time checks in the relevant reconciler. Reconcile-time violations surface in status — most as `Ready=False` with a specific `reason`; AgentClass-vs-Agent-spec mismatches (rules 2, 5, 24, and 26) follow the established class-mismatch handling and transition the Agent to `phase=Degraded` (or the AgentTask to `phase=Failed`) with `reason` set per the rule (`ClassConstraintViolation` for image/provider, `PersistenceNotAllowed` for persistence, `HibernationNotAllowed` for hibernation); rule 29 (a spec-internal hibernation/persistence coupling) reuses the same `Degraded` handling with `reason=HibernationRequiresPersistence`; a small subset (rule 20) emit only a `Warning` event without affecting `Ready`. None are surfaced via admission webhook — Agentry has no admission webhook server.

1. `Agent.spec.agentClassRef` and `AgentTask.spec.agentClassRef` must resolve to an existing AgentClass.
2. `Agent.spec.image` and `AgentTask.spec.image` must match at least one pattern in `AgentClass.spec.image.allowedImages` (if the list is non-empty).
3. Every `providerRef` in Agent/AgentTask must resolve to a ModelProvider (when `spec.providers` is present).
4. Every referenced ModelProvider must have the Agent's namespace in its `allowedNamespaces`.
5. Every referenced ModelProvider must appear in the AgentClass's `allowedProviders`.
6. Resource `limits` in Agent/AgentTask must not exceed `AgentClass.spec.resources.maxLimits`.
7. `persistence.sizeGi` must not exceed `AgentClass.spec.persistence.maxSizeGi`.
8. `lifecycle.idleTimeout` must not exceed `AgentClass.spec.lifecycle.maxIdleTimeout`.
9. `lifecycle.wakeTimeout` must not exceed `AgentClass.spec.lifecycle.maxWakeTimeout`.
10. `lifecycle.hibernationDelay` must not exceed `AgentClass.spec.lifecycle.maxHibernationDelay`.
11. `ModelProvider.spec.fallback` chains must not be circular (validated by walking the full chain up to `maxFallbackDepth`).
12. `ModelProvider.spec.fallback` entries must have the same `spec.type` as the primary provider (no cross-format fallback).
13. `AgentChannel.spec.agentRef` must resolve to an existing Agent.
14. The referenced Agent must have `spec.service.enabled: true` for an AgentChannel to be valid.
15. `AgentChannel.spec.webhook.path` must begin with `/channels/{namespace}/` where `{namespace}` matches the AgentChannel's own namespace. Enforced at **reconcile time** by the AgentChannelReconciler (`Ready=False, reason=InvalidPath`) and independently by the gateway, which refuses to register non-conforming paths — CRD CEL cannot express this rule, because `metadata.namespace` is not reachable from CRD validation rules at any scope (only `metadata.name`/`metadata.generateName` are; contrast rule 21, which is expressible because it needs only `metadata.name`). Namespace-scoping eliminates cross-tenant path conflicts at the routing layer — two namespaces cannot claim the same path prefix, and a violating channel never receives traffic. Within a namespace, paths must still be unique; on conflict, the reconciler marks the newer AgentChannel (by `creationTimestamp`) as `Ready=False, reason=PathConflict`, and the gateway routes webhook traffic only to AgentChannels whose `status.conditions[type=Ready].status == True` (see [GATEWAY_USER.md § Request Flow step 4](./GATEWAY_USER.md#user-gateway--request-flow)).
16. `AgentChannel.spec.webhook.path` must not begin with `/v1/`. Overlaps rule 15 (the required `/channels/` prefix inherently avoids `/v1/`), but rule 15 is reconcile-time while this rule is field-scoped CRD CEL — retaining it makes it the only **apply-time** guard on reserved paths, rejecting `/v1/`-prefixed paths at the API server before the reconciler ever observes them. See [Reserved Gateway Paths](./API_ENDPOINTS.md#reserved-gateway-paths).
17. `AgentTask` with `spec.completion.condition: exitCode` must not declare `spec.artifacts`. Artifact collection requires the `POST /v1/task/complete` payload, which is only available in `agentReported` mode. Enforced via CRD schema validation (CEL).
18. `ModelProvider.budget.policies[x].degradeTo` must match a model `id` in the same ModelProvider's `spec.models` list. A missing target model would cause silent routing failures when the budget threshold is crossed.
19. `AgentClass.spec.network.egress.allowedCIDRs` entries must parse as valid CIDR blocks (IPv4 or IPv6). Invalid entries cause `Ready=False, reason=InvalidCIDR` on the AgentClass.
20. `AgentClass.spec.network.egress.allowedHosts` entries must be valid DNS names (RFC 1123). If the field is non-empty and the AgentClassReconciler's startup CNI probe did not detect FQDN egress policy support (Cilium's `CiliumNetworkPolicy` CRD or Calico Enterprise's equivalent), the reconciler emits a `Warning` event (`reason=FQDNPolicyUnsupported`) on the AgentClass and ignores `allowedHosts` when synthesizing the per-agent NetworkPolicy. The AgentClass still becomes `Ready=True`; `allowedCIDRs` alone governs egress.
21. `Agent.metadata.name` and `AgentTask.metadata.name` must be DNS-1123 **labels** — `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, no dots, and at most 63 characters (`size(self.metadata.name) <= 63` — names are used verbatim as single DNS labels in cert SANs and as per-Agent Service names, both of which cap at 63; Kubernetes itself would allow up to 253). Enforced at apply time via **root-scoped** CRD CEL on both kinds (`self.metadata.name.matches(…) && size(self.metadata.name) <= 63` — `metadata.name` and `metadata.generateName` are the only metadata fields reachable from CRD validation rules, and only from the object root; `metadata.namespace` is not, which is why rule 15 is reconcile-time). Kubernetes allows DNS-1123 *subdomain* names (which permit dots) on most namespaced resources; Agentry restricts Agent and AgentTask to the stricter label form so the gateway's cert-SAN namespace extraction (which reads label index 1 of the `.`-split SAN) cannot be tricked by a dotted name. See the Agent and AgentTask design notes above for the threat scenario and [Namespace Identification § Mode 1](./GATEWAY_LLM.md#namespace-identification) for the gateway's defense-in-depth label-count check.
22. `AgentChannel.spec.webhook.callbackUrl`, when set, must use the `https://` scheme, and its host must not resolve to loopback (127.0.0.0/8, ::1), link-local (169.254.0.0/16, fe80::/10), RFC1918 private ranges (10/8, 172.16/12, 192.168/16), unique-local IPv6 (fc00::/7), or the cloud-metadata IPs 169.254.169.254 / fd00:ec2::254. Violations set `Ready=False, reason=InvalidCallbackUrl`. The AgentChannelReconciler performs the check at admission/reconcile time, and the gateway re-resolves the host and repeats the check on every delivery attempt to prevent DNS rebinding — see [GATEWAY_USER.md § Request Flow](./GATEWAY_USER.md#user-gateway--request-flow). The deny-internal default may be replaced with an explicit allowlist via the Helm value `gateway.callbackUrl.allowlist`.
23. `AgentClass.spec.image.imagePullSecrets[*].name`, when referenced by an Agent or AgentTask, must exist as a Secret in the **referencing workload's namespace** at reconcile time. AgentClass is cluster-scoped but Secrets are namespace-scoped; the controller does not copy Secrets across namespaces. Missing Secrets set `Ready=False, reason=ImagePullSecretMissing` on the Agent/AgentTask with a message naming the namespace and secret, and the Pod is not created. Checked in AgentReconciler and AgentTaskReconciler — see [AgentClass design notes](#design-notes).
24. `Agent.spec.persistence.enabled` and `AgentTask.spec.persistence.enabled` must not be `true` if the referenced AgentClass has `spec.persistence.enabled: false`. The class is the authority on whether persistence is allowed for workloads of that category — a developer cannot opt in to a PVC that the class disallows. Violations follow the established AgentClass-vs-spec mismatch handling (see [CONTROLLER_LIFECYCLE.md § AgentClass change handling](./CONTROLLER_LIFECYCLE.md#agentclass-change-handling)):
    - **Agent**: transitions to `phase=Degraded, reason=PersistenceNotAllowed` with `preDegradedPhase` set; the Pod and PVC are not created (or not recreated, if class drift introduced the conflict after the Pod was already running). The Agent re-enters its prior phase when the developer aligns the spec.
    - **AgentTask**: transitions to `phase=Failed, reason=PersistenceNotAllowed` (AgentTask has no `Degraded` phase; this matches the AgentClass-drift handling for backoff retries documented in [CONTROLLER_RECONCILERS.md § AgentTaskReconciler](./CONTROLLER_RECONCILERS.md#agenttaskreconciler)).

    Enforced at reconcile time by AgentReconciler and AgentTaskReconciler. CRD CEL cannot express this rule because it spans two resources and Kubernetes CEL validations on Agent cannot read AgentClass fields.
25. When `AgentChannel.spec.webhook.callbackUrl` is set, `AgentChannel.spec.webhook.callbackAuth` must also be set. Enforced at apply time via CRD CEL on AgentChannel (`has(self.spec.webhook.callbackUrl) ? has(self.spec.webhook.callbackAuth) : true`). Outbound callbacks must be cryptographically attributable to the gateway so receivers can reject forged POSTs; an unsigned `callbackUrl` would let any third party that learns the URL deliver fake response or error payloads. The AgentChannelReconciler additionally validates that the referenced Secret exists and contains the configured `data` key — same shape as the inbound `auth` validation. Violations set `Ready=False, reason=CallbackAuthMissing` (CEL bypass case) or `reason=CallbackAuthInvalid` (Secret not found / key missing). See [API_ENDPOINTS.md § Callback delivery](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the wire contract and [SECURITY.md threat model](./SECURITY.md#threat-model) for the forgery row.
26. `Agent.spec.lifecycle.hibernationEnabled` must not be `true` if the referenced AgentClass has `spec.lifecycle.hibernationAllowed: false`. The class is the authority on whether hibernation is allowed for workloads of that category — a developer cannot opt in to hibernation that the class disallows. Violations follow the same AgentClass-vs-spec mismatch handling as rule 24: the Agent transitions to `phase=Degraded, reason=HibernationNotAllowed` with `preDegradedPhase` set; the Pod is not created (or not recreated, if class drift introduced the conflict after the Pod was already running). The Agent re-enters its prior phase when the developer aligns the spec. AgentTask is not affected — hibernation does not apply to one-shot tasks. Enforced at reconcile time by AgentReconciler. CRD CEL cannot express this rule because it spans two resources — same reasoning as rule 24.
27. `Agent.spec.persistence.existingClaim`, when set, must (a) not be combined with `persistence.sizeGi` — enforced at apply time via CRD CEL on the `persistence` block (`!has(self.sizeGi) || !has(self.existingClaim)`) — and (b) name a PersistentVolumeClaim that exists in the Agent's namespace at reconcile time; a missing claim sets `Ready=False, reason=ExistingClaimNotFound` with a message naming the claim, and the Pod is not created. Rule 24 (class-level `persistence.enabled`) applies to `persistence.enabled: true` regardless of whether the PVC is Agentry-provisioned or pre-existing. The reconciler adds no ownerRef to a pre-existing PVC, so `pvcRetention` semantics apply only to Agentry-provisioned PVCs — see the [Agent design notes](#agent). AgentTask does not support `existingClaim` (the field exists only on the Agent schema).
28. `Agent`, `AgentTask`, and `AgentChannel` resources must not be created in `agentry-system`. The reconcilers refuse to provision workloads there — `Ready=False, reason=SystemNamespaceForbidden`, no child resources created. This is a SAN-integrity guard: per-Agent certificates carry the SAN `{name}.{namespace}.svc.cluster.local`, so an Agent named `agentry-gateway` in `agentry-system` would be issued a certificate whose SAN equals the gateway's own — the exact identity that every internal SAN-based authorization (activator, `/v1/activity`, `/v1/channels/health`, and the agent-side `/v1/message` client-cert check) trusts. Enforced at reconcile time (namespace scoping is not expressible in CRD CEL — same limitation as rule 15). See the [SECURITY.md threat model](./SECURITY.md#threat-model) for the collision scenario.
29. `Agent.spec.lifecycle.hibernationEnabled` must not be `true` unless the same Agent has `spec.persistence.enabled: true`. Hibernation works by deleting the Pod while keeping the PVC and recreating the Pod with the same mount ([CONTROLLER_LIFECYCLE.md § Hibernation mechanics](./CONTROLLER_LIFECYCLE.md#hibernation-mechanics)), and [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) item 7 requires hibernation-enabled agents to persist their message-dedup buffer across Pod restarts — neither is possible without a PVC. Violations follow the same handling as rules 24 and 26: the Agent transitions to `phase=Degraded, reason=HibernationRequiresPersistence` with `preDegradedPhase` set; the Pod is not created (or not recreated). The Agent re-enters its prior phase when the developer aligns the spec. AgentTask is not affected — hibernation does not apply to one-shot tasks. Enforced at reconcile time by AgentReconciler alongside rules 24 and 26 — the three checks share the mismatch-handling path, and reconcile-time enforcement surfaces the violation on pre-existing Agents as recoverable `Degraded` status rather than stranding them behind a new apply-time rule.

Field-level schema validation uses CEL expressions (`x-kubernetes-validations`) embedded in the CRD OpenAPI schema. No admission webhook server is required.

## Defaulting

AgentClass defaults are applied at reconcile time when Agent/AgentTask fields are absent:

- `resources` defaults from `AgentClass.spec.resources.defaults`
- `persistence.sizeGi` defaults from `AgentClass.spec.persistence.defaultSizeGi` — not applied when `persistence.existingClaim` is set (no PVC is provisioned, so a size is meaningless; see rule 27)
- `image` defaults from `AgentClass.spec.image.defaultImage`
- `lifecycle.idleTimeout` defaults from `AgentClass.spec.lifecycle.defaultIdleTimeout`
- `lifecycle.hibernationDelay` defaults from `AgentClass.spec.lifecycle.defaultHibernationDelay`
- `lifecycle.wakeTimeout` defaults from `AgentClass.spec.lifecycle.defaultWakeTimeout`

Defaults are applied at reconcile time rather than admission. The stored spec reflects what the developer wrote. Effective values can be derived by merging the AgentClass defaults at read time. This avoids a mutating webhook dependency while keeping the behavior predictable.
