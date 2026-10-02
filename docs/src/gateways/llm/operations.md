# LLM Gateway operations

This page covers what it takes to run the LLM Gateway: when a replica is considered ready to serve traffic, what it reports to Prometheus, and how it behaves when something breaks.

## Gateway error responses

When the gateway cannot fulfill an LLM request, it returns a structured error response so agents can handle failures programmatically (scenario S10, graceful degradation on budget exhaustion, for example). See [Error reference](../api/errors.md#llm-gateway-error-responses) for the full error schema and status code mapping.

---

## Gateway readiness

Readiness is a gate, not a formality. A gateway replica that is listening but has not hydrated its caches would answer real requests with wrong answers: spurious `404`, `403`, or `invalid_request` responses caused by lookups against an empty cache, not by anything the caller did. The readiness probe exists to keep such a replica out of the Service until it can answer correctly.

The probe is `GET /readyz` on the internal health port (`:8081` by default, Helm value `gateway.healthPort`). That port serves TLS with no client auth and exposes only `/healthz` and `/readyz`. The probe runs four checks concurrently, bounded to 500ms as a whole, so it answers within the kubelet's default 1s probe timeout. Each check writes one line to the response body: the check's name, followed by `ok` or the error, for example `cluster_listener: ok` or `informers: not synced: ModelProvider`. The response is `200` only when **all** of the following checks pass, otherwise `503`:

1. **`cluster_listener`**: the cluster listener on `:8443` is bound and accepting TLS connections. The probe completes a local TLS handshake to confirm, then closes the connection.
2. **`user_listener`**: the user listener on `:8080` is bound and accepting TLS connections (both listeners use the `kaalm-gateway-tls` certificate). The probe completes a local TLS handshake to confirm, then closes the connection.
3. **`informers`**: every informer cache the request path depends on has completed its initial sync. The probe reads each informer's `HasSynced` state; at startup, the gateway process itself waits on `WaitForCacheSync` for every one of them before it opens any listener.
4. **`serving_cert`**: the gateway serving certificate (`kaalm-gateway-tls`, mounted at `--tls-cert`/`--tls-key`) loads from disk. The chart mounts the Secret as a required volume, so the Pod doesn't start until cert-manager has issued it, and the gateway process exits if the certificate can't be loaded at startup. If the certificate file later becomes unreadable, this check fails; the health listener keeps serving TLS with the last certificate it loaded successfully, so the probe still gets an answer that names the failure.

Neither TLS handshake check verifies the certificate chain: the connection is over loopback, and the certificate names the Service, not `localhost`.

### The informers the request path depends on

Each cache in check 3 backs a specific step of request handling:

| Informer | What the request path uses it for |
|---|---|
| `Pod` | Source-IP to namespace resolution |
| `Agent` | Provider-routing ownerRef resolution, hibernation-state checks |
| `AgentTask` | Provider-routing ownerRef resolution, hibernation-state checks |
| `AgentClass` | The `allowedNamespaces` and `allowedProviders` gates in the mTLS-tier routing chain |
| `AgentChannel` | Channel lookup by path through the `spec.path` field index, for webhook, Discord, and WhatsApp channels |
| `ModelProvider` | Model validation, `allowedNamespaces`, fallback chain traversal |
| `ToolProvider` | The MCP broker's `/v1/mcp/{toolProvider}` lookup |

Until every cache is synced, namespace identification, provider routing, channel routing, and tool-provider resolution would either fail or return spurious `404` / `403` / `invalid_request` responses while caches hydrate. See [Workload identity](workload-identity.md) for how source-IP and auth-mode resolution use the `Pod` cache.

### Probe failure behavior

Any single failure above returns `503 Service Unavailable` with a body listing which checks failed. Kubernetes retries the probe per the Pod's `readinessProbe.periodSeconds` (default 10s) until the gateway is fully ready, which keeps the gateway Pod out of the Service's endpoints during the startup window. The same checks feed the "Gateway replica not ready" row in [Failure modes](#failure-modes).

Because every check above must pass for the probe to succeed, the Service never receives traffic for a listener that would error at connection time, for a Pod that cannot yet resolve source IPs to namespaces, map ownerRefs to Agents/AgentTasks, look up AgentChannels, resolve a ToolProvider by name, or validate requested models, or for a replica whose serving certificate can't be read.

---

## Observability

The gateway exposes Prometheus metrics on `:9090/metrics`:

- `kaalm_llm_requests_total{provider,model,namespace,status}`
- `kaalm_llm_request_duration_seconds{provider,model}` (forwarded requests only, stream relay included, labeled with the provider that answered; local denials such as rate limiting and budget blocks are not observed)
- `kaalm_llm_tokens_total{provider,model,namespace,direction}` (direction = input|output)
- `kaalm_llm_spend_usd_total{provider,namespace}`
- `kaalm_llm_fallback_total{from_provider,to_provider,reason}`
- `kaalm_llm_budget_utilization{provider,namespace,period}` (gauge, 0-1)
- `kaalm_budget_threshold_events_total{provider,namespace,action}` (action = warn|degrade|block; one increment per request the budget ladder acted on)
- `kaalm_llm_budget_boundary_events_total{provider,namespace,event}` (event = engaged|throttled|fail_closed|margin_raised; emitted only by hard-enforcement providers, see [Hard enforcement](budgets-and-rate-limits.md#hard-enforcement))
- `kaalm_llm_server_tool_use_total{provider,namespace,tool}` (provider-side tool calls extracted from response usage, such as `web_search`; see [the tool plane](../tool-plane.md#provider-side-tools))
- `kaalm_llm_usage_missing_total{provider,model}` (successful responses, streamed or not, that carried no usage and settled at zero spend; see [Streaming responses](request-handling.md#streaming-responses))

On naming: the counters carry the `_total` suffix and `kaalm_llm_budget_utilization` does not, because it is a gauge rather than a monotonic counter. The gauge is computed on every scrape from the replica's folded ledger (own live counter plus peer partials), so every replica reports the same ratio to within one publish interval and dashboards aggregate it with `max`; it is the namespace's share of the provider's per-namespace ceiling, uncapped above 1, and a provider without a per-namespace ceiling reports no series.

For User Gateway metrics, see [User Gateway operations](../user/operations.md#observability).

---

## Failure modes

| Failure | Behavior |
|---|---|
| Gateway replica crashes | Other replicas continue; Kubernetes restarts the crashed replica |
| All gateway replicas down | LLM calls from agents fail; up to 10s of spend data may be lost (see [Budget state management](budgets-and-rate-limits.md#budget-state-management)) |
| Gateway replica not ready (a listener isn't accepting connections, a dependent informer hasn't synced, or the serving certificate can't be loaded) | Readiness probe returns 503; replica excluded from Service endpoints until all checks pass. See [Gateway readiness](#gateway-readiness) |
| Provider API down | Fallback chain walked (same-type or translatable-format providers, up to `maxFallbackDepth` attempts); if all providers in the chain fail, the request fails with a fallback-exhausted error |
| Budget exhausted | Request blocked (`429 budget_exhausted` with `Retry-After` header) or degraded per policy; Warning event emitted on ModelProvider |
| `TokenReview` apiserver unreachable (mode 2 only) | Gateway returns `503 Service Unavailable` to the caller for requests that miss the token cache; mTLS requests and cached-token requests are unaffected |
| CNI does not support FQDN egress policy but AgentClass sets `allowedHosts` | AgentClassReconciler emits a `Warning` event, and no CiliumNetworkPolicy is written, so `allowedHosts` is ignored and `allowedCIDRs` alone governs egress. See [AgentClassReconciler](../../controller/reconcilers.md#agentclassreconciler) |

Details for the rows that need them:

- **Gateway replica not ready**: the dependent informers are `Pod`, `Agent`, `AgentTask`, `AgentClass`, `AgentChannel`, `ModelProvider`, and `ToolProvider`.
- **Provider API down**: the fallback-exhausted error is `502 provider_error`, or `503` / `504` when every attempt was unreachable or timed out. See [Depth cap semantics](fallback.md#depth-cap-semantics).
- **`TokenReview` apiserver unreachable**: the `503` carries `error.type: internal_unavailable`, `retryable: true`, and `Retry-After: 1`. See [LLM Gateway error responses](../api/errors.md#llm-gateway-error-responses).
