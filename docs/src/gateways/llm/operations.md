# LLM Gateway Operations

This page covers what it takes to run the LLM Gateway: when a replica is considered ready to serve traffic, what it reports to Prometheus, and how it behaves when something breaks.

## Gateway Error Responses

When the gateway cannot fulfill an LLM request, it returns a structured error response so agents can handle failures programmatically (e.g., scenario S10, graceful degradation on budget exhaustion). See [Error Reference](../api/errors.md#llm-gateway-error-responses) for the full error schema and status code mapping.

---

## Gateway Readiness

Readiness is a gate, not a formality. A gateway replica that is listening but has not yet hydrated its caches would answer real requests with wrong answers: spurious `404`, `403`, or `invalid_request` responses caused by lookups against an empty cache, not by anything the caller did. The readiness probe exists to keep such a replica out of the Service until it can answer correctly.

The probe is `GET /readyz` on the internal health port (`:8081` by default, Helm value `gateway.healthPort`). That port serves TLS with no client auth and exposes only `/healthz` and `/readyz`. It returns `200` only when **all** of the following are true:

1. The **LLM listener** on `:8443` is bound and accepting TLS connections. The probe performs a local dial to confirm.
2. The **User listener** on `:8080` is bound and accepting TLS connections (both listeners use the `agentry-gateway-tls` certificate). The probe performs a local TLS dial to confirm.
3. **All informer caches** the request path depends on have completed their initial sync (`cache.WaitForCacheSync` returned true for each).
4. The **gateway serving certificate** (`agentry-gateway-tls`) has been loaded from disk. On startup, the gateway reads the mounted Secret; if the Secret does not yet exist (cert-manager has not issued it), readiness fails. This matters on initial chart install, where the Pod may start before cert-manager completes issuance.

### The informers the request path depends on

Each cache in check 3 backs a specific step of request handling:

| Informer | What the request path uses it for |
|---|---|
| `Pod` | Source-IP to namespace resolution |
| `Agent` | Provider-routing ownerRef resolution, hibernation-state checks |
| `AgentTask` | Provider-routing ownerRef resolution, hibernation-state checks |
| `AgentClass` | The `allowedProviders` gate in the mTLS-tier routing chain |
| `AgentChannel` | Webhook path to target Agent lookup |
| `ModelProvider` | Model validation, `allowedNamespaces`, fallback chain traversal |

Until every cache is synced, namespace identification, provider routing, and channel routing would either fail or return spurious `404` / `403` / `invalid_request` responses while caches hydrate. See [Namespace Identification](workload-identity.md) for how source-IP and auth-mode resolution use the `Pod` cache.

### Probe failure behavior

Any single failure above returns `503 Service Unavailable` with a body listing which checks failed. Kubernetes retries the probe per the Pod's `readinessProbe.periodSeconds` (default 10s) until the gateway is fully ready, which keeps the gateway Pod out of the Service's endpoints during the startup window. The same checks feed the "Gateway not ready" row in [Failure Modes](#failure-modes).

Because both listeners and every dependent informer must be green for the probe to pass, the Service never receives traffic for a listener that would error at connection time or for a Pod that cannot yet resolve source IPs to namespaces, map ownerRefs to Agents/AgentTasks, look up AgentChannels, or validate requested models.

---

## Observability

The gateway exposes Prometheus metrics on `:9090/metrics`:

- `agentry_llm_requests_total{provider,model,namespace,status}`
- `agentry_llm_request_duration_seconds{provider,model}`
- `agentry_llm_tokens_total{provider,model,namespace,direction}` (direction = input|output)
- `agentry_llm_spend_usd_total{provider,namespace}`
- `agentry_llm_fallback_total{from_provider,to_provider,reason}`
- `agentry_llm_budget_utilization{provider,namespace,period}` (gauge, 0-1)

Note the naming: the counters carry the `_total` suffix and `agentry_llm_budget_utilization` does not, because it is a gauge rather than a monotonic counter.

For User Gateway metrics, see [User Gateway Operations](../user/operations.md#observability).

---

## Failure Modes

| Failure | Behavior |
|---|---|
| Gateway replica crashes | Other replicas continue; Kubernetes restarts the crashed replica |
| All gateway replicas down | LLM calls from agents fail; up to 10s of spend data may be lost (see [Budget State Management](budgets-and-rate-limits.md#budget-state-management)) |
| Gateway replica not ready (listener dial fails, any of the dependent informers not synced, or cert not yet issued) | Readiness probe returns 503; replica excluded from Service endpoints until all checks pass. See [Gateway Readiness](#gateway-readiness) |
| Provider API down | Fallback chain walked (same-type providers only, up to `maxFallbackDepth` depth); if all providers in the chain fail, the request fails with a fallback-exhausted error |
| Budget exhausted | Request blocked (`429 budget_exhausted` with `Retry-After` header) or degraded per policy; Warning event emitted on ModelProvider |
| `TokenReview` apiserver unreachable (mode 2 only) | Gateway returns `503 Service Unavailable` to the caller for requests that miss the token cache; mTLS requests and cached-token requests are unaffected |
| CNI does not support FQDN egress policy but AgentClass sets `allowedHosts` | AgentClassReconciler emits a `Warning` event and ignores `allowedHosts`; `allowedCIDRs` alone governs egress. See [AgentClassReconciler](../../controller/reconcilers.md#agentclassreconciler) |

Details for the rows that need them:

- **Gateway replica not ready**: the dependent informers are `Pod`, `Agent`, `AgentTask`, `AgentClass`, `AgentChannel`, and `ModelProvider`.
- **Provider API down**: the fallback-exhausted error is `502 provider_error`, or `503` / `504` when every attempt was unreachable or timed out. See [Depth cap semantics](fallback.md#depth-cap-semantics).
- **`TokenReview` apiserver unreachable**: the `503` carries `error.type: internal_unavailable`, `retryable: true`, and `Retry-After: 1`. See [LLM Gateway Error Responses](../api/errors.md#llm-gateway-error-responses).
