# ModelProvider

ModelProvider is a cluster-scoped resource that defines a managed LLM provider. It holds a reference to a Secret with credentials, a model catalog, budgets, request and token rate limits, a fallback list, and the namespace tenancy check. The gateway enforces the catalog, the budgets, the rate limits, and the fallback walk on every request it routes; the controller validates the spec, probes the upstream, and folds spend into status.

Because it is cluster-scoped, a ModelProvider is a platform-team resource: application teams reference it from their namespaces, and only the namespaces listed in `spec.allowedNamespaces` may do so. `allowedNamespaces` is the tenancy check that applies to every caller, in both adoption tiers ([Provider access checks](../concepts/tenancy-and-tiers.md#provider-access-checks)).

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
  # google-vertex type is reserved on Request handling); only the
  # controller's liveness probe mints Vertex tokens (see The google-vertex
  # probe on ModelProviderReconciler).
  type: anthropic

  # Required. The schema pattern ^https:// rejects any other scheme: the
  # gateway forwards the credential to this URL. Its hostname must be
  # approved by the credential Secret (rule 50).
  endpoint: "https://api.anthropic.com"

  # Required. A Secret key in the operator namespace. For google-vertex the
  # value is a GCP service-account JSON key, not a static API key (see The
  # google-vertex probe on ModelProviderReconciler). The Secret must carry
  # the label kaalm.io/provider-credential: "true" (rule 49) and list the
  # endpoint host in its kaalm.io/provider-hosts annotation (rule 50).
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
      # a request; the reconciler sets the MaxOutputTokensUnset condition
      # on the primary that maps to it.
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
    # Cluster-wide ceiling per (namespace, model); see Rate limit scope.
    requestsPerMinute: 300
    # Cluster-wide ceiling on input plus output tokens per minute, per
    # (namespace, model). Unset or 0 means no token limit.
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
    - namespace: "team-support"
      period: "2026-03"
      spentUSD: "301.20"
      percentUsed: 60
      state: "Normal"
  clusterSpentUSD: "699.50"
```

| Condition | Meaning |
|---|---|
| `Ready` | The spec is valid and the credential resolves. `True` with `reason: CredentialsValid`. `False` with one of `CredentialsMissing` (the Secret or key is absent or empty), `SecretNotOptedIn` (the Secret lacks the label `kaalm.io/provider-credential: "true"`, rule 49), `EndpointHostNotApproved` (the Secret's `kaalm.io/provider-hosts` annotation omits the `spec.endpoint` host, rule 50), `CredentialsInvalid` (the probe was refused with a 401 or 403), `FallbackIneligible` (rules 11 and 12), `InvalidDegradeTarget` (rule 18), `InvalidModelMap` (rule 41), `HardBudgetUnpriced` (rule 33), `InvalidNamespacePattern` (an `allowedNamespaces` entry is not a valid pattern, rule 51), or `DeletionBlocked` while a delete waits on a referrer ([Cluster-scoped resources](../controller/finalizers.md#cluster-scoped-resources)). Each reason sends a `Warning` event with the same reason when it first appears on `Ready` ([Event emission](../controller/operations.md#event-emission)). |
| `Healthy` | The periodic upstream probe, run against every provider type including `google-vertex`. `True` with `UpstreamReachable`; `False` with `ProviderUnhealthy` and a `Warning` event on every failing probe, or with `CredentialsInvalid` when the probe itself is refused. `Unknown` with `NotProbed` when the last pass ended before the probe: a credential or configuration check set `Ready=False`, `healthCheck.enabled` is `false`, or a delete is held. The message names which, and no event is sent. `True` and `False` come from the latest probe, and the passes between probes keep them ([When the probe runs](../controller/reconcilers/modelprovider.md#when-the-probe-runs)). |
| `GatewayReachable` | `True` with `GatewayReady` when at least one gateway Pod is Ready and not being deleted, else `False` with `GatewayUnavailable`. Set on every pass, a held delete included, and refreshed at once when a gateway Pod's readiness changes. Every ModelProvider shows the same value, whether or not it passes its checks and whether or not its delete is held. |
| `FallbackIneligible` | Advisory; never affects `Ready`. `True` with `reason: FallbackIneligible` when the reconcile-time scan finds a fallback candidate that a caller's namespace or model can never reach; `False` with `AllCandidatesEligible` once the findings clear. A `Warning` event with the same reason names each finding when it is added. A provider with no findings carries no such condition ([Reconcile-time fallback eligibility scan](../controller/reconcilers/modelprovider.md#reconcile-time-fallback-eligibility-scan)). |
| `DegradeTargetNotCheapest` | Advisory; never affects `Ready`. `True` with `CheaperModelAvailable` when a degrade policy's `degradeTo` costs more than another priced model in the catalog, `False` with `DegradeTargetCheapest` once none does. The message lists every such target. One `Warning` event fires on the transition to `True` ([`degradeTo` validation](#degradeto-validation)). |
| `MaxOutputTokensUnset` | Advisory; never affects `Ready`. `True` with `MaxOutputTokensUnset` when a fallback edge from an `openai` or `openai-compatible` provider into an `anthropic` provider reaches models that declare no `maxOutputTokens`. The message lists each `provider/model` in sorted order. `False` with `MaxOutputTokensDeclared` once none remain. A provider with no findings carries no such condition. The `Warning` event fires on the transition to `True` ([What does not translate](../gateways/llm/fallback.md#what-does-not)). |
| `BoundaryMarginRaised` | Hard enforcement only. `True` when a gateway replica observed traffic that needed a wider boundary margin than `hard.boundaryMarginPercent` configures ([Hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement)). |

`healthCheck.enabled: false` disables the probe and leaves `Healthy` `Unknown` with `NotProbed`; `intervalSeconds` (default 60) sets its cadence for a healthy provider and `timeoutSeconds` (default 10) bounds each request. A failing probe requeues on a backoff instead of the plain interval ([Probe backoff](../controller/reconcilers/modelprovider.md#probe-backoff)). `budgetUsage` is per-namespace spend. It holds the current period's entries and, from the first pass after a period rollover until the next rollover, the previous period's entries, so two periods can appear for a whole period. Each entry's `period` tells them apart; select on it when you sum or filter rows. `clusterSpentUSD` is the sum across namespaces for the current period only. Both are empty while the provider tracks no budget (`period: none`), and turning a budget off clears them, the previous-period entries included ([Budget reconciliation](../controller/reconcilers/modelprovider.md#budget-reconciliation) covers the details and the missing-ConfigMap case).

A previous-period entry's `state` is always `Normal`, because the gateway enforces only the current period. An alert or script that matches `state: Blocked` therefore still means blocked now. Its `percentUsed` is the period's final ratio against the ceilings as they stand now, so a ceiling edit since then explains a percentage that differs from what was enforced at the time.

Each current-period `budgetUsage` entry's `state` is a per-namespace state machine:

![Per-namespace budget state for one ModelProvider and one period. The period opening enters Normal. Normal moves to Throttled when spend reaches a degrade policy's atPercent. Normal or Throttled move to Blocked when spend reaches a block policy's atPercent. A period rollover, or a spec edit that raises the ceiling or changes the policies, moves Throttled or Blocked back to Normal.](../diagrams/budget-namespace-states.svg)

The state is derived from `percentUsed` and the highest policy threshold at or below it. `percentUsed` is the worse of two ratios: the namespace's spend against `perNamespaceUSD`, and the provider's cluster-wide spend against `clusterUSD`. An unset ceiling adds no ratio. This is the same rule the gateway's admission decision applies, so a namespace under its own ceiling that the cluster ceiling blocks reports `state: Blocked`. The state is `Throttled` for a degrade policy, `Blocked` for a block policy, `Normal` when no threshold is crossed or no policies exist. Warn policies record a metric and change no state. Spend is monotonic within a period, so only the rollover or a spec edit moves a namespace back. Hard enforcement changes nothing in this state machine: the boundary region is a transient gateway admission mode, not a namespace state.

## Design notes

### Credential scoping

Credentials are referenced from the operator's namespace and read directly by the gateway there. They never leave that namespace or reach agent containers: an agent that wants to call an LLM goes through the gateway, which attaches the credential server-side ([Credential handling](../security/credentials.md)). Only a Secret that opted in with the label and listed the endpoint host (rules 49 and 50) may serve as a credential, so a provider cannot name an arbitrary Secret in `kaalm-system` or send a credential to a host that the credential manager did not approve ([Provider credentials](validation/providers.md#provider-credentials)).

### Budget accounting

Budget state in status is the source of truth for display only. Enforcement reads each gateway replica's live counter plus its peers' published partials; status is folded from them periodically, because status updates are rate-limited and lossy ([Budget state management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management)).

Periods reset at midnight UTC: `monthly` on the first day of the calendar month, `weekly` on Monday, `daily` every day. The `Retry-After` on `429 budget_exhausted` is the seconds to the next reset. When a block fires, `error.message` names which ceiling won ([LLM Gateway error responses](../gateways/api/errors.md#llm-gateway-error-responses)).

There is no pre-request cost estimation: a request's cost is knowable only after the response, so soft mode counts after the fact within its stated bound, and hard mode bounds the crossing with serialized admission rather than estimates.

### Rate limit scope

Both `rateLimits` fields are cluster-wide ceilings per (namespace, model), so N namespaces, or N models in one namespace, each get the full ceiling. `tokensPerMinute` therefore does not keep a shared provider key under the provider's own tokens-per-minute limit. The token limit is enforced after the call, so a large call blocks the next request, not itself ([Rate limiting](../gateways/llm/budgets-and-rate-limits.md#rate-limiting)).

Each gateway replica enforces its share of a ceiling, so the split is approximate. When `requestsPerMinute` is lower than the replica count, a burst can admit up to one request per replica while the long-run rate stays at the limit ([Request limits below the replica count](../gateways/llm/budgets-and-rate-limits.md#request-limits-below-the-replica-count)).

### Glob semantics in `allowedNamespaces`

Patterns use Go's [`path.Match`](https://pkg.go.dev/path#Match) rules: `*` matches any run of non-`/` characters. Namespace names contain no `/`, so `sandbox-*` matches `sandbox-foo` and `sandbox-foo-bar` alike.

A malformed pattern, such as `[`, matches nothing, and the provider shows `Ready=False, reason: InvalidNamespacePattern` naming the entry ([rule 51](validation/references-and-access.md#access-checks-on-providers-and-tools)). The valid entries keep admitting their namespaces.

### Fallback trees

Each provider's `spec.fallback` list may name providers with lists of their own, forming a tree the gateway walks depth-first in declared order. The gateway-level `maxFallbackDepth` (default 3) bounds the total number of providers attempted per request, the primary included, not the nesting depth.

![A fallback tree rooted at anthropic-shared, whose two declared fallbacks are anthropic-backup and anthropic-overflow, and where anthropic-backup declares anthropic-eu and anthropic-apac. Depth-first declared order numbers the nodes: anthropic-shared is visit 1, anthropic-backup 2, anthropic-eu 3, anthropic-apac 4, and anthropic-overflow 5. With maxFallbackDepth 3 the first three visits are drawn as attempted and visits 4 and 5 greyed out, including anthropic-overflow although it sits one level below the primary.](../diagrams/fallback-tree.svg)

Follow the visit numbers, not the levels: `anthropic-overflow` is a direct child of the primary and is still cut, because the walk reaches it fifth and the three slots are gone. How a budget-blocked primary, a budget-blocked fallback, and an ineligible candidate each affect the walk, and how exhaustion maps to error codes, are on [Fallback logic](../gateways/llm/fallback.md).

Each `spec.fallback[]` entry is a name and an optional `modelMap` (rule 41). Rule 12 governs which provider types may reference which ([Crossing formats](../gateways/llm/fallback.md#crossing-formats)). The map lives on the edge rather than on the provider because the same fallback can serve different primaries under different names, and because the primary is the resource the platform team edits when they add a backup.

Rule 11 rejects a circular tree: a provider that appears among its own ancestors. A backup that two branches share is not circular. The gateway attempts it once per request, at its first position in the walk.

### Cost fields are strings

Cost fields are decimal strings, not floats, to avoid precision loss.

### `degradeTo` validation

Every `degradeTo` must name a model in the same provider's catalog (rule 18, `Ready=False, reason=InvalidDegradeTarget`). On every pass that gets past the credentials check, including a pass that fails a `Ready` check ([What it checks](../controller/reconcilers/modelprovider.md#what-it-checks)), the reconciler also runs a cost check: it averages each model's input and output prices and, when a degrade target costs more than another priced model, sets the `DegradeTargetNotCheapest` condition and emits one `Warning` event with `reason=DegradeTargetNotCheapest` when the condition turns `True`. Both name every such `degradeTo` target with the cheapest priced model. A target tied for the lowest average, or without both prices, is not listed. The check is advisory and does not affect `Ready`, since a platform team may prefer a target for latency or capability; it catches the misconfiguration where a policy labeled "degrade" raises cost at the threshold ([Cost sanity on degradeTo](../controller/reconcilers/modelprovider.md#cost-sanity-on-degradeto)).

### Deletion

A ModelProvider is held in deletion while any Agent, AgentTask, or AgentClass references it; the finalizer releases when the last reference goes away ([Finalizers](../controller/finalizers.md)).

Deleting a ModelProvider also removes the spend recorded for it. A ModelProvider created later under the same name starts with no spend, an empty `budgetUsage`, and no `Blocked` namespaces, even within the same period, so recreating a provider resets its budget for that period ([When a provider is deleted](../gateways/llm/budgets-and-rate-limits.md#when-a-provider-is-deleted)).
