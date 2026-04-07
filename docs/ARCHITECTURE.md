# Agentry — Architecture Overview

This document describes the high-level architecture of Agentry: the control plane, the data plane, and the integration points with the surrounding ecosystem. Implementation detail lives in the Controller Design, ModelProvider Gateway, and API Design docs.

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
        │   │     LLM Gateway         │  │     User Gateway           │    │
        │   │                         │  │                            │    │
        │   │  Request validation     │  │  Platform adapters         │    │
        │   │  Budget check           │  │  (Discord, WhatsApp,       │    │
        │   │  Rate limiting          │  │   webhook, ...)            │    │
        │   │  Fallback routing       │  │                            │    │
        │   │  Provider adapters      │  │  Message normalization     │    │
        │   │  Token counting         │  │  Agent delivery            │    │
        │   │  Spend reporting        │  │  Response translation      │    │
        │   └──────────┬──────────────┘  └───────────┬───────────────┘    │
        │              │                              │                    │
        │              ▼ (egress)                     ▲ (inbound)         │
        │       LLM Provider APIs              Channel Platforms           │
        │   (Anthropic, OpenAI, etc.)     (Discord, WhatsApp, etc.)       │
        └──────────────────────────────────────────────────────────────────┘
```

## Control Plane

The Agentry control plane consists of a single operator (Go, built on `controller-runtime`) running as a Deployment in a dedicated namespace (`agentry-system`). The operator hosts five reconcilers:

1. **Agent Reconciler** — watches `Agent` resources. Translates each Agent into a Pod, PVC, Service, and ConfigMap. Drives the persistent-agent state machine, including idle detection, hibernation, and wake-on-demand via the gateway activator.

2. **AgentTask Reconciler** — watches `AgentTask` resources. Creates a Pod to execute the task, monitors the completion condition (agent-reported via gateway or container exit code), collects artifacts from the task completion payload, and tears down resources on completion or timeout.

3. **ModelProvider Reconciler** — watches `ModelProvider` resources. Validates provider configuration, verifies the referenced Secret exists and is well-formed, maintains provider health status, and manages per-namespace spend state.

4. **AgentClass Reconciler** — watches `AgentClass` resources. Validates that referenced ModelProviders exist, maintains usage counts, and updates status conditions.

5. **AgentChannel Reconciler** — watches `AgentChannel` resources. Validates that the referenced Agent exists and is Running, provisions channel adapter configuration in the gateway, and monitors channel health.

The controller does **not** host admission webhooks. Field-level validation uses CEL expressions in CRD schemas. Cross-resource validation (reference resolution, image allowlists, provider access) is handled at reconcile time and surfaced as status conditions rather than admission errors.

The controller exposes an internal ClusterIP Service (`agentry-controller.agentry-system.svc.cluster.local`, default port 9443) for the activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) and health/readiness probes. The gateway uses this Service to send wake requests when a channel message arrives for a hibernated agent.

Leader election is enabled so the operator can run with multiple replicas for availability.

## Data Plane

The data plane is what actually runs when an Agent is created. For each Agent in `Running` state, the controller provisions:

- **One Pod** containing the user's agent container. The Pod runs under the RuntimeClass specified by its AgentClass (runc, gVisor, or Kata).
- **One PVC** if the Agent spec requests persistence, mounted into the agent container at a configured path.
- **One Service** (ClusterIP) exposing the agent's HTTP endpoint for intra-cluster traffic. The gateway uses this Service to deliver channel messages; direct external exposure remains the developer's responsibility.
- **One ConfigMap** holding non-sensitive agent configuration (gateway endpoint, role mappings, feature flags).

There is no sidecar container. The **Agentry Gateway** in `agentry-system` handles all LLM traffic and inbound channel messages as a shared cluster-level service.

For AgentTask, the data plane is the same minus the Service (tasks do not typically receive channel messages) and with artifact data collected directly from the task completion payload posted by the agent to the gateway.

## The Agentry Gateway

The gateway is a replicated Deployment in `agentry-system` that serves two distinct roles on separate listeners:

**LLM Gateway** (outbound, agent → provider)
- Receives LLM API calls from agent containers via `$AGENTRY_PROVIDER_ENDPOINT`
- Validates the requested model and checks namespace access
- Enforces soft budget guardrails and per-namespace rate limits
- Routes to the upstream provider, falling back through the chain on errors
- Extracts actual token usage from the provider response and updates spend counters

**User Gateway** (inbound, channel → agent)
- Watches `AgentChannel` resources directly to determine message routing
- Listens for inbound platform events (Discord webhook, WhatsApp message, etc.)
- Normalizes platform payloads into the standard Agentry message envelope
- Looks up the AgentChannel resource to find the target Agent and its endpoint
- If the agent is `Hibernated`, the gateway signals the controller to wake it via the activator endpoint and waits (sending a "typing" indicator to the platform) until the Pod is ready
- Delivers the message to the agent container via `POST /v1/message`
- Translates the agent's response back to the platform-native format and replies

LLM provider credentials are stored as Secrets in `agentry-system` and read directly by the gateway. They never leave `agentry-system` namespace.

## Agent Runtime Contract

Agentry is BYO-image, but containers must satisfy a minimal contract to participate in the lifecycle:

1. **HTTP health endpoint** on a known port (`$AGENTRY_HEALTH_PORT`, default 8080) returning 200 when ready.
2. **Graceful SIGTERM handling** — on receiving SIGTERM, the agent should finish in-flight work and exit within the configured `terminationGracePeriodSeconds`.
3. **LLM traffic via the gateway** (optional) — if the agent uses LLM providers, it reads `$AGENTRY_PROVIDER_ENDPOINT` and sends LLM requests there rather than calling providers directly. This is how spend tracking and fallback work. Agents that do not reference a ModelProvider do not receive this variable.
4. **Message endpoint** (optional) — if the agent uses an AgentChannel, it exposes `POST /v1/message` on `$AGENTRY_HEALTH_PORT` accepting the standard Agentry message envelope and returning a response envelope. Agents without an AgentChannel do not need to implement this.
5. **Optional activity signal** — for idle detection, the agent may emit activity heartbeats by calling `POST /v1/agent/heartbeat` on the gateway. Alternatively, the gateway infers activity from observed LLM traffic.
6. **Optional completion signal** (AgentTask only) — the agent reports completion to the gateway via `POST /v1/task/complete` with a status payload that may include artifact key-value pairs.

Agentry will ship a reference base image (Python and Go variants) that implements this contract. Using it is optional.

## Integration Points

### Agent Sandbox (optional backend)

An `AgentClass` may specify `spec.runtime.backend: agentSandbox`. When set, the Agent Reconciler creates `Sandbox` custom resources (from the SIG Apps Agent Sandbox project) instead of raw Pods. This gives Agentry agents access to Agent Sandbox's warm pools and enhanced isolation without reimplementing those features. When the backend is `pod` (default), the controller creates Pods directly.

### MCP (Model Context Protocol)

Agentry does not mandate MCP but is compatible with it. Agent containers are free to connect to MCP servers for tool access. AgentClass may declare a list of `allowedMCPServers` that the agent container can reach via NetworkPolicy constraints. MCP server provisioning itself is out of scope for v1.

### LLM Providers

Agentry supports any HTTP-based LLM provider. Out of the box, the gateway understands Anthropic, OpenAI, Google Vertex, and OpenAI-compatible endpoints (including Ollama, vLLM, LiteLLM gateways). Adding a new provider type is a plugin-style extension in the gateway.

### Channel Platforms

The User Gateway ships with adapters for Discord, WhatsApp, and a generic webhook. Additional platform adapters follow the same plugin pattern as LLM provider adapters.

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
- The Agentry Gateway Deployment with its RBAC and ServiceAccount.
- A default set of AgentClasses (e.g., `standard`, `sandboxed`) that platform teams can customize or delete.
- Optional: a sample ModelProvider manifest stub (keys not included) as a starting template.

No cert-manager dependency — admission webhooks are not used.

The Helm chart supports a tiered on-ramp:
1. **Gateway only**: install the chart, configure a ModelProvider, and point existing workloads at the gateway for LLM traffic and spend tracking.
2. **Add agent lifecycle**: configure AgentClasses, deploy Agents with hibernation and wake-on-demand.
3. **Add channel integration**: configure AgentChannels to connect agents to Discord, WhatsApp, or webhooks.
