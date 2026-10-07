# Budgets and rate limits

The LLM gateway enforces two per-namespace controls on LLM traffic: **budgets** (how much a namespace may spend in a period) and **rate limits** (how fast a namespace may call a model). Both are enforced in the gateway itself, because every LLM request already passes through it.

Rate limits, and budgets in their default **soft** mode, are approximate: the design trades exactness for the absence of a distributed coordination layer, and each section states the bound on the resulting error so you can decide whether that trade fits your deployment. Budgets also offer an opt-in **hard** mode ([Hard enforcement](#hard-enforcement)) that pays for a stated spend guarantee with serialized admission near the ceiling.

## Budget state management

Each gateway replica keeps an in-memory spend counter per (provider, namespace, period) and updates it on every LLM call. Replicas never talk to each other. They exchange spend through a ConfigMap in `kaalm-system` named `kaalm-budget-{providerName}`, and the [ModelProviderReconciler](../../controller/reconcilers/modelprovider.md) is the reducer over what they write. This avoids a Prometheus dependency and works with the gateway's existing ConfigMap RBAC ([Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions)).

### The budget counter exchange

Five data movements make up the exchange. The first four run in every replica; the last runs in the reconciler.

| Step | Actor | When | What it does |
|---|---|---|---|
| Seed | replica | Startup, once | Starts its in-memory counter from `_canonical` |
| Count | replica | Every LLM call | Adds the call's cost to its in-memory counter |
| Publish | replica | Every 10s, and immediately on settle inside the hard-mode boundary region | Writes its own key |
| Fold | replica | Every ConfigMap watch event, with the 10s tick as a backstop | Rebuilds its enforcement view from peers' current-period keys and `_retired` |
| Reduce | reconciler | Every reconcile pass, including a pass where the provider fails a check. Passes are event-driven plus timed requeues ([ModelProviderReconciler Timing](../../controller/reconcilers/modelprovider.md#timing)) | Writes `_retired`, `_canonical`, `_previous`, and `status.budgetUsage` from every key |

![Sequence diagram of the replica side of the exchange. At startup a replica reads _canonical to seed its counter. On every LLM call it increments the in-memory counter. Every 10 seconds each replica server-side-applies its own key. On every ConfigMap watch event a replica folds its peers' partials plus _retired into its enforcement view.](../../diagrams/budget-exchange-publish.svg)

Each replica publishes its own key with server-side apply, so simultaneous writes never conflict:

```yaml
data:
  kaalm-gateway-0: '{"period": "2026-04", "_providerUID": "7d2c1a90-5b3e-4c1a-9d27-0e6f4b8a1c33", "team-support": "142.50", "team-ml": "87.30"}'
  kaalm-gateway-1: '{"period": "2026-04", "_providerUID": "7d2c1a90-5b3e-4c1a-9d27-0e6f4b8a1c33", "team-support": "138.20", "team-ml": "91.10"}'
  _retired: '{"period": "2026-04", "_providerUID": "7d2c1a90-5b3e-4c1a-9d27-0e6f4b8a1c33", "team-support": "12.10"}'
  _canonical: '{"team-support": "292.80", "team-ml": "178.40"}'
  _previous: '{"period": "2026-03", "archivedIn": "2026-04", "providerUID": "7d2c1a90-5b3e-4c1a-9d27-0e6f4b8a1c33", "sources": {"kaalm-gateway-0": {"team-support": "150.00"}, "kaalm-gateway-1": {"team-support": "141.20"}}}'
```

The ConfigMap holds four kinds of key:

- **Per-replica keys** (`kaalm-gateway-0`, `kaalm-gateway-1`) are partials: one replica's view of its own spend, tagged with the `period` it belongs to and the `_providerUID` of the ModelProvider it was written for ([When a provider is deleted](#when-a-provider-is-deleted)). Underscore-prefixed fields inside a partial, such as `_marginExceeded` ([Hard enforcement](#hard-enforcement)) and `_providerUID`, are flags or this tag, never spend. A flag value is never a bare number, so an older replica's parser cannot read it as a namespace total.
- **`_retired`** is written only by the reconciler. When it prunes a terminated replica's current-period key, that key's totals fold into this period-tagged accumulator first, so spend a dead replica already published is never erased. Replicas fold `_retired` into their enforcement view like a peer partial.
- **`_canonical`** is written only by the reconciler. It is the durable roll-up, including `_retired`.
- **`_previous`** is written only by the reconciler. It holds the previous period's spend per source (each replica key and `_retired`) under `sources`, with the archived `period` and the period it was written in (`archivedIn`). Replicas never fold it, because the gateway enforces only the current period. Its value is a nested object, so no parser reads it as a namespace total, including an older replica's.

### Cross-replica enforcement view

The value a replica enforces against is its own live counter, plus every peer's most recently published partial, plus `_retired`. Freshness is bounded by how often peers publish, not by how often replicas fold: at most one 10-second publish interval under soft enforcement, and one watch propagation under hard enforcement, where settles inside the boundary region publish immediately. Replicas keep folding because a replica that only read `_canonical` at startup would never observe peer spend, and drift would grow without bound within a period.

`_canonical` is not on the enforcement path. It is the roll-up the reconciler writes for status reporting and for the seed at replica start, and a replica reads it only at startup. Per-request enforcement never waits on the reconciler.

### The reducer

![Sequence diagram of one reconcile pass. The reconciler reads every key from the ConfigMap, moves old-period keys into _previous and deletes them, folds dead replicas' current-period keys into _retired and deletes them, sums the current partials plus _retired, writes _retired, _canonical, and _previous back, and updates ModelProvider status with the current and previous periods' budgetUsage.](../../diagrams/budget-exchange-reduce.svg)

On every pass the reconciler handles each key by its `period` tag and by whether a live gateway Pod of that name exists:

- A current-period key from a live replica is summed.
- A current-period key from a replica that no longer exists is folded into `_retired`, then deleted. Deleting the key must not delete the spend it recorded. Under soft enforcement the fold prevents a small bounded undercount per rollout. Under hard enforcement it keeps the cap intact, because a rolling restart that erased each replaced replica's published spend would void the ceiling once per rollout.
- An old-period key, from any replica and including `_retired`, is moved into `_previous` under its source name and deleted. Both happen in the same ConfigMap write, so a failed status write loses nothing, which is why the archive lives in the ConfigMap and not only in status. A replica can publish one more old-period partial after the boundary, because its tick publishes before it folds. A replica skips it when its ledger has already rolled to the new period, which happens at its first request or fold of that period, including a fold a peer's new-period publish triggers. So a source can appear again after it was archived. That partial is a newer snapshot of the same counter, so the reducer keeps the larger figure per namespace instead of adding the two, and the spend is never counted twice. Only the newest old period is kept.
- A key tagged with another provider's UID is deleted without being summed, retired, or archived, so an earlier provider of the same name never enters this one's figures or its previous-period entry.

The pass then writes `_retired`, `_canonical` (the current-period partials plus `_retired`), `_previous`, and `status.budgetUsage`.

`_previous` holds the previous period from the first pass after a rollover until the next rollover. At the next rollover, the new old-period keys replace it, and if none arrive it is dropped. [ModelProvider status](../../resources/modelprovider.md#status) covers what `budgetUsage` shows and how to tell the two periods apart.

**Period rollover.** Periods roll over at midnight UTC; [Budget accounting](../../resources/modelprovider.md#budget-accounting) lists the daily, weekly, and monthly boundaries. Each replica detects the new period on its first request of the period, resets its counter, and publishes a new-period partial. Replicas transition independently, so the ConfigMap holds mixed-period entries for a window, and the `period` tag lets the reducer archive the old entries instead of summing them into the new period. Until every replica has published a new-period partial, `_canonical` is underestimated, which is acceptable for a soft guardrail.

### When a provider is deleted

The reconciler deletes `kaalm-budget-{name}` and `kaalm-agentspend-{name}` when it releases the provider's finalizer. While referrers hold the delete, both ConfigMaps stay, because their agents still spend.

A replica's publish can race that delete, and a provider recreated under the same name must not inherit the old one's spend or `Blocked` state. So each per-replica key and `_retired` carries `_providerUID`, the UID of the ModelProvider it was written for:

- A replica that sees the provider name with a new UID starts its counters from zero.
- Folds skip keys tagged with another UID, and the reducer deletes them.
- An untagged key counts as the current provider's, so a partial from a replica that doesn't write the tag still counts.

### Budget state on crash

If all gateway replicas crash at once, up to 10s of spend, one publish interval, is lost. Under soft enforcement the loss is small relative to typical budget thresholds. [Hard enforcement](#hard-enforcement) tightens both the loss bound and the restart behavior.

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

Soft enforcement is spend visibility and guardrails, not a financial cap: it adds no coordination to the request path, and the overspend stays within the bound this section gives. Teams that need a cap opt in to [hard enforcement](#hard-enforcement). Provider-level account limits remain sound defense in depth under either mode.

## Per-workload spend

The ledger also answers "which agent spent it". Beside the per-namespace enforcement counters, each replica accumulates per-workload spend keyed `{namespace}/{workload}`, where the workload is the attested `agent/{name}` or `task/{name}` from the caller's certificate SAN, or the visible `(unattributed)` bucket for gateway-only-tier callers, which authenticate by token and carry no workload identity. Keeping that bucket visible makes the per-workload rows always sum to the namespace figure. Workload spend rolls over with the same period reset as the namespace counters. Admission never reads it, so hard enforcement is unaffected.

Persistence uses the same exchange in a second object, `kaalm-agentspend-{provider}`, with the same one-key-per-replica partials, period tag, and seed at startup. The same provider tag and rules apply to it ([When a provider is deleted](#when-a-provider-is-deleted)). The reducer treats it like the budget ConfigMap: a pruned replica's current-period partial folds into `_retired` before its key is deleted, and old periods drop. The breakdown has its own ConfigMap for two reasons. Safety: the budget fold sums every non-underscore key in the budget ConfigMap as namespace spend, so workload keys inside it would silently corrupt utilization. Capacity: a ConfigMap is capped at about 1 MiB, and at the design target of 1000+ agents the workload keys need the room.

Three deliberate boundaries:

- The breakdown keeps the **current period only**. The namespace figures keep the previous period until the next rollover ([ModelProvider status](../../resources/modelprovider.md#status)); the breakdown does not, and it never enters provider status at all, because namespaces times workloads would grow the CR against the same object cap.
- It never becomes a metric label. Per-workload resolution lives in the [console read API](../../console/overview.md) through the gateway's [GET /v1/spend](../api/internal-endpoints.md#get-v1spend), which any single replica answers, current to within one publish interval.
- It is priced spend: calls to unpriced models cost zero and do not appear, the same soft-guardrail behavior the namespace figures have.

## Hard enforcement

Setting `spec.budget.enforcement: hard` on a ModelProvider (default `soft`) turns that provider's `action: block` thresholds into a cap with a stated guarantee. Nothing else changes: `warn` and `degrade` policies remain advisory in both modes, both ceilings (`perNamespaceUSD` and `clusterUSD`) participate through the same worse-of-two utilization, and outside the boundary region described below enforcement is the soft behavior. Three validation rules constrain the mode: hard requires at least one `block` policy (rule 32), every catalog model priced (rule 33, because an unpriced call settles at zero and a cap cannot count spend it never prices), and a coherent boundary margin (rule 34). See [Cross-resource validation](../../resources/validation-and-defaulting.md#cross-resource-validation).

### The boundary region

After-the-fact counting alone cannot enforce a hard cap: by the time a settled cost lands on the counters, the spend has happened. Estimating request costs up front is not possible in general, least of all for streams. Instead, hard mode changes behavior only inside a **boundary region** immediately below each `block` threshold, where the remaining headroom is small enough that uncoordinated concurrency could cross the ceiling.

The region starts `boundaryMarginPercent` percentage points below each `block` policy's `atPercent` (the knob under `budget.hard`, default 5). The configured value is a floor. Each replica also computes a margin from what it has observed this period, and the **effective margin** is the larger of the two:

```
marginUSD = replicas x maxObservedCostPerCall
          + (replicas - 1) x observedPeakSpendRatePerSec x stalenessWindowSeconds

effectiveMarginPercent = max(boundaryMarginPercent, 100 x marginUSD / ceilingUSD)
```

The first term covers one unsettled in-flight request per replica; the second covers spend a peer has settled but not propagated. The observed inputs are the largest settled cost of a single call and the peak spend rate this period. Neither shrinks until rollover, so an early burst widens the margin for the rest of the period, which errs toward throttling earlier, the safe direction. The replica count includes not-yet-Ready gateway Pods, also deliberately conservative.

When the computed margin exceeds the configured knob, the replica raises the `_marginExceeded` flag in its published partial, and the ModelProviderReconciler surfaces it as a `BoundaryMarginRaised` condition and a Warning event on the ModelProvider. The operator learns the knob is undersized for the observed traffic without the guarantee ever having depended on it. One residual remains: a traffic burst without precedent in the current period can outrun the computed margin in the first staleness window it appears. Sizing the configured knob from the [soft overspend bound](#the-overspend-bound) formula with your own worst-case rates closes that gap.

### Serialized admission

Inside the boundary region, each replica admits **at most one request at a time** per governed ceiling: a per-`(provider, namespace)` slot for the namespace ceiling, and a provider-wide slot when the cluster ceiling is in its boundary region. A request that finds the slot held is rejected immediately with `429 budget_throttled` and `Retry-After: 1` ([error schema](../api/errors.md#llm-gateway-error-responses)). There is no queue, deliberately: a queued request holding one provider's slot while waiting on another's (reachable through a fallback chain) can deadlock, and near the ceiling a queue mostly drains into blocks anyway.

Settlement is the other half of the invariant. When the admitted request completes, its actual cost lands on the counter and the slot frees together, so the next admitted request always sees the previous one's real cost. The settle also publishes the replica's partial immediately instead of waiting for the 10-second tick, which shrinks peer staleness to one watch propagation inside the region. The slot releases only on settle, even if a fold meanwhile drops utilization below the boundary.

The block decision itself is unchanged from soft mode, with one addition: the `429 budget_exhausted` message names which ceiling fired, the namespace's or the cluster's.

### The guarantee

For a hard-mode provider, spend within a period never exceeds a `block` ceiling by more than:

- the actual cost of at most **one in-flight request per replica, per governed ceiling**, plus
- each peer's settled-but-not-yet-propagated spend, bounded by **one settle-publish-to-watch propagation** per peer, typically well under a second.

Three limits are part of the guarantee:

- The gateway can only cap what providers report. A response with no usage metadata settles at zero cost ([Streaming responses](request-handling.md#streaming-responses)); a provider that omits usage evades any gateway-side cap. Rule 33 keeps unpriced *models* out of hard mode, but usage-less *responses* are a provider behavior no proxy can price.
- The margin bounds when serialization engages, not what one admitted request may cost. A single request larger than the remaining headroom is admitted, and its overshoot is the one-in-flight-request bound of the guarantee above.
- A simultaneous crash of all replicas loses at most the in-flight requests' costs inside the boundary region (settles publish immediately there), rather than soft mode's 10 seconds of spend.

### Behavior under failure

Hard mode fails closed, but only where the guarantee is live. Below the boundary region, apiserver unavailability degrades hard enforcement to exactly the soft bound: counters keep counting, publishes retry on the tick, requests flow. Inside the boundary region, a replica rejects requests with `503 budget_state_unavailable` when either staleness signal trips:

- **Write path**: it holds settled spend older than the staleness window that it has been unable to publish, so peers may be admitting against a stale view of this replica.
- **Read path**: it has not successfully refreshed its peer view within the staleness window. A dead watch with quiet peers looks like silence, so the exchange tick doubles as the liveness probe.

The staleness window is not configurable: it is three publish intervals, 30 seconds. Recovery is automatic on the first successful publish or fold. A cap that fails open is not a cap, and a cap that fails closed everywhere is an availability hazard, so the design fails closed only inside the region where the ceiling is at stake.

### Restarts and rollover

A restarting replica seeds from `_canonical` and folds live partials on top. Until the reconciler's next pass, the replica's own pre-restart key may be counted twice, so the enforcement view can transiently overcount. Overcounting blocks early rather than late, the correct failure side for a cap, and it clears within one reconcile. Rolling restarts do not erase published spend, because the reconciler folds pruned keys into `_retired` before deleting them ([The reducer](#the-reducer)).

At period rollover, counters, slots, and the observed-traffic tracker all reset, so the margin returns to the configured knob until new observations accrue. A request admitted before midnight settles into the new period, as in soft mode, and the boundary flag drops with the first new-period publish.

### Streaming under hard enforcement

A streaming request admitted in the boundary region holds its admission slot until the stream ends, since its cost settles only then. Nothing caps the hold except a stalled stream: `gateway.providerFirstByteTimeout` (default `120s`) bounds each gap between chunks, not the stream's length. Near the cap, streams serialize, and a long stream makes its neighbors wait out `budget_throttled` retries for its whole duration. That is the cost of a hard cap over pay-per-token streaming, and the reason the region is kept as narrow as the margin math allows.

### Interaction with fallback

A hard-mode primary that is blocked, throttled, or failed closed short-circuits exactly as a soft block does: the fallback walk never starts, because a capped namespace must not drain a fallback provider's budget. Inside a walk, a hard fallback candidate is governed by the same admission rules, consumes an attempt slot, and falls through to its children. A walk exhausted entirely by budget outcomes returns a budget error, never `502 provider_error` ([Depth cap semantics](fallback.md#depth-cap-semantics)).

### What hard mode costs

Serialized admission near the ceiling, one ConfigMap write per settle inside the boundary region, and a ConfigMap watch per gateway replica. Plan for the first: budget utilization is monotonic within a period, so a namespace that reaches its boundary region stays in it until rollover, and month-end is exactly when its traffic serializes. Namespaces that need throughput at high utilization should widen their budget, not their margin.

## Rate limiting

Rate limits are enforced at the gateway. Limits come from `ModelProvider.spec.rateLimits` and represent **cluster-wide ceilings**. Each (namespace, model) pair has up to two buckets, both refilled continuously:

| Bucket | Limit | Counts | Admits a request when |
|---|---|---|---|
| Request bucket | `requestsPerMinute` | Requests | It holds at least one request |
| Token bucket | `tokensPerMinute` | Input plus output LLM tokens | It holds more than 0 tokens |

An unset or zero limit disables its bucket. A request refused by either bucket gets `429 rate_limited` with a computed `Retry-After` ([Retry-After](#retry-after)). The rejection is retryable and does not fall back.

### The token limit

The token limit is enforced after the fact, because the gateway cannot know a call's token count before the provider answers.

1. **Admit.** The gateway admits a request while the token bucket is above 0. The request does not need a whole token, and admission consumes no tokens. The token check runs first, and the request check then takes one request token, so a request the token bucket refuses keeps its request token.
2. **Debit.** After the call, the gateway subtracts the settled input plus output tokens from the token bucket that admitted the request: the primary provider's bucket for the requested model, after any budget `degrade` rewrite, even when a fallback provider served the call. A buffered response debits when the gateway reads it. A stream debits when the stream ends ([Streaming responses](request-handling.md#streaming-responses)).
3. **Block.** A large call blocks the next request, not itself.

The token bucket may go negative. The debt is clamped at minus one burst (`tokensPerMinute / number_of_replicas`), so one call larger than a burst blocks the key for at most about one minute.

### Retry-After

The gateway computes `Retry-After` from how far the refused bucket is from admitting again, as whole seconds and at least 1:

| Refused by | `Retry-After` (seconds) |
|---|---|
| Token bucket | `ceil(-tokens / per_replica_tokens_per_minute * 60)` |
| Request bucket, including the tool-plane (namespace, ToolProvider) bucket | `ceil((1 - tokens) / per_replica_requests_per_minute * 60)` |

For example, with `requestsPerMinute: 2` and one replica, the third request in a burst gets `Retry-After: 30`. When each replica's share is 60 or more requests or calls per minute, a refusal gives `Retry-After: 1`. Heartbeat `rate_limited` responses send `Retry-After: 1`.

### Limits of the token limit

- **Concurrent long streams all pass.** The debit lands only when each stream ends, so streams that start while the bucket is positive are all admitted.
- **The split is approximate.** The per-replica split is approximate, as for requests ([Dividing by live replica count](#dividing-by-live-replica-count)), and the debit lands only on the replica that served the call.
- **Only input plus output tokens count.** The debit is the input plus output count from the response's usage. For `anthropic`, cache read and cache creation tokens are not in that count. For `openai` and `openai-compatible`, `prompt_tokens` already includes cached prompt tokens, so cache hits count. A response with no usage debits nothing, the case counted as `kaalm_llm_usage_missing_total` ([Streaming responses](request-handling.md#streaming-responses)).
- **Each key gets the full ceiling.** Buckets are per (namespace, model), so N namespaces, or N models in one namespace, each get the full `requestsPerMinute` and the full `tokensPerMinute`. Together, tenants can still reach the provider's own rate limit, so these limits do not keep a shared provider key under it.

### Dividing by live replica count

Each gateway replica divides each configured limit by the number of gateway Pods, a count that can be up to 5 seconds old. Each bucket's burst equals that per-replica share, except the request bucket and the tool-plane bucket (`ToolProvider` `requestsPerMinute`), which always hold at least one request or call ([Request limits below the replica count](#request-limits-below-the-replica-count)). The token bucket's burst is exactly `tokensPerMinute / number_of_replicas`, with no floor. When replicas scale up or down, each replica resizes its local buckets within that lag and the next refill cycle, so the configured value stays the intended cluster-wide limit regardless of replica count.

Because each replica enforces its share independently, the effective cluster-wide limit is approximate. Transient bursts may exceed the configured ceiling by up to one replica's full bucket: `configured_limit / number_of_replicas`. This is the accepted trade for a coordination-free request path.

### Request limits below the replica count

When `requestsPerMinute` is lower than the number of gateway replicas, each replica's share is less than one request per minute. The request bucket always has room for one request, so each replica admits one request for each (namespace, model), then refuses until the bucket refills at `requestsPerMinute / number_of_replicas` requests per minute. For example, `requestsPerMinute: 2` on 3 replicas admits one request per replica every 90 seconds.

Because every replica's bucket starts with one request, a cluster-wide burst can admit up to `number_of_replicas` requests at once. The long-run rate stays at `requestsPerMinute`. The limit does not cap how many requests run at once.

The tool-plane bucket follows the same rules, counting calls instead of requests: `ToolProvider` `requestsPerMinute: 1` on 2 replicas admits one call per replica every 120 seconds.

### Worst-case deviation during scaling events

During scale-up, existing replicas divide by N+1 as soon as the new Pod appears, before it serves traffic, momentarily reducing each existing replica's effective limit. During rolling restarts (`maxUnavailable: 1`), different replicas can transiently hold different bucket sizes, so the effective cluster-wide ceiling deviates by up to one replica's share.

Per-replica division is the design point. A shared bucket coordinated through a ConfigMap, as the budget exchange is, would cost a write per request.

## Related

- [ModelProviderReconciler](../../controller/reconcilers/modelprovider.md): the reducer over the budget ConfigMap.
- [ModelProvider](../../resources/modelprovider.md): where `spec.rateLimits` and the budget configuration are declared.
- [Streaming responses](request-handling.md#streaming-responses): why streamed spend is invisible until the stream completes.
- [LLM Gateway error responses](../api/errors.md#llm-gateway-error-responses): the structured error schema and status code mapping, including budget exhaustion and 429 responses.
- [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions): the ConfigMap RBAC the budget exchange relies on.
