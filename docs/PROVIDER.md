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

Pros: Credentials never leave `agentry-system`. NetworkPolicy cleanly isolates agent Pods (deny all egress to LLM provider IPs; allow egress to the gateway Service — this is cross-Pod and fully enforceable). Budget state is centralized in one process with no replication lag. The gateway also serves as the activator for hibernated agents. SPOF concern is addressed with 2-3 replicas, a PodDisruptionBudget (`minAvailable: 1`), and `maxUnavailable: 1` rolling updates.

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
│  │              Activator / Activity Store                     │     │
│  │  Monitors Agent hibernation state; signals controller to    │     │
│  │  wake agents on incoming traffic; tracks per-agent activity │     │
│  │  timestamps in-memory; serves GET /v1/activity to controller│     │
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

1. **Agent sends request**: the agent container makes an HTTPS request to `$AGENTRY_PROVIDER_ENDPOINT` (resolves to the gateway Service in `agentry-system`). The agent uses the upstream provider's native API path (e.g., `/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible) and includes a qualified model name in the request body (see Model Identification below).
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

**Pod IP reassignment**: when a Pod is deleted and a new Pod receives the same IP (common in small CIDR ranges), the informer cache may briefly map the old Pod. The gateway MUST process Pod delete events before accepting traffic from recycled IPs. The Pod informer must be fully synced before the gateway's readiness probe passes. In practice, the watch event for Pod deletion arrives before the new Pod is scheduled, so the window is negligible — but the gateway must not serve LLM traffic until its initial informer sync is complete.

---

## TLS on the LLM Gateway Listener

The LLM Gateway listener serves TLS to protect LLM request and response payloads in transit within the cluster. Without TLS, prompts and completions traverse the cluster network in plaintext, which is unacceptable when agent containers run untrusted code on shared nodes.

**Certificate provisioning**: the operator generates a self-signed CA certificate and stores it in a Secret in `agentry-system` (`agentry-gateway-ca`). On startup, the operator creates a TLS serving certificate for the gateway signed by this CA and stores it in a separate Secret (`agentry-gateway-tls`). The gateway reads this Secret to serve HTTPS on its LLM listener.

**Agent trust**: the CA certificate is injected into agent Pods as a projected volume mount at a well-known path (`/var/run/agentry/ca.crt`). The controller sets the `$AGENTRY_CA_CERT` environment variable pointing to this path. Agent containers (or their HTTP clients) must trust this CA when calling `$AGENTRY_PROVIDER_ENDPOINT`. The reference base images handle this automatically.

**Certificate rotation**: the operator rotates the serving certificate before expiry (default: 90-day lifetime, rotate at 60 days). The gateway watches the TLS Secret and reloads without restart. The CA certificate has a longer lifetime (1 year) and is rotated less frequently.

**No cert-manager dependency**: the operator manages its own CA and serving certificates. This keeps the deployment self-contained. Teams that prefer cert-manager can override the TLS Secrets externally; the gateway does not care how the certificate is provisioned as long as the Secret exists.

`$AGENTRY_PROVIDER_ENDPOINT` is an `https://` URL when TLS is enabled (the default). The User Gateway's internal delivery to agent Services (`POST /v1/message`) remains plaintext HTTP since it is initiated by a trusted component (the gateway) to the agent, not the reverse.

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

The gateway uses the detected format to parse the request body (extracting the model name and other fields), then forwards to the upstream provider.

The gateway is **protocol-aware** in that it understands request/response shapes for supported provider types (for token extraction, model name parsing, etc.), but it does **not** translate between formats. Cross-format fallback (e.g., Anthropic-format request falling back to an OpenAI-compatible endpoint) is not supported in v1. Fallback is restricted to providers of the same `spec.type` — see Fallback Logic below. This keeps the gateway's request path simple and avoids the large, error-prone surface area of bidirectional API translation (streaming, tool use, multimodal content, etc.).

---

## Budget State Management

Budget counters are maintained **in-process in the gateway**. Because the gateway is the single choke point for all LLM traffic, there is no need for a separate aggregator or distributed counter.

Each gateway replica maintains an in-memory spend counter per (provider, namespace, period) tuple. On startup, each replica reads the current period's spent value from the canonical ConfigMap managed by the ModelProviderReconciler. On each LLM call, the counter is updated synchronously.

**Budget counter exchange interface**: each gateway replica periodically (every 10s) writes its partial spend counters to a ConfigMap in `agentry-system` named `agentry-budget-{providerName}`, keyed by the replica's Pod name. The ConfigMap data structure is:

```yaml
data:
  # Each key is a gateway Pod name; value is JSON with per-namespace spend
  agentry-gateway-0: '{"team-support": "142.50", "team-ml": "87.30"}'
  agentry-gateway-1: '{"team-support": "138.20", "team-ml": "91.10"}'
  _canonical: '{"team-support": "280.70", "team-ml": "178.40"}'
```

The ModelProviderReconciler reads this ConfigMap every 30s, sums the per-replica partials, writes the `_canonical` key with the total, and updates `status.budgetUsage` on the ModelProvider. Gateway replicas read the `_canonical` key on startup to initialize their local counters. This avoids a Prometheus dependency and works with existing ConfigMap RBAC.

**Budget state on crash**: if all gateway replicas crash simultaneously, up to 10s of spend data (the partial-write interval) may be lost. This is acceptable given that budgets are soft limits — the bounded loss is small relative to typical budget thresholds. Gateway replicas re-initialize from the `_canonical` ConfigMap value on restart.

This means budget enforcement is **approximate under high concurrency** — replicas can collectively overspend before the next reconcile cycle. The overspend is bounded by: `number_of_replicas × max_calls_per_second_per_replica × cost_per_call × reconcile_interval_seconds`. For typical deployments (2-3 replicas, 1 call/sec peak per replica, $0.01-0.10 per call, 30s interval), the maximum overspend per cycle is roughly $0.60-$9.00, which is acceptable for a soft guardrail.

**Agentry's budget feature is spend visibility and soft guardrails, not a hard financial cap.** This is an explicit design decision, not a limitation to be fixed. Teams requiring hard caps should use provider-level account limits in addition to Agentry's per-namespace guardrails.

**Budget period rollover**: budget periods roll over at midnight UTC. The gateway detects period changes on each request and resets its local counters when the current period no longer matches the stored period. The controller archives the previous period's totals to ModelProvider status for auditability, deletes all per-replica keys from the budget ConfigMap, and writes a fresh `_canonical: {}`.

**Stale replica cleanup**: when a gateway replica is scaled down or replaced, its entry in the budget ConfigMap persists. The ModelProviderReconciler cross-references ConfigMap keys against the current set of gateway Pod names and deletes stale entries before summing partials. This prevents inflated spend totals from terminated replicas.

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

1. Verify the fallback provider has the **same `spec.type`** as the primary provider (e.g., both `anthropic`, or both `openai-compatible`). If the types differ, the fallback is skipped. This constraint exists because the gateway does not translate between API formats — see Request Format Detection above.
2. Verify the namespace is in the fallback provider's `allowedNamespaces`.
3. Verify the requested model exists in the fallback provider's `models`.
4. Check the fallback provider's budget state for the agent's namespace. If the fallback is budget-blocked, skip it and try the next fallback in the list (or return an error if no more fallbacks remain).
5. Forward the request with the fallback provider's credentials.

Fallback does not chain beyond one level — if the fallback itself fails, the gateway returns an error rather than walking the fallback's fallback.

**Same-type constraint**: this is validated at reconcile time by the ModelProviderReconciler. A ModelProvider with `type: anthropic` cannot list a fallback with `type: openai`. Cross-format fallback may be considered for a future version if there is sufficient demand, but the translation surface area (streaming, tool use, system prompts, multimodal content) is large and error-prone.

---

## Rate Limiting

Rate limits are enforced at the gateway using token-bucket limiters keyed on (namespace, model). Limits come from `ModelProvider.spec.rateLimits`. When a limit is hit, the gateway returns HTTP 429 with a `Retry-After` header.

In a replicated gateway deployment, rate limiting is per-replica (approximate at the cluster level). This is acceptable for v1; a coordinated rate limiter can be introduced in v2 if needed.

**Note:** the effective cluster-wide rate limit is approximately `configured_limit × number_of_replicas`, since each replica enforces independently. Platform engineers should account for this when setting `rateLimits` values — e.g., to achieve a cluster-wide ceiling of 300 req/min with 3 replicas, set `requestsPerMinute: 100`.

---

## User Gateway — Request Flow

1. **Webhook event arrives**: an external system POSTs to the gateway's webhook endpoint (e.g., `/channels/support-assistant`).
2. **Webhook adapter authenticates**: the gateway verifies the request using the configured auth method (bearer token, HMAC signature, etc.) from the AgentChannel's `credentialsRef`.
3. **Normalization**: the adapter translates the webhook payload into the Agentry message envelope:
   ```json
   {
     "messageId": "uuid",
     "channelType": "webhook",
     "channelId": "/channels/support-assistant",
     "userId": "caller-id-from-header-or-body",
     "content": "Hello, I need help with my order",
     "attachments": [],
     "metadata": {}
   }
   ```
4. **AgentChannel lookup**: the gateway finds the AgentChannel resource matching the webhook path, which identifies the target Agent and namespace.
5. **Activator check**: if the Agent is `Hibernated`, the gateway signals the controller to wake it and waits up to `wakeTimeout` for the Pod to become Ready. The webhook caller receives a response only after the agent processes the message (or a timeout error).
6. **Message delivery**: the gateway posts the normalized envelope to `POST /v1/message` on the Agent's ClusterIP Service (or the override endpoint in `AgentChannel.spec.agentEndpoint`).
7. **Response**: the agent returns a response envelope; the gateway returns it as the webhook HTTP response body.

### Platform Adapters

v1 ships with the **generic webhook adapter** only (inbound HTTP POST with configurable auth). Discord and WhatsApp adapters are deferred to v1.1 — they require persistent connections (Discord WebSocket, WhatsApp Cloud API registration), platform-specific reconnection logic, and API versioning, which adds significant implementation surface. The webhook adapter is stateless and covers the core channel integration pattern.

Platform adapters follow a plugin pattern for future extensibility:

```go
type ChannelAdapter interface {
    Type() string
    Authenticate(req *http.Request, credentials Credentials) error
    ParseInbound(req *http.Request) (MessageEnvelope, error)
    FormatOutbound(envelope MessageEnvelope) ([]byte, error)
    SendReply(ctx context.Context, envelope MessageEnvelope, credentials Credentials) error
}
```

Discord and WhatsApp adapters will implement this interface in v1.1.

---

## Activator

When an Agent is in the `Hibernated` phase, its Service has no endpoints. The gateway serves as the activator:

1. A channel message arrives at the User Gateway targeting a hibernated Agent (via AgentChannel).
2. The gateway calls the controller's activator endpoint (`POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service) to signal a wake request.
3. The controller transitions the Agent from `Hibernated` to `Resuming` and recreates the Pod.
4. The gateway waits for the Pod to become Ready (bounded by `spec.lifecycle.wakeTimeout`, which defaults from AgentClass), then delivers the message. If the timeout is exceeded, the gateway returns a timeout error to the webhook caller.

### Activator Authentication

The activator endpoint is authenticated to prevent unauthorized wake-ups from arbitrary Pods in the cluster. The operator generates a shared HMAC key on installation and stores it in a Secret in `agentry-system` (`agentry-activator-key`). Both the controller and gateway read this Secret:

- The gateway includes an `Authorization: Bearer <HMAC(timestamp:namespace:agentName)>` header with each activation request, plus an `X-Agentry-Timestamp` header.
- The controller validates the HMAC signature and rejects requests with timestamps older than 30 seconds (replay window).

This ensures only the gateway (which holds the shared key) can trigger agent wake-ups. The HMAC key is rotated by the operator on a configurable interval (default: 30 days); both components watch the Secret for changes.

---

## Activity Tracking API

The gateway maintains per-agent activity timestamps in-memory, updated on every LLM request, channel message delivery, and agent heartbeat. This avoids per-request etcd writes, which is critical at scale (hundreds of thousands of agents).

The gateway exposes an internal endpoint for the controller to query activity state:

**`GET /v1/activity?namespace={ns}`**

Returns a JSON map of agent names to their last-activity timestamps for the given namespace:

```json
{
  "support-assistant": "2026-04-05T11:58:22Z",
  "code-helper": "2026-04-05T11:45:10Z"
}
```

The controller queries this endpoint on each reconcile for agents in `Running` or `Idle` phase to evaluate idle and hibernation transitions. If the gateway is unreachable, the controller preserves the agent's current phase — no idle transitions are made without activity data.

Activity data is ephemeral: it is lost on gateway restart. This is acceptable because:
- On restart, agents in `Running` will have no recorded activity and will naturally transition to `Idle` after `idleTimeout` if they are truly idle.
- Agents that are actively sending traffic will re-establish their activity timestamps immediately.
- The worst case is a premature `Running → Idle` transition, which is self-correcting on the next activity signal.

The `activitySource` setting on Agent (`providerTraffic`, `agentHeartbeat`, `both`) controls which signals the gateway includes in its per-agent timestamp. The gateway tracks both sources separately and returns the appropriate one based on the Agent's configuration.

---

## Gateway Error Responses

When the gateway cannot fulfill an LLM request, it returns a structured error response so agents can handle failures programmatically (e.g., scenario S10 — graceful degradation on budget exhaustion).

**Error response body:**

```json
{
  "error": {
    "type": "budget_exhausted",
    "message": "Monthly budget for namespace team-support on provider anthropic-shared is exhausted (100% used)",
    "provider": "anthropic-shared",
    "retryable": false
  }
}
```

**HTTP status code mapping:**

| Status | `error.type` | Meaning |
|---|---|---|
| 400 | `invalid_request` | Malformed request, unknown model, missing provider prefix |
| 403 | `access_denied` | Namespace not in `allowedNamespaces`, model not in Agent's providers |
| 413 | `payload_too_large` | Artifact payload exceeds size limits (task completion only) |
| 429 | `rate_limited` | Per-namespace rate limit exceeded; includes `Retry-After` header |
| 502 | `provider_error` | Upstream provider returned an error after exhausting fallback chain |
| 503 | `budget_exhausted` | Budget blocked per policy; `retryable: false` |
| 503 | `provider_unavailable` | All providers (primary + fallback) unreachable |
| 504 | `provider_timeout` | Upstream provider timed out after exhausting fallback chain |

The `error.retryable` field indicates whether the agent should retry the request. Rate-limited requests are retryable; budget-exhausted and access-denied are not.

---

## Upstream TLS Configuration

The gateway always connects to upstream LLM providers over HTTPS. For enterprise environments that require custom CA bundles or HTTP proxies for outbound traffic, the gateway supports:

- **Custom CA bundle**: a ConfigMap in `agentry-system` (`agentry-upstream-ca`) containing additional CA certificates. The gateway loads these on startup and watches for changes. All upstream HTTPS connections trust both the system CA bundle and the custom bundle.
- **HTTP proxy**: standard `HTTPS_PROXY` / `NO_PROXY` environment variables on the gateway Deployment. The gateway respects these for all upstream provider calls.

These are gateway-level settings (not per-ModelProvider) because they typically reflect cluster-wide network infrastructure.

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
| All gateway replicas down | LLM calls from agents fail; webhook callers receive 503; controller cannot wake hibernated agents; up to 10s of spend data may be lost (see Budget State Management) |
| Provider API down | Fallback attempted (same-type providers only); if fallback also down, request fails with `502 provider_error` |
| Budget exhausted | Request blocked or degraded per policy; Warning event emitted on ModelProvider |
| Channel credential invalid | AgentChannel marked `Ready=False`; platform connection drops |
| Agent Pod not ready (resuming) | User Gateway holds or retries message delivery up to configured timeout |
