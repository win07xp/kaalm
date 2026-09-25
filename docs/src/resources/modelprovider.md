# ModelProvider

ModelProvider is a cluster-scoped resource that defines a managed LLM provider. It holds a reference to a Secret with credentials, a model catalog, budgets, a request rate limit, a fallback list, and the namespace tenancy gate. The gateway enforces the catalog, the budgets, the rate limit, and the fallback walk on every request it routes; the controller validates the spec, probes the upstream, and folds spend into status.

Because it is cluster-scoped, a ModelProvider is a platform-team resource: application teams reference it from their namespaces, and only the namespaces listed in `spec.allowedNamespaces` may do so. `allowedNamespaces` is the one tenancy gate every caller faces, in both adoption tiers ([Provider access gating](../concepts/tenancy-and-tiers.md#provider-access-gating)).

## Spec

The annotated example shows every spec field.

```yaml
apiVersion: kaalm.io/v1beta1
kind: ModelProvider
metadata:
  name: anthropic-shared
spec:
  # Required. "anthropic" | "openai" | "google-vertex" | "openai-compatible".
  # The gateway routes no inbound path for google-vertex (see The
  # google-vertex type is reserved on Request handling).
  type: anthropic

  # Required. The schema pattern ^https:// rejects any other scheme: the
  # gateway forwards the credential to this URL.
  endpoint: "https://api.anthropic.com"

  # Required. A Secret key in the operator namespace, read by the gateway
  # there; credentials never reach agent containers.
  credentialsRef:
    name: anthropic-api-key
    key: api-key

  # The catalog. A request must name a model in it. Keyed by id: the schema
  # declares the list as a map, so a duplicate id is rejected at apply time.
  models:
    - id: "claude-opus-4-6"
      displayName: "Claude Opus 4.6"
      costPer1MInputTokens:  "15.00"
      costPer1MOutputTokens: "75.00"
    - id: "claude-sonnet-4-6"
      displayName: "Claude Sonnet 4.6"
      costPer1MInputTokens:  "3.00"
      costPer1MOutputTokens: "15.00"
      # Declared output ceiling, minimum 1. Supplied as max_tokens when a
      # request crosses a fallback edge into an anthropic provider without
      # one, and caps a larger value. A model without it cannot serve such
      # a request; the reconciler warns (MaxOutputTokensUnset) on the
      # primary that maps to it.
      maxOutputTokens: 64000

  # Glob patterns. "*" matches every namespace; an empty list admits none.
  allowedNamespaces:
    - "team-support"
    - "team-ml"
    - "sandbox-*"

  budget:
    # "monthly" | "weekly" | "daily" | "none". Schema default none, under
    # which nothing is tracked.
    period: monthly
    # Decimal USD strings. Either ceiling may be set without the other.
    perNamespaceUSD: "500.00"
    clusterUSD: "10000.00"
    # "soft" (schema default) | "hard". Hard requires a block policy (rule
    # 32) and a fully priced catalog (rule 33).
    enforcement: hard
    hard:
      # 0 to 100, schema default 5. Must sit strictly below every block
      # policy's atPercent (rule 34).
      boundaryMarginPercent: 5
    # Threshold actions. atPercent is 0 to 100; action is "block" | "warn"
    # | "degrade"; degradeTo is required for degrade and must name a
    # catalog model (rule 18).
    policies:
      - atPercent: 80
        action: degrade
        degradeTo: "claude-sonnet-4-6"
      - atPercent: 100
        action: block

  rateLimits:
    # Cluster-wide ceiling per (namespace, model); each replica enforces its
    # share.
    requestsPerMinute: 300
    # Accepted by the schema and not enforced (#202).
    tokensPerMinute: 500000

  # Providers tried when this one fails, each with its own fallback list,
  # forming a tree. Must terminate (rule 11) and stay within formats the
  # gateway translates (rule 12).
  fallback:
    - name: anthropic-backup
    # An edge that crosses formats names, per model of this provider, the
    # model the fallback serves in its place (rule 41).
    - name: openai-backup
      modelMap:
        claude-opus-4-6: gpt-5
        claude-sonnet-4-6: gpt-5-mini

  # The controller's periodic upstream probe. An omitted block means an
  # enabled probe with the defaults.
  healthCheck:
    enabled: true
    intervalSeconds: 60
    timeoutSeconds: 10
```

`kubectl get mp` prints the type and the `Ready` and `Healthy` conditions.

## Status

```yaml
status:
  observedGeneration: 2
  conditions:
    - type: Ready
      status: "True"
      reason: CredentialsValid
    - type: Healthy
      status: "True"
      reason: UpstreamReachable
      lastTransitionTime: "2026-04-05T12:00:00Z"
    - type: GatewayReachable
      status: "True"
      reason: GatewayReady
  budgetUsage:
    - namespace: "team-support"
      period: "2026-04"
      spentUSD: "287.50"
      percentUsed: 57
      state: "Normal"
    - namespace: "team-ml"
      period: "2026-04"
      spentUSD: "412.00"
      percentUsed: 82
      state: "Throttled"
  clusterSpentUSD: "699.50"
```

| Condition | Meaning |
|---|---|
| `Ready` | The spec is valid and the credential resolves. `True` with `reason: CredentialsValid`. `False` with one of `CredentialsMissing` (the Secret or key is absent or empty), `CredentialsInvalid` (the probe was refused with a 401 or 403), `FallbackIneligible` (rules 11 and 12), `InvalidDegradeTarget` (rule 18), `InvalidModelMap` (rule 41), or `HardBudgetUnpriced` (rule 33). |
| `Healthy` | The periodic upstream probe. `True` with `UpstreamReachable`; `False` with `ProviderUnhealthy` and a `Warning` event; `Unknown` with `ProbeSkipped` for `google-vertex`, which has no probe. |
| `GatewayReachable` | Set on every pass: `True` with `GatewayReady` when at least one gateway Pod is Ready, else `False` with `GatewayUnavailable`. The same value is mirrored onto every ModelProvider. |
| `DegradeTargetNotCheapest` | Advisory; never affects `Ready`. `True` with `CheaperModelAvailable` when a degrade policy's `degradeTo` is not the cheapest model in the catalog, `False` with `DegradeTargetCheapest` once it is. The `Warning` event fires on the transition to `True` ([`degradeTo` validation](#degradeto-validation)). |
| `BoundaryMarginRaised` | Hard enforcement only. `True` when a gateway replica observed traffic that needed a wider boundary margin than `hard.boundaryMarginPercent` configures ([Hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement)). |

`healthCheck.enabled: false` disables the probe (for example for an offline test fixture); `intervalSeconds` (default 60) sets its cadence and `timeoutSeconds` (default 10) bounds each request. `budgetUsage` is per-namespace spend for the current period, and `clusterSpentUSD` is the sum across namespaces.

Each `budgetUsage` entry's `state` is a per-namespace state machine over the current period:

![Per-namespace budget state for one ModelProvider and one period. The period opening enters Normal. Normal moves to Throttled when spend crosses a degrade policy's atPercent. Normal or Throttled move to Blocked when spend crosses a block policy's atPercent. A period rollover, or a spec edit that raises the ceiling or changes the policies, moves Throttled or Blocked back to Normal.](../diagrams/budget-namespace-states.svg)

The state is derived from `percentUsed`, which is spend against `perNamespaceUSD` alone, and the highest policy threshold at or below it: `Throttled` for a degrade policy, `Blocked` for a block policy, `Normal` when no threshold is crossed or no policies exist. Warn policies record a metric and a log line and change no state. Spend is monotonic within a period, so only the rollover or a spec edit moves a namespace back; the reconciler recomputes the state from the current spec on every pass. The field is display truth: enforcement reads each gateway replica's live counter plus its peers' partials ([Budget state management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management)), and the gateway's admission decision also honors `clusterUSD`, which this state does not. A namespace blocked by the cluster ceiling reports `state: Normal`; issue #240 tracks it. Hard enforcement changes nothing in this state machine: the boundary region is a transient gateway admission mode, not a namespace state.

## Design notes

### Credential scoping

Credentials are referenced from the operator's namespace and read directly by the gateway there. They never leave that namespace or reach agent containers: an agent that wants to call an LLM goes through the gateway, which attaches the credential server-side ([Credential handling](../security/credentials.md)).

### Budget accounting

Budget state in status is the source of truth for display; each gateway replica holds an authoritative live counter that is folded into status periodically, because status updates are rate-limited and lossy ([Budget state management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management)).

Periods reset at midnight UTC: `monthly` on the first day of the calendar month, `weekly` on Monday, `daily` every day. The `Retry-After` on `429 budget_exhausted` is the seconds to the next reset. Setting `clusterUSD` without `perNamespaceUSD`, or the reverse, is supported: the unset ceiling is not enforced. When a block fires, `error.message` names which ceiling won ([LLM Gateway error responses](../gateways/api/errors.md#llm-gateway-error-responses)).

There is no pre-request cost estimation: a request's cost is knowable only after the response, so soft mode counts after the fact within its stated bound, and hard mode bounds the crossing with serialized admission rather than estimates.

### Glob semantics in `allowedNamespaces`

Patterns use Go's [`path.Match`](https://pkg.go.dev/path#Match) rules: `*` matches any run of non-`/` characters. Namespace names contain no `/`, so `sandbox-*` matches `sandbox-foo` and `sandbox-foo-bar` alike. Prefer exact names where possible.

### Fallback trees

Each provider's `spec.fallback` list may name providers with lists of their own, forming a tree the gateway walks depth-first in declared order. The gateway-level `maxFallbackDepth` (default 3) bounds the total number of providers attempted per request, the primary included, not the nesting depth.

![A fallback tree rooted at anthropic-shared, whose two declared fallbacks are anthropic-backup and anthropic-overflow, and where anthropic-backup declares anthropic-eu and anthropic-apac. Depth-first declared order numbers the nodes: anthropic-shared is visit 1, anthropic-backup 2, anthropic-eu 3, anthropic-apac 4, and anthropic-overflow 5. With maxFallbackDepth 3 the first three visits are drawn as attempted and visits 4 and 5 greyed out, including anthropic-overflow although it sits one level below the primary.](../diagrams/fallback-tree.svg)

Follow the visit numbers, not the levels: `anthropic-overflow` is a direct child of the primary and is still cut, because the walk reaches it fifth and the three slots are gone. How a budget-blocked primary, a budget-blocked fallback, and an ineligible candidate each affect the walk, and how exhaustion maps to error codes, are on [Fallback logic](../gateways/llm/fallback.md).

Each `spec.fallback[]` entry is a name and an optional `modelMap` (rule 41). Rule 12 governs which types may reference which: `anthropic` and `openai` or `openai-compatible` may cross in either direction, with the gateway translating at the crossing; `google-vertex` chains stay same-type. The map lives on the edge rather than on the provider because the same fallback can serve different primaries under different names, and because the primary is the resource the platform team edits when they add a backup ([Crossing formats](../gateways/llm/fallback.md#crossing-formats)).

Rule 11 rejects a circular tree. As shipped the walk marks each provider visited once for the whole tree, so two providers that name the same backup are also reported as circular; issue #240 tracks it.

### Cost fields are strings

Cost fields are decimal strings, not floats, to avoid precision loss. The gateway parses them as decimals.

### `degradeTo` validation

Every `degradeTo` must name a model in the same provider's catalog (rule 18, `Ready=False, reason=InvalidDegradeTarget`). On every pass the reconciler also runs a cost sanity check: it averages each model's input and output prices and, when the degrade target is not the cheapest, sets the `DegradeTargetNotCheapest` condition and emits a `Warning` event with `reason=DegradeTargetNotCheapest` naming the cheaper model. The event fires once, when the condition turns `True`, not on every pass. The check is advisory and does not affect `Ready`, since a platform team may prefer a target for latency or capability; it catches the common misconfiguration where a policy labeled "degrade" raises cost at the threshold ([ModelProviderReconciler](../controller/reconcilers.md#modelproviderreconciler)).

### Deletion

A ModelProvider is held in deletion while any Agent, AgentTask, or AgentClass references it; the finalizer releases when the last reference goes away ([Finalizers](../controller/finalizers.md)).
