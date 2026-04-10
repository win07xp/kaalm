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
        │  │  • $AGENTRY_PROVIDER_ENDPOINT → LLM Gateway (optional)  │    │
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

The controller exposes an internal ClusterIP Service (`agentry-controller.agentry-system.svc.cluster.local`, default port 9443) for the activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) and health/readiness probes. The gateway uses this Service to send wake requests when a channel message arrives for a hibernated agent.

Leader election is enabled so the operator can run with multiple replicas for availability.

## Data Plane

The data plane is what actually runs when an Agent is created. For each Agent in `Running` state, the controller provisions:

- **One Pod** containing the user's agent container. The Pod runs under the RuntimeClass specified by its AgentClass (runc, gVisor, or Kata).
- **One PVC** if the Agent spec requests persistence, mounted into the agent container at a configured path.
- **One Service** (ClusterIP) exposing the agent's HTTPS endpoint for intra-cluster traffic. The gateway uses this Service to deliver channel messages over TLS; direct external exposure remains the developer's responsibility.
- **One TLS Secret** containing a per-agent serving certificate and key, signed by the operator-managed CA. Mounted into the Pod for the agent's HTTPS listener. During CA rotation, the CA bundle (projected into Pods at `/var/run/agentry/ca.crt`) contains both old and new CAs to ensure zero-downtime certificate rollover.
- **One ConfigMap** holding non-sensitive agent configuration (gateway endpoint, feature flags).

There is no sidecar container. The **Agentry Gateway** in `agentry-system` handles all LLM traffic and inbound channel messages as a shared cluster-level service.

For AgentTask, the data plane is the same minus the Service (tasks do not typically receive channel messages) and with artifact data collected directly from the task completion payload posted by the agent to the gateway.

## The Agentry Gateway

The gateway is a replicated Deployment in `agentry-system` that serves two distinct roles on separate listeners:

**LLM Gateway** (outbound, agent → provider)
- Serves TLS on port 8443; agent containers connect via `$AGENTRY_PROVIDER_ENDPOINT` (HTTPS)
- Identifies the source namespace via source IP → Pod resolution from the Pod informer cache (unforgeable)
- Resolves the target ModelProvider from the qualified `provider/model` name in the request and the Agent's `spec.providers`
- Detects the request format from the URL path (`/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible)
- Validates the requested model and checks namespace access
- Enforces soft budget guardrails and per-namespace rate limits
- Routes to the upstream provider; on failure, walks the fallback chain (same-type providers only, up to `maxFallbackDepth` depth; no cross-format translation)
- Extracts actual token usage from the provider response and updates spend counters
- Returns structured error responses (JSON with `error.type`) on failure — see [LLM Gateway Error Responses](./API_ENDPOINTS.md#llm-gateway-error-responses)

**User Gateway** (inbound, channel → agent)
- Watches `AgentChannel` resources directly to determine message routing
- Listens for inbound webhook events on port 8080 (plaintext HTTP, behind Ingress with TLS termination)
- Normalizes webhook payloads into the standard Agentry message envelope
- Looks up the AgentChannel resource to find the target Agent and its endpoint
- If the agent is `Hibernated`, the gateway signals the controller to wake it via the authenticated activator endpoint and waits until the Pod is ready (bounded by `wakeTimeout`)
- Delivers the message to the agent container via `POST /v1/message`
- Supports both synchronous and asynchronous response modes per AgentChannel: sync (default) returns the agent's response as the webhook HTTP response; async returns 202 Accepted immediately and delivers the response via callback URL or polling endpoint
- v1 supports webhook channels only; Discord and WhatsApp adapters are planned for v1.1

LLM provider credentials are stored as Secrets in `agentry-system` and read directly by the gateway. They never leave `agentry-system` namespace.

## Agent Runtime Contract

Agentry is BYO-image, but containers must satisfy a minimal contract to participate in the lifecycle:

1. **HTTP health endpoint** on a known port (`$AGENTRY_HEALTH_PORT`, default 8080) returning 200 when ready.
2. **Graceful SIGTERM handling** — on receiving SIGTERM, the agent should finish in-flight work and exit within the configured `terminationGracePeriodSeconds`.
3. **LLM traffic via the gateway** (optional) — if the agent uses LLM providers, it reads `$AGENTRY_PROVIDER_ENDPOINT` (an HTTPS URL) and sends LLM requests there rather than calling providers directly. Two TLS requirements apply:
   - **Server verification**: the agent must trust the operator-managed CA certificate at `$AGENTRY_CA_CERT` (`/var/run/agentry/ca.crt`) to verify the gateway's TLS certificate.
   - **Client certificate (mTLS)**: the agent must present its operator-issued TLS certificate (`$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY`) as a client certificate on every request to `$AGENTRY_PROVIDER_ENDPOINT`. The gateway uses this certificate to cryptographically identify the agent and its namespace. Reference base images handle both requirements automatically. Custom images must configure their HTTP client to present the client cert. Agents that do not reference a ModelProvider do not receive these variables and do not need to satisfy this requirement.
4. **Message endpoint** (optional) — if the agent uses an AgentChannel, it exposes `POST /v1/message` on `$AGENTRY_HEALTH_PORT` over TLS, accepting the standard Agentry message envelope and returning a response envelope. The agent serves TLS using the operator-issued certificate at `$AGENTRY_TLS_CERT` (`/var/run/agentry/tls.crt`) and key at `$AGENTRY_TLS_KEY` (`/var/run/agentry/tls.key`). The reference base images handle TLS setup automatically. Agents without an AgentChannel do not need to implement this. Agents must also **watch the cert and key files for changes** (the kubelet automatically updates projected volume contents when the backing Secret is rotated — see [Lifecycle of an agent TLS serving certificate](./SECURITY.md#lifecycle-of-an-agent-tls-serving-certificate)) and reload their TLS configuration for new connections without dropping existing ones. Standard approaches: Go's `tls.Config.GetCertificate` callback (re-reads from disk on each new TLS handshake), Python's `SSLContext` reload on `inotify` event. Reference base images will handle this automatically.
5. **Optional activity signal** — for idle detection, the agent may emit activity heartbeats by calling `POST /v1/agent/heartbeat` on the gateway. The gateway tracks these timestamps in-memory (no etcd writes). Alternatively, the gateway infers activity from observed LLM and channel traffic.
6. **Optional completion signal** (AgentTask only) — the agent reports completion to the gateway via `POST /v1/task/complete` with a status payload that may include artifact key-value pairs.
7. **Message deduplication** (required when `hibernationEnabled: true`) — each message delivered via `POST /v1/message` carries a unique `messageId`. In sync mode, if a wake takes longer than the webhook caller's HTTP timeout, the caller receives 504 and commonly retries, which the gateway delivers as a new message (see [Activator](./GATEWAY_USER.md#activator)). Agents with `hibernationEnabled: true` MUST implement `messageId`-based deduplication — buffer received IDs (scoped to the session or a rolling time window) and return a cached response for duplicates without reprocessing. Reference base images will provide this automatically.

All agent↔gateway communication is over TLS. Agent→gateway traffic (LLM requests, heartbeats, task completion) is authenticated via source IP → Pod resolution. Gateway→agent traffic (channel message delivery) uses the agent's operator-issued TLS certificate, verified against the operator CA. No API keys or tokens are exchanged between agent containers and the gateway. Activity timestamps are maintained in-memory in the gateway — the controller queries them via an internal API endpoint rather than reading Pod annotations, avoiding per-request etcd writes at scale.

Agentry plans to ship reference base images (Python and Go variants) that implement this contract in a future release. These are not part of the v1 scope. Using them will be optional.

## Integration Points

### Agent Sandbox (optional backend)

An `AgentClass` may specify `spec.runtime.backend: agentSandbox`. When set, the Agent Reconciler creates `Sandbox` custom resources (from the SIG Apps Agent Sandbox project) instead of raw Pods. This gives Agentry agents access to Agent Sandbox's warm pools and enhanced isolation without reimplementing those features. When the backend is `pod` (default), the controller creates Pods directly.

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
- The Agentry Gateway Deployment with its RBAC, ServiceAccount, PodDisruptionBudget (`minAvailable: 1`), and `maxUnavailable: 1` rolling update strategy.
- A default set of AgentClasses (e.g., `standard`, `sandboxed`) that platform teams can customize or delete.
- Optional: a sample ModelProvider manifest stub (keys not included) as a starting template.

No cert-manager dependency — admission webhooks are not used.

The Helm chart supports a tiered on-ramp:
1. **Gateway only**: install the chart, configure a ModelProvider, and point existing workloads at the gateway for LLM traffic and spend tracking. No AgentClass or Agent resources required.
2. **Full agent lifecycle**: configure AgentClasses, deploy Agents and AgentTasks with hibernation and wake-on-demand, and connect them to user-facing channels via AgentChannels (webhook in v1). Channel integration is included in this tier because wake-on-demand requires a channel to be fully testable.
