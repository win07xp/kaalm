# LLM Gateway operations

This page covers what it takes to run the LLM Gateway: when a replica is considered ready to serve traffic, what it reports to Prometheus, and how it behaves when something breaks.

## Gateway error responses

When the gateway cannot fulfill an LLM request, it returns a structured error response so agents can handle failures programmatically (scenario S10, graceful degradation on budget exhaustion, for example). See [Error reference](../api/errors.md#llm-gateway-error-responses) for the error schema and status code mapping.

---

## Gateway readiness

A gateway replica that is listening but has not hydrated its caches answers real requests wrongly: spurious `404`, `403`, or `invalid_request` responses caused by lookups against an empty cache, not by anything the caller did. The readiness probe keeps such a replica out of the Service until it can answer correctly.

The probe is `GET /readyz` on the internal health port (`:8081` by default, Helm value `gateway.healthPort`). That port serves TLS with no client auth and exposes only `/healthz` and `/readyz`. The probe runs four checks concurrently, bounded to 500ms as a whole, so it answers within the kubelet's default 1s probe timeout. Each check adds one line to the response body: its name, then `ok` or the failure. The response is `200` only when **all** of the following checks pass, otherwise `503`:

1. **`cluster_listener`**: the cluster listener on `:8443` is bound and accepting TLS connections. The probe confirms this with a local TLS handshake.
2. **`user_listener`**: the user listener on `:8080` is bound and accepting TLS connections. Both listeners use the `kaalm-gateway-tls` certificate.
3. **`informers`**: every informer cache the request path depends on has completed its initial sync. At startup, the gateway waits for every one of them before it opens the cluster, user, and health listeners.
4. **`serving_cert`**: the gateway serving certificate (`kaalm-gateway-tls`) loads from disk. The chart mounts the Secret as a required volume, so the Pod doesn't start until cert-manager has issued it, and the gateway exits if the certificate can't be loaded at startup. If the file later becomes unreadable, this check fails; the health listener keeps serving TLS with the last certificate it loaded, so the probe still gets an answer that names the failure.

### The informers the request path depends on

Each cache in check 3 backs a specific step of request handling:

| Informer | What the request path uses it for |
|---|---|
| `Pod` | Source-IP to namespace resolution |
| `Agent` | Provider-routing workload lookup by SAN name, hibernation-state checks |
| `AgentTask` | Provider-routing workload lookup by SAN name, hibernation-state checks |
| `AgentClass` | The `allowedNamespaces` and `allowedProviders` gates in the mTLS-tier routing chain |
| `AgentChannel` | Channel lookup by path through the `spec.path` field index, for webhook, Discord, and WhatsApp channels |
| `ModelProvider` | Model validation, `allowedNamespaces`, fallback chain traversal |
| `ToolProvider` | The MCP broker's `/v1/mcp/{toolProvider}` lookup |

Until every cache is synced, namespace identification, provider routing, channel routing, and tool-provider resolution would fail or return spurious responses.

### Probe failure behavior

Any single failure returns `503 Service Unavailable` with a body listing which checks failed. Kubernetes retries the probe per the Pod's `readinessProbe.periodSeconds` (10s in the chart) until the gateway is ready, which keeps the Pod out of the Service's endpoints during the startup window. The same checks feed the "Gateway replica not ready" row in [Failure modes](#failure-modes).

---

## Observability

The gateway exposes Prometheus metrics on `:9090/metrics`:

- `kaalm_llm_requests_total{provider,model,namespace,status}` (status = ok|error|rate_limited|client_closed, one increment per request; the values are listed after this list)
- `kaalm_llm_request_duration_seconds{provider,model}` (forwarded requests only, stream relay included, labeled with the provider that answered; local denials such as rate limiting and budget blocks are not observed)
- `kaalm_llm_tokens_total{provider,model,namespace,direction}` (direction = input|output)
- `kaalm_llm_spend_usd_total{provider,namespace}`
- `kaalm_llm_fallback_total{from_provider,to_provider,reason}`
- `kaalm_llm_budget_utilization{provider,namespace,period}` (gauge, 0-1)
- `kaalm_budget_threshold_events_total{provider,namespace,action}` (action = warn|degrade|block; one increment per request the budget ladder acted on)
- `kaalm_llm_budget_boundary_events_total{provider,namespace,event}` (event = engaged|throttled|fail_closed|margin_raised; emitted only by hard-enforcement providers, see [Hard enforcement](budgets-and-rate-limits.md#hard-enforcement))
- `kaalm_llm_server_tool_use_total{provider,namespace,tool}` (provider-side tool calls extracted from response usage, such as `web_search`; see [the tool plane](../tool-plane.md#provider-side-tools))
- `kaalm_llm_usage_missing_total{provider,model}` (successful responses, streamed or not, that carried no usage and settled at zero spend; see [Streaming responses](request-handling.md#streaming-responses))

The `status` values of `kaalm_llm_requests_total`:

- `ok`: a 2xx relayed in full.
- `error`: the gateway or the provider failed the request, including a stream the provider broke or that went idle partway ([Streaming responses](request-handling.md#streaming-responses)).
- `rate_limited`: the gateway's rate limit refused the request.
- `client_closed`: the caller disconnected before a stream finished.

A streamed request is counted when its relay ends, so its `status` reflects how the stream ended: a stream that ended in an error event counts as `error`, not `ok`. `client_closed` is not a wire error type. No caller receives it, so it is not in the [error vocabulary](../api/errors.md).

The share of requests with `status!="ok"`, which the provider dashboard's Error ratio panel plots, includes `rate_limited` and `client_closed`. Select `status="error"` to see provider failures alone, so an alert on provider health does not fire whenever agents abandon streams early.

`kaalm_llm_budget_utilization` is the namespace's share of the provider's per-namespace ceiling, uncapped above 1. A provider without a per-namespace ceiling reports no series. Every replica reports the same ratio to within one publish interval, so dashboards aggregate it with `max`, not `sum`.

For User Gateway metrics, see [User Gateway operations](../user/operations.md#observability).

---

## Failure modes

| Failure | Behavior |
|---|---|
| Gateway replica crashes | Other replicas continue; Kubernetes restarts the crashed replica |
| All gateway replicas down | LLM calls from agents fail; up to 10s of spend data may be lost (see [Budget state management](budgets-and-rate-limits.md#budget-state-management)) |
| Gateway replica not ready (a listener isn't accepting connections, a dependent informer hasn't synced, or the serving certificate can't be loaded) | Readiness probe returns 503; replica excluded from Service endpoints until all checks pass. See [Gateway readiness](#gateway-readiness) |
| Provider API down | Fallback chain walked (same-type or translatable-format providers, up to `maxFallbackDepth` attempts); if all providers in the chain fail, the request fails with `502 provider_error`, or `503` / `504` when every attempt was unreachable or timed out. See [Depth cap semantics](fallback.md#depth-cap-semantics) |
| Budget exhausted | Request blocked (`429 budget_exhausted` with `Retry-After`) or degraded per policy. A block also gives each Agent in the namespace that lists that provider a `Degraded` condition with `reason=BudgetExhausted` and a Warning event ([Event emission](../../controller/operations.md#event-emission)) |
| `TokenReview` apiserver unreachable (mode 2 only) | Requests that miss the token cache get `503 internal_unavailable`; mTLS and cached-token requests are unaffected. See [Mode 2](workload-identity.md#mode-2-serviceaccount-bearer-token) |
