# Budgets and Rate Limits

The LLM gateway enforces two per-namespace controls on LLM traffic: **budgets** (how much a namespace may spend in a period) and **rate limits** (how fast a namespace may call a model). Both are enforced in the gateway itself, because the gateway is the single choke point every LLM request already passes through.

Both controls are deliberately **approximate**. Read the two sections below with that framing in mind: the design trades exactness for the absence of a distributed coordination layer, and both sections state the exact bound on the resulting error so you can decide whether that trade works for your deployment.

## Budget State Management

Budget counters are maintained **in-process in the gateway**. Because the gateway is the single choke point for all LLM traffic, there is no need for a separate aggregator or distributed counter.

Each gateway replica maintains an in-memory spend counter per (provider, namespace, period) tuple. On startup, each replica reads the current period's spent value from the canonical ConfigMap managed by the [ModelProviderReconciler](../../controller/reconcilers.md#modelproviderreconciler). On each LLM call, the counter is updated synchronously.

### The budget counter exchange

Replicas do not talk to each other directly. They exchange spend through a ConfigMap, and the reconciler acts as the reducer over what they write.

Each gateway replica periodically (every 10s) writes its partial spend counters to a ConfigMap in `agentry-system` named `agentry-budget-{providerName}`, keyed by the replica's Pod name. Replicas use **server-side apply** with per-replica field managers (field manager name = Pod name), so each replica owns only its own key. This eliminates optimistic concurrency conflicts between replicas writing simultaneously.

The ConfigMap data structure is:

```yaml
data:
  # Each key is a gateway Pod name; value is JSON with the budget period and per-namespace spend.
  # The "period" field is required so the reconciler can exclude stale entries from prior periods
  # during rollover. Replicas transition to the new period independently on their first request,
  # so mixed-period entries are expected in the rollover window.
  agentry-gateway-0: '{"period": "2026-04", "team-support": "142.50", "team-ml": "87.30"}'
  agentry-gateway-1: '{"period": "2026-04", "team-support": "138.20", "team-ml": "91.10"}'
  _canonical: '{"team-support": "280.70", "team-ml": "178.40"}'
```

The two roles in this ConfigMap are worth naming precisely:

- **Per-replica keys** (`agentry-gateway-0`, `agentry-gateway-1`) are written by the replicas, each owning exactly one key via its own field manager. They are partials: one replica's view of its own spend.
- **`_canonical`** is written only by the reconciler. It is the durable roll-up.

The ModelProviderReconciler reads this ConfigMap on each reconcile pass (event-driven plus the controller's 5-minute periodic requeue per [Reconcile Interval and Performance](../../controller/overview.md#reconcile-interval-and-performance)), **filters out any per-replica entries whose `period` does not match the current period**, sums the remaining partials, writes the `_canonical` key with the total, and updates `status.budgetUsage` on the ModelProvider. Gateway replicas read the `_canonical` key on startup to initialize their local counters. This avoids a Prometheus dependency and works with existing ConfigMap RBAC.

### Cross-replica enforcement view

After startup, each replica **watches the budget ConfigMap** (via the `agentry-system` ConfigMap informer it already holds) and folds the other replicas' current-period partials into its enforcement view on every change. The spend value a replica enforces budget policies against is therefore its own live in-memory counter plus every peer's most recently written partial, at most one 10-second write interval stale.

This is load-bearing: if replicas only read `_canonical` at startup, a long-lived replica would never observe peer spend and enforcement drift would grow unbounded within a budget period. The reconciler's `_canonical` write remains the durable roll-up for status reporting and replica (re)starts; per-request enforcement never waits on it.

### Period tag rationale

At period rollover (midnight UTC), gateway replicas detect the new period on their first incoming request and reset their local counter to zero. Because replicas transition independently, there is a window where some replicas have written new-period partials and others still hold old-period totals.

Without the `period` field, the reconciler would sum mixed values and produce an incorrect canonical total. By tagging each entry, the reconciler skips old-period entries until all replicas have transitioned, giving a correct (if slightly underestimated) total during the rollover window, which is acceptable for a soft guardrail.

### Budget state on crash

If all gateway replicas crash simultaneously, up to 10s of spend data (the partial-write interval) may be lost. This is acceptable given that budgets are soft limits: the bounded loss is small relative to typical budget thresholds. Gateway replicas re-initialize from the `_canonical` ConfigMap value on restart.

### The overspend bound

This means budget enforcement is **approximate under high concurrency**. Replicas can collectively overspend within a partial-write window, since each replica sees peer spend at most ~10s stale (the partial-write interval). The overspend is bounded by:

```
number_of_replicas x max_calls_per_second_per_replica x cost_per_call x partial_write_interval_seconds
```

For typical deployments (2-3 replicas, 1 call/sec peak per replica, $0.01-0.10 per call, 10s partial-write interval), the maximum overspend per window is roughly $0.20-$3.00, which is acceptable for a soft guardrail.

**Streaming widens this window.** Streamed usage lands on the counters only after the stream completes (see [Streaming Responses](request-handling.md#streaming-responses)), so an in-flight stream's cost is invisible to every replica, including the one serving it, for the stream's full duration, often 30 to 120s rather than 10s. The bound therefore carries an additional term:

```
+ number_of_replicas x concurrent_streams_per_replica x cost_per_call
```

Streaming-heavy namespaces should size their soft-guardrail slack from that larger figure.

**Agentry's budget feature is spend visibility and soft guardrails, not a hard financial cap.** This is an explicit design decision, not a limitation to be fixed. Teams requiring hard caps should use provider-level account limits in addition to Agentry's per-namespace guardrails.

### Budget period rollover

Budget periods roll over at midnight UTC. Each gateway replica detects the period change on its first request of the new period and resets its local counter, writing a new-period entry (with the updated `period` field) to its ConfigMap key. The reconciler, filtering by the current period, excludes old-period entries during the rollover window: the canonical total may be temporarily underestimated until all replicas have written new-period entries, which is acceptable for soft guardrails.

Once the new period is fully established, the controller:

1. Archives the previous period's totals to ModelProvider status for auditability.
2. Deletes all per-replica keys from the budget ConfigMap.
3. Writes a fresh `_canonical: {}`.

### Stale replica cleanup

When a gateway replica is scaled down or replaced, its entry in the budget ConfigMap persists. Server-side apply gives each replica ownership of its own key, but nothing reclaims that key when the replica goes away.

The ModelProviderReconciler cross-references ConfigMap keys against the current set of gateway Pod names and deletes stale entries before summing partials. This prevents inflated spend totals from terminated replicas.

## Rate Limiting

Rate limits are enforced at the gateway using token-bucket limiters keyed on (namespace, model). Limits come from `ModelProvider.spec.rateLimits` and represent **cluster-wide ceilings**. When a limit is hit, the gateway returns HTTP 429 with a `Retry-After` header.

### Dividing by live replica count

Each gateway replica divides the configured limit by the number of active gateway replicas (discovered from its Pod informer: count Pods matching the gateway label selector). When replicas scale up or down, each replica adjusts its local token bucket capacity on the next refill cycle. This means the configured value directly represents the intended cluster-wide rate limit regardless of replica count.

**Note:** because each replica enforces its share independently, the effective cluster-wide limit is approximate. Transient bursts may slightly exceed the configured ceiling. The approximation is bounded by `configured_limit / number_of_replicas` per replica (one replica's full bucket) and is acceptable for v1.

### Worst-case deviation during scaling events

During scale-up, existing replicas immediately divide by N+1 when the new Pod appears in their informer, before the new replica begins serving traffic, momentarily reducing each existing replica's effective limit.

During rolling restarts (`maxUnavailable: 1`), different replicas can transiently hold different bucket sizes, causing the effective cluster-wide ceiling to deviate by up to one replica's share.

If tighter enforcement is required, replace per-replica division with a shared ConfigMap-backed token bucket (the `agentry-budget-{providerName}` ConfigMap already provides the coordination primitive).

## Related

- [ModelProviderReconciler](../../controller/reconcilers.md#modelproviderreconciler): the reducer that sums partials, prunes stale keys, and writes `_canonical` and `status.budgetUsage`.
- [ModelProvider](../../resources/modelprovider.md): where `spec.rateLimits` and the budget configuration are declared.
- [Streaming Responses](request-handling.md#streaming-responses): why streamed spend is invisible until the stream completes.
- [Gateway Error Responses](../api/errors.md#llm-gateway-error-responses): the structured error schema and status code mapping, including budget exhaustion and 429 responses.
- [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions): the ConfigMap RBAC the budget exchange relies on.
