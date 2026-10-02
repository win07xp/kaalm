# ModelProviderReconciler

This page specifies the ModelProviderReconciler: what one pass does, and its liveness probe, budget reconciliation, fallback validation, and cost check. What it watches is in [Reconcilers](../reconcilers.md#what-each-reconciler-watches).

1. **Credentials.** Read the Secret named by `spec.credentialsRef` from the operator namespace only. A missing Secret sets `Ready=False, reason=CredentialsMissing`. A Secret without the label `kaalm.io/provider-credential: "true"` sets `reason=SecretNotOptedIn` (rule 49). A Secret whose `kaalm.io/provider-hosts` annotation does not list the `spec.endpoint` host sets `reason=EndpointHostNotApproved` (rule 50). A missing or empty key sets `reason=CredentialsMissing`. Any of these ends the pass before the rest of the steps; [Provider credentials](../../resources/validation/providers.md#provider-credentials) states the rules and what the early end protects. A change to the Secret, including its label or annotation, re-evaluates every provider that names it at once.
2. **Configuration validation.** Three checks can set `Ready=False` and end the pass. The message lists every problem found, and the reason is `InvalidDegradeTarget` if any degrade target is bad, else `HardBudgetUnpriced`, else `InvalidModelMap`, else `FallbackIneligible`.
    - The fallback chain ([Fallback chain validation](#fallback-chain-validation)): a violation of rule 11 or 12 gives `reason=FallbackIneligible`, and a violation of rule 41 gives `reason=InvalidModelMap`.
    - The degrade targets: every `budget.policies[].degradeTo` must name a model in `spec.models`, else `reason=InvalidDegradeTarget`.
    - Hard-budget pricing ([rule 33](../../resources/validation/providers.md)): else `reason=HardBudgetUnpriced`.

    Three advisory checks never touch `Ready`: the [degrade-target cost check](#cost-sanity-on-degradeto), the `MaxOutputTokensUnset` check (see [Fallback chain validation](#fallback-chain-validation)), and the [reconcile-time fallback eligibility scan](#reconcile-time-fallback-eligibility-scan). The cost check and the `MaxOutputTokensUnset` check run on every pass that reaches this step, including a pass that fails a `Ready` check above. The eligibility scan runs only when a provider declares `spec.fallback` and every `Ready` check passes, so a failing pass leaves its `FallbackIneligible` condition as it was.
3. **Gateway mirror.** Set `GatewayReachable=True` when at least one gateway Pod in `kaalm-system` is Ready, else `False`. The condition is cluster-wide and mirrored onto every ModelProvider for `kubectl describe`. A gateway Pod becoming Ready or not Ready re-enqueues every ModelProvider at once, so a gateway outage or recovery shows without waiting for the next periodic pass.
4. **Budget fold.** Reduce the gateway replicas' partial spend into the canonical totals; see [Budget reconciliation](#budget-reconciliation). The same step folds the per-agent spend counters from `kaalm-agentspend-{name}` and writes `status.clusterSpentUSD` and the `kaalm_provider_budget_canonical_usd` gauge.
5. **Liveness probe**, when `healthCheck.enabled` (the default): see [Liveness probe](#liveness-probe).
6. **Ready.** Set `Ready=True, reason=CredentialsValid`.

The pass requeues at the probe's next delay ([Probe backoff](#probe-backoff)) when the probe ran, and every minute otherwise for a provider with a budget period, so the fold keeps running with the probe disabled. The reconciler never distributes credentials to agent Pods: the gateway reads them from `kaalm-system` itself.

## Liveness probe

The probe uses the provider's model-list endpoint, which requires authentication but consumes no tokens. It sends `GET {spec.endpoint}/v1/models` and carries the credential in the headers for the provider type:

| `spec.type` | Credential headers |
|---|---|
| `anthropic` | `x-api-key: <key>` and `anthropic-version: 2023-06-01` |
| `openai`, `openai-compatible` | `Authorization: Bearer <key>` |

For `google-vertex` the probe mints an OAuth2 access token from the credential and requests the publisher-model list; see [The google-vertex probe](#the-google-vertex-probe). Every provider type is probed. An `openai-compatible` server that expects a different header rejects the probe, which shows as `CredentialsInvalid` on a `401` or `403`.

The probe never follows a redirect, because Go's cross-host header stripping does not cover `x-api-key` and a redirecting endpoint is not a healthy one. A redirect counts as a transient error, so it takes the last row of the following table. Each probe is bounded by `healthCheck.timeoutSeconds` (default 10s). The probe is a second credential egress, apart from the gateway's data path; [Health probes are a second credential egress](../../security/credentials.md#health-probes-are-a-second-credential-egress) states its bound.

| Outcome | Conditions | Requeue |
|---|---|---|
| `2xx` | `Healthy=True, reason=UpstreamReachable` | the healthy interval |
| `401` or `403` (for `google-vertex`, also an unusable credential or a token endpoint that refuses it) | `Healthy=False` and `Ready=False`, both `reason=CredentialsInvalid`, and a `Warning` event when `Ready` first takes that reason; the pass ends | the failing backoff |
| other error, network failure, or `5xx` | `Healthy=False, reason=ProviderUnhealthy` and a `Warning` event on every failing pass | the failing backoff |

The 401 and 403 classification matches the credential-problem class in [Fallback triggers](../../gateways/llm/fallback.md#fallback-triggers).

## Probe backoff

A probe whose `Healthy` condition is not `False` requeues at `healthCheck.intervalSeconds` (default 60). A failing probe (`Healthy=False`, reason `ProviderUnhealthy` or `CredentialsInvalid`) backs off: each periodic failure doubles the wait (interval, 2x, 4x, 8x, and so on), capped at ten intervals or ten minutes, whichever is smaller, and never below the interval. With the default 60-second interval the cap is 10 minutes. A controller restart keeps the backoff, because it is read from the `Healthy` condition's `lastTransitionTime`. One successful probe returns the provider to the plain interval.

The backoff sets only the periodic requeue. A credential Secret change, a spec change, a referencing workload change, a budget ConfigMap change, or a gateway Pod readiness change still re-enqueues the provider at once, so a fixed credential or endpoint takes effect without waiting out the backoff. The same backoff governs the ToolProvider probe ([ToolProviderReconciler](toolprovider.md)).

## The google-vertex probe

For `spec.type: google-vertex` the credential Secret holds a GCP service-account JSON key: `type: service_account`, with `project_id`, `client_email`, and a PEM RSA `private_key`, and an optional `token_uri`, which must be an `https` URL and defaults to `https://oauth2.googleapis.com/token`.

The probe mints an OAuth2 access token (scope `https://www.googleapis.com/auth/cloud-platform`) with a JWT bearer grant signed by the key, then sends `GET {endpoint}/v1/projects/{project}/locations/{location}/publishers/google/models` to `spec.endpoint` with the token as a bearer credential. `project` is the key's `project_id`. `location` is the `{location}` of a regional `{location}-aiplatform.googleapis.com` endpoint host, and `global` for any other host, including the global `aiplatform.googleapis.com` endpoint. The private key signs the assertion and is never sent. The probe sends the assertion to the key's `token_uri` and the access token to `spec.endpoint`, so those two hosts bound where this credential goes.

A 2xx response from Vertex sets `Healthy=True`. `CredentialsInvalid` covers a 401 or 403 from Vertex, a token endpoint that refuses the key (HTTP 401, 403, or a 400 carrying OAuth2 error `invalid_grant` or `invalid_client`), and a Secret value that is not a usable service-account key. Any other failure (a network error, a 5xx, or another token-endpoint error) sets `ProviderUnhealthy`.

Only the controller's liveness probe mints Vertex tokens. The gateway does not serve the `google-vertex` type; see [The google-vertex type is reserved](../../gateways/llm/request-handling.md#the-google-vertex-type-is-reserved).

## Probe TLS trust

Probes trust the system roots plus the gateway's upstream trust (`gateway.trustClusterCAForUpstream` and `gateway.upstreamCA`), so they trust what forwarding trusts, plus anything the deprecated `controller.trustClusterCAForProbes` and `controller.probeCA` values add. [Deployment](../../operations/deployment.md#configuration-reference) lists the values.

The pool is additive and follows rotation without a restart. The same pool serves the ModelProvider and ToolProvider probes, including the Vertex token request. An in-cluster endpoint under a private CA can therefore probe `Healthy` instead of failing every handshake. Probes honor `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` from the controller's environment.

## Budget reconciliation

The reconciler reads the per-replica partial spend counters from the provider's budget ConfigMap in `kaalm-system`; see [Budget state management](../../gateways/llm/budgets-and-rate-limits.md#budget-state-management) for the format.

Before summing, it prunes the entries left by scaled-down or replaced gateway replicas, folding each pruned current-period entry into the `_retired` accumulator first so published spend survives rollouts (required under [hard enforcement](../../gateways/llm/budgets-and-rate-limits.md#hard-enforcement)). It sums the remaining partials plus `_retired`, writes the canonical total to the `_canonical` key, and updates `status.budgetUsage` per namespace. A replica's `_marginExceeded` flag sets the `BoundaryMarginRaised` condition, cleared with `reason=MarginSufficient` when no replica reports it; the matching `Warning` event fires once, when the condition first turns on.

On budget period rollover, the previous period's totals are archived to status; see [The reducer](../../gateways/llm/budgets-and-rate-limits.md#the-reducer). Rollover is processed on the next pass after the boundary, so the lag from the boundary to the archive write is at most one requeue interval, which is acceptable for a display and restart-seed value that per-request enforcement never waits on.

## Fallback chain validation

The reconciler walks the full fallback chain from each provider's `spec.fallback` and confirms that no reference is circular (rule 11), that every referenced provider exists, and that every edge satisfies rule 12 (the same `spec.type`, or a crossing the gateway translates) and rule 41 (every `modelMap` key names one of the provider's own models and every value one of the fallback's). A violation sets `Ready=False`, with `reason=InvalidModelMap` for a bad map and `reason=FallbackIneligible` otherwise. A change to any ModelProvider re-enqueues every other provider that declares a fallback, so a chain recovers when a missing provider is created or a bad one is fixed. A cross-format hop into an Anthropic model with no `maxOutputTokens` also sets the advisory `MaxOutputTokensUnset` condition to `True`, listing the models, and emits a `MaxOutputTokensUnset` `Warning` event when the condition turns `True`. The condition goes `False` with `reason=MaxOutputTokensDeclared` once no such model remains, and it never affects `Ready`. The reconciler validates the whole chain whatever `maxFallbackDepth` is, because the cap is a gateway-level setting that can change without re-reconciling providers.

## Reconcile-time fallback eligibility scan

Once a provider's fallback tree passes the structural checks above and it declares `spec.fallback`, the reconciler scans the tree for candidates a request could never reach: the reconcile-time counterpart of the gateway's request-time checks under [Per-candidate checks](../../gateways/llm/fallback.md#per-candidate-checks).

**Callers.** The namespaces of the Agents and AgentTasks whose `spec.providers[].providerRef` names the primary, kept only if the primary's own `allowedNamespaces` admits them. A primary with no callers still runs the model check below, with no namespace check.

**The walk.** For each caller namespace and each of the primary's `spec.models[].id`, the reconciler walks the fallback tree the way the gateway walks it for one request, and applies the gateway's two per-candidate configuration checks to every candidate it reaches: the candidate's `allowedNamespaces` admits the namespace, and the candidate's `spec.models` offers the model the walk carries to it (the primary's model, rewritten by each edge's `modelMap` entry on the way down). An ineligible candidate's own fallbacks are not walked, because a request never reaches them.

**Findings and the condition.** A finding names a fallback that does not admit a caller namespace, or does not offer a model. The `FallbackIneligible` condition lists the findings in its message: `True, reason=FallbackIneligible` when there are findings, `False, reason=AllCandidatesEligible` once they clear. A provider that never had a finding carries no such condition. A `Warning` event of the same reason fires on the primary when the scan adds a finding, and names only the added findings; an unchanged or smaller set, and clearing, send none. Additions are announced because other people's actions can add findings, for example a new team's Agents that use the primary from a namespace a fallback does not admit, and whoever fixes a finding already knows. The scan never touches `Ready`: `Ready=False, reason=FallbackIneligible` stays the structural rule 11, 12, or existence failure above.

A change to any ModelProvider re-enqueues every other provider that declares a fallback, and a change to a referencing Agent or AgentTask re-enqueues the providers it references, so the scan re-runs when a caller or a candidate's configuration changes. The gateway's request-time check still runs on every request regardless: it catches a configuration change between reconcile passes and a crossing request whose body the target format cannot express, a check only the gateway can make.

## Cost sanity on degradeTo

For each policy whose `action: degrade`, the reconciler computes `avgCost(model) = (costPer1MInputTokens + costPer1MOutputTokens) / 2` for the target and compares it with the other models in `spec.models`. A model is priced when both prices parse as decimals, by the same parse the gateway ledger uses. Only priced models are candidates for cheapest.

A target is a finding when it is priced and some priced model has a strictly lower average. A target tied for the lowest average is not a finding. A target without both prices is skipped, because its cost is unknown; [rule 33](../../resources/validation/providers.md) requires full pricing under hard budget enforcement.

With any finding, the reconciler sets the `DegradeTargetNotCheapest` condition to `True` with `reason=CheaperModelAvailable`. Its message names each distinct target that is a finding once, even when two policies name it, with its average cost and the cheapest priced model's.

The `Warning` event with `reason=DegradeTargetNotCheapest` fires once, when the condition turns `True`, and carries the condition message. While the condition stays `True`, a changed list of findings updates the condition message and sends no event, the same as `MaxOutputTokensUnset` and unlike the [eligibility scan](#reconcile-time-fallback-eligibility-scan), which warns about each finding it adds. A steady misconfiguration therefore emits one event, not one per pass. When no target costs more than a priced model, the condition goes `False` with `reason=DegradeTargetCheapest` and sends nothing.

A degrade policy that points at a more expensive model is almost always a misconfiguration, but the check is advisory and never sets `Ready=False`, because platform teams may have non-cost reasons (latency, capability, quality) to prefer a degrade target. A `degradeTo` that names a model missing from the catalog is skipped, since that model has no price; that target still gets the `InvalidDegradeTarget` failure (rule 18).
