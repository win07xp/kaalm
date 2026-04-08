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

1. **Agent sends request**: the agent container makes an HTTP request to `$AGENTRY_PROVIDER_ENDPOINT` (resolves to the gateway Service in `agentry-system`). The agent uses the upstream provider's native API path (e.g., `/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible) and includes a qualified model name in the request body (see Model Identification below).
2. **Namespace identification**: the gateway resolves the source IP to a Pod via its Pod informer cache, then reads the Pod's namespace (see Namespace Identification below).
3. **Provider routing**: the gateway resolves the Pod's ownerRef to the Agent resource, reads `spec.providers` to determine which ModelProviders this agent is allowed to use, and parses the `provider/model` name from the request to identify the target ModelProvider (see Provider Routing below).
4. **Gateway validates**: confirms the requested model is listed in the target ModelProvider's `models` and the namespace is in `allowedNamespaces`.
5. **Budget check**: the gateway reads the current budget state for the agent's namespace. If a `degrade` policy applies, it rewrites the model name in the request. If `block` applies, it returns an error to the agent.
6. **Rate limit check**: per-namespace token-bucket rate limiter on requests/min and tokens/min.
7. **Route to upstream**: the gateway adds the provider API key (read directly from Secrets in `agentry-system`), strips the provider prefix from the model name, and forwards the request. If the upstream fails (connection error, 5xx, timeout), the gateway tries the next provider in `spec.fallback`.
8. **Response returned**: the gateway relays the response to the agent container.
9. **Token counting**: the gateway extracts actual token usage from the provider response (`usage.input_tokens` / `usage.output_tokens` for Anthropic; `usage.prompt_tokens` / `usage.completion_tokens` for OpenAI, etc.). Actual usage from the provider response is always preferred over pre-call estimation.
10. **Spend update**: the gateway updates the in-process spend counter for the namespace.

---

## Namespace Identification

The gateway must identify the source namespace of each LLM request to enforce access controls and per-namespace budget tracking. It does this via **source IP resolution**:

1. The gateway maintains a Pod informer cache (it already watches Pods cluster-wide for annotation writes).
2. On each inbound LLM request, the gateway resolves the source IP to a Pod via the informer cache, then reads the Pod's namespace.
3. If the source IP does not resolve to a known Pod, the request is rejected.

This approach is **unforgeable**: source IPs are assigned by the cluster network (CNI) and cannot be overridden from within a container. An agent cannot claim to be from a different namespace. No agent-provided headers or tokens are trusted for namespace identification.

---

## Model Identification

Agents identify both the provider and the model in each LLM request using a **qualified model name** format: `{providerRef}/{modelId}`.

Examples:
- `anthropic-shared/claude-opus-4-6` — Claude Opus via the `anthropic-shared` ModelProvider
- `anthropic-shared/claude-sonnet-4-6` — Claude Sonnet via the same provider
- `openai-fallback/gpt-4o` — GPT-4o via the `openai-fallback` ModelProvider
- `local-vllm/llama-3-70b` — Llama 3 70B via a local vLLM instance registered as a ModelProvider

The gateway splits the model name on the first `/`: the prefix identifies the ModelProvider by `metadata.name`, and the suffix is the raw model ID that must appear in the ModelProvider's `models` list. Before forwarding upstream, the gateway strips the provider prefix and sends only the raw model ID (e.g., the upstream Anthropic API receives `claude-opus-4-6`, not `anthropic-shared/claude-opus-4-6`).

This format uniquely identifies the (provider, model) pair and eliminates ambiguity when multiple ModelProviders offer models with similar names (e.g., a managed Anthropic endpoint and an OpenAI-compatible proxy both serving Claude models).

The `defaultModel` field in `Agent.spec.providers` uses the raw model ID (not the qualified format) because the provider context is already established by the enclosing `providerRef`. The agent is responsible for constructing the qualified `provider/model` name in its API calls.

---

## Provider Routing

The gateway resolves which ModelProvider to use for each LLM request through the following chain:

1. **Source IP → Pod**: resolved from the Pod informer cache (see Namespace Identification).
2. **Pod → Agent**: the Pod's ownerRef identifies the Agent (or AgentTask) resource. The gateway maintains an Agent informer cache for this lookup.
3. **Agent → allowed providers**: the Agent's `spec.providers` lists the ModelProviders this agent may use.
4. **Model name → ModelProvider**: the gateway parses the `provider/model` qualified name from the request body. The provider prefix must match a `providerRef` in the Agent's `spec.providers`. If it does not, the request is rejected.
5. **ModelProvider → upstream**: the gateway reads the ModelProvider's `spec.endpoint`, `spec.type`, and credentials to forward the request.

This chain ensures that an agent can only reach ModelProviders explicitly listed in its spec, which in turn must be in the AgentClass's `allowedProviders` and must include the agent's namespace in `allowedNamespaces`. All three access checks (Agent → ModelProvider → Namespace) must pass.

---

## Request Format Detection

The agent sends LLM requests using the upstream provider's native API format. The gateway detects the request format from the **URL path** the agent uses:

- `/v1/messages` → Anthropic format
- `/v1/chat/completions` → OpenAI / OpenAI-compatible format (also used by vLLM, Ollama, LiteLLM)
- `/v1/completions` → OpenAI legacy completions format

The gateway uses the detected format to parse the request body (extracting the model name and other fields), then forwards to the upstream provider. If the upstream provider expects a different format than what the agent sent (e.g., the agent sends Anthropic-format but the fallback provider is OpenAI-compatible), the gateway translates the request and response between formats.

This means the gateway is **protocol-aware**, not a simple pass-through proxy. It understands the request/response shapes of supported provider types and can translate between them. This translation enables cross-provider fallback (e.g., falling back from Anthropic to an OpenAI-compatible endpoint) without requiring the agent to handle multiple API formats.

---

## Budget State Management

Budget counters are maintained **in-process in the gateway**. Because the gateway is the single choke point for all LLM traffic, there is no need for a separate aggregator or distributed counter.

Each gateway replica maintains an in-memory spend counter per (provider, namespace, period) tuple. On startup, each replica reads the current period's spent value from a ConfigMap managed by the ModelProviderReconciler. On each LLM call, the counter is updated synchronously.

**Multi-replica consistency**: In a replicated gateway deployment, each replica has its own counter. Spend is reconciled across replicas by the controller periodically (every 30s) — the controller sums the per-replica partial counters and writes the canonical total back to ModelProvider status. Each replica then refreshes from the canonical total.

This means budget enforcement is **approximate under high concurrency** — replicas can collectively overspend before the next reconcile cycle. The overspend is bounded by: `number_of_replicas × max_calls_per_second_per_replica × cost_per_call × reconcile_interval_seconds`. For typical deployments (2-3 replicas, 1 call/sec peak per replica, $0.01-0.10 per call, 30s interval), the maximum overspend per cycle is roughly $0.60-$9.00, which is acceptable for a soft guardrail.

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
3. Check the fallback provider's budget state for the agent's namespace. If the fallback is budget-blocked, skip it and try the next fallback in the list (or return an error if no more fallbacks remain).
4. Forward the request with the fallback provider's credentials.

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
