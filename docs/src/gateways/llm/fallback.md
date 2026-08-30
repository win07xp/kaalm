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

1. Verify the candidate's format is **compatible** with the request: the same `spec.type` as the primary, or (since v0.7.0) a crossing the gateway can translate, `anthropic` against `openai` or `openai-compatible` in either direction. `google-vertex` and inbound `/v1/completions` requests never cross. See [Crossing formats](#crossing-formats).
2. Verify the namespace is in the candidate provider's `allowedNamespaces`.
3. Verify the **mapped** model exists in the candidate provider's `models`: the edge's `modelMap` entry for the requested model, or the requested model itself when the map has no entry.

One more check is derived from the request body rather than configuration, and it applies only when check 1 passed by crossing (since v0.7.0): the request must be expressible in the candidate's format. A request carrying a feature the translation cannot express makes the candidate ineligible for this request, with the same effect as a static failure (no slot consumed, a `FallbackIneligible` event naming the feature) and one difference: the reconciler cannot pre-detect it. See [What does not](#what-does-not).

A static-eligibility failure is a misconfiguration, usually discoverable at reconcile time. The gateway **skips the candidate without consuming an `attemptCount` slot** and emits a `Warning` event with `reason=FallbackIneligible` on the primary `ModelProvider`, naming the offender and the specific failure (e.g., `"fallback 'openai-backup' skipped: namespace 'team-ml' not in allowedNamespaces"`). Silently burning attempt slots on misconfigured fallbacks hides the problem and makes the misconfiguration indistinguishable from upstream outages in metrics; surfacing it as a status event makes it fixable.

**Runtime gating**, derived from request-time state:

4. Check the candidate provider's budget state for the agent's namespace. If the candidate is budget-blocked, skip it and **do** consume an `attemptCount` slot. This applies only while walking the chain after a non-budget primary failure: a budget-blocked *primary* never reaches this step (see above). Budget state is legitimately runtime, and slot-bounded latency still matters.
5. Forward the request with the candidate provider's credentials.

## Traversal Algorithm

`ModelProvider.spec.fallback` is a list, and each entry may carry its own `spec.fallback` list, so the chain is a tree rather than a flat sequence. The gateway walks it **depth-first in declared order**:

![Activity diagram of a single tryWithFallbacks invocation. On entry, primary is threaded unchanged through every recursive call. A provider already in visited returns cycle_detected, a runtime dedup that is defense in depth because cycles are rejected at reconcile time. The provider is added to visited, then static eligibility (checks 1 to 3: a compatible format, namespace in allowedNamespaces, the mapped model in models, and translatability when the edge crosses) is tested; failing it emits a FallbackIneligible Warning event on the primary ModelProvider rather than on the provider that failed, and returns statically_ineligible with no attempt slot consumed, without walking that provider's children. Next, attemptCount at or above maxFallbackDepth returns fallback_depth_exhausted, and otherwise attemptCount is incremented. Then the runtime budget gate (check 4): a budget-blocked candidate does consume the slot and falls through to its children without forwarding, while an unblocked one forwards the request, returning the response on success, and returning it verbatim without falling back when it is not fallbackable (400, 422, other 4xx). Finally the walk loops over provider.spec.fallback in declared order, recursing with attemptCount threaded back out so increments inside one subtree are visible to the next sibling, and returns all_fallbacks_exhausted when nothing is left.](../../diagrams/fallback-traversal.svg)

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

    if not staticallyEligible(provider, request):   # format compatibility, allowedNamespaces, mapped model, translatability (checks 1-3)
        # Static misconfiguration. Do NOT consume an attempt slot.
        # Emit Warning event reason=FallbackIneligible on the PRIMARY
        # (not this provider) so the platform team sees the misconfig on
        # the ModelProvider they own.
        emitFallbackIneligible(primary, provider, reason)
        # Do not walk children of a format-incompatible provider either:
        # validation guarantees compatible chains, so children should be
        # reachable via an eligible ancestor.
        return error("statically_ineligible"), attemptCount

    if attemptCount >= maxFallbackDepth:       # cap on total providers tried
        return error("fallback_depth_exhausted"), attemptCount
    attemptCount += 1

    if budgetBlocked(provider, request.namespace):  # runtime gate (check 4)
        # Consumed a slot; fall through to children.
        pass
    else:
        response = forward(provider, translate(request, provider))  # identity unless the edge crosses formats
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

## Crossing formats

*(Since v0.7.0.)* A fallback edge may cross API formats: an `anthropic` provider may name an `openai` or `openai-compatible` fallback and the reverse. `google-vertex` stays same-type in both directions (its model rides in the URL and its wire shape is a third format; the matrix stays two-sided until there is demand), and an inbound `/v1/completions` request, the legacy completions shape, never crosses. Rule 12 states this ([Cross-Resource Validation](../../resources/validation-and-defaulting.md#cross-resource-validation)).

The primary is always spoken to in the caller's format, so nothing changes until the walk reaches a candidate of the other format. There, and only there, the gateway rewrites the request into the candidate's format before forwarding and rewrites the response back, streaming or not, so the caller never sees a shape it did not ask for. Translation adds no round trip and happens before the first byte, which keeps the [point of no return](request-handling.md#streaming-responses) where it is: a stream that has started does not fall back, translated or not.

### The model on the other side

A caller asks for `anthropic-shared/claude-sonnet-4-6`, and an `openai` fallback has no such model, so check 3 would skip every cross-format candidate. The edge carries the mapping: each `spec.fallback[]` entry is a `FallbackReference` with a `name` and an optional `modelMap` from this provider's model ids to the fallback's ([ModelProvider](../../resources/modelprovider.md#fallback-trees)). Check 3 tests the mapped model, or the requested model itself when the map has no entry (the same-type case, or a compatible provider that happens to offer the same id). Rule 41 validates the map at reconcile time: every key is one of this provider's models and every value one of the fallback's. The mapped model is what the walk carries into the candidate: the budget gate, the request body, and spend accounting all see it.

### What translates

Both directions, Anthropic messages and OpenAI chat completions:

| Anthropic | OpenAI | Notes |
|---|---|---|
| `system` (string or text blocks) | the leading `system` or `developer` message | Blocks are joined with blank lines. |
| `messages[].content` text and `image` blocks (base64 or URL) | `content` strings or `text` and `image_url` parts | A base64 image becomes a data URL and back. |
| assistant `tool_use` blocks | assistant `tool_calls[]` (`function.arguments` as a JSON string) | The `id` is preserved so the round trip matches. |
| user `tool_result` blocks | `role: tool` messages with `tool_call_id` | Result text blocks are joined; `is_error` becomes a prefixed line. |
| `tools[].input_schema` | `tools[].function.parameters` | `description` carries over. |
| `tool_choice` `auto` / `any` / `tool` | `auto` / `required` / a named function | `disable_parallel_tool_use` against `parallel_tool_calls: false`. |
| `max_tokens`, `temperature`, `top_p`, `stop_sequences`, `stream`, `metadata.user_id` | `max_tokens` (or `max_completion_tokens`), `temperature`, `top_p`, `stop`, `stream`, `user` | `max_tokens` is required on the Anthropic side. When an OpenAI request omits it, the translator supplies the mapped model's `maxOutputTokens` from the fallback's catalog ([ModelProvider](../../resources/modelprovider.md#spec)); a model that declares none makes the candidate ineligible for that request, with the event naming the field to set. A request carrying more than a declared ceiling is capped to it. `top_k`, `seed`, the penalties, and `logit_bias` have no counterpart and drop. |
| response `content` blocks and `stop_reason` | `choices[0].message` and `finish_reason` | `end_turn`/`stop`, `max_tokens`/`length`, `tool_use`/`tool_calls`, `stop_sequence`/`stop`, `refusal`/`content_filter`. |
| `usage.input_tokens`, `usage.output_tokens` | `usage.prompt_tokens`, `usage.completion_tokens`, `total_tokens` | Cache-read counts fold into input on the way to OpenAI. |

Two fields are dropped without penalty: Anthropic `cache_control` markers (an optimization hint, not a semantic) and OpenAI `stream_options` (the gateway injects its own on the way back out).

### What does not

A request that carries a feature the other format cannot express is not forwarded lossily. The candidate is ineligible **for this request**: no attempt slot is consumed, a `FallbackIneligible` event on the primary names the feature (`"fallback 'openai-backup' skipped: request uses extended thinking, which openai cannot express"`), and the walk continues with the next sibling. The features:

- Anthropic to OpenAI: extended `thinking`, server tools (`web_search` and its kin), `mcp_servers`, `document` and audio content blocks, `output_format` structured outputs.
- OpenAI to Anthropic: `n` greater than 1, `logprobs`, `response_format`, the legacy `functions` and `function_call` fields, audio content parts, and a request without `max_tokens` when the mapped model declares no `maxOutputTokens`.

The reconciler warns about the last one ahead of traffic: a crossing edge into an `anthropic` provider whose mapped models declare no `maxOutputTokens` gets a `Warning` event (`reason=MaxOutputTokensUnset`) on the primary at reconcile time. The edge stays valid, because same-format traffic and requests that carry their own `max_tokens` cross it fine.

This is the one eligibility check the reconciler cannot run ahead of time, because it depends on the request body; the event is the operator's signal that a chain was configured to cross and the traffic cannot.

### Streaming across the crossing

When the candidate answers with SSE, the relay translates event by event, still without buffering. From OpenAI chunks to Anthropic events: the first chunk opens `message_start` (with `input_tokens` zero, because OpenAI reports usage only at the end) and `content_block_start`; text deltas become `content_block_delta` `text_delta` events and tool-call argument deltas `input_json_delta` events; the `finish_reason` chunk closes the block and emits `message_delta` with the mapped `stop_reason` and the final `output_tokens`, then `message_stop`. From Anthropic events to OpenAI chunks: `text_delta` and `input_json_delta` become `delta.content` and `delta.tool_calls` chunks, `message_delta` becomes the `finish_reason` chunk followed by the usage chunk, `message_stop` becomes `[DONE]`.

### Usage, spend, and errors

Usage is read with the **serving candidate's** adapter from the untranslated upstream response, never from the translated one, and spend lands on the provider that served, at that provider's prices for the mapped model. (Before v0.7.0 the gateway read usage with the caller's adapter, which was only correct while formats matched; crossing is what makes the distinction load-bearing.)

A non-fallbackable `4xx` from a cross-format candidate (`400`, `422`, other `4xx`) is relayed in the **caller's** error envelope shape, because the caller cannot parse the other format's; the status and message carry over. Fallbackable failures are classified exactly as before.

The response's `model` field is the raw id of the model that served, as it is for a same-type fallback: the caller already learns which provider answered only from the model id, and crossing does not change that.
