# Fallback Logic

A single upstream provider is a single point of failure. When the primary provider returns a **fallbackable** response, the gateway walks `ModelProvider.spec.fallback` in order, trying other providers until one succeeds or the attempt budget runs out.

One case is deliberately excluded. A **budget-blocked primary does not trigger fallback**. The gateway returns `429 budget_exhausted` to the agent immediately (see [Request Flow](request-handling.md#request-flow) step 5). This keeps budget enforcement predictable: a namespace at its cap does not silently drain budget from a fallback provider.

## Fallback Triggers

Not every upstream error is a fallback signal. A malformed prompt sent to provider A will fail identically on provider B, so forwarding it just wastes latency and budget. The gateway classifies upstream outcomes as follows:

| Upstream response | Action | Rationale |
|---|---|---|
| Connection error / DNS failure / TLS handshake failure | Fall back | Upstream is unreachable; a different provider may be reachable. |
| Timeout before any response bytes | Fall back | Treated like a connection error: the request never landed. |
| `5xx` | Fall back | Upstream-side failure; retrying the same upstream would hit the same failure. |
| `429` (upstream-side rate limit) | Fall back | The primary's capacity is exhausted, and a different provider will often succeed. |
| `401` / `403` from upstream | Fall back **and** emit a `Warning` event with `reason=CredentialsInvalid` on the primary ModelProvider | Upstream refuses the credential. Falling back preserves availability while signalling that rotation is needed. |
| `400` / `422` (malformed or unprocessable request) | Return to caller unchanged; **do not fall back** | The request itself is malformed; fallback will fail for the same reason. Consumes one attempt slot. |
| Other `4xx` | Return to caller unchanged; do not fall back | Client-side error surface: the caller should fix the request. |

Notes on individual rows:

- **Timeout before any response bytes.** The per-attempt bound is `gateway.providerFirstByteTimeout` (default `120s`; see [Deployment](../../operations/deployment.md#helm-chart-contents)), applied from connection start through first response byte.
- **Upstream `429`.** This is distinct from the gateway's own `429 rate_limited`, which is returned to the caller without fallback. An upstream 429 indicates the primary's capacity is exhausted, which a different provider may not share.
- **Upstream `401` / `403`.** If a subsequent health probe still sees 401/403, the reconciler sets `Ready=False, reason=CredentialsInvalid`. The event tells the platform team that rotation or re-issuance is needed while the fallback keeps traffic serving.

The distinction matters most at the `4xx` boundary: `429` (transient capacity) and `401`/`403` (credential problem, often fixable by switching provider) fall back; `400`/`422` (caller-driven) do not. This avoids turning a bad-prompt bug into a cross-provider retry storm that drains budget across every fallback.

## Per-Candidate Checks

For each candidate provider (primary or a fallback entry) the gateway performs these checks before forwarding. The checks split into two kinds with different effects on the `attemptCount` budget.

**Static eligibility**, derived from configuration alone and unchanged between requests:

1. Verify the candidate provider has the **same `spec.type`** as the primary provider (e.g., both `anthropic`, or both `openai-compatible`). If the types differ, the candidate is skipped. This constraint exists because the gateway does not translate between API formats, see [Request Format Detection](request-handling.md#request-format-detection).
2. Verify the namespace is in the candidate provider's `allowedNamespaces`.
3. Verify the requested model exists in the candidate provider's `models`.

A static-eligibility failure is a misconfiguration, usually discoverable at reconcile time. The gateway **skips the candidate without consuming an `attemptCount` slot** and emits a `Warning` event with `reason=FallbackIneligible` on the primary `ModelProvider`, naming the offender and the specific failure (e.g., `"fallback 'openai-backup' skipped: namespace 'team-ml' not in allowedNamespaces"`). Silently burning attempt slots on misconfigured fallbacks hides the problem and makes the misconfiguration indistinguishable from upstream outages in metrics; surfacing it as a status event makes it fixable.

**Runtime gating**, derived from request-time state:

4. Check the candidate provider's budget state for the agent's namespace. If the candidate is budget-blocked, skip it and **do** consume an `attemptCount` slot. This applies only while walking the chain after a non-budget primary failure: a budget-blocked *primary* never reaches this step (see above). Budget state is legitimately runtime, and slot-bounded latency still matters.
5. Forward the request with the candidate provider's credentials.

## Traversal Algorithm

`ModelProvider.spec.fallback` is a list, and each entry may carry its own `spec.fallback` list, so the chain is a tree rather than a flat sequence. The gateway walks it **depth-first in declared order**:

![Activity diagram of a single tryWithFallbacks invocation. On entry, primary is threaded unchanged through every recursive call. A provider already in visited returns cycle_detected, a runtime dedup that is defense in depth because cycles are rejected at reconcile time. The provider is added to visited, then static eligibility (checks 1 to 3: same spec.type, namespace in allowedNamespaces, model in models) is tested; failing it emits a FallbackIneligible Warning event on the primary ModelProvider rather than on the provider that failed, and returns statically_ineligible with no attempt slot consumed, without walking that provider's children. Next, attemptCount at or above maxFallbackDepth returns fallback_depth_exhausted, and otherwise attemptCount is incremented. Then the runtime budget gate (check 4): a budget-blocked candidate does consume the slot and falls through to its children without forwarding, while an unblocked one forwards the request, returning the response on success, and returning it verbatim without falling back when it is not fallbackable (400, 422, other 4xx). Finally the walk loops over provider.spec.fallback in declared order, recursing with attemptCount threaded back out so increments inside one subtree are visible to the next sibling, and returns all_fallbacks_exhausted when nothing is left.](../../diagrams/fallback-traversal.svg)

Reading the diagram: follow the `attemptCount` variable rather than the control flow. It is untouched by the static-eligibility exit, incremented before the budget gate (so a budget block spends a slot the static skip does not), and threaded back out of every recursive call, which is what makes the cap bound the whole tree rather than one root-to-leaf path.

```
# Top-level entry called by the request handler:
#   tryWithFallbacks(primary=primary, provider=primary, request, attemptCount=0, visited={})
# `primary` is threaded unchanged through every recursive call so the
# FallbackIneligible event is always emitted on the primary ModelProvider
# (the resource the platform team owns and watches).
# `attemptCount` is returned from every call so increments inside one subtree
# are visible to the next sibling iteration. The depth cap counts attempts
# across the entire tree, not per path. Without this thread-back, sibling
# fallbacks would each restart from the caller's local count and the cap
# could be violated along the breadth dimension.

tryWithFallbacks(primary, provider, request, attemptCount, visited) -> (result, attemptCount):
    if provider.name in visited:               # runtime dedup, defense in depth
        return error("cycle_detected"), attemptCount
    visited.add(provider.name)

    if not staticallyEligible(provider, request):   # type, allowedNamespaces, models (checks 1-3)
        # Static misconfiguration. Do NOT consume an attempt slot.
        # Emit Warning event reason=FallbackIneligible on the PRIMARY
        # (not this provider) so the platform team sees the misconfig on
        # the ModelProvider they own.
        emitFallbackIneligible(primary, provider, reason)
        # Do not walk children of a type-mismatched provider either:
        # validation guarantees same-type chains, so children should be
        # reachable via an eligible ancestor.
        return error("statically_ineligible"), attemptCount

    if attemptCount >= maxFallbackDepth:       # cap on total providers tried
        return error("fallback_depth_exhausted"), attemptCount
    attemptCount += 1

    if budgetBlocked(provider, request.namespace):  # runtime gate (check 4)
        # Consumed a slot; fall through to children.
        pass
    else:
        response = forward(provider, request)
        if response.ok:
            return response, attemptCount
        if not isFallbackable(response):        # see Fallback triggers table above
            return response, attemptCount       # pass 400/422/other 4xx back to caller

    for next in provider.spec.fallback:        # declared order, depth-first
        result, attemptCount = tryWithFallbacks(primary, next, request, attemptCount, visited)
        if result.ok:
            return result, attemptCount

    return error("all_fallbacks_exhausted"), attemptCount
```

`isFallbackable(response)` encapsulates the table above: it returns true for connection/DNS/TLS errors, pre-stream timeouts, any `5xx`, upstream `429`, and upstream `401`/`403` (with the credential-warning side effect); false for `400`, `422`, and other `4xx`. Non-fallbackable responses are passed through to the caller verbatim and do not consume additional chain attempts, because continuing the walk would both waste latency and be wrong: no other provider will succeed with the same bad request.

`FallbackIneligible` is surfaced as a Kubernetes `Warning` event on the primary `ModelProvider`, not returned to the caller as a 5xx. The caller's request continues walking the tree. The event exists so platform teams see the misconfiguration on the `ModelProvider` resource (`kubectl describe modelprovider …`) rather than discovering it only via an elevated fallback failure rate in metrics. The `ModelProviderReconciler` also emits this event at reconcile time when it detects static eligibility violations in the declared chain, see [ModelProviderReconciler step 5](../../controller/reconcilers.md#modelproviderreconciler).

## Depth cap semantics

`maxFallbackDepth` (default `3`, set via Helm `gateway.maxFallbackDepth` → `KAALM_MAX_FALLBACK_DEPTH`) bounds the **total number of providers attempted per request, including the primary**, not the nesting depth of the tree. With the default, the gateway tries at most the primary plus two others before giving up, regardless of how the fallback tree is shaped.

This is the latency guarantee: each attempt is bounded from connect through first response byte by `gateway.providerFirstByteTimeout` (default `120s`), so no single request waits more than `maxFallbackDepth × providerFirstByteTimeout` before a terminal error. Once a stream has started, the same value applies as an idle-bytes timeout between SSE chunks. An upstream that stalls without closing is terminated with the documented mid-stream error event rather than holding gateway and agent connections open indefinitely.

If the chain is exhausted or the cap is reached without a successful response, the gateway returns a **fallback-exhausted error whose `error.type` reflects the failure classes observed across the walk**:

| Observed across the walk | Response |
|---|---|
| Every attempted provider failed at the connect layer (connection error, DNS failure, TLS handshake failure) | `503 provider_unavailable` |
| Every attempt timed out pre-stream | `504 provider_timeout` |
| Anything else | `502 provider_error` |

`502 provider_error` covers any upstream error response (5xx, upstream 429, 401/403) or a mix of failure classes, including a walk exhausted purely by budget-blocked candidates. All three share the fallback-exhausted `retryable: false` rationale and carry the originally-requested provider in `error.provider`, see [LLM Gateway Error Responses](../api/errors.md#llm-gateway-error-responses).

Circular references are rejected at reconcile time by the [ModelProviderReconciler](../../controller/reconcilers.md#modelproviderreconciler), so cycles should never reach the gateway; the runtime `visited` check is defense in depth.

## Same-type constraint

Each provider in the fallback chain must have the same `spec.type` as the primary provider (e.g., all `anthropic` or all `openai-compatible`). A ModelProvider with `type: anthropic` cannot list a fallback with `type: openai`. This is validated at reconcile time by the ModelProviderReconciler.

Cross-format fallback may be considered for a future version if there is sufficient demand, but the translation surface area (streaming, tool use, system prompts, multimodal content) is large and error-prone.
