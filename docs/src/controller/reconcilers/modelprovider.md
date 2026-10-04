# ModelProviderReconciler

## What it's for

The ModelProviderReconciler decides whether a [ModelProvider](../../resources/modelprovider.md) is valid: it checks the credential Secret, the fallback chain, and the budget configuration, and probes the upstream. It also mirrors gateway readiness, reduces the gateway replicas' spend into canonical totals, and scans the fallback tree for candidates a request can never reach. It creates no objects. On delete it holds the provider while any Agent, AgentTask, or AgentClass references it, then releases the finalizer ([Cluster-scoped resources](../finalizers.md#cluster-scoped-resources)).

## What it owns and watches

Besides the `kaalm.io/provider-finalizer` finalizer and the provider's status, the reconciler writes the `_canonical` and `_retired` keys, and deletes the keys of gone replicas and past periods, in two ConfigMaps in `kaalm-system` that the gateway replicas fill: `kaalm-budget-{name}` and `kaalm-agentspend-{name}`.

It reads the credential Secret in `kaalm-system`, the other ModelProviders along the fallback chain, the Agents, AgentTasks, and AgentClasses that name the provider, and the gateway Pods in `kaalm-system`. What re-runs a provider is listed under [What each reconciler watches](../reconcilers.md#what-each-reconciler-watches).

### Budget reconciliation

The reconciler reduces the per-replica spend partials in `kaalm-budget-{name}`. It writes `_canonical`, `status.budgetUsage` per namespace, `status.clusterSpentUSD`, and the `kaalm_provider_budget_canonical_usd` gauge, and reduces the per-agent counters in `kaalm-agentspend-{name}` into that ConfigMap only ([Per-workload spend](../../gateways/llm/budgets-and-rate-limits.md#per-workload-spend)). [The reducer](../../gateways/llm/budgets-and-rate-limits.md#the-reducer) specifies how each key is handled, and [Budget state management](../../gateways/llm/budgets-and-rate-limits.md#budget-state-management) gives the format.

A replica's `_marginExceeded` flag sets the `BoundaryMarginRaised` condition. The condition clears with `reason=MarginSufficient` when no replica reports the flag. The matching `Warning` event fires once, when the condition first turns on.

On period rollover, the previous period's totals are archived to status in the first pass after the boundary ([Timing](#timing) gives the lag). Per-request enforcement never waits on it.

## What it checks

The checks run in the order of the following table. A failing credential or configuration check ends the pass before the gateway mirror, the budget reduction, and the probe, so `GatewayReachable` and `budgetUsage` keep their last values and `Healthy` goes `Unknown` with `NotProbed`. A stale `Healthy=True` beside `Ready=False` would read as a working provider.

| Check | Reason when it fails | Rule |
|---|---|---|
| The `spec.credentialsRef` Secret exists in the operator namespace, carries the opt-in label, lists the `spec.endpoint` host, and holds the key | `CredentialsMissing`, `SecretNotOptedIn`, `EndpointHostNotApproved` | [49](../../resources/validation/providers.md#provider-credentials), [50](../../resources/validation/providers.md#provider-credentials) |
| The fallback chain ends, every fallback exists, and each edge joins types the gateway translates | `FallbackIneligible` | [11](../../resources/validation/providers.md#providers-fallback-and-budgets), [12](../../resources/validation/providers.md#providers-fallback-and-budgets) |
| Every `modelMap` key names one of the provider's own models and every value one of the fallback's | `InvalidModelMap` | [41](../../resources/validation/providers.md#providers-fallback-and-budgets) |
| Every `budget.policies[].degradeTo` names a model in `spec.models` | `InvalidDegradeTarget` | [18](../../resources/validation/providers.md#providers-fallback-and-budgets) |
| Under hard enforcement, every model is priced | `HardBudgetUnpriced` | [33](../../resources/validation/providers.md#providers-fallback-and-budgets) |
| The upstream accepts the credential (skipped when `healthCheck.enabled` is `false`) | `CredentialsInvalid` | |

The four configuration rows run together, and the `Ready` message lists every problem found. When more than one fails, the reason is `InvalidDegradeTarget` if any degrade target is bad, else `HardBudgetUnpriced`, else `InvalidModelMap`, else `FallbackIneligible`. [Provider credentials](../../resources/validation/providers.md#provider-credentials) states what the early end on a credential failure protects.

Three advisory checks never touch `Ready`: the [cost check](#cost-sanity-on-degradeto), the `MaxOutputTokensUnset` check in [Fallback chain validation](#fallback-chain-validation), and the [eligibility scan](#reconcile-time-fallback-eligibility-scan). The cost check and the `MaxOutputTokensUnset` check run on every pass that passes the credential check, including a pass that fails a configuration check. The eligibility scan runs only on a pass that passes the credential and configuration checks, so a failing pass leaves its `FallbackIneligible` condition as it was.

### Fallback chain validation

The reconciler walks the full chain from each provider's `spec.fallback` and checks every edge against rules 11, 12, and 41. A cross-format hop into an Anthropic model with no `maxOutputTokens` also sets the advisory `MaxOutputTokensUnset` condition to `True`, listing the models, with a `Warning` event when it turns `True`. The condition goes `False` with `reason=MaxOutputTokensDeclared` once no such model remains.

### Liveness probe

The probe sends `GET {spec.endpoint}/v1/models`, the provider's model-list call, which requires authentication but consumes no tokens. It carries the credential in the headers for the provider type:

| `spec.type` | Credential headers |
|---|---|
| `anthropic` | `x-api-key: <key>` and `anthropic-version: 2023-06-01` |
| `openai`, `openai-compatible` | `Authorization: Bearer <key>` |

For `google-vertex` the probe mints an OAuth2 access token from the credential and requests the publisher-model list; see [The google-vertex probe](#the-google-vertex-probe). An `openai-compatible` server that expects a different header rejects the probe, which shows as `CredentialsInvalid` on a `401` or `403`.

The probe never follows a redirect, so a redirecting endpoint cannot receive the credential headers at another host. A redirect counts as a transient error, the last row below. Each probe is bounded by `healthCheck.timeoutSeconds` (default 10s). The probe is a second credential egress; [Health probes are a second credential egress](../../security/credentials.md#health-probes-are-a-second-credential-egress) states its bound.

| Outcome | Conditions | Requeue |
|---|---|---|
| `2xx` | `Healthy=True, reason=UpstreamReachable` | the healthy interval |
| `401` or `403` (for `google-vertex`, also the failures under [The google-vertex probe](#the-google-vertex-probe)) | `Healthy=False` and `Ready=False`, both `reason=CredentialsInvalid`, and a `Warning` event when `Ready` first takes that reason; the pass ends | the failing backoff |
| other error, network failure, or `5xx` | `Healthy=False, reason=ProviderUnhealthy` and a `Warning` event on every failing pass; `Ready` stays `True` | the failing backoff |

The `401` and `403` class matches the credential problems in [Fallback triggers](../../gateways/llm/fallback.md#fallback-triggers).

### The google-vertex probe

For `spec.type: google-vertex` the credential Secret holds a GCP service-account JSON key: `type: service_account`, with `project_id`, `client_email`, and a PEM RSA `private_key`, and an optional `token_uri`, which must be an `https` URL and defaults to `https://oauth2.googleapis.com/token`.

The probe mints an OAuth2 access token (scope `https://www.googleapis.com/auth/cloud-platform`) with a JWT bearer grant signed by the key, then sends `GET {endpoint}/v1/projects/{project}/locations/{location}/publishers/google/models` to `spec.endpoint` with the token as a bearer credential. `project` is the key's `project_id`. `location` is the `{location}` of a regional `{location}-aiplatform.googleapis.com` endpoint host, and `global` for any other host, including the global `aiplatform.googleapis.com` endpoint. The private key signs the assertion and is never sent. The probe sends the assertion to the key's `token_uri` and the access token to `spec.endpoint`, so those two hosts bound where this credential goes.

`CredentialsInvalid` covers a `401` or `403` from Vertex, a token endpoint that refuses the key (HTTP `401`, `403`, or a `400` carrying OAuth2 error `invalid_grant` or `invalid_client`), and a Secret value that is not a usable service-account key. Any other failure (a network error, a `5xx`, or another token-endpoint error) sets `ProviderUnhealthy`.

Only this probe mints Vertex tokens; the gateway does not serve the type ([The google-vertex type is reserved](../../gateways/llm/request-handling.md#the-google-vertex-type-is-reserved)).

### Probe TLS trust

Probes trust the system roots plus the gateway's upstream trust (`gateway.trustClusterCAForUpstream` and `gateway.upstreamCA`), so they trust what forwarding trusts, plus anything the deprecated `controller.trustClusterCAForProbes` and `controller.probeCA` values add. [Deployment](../../operations/deployment.md#configuration-reference) lists the values.

The pool is additive and follows rotation without a restart. It serves the ModelProvider and ToolProvider probes, including the Vertex token request. Probes honor `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` from the controller's environment.

### Reconcile-time fallback eligibility scan

The reconciler scans the fallback tree for candidates a request could never reach, the reconcile-time counterpart of the gateway's [Per-candidate checks](../../gateways/llm/fallback.md#per-candidate-checks). A provider without `spec.fallback` has no findings.

**Callers.** The namespaces of the Agents and AgentTasks whose `spec.providers[].providerRef` names the primary, kept only if the primary's own `allowedNamespaces` admits them. With no callers, only the model check runs.

**The walk.** For each caller namespace and each of the primary's `spec.models[].id`, the reconciler walks the tree the way the gateway does for one request. It checks every candidate it reaches against the gateway's two per-candidate checks: the candidate's `allowedNamespaces` admits the namespace, and its `spec.models` offers the model the walk carries to it (the primary's model, rewritten by each edge's `modelMap` entry). An ineligible candidate's own fallbacks are not walked, because a request never reaches them.

**Findings and the condition.** A finding names a fallback that does not admit a caller namespace or does not offer a model. The `FallbackIneligible` condition lists them in its message: `True, reason=FallbackIneligible` when there are findings, `False, reason=AllCandidatesEligible` once they clear. A provider that never had a finding carries no such condition. A `Warning` event of the same reason fires on the primary when the scan adds a finding, and names only the added findings; an unchanged or smaller set, and clearing, send none ([Event emission](../operations.md#event-emission) gives the reason). `Ready=False, reason=FallbackIneligible` is the structural failure of rule 11, rule 12, or a missing fallback provider, not a scan finding.

The gateway's request-time check still runs on every request: it catches a change between passes and a request body the target format cannot express.

### Cost sanity on degradeTo

For each policy whose `action: degrade`, the reconciler computes `avgCost(model) = (costPer1MInputTokens + costPer1MOutputTokens) / 2` for the target and for the other models in `spec.models`. A model is priced when both prices parse as decimals, by the gateway ledger's parse, and only priced models count as cheapest.

A target is a finding when it is priced and some priced model has a strictly lower average. A target tied for the lowest average is not. A target without both prices, or missing from the catalog, is skipped because its cost is unknown. [Rule 33](../../resources/validation/providers.md#providers-fallback-and-budgets) requires full pricing under hard enforcement, and a missing model still fails with `InvalidDegradeTarget` (rule 18).

With any finding, the reconciler sets the `DegradeTargetNotCheapest` condition to `True` with `reason=CheaperModelAvailable`. Its message names each distinct target once, even when two policies name it, with its average cost and the cheapest priced model's. A `Warning` event with `reason=DegradeTargetNotCheapest` fires once, when the condition turns `True`. While it stays `True`, a changed list of findings updates the message and sends no event, as for `MaxOutputTokensUnset`, so a steady misconfiguration emits one event, not one per pass. When no target costs more than a priced model, the condition goes `False` with `reason=DegradeTargetCheapest` and sends nothing.

The check is advisory and never sets `Ready=False`; the reason is under [`degradeTo` validation](../../resources/modelprovider.md#degradeto-validation).

## What it reports

A ModelProvider is cluster-scoped and has no phase. [ModelProvider status](../../resources/modelprovider.md#status) lists every condition.

- **`Ready`** is `True` with `reason: CredentialsValid` when every check passes, and `False` with the reason from [What it checks](#what-it-checks) otherwise. A provider whose probe fails with `ProviderUnhealthy` stays `Ready=True`.
- **`Healthy`** is `True` or `False` only from a probe in the same pass ([Liveness probe](#liveness-probe)). A pass that ends without one sets `Unknown` with `NotProbed`: a failing credential or configuration check, a disabled probe, or a held delete. [ModelProvider status](../../resources/modelprovider.md#status) gives the meaning.
- **`GatewayReachable`** is `True` with `GatewayReady` when at least one gateway Pod in `kaalm-system` is Ready, else `False` with `GatewayUnavailable`. The value is cluster-wide, the same on every provider that passes the credential and configuration checks.
- **`BoundaryMarginRaised`** and `status.budgetUsage` come from [Budget reconciliation](#budget-reconciliation). The advisory conditions `MaxOutputTokensUnset`, `FallbackIneligible`, and `DegradeTargetNotCheapest` come from the checks above.
- **Events.** Every `Ready=False` reason raises a `Warning` event with the same reason, once, when it first appears on `Ready`. [Event emission](../operations.md#event-emission) lists the rest.

## Timing

- **A failing credential or configuration check.** The pass ends with no requeue, so an event re-runs it. A change to the credential Secret, including its label or annotation, re-evaluates every provider that names it at once. A spec edit re-runs the checks at once. A change to any ModelProvider re-enqueues every other provider that declares a fallback, so a chain recovers when a missing provider appears or a bad one is fixed.
- **After the configuration checks.** A pass requeues at the probe's next delay when the probe ran ([Probe backoff](#probe-backoff)). With the probe disabled, it requeues every minute for a provider with a budget period, so the reduction keeps running. With neither, only events re-run it.
- **Gateway readiness.** A gateway Pod's creation, deletion, or change of Ready state re-enqueues every provider at once.
- **Spend.** A replica's write to the budget ConfigMap re-enqueues its provider between timed passes. A period rollover is archived in the first pass after the boundary: the next requeue at the latest, or sooner when such a write arrives.
- **Referrers.** A change to an Agent, AgentTask, or AgentClass that names the provider re-enqueues it at once, so the delete hold releases and the eligibility scan re-runs.

### Probe backoff

A probe whose `Healthy` condition is not `False` requeues at `healthCheck.intervalSeconds` (default 60). A failing probe (`Healthy=False`, reason `ProviderUnhealthy` or `CredentialsInvalid`) backs off: each periodic failure doubles the wait (interval, 2x, 4x, 8x, and so on), capped at ten intervals or ten minutes, whichever is smaller, and never below the interval. A controller restart keeps the backoff, because it is read from the `Healthy` condition's `lastTransitionTime`. One successful probe returns the provider to the plain interval. A pass that runs no probe sets `Healthy` to `Unknown`, not `False`, so the first failure after it starts the backoff at the interval: time spent not probing says nothing about how long the upstream has been failing, and an operator who fixes a Secret sees a prompt re-probe.

The backoff delays only the periodic requeue. The event-driven re-runs above are not delayed, so a fixed credential or endpoint takes effect at once. The same backoff governs the ToolProvider probe ([ToolProviderReconciler](toolprovider.md)).

## Design choices

- **The reconciler never distributes credentials to agent Pods.** The gateway reads them from `kaalm-system` itself ([Lifecycle of an LLM API key](../../security/credentials.md#lifecycle-of-an-llm-api-key)).
- **Chain validation covers the whole chain whatever `maxFallbackDepth` is.** The cap is a gateway-level setting that can change without re-reconciling providers.
