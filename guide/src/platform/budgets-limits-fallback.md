# Budgets, Limits, and Fallback

This page is the operating manual for the guardrails on a ModelProvider: what
each knob does, what the calling agent experiences when it fires, and how to
read the aftermath from status.

One design fact up front: budgets are **soft limits by default**. Each
gateway replica keeps a local ledger and replicas exchange totals through a
ConfigMap, so a burst of parallel requests can overshoot a ceiling slightly
before every replica has caught up. Soft budgets are guardrails against
runaway spend, not billing-grade metering. Since v0.3.0 a provider can opt
in to hard enforcement, which turns its block policies into a cap with a
stated guarantee; that task is [below](#turning-on-hard-enforcement).

## Budget policies: warn, degrade, block

The budget block from `config/samples/kaalm_v1alpha1_modelprovider.yaml`:

```yaml
budget:
  period: monthly
  perNamespaceUSD: "500"
  policies:
    - atPercent: 80
      action: warn
    - atPercent: 100
      action: degrade
      degradeTo: claude-opus-4-6
```

Policies fire as spend crosses their `atPercent`; when several have been
crossed, the highest one wins. What each action means from both seats:

| Action | The caller sees | You see |
|---|---|---|
| `warn` | Nothing; requests flow unchanged | A gateway log line and a budget-threshold metric |
| `degrade` | Responses come from the `degradeTo` model, whatever was requested | The threshold metric; spend keeps accruing at the cheaper rate |
| `block` | `429` with error type `budget_exhausted` and a `Retry-After` giving the seconds until the period resets | The namespace shows `state: Blocked` in provider status |

Two validation notes on `degradeTo`: it must name a model in the same
provider's catalog (`Ready=False, reason=InvalidDegradeTarget` otherwise),
and if it is not the cheapest model in the catalog the controller emits an
advisory `DegradeTargetNotCheapest` event, since a "degrade" that escalates
cost is usually a mistake.

Periods reset at midnight UTC: `monthly` on the first of the month, `weekly`
on Monday, `daily` every day. `perNamespaceUSD` caps each namespace
independently; add `clusterUSD` for a ceiling on the sum across namespaces.
Either ceiling alone is fine; a blocked request's error message names which
one fired.

What a blocked team observes: their agents keep running (exhaustion is
recoverable, not a lifecycle event) and every LLM call answers `429` until
the period resets. The namespace's `state: Blocked` in provider status is
your side of the same picture. Each affected Agent also carries a `Degraded`
condition (reason `BudgetExhausted`) visible in `kubectl describe agent`,
with its phase preserved; the condition clears on its own when the budget
frees up. All of this is identical under hard enforcement; what hard adds
is the behavior just below and at the ceiling.

## Turning on hard enforcement

For a provider whose invoice must not exceed the manifest, set
`budget.enforcement: hard`:

```yaml
budget:
  period: monthly
  perNamespaceUSD: "500"
  enforcement: hard
  hard:
    boundaryMarginPercent: 5
  policies:
    - atPercent: 100
      action: block
```

Three validation gates apply: hard requires at least one `block` policy
(rejected at apply time otherwise), every model in the catalog must be
priced (`Ready=False, reason=HardBudgetUnpriced` until it is), and
`boundaryMarginPercent` must sit strictly below every block threshold (also
apply-time). `warn` and `degrade` policies keep working exactly as in soft
mode.

What changes at runtime happens only near the ceiling. A few points below
each block threshold (the margin), requests to that provider serialize: one
in-flight request at a time per namespace, with concurrent requests
answered `429 budget_throttled` and `Retry-After: 1`, which callers should
retry on a short backoff (the opposite of `budget_exhausted`'s
wait-for-the-period guidance). The request that would cross the ceiling is
rejected with no call to the upstream provider, and the block message names
which ceiling fired. If a gateway replica cannot verify budget state inside
that region, it answers `503 budget_state_unavailable` rather than spending
blind, and recovers on its next successful exchange.

Two things to watch after enabling it:

- A `BoundaryMarginRaised` condition on the provider means observed traffic
  needed a wider margin than your `boundaryMarginPercent`; the gateway
  widened it automatically and the guarantee held, but size the knob
  deliberately using the overspend-bound formula in the design book.
- A namespace that lives near its ceiling (month-end, typically) lives with
  serialized admission until the period resets. If a team needs throughput
  at high utilization, widen their budget rather than their margin.

The exact guarantee, its fine print (streams, usage-less responses), and
the mechanism live in the design book's Budgets and Rate Limits chapter,
Hard Enforcement section.

## Reading spend

```bash
kubectl get modelprovider anthropic-shared -o jsonpath='{.status.budgetUsage}' | jq
kubectl get modelprovider anthropic-shared -o jsonpath='{.status.clusterSpentUSD}'
```

Each `budgetUsage` entry carries the namespace, the period key, `spentUSD`,
`percentUsed`, and a `state` of `Normal`, `Throttled`, or `Blocked`. Status is
synced periodically from the gateway ledgers, so it can lag live spend by a
sync interval; it is the display surface, not the enforcement counter.

## Rate limits

```yaml
rateLimits:
  requestsPerMinute: 300
  tokensPerMinute: 500000
```

Buckets are per `(namespace, model)`: each pair gets the full configured
ceiling, so a namespace using three models can reach three times the ceiling
against the provider in aggregate. The configured value is the intended
cluster-wide limit; each gateway replica enforces its share. A limited caller
gets `429` with error type `rate_limited`; unlike a budget block, this clears
in seconds, not at a period boundary.

## Fallback chains

```yaml
fallback:
  - name: anthropic-backup
```

If the provider is unreachable, times out, or returns a 5xx, the gateway
tries the fallback chain in declared order, walking each fallback's own
chain depth-first. The rules that surprise people:

- Every provider in a chain must have the **same `spec.type`** as the
  primary; there is no cross-format translation.
- The gateway-level depth cap (`gateway.maxFallbackDepth`, default 3) bounds
  the **total providers attempted per request, including the primary**, not
  the nesting depth.
- A budget-blocked **primary** returns `429 budget_exhausted` immediately
  with no fallback: a capped namespace must not drain the backup's budget. A
  budget-blocked **fallback candidate** is skipped but still consumes an
  attempt slot.
- If the whole walk fails, the caller gets `502 provider_error` in the
  general case, or `503`/`504` when every attempt was unreachable or timed
  out.

---

*How this works: design book pages Resources, ModelProvider (fallback trees,
with the diagram of the depth cap), Gateways, LLM, Budgets and Rate Limits
(the ledger and the replica exchange), and Gateways, LLM, Fallback (the
traversal pseudocode).*
