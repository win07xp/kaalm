# Budgets, limits, and fallback

This page is the operating manual for the guardrails on a ModelProvider: what
each value does, what the calling agent experiences when it fires, and how to
read the aftermath from status.

Budgets are *soft limits by default*. Gateway replicas exchange spend totals,
so a burst of parallel requests can overshoot a ceiling slightly before every
replica catches up ([the overspend bound](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/budgets-and-rate-limits.md#the-overspend-bound)).
Soft budgets are guardrails against runaway spend, not billing-grade metering.
A provider can opt in to hard enforcement, which turns its block policies into
a cap with a stated guarantee; see
[Turning on hard enforcement](#turning-on-hard-enforcement).

## Budget policies: warn, degrade, block

The budget block from `config/samples/kaalm_v1beta1_modelprovider.yaml`:

```yaml
budget:
  period: monthly
  perNamespaceUSD: "500"
  policies:
    - atPercent: 80
      action: warn
    - atPercent: 100
      action: degrade
      degradeTo: claude-sonnet-4-6
```

A policy applies when spend reaches its `atPercent`; when spend has reached
several, the policy with the highest `atPercent` wins. What each action means
for the caller and for you:

| Action | The caller sees | You see |
|---|---|---|
| `warn` | Nothing; requests flow unchanged | A gateway log line and a budget-threshold metric |
| `degrade` | Responses come from the `degradeTo` model, whatever was requested | The threshold metric; spend keeps accruing at the cheaper rate |
| `block` | `429` with error type `budget_exhausted` and a `Retry-After` giving the seconds until the period resets | The namespace shows `state: Blocked` in provider status |

Two validation notes on `degradeTo`: it must name a model in the same
provider's catalog (`Ready=False, reason=InvalidDegradeTarget` otherwise),
and if it costs more than another priced model in the catalog the controller
sets the advisory `DegradeTargetNotCheapest` condition and emits an event of
the same name, since a "degrade" that escalates cost is usually a mistake. The
condition clears when the target changes. A `degradeTo` that names the model
being requested changes nothing.

Periods reset at midnight UTC: `monthly` on the first of the month, `weekly`
on Monday, `daily` every day. `perNamespaceUSD` caps each namespace
independently; add `clusterUSD` for a ceiling on the sum across namespaces.
Either ceiling alone is fine; a blocked request's error message names which
one fired.

What a blocked team observes: their agents keep running and every LLM call
answers `429` until the period resets. The namespace's `state: Blocked` in
provider status is your side of the same picture. Each affected Agent also
carries a `Degraded` condition (reason `BudgetExhausted`) visible in
`kubectl describe agent`, with its phase preserved; the condition clears on its
own when the budget frees up. All of this is identical under hard enforcement;
hard adds the behavior near the ceiling.

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

What changes at runtime happens only near the ceiling. Within the margin
below each block threshold, requests to that provider serialize: one in-flight
request at a time per namespace on each gateway replica. A concurrent request
gets `429 budget_throttled` with `Retry-After: 1`, which callers retry on a
short backoff (unlike `budget_exhausted`, which waits for the period). At the
ceiling, `block` works as in soft mode. If a gateway
replica cannot verify budget state inside the margin, it answers
`503 budget_state_unavailable` rather than spending blind, and recovers on its
own.

Two things to watch after enabling it:

- A `BoundaryMarginRaised` condition on the provider means observed traffic
  needed a wider margin than your `boundaryMarginPercent`. The gateway
  widened it automatically and the guarantee held, but size the value
  from [the boundary region](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/budgets-and-rate-limits.md#the-boundary-region).
- A namespace that lives near its ceiling (month-end, typically) lives with
  serialized admission until the period resets. If a team needs throughput
  at high utilization, widen their budget rather than their margin.

The exact guarantee and its fine print (streams, usage-less responses) are in
the design book's [Hard enforcement](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/budgets-and-rate-limits.md#hard-enforcement) section.

## Reading spend

```bash
kubectl get modelprovider anthropic-shared -o jsonpath='{.status.budgetUsage}' | jq
kubectl get modelprovider anthropic-shared -o jsonpath='{.status.clusterSpentUSD}'
```

Each `budgetUsage` entry carries the namespace, the period key, `spentUSD`,
`percentUsed`, and a `state` of `Normal`, `Throttled`, or `Blocked`. Status is
synced periodically from the gateway ledgers, so it can lag live spend by a
sync interval. It is the display surface, not the enforcement counter.

After a period rollover, the list also holds the previous period's entries,
with `state` `Normal`, until the next rollover. A namespace has a
current-period entry only after it spends in that period. Until then, the
previous period's entry is the only one it has, and on a daily budget with no
traffic that day that lasts all day. So the latest key is not always the
current period. Filter on the current period's key instead. This example reads
a monthly budget, whose key is `YYYY-MM`:

```bash
kubectl get modelprovider anthropic-shared -o json \
  | jq --arg p "$(date -u +%Y-%m)" '.status.budgetUsage | map(select(.period == $p))'
```

For a daily budget the key is `YYYY-MM-DD` (`date -u +%F`). For a weekly budget
it is the ISO week, such as `2026-W41` (`date -u +%G-W%V`). An empty result
means no namespace has spent in the current period.

The design book's
[ModelProvider](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/modelprovider.md#status)
page gives the rule.

With `period: none` the provider tracks no spend, so `budgetUsage` and
`clusterSpentUSD` are empty. Turning a budget off clears them, along with any
`BudgetExhausted` condition on Agents. See the design book's
[ModelProviderReconciler](https://github.com/win07xp/kaalm/blob/main/docs/src/controller/reconcilers/modelprovider.md#budget-reconciliation)
page.

Deleting a ModelProvider discards its spend for the current period. If you
recreate it under the same name, every namespace's budget starts from zero. See
the design book's
[ModelProvider](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/modelprovider.md#deletion)
page.

## Rate limits

```yaml
rateLimits:
  requestsPerMinute: 300
```

Buckets are per `(namespace, model)`: each pair gets the full configured
ceiling of requests per minute, so a namespace using three models can reach
three times the ceiling against the provider in aggregate. The configured value
is the intended cluster-wide limit; each gateway replica enforces its share. A
limited caller gets `429` with error type `rate_limited` and a `Retry-After`
that says when to retry. Unlike a budget block, it clears when `Retry-After`
says, not at a period boundary.

`tokensPerMinute` is a second ceiling on the input plus output tokens per
minute, with the same per-`(namespace, model)` key. Leave it unset for no token
limit. Tokens are counted after the call, so it behaves differently from the
request limit:

- The gateway admits a request while the namespace's token bucket for that
  model is above 0, then subtracts the call's tokens when the call ends, or
  when a stream ends. A large call blocks the next request, not itself, and
  concurrent long streams all pass while the bucket is positive.
- The call's tokens come off the primary provider's bucket for the model you
  asked for, even when a fallback provider answered.
- Anthropic cache tokens are not counted. OpenAI and OpenAI-compatible input
  counts include cached prompt tokens, so cache hits count. A response with no
  usage debits nothing.
- Every namespace, and every model in a namespace, gets the full ceiling, so
  the limit does not keep a shared provider key under the provider's own
  tokens-per-minute limit.

The design book's [Rate limiting](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/budgets-and-rate-limits.md#rate-limiting)
section has the bucket sizes and the `Retry-After` arithmetic, including how
the delay grows when the request limit is below the replica count.

## Fallback chains

```yaml
fallback:
  - name: anthropic-backup
  - name: openai-backup
    modelMap:
      claude-opus-4-6: gpt-5
      claude-sonnet-4-6: gpt-5-mini
```

If the provider is unreachable, times out, or answers with a `5xx`, `429`,
`401`, or `403`, the gateway tries the fallback chain in declared order
([Fallback logic](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/fallback.md) draws the traversal). Other `4xx` responses return to
the caller unchanged. The rules to know:

- A fallback may have a different `spec.type` when the gateway can
  translate between the two: `anthropic` and `openai` (or
  `openai-compatible`) in either direction. Put a `modelMap` on that edge
  naming the fallback's model for each of yours. `google-vertex` chains stay
  same-type. When the fallback is `anthropic`, give its models a
  `maxOutputTokens`: Anthropic's API requires `max_tokens`, OpenAI clients
  often omit it, and the gateway fills it from the catalog. When it cannot,
  your provider gets the advisory `MaxOutputTokensUnset` condition and an
  event. A request using a feature the other format lacks (extended thinking,
  `response_format`) skips that fallback with a `FallbackIneligible` event on
  your provider.
- A candidate whose `allowedNamespaces` or `models` can never serve one of
  your callers is flagged before any request reaches it: the primary gets a
  `FallbackIneligible` condition, with an event naming each new finding. The
  provider stays `Ready`; the check is advisory.
- The gateway-level depth cap (`gateway.maxFallbackDepth`, default 3) bounds
  the *total providers attempted per request, including the primary*, not
  the nesting depth.
- A budget-blocked *primary* returns `429 budget_exhausted` immediately
  with no fallback: a capped namespace must not drain the backup's budget. A
  budget-blocked *fallback candidate* is skipped, still consumes an attempt
  slot, and its own fallbacks are still walked.
- If the whole walk fails, the caller gets `502 provider_error` in the
  general case, `503 provider_unavailable` when every attempt was unreachable,
  or `504 provider_timeout` when every attempt timed out.

---

*How this works: design book pages Resources, ModelProvider (fallback trees,
with the diagram of the depth cap), Gateways, LLM, Budgets and rate limits
(the ledger and the replica exchange), Gateways, LLM, Fallback logic (the
traversal pseudocode), and Controller, Reconcilers (the reconcile-time
fallback eligibility scan).*
