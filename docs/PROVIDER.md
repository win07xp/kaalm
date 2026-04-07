# Agentry — Gateway Design

This document covers the Agentry Gateway: the shared cluster-level component responsible for mediating LLM traffic between agent containers and upstream providers (LLM Gateway) and delivering channel messages from user-facing platforms to agent containers (User Gateway). It is where spend tracking, budget guardrails, rate limiting, fallback, and channel protocol translation live.

## Why a Shared Gateway

Agent containers need to call LLM providers. Doing this naively (agents holding API keys and calling providers directly) gives up all centralized control: no spend visibility, no fallback, no per-namespace accounting, and every agent image must embed credentials. Agentry interposes on LLM traffic to deliver ModelProvider guarantees.

Similarly, agents need to be reachable from user-facing platforms (Discord, WhatsApp, webhooks). Rather than requiring each developer to build their own webhook receiver and protocol adapter, Agentry provides a shared channel ingress point.

### Architecture Option Analysis

Three architectural options were evaluated for the LLM proxy component:

**Option A: Per-Agent Sidecar Proxy**
A small proxy container runs as a sidecar in every Agent Pod.

Pros: Small failure domain; no shared state contention at request time.

Cons: Kubernetes `NetworkPolicy` cannot enforce per-container rules within a Pod — the sidecar and agent container share the same network namespace and the same IP, so NetworkPolicy cannot prevent the agent container from making direct egress calls to LLM providers if the node allows it. Credentials must be copied into user namespaces. Budget state requires eventual-consistent replication across all sidecars.

**Option B: Namespace-Scoped Gateway (not selected)**
One proxy Deployment per namespace.

Cons: More complex operator (gateway lifecycle per namespace), harder to reason about at scale, still requires per-namespace credential propagation.

**Option C: Cluster-Wide Gateway (SELECTED for v1)**
One replicated proxy Deployment in `agentry-system`.

Pros: Credentials never leave `agentry-system`. NetworkPolicy cleanly isolates agent Pods (deny all egress to LLM provider IPs; allow egress to the gateway Service — this is cross-Pod and fully enforceable). Budget state is centralized in one process with no replication lag. The gateway also serves as the activator for hibernated agents. SPOF concern is addressed with 2-3 replicas and a PodDisruptionBudget.

**v1 ships with Option C.** The per-Pod sidecar pattern was rejected because the same-Pod network namespace sharing undermines the credential isolation guarantee on standard Kubernetes clusters without a service mesh.

---

## Gateway Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│                    Agentry Gateway (agentry-system)                  │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐     │
│  │                    LLM Gateway Listener                     │     │
│  │                                                             │     │
│  │  Request validator ──▶ Model allow-list check               │     │
│  │                  ──▶ Namespace access check                 │     │
│  │                  ──▶ Budget check + policy enforcement      │     │
│  │                  ──▶ Rate limiter                           │     │
│  │                  ──▶ Upstream router (with fallback)        │     │
│  │                  ──▶ Provider adapter                       │     │
│  │                  ◀── Response relay                         │     │
│  │                  ──▶ Token counter (post-call)              │     │
│  │                  ──▶ Spend state update                     │     │
│  └─────────────────────────────────────────────────────────────┘     │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐     │
│  │                   User Gateway Listener                     │     │
│  │                                                             │     │
│  │  Platform adapter ──▶ Authenticates platform event          │     │
│  │                  ──▶ Normalizes to Agentry message envelope │     │
│  │                  ──▶ Looks up AgentChannel → Agent Service  │     │
│  │                  ──▶ Activator check (wake if hibernated)   │     │
│  │                  ──▶ POST /v1/message to agent Pod          │     │
│  │                  ◀── Agent response envelope                │     │
│  │                  ──▶ Translates to platform-native reply    │     │
│  └─────────────────────────────────────────────────────────────┘     │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐     │
│  │                  Activator / Heartbeat                      │     │
│  │  Monitors Agent hibernation state; signals controller to    │     │
│  │  wake agents on incoming traffic; tracks agent heartbeats.  │     │
│  └─────────────────────────────────────────────────────────────┘     │
└──────────────────────────────────────────────────────────────────────┘
         │ (egress)                        ▲ (inbound)
         ▼                                 │
  LLM Provider APIs               Channel Platforms
  (Anthropic, OpenAI,           (Discord, WhatsApp,
   Vertex, etc.)                 webhooks, etc.)
```

---

## LLM Gateway — Request Flow

1. **Agent sends request**: the agent container makes an HTTP request to `$AGENTRY_PROVIDER_ENDPOINT` (resolves to the gateway Service in `agentry-system`). The request format is provider-native (the agent uses Anthropic's or OpenAI's API shape).
2. **Gateway validates**: confirms the requested model is listed in the target ModelProvider's `models` and the namespace is in `allowedNamespaces`.
3. **Budget check**: the gateway reads the current budget state for the agent's namespace. If a `degrade` policy applies, it rewrites the model name in the request. If `block` applies, it returns an error to the agent.
4. **Rate limit check**: per-namespace token-bucket rate limiter on requests/min and tokens/min.
5. **Route to upstream**: the gateway adds the provider API key (read directly from Secrets in `agentry-system`) and forwards the request. If the upstream fails (connection error, 5xx, timeout), the gateway tries the next provider in `spec.fallback`.
6. **Response returned**: the gateway relays the response to the agent container.
7. **Token counting**: the gateway extracts actual token usage from the provider response (`usage.input_tokens` / `usage.output_tokens` for Anthropic; `usage.prompt_tokens` / `usage.completion_tokens` for OpenAI, etc.). Actual usage from the provider response is always preferred over pre-call estimation.
8. **Spend update**: the gateway updates the in-process spend counter for the namespace.

---

## Namespace Identification

The gateway must identify the source namespace of each LLM request to enforce access controls and per-namespace budget tracking. It does this via **source IP resolution**:

1. The gateway maintains a Pod informer cache (it already watches Pods cluster-wide for annotation writes).
2. On each inbound LLM request, the gateway resolves the source IP to a Pod via the informer cache, then reads the Pod's namespace.
3. If the source IP does not resolve to a known Pod, the request is rejected.

This approach is **unforgeable**: source IPs are assigned by the cluster network (CNI) and cannot be overridden from within a container. An agent cannot claim to be from a different namespace. No agent-provided headers or tokens are trusted for namespace identification.

---

## Budget State Management

Budget counters are maintained **in-process in the gateway**. Because the gateway is the single choke point for all LLM traffic, there is no need for a separate aggregator or distributed counter.

Each gateway replica maintains an in-memory spend counter per (provider, namespace, period) tuple. On startup, each replica reads the current period's spent value from a ConfigMap managed by the ModelProviderReconciler. On each LLM call, the counter is updated synchronously.

**Multi-replica consistency**: In a replicated gateway deployment, each replica has its own counter. Spend is reconciled across replicas by the controller periodically (every 30s) — the controller sums the per-replica partial counters and writes the canonical total back to ModelProvider status. Each replica then refreshes from the canonical total.

This means budget enforcement is **approximate under high concurrency** — replicas can collectively overspend before the next reconcile cycle. The overspend is bounded: `number_of_replicas × cost_per_call × reconcile_interval`. For typical deployments (2-3 replicas, $0.01-0.10 per call, 30s interval), this is acceptable.

**Agentry's budget feature is spend visibility and soft guardrails, not a hard financial cap.** This is an explicit design decision, not a limitation to be fixed. Teams requiring hard caps should use provider-level account limits in addition to Agentry's per-namespace guardrails.

Budget periods roll over at midnight UTC. The controller archives the previous period's total to ModelProvider status for auditability.

---

## Credential Handling

Provider credentials are stored as Secrets in `agentry-system` and referenced by ModelProvider. The gateway reads these Secrets directly at startup and watches them for rotation. Credentials never leave `agentry-system` — there is no per-agent or per-namespace credential copying.

When a credential Secret is updated, the gateway's Secret watcher picks up the change and refreshes the in-memory credential without a restart.

---

## Provider Adapters

The gateway supports multiple upstream provider types via a pluggable adapter interface:

```go
type ProviderAdapter interface {
    Type() string                               // "anthropic", "openai", etc.
    ExtractUsage(resp Response) (Usage, error)
    ForwardRequest(ctx, req, credentials) (Response, error)
    HealthCheck(ctx, endpoint, credentials) error
}
```

v1 ships adapters for: Anthropic, OpenAI, Google Vertex, OpenAI-compatible (Ollama/vLLM/LiteLLM gateways).

Pre-call token estimation is not used for budget gating (it adds latency and is inaccurate). Budget checks use the last-known spend state. Post-call actual usage is authoritative for accounting.

---

## Fallback Logic

When the primary provider fails (network error, 5xx, timeout, or budget-blocked), the gateway walks `ModelProvider.spec.fallback` in order:

1. Verify the namespace is in the fallback provider's `allowedNamespaces`.
2. Verify the requested model exists in the fallback provider's `models`.
3. Forward the request with the fallback provider's credentials.

Fallback does not chain beyond one level — if the fallback itself fails, the gateway returns an error rather than walking the fallback's fallback.

---

## Rate Limiting

Rate limits are enforced at the gateway using token-bucket limiters keyed on (namespace, model). Limits come from `ModelProvider.spec.rateLimits`. When a limit is hit, the gateway returns HTTP 429 with a `Retry-After` header.

In a replicated gateway deployment, rate limiting is per-replica (approximate at the cluster level). This is acceptable for v1; a coordinated rate limiter can be introduced in v2 if needed.

---

## User Gateway — Request Flow

1. **Platform event arrives**: Discord posts a webhook event, WhatsApp sends a message notification, etc.
2. **Platform adapter authenticates**: the gateway verifies the event is from the expected platform (e.g., validates Discord's request signature).
3. **Normalization**: the adapter translates the platform-native payload into the Agentry message envelope:
   ```json
   {
     "messageId": "uuid",
     "channelType": "discord",
     "channelId": "987654321098765432",
     "userId": "111222333444555666",
     "content": "Hello, I need help with my order",
     "attachments": [],
     "metadata": { "guildId": "123456789012345678" }
   }
   ```
4. **AgentChannel lookup**: the gateway finds the AgentChannel resource matching the (channelType, channelId) pair, which identifies the target Agent and namespace.
5. **Activator check**: if the Agent is `Hibernated`, the gateway signals the controller to wake it and waits (returning a "typing" or "processing" indicator to the platform while the Pod starts).
6. **Message delivery**: the gateway posts the normalized envelope to `POST /v1/message` on the Agent's ClusterIP Service (or the override endpoint in `AgentChannel.spec.agentEndpoint`).
7. **Response translation**: the agent returns a response envelope; the gateway translates it to the platform-native reply format and sends it.

### Platform Adapters

v1 ships adapters for: Discord (bot token + webhook), WhatsApp (Cloud API), generic webhook (inbound HTTP POST with configurable auth).

Platform adapters follow the same plugin pattern as LLM provider adapters:

```go
type ChannelAdapter interface {
    Type() string
    Authenticate(req *http.Request, credentials Credentials) error
    ParseInbound(req *http.Request) (MessageEnvelope, error)
    FormatOutbound(envelope MessageEnvelope) ([]byte, error)
    SendReply(ctx context.Context, envelope MessageEnvelope, credentials Credentials) error
}
```

---

## Activator

When an Agent is in the `Hibernated` phase, its Service has no endpoints. The gateway serves as the activator:

1. A channel message arrives at the User Gateway targeting a hibernated Agent (via AgentChannel).
2. The gateway calls the controller's activator endpoint (`POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service) to signal a wake request.
3. The controller transitions the Agent from `Hibernated` to `Resuming` and recreates the Pod.
4. The gateway sends a "typing" or "processing" indicator to the channel platform while waiting, then delivers the message once the Pod is ready (bounded by a configurable timeout). If the timeout is exceeded, the gateway returns an appropriate error to the platform.

---

## Observability

The gateway exposes Prometheus metrics on `:9090/metrics`:

**LLM Gateway:**
- `agentry_llm_requests_total{provider,model,namespace,status}`
- `agentry_llm_request_duration_seconds{provider,model}`
- `agentry_llm_tokens_total{provider,model,namespace,direction}` (direction = input|output)
- `agentry_llm_spend_usd_total{provider,namespace}`
- `agentry_llm_fallback_total{from_provider,to_provider,reason}`
- `agentry_llm_budget_utilization{provider,namespace,period}` (gauge, 0-1)

**User Gateway:**
- `agentry_channel_messages_total{channel_type,namespace,status}`
- `agentry_channel_message_duration_seconds{channel_type}`
- `agentry_channel_wake_total{namespace}` (count of hibernation wakes triggered)

---

## Failure Modes

| Failure | Behavior |
|---|---|
| Gateway replica crashes | Other replicas continue; Kubernetes restarts the crashed replica |
| All gateway replicas down | LLM calls from agents fail; channel messages queue at the platform (Discord, WhatsApp retry); controller cannot wake hibernated agents |
| Provider API down | Fallback attempted; if fallback also down, request fails with a structured error |
| Budget exhausted | Request blocked or degraded per policy; Warning event emitted on ModelProvider |
| Channel credential invalid | AgentChannel marked `Ready=False`; platform connection drops |
| Agent Pod not ready (resuming) | User Gateway holds or retries message delivery up to configured timeout |
