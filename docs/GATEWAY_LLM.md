# Agentry — LLM Gateway Design

This document covers the LLM Gateway: the shared cluster-level component responsible for mediating LLM traffic between agent containers and upstream providers. It is where spend tracking, budget guardrails, rate limiting, fallback, and credential isolation live.

For the User Gateway (channel message delivery, activator, activity tracking), see [GATEWAY_USER.md](./GATEWAY_USER.md). For the HTTP endpoint contracts agents use, see [API_ENDPOINTS.md](./API_ENDPOINTS.md).

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
│  │                      (see GATEWAY_USER.md)                  │     │
│  └─────────────────────────────────────────────────────────────┘     │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐     │
│  │              Activator / Activity Store                     │     │
│  │                      (see GATEWAY_USER.md)                  │     │
│  └─────────────────────────────────────────────────────────────┘     │
└──────────────────────────────────────────────────────────────────────┘
         │ (egress)
         ▼
  LLM Provider APIs
  (Anthropic, OpenAI,
   Vertex, etc.)
```

---

## LLM Gateway — Request Flow

1. **Agent sends request**: the agent container makes an HTTPS request to `$AGENTRY_PROVIDER_ENDPOINT` (resolves to the gateway Service in `agentry-system`). The agent uses the upstream provider's native API path (e.g., `/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible) and includes a qualified model name in the request body (see [Model Identification](#model-identification) below).
2. **Namespace identification**: the gateway resolves the source IP to a Pod via its Pod informer cache, then reads the Pod's namespace (see [Namespace Identification](#namespace-identification) below).
3. **Provider routing**: the gateway resolves the Pod's ownerRef to the Agent resource, reads `spec.providers` to determine which ModelProviders this agent is allowed to use, and parses the `provider/model` name from the request to identify the target ModelProvider (see [Provider Routing](#provider-routing) below).
4. **Gateway validates**: confirms the requested model is listed in the target ModelProvider's `models` and the namespace is in `allowedNamespaces`.
5. **Budget check**: the gateway reads the current budget state for the agent's namespace. If a `degrade` policy applies, it rewrites the model name in the request. If `block` applies, it returns an error to the agent.
6. **Rate limit check**: per-namespace token-bucket rate limiter on requests/min and tokens/min.
7. **Route to upstream**: the gateway adds the provider API key (read directly from Secrets in `agentry-system`), strips the provider prefix from the model name, and forwards the request. If the upstream fails (connection error, 5xx, timeout), the gateway walks the fallback chain — trying the primary provider's `spec.fallback` entries, then each fallback's own fallback, up to `maxFallbackDepth` (default 3). See [Fallback Logic](#fallback-logic) below.
8. **Response returned**: the gateway relays the response to the agent container.
9. **Token counting**: the gateway extracts actual token usage from the provider response (`usage.input_tokens` / `usage.output_tokens` for Anthropic; `usage.prompt_tokens` / `usage.completion_tokens` for OpenAI, etc.). Actual usage from the provider response is always preferred over pre-call estimation.
10. **Spend update**: the gateway updates the in-process spend counter for the namespace.

---

## Namespace Identification

The gateway identifies the source namespace and agent identity of each LLM request using **mutual TLS (mTLS)** as the primary mechanism, with source IP as a secondary defense-in-depth cross-check.

### Primary: mTLS client certificate

The LLM Gateway listener requires a client certificate from every agent that connects. Agents present their operator-issued TLS certificate (mounted at `$AGENTRY_TLS_CERT`, `/var/run/agentry/tls.crt`) as the client cert. The gateway verifies it against the operator-managed CA and extracts the agent's identity from the certificate's SAN field: `{name}.{namespace}.svc.cluster.local`. This gives the gateway a cryptographically attested (namespace, agent name) pair on every request.

Reference base images handle client cert presentation automatically — agents using the base image do not need to configure this. Custom images must configure their HTTP client to present the cert at `$AGENTRY_TLS_CERT` with the key at `$AGENTRY_TLS_KEY` when calling `$AGENTRY_PROVIDER_ENDPOINT`. See [Agent Runtime Contract](./ARCHITECTURE.md#agent-runtime-contract).

This approach is **CNI-independent**: agent identity is cryptographically attested by the certificate, not by a network-layer header that may be modified by intermediate infrastructure. No agent can claim a different identity without a valid CA-signed certificate for that identity.

### Secondary: source IP cross-check

After extracting identity from the client certificate, the gateway additionally resolves the source IP to a Pod via its Pod informer cache and confirms the Pod belongs to the same (namespace, agent name) as the certificate claims. If there is a mismatch, the request is rejected. This cross-check protects against a compromised agent presenting a stolen certificate from a different agent — the certificate identity must match the actual source Pod.

The gateway maintains a Pod informer cache for this lookup (and for other purposes — provider routing resolution, activity tracking). The Pod informer must be fully synced before the gateway's readiness probe passes.

**Pod IP reassignment**: when a Pod is deleted and a new Pod receives the same IP (common in small CIDR ranges), the informer cache may briefly map the old Pod. The gateway MUST process Pod delete events before accepting traffic from recycled IPs. In practice, the watch event for Pod deletion arrives before the new Pod is scheduled, so the window is negligible.

See [Agent-to-Gateway Authentication](./SECURITY.md#agentgateway-authentication) for the full security analysis.

---

## TLS on the LLM Gateway Listener

The LLM Gateway listener serves TLS to protect LLM request and response payloads in transit within the cluster. Without TLS, prompts and completions traverse the cluster network in plaintext, which is unacceptable when agent containers run untrusted code on shared nodes. See [In-cluster TLS](./SECURITY.md#in-cluster-tls-bidirectional) for the full security analysis.

**Certificate provisioning**: the operator generates a self-signed CA certificate and stores it in a Secret in `agentry-system` (`agentry-gateway-ca`). On startup, the operator creates a TLS serving certificate for the gateway signed by this CA and stores it in a separate Secret (`agentry-gateway-tls`). The gateway reads this Secret to serve HTTPS on its LLM listener.

**Agent trust**: the CA certificate is injected into agent Pods as a projected volume mount at a well-known path (`/var/run/agentry/ca.crt`). The controller sets the `$AGENTRY_CA_CERT` environment variable pointing to this path. Agent containers (or their HTTP clients) must trust this CA when calling `$AGENTRY_PROVIDER_ENDPOINT`. The reference base images handle this automatically.

**Mutual TLS (mTLS)**: the LLM Gateway listener requires client certificates from all connecting agents. Agents present their operator-issued per-agent TLS certificate (the same cert used for gateway→agent delivery) as the client cert when calling `$AGENTRY_PROVIDER_ENDPOINT`. The gateway verifies the client cert against the operator CA and extracts the SAN to identify the agent and namespace. Reference base images configure client cert presentation automatically. Custom images must explicitly configure their HTTP client to use `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY` as the client certificate. This is the primary identity mechanism for agent→gateway communication — see [Namespace Identification](#namespace-identification).

**Certificate rotation**: the operator rotates the serving certificate before expiry (default: 90-day lifetime, rotate at 60 days). The gateway watches the TLS Secret and reloads without restart. The CA certificate has a longer lifetime (1 year) and is rotated less frequently.

**CA rotation**: CA rotation uses a **bundle-based approach** to avoid TLS outages. The `agentry-gateway-ca` Secret contains a CA bundle (a concatenated PEM file) rather than a single certificate. During rotation, the bundle contains both the current and the new CA certificates. The rotation sequence is:

1. **Generate new CA**: the operator creates a new CA certificate and appends it to the CA bundle in `agentry-gateway-ca`. Both old and new CA certificates are now trusted by all components.
2. **Re-issue gateway cert**: the operator re-issues the gateway serving certificate (`agentry-gateway-tls`) signed by the new CA. The gateway reloads it. Agents trust both CAs via the bundle, so there is no interruption.
3. **Re-issue agent certs**: the operator re-issues per-agent TLS certificates signed by the new CA in a rate-limited rolling fashion — at most 50 agents per reconcile cycle (configurable), tracked via a ConfigMap in `agentry-system` (`agentry-ca-rotation-state`). The gateway trusts both CAs via the bundle, so agents with old certs remain valid during the rollout. See [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) for the implementation detail.
4. **Remove old CA**: once the reconciler has confirmed that every agent cert Secret is signed by the new CA (verified by checking all agent Secrets in `agentry-ca-rotation-state`), the operator removes the old CA from the bundle. Only the new CA remains.

This is the same pattern Kubernetes itself uses for service account signing key rotation. The CA bundle is projected into agent Pods at `/var/run/agentry/ca.crt`; the kubelet updates the projected volume contents automatically when the Secret changes.

**Agent health probes and TLS**: because the agent serves HTTPS on `$AGENTRY_HEALTH_PORT` (using the same operator-issued certificate), the readiness and liveness probes injected by the AgentReconciler must set `httpGet.scheme: HTTPS`. Kubernetes httpGet probes do not verify TLS certificates, so no additional CA configuration is required on the probe. See [Agent Runtime Contract](./ARCHITECTURE.md#agent-runtime-contract).

**No cert-manager dependency**: the operator manages its own CA and serving certificates, including CA rotation. This keeps the deployment self-contained. Teams that prefer cert-manager can override the TLS Secrets externally; the gateway does not care how the certificate is provisioned as long as the Secret exists.

`$AGENTRY_PROVIDER_ENDPOINT` is an `https://` URL when TLS is enabled (the default).

**Agent Serving TLS**: the User Gateway's delivery to agent Services (`POST /v1/message`) is also over HTTPS. The operator issues a per-agent TLS serving certificate signed by the same operator-managed CA. The certificate and key are mounted into the agent Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The certificate's SAN includes the agent's Service DNS name. The gateway verifies the agent's certificate against the operator CA on every message delivery request. Certificate lifecycle (rotation, expiry) follows the same policy as the gateway serving certificate — see [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler).

---

## Model Identification

Agents identify both the provider and the model in each LLM request using a **qualified model name** format: `{providerRef}/{modelId}`.

Examples:
- `anthropic-shared/claude-opus-4-6` — Claude Opus via the `anthropic-shared` ModelProvider
- `anthropic-shared/claude-sonnet-4-6` — Claude Sonnet via the same provider
- `openai-fallback/gpt-4o` — GPT-4o via the `openai-fallback` ModelProvider
- `local-vllm/llama-3-70b` — Llama 3 70B via a local vLLM instance registered as a ModelProvider

The gateway splits the model name on the first `/`: the prefix identifies the ModelProvider by `metadata.name`, and the suffix is the raw model ID that must appear in the ModelProvider's `models` list. Before forwarding upstream, the gateway strips the provider prefix and sends only the raw model ID (e.g., the upstream Anthropic API receives `claude-opus-4-6`, not `anthropic-shared/claude-opus-4-6`).

This format uniquely identifies the (provider, model) pair and eliminates ambiguity when multiple ModelProviders offer models with similar names (e.g., a managed Anthropic endpoint and an OpenAI-compatible proxy both serving Claude models). The agent is always responsible for constructing the qualified `provider/model` name in its API calls.

---

## Provider Routing

The gateway resolves which ModelProvider to use for each LLM request through the following chain:

1. **Source IP -> Pod**: resolved from the Pod informer cache (see [Namespace Identification](#namespace-identification)).
2. **Pod -> Agent**: the Pod's ownerRef identifies the Agent (or AgentTask) resource. The gateway maintains an Agent informer cache for this lookup.
3. **Agent -> allowed providers**: the Agent's `spec.providers` lists the ModelProviders this agent may use.
4. **Model name -> ModelProvider**: the gateway parses the `provider/model` qualified name from the request body. The provider prefix must match a `providerRef` in the Agent's `spec.providers`. If it does not, the request is rejected.
5. **ModelProvider -> upstream**: the gateway reads the ModelProvider's `spec.endpoint`, `spec.type`, and credentials to forward the request.

This chain ensures that an agent can only reach ModelProviders explicitly listed in its spec, which in turn must be in the AgentClass's `allowedProviders` and must include the agent's namespace in `allowedNamespaces`. All three access checks (Agent -> ModelProvider -> Namespace) must pass.

---

## Request Format Detection

The agent sends LLM requests using the upstream provider's native API format. The gateway detects the request format from the **URL path** the agent uses:

- `/v1/messages` -> Anthropic format
- `/v1/chat/completions` -> OpenAI / OpenAI-compatible format (also used by vLLM, Ollama, LiteLLM)
- `/v1/completions` -> OpenAI legacy completions format

The gateway uses the detected format to parse the request body (extracting the model name and other fields), then forwards to the upstream provider.

The gateway is **protocol-aware** in that it understands request/response shapes for supported provider types (for token extraction, model name parsing, etc.), but it does **not** translate between formats. Cross-format fallback (e.g., Anthropic-format request falling back to an OpenAI-compatible endpoint) is not supported in v1. Fallback is restricted to providers of the same `spec.type` — see [Fallback Logic](#fallback-logic) below. This keeps the gateway's request path simple and avoids the large, error-prone surface area of bidirectional API translation (streaming, tool use, multimodal content, etc.).

---

## Budget State Management

Budget counters are maintained **in-process in the gateway**. Because the gateway is the single choke point for all LLM traffic, there is no need for a separate aggregator or distributed counter.

Each gateway replica maintains an in-memory spend counter per (provider, namespace, period) tuple. On startup, each replica reads the current period's spent value from the canonical ConfigMap managed by the [ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler). On each LLM call, the counter is updated synchronously.

**Budget counter exchange interface**: each gateway replica periodically (every 10s) writes its partial spend counters to a ConfigMap in `agentry-system` named `agentry-budget-{providerName}`, keyed by the replica's Pod name. Replicas use **server-side apply** with per-replica field managers (field manager name = Pod name), so each replica owns only its own key. This eliminates optimistic concurrency conflicts between replicas writing simultaneously. The ConfigMap data structure is:

```yaml
data:
  # Each key is a gateway Pod name; value is JSON with the budget period and per-namespace spend.
  # The "period" field is required so the reconciler can exclude stale entries from prior periods
  # during rollover — replicas transition to the new period independently on their first request,
  # so mixed-period entries are expected in the rollover window.
  agentry-gateway-0: '{"period": "2026-04", "team-support": "142.50", "team-ml": "87.30"}'
  agentry-gateway-1: '{"period": "2026-04", "team-support": "138.20", "team-ml": "91.10"}'
  _canonical: '{"team-support": "280.70", "team-ml": "178.40"}'
```

The ModelProviderReconciler reads this ConfigMap every 30s, **filters out any per-replica entries whose `period` does not match the current period**, sums the remaining partials, writes the `_canonical` key with the total, and updates `status.budgetUsage` on the ModelProvider. Gateway replicas read the `_canonical` key on startup to initialize their local counters. This avoids a Prometheus dependency and works with existing ConfigMap RBAC.

**Period tag rationale**: at period rollover (midnight UTC), gateway replicas detect the new period on their first incoming request and reset their local counter to zero. Because replicas transition independently, there is a window where some replicas have written new-period partials and others still hold old-period totals. Without the `period` field, the reconciler would sum mixed values and produce an incorrect canonical total. By tagging each entry, the reconciler skips old-period entries until all replicas have transitioned, giving a correct (if slightly underestimated) total during the rollover window — which is acceptable for a soft guardrail.

**Budget state on crash**: if all gateway replicas crash simultaneously, up to 10s of spend data (the partial-write interval) may be lost. This is acceptable given that budgets are soft limits — the bounded loss is small relative to typical budget thresholds. Gateway replicas re-initialize from the `_canonical` ConfigMap value on restart.

This means budget enforcement is **approximate under high concurrency** — replicas can collectively overspend before the next reconcile cycle. The overspend is bounded by: `number_of_replicas x max_calls_per_second_per_replica x cost_per_call x reconcile_interval_seconds`. For typical deployments (2-3 replicas, 1 call/sec peak per replica, $0.01-0.10 per call, 30s interval), the maximum overspend per cycle is roughly $0.60-$9.00, which is acceptable for a soft guardrail.

**Agentry's budget feature is spend visibility and soft guardrails, not a hard financial cap.** This is an explicit design decision, not a limitation to be fixed. Teams requiring hard caps should use provider-level account limits in addition to Agentry's per-namespace guardrails.

**Budget period rollover**: budget periods roll over at midnight UTC. Each gateway replica detects the period change on its first request of the new period and resets its local counter, writing a new-period entry (with the updated `period` field) to its ConfigMap key. The reconciler, filtering by the current period, excludes old-period entries during the rollover window — the canonical total may be temporarily underestimated until all replicas have written new-period entries, which is acceptable for soft guardrails. Once the new period is fully established, the controller archives the previous period's totals to ModelProvider status for auditability, deletes all per-replica keys from the budget ConfigMap, and writes a fresh `_canonical: {}`.

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

1. Verify the fallback provider has the **same `spec.type`** as the primary provider (e.g., both `anthropic`, or both `openai-compatible`). If the types differ, the fallback is skipped. This constraint exists because the gateway does not translate between API formats — see [Request Format Detection](#request-format-detection) above.
2. Verify the namespace is in the fallback provider's `allowedNamespaces`.
3. Verify the requested model exists in the fallback provider's `models`.
4. Check the fallback provider's budget state for the agent's namespace. If the fallback is budget-blocked, skip it and try the next fallback in the list (or return an error if no more fallbacks remain).
5. Forward the request with the fallback provider's credentials.

Fallback chains up to a configurable depth. If provider A's fallback is B, and B has a fallback to C, the gateway tries A -> B -> C. The maximum depth is controlled by the gateway-level `maxFallbackDepth` setting (default: 3), configured via the Helm value `gateway.maxFallbackDepth`, which sets the `AGENTRY_MAX_FALLBACK_DEPTH` environment variable on the gateway Deployment. If the chain is exhausted or the depth cap is reached without a successful response, the gateway returns an error. The depth cap prevents unbounded latency from long fallback chains. Circular references are rejected at reconcile time by the [ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler), so the gateway does not need runtime cycle detection.

**Same-type constraint**: this is validated at reconcile time by the ModelProviderReconciler. Each provider in the fallback chain must have the same `spec.type` as the primary provider (e.g., all `anthropic` or all `openai-compatible`). A ModelProvider with `type: anthropic` cannot list a fallback with `type: openai`. Cross-format fallback may be considered for a future version if there is sufficient demand, but the translation surface area (streaming, tool use, system prompts, multimodal content) is large and error-prone.

---

## Rate Limiting

Rate limits are enforced at the gateway using token-bucket limiters keyed on (namespace, model). Limits come from `ModelProvider.spec.rateLimits` and represent **cluster-wide ceilings**. When a limit is hit, the gateway returns HTTP 429 with a `Retry-After` header.

Each gateway replica divides the configured limit by the number of active gateway replicas (discovered from its Pod informer — count Pods matching the gateway label selector). When replicas scale up or down, each replica adjusts its local token bucket capacity on the next refill cycle. This means the configured value directly represents the intended cluster-wide rate limit regardless of replica count.

**Note:** because each replica enforces its share independently, the effective cluster-wide limit is approximate — transient bursts may slightly exceed the configured ceiling. The approximation is bounded by `configured_limit / number_of_replicas` per replica (one replica's full bucket) and is acceptable for v1.

**Worst-case deviation during scaling events**: during scale-up, existing replicas immediately divide by N+1 when the new Pod appears in their informer — before the new replica begins serving traffic — momentarily reducing each existing replica's effective limit. During rolling restarts (`maxUnavailable: 1`), different replicas can transiently hold different bucket sizes, causing the effective cluster-wide ceiling to deviate by up to one replica's share. If tighter enforcement is required, replace per-replica division with a shared ConfigMap-backed token bucket (the `agentry-budget-{providerName}` ConfigMap already provides the coordination primitive).

---

## Gateway Error Responses

When the gateway cannot fulfill an LLM request, it returns a structured error response so agents can handle failures programmatically (e.g., scenario S10 — graceful degradation on budget exhaustion). See [API_ENDPOINTS.md](./API_ENDPOINTS.md#llm-gateway-error-responses) for the full error schema and status code mapping.

---

## Upstream TLS Configuration

The gateway always connects to upstream LLM providers over HTTPS. For enterprise environments that require custom CA bundles or HTTP proxies for outbound traffic, the gateway supports:

- **Custom CA bundle**: a ConfigMap in `agentry-system` (`agentry-upstream-ca`) containing additional CA certificates. The gateway loads these on startup and watches for changes. All upstream HTTPS connections trust both the system CA bundle and the custom bundle.
- **HTTP proxy**: standard `HTTPS_PROXY` / `NO_PROXY` environment variables on the gateway Deployment. The gateway respects these for all upstream provider calls.

These are gateway-level settings (not per-ModelProvider) because they typically reflect cluster-wide network infrastructure.

---

## Observability

The gateway exposes Prometheus metrics on `:9090/metrics`:

- `agentry_llm_requests_total{provider,model,namespace,status}`
- `agentry_llm_request_duration_seconds{provider,model}`
- `agentry_llm_tokens_total{provider,model,namespace,direction}` (direction = input|output)
- `agentry_llm_spend_usd_total{provider,namespace}`
- `agentry_llm_fallback_total{from_provider,to_provider,reason}`
- `agentry_llm_budget_utilization{provider,namespace,period}` (gauge, 0-1)

For User Gateway metrics, see [GATEWAY_USER.md](./GATEWAY_USER.md#observability).

---

## Failure Modes

| Failure | Behavior |
|---|---|
| Gateway replica crashes | Other replicas continue; Kubernetes restarts the crashed replica |
| All gateway replicas down | LLM calls from agents fail; up to 10s of spend data may be lost (see [Budget State Management](#budget-state-management)) |
| Provider API down | Fallback chain walked (same-type providers only, up to `maxFallbackDepth` depth); if all providers in the chain fail, request fails with `502 provider_error` |
| Budget exhausted | Request blocked or degraded per policy; Warning event emitted on ModelProvider |
