# Budgets and rate limits

The LLM gateway enforces two per-namespace controls on LLM traffic: **budgets** (how much a namespace may spend in a period) and **rate limits** (how fast a namespace may call a model). Both are enforced in the gateway itself, because every LLM request already passes through it.

Rate limits, and budgets in their default **soft** mode, are approximate: the design trades exactness for the absence of a distributed coordination layer, and each section states the bound on the resulting error so you can decide whether that trade fits your deployment. Budgets also offer an opt-in **hard** mode ([Hard enforcement](#hard-enforcement)) that pays for a stated spend guarantee with serialized admission near the ceiling.

## Budget state management

Budget counters live in the gateway process. Each replica keeps an in-memory spend counter per (provider, namespace, period) tuple and updates it synchronously on every LLM call. Replicas never talk to each other. They exchange spend through a ConfigMap in `kaalm-system` named `kaalm-budget-{providerName}`, and the [ModelProviderReconciler](../../controller/reconcilers.md#modelproviderreconciler) is the reducer over what they write. This avoids a Prometheus dependency and works with the gateway's existing ConfigMap RBAC ([Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions)).

### The budget counter exchange

Five data movements make up the exchange. The first four run in every replica; the last runs in the reconciler.

| Step | Actor | When | Reads | Writes |
|---|---|---|---|---|
| Seed | replica | Startup, once | `_canonical` | Its in-memory counter |
| Count | replica | Every LLM call | Nothing | Its in-memory counter |
| Publish | replica | Every 10s, and immediately on settle inside the hard-mode boundary region | Nothing | Its own key, by server-side apply |
| Fold | replica | Every ConfigMap watch event, with the 10s tick as a backstop | Peers' current-period keys and `_retired` | Its enforcement view |
| Reduce | reconciler | Every reconcile pass: event-driven, plus a requeue at the provider's health-check interval (60s by default) | Every key | `_retired`, `_canonical`, `status.budgetUsage` |

![Sequence diagram of the replica side of the exchange. At startup a replica reads _canonical to seed its counter. On every LLM call it increments the in-memory counter. Every 10 seconds each replica server-side-applies its own key. On every ConfigMap watch event a replica folds its peers' partials plus _retired into its enforcement view.](../../diagrams/budget-exchange-publish.svg)

Each replica publishes with server-side apply under a field manager named after its own Pod, so it writes exactly one key and simultaneous writes never conflict:

```yaml
data:
  kaalm-gateway-0: '{"period": "2026-04", "team-support": "142.50", "team-ml": "87.30"}'
  kaalm-gateway-1: '{"period": "2026-04", "team-support": "138.20", "team-ml": "91.10"}'
  _retired: '{"period": "2026-04", "team-support": "12.10"}'
  _canonical: '{"team-support": "292.80", "team-ml": "178.40"}'
```

The ConfigMap holds three kinds of key:

- **Per-replica keys** (`kaalm-gateway-0`, `kaalm-gateway-1`) are partials: one replica's view of its own spend, tagged with the `period` it belongs to. Inside a partial, underscore-prefixed fields (for example `_marginExceeded`, see [Hard enforcement](#hard-enforcement)) are flags, never spend. Parsers skip them, and flag values are never bare numbers, so an older parser cannot mistake one for a namespace total.
- **`_retired`** is written only by the reconciler. When it prunes a terminated replica's current-period key, that key's totals fold into this period-tagged accumulator first, so spend a dead replica already published is never erased. Replicas fold `_retired` into their enforcement view like a peer partial.
- **`_canonical`** is written only by the reconciler. It is the durable roll-up, including `_retired`.

### Cross-replica enforcement view

The value a replica enforces against is its own live counter, plus every peer's most recently published partial, plus `_retired`. Freshness is bounded by how often peers publish, not by how often replicas fold: at most one 10-second publish interval under soft enforcement, and one watch propagation under hard enforcement, where settles inside the boundary region publish immediately. The fold is what keeps replicas converged. A replica that only read `_canonical` at startup would never observe peer spend, and drift would grow without bound within a period.

`_canonical` is not on the enforcement path. It is the roll-up the reconciler writes for status reporting and for the seed at replica start, and a replica reads it exactly once, at startup. Per-request enforcement never waits on the reconciler.

### The reducer

![Sequence diagram of one reconcile pass. The reconciler reads every key from the ConfigMap, archives and deletes old-period keys, folds dead replicas' current-period keys into _retired and deletes them, sums the current partials plus _retired, writes _retired and _canonical back, and updates ModelProvider status with budgetUsage and the previous-period archive.](../../diagrams/budget-exchange-reduce.svg)

On every pass the reconciler reads every key and handles each by its `period` tag and by whether a live gateway Pod of that name exists, from the controller's gateway-Pod informer:

- A current-period key from a live replica is summed.
- A current-period key from a replica that no longer exists is folded into `_retired`, then deleted. Deleting the key must not delete the spend it recorded. Under soft enforcement the fold prevents a small bounded undercount per rollout; under hard enforcement it keeps the cap intact, because a rolling restart that erased each replaced replica's published spend would void the ceiling once per rollout.
- An old-period key, from any replica and including `_retired`, is archived into the previous-period entry of `ModelProvider.status` and deleted.

The pass then writes `_retired` and `_canonical` (the sum of current-period partials plus `_retired`) and updates `status.budgetUsage`.

**Period rollover.** Periods roll over at midnight UTC; [Budget accounting](../../resources/modelprovider.md#budget-accounting) lists the daily, weekly, and monthly boundaries. Each replica detects the new period on its first request of the period, resets its counter, and publishes a new-period partial under its key. Replicas transition independently, so the ConfigMap holds mixed-period entries for a window, and the `period` tag is what lets the reducer archive the old entries instead of summing them into the new period. Until every replica has published a new-period partial, `_canonical` is underestimated, which is acceptable for a soft guardrail.

### Budget state on crash

If all gateway replicas crash at once, up to 10s of spend, one publish interval, is lost. Under soft enforcement the loss is small relative to typical budget thresholds. Replicas re-seed from `_canonical` on restart and fold live partials on top. [Hard enforcement](#hard-enforcement) tightens both the loss bound and the restart behavior.

### The overspend bound

Soft budget enforcement is approximate under high concurrency: each replica sees peer spend up to one publish interval stale, so replicas can collectively overspend within that window. The overspend is bounded by:

```
number_of_replicas x max_calls_per_second_per_replica x cost_per_call x partial_write_interval_seconds
```

For typical deployments (2 to 3 replicas, 1 call/sec peak per replica, $0.01 to $0.10 per call, 10s publish interval), the maximum overspend per window is roughly $0.20 to $3.00.

**Streaming widens this window.** Streamed usage lands on the counters only after the stream completes (see [Streaming responses](request-handling.md#streaming-responses)), so an in-flight stream's cost is invisible to every replica, including the replica serving it, for the stream's full duration, often 30 to 120s rather than 10s. The bound therefore carries an additional term:

```
+ number_of_replicas x concurrent_streams_per_replica x cost_per_call
```

Streaming-heavy namespaces should size their soft-guardrail slack from that larger figure.

Soft enforcement is spend visibility and guardrails, not a financial cap. Its design point is zero coordination cost on the request path, with the overspend bounded and quantified above. Teams that need a cap opt in to [hard enforcement](#hard-enforcement), which states what it guarantees and what it costs. Provider-level account limits remain sound defense in depth under either mode.

## Per-workload spend

The ledger also answers "which agent spent it". Beside the per-namespace enforcement counters, each replica accumulates per-workload spend keyed `{namespace}/{workload}`, where the workload is the attested `agent/{name}` or `task/{name}` from the caller's certificate SAN, or the visible `(unattributed)` bucket for gateway-only-tier callers, which authenticate by token and carry no workload identity. Keeping that bucket visible is what makes the per-workload rows always sum to the namespace figure. Spend lands at the same single settle point the namespace counters use, under the same mutex, and rolls over with the same period reset. The admission math never reads the workload maps, so hard enforcement is unaffected by construction.

Persistence uses the same exchange in a second object, `kaalm-agentspend-{provider}`: each replica publishes its workload partial with the same server-side-apply one-key-per-replica pattern and the same period tag, folds peers back on the tick and on the ConfigMap watch, and seeds from `_canonical` at startup. The reducer mirrors the budget reducer: a pruned replica's current-period partial folds into `_retired` before its key is deleted, old periods drop, and `_canonical` carries live plus retired. The breakdown has its own ConfigMap for two reasons. Safety: the budget fold sums every non-underscore key in the budget ConfigMap as namespace spend, so workload keys inside it would silently corrupt utilization. Capacity: both objects live under the object cap of about 1 MiB, and at the design target of 1000+ agents the workload keys need the room.

Three deliberate boundaries:

- The breakdown keeps the **current period only**. The namespace figures archive one prior generation in `ModelProvider.status`; the breakdown does not, and it never enters provider status at all, because namespaces times workloads would grow the CR against the same object cap.
- It never becomes a metric label. Per-workload resolution lives in the [console read API](../../console/overview.md) through the gateway's [GET /v1/spend](../api/internal-endpoints.md#get-v1spend), which any single replica answers from its folded union, current to within one publish interval.
- It is priced spend: calls to unpriced models cost zero and do not appear, the same soft-guardrail behavior the namespace figures have.

## Hard enforcement

Setting `spec.budget.enforcement: hard` on a ModelProvider (default `soft`) turns that provider's `action: block` thresholds into a cap with a stated guarantee. Nothing else changes: `warn` and `degrade` policies remain advisory in both modes, both ceilings (`perNamespaceUSD` and `clusterUSD`) participate through the same worse-of-two utilization, and outside the boundary region described below the code path is the soft path. Three validation rules gate the mode: hard requires at least one `block` policy (rule 32), every catalog model priced (rule 33, because an unpriced call settles at zero and a cap cannot count spend it never prices), and a coherent boundary margin (rule 34). See [Cross-resource validation](../../resources/validation-and-defaulting.md#cross-resource-validation).

### The boundary region

A hard cap cannot be enforced by after-the-fact counting alone: by the time a settled cost lands on the counters, the spend has happened. Instead of estimating request costs up front, which is not possible in general and least of all for streams, hard mode changes behavior only inside a **boundary region** immediately below each `block` threshold, where the remaining headroom is small enough that uncoordinated concurrency could cross the ceiling.

The region starts `boundaryMarginPercent` percentage points below each `block` policy's `atPercent` (the knob under `budget.hard`, default 5). The configured value is a floor. Each replica also computes a margin from what it has observed this period, and the **effective margin** is the larger of the two:

```
marginUSD = replicas x maxObservedCostPerCall
          + (replicas - 1) x observedPeakSpendRatePerSec x stalenessWindowSeconds

effectiveMarginPercent = max(boundaryMarginPercent, 100 x marginUSD / ceilingUSD)
```

The first term covers one unsettled in-flight request per replica; the second covers spend a peer has settled but not propagated. The observed inputs are maintained per provider within the period: the running maximum settled cost of a single call, and the peak spend rate over 10-second buckets, monotone within the period and reset at rollover. An early burst therefore widens the margin for the rest of the period, which errs toward throttling earlier, the safe direction. The replica count includes not-yet-Ready gateway Pods, also the conservative direction, and deliberate.

When the computed margin exceeds the configured knob, the replica raises the `_marginExceeded` flag in its published partial, and the ModelProviderReconciler surfaces it as a `BoundaryMarginRaised` condition and a Warning event on the ModelProvider: the operator learns the knob is undersized for the observed traffic without the guarantee ever having depended on it. One residual remains that this reactive scheme cannot close: a traffic burst without precedent in the current period can outrun the computed margin in the first staleness window it appears. Sizing the configured knob from the [soft overspend bound](#the-overspend-bound) formula with your own worst-case rates closes that gap with operator knowledge the gateway cannot have.

### Serialized admission

Inside the boundary region, each replica admits **at most one request at a time** per governed ceiling: a per-`(provider, namespace)` slot for the namespace ceiling, and a provider-wide slot when the cluster ceiling is the one in its boundary region, both acquired in the same atomic step. A request that finds the slot held is rejected immediately with `429 budget_throttled` and `Retry-After: 1` ([error schema](../api/errors.md#llm-gateway-error-responses)). There is no queue, deliberately: a queued request holding one provider's slot while waiting on another's (reachable through a fallback chain) can deadlock, and near the ceiling a queue mostly drains into blocks anyway.

Settlement is the other half of the invariant. When the admitted request completes, its actual cost lands on the counter and the slot frees in one atomic step, so the next admitted request always sees the previous one's real cost. The settle also publishes the replica's partial immediately instead of waiting for the 10-second tick, which is what shrinks peer staleness to one watch propagation inside the region. A held slot stays authoritative even if a fold momentarily drops utilization below the boundary (a peer prune during a rollout, for example): the slot releases only on settle, never on recomputation.

The block decision itself is unchanged from soft mode, with one addition: the `429 budget_exhausted` message names which ceiling fired, `namespace budget exhausted: <ns>` or `cluster budget exhausted`, so a block is attributable at a glance.

### The guarantee

For a hard-mode provider, spend within a period never exceeds a `block` ceiling by more than:

- the actual cost of at most **one in-flight request per replica, per governed ceiling**, plus
- each peer's settled-but-not-yet-propagated spend, bounded by **one settle-publish-to-watch propagation** per peer, typically well under a second.

Three limits are part of the guarantee:

- The gateway can only cap what providers report. A response with no usage metadata settles at zero cost ([Streaming responses](request-handling.md#streaming-responses)); a provider that omits usage evades any gateway-side cap. Rule 33 keeps unpriced *models* out of hard mode, but usage-less *responses* are a provider behavior no proxy can price.
- The margin computation bounds when serialization engages, not what a single admitted request may cost. A single request larger than the remaining headroom is admitted (there is exactly one of it per replica) and its overshoot is the first bullet's bound.
- A simultaneous crash of all replicas loses at most the in-flight requests' costs inside the boundary region (settles publish immediately there), rather than soft mode's 10 seconds of spend.

### Behavior under failure

Hard mode fails closed, but only where the guarantee is live. Below the boundary region, apiserver unavailability degrades hard enforcement to exactly the soft bound: counters keep counting, publishes retry on the tick, requests flow. Inside the boundary region, a replica rejects requests with `503 budget_state_unavailable` when either staleness signal trips:

- **Write path**: it holds settled spend older than the staleness window that it has been unable to publish, so peers may be admitting against a stale view of this replica.
- **Read path**: it has not successfully refreshed its peer view within the staleness window. A dead watch with quiet peers is indistinguishable from silence, so the exchange tick doubles as the liveness probe.

The staleness window is derived, not configurable: three publish intervals, 30 seconds. Recovery is automatic on the first successful publish or fold. A cap that fails open is not a cap, and a cap that fails closed everywhere is an availability hazard. Failing closed only inside the region where the ceiling is at stake is the trade this design picks.

### Restarts and rollover

A restarting replica seeds from `_canonical` and folds live partials on top. Until the reconciler's next pass, the replica's own pre-restart key may still be present alongside `_canonical` totals that already include it, so the enforcement view can transiently overcount. Overcounting blocks early rather than late, the correct failure side for a cap, and it clears within one reconcile. Rolling restarts do not erase published spend: the reconciler folds pruned keys into `_retired` before deleting them (see [The reducer](#the-reducer)).

At period rollover, counters, slots, and the observed-traffic tracker all reset. A request admitted before midnight settles into the new period (the same attribution soft mode gives a midnight-spanning call), the boundary flag drops with the first new-period publish, and the margin collapses back to the configured knob until new observations accrue.

### Streaming under hard enforcement

A streaming request admitted in the boundary region holds its admission slot for the stream's full duration, since its cost settles only when the stream completes. The upstream client timeout (default 120 seconds) bounds the hold. Near the cap, streams serialize, and a long stream makes its neighbors wait out `budget_throttled` retries for its whole duration. That is the cost of a hard cap over pay-per-token streaming, and the reason the region is kept as narrow as the margin math allows.

### Interaction with fallback

A hard-mode primary that is blocked, throttled, or failed closed short-circuits exactly as a soft block does: the fallback walk never starts, because a capped namespace must not drain a fallback provider's budget. Inside a walk, a hard fallback candidate is governed by the same admission rules, consumes an attempt slot, and falls through to its children. A walk exhausted entirely by budget outcomes returns `429 budget_exhausted` (with the largest observed `Retry-After`), or `503 budget_state_unavailable` when fail-closed candidates are why, never `502 provider_error`.

### What hard mode costs

Serialized admission near the ceiling, one ConfigMap write per settle inside the boundary region, and a ConfigMap watch per gateway replica. Plan for the first: budget utilization is monotonic within a period, so a namespace that reaches its boundary region stays in it until rollover, and month-end is exactly when its traffic serializes. Namespaces that need throughput at high utilization should widen their budget, not their margin.

## Rate limiting

Rate limits are enforced at the gateway using token-bucket limiters keyed on (namespace, model). Limits come from `ModelProvider.spec.rateLimits` and represent **cluster-wide ceilings**. When a limit is hit, the gateway returns HTTP 429 with a `Retry-After` header.

### Dividing by live replica count

Each gateway replica divides the configured limit by the number of active gateway replicas, counted from its Pod informer by the gateway label selector and refreshed at most every few seconds rather than on every request. When replicas scale up or down, each replica resizes its local token bucket within that refresh and the next refill cycle, so the configured value represents the intended cluster-wide limit regardless of replica count.

Because each replica enforces its share independently, the effective cluster-wide limit is approximate. Transient bursts may exceed the configured ceiling by up to one replica's full bucket, `configured_limit / number_of_replicas`, the accepted trade for a coordination-free request path.

### Worst-case deviation during scaling events

During scale-up, existing replicas divide by N+1 as soon as the new Pod appears in their informer, before the new replica begins serving traffic, momentarily reducing each existing replica's effective limit. During rolling restarts (`maxUnavailable: 1`), different replicas can transiently hold different bucket sizes, so the effective cluster-wide ceiling deviates by up to one replica's share.

Per-replica division is the design point. A shared token bucket coordinated through a ConfigMap the way the budget exchange is would cost a coordinated write per request, which is why it was not chosen.

## Related

- [ModelProviderReconciler](../../controller/reconcilers.md#modelproviderreconciler): the reducer that sums partials, prunes stale keys, and writes `_canonical` and `status.budgetUsage`.
- [ModelProvider](../../resources/modelprovider.md): where `spec.rateLimits` and the budget configuration are declared.
- [Streaming responses](request-handling.md#streaming-responses): why streamed spend is invisible until the stream completes.
- [LLM Gateway error responses](../api/errors.md#llm-gateway-error-responses): the structured error schema and status code mapping, including budget exhaustion and 429 responses.
- [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions): the ConfigMap RBAC the budget exchange relies on.
