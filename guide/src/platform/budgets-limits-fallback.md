# Budgets, Limits, and Fallback

This page is the operating manual for the guardrails on a ModelProvider: what
each knob does, what the calling agent experiences when it fires, and how to
read the aftermath from status.

One design fact up front: budgets are **soft limits**. Each gateway replica
keeps a local ledger and replicas exchange totals through a ConfigMap, so a
burst of parallel requests can overshoot a ceiling slightly before every
replica has caught up. Budgets are guardrails against runaway spend, not
billing-grade metering.

## Budget policies: warn, degrade, block

The budget block from `config/samples/agentry_v1alpha1_modelprovider.yaml`:

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

What a blocked team's Agents look like: the controller surfaces budget
exhaustion as a `Degraded` **condition** on the Agent (visible in
`kubectl describe agent`), while the phase stays `Running`; exhaustion is
recoverable, not a lifecycle event.

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
