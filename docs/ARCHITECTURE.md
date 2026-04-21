# Agentry — Architecture Overview

This document describes the high-level architecture of Agentry: the control plane, the data plane, and the integration points with the surrounding ecosystem.

## Documentation Map

| Document | Contents |
|---|---|
| [VISION.md](./VISION.md) | Problem statement, design principles, v1 scope |
| [STORIES.md](./STORIES.md) | Personas and user scenarios driving the design |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | This file — system topology, control/data plane, deployment |
| [API_RESOURCES.md](./API_RESOURCES.md) | CRD specs: AgentClass, ModelProvider, Agent, AgentTask, AgentChannel |
| [API_ENDPOINTS.md](./API_ENDPOINTS.md) | Gateway HTTP endpoints and agent-implemented contracts |
| [GATEWAY_LLM.md](./GATEWAY_LLM.md) | LLM Gateway: routing, budget, fallback, TLS, credentials |
| [GATEWAY_USER.md](./GATEWAY_USER.md) | User Gateway: webhook delivery, activator, activity tracking |
| [CONTROLLER_RECONCILERS.md](./CONTROLLER_RECONCILERS.md) | Operator structure, five reconcilers, error handling |
| [CONTROLLER_LIFECYCLE.md](./CONTROLLER_LIFECYCLE.md) | State machines for Agent and AgentTask, finalizers |
| [SECURITY.md](./SECURITY.md) | Trust model, RBAC, credential lifecycle, TLS, isolation |
| [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md) | Go and Python starter templates implementing the runtime contract |

## System Topology

```
                          ┌─────────────────────────────────────────┐
                          │              Kubernetes API             │
                          └─────────────────────────────────────────┘
                                           ▲
                                           │ watches / reconciles
                                           │
        ┌──────────────────────────────────┴──────────────────────────────────┐
        │                      Agentry Controller                             │
        │  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐   │
        │  │ Agent Reconciler │  │ AgentTask Reconc │  │ Provider Reconc  │   │
        │  └──────────────────┘  └──────────────────┘  └──────────────────┘   │
        │  ┌──────────────────┐  ┌──────────────────┐                         │
        │  │AgentClass Reconc │  │AgentChannel Reconc                         │
        │  └──────────────────┘  └──────────────────┘                         │
        └───────────────────────────────┬─────────────────────────────────────┘
                                        │ creates/updates
                                        ▼
        ┌──────────────────────────────────────────────────────────────────┐
        │                    Kubernetes Primitives                         │
        │         Pods / PVCs / Services / Secrets / ConfigMaps            │
        └──────────────────────────────────────────────────────────────────┘
                                        │
                                        ▼
        ┌──────────────────────────────────────────────────────────────────┐
        │                       Agent Pod                                  │
        │  ┌──────────────────────────────────────────────────────────┐    │
        │  │                  Agent Container (user image)            │    │
        │  │                                                          │    │
        │  │  • $AGENTRY_GATEWAY_ENDPOINT  → Gateway (always)        │    │
        │  │  • POST /v1/message           → receives channel msgs    │    │
        │  │  • GET  /health               → liveness / readiness     │    │
        │  └──────────────────────────────────────────────────────────┘    │
        └──────────────────────────────────────────────────────────────────┘
                │ calls (LLM traffic)              ▲ delivers (channel msgs)
                ▼                                  │
        ┌──────────────────────────────────────────────────────────────────┐
        │              Agentry Gateway (agentry-system namespace)          │
        │                                                                  │
        │   ┌─────────────────────────┐  ┌───────────────────────────┐    │
        │   │   LLM Gateway (TLS)    │  │     User Gateway           │    │
        │   │                         │  │                            │    │
        │   │  Request validation     │  │  Webhook adapter           │    │
        │   │  Budget check           │  │  (v1: webhook only;        │    │
        │   │  Rate limiting          │  │   Discord, WhatsApp v1.1)  │    │
        │   │  Fallback routing       │  │                            │    │
        │   │  Provider adapters      │  │  Message normalization     │    │
        │   │  Token counting         │  │  Agent delivery            │    │
        │   │  Spend reporting        │  │  Response translation      │    │
        │   └──────────┬──────────────┘  └───────────┬───────────────┘    │
        │              │                              │                    │
        │              ▼ (egress)                     ▲ (inbound)         │
        │       LLM Provider APIs              Webhook Callers              │
        │   (Anthropic, OpenAI, etc.)     (external systems, bots, etc.)  │
        └──────────────────────────────────────────────────────────────────┘
```

## Control Plane

The Agentry control plane consists of a single operator (Go, built on `controller-runtime`) running as a Deployment in a dedicated namespace (`agentry-system`). The operator hosts five reconcilers:

1. **Agent Reconciler** — watches `Agent` resources. Translates each Agent into a Pod, PVC, Service, and ConfigMap. Drives the persistent-agent state machine, including idle detection, hibernation, and wake-on-demand via the gateway activator.

2. **AgentTask Reconciler** — watches `AgentTask` resources. Creates a Pod to execute the task, monitors the completion condition (agent-reported via gateway or container exit code), collects artifacts from the task completion payload, and tears down resources on completion or timeout.

3. **ModelProvider Reconciler** — watches `ModelProvider` resources. Validates provider configuration, verifies the referenced Secret exists and is well-formed, maintains provider health status, and manages per-namespace spend state.

4. **AgentClass Reconciler** — watches `AgentClass` resources. Validates that referenced ModelProviders exist, maintains usage counts, and updates status conditions.

5. **AgentChannel Reconciler** — watches `AgentChannel` resources. Validates that the referenced Agent exists and has a Service, validates channel credentials, and monitors channel health. The gateway watches AgentChannel resources directly for platform connection management.

The controller does **not** host admission webhooks. Field-level validation uses CEL expressions in CRD schemas. Cross-resource validation (reference resolution, image allowlists, provider access) is handled at reconcile time and surfaced as status conditions rather than admission errors.

The controller exposes an internal ClusterIP Service (`agentry-controller.agentry-system.svc.cluster.local`, default port 9443) for the activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) and health/readiness probes. The activator endpoint serves HTTPS using a certificate issued by the Agentry cert-manager `ClusterIssuer` (see [Deployment Model](#deployment-model)); the gateway trusts the Agentry CA to verify it. The activator handler is served on **every** controller replica, not only the leader: the handler patches `agentry.io/wake=true` on the target Agent, and the leader's existing Agent watch fires the manual-wake path in the reconciler. This keeps the Service round-robin behavior correct without any leader-aware endpoint plumbing. The gateway uses this Service to send wake requests when a channel message arrives for a hibernated agent.

Leader election is enabled so the operator can run with multiple replicas for availability.

## Data Plane

The data plane is what actually runs when an Agent is created. For each Agent in `Running` state, the controller provisions:

- **One Pod** containing the user's agent container. The Pod runs under the RuntimeClass specified by its AgentClass (runc, gVisor, or Kata).
- **One PVC** if the Agent spec requests persistence, mounted into the agent container at a configured path.
- **One Service** (ClusterIP) exposing the agent's HTTPS endpoint for intra-cluster traffic. The gateway uses this Service to deliver channel messages over TLS; direct external exposure remains the developer's responsibility.
- **One cert-manager `Certificate`** (and the Secret it writes) containing a per-agent serving certificate and key, signed by the Agentry CA via the `agentry-system` `Issuer`. Mounted into the Pod for the agent's HTTPS listener. cert-manager handles rotation continuously; the Agentry CA certificate is projected into Pods at `/var/run/agentry/ca.crt` and kubelet refreshes the volume on Secret change.
- **One ConfigMap** holding non-sensitive agent configuration (gateway endpoint, feature flags).

There is no sidecar container. The **Agentry Gateway** in `agentry-system` handles all LLM traffic and inbound channel messages as a shared cluster-level service.

For AgentTask, the data plane is the same minus the Service (tasks do not typically receive channel messages) and with artifact data collected directly from the task completion payload posted by the agent to the gateway.

## The Agentry Gateway

The gateway is a replicated Deployment in `agentry-system` that serves two distinct roles on separate listeners:

**LLM Gateway** (outbound, agent → provider)
- Serves TLS on port 8443; agent containers connect via `$AGENTRY_GATEWAY_ENDPOINT` (HTTPS, always injected)
- Identifies the source namespace via mTLS client-cert SAN (Agentry-managed Agent/AgentTask Pods) or `TokenReview`-validated ServiceAccount bearer token (gateway-only tier), with a source-IP → Pod cross-check from the informer cache as defense in depth — see [Namespace Identification](./GATEWAY_LLM.md#namespace-identification)
- Resolves the target ModelProvider from the qualified `provider/model` name in the request and the Agent's `spec.providers`
- Detects the request format from the URL path (`/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible)
- Validates the requested model and checks namespace access
- Enforces soft budget guardrails and per-namespace rate limits
- Routes to the upstream provider; on failure, walks the fallback chain (same-type providers only, up to `maxFallbackDepth` depth; no cross-format translation)
- Extracts actual token usage from the provider response and updates spend counters
- Returns structured error responses (JSON with `error.type`) on failure — see [LLM Gateway Error Responses](./API_ENDPOINTS.md#llm-gateway-error-responses)

**User Gateway** (inbound, channel → agent)
- Watches `AgentChannel` resources directly to determine message routing
- Listens for inbound webhook events on port 8080 over TLS (serves `agentry-gateway-tls`; Ingress is configured for backend re-encrypt or TLS pass-through)
- Normalizes webhook payloads into the standard Agentry message envelope
- Looks up the AgentChannel resource to find the target Agent and its endpoint
- If the agent is `Hibernated`, the gateway signals the controller to wake it via the authenticated activator endpoint and waits until the Pod is ready (bounded by `wakeTimeout`)
- Delivers the message to the agent container via `POST /v1/message`
- Supports both synchronous and asynchronous response modes per AgentChannel: sync (default) returns the agent's response as the webhook HTTP response; async returns 202 Accepted immediately and delivers the response via callback URL or polling endpoint
- v1 supports webhook channels only; Discord and WhatsApp adapters are planned for v1.1

LLM provider credentials are stored as Secrets in `agentry-system` and read directly by the gateway. They never leave `agentry-system` namespace.

## Agent Runtime Contract

Agentry is BYO-image, but containers must satisfy a minimal contract to participate in the lifecycle:

1. **HTTPS health endpoint** on a known port (`$AGENTRY_HEALTH_PORT`, default 8080) returning 200 when ready. The agent serves TLS on this port using the cert-manager-issued per-Agent certificate (`$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY`), so liveness and readiness probes must be configured with `httpGet.scheme: HTTPS`. Kubernetes does not verify TLS certificates for httpGet probes, so the cert works without any additional CA configuration on the probe.
2. **Graceful SIGTERM handling** — on receiving SIGTERM, the agent should finish in-flight work and exit within the configured `terminationGracePeriodSeconds`.
3. **Gateway communication** — the controller injects `$AGENTRY_GATEWAY_ENDPOINT`: an HTTPS URL pointing to the gateway's LLM listener (port 8443). This is the base URL for **all** agent→gateway calls: LLM requests, heartbeats (`POST /v1/agent/heartbeat`), and task completion (`POST /v1/task/complete`). Always injected regardless of whether `spec.providers` is set, so provider-less agents can still reach the gateway for heartbeats and task completion.

   Two TLS requirements apply to all calls to `$AGENTRY_GATEWAY_ENDPOINT`:
   - **Server verification**: the agent must trust the Agentry CA certificate at `$AGENTRY_CA_CERT` (`/var/run/agentry/ca.crt`) to verify the gateway's TLS certificate. The CA is managed by cert-manager (see [Deployment Model](#deployment-model)).
   - **Client authentication**: one of two modes, depending on how the workload was provisioned:
     - **mTLS** (Agentry-managed Pods) — the AgentReconciler (for Agents) and the AgentTaskReconciler (for AgentTasks) create a cert-manager `Certificate` for the Pod; the cert and key are mounted at `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY`. The agent must present this client certificate on every request to the gateway. The gateway extracts the agent's namespace from the SAN — `{name}.{namespace}.svc.cluster.local` for Agents, `{name}.{namespace}.task.agentry.io` for AgentTasks (the latter avoids implying a Service the task does not have). This is the only mode accepted for Pods managed by Agentry — see [Namespace Identification](./GATEWAY_LLM.md#namespace-identification).
     - **ServiceAccount bearer token** (gateway-only tier) — for workloads that are not managed by an Agent resource, the HTTP client presents a projected ServiceAccount token in the `Authorization: Bearer <jwt>` header. The gateway validates the token via the Kubernetes `TokenReview` API and extracts the namespace from the validated `status.user.username`. No client cert is required.
     The starter templates (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)) handle both modes. Custom images must configure their HTTP client for the appropriate mode.
4. **Message endpoint** (optional) — if the agent uses an AgentChannel, it exposes `POST /v1/message` on `$AGENTRY_HEALTH_PORT` over TLS, accepting the standard Agentry message envelope and returning a response envelope. The agent serves TLS using the cert-manager-issued certificate at `$AGENTRY_TLS_CERT` (`/var/run/agentry/tls.crt`) and key at `$AGENTRY_TLS_KEY` (`/var/run/agentry/tls.key`). Agents without an AgentChannel do not need to implement this. Agents must also **watch the cert and key files for changes** (the kubelet automatically updates projected volume contents when the backing Secret is rotated — see [Lifecycle of an agent TLS serving certificate](./SECURITY.md#lifecycle-of-an-agent-tls-serving-certificate)) and reload their TLS configuration for new connections without dropping existing ones. Standard approaches: Go's `tls.Config.GetCertificate` callback (re-reads from disk on each new TLS handshake), Python's `SSLContext` reload on `inotify` event. The starter templates implement this reload pattern — see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md).
5. **Optional activity signal** — for idle detection, the agent may emit activity heartbeats by calling `POST /v1/agent/heartbeat` on the gateway. The gateway tracks these timestamps in-memory (no etcd writes). Alternatively, the gateway infers activity from observed LLM and channel traffic.
6. **Optional completion signal** (AgentTask only) — the agent reports completion to the gateway via `POST /v1/task/complete` with a status payload that may include artifact key-value pairs.
7. **Message deduplication** (required when `hibernationEnabled: true`) — each message delivered via `POST /v1/message` carries a unique `messageId`. In sync mode, if a wake takes longer than the webhook caller's HTTP timeout, the caller receives 504 and commonly retries, which the gateway delivers as a new message (see [Activator](./GATEWAY_USER.md#activator)). Agents with `hibernationEnabled: true` MUST implement `messageId`-based deduplication — buffer received IDs (scoped to the session or a rolling time window) and return a cached response for duplicates without reprocessing. The starter templates (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)) implement this as an in-memory LRU over the last 1024 `messageId`s; adopters can layer PVC-backed persistence on top if stronger-than-process-lifetime dedup is needed.

All agent↔gateway communication is over TLS. Agent→gateway traffic (LLM requests, heartbeats, task completion) is authenticated via mTLS for Agentry-managed Pods or via a `TokenReview`-validated ServiceAccount bearer token for gateway-only-tier workloads, with source-IP → Pod cross-check in both modes (see [GATEWAY_LLM.md § Namespace Identification](./GATEWAY_LLM.md#namespace-identification)). Gateway→agent traffic (channel message delivery) uses the agent's cert-manager-issued TLS certificate, verified against the Agentry CA (`agentry-ca`). Activity timestamps are maintained in-memory in the gateway — the controller queries them via an internal API endpoint rather than reading Pod annotations, avoiding per-request etcd writes at scale.

Agentry ships **starter templates** (one Go, one Python) under `examples/starter-go/` and `examples/starter-python/` as part of v1. Each template implements the full runtime contract end-to-end: HTTPS serving on `$AGENTRY_HEALTH_PORT`, mTLS client certificate presentation on gateway calls (with a `TokenReview` token-auth path available for gateway-only tenants), cert-file watch and reload, `/v1/message` handler skeleton, and `messageId`-based deduplication. Adopters copy the template and replace the agent logic — the boilerplate stays. See [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md). Full-featured reference base images (published container images that embed and wrap the contract) are planned for a future release.

## Integration Points

### Agent Sandbox (optional backend — v1.1)

An `AgentClass` will be able to specify `spec.runtime.backend: agentSandbox` (v1.1). When set, the Agent Reconciler will create `Sandbox` custom resources (from the SIG Apps Agent Sandbox project) instead of raw Pods. This will give Agentry agents access to Agent Sandbox's warm pools and enhanced isolation without reimplementing those features. In v1, the only supported backend is `pod` — the CRD schema rejects `agentSandbox` at apply time.

### MCP (Model Context Protocol)

Agentry does not mandate MCP but is compatible with it. Agent containers are free to connect to MCP servers for tool access. AgentClass may declare a list of `allowedMCPServers` that the agent container can reach via NetworkPolicy constraints. MCP server provisioning itself is out of scope for v1.

### LLM Providers

Agentry supports any HTTP-based LLM provider. Out of the box, the gateway understands Anthropic, OpenAI, Google Vertex, and OpenAI-compatible endpoints (including Ollama, vLLM, LiteLLM gateways). Adding a new provider type is a plugin-style extension in the gateway.

### Channel Platforms

The User Gateway ships with a **generic webhook adapter** in v1 (inbound HTTP POST with configurable auth). Discord and WhatsApp adapters are planned for v1.1 — they require persistent connections and platform-specific reconnection logic. Additional platform adapters follow the same plugin pattern as LLM provider adapters.

## Scoping Summary

| Concern | Where it lives |
|---|---|
| Policy (who can use what, at what cost) | AgentClass, ModelProvider (cluster-scoped) |
| Workload definition | Agent, AgentTask (namespace-scoped) |
| Channel integration | AgentChannel (namespace-scoped) |
| Lifecycle orchestration | Agentry Controller (cluster-level) |
| Runtime isolation | RuntimeClass via AgentClass, or Sandbox backend |
| LLM traffic / spend tracking | LLM Gateway in agentry-system (shared) |
| Channel message routing | User Gateway in agentry-system (shared) |
| Tool access | MCP (external, not managed by Agentry in v1) |
| External exposure | Ingress/Gateway (user-managed, not Agentry) |

## Deployment Model

Agentry ships as a Helm chart that installs:

- The CRDs (AgentClass, ModelProvider, Agent, AgentTask, AgentChannel).
- The operator Deployment with RBAC, ServiceAccount, and leader election.
- The Agentry Gateway Deployment with its RBAC, ServiceAccount, PodDisruptionBudget (`minAvailable: 1`), and `maxUnavailable: 1` rolling update strategy. The gateway's operational settings are exposed as Helm values — notably `gateway.maxFallbackDepth` (default: `3`), which sets the `AGENTRY_MAX_FALLBACK_DEPTH` environment variable on the gateway Deployment and controls the maximum fallback chain depth for LLM provider routing (see [Fallback Logic](./GATEWAY_LLM.md#fallback-logic)).
- A default set of AgentClasses (e.g., `standard`, `sandboxed`) that platform teams can customize or delete.
- Optional: a sample ModelProvider manifest stub (keys not included) as a starting template.

**cert-manager and trust-manager are required dependencies.** The chart does not install the cert-manager or trust-manager controllers themselves (teams with an existing cert-manager deployment reuse them); it ships the `ClusterIssuer`, `Certificate`, and `Bundle` resources Agentry needs:
- A self-signed `ClusterIssuer` (`agentry-selfsigned`) creates a `Certificate` for the Agentry CA.
- A `ClusterIssuer` (`agentry-ca-issuer`) sourcing from the `agentry-ca` Secret in `agentry-system` signs all Agentry-issued leaf certs using the CA above. A `ClusterIssuer` is used (rather than a namespace-scoped `Issuer`) because per-Agent/AgentTask `Certificate` resources live in user namespaces and reference the same signing key; cert-manager's `issuerRef` cannot span namespaces to a namespaced `Issuer`.
- A `Certificate` for the gateway serving cert (`agentry-gateway-tls`) used by both listeners — the LLM listener on port 8443 and the User listener on port 8080 — both serving TLS from the same cert. External webhook traffic arrives via Ingress configured for backend re-encrypt (or TLS pass-through); there is no plaintext listener on the gateway.
- A `Certificate` for the controller's activator endpoint (`agentry-controller-tls`).
- One `Certificate` per Agent, created by the AgentReconciler at provisioning time and owned by the Agent resource via ownerRef.
- One `Certificate` per AgentTask, created by the AgentTaskReconciler at provisioning time and owned by the AgentTask resource via ownerRef.
- A `trust-manager` `Bundle` resource that projects the Agentry CA as a ConfigMap into every namespace that hosts an Agent or AgentTask; agent Pods mount it at `/var/run/agentry/ca.crt` to verify the gateway's TLS cert.

Admission webhooks are not used; the cert-manager dependency is solely for TLS lifecycle management.

The Helm chart supports a tiered on-ramp:
1. **Gateway only**: install the chart, configure a ModelProvider, and point existing workloads at the gateway for LLM traffic and spend tracking. No AgentClass or Agent resources required. Existing workloads authenticate to the gateway using their own projected ServiceAccount tokens — no client certificate is required in this tier. See [Namespace Identification](./GATEWAY_LLM.md#namespace-identification). Access control in this tier is governed by `ModelProvider.spec.allowedNamespaces` plus `spec.models` only — AgentClass `allowedProviders` does not apply because there is no Agent resource to reconcile against. Platform teams who need class-scoped provider policy must use the full lifecycle tier.
2. **Full agent lifecycle**: configure AgentClasses, deploy Agents and AgentTasks with hibernation and wake-on-demand, and connect them to user-facing channels via AgentChannels (webhook in v1). Channel integration is included in this tier because wake-on-demand requires a channel to be fully testable. Agentry-managed Pods authenticate via mTLS using per-agent certificates issued by cert-manager. The LLM Gateway enforces the full routing chain (Agent → AgentClass `allowedProviders` → ModelProvider `allowedNamespaces`/`models`) for this tier.
