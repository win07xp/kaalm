# Agentry — CRD API Design

This document defines the custom resources Agentry provides, their spec and status schemas, and the rationale for design choices. It is the canonical API reference for implementation.

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
  # Runtime backend: "pod" (default) or "agentSandbox"
  runtime:
    backend: pod
    runtimeClassName: runc           # optional; matches k8s RuntimeClass
    # For agentSandbox backend, additional fields control SandboxTemplate ref:
    # sandboxTemplateRef: { name: "standard-sandbox", namespace: "agent-sandbox-system" }

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
    enabled: true
    defaultSizeGi: 5
    maxSizeGi: 50
    storageClassName: "standard"   # k8s StorageClass
    reclaimPolicy: Delete          # Delete | Retain

  # Provider access: which ModelProviders may agents of this class reference?
  # Empty list = no providers allowed.
  allowedProviders:
    - name: anthropic-shared
    - name: openai-fallback

  # Network policy hints (the controller translates these into NetworkPolicy resources)
  network:
    egress:
      # Allowed external destinations beyond the Agentry gateway. Agent containers
      # call providers through the gateway; this governs other egress (MCP, tools).
      allowedHosts:
        - "mcp.internal.corp"
        - "api.github.com"
    allowHostNetwork: false

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
    hibernationEnabled: true
    defaultWakeTimeout: "2m"     # default time gateway waits for Pod Ready on wake
    maxWakeTimeout: "5m"         # cap on per-Agent wakeTimeout overrides
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
- `allowedProviders` is the primary access control mechanism for LLM providers at the class level. ModelProvider itself has `allowedNamespaces` for namespace-level control. Both must pass for an Agent to use a provider.
- Defaults vs. maxLimits: defaults are applied at reconcile time if the Agent does not specify; maxLimits are enforced regardless and reject manifests that exceed them.
- `runtime.backend: agentSandbox` is future-facing. v1 may ship with only `pod` and add the Sandbox backend in v1.1.

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
  endpoint: "https://api.anthropic.com"

  # Credentials: a reference to a Secret in the operator's namespace.
  # The gateway reads this directly from agentry-system; credentials
  # never leave that namespace.
  credentialsRef:
    name: anthropic-api-key
    key: api-key

  # Models offered through this provider. The gateway validates that requested
  # models are in this list; unknown models are rejected.
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

  # Rate limits enforced at the gateway (per namespace).
  rateLimits:
    requestsPerMinute: 300
    tokensPerMinute: 500000

  # Fallback chain. If this provider is unavailable or budget-blocked, the
  # gateway tries the next one in order. Referenced providers must also allow the namespace.
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
      state: "Normal"    # "Normal" | "Degraded" | "Blocked"
    - namespace: "team-ml"
      period: "2026-04"
      spentUSD: "412.00"
      percentUsed: 82
      state: "Degraded"
  clusterSpentUSD: "699.50"
```

### Design notes

- **Secret scoping**: credentials are referenced from the operator's namespace and read directly by the gateway in `agentry-system`. They never leave that namespace or reach agent containers.
- **Budget state**: persisted in status is the source of truth for display, but the gateway maintains a local authoritative counter that is synced to status periodically. This matters because status updates are rate-limited and lossy.
- **Glob in `allowedNamespaces`**: supports common patterns like `team-*`. Exact match is preferred where possible.
- **Fallback chains** are flat (not nested). Circular references are rejected by validation.
- **Cost fields are strings** (not floats) to avoid precision issues. The gateway parses them as decimals.

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
  # When omitted, $AGENTRY_PROVIDER_ENDPOINT is not injected.
  providers:
    - providerRef: { name: anthropic-shared }
      defaultModel: "claude-opus-4-6"

  # Resource overrides (must fit within AgentClass.resources.maxLimits).
  resources:
    requests: { cpu: "500m", memory: "1Gi" }
    limits:   { cpu: "1",    memory: "2Gi" }

  # Persistence: request a PVC mounted into the agent container.
  persistence:
    enabled: true
    sizeGi: 10
    mountPath: "/var/agent/memory"

  # Lifecycle: persistent means long-lived with hibernation support.
  mode: persistent
  lifecycle:
    idleTimeout: "30m"           # transition to Hibernating after this much idle time
    hibernationEnabled: true
    activitySource: providerTraffic   # "providerTraffic" | "agentHeartbeat" | "both"
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
  endpoint: "http://support-assistant.team-support.svc.cluster.local:8080"
  podName: "support-assistant-7d4b9f"
  pvcName: "support-assistant-memory"
  lastActivityTime: "2026-04-05T11:58:22Z"
  hibernatedAt: null
```

### Design notes

- **`mode` is on Agent, not a separate CRD.** There was a design discussion about splitting persistent and task into separate CRDs. The decision: AgentTask is separate (see below) because task lifecycle semantics differ significantly; but `mode` remains on Agent in case future modes (e.g., `mode: scheduled` for cron-style agents) are added without proliferating CRDs.
- **`providers` is optional**. Agents that do not call LLM providers (sub-agents, coding agents with IDE integration, pure message handlers) omit it entirely. When present, it is a flat list of provider references with a default model. All providers are routed through the single `$AGENTRY_PROVIDER_ENDPOINT`. The agent uses a qualified model name format (`{providerRef}/{modelId}`, e.g., `anthropic-shared/claude-opus-4-6`) in API calls to identify both the provider and model. The gateway resolves the provider, validates access, strips the prefix, and forwards the raw model name upstream. See the Gateway Design doc for the full routing chain.
- **`activitySource`**: agents may not always have meaningful LLM traffic (could be polling, waiting on webhooks). Supporting `agentHeartbeat` lets the agent explicitly signal liveness. See Controller Design for the heartbeat protocol.
- **`service` is always ClusterIP**. The Agentry User Gateway uses this Service to deliver channel messages. Developers who need external exposure create their own Ingress/HTTPRoute pointing at the Service.

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
      defaultModel: "claude-opus-4-6"

  resources:
    requests: { cpu: "1", memory: "2Gi" }
    limits:   { cpu: "2", memory: "4Gi" }

  # Scratch persistence for the task. Lifecycle is tied to the AgentTask.
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
    # What to do if timeout is hit before completion.
    onTimeout: Fail    # "Fail" | "Succeed" (rarely used) | "Retry" (v1.1+)
    # Retry on failure. v1 supports simple count-based retries, no backoff tuning.
    backoffLimit: 0

  # Artifacts to collect on completion. The agent includes values for these
  # names in the POST /v1/task/complete body.
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
  startTime: "2026-04-05T11:05:12Z"
  completionTime: "2026-04-05T11:30:42Z"
  podName: "fix-issue-342-xk9p2"
  # Artifact values captured into status (small) or referenced by ConfigMap name (large).
  artifactValues:
    pr-url: "https://github.com/acme/widgets/pull/587"
    summary: "Fixed null pointer in WidgetService.get(). Added regression test."
  # For large artifacts, a ConfigMap or ObjectRef is used instead:
  # artifactRefs:
  #   - name: build-log
  #     configMapRef: { name: fix-issue-342-build-log }
  agentReportedStatus: "success"
  agentReportedMessage: "PR opened successfully"
```

### Design notes

- **`completion.condition: agentReported` is the v1 default.** The agent container calls the gateway's completion endpoint with a status payload that may include artifact key-value pairs. This is more flexible than exit codes alone because the agent can report structured metadata and artifacts in a single call.
- **Artifact collection via completion payload**: artifacts are declared by name in the spec. The agent includes artifact values (keyed by name) in the `POST /v1/task/complete` body. The controller validates that all declared artifact names are present in the payload. This eliminates race conditions, removes the need for `pods/exec` RBAC, and keeps the artifact contract simple. Artifact size limits still apply (4 KiB per artifact, 32 KiB total inline; larger artifacts use ConfigMap references).
- **`onTimeout: Retry` and `completion.condition: webhook` are intentionally deferred.** v1 is simple: one attempt, report or exit, collect artifacts, done.
- **`ttlSecondsAfterFinished`** mirrors Job semantics. The controller garbage-collects the resource (and its Pod, PVC) after the TTL.
- **Concurrency**: unlike Job, AgentTask is always parallelism=1 in v1. Parallel fan-out tasks would be a separate future resource (`AgentTaskSet`) rather than a field on AgentTask.

---

## AgentChannel

AgentChannel is a namespace-scoped resource that connects a running Agent to a user-facing communication channel. It is how humans reach agents: via Discord, WhatsApp, iMessage, a generic webhook, or other platform adapters supported by the User Gateway.

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

  # Channel platform type. Built-in: "discord" | "whatsapp" | "webhook"
  type: discord

  # Credentials for the channel platform. Referenced Secret must exist
  # in the same namespace as the AgentChannel.
  credentialsRef:
    name: discord-bot-credentials
    key: bot-token

  # Platform-specific configuration.
  discord:
    # The guild (server) ID to listen in.
    guildId: "123456789012345678"
    # Optional: restrict to specific channels. Empty = all channels in the guild.
    allowedChannelIds:
      - "987654321098765432"

  # Optional: override how the gateway delivers messages to the agent.
  # By default the gateway posts to POST /v1/message on the agent's Service port.
  # Use this to accommodate agents with a custom endpoint.
  agentEndpoint:
    path: /v1/message          # default
    port: 8080                 # default ($AGENTRY_HEALTH_PORT)

  # Optional: session configuration. When enabled, the gateway tracks a
  # session ID per (channelId, userId) pair and includes it in the message
  # envelope so the agent can maintain conversation context.
  session:
    enabled: true
    ttl: "1h"    # session expires after this much inactivity
```

### Webhook variant

```yaml
apiVersion: agentry.io/v1alpha1
kind: AgentChannel
metadata:
  name: support-webhook
  namespace: team-support
spec:
  agentRef:
    name: support-assistant
  type: webhook
  webhook:
    # The gateway exposes this path externally (requires an Ingress pointing
    # at the gateway Service in agentry-system).
    path: /channels/support-assistant
    auth:
      type: bearer
      secretRef: { name: webhook-secret, key: token }
```

### Status

```yaml
status:
  observedGeneration: 1
  phase: Active     # Pending | Active | Degraded | Failed
  conditions:
    - type: Ready
      status: "True"
      reason: AgentReachable
    - type: PlatformConnected
      status: "True"
      reason: BotOnline
      message: "Discord bot connected, listening in guild 123456789012345678"
  messagesDelivered: 142
  lastMessageTime: "2026-04-05T11:58:22Z"
```

### Design notes

- **AgentChannel owns no Pod resources.** The gateway watches AgentChannel resources directly and manages platform connections based on their specs. The reconciler's role is validation and status reporting.
- **Credentials stay in the agent's namespace.** Unlike LLM provider credentials (which live in `agentry-system`), channel credentials (Discord bot tokens, etc.) are stored in the agent's namespace for organizational isolation. They are typically created by the platform team or a provisioning service, not by the developer. The gateway reads them via scoped RBAC and holds them in-process for the channel adapter.
- **One AgentChannel per (Agent, channel) pair.** An Agent may have multiple AgentChannels (e.g., both a Discord channel and a webhook). Each is a separate resource.
- **Session management is opt-in.** When `session.enabled: true`, the gateway tracks `(channelId, userId)` pairs and adds a `sessionId` to the message envelope. Agents use this to look up conversation context in their PVC. When disabled, each message is stateless from the gateway's perspective.
- **The agent's Service must be enabled** (`spec.service.enabled: true`) for AgentChannel to function — the gateway delivers messages via the ClusterIP Service.
- **AgentChannel references Agent only, not AgentTask.** Tasks are ephemeral and lack a stable Service endpoint. The `agentRef` field must point to an `Agent` resource.
- **Wake-on-demand integration**: if the target Agent is `Hibernated` when a channel message arrives, the gateway wakes it (via the activator mechanism) before delivering the message. The platform receives an appropriate "typing" or "processing" indicator while the agent resumes.

---

## Cross-Resource Validation

The following constraints are enforced at reconcile time. Failed validation results in a `Ready=False` status condition with a clear message rather than an admission rejection.

1. `Agent.spec.agentClassRef` and `AgentTask.spec.agentClassRef` must resolve to an existing AgentClass.
2. `Agent.spec.image` and `AgentTask.spec.image` must match at least one pattern in `AgentClass.spec.image.allowedImages` (if the list is non-empty).
3. Every `providerRef` in Agent/AgentTask must resolve to a ModelProvider (when `spec.providers` is present).
4. Every referenced ModelProvider must have the Agent's namespace in its `allowedNamespaces`.
5. Every referenced ModelProvider must appear in the AgentClass's `allowedProviders`.
6. Resource `limits` in Agent/AgentTask must not exceed `AgentClass.spec.resources.maxLimits`.
7. `persistence.sizeGi` must not exceed `AgentClass.spec.persistence.maxSizeGi`.
8. `lifecycle.idleTimeout` must not exceed `AgentClass.spec.lifecycle.maxIdleTimeout`.
9. `lifecycle.wakeTimeout` must not exceed `AgentClass.spec.lifecycle.maxWakeTimeout`.
10. `ModelProvider.spec.fallback` chains must not be circular.
11. `AgentChannel.spec.agentRef` must resolve to an existing Agent.
12. The referenced Agent must have `spec.service.enabled: true` for an AgentChannel to be valid.

Field-level schema validation uses CEL expressions (`x-kubernetes-validations`) embedded in the CRD OpenAPI schema. No admission webhook server is required.

## Defaulting

AgentClass defaults are applied at reconcile time when Agent/AgentTask fields are absent:

- `resources` defaults from `AgentClass.spec.resources.defaults`
- `persistence.sizeGi` defaults from `AgentClass.spec.persistence.defaultSizeGi`
- `image` defaults from `AgentClass.spec.image.defaultImage`
- `lifecycle.idleTimeout` defaults from `AgentClass.spec.lifecycle.defaultIdleTimeout`
- `lifecycle.wakeTimeout` defaults from `AgentClass.spec.lifecycle.defaultWakeTimeout`

Defaults are applied at reconcile time rather than admission. The stored spec reflects what the developer wrote; effective configuration is reflected in the Agent's status. This avoids a mutating webhook dependency while keeping the behavior predictable.

---

## Gateway Endpoints

The Agentry Gateway exposes several HTTP endpoints that agent containers may call. All requests are authenticated via **source IP → Pod resolution** — no API keys or tokens are exchanged. The gateway resolves the caller's Pod and namespace from its Pod informer cache.

### `POST /v1/task/complete` (AgentTask only)

Called by the agent container to report task completion. The gateway writes the payload to the Pod annotation `agentry.io/task-status`; the AgentTaskReconciler watches for this annotation to drive the Completing transition.

**Request body:**

```json
{
  "status": "success",
  "message": "PR opened successfully",
  "artifacts": {
    "pr-url": "https://github.com/acme/widgets/pull/587",
    "summary": "Fixed null pointer in WidgetService.get(). Added regression test."
  }
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `status` | string | yes | `"success"` or `"failure"` |
| `message` | string | no | Human-readable completion message |
| `artifacts` | map[string]string | no | Key-value pairs matching the names declared in `spec.artifacts`. The reconciler validates that all declared artifact names are present. |

**Response:** `200 OK` with empty body on success. `400 Bad Request` if the calling Pod is not associated with an AgentTask. `413 Payload Too Large` if any single artifact value exceeds 4 KiB or total artifacts exceed 32 KiB (large artifacts should be stored externally and referenced by URL in the value).

### `POST /v1/agent/heartbeat` (Agent only)

Called by the agent container to signal liveness for idle detection. Only meaningful when `spec.lifecycle.activitySource` is `agentHeartbeat` or `both`. The gateway writes the current timestamp to the Pod annotation `agentry.io/last-heartbeat`.

**Request body:** empty or `{}`.

**Response:** `200 OK` with empty body. `400 Bad Request` if the calling Pod is not associated with an Agent.

Heartbeat frequency is the agent's choice. A reasonable default is every 30-60 seconds. The gateway coalesces rapid heartbeats — annotation writes are debounced to at most once per 5 seconds to avoid API server pressure.

### Manual Wake Annotation

Agents in the `Hibernated` phase can be manually woken by applying the annotation:

```
kubectl annotate agent <name> agentry.io/wake=true
```

The AgentReconciler watches for this annotation. When observed on a `Hibernated` Agent, the reconciler transitions the Agent to `Resuming`, recreates the Pod, and removes the annotation after processing. This provides an escape hatch when no AgentChannel is configured or for operational use cases (e.g., pre-warming an agent before business hours).