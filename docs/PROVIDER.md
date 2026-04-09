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
7. **Route to upstream**: the gateway adds the provider API key (read directly from Secrets in `agentry-system`), strips the provider prefix from the model name, and forwards the request. If the upstream fails (connection error, 5xx, timeout), the gateway walks the fallback chain — trying the primary provider's `spec.fallback` entries, then each fallback's own fallback, up to `maxFallbackDepth` (default 3). See Fallback Logic below.
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

**CA rotation**: CA rotation uses a **bundle-based approach** to avoid TLS outages. The `agentry-gateway-ca` Secret contains a CA bundle (a concatenated PEM file) rather than a single certificate. During rotation, the bundle contains both the current and the new CA certificates. The rotation sequence is:

1. **Generate new CA**: the operator creates a new CA certificate and appends it to the CA bundle in `agentry-gateway-ca`. Both old and new CA certificates are now trusted by all components.
2. **Re-issue gateway cert**: the operator re-issues the gateway serving certificate (`agentry-gateway-tls`) signed by the new CA. The gateway reloads it. Agents trust both CAs via the bundle, so there is no interruption.
3. **Re-issue agent certs**: the operator re-issues per-agent TLS certificates signed by the new CA, rolling across agents over multiple reconcile cycles. The gateway trusts both CAs via its copy of the bundle, so agents with old certs remain valid during the rollout.
4. **Remove old CA**: once all serving certificates have been re-issued from the new CA, the operator removes the old CA from the bundle. Only the new CA remains.

This is the same pattern Kubernetes itself uses for service account signing key rotation. The CA bundle is projected into agent Pods at `/var/run/agentry/ca.crt`; the kubelet updates the projected volume contents automatically when the Secret changes.

**No cert-manager dependency**: the operator manages its own CA and serving certificates, including CA rotation. This keeps the deployment self-contained. Teams that prefer cert-manager can override the TLS Secrets externally; the gateway does not care how the certificate is provisioned as long as the Secret exists.

`$AGENTRY_PROVIDER_ENDPOINT` is an `https://` URL when TLS is enabled (the default).

**Agent Serving TLS**: the User Gateway's delivery to agent Services (`POST /v1/message`) is also over HTTPS. The operator issues a per-agent TLS serving certificate signed by the same operator-managed CA. The certificate and key are mounted into the agent Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The certificate's SAN includes the agent's Service DNS name. The gateway verifies the agent's certificate against the operator CA on every message delivery request. Certificate lifecycle (rotation, expiry) follows the same policy as the gateway serving certificate — see Controller Design doc § AgentReconciler.

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

**Budget counter exchange interface**: each gateway replica periodically (every 10s) writes its partial spend counters to a ConfigMap in `agentry-system` named `agentry-budget-{providerName}`, keyed by the replica's Pod name. Replicas use **server-side apply** with per-replica field managers (field manager name = Pod name), so each replica owns only its own key. This eliminates optimistic concurrency conflicts between replicas writing simultaneously. The ConfigMap data structure is:

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

Fallback chains up to a configurable depth. If provider A's fallback is B, and B has a fallback to C, the gateway tries A → B → C. The maximum depth is controlled by the gateway-level `maxFallbackDepth` setting (default: 3). If the chain is exhausted or the depth cap is reached without a successful response, the gateway returns an error. The depth cap prevents unbounded latency from long fallback chains. Circular references are rejected at reconcile time by the ModelProviderReconciler (see Controller Design doc), so the gateway does not need runtime cycle detection.

**Same-type constraint**: this is validated at reconcile time by the ModelProviderReconciler. Each provider in the fallback chain must have the same `spec.type` as the primary provider (e.g., all `anthropic` or all `openai-compatible`). A ModelProvider with `type: anthropic` cannot list a fallback with `type: openai`. Cross-format fallback may be considered for a future version if there is sufficient demand, but the translation surface area (streaming, tool use, system prompts, multimodal content) is large and error-prone.

---

## Rate Limiting

Rate limits are enforced at the gateway using token-bucket limiters keyed on (namespace, model). Limits come from `ModelProvider.spec.rateLimits` and represent **cluster-wide ceilings**. When a limit is hit, the gateway returns HTTP 429 with a `Retry-After` header.

Each gateway replica divides the configured limit by the number of active gateway replicas (discovered from its Pod informer — count Pods matching the gateway label selector). When replicas scale up or down, each replica adjusts its local token bucket capacity on the next refill cycle. This means the configured value directly represents the intended cluster-wide rate limit regardless of replica count.

**Note:** because each replica enforces its share independently, the effective cluster-wide limit is approximate — transient bursts may slightly exceed the configured ceiling. The approximation is bounded by `configured_limit / number_of_replicas` per replica (one replica's full bucket) and is acceptable for v1.

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
4. **AgentChannel lookup**: the gateway finds the AgentChannel resource matching the webhook path, which identifies the target Agent and namespace. If multiple AgentChannels claim the same path (a transient conflict before the reconciler marks the newer one as `Ready=False`), the gateway routes to the AgentChannel with the earliest `creationTimestamp`.
   If the AgentChannel has `session.enabled: true`, the gateway generates a deterministic `sessionId` from the message's `channelId` and `userId`: `sessionId = UUIDv5(namespace: fixed Agentry UUID, name: channelId + ":" + userId)`. This ID is stable across gateway replicas and restarts — no gateway-side session state is required. Session expiry and rotation are the agent's responsibility using its PVC state.
5. **Activator check**: if the Agent is `Hibernated`, the gateway signals the controller to wake it and waits up to `wakeTimeout` for the Pod to become Ready. In sync mode, the webhook caller blocks during this wait. In async mode, the gateway has already returned 202 (see step 5a).
5a. **Async early return** (async mode only): if `AgentChannel.spec.webhook.responseMode` is `async`, the gateway returns HTTP 202 Accepted with a `requestId` immediately after normalization (step 3). Steps 5-7 proceed asynchronously — the webhook caller does not block. See API Design doc § Async Webhook Response for the response schemas.
6. **Message delivery**: the gateway posts the normalized envelope to `POST /v1/message` on the Agent's ClusterIP Service over HTTPS (or the override endpoint in `AgentChannel.spec.agentEndpoint`). The gateway verifies the agent's TLS certificate against the operator-managed CA.
7. **Response (sync mode, default)**: the agent returns a response envelope; the gateway returns it as the webhook HTTP response body.
8. **Response (async mode)**: the agent returns a response envelope; the gateway POSTs it to the configured `callbackUrl` (with retries) or stores it for polling retrieval via `GET /v1/channels/{channelId}/responses/{requestId}`.

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

The `SendReply` method is used for async response delivery: when `responseMode: async`, the gateway calls `SendReply` to POST the agent's response to the configured `callbackUrl` after the agent has processed the message. For sync mode, `SendReply` is not called — the response is returned inline as the HTTP response body. Discord and WhatsApp adapters (v1.1) will use `SendReply` for all responses since those platforms are inherently asynchronous.

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

The gateway exposes an internal endpoint for the controller to query activity state. This endpoint uses the same HMAC authentication as the activator endpoint (shared key from `agentry-activator-key` Secret) — see § Activator Authentication.

**`GET /v1/activity?namespace={ns}`**

Returns a JSON object containing the gateway's startup timestamp and a map of agent names to their last-activity timestamps for the given namespace:

```json
{
  "startedAt": "2026-04-05T06:00:00Z",
  "agents": {
    "support-assistant": "2026-04-05T11:58:22Z",
    "code-helper": "2026-04-05T11:45:10Z"
  }
}
```

The `startedAt` field indicates when the gateway started. The controller uses this to detect gateway restarts — if the gateway started more recently than an agent's last phase transition, missing activity data is treated as "unknown" rather than "no activity" (see Controller Design doc § Activity Detection).

The controller queries this endpoint on each reconcile for agents in `Running` or `Idle` phase to evaluate idle and hibernation transitions. If the gateway is unreachable, the controller preserves the agent's current phase — no idle transitions are made without activity data.

Activity data is ephemeral: it is lost on gateway restart. The gateway includes its `startedAt` timestamp in the `/v1/activity` response so the controller can detect this condition. After a gateway restart:
- The controller defers idle and hibernation transitions for agents whose last phase transition predates the gateway's `startedAt`, treating missing data as "unknown" until the gateway has been running for at least `idleTimeout`.
- Agents that are actively sending traffic re-establish their activity timestamps immediately.
- Agents that are truly idle will transition to `Idle` after `idleTimeout` elapses from the gateway's startup, which is the correct behavior.

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
| Provider API down | Fallback chain walked (same-type providers only, up to `maxFallbackDepth` depth); if all providers in the chain fail, request fails with `502 provider_error` |
| Budget exhausted | Request blocked or degraded per policy; Warning event emitted on ModelProvider |
| Channel credential invalid | AgentChannel marked `Ready=False`; platform connection drops |
| Agent Pod not ready (resuming) | User Gateway holds or retries message delivery up to configured timeout |
