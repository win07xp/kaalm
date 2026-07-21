# ModelProvider

ModelProvider is a cluster-scoped resource that defines a managed LLM provider. It holds a reference to a Secret with credentials, configures rate limits and budgets, and controls which namespaces may use it.

Because it is cluster-scoped, a ModelProvider is a platform-team resource: application teams reference it from their namespaces, but only the namespaces listed in `spec.allowedNamespaces` may do so. The gateway enforces every limit defined here (model allowlist, budgets, rate limits, fallback) on each request it routes.

`allowedNamespaces` is the one tenancy gate that every caller faces, in both adoption tiers. For where it sits relative to the class-level and workload-level gates, and which error each one returns, see [Provider access gating](../concepts/tenancy-and-tiers.md#provider-access-gating).

## Spec

The annotated example below shows every spec field.

```yaml
apiVersion: agentry.io/v1alpha1
kind: ModelProvider
metadata:
  name: anthropic-shared
spec:
  # Provider type. Built-in: "anthropic" | "openai" | "google-vertex" | "openai-compatible"
  type: anthropic

  # Endpoint override (for self-hosted or custom gateways). Optional for known types.
  # Must use https://, because the gateway forwards provider credentials to this URL;
  # a non-TLS scheme would leak them in cleartext. CRD schema enforces this via
  # x-kubernetes-validations:
  #   - rule: "self.startsWith('https://')"
  #     message: "endpoint must use https"
  endpoint: "https://api.anthropic.com"

  # Credentials: a reference to a Secret in the operator's namespace.
  # The gateway reads this directly from agentry-system; credentials
  # never leave that namespace.
  credentialsRef:
    name: anthropic-api-key
    key: api-key

  # Models offered through this provider. The gateway validates that requested
  # models are in this list; unknown models are rejected.
  # Each entry's `id` must be unique within the provider: the gateway routes
  # by the qualified name `{providerRef}/{modelId}`, so duplicates would silently
  # win-last. Uniqueness is enforced structurally, not via CEL: the CRD schema
  # declares the list as a map keyed by id:
  #   x-kubernetes-list-type: map
  #   x-kubernetes-list-map-keys: ["id"]
  # so the apiserver rejects duplicate ids natively at apply time and
  # server-side apply merges entries by id. (A quadratic CEL rule such as
  # self.all(m, self.exists_one(n, n.id == m.id)) is not usable here: the
  # apiserver statically budgets worst-case CEL cost at CRD write time, and an
  # O(n²) walk over an unbounded array exceeds the per-rule cost limit,
  # rejecting the CRD itself.)
  models:
    - id: "claude-opus-4-6"
      displayName: "Claude Opus 4.6"
      costPer1MInputTokens:  "15.00"
      costPer1MOutputTokens: "75.00"
    - id: "claude-sonnet-4-6"
      displayName: "Claude Sonnet 4.6"
      costPer1MInputTokens:  "3.00"
      costPer1MOutputTokens: "15.00"

  # Which namespaces may reference this provider.
  # "*" matches all namespaces. Empty list = no namespaces (provider is inert).
  allowedNamespaces:
    - "team-support"
    - "team-ml"
    - "sandbox-*"   # glob supported

  # Budget enforcement. Budgets are tracked per namespace, per calendar period.
  budget:
    # "monthly" (calendar month) | "daily" | "weekly" | "none"
    period: monthly
    perNamespaceUSD: "500.00"
    # Enforcement policy applied as budget is consumed.
    policies:
      - atPercent: 80
        action: degrade
        degradeTo: "claude-sonnet-4-6"   # model to downgrade to
      - atPercent: 100
        action: block    # "block" | "warn" | "degrade"
    # Cluster-wide ceiling (sum across all namespaces). Optional.
    clusterUSD: "10000.00"

  # Rate limits enforced at the gateway (per namespace, cluster-wide ceiling).
  # Buckets are keyed per (namespace, model): each (namespace, model) pair
  # carries the full configured ceiling independently, so a namespace's
  # aggregate throughput against this provider can reach ceiling × the number
  # of models it uses (see Rate Limiting).
  # Each gateway replica divides these values by the number of active replicas
  # and enforces its share independently. The configured value represents the
  # intended cluster-wide limit regardless of replica count.
  rateLimits:
    requestsPerMinute: 300
    tokensPerMinute: 500000

  # Fallback chain. If this provider is unavailable (network error, 5xx,
  # timeout), the gateway tries the next provider in order. A budget-blocked
  # primary does NOT trigger fallback: the gateway returns 429
  # budget_exhausted immediately. A budget-blocked *fallback* candidate is
  # skipped (an attempt slot IS consumed) and its own `spec.fallback` children
  # are walked. If the entire walk is exhausted by error, budget-block, or
  # maxFallbackDepth, the gateway returns a fallback-exhausted error, never
  # 429: 502 provider_error in the general case, or 503/504 when every
  # attempt failed unreachable / timed out (see Depth cap
  # semantics for the failure-class mapping). The
  # gateway walks each fallback provider's own fallback chain, up to the
  # gateway-level maxFallbackDepth setting (default 3). Referenced providers
  # must also allow the namespace, and must carry the same spec.type as this
  # provider: there is no cross-format fallback (see Fallback trees below).
  # See Fallback Logic for the traversal algorithm.
  fallback:
    - name: anthropic-backup

  # Health check configuration.
  healthCheck:
    enabled: true
    intervalSeconds: 60
    timeoutSeconds: 10
```

## Status

```yaml
status:
  observedGeneration: 2
  conditions:
    - type: Ready
      status: "True"
      reason: CredentialsValid
      message: ""
    - type: Healthy
      status: "True"
      reason: UpstreamReachable
      lastProbeTime: "2026-04-05T12:00:00Z"
  budgetUsage:
    - namespace: "team-support"
      period: "2026-04"
      spentUSD: "287.50"
      percentUsed: 57
      state: "Normal"    # "Normal" | "Throttled" | "Blocked"
    - namespace: "team-ml"
      period: "2026-04"
      spentUSD: "412.00"
      percentUsed: 82
      state: "Throttled"
  clusterSpentUSD: "699.50"
```

Two conditions summarize provider health: `Ready` reports whether the spec is valid and credentials check out, and `Healthy` reports the result of the periodic upstream probe. The probe runs by default; set `healthCheck.enabled: false` to disable it (for example for a provider type with no probe, or an offline test fixture). `healthCheck.intervalSeconds` sets the probe cadence (default 60) and `healthCheck.timeoutSeconds` bounds each probe request (default 10). `budgetUsage` shows per-namespace spend for the current period, and `clusterSpentUSD` shows the sum across all namespaces.

## Design Notes

### Credential scoping

Credentials are referenced from the operator's namespace and read directly by the gateway in `agentry-system`. They never leave that namespace or reach agent containers. This keeps provider API keys out of every application namespace: an agent that wants to call an LLM must go through the gateway, which attaches the credentials server-side.

### Budget accounting

Budget state persisted in status is the source of truth for display, but the gateway maintains a local authoritative counter that is synced to status periodically. This matters because status updates are rate-limited and lossy. See [Budget State Management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management).

Budget periods reset at midnight UTC:

- `monthly` resets on the first day of the UTC calendar month at 00:00.
- `weekly` resets Monday 00:00 UTC.
- `daily` resets at 00:00 UTC.

Per-replica rollover detection, archival of previous-period totals to status, and the underestimate behavior during rollover are documented in [Budget State Management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management). The `Retry-After` header on `429 budget_exhausted` ([LLM Gateway error responses](../gateways/api/errors.md#llm-gateway-error-responses)) is the delta-seconds to the next reset.

### Budget enforcement hierarchy

Every routed request checks both `clusterUSD` (sum across namespaces) and `perNamespaceUSD` (the calling namespace's share). The request is blocked with `429 budget_exhausted` if either `clusterSpent + cost > clusterUSD` OR `nsSpent + cost > perNamespaceUSD`. `error.message` names which ceiling fired (`"cluster budget exhausted"` vs `"namespace budget exhausted: <ns>"`) so operators can attribute the block. `Retry-After` is the delta-seconds to the next period reset (see the boundary rules above). Setting `clusterUSD` without `perNamespaceUSD` (or vice versa) is supported: the unset ceiling is simply not enforced. See [Budget State Management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management) for replica-side accounting.

### Glob semantics in `allowedNamespaces`

Globs support common patterns like `team-*`, using Go's [`path.Match`](https://pkg.go.dev/path#Match) rules: `*` matches any sequence of non-`/` characters and does not cross path separators. Since Kubernetes namespace names are DNS labels (no `/`), `*` behaves as expected: `sandbox-*` matches `sandbox-foo` but not `sandbox-foo-bar/sub`. Exact match is preferred where possible.

### Fallback trees

Fallback chains form a tree (each provider may have its own `spec.fallback` list) that the gateway walks **depth-first in declared order**. The gateway-level `maxFallbackDepth` cap (default 3) bounds the **total number of providers attempted per request, including the primary**, not the nesting depth of the tree. With the default, the gateway tries at most three providers before giving up, regardless of the tree's shape.

![A fallback tree rooted at the primary provider anthropic-shared, whose two declared fallbacks are anthropic-backup and anthropic-overflow, and where anthropic-backup itself declares anthropic-eu and anthropic-apac. Depth-first declared order numbers the nodes: anthropic-shared is visit 1, anthropic-backup visit 2, anthropic-eu visit 3, anthropic-apac visit 4 and anthropic-overflow visit 5. With maxFallbackDepth 3 the first three visits consume the attempt slots and are drawn as attempted, while visits 4 and 5 are drawn greyed out and never attempted, including anthropic-overflow despite it sitting only one level below the primary.](../diagrams/fallback-tree.svg)

**Reading the diagram.** Follow the visit numbers, not the levels. `anthropic-overflow` is a direct child of the primary, one level up from `anthropic-eu`, and it is still cut, because the depth-first walk reaches it fifth and the three attempt slots are already gone. That is what "bounds the providers attempted, not the nesting depth" means in practice. The two notes cover the asymmetry that catches people out: a budget-blocked *primary* ends the request at `429 budget_exhausted` before the tree is walked at all, while a budget-blocked *fallback* silently costs an attempt slot and still has its children visited.

Circular references are rejected by validation. All providers in the chain must have the **same `spec.type`** as the primary provider (e.g., all `anthropic` or all `openai-compatible`). Cross-format fallback is not supported in v1: the gateway does not translate between API formats.

The depth cap is a gateway-level operational setting (not per-ModelProvider) because it bounds request latency for the entire cluster. See [Fallback Logic](../gateways/llm/fallback.md) for the traversal pseudocode and [Depth cap semantics](../gateways/llm/fallback.md#depth-cap-semantics) for how exhaustion maps to error codes.

### Cost fields are strings

Cost fields are strings (not floats) to avoid precision issues. The gateway parses them as decimals.

### `degradeTo` validation

Every `degradeTo` value in `budget.policies` must reference a model `id` in the same provider's `spec.models` list. The ModelProviderReconciler validates this and sets `Ready=False, reason=InvalidDegradeTarget` if violated. See validation rule 18 in [Cross-Resource Validation](validation-and-defaulting.md#cross-resource-validation).

After the existence check passes, the reconciler also runs a cost sanity check: it computes `(costPer1MInputTokens + costPer1MOutputTokens) / 2` for the degrade target and compares it against the same metric for every other model in `spec.models`. If the target is not strictly the cheapest, the reconciler emits a `Warning` event (`reason=DegradeTargetNotCheapest`) on the ModelProvider naming the cheaper alternative. This is **advisory only**: it does not block `Ready=True`, since platform teams may have non-cost reasons to prefer a particular degrade target (latency, capability, quality). The check catches the common misconfiguration where a policy labelled "degrade" silently escalates cost at the budget threshold. See [ModelProviderReconciler](../controller/reconcilers.md#modelproviderreconciler).
