# Fallback logic

A single upstream provider is a single point of failure. When the primary provider returns a **fallbackable** response, the gateway walks `ModelProvider.spec.fallback`, trying other providers until one succeeds or the attempt budget runs out. Each entry may carry its own `spec.fallback` list, so the chain is a tree, and the gateway walks it depth-first in declared order.

A budget-blocked primary never falls back. The gateway returns `429 budget_exhausted` to the agent at once (see [Request flow](request-handling.md#request-flow), step 5), so a namespace at its cap cannot drain budget from a fallback provider.

## Fallback triggers

Not every upstream error is a fallback signal. A malformed prompt sent to provider A fails identically on provider B, so forwarding it only spends latency and budget. The gateway classifies upstream outcomes as follows:

| Upstream response | Action | Rationale |
|---|---|---|
| Connection error, DNS failure, TLS handshake failure | Fall back | Upstream is unreachable; a different provider may be reachable. |
| Timeout before any response bytes | Fall back | Treated like a connection error: the request never landed. |
| `5xx` | Fall back | Upstream-side failure; retrying the same upstream would hit the same failure. |
| `429` (upstream-side rate limit) | Fall back | The primary's capacity is exhausted, and a different provider will often succeed. |
| `401` or `403` from upstream | Fall back **and** emit a `Warning` event with `reason=CredentialsInvalid` on the ModelProvider whose credential was refused | Falling back preserves availability while signaling that rotation is needed. |
| `400` or `422` (malformed or unprocessable request) | Return to the caller unchanged; do not fall back | The request itself is malformed; every other provider fails the same way. |
| Other `4xx` | Return to the caller unchanged; do not fall back | Client-side error: the caller should fix the request. |

Notes on individual rows:

- **Timeout before any response bytes.** The per-attempt bound is `gateway.providerFirstByteTimeout` (default `120s`; see [Deployment](../../operations/deployment.md#helm-chart-contents)). As shipped it bounds the whole upstream call, from connect through the last response byte, so a timeout after the first byte is a mid-stream failure and does not fall back.
- **Upstream `429`.** This is distinct from the gateway's own `429 rate_limited`, which is returned to the caller without fallback.
- **Upstream `401` or `403`.** If a later health probe still sees 401 or 403, the reconciler sets `Ready=False, reason=CredentialsInvalid` on that provider. The event tells the platform team that rotation or re-issuance is needed while the fallback keeps traffic serving.

## Per-candidate checks

Every candidate, the primary included, passes these checks before the gateway forwards to it. The first five are static: derived from configuration or from the request body, and free, because failing one consumes no attempt slot. The last two are the attempt itself.

| Check | Derived from | Slot consumed on failure | Children walked | Signal |
|---|---|---|---|---|
| The fallback entry names a ModelProvider that exists | configuration | no | no | `Warning` event, `reason=FallbackIneligible`, on the primary |
| The candidate's format is compatible: the primary's `spec.type`, or a crossing the gateway translates (rule 12; see [Crossing formats](#crossing-formats)) | configuration | no | no | `FallbackIneligible` on the primary |
| The namespace is in the candidate's `allowedNamespaces` | configuration | no | no | `FallbackIneligible` on the primary |
| The mapped model is in the candidate's `models`: the edge's `modelMap` entry for the requested model, or the requested model itself when the map has no entry | configuration | no | no | `FallbackIneligible` on the primary |
| The request is expressible in the candidate's format (crossing edges only; see [What does not](#what-does-not)) | request body | no | no | `FallbackIneligible` on the primary, naming the feature |
| Budget admission for the agent's namespace: not blocked, throttled, or failed closed | request-time state | yes | yes | The outcome's class feeds the exhaustion mapping under [Depth cap semantics](#depth-cap-semantics) |
| Forward with the candidate's credentials | upstream | yes | yes, when the response is fallbackable | `CredentialsInvalid` on the candidate for `401` or `403` |

A static failure is a misconfiguration, and all but the translatability check are discoverable at reconcile time. The gateway skips the candidate, does not walk its children (validation guarantees compatible chains, so children stay reachable through an eligible ancestor), and emits the event on the primary ModelProvider, the resource the platform team manages and watches, naming the offender and the failure: for example `fallback 'openai-backup' skipped: namespace 'team-ml' not in allowedNamespaces`. Spending attempt slots on misconfigured fallbacks would hide the problem and make it indistinguishable from upstream outages in metrics. The event is not returned to the caller, whose request continues down the tree. The [ModelProviderReconciler](../../controller/reconcilers.md#modelproviderreconciler) emits the same event at reconcile time, step 5, for the static failures it can detect ahead of traffic.

A budget outcome costs a slot because budget state is runtime state, and slot-bounded latency still matters. This applies only while walking the chain after a non-budget primary failure; a budget-blocked primary never reaches the walk. Under [hard enforcement](budgets-and-rate-limits.md#interaction-with-fallback) a candidate is admitted under the same rules as a primary, and after a fallbackable failure its boundary slot is freed at zero cost before the walk descends, so no slot is ever held across another provider's upstream call. The primary itself is admitted before the walk starts and is never re-admitted.

## Traversal algorithm

![Flowchart of one tryWithFallbacks invocation in three rows. The pre-checks row exits without a slot on an already visited candidate, a statically ineligible one with a FallbackIneligible event on the primary, or the depth cap. The attempt row increments attemptCount, then either records a budget outcome or forwards the request, returning a 2xx response, returning a non-fallbackable 4xx to the caller as is, or recording a fallbackable failure and freeing the boundary slot. The children row recurses into each spec.fallback entry in declared order with attemptCount threaded back out, returning the first result or no result.](../../diagrams/fallback-traversal.svg)

| Exit | Slot consumed | Children walked | What the caller gets |
|---|---|---|---|
| Already visited | no | no | Nothing from this candidate; the walk continues with the next sibling. Cycles are rejected at reconcile time (rule 11), so this is defense in depth. |
| Statically ineligible | no | no | Nothing; `FallbackIneligible` on the primary. |
| Depth cap reached | no | no | Nothing; the walk is over for every remaining candidate. |
| Budget outcome | yes | yes | Nothing from this candidate; its class is recorded. |
| `2xx` | yes | no | The response. |
| Non-fallbackable `4xx` | yes | no | The response, verbatim; the walk ends. |
| Fallbackable failure | yes | yes | Nothing from this candidate; its class is recorded. |
| Children exhausted | none | none | Nothing; the parent continues with the next sibling, or the root returns the exhaustion error. |

`attemptCount` is returned from every recursive call, so increments inside one subtree are visible to the next sibling. The cap therefore bounds the total providers attempted across the whole tree, not one root-to-leaf path. Without the thread-back, each sibling would restart from the caller's local count and the cap could be exceeded along the breadth of the tree.

## Depth cap semantics

`maxFallbackDepth` (default `3`, the Helm value `gateway.maxFallbackDepth`, passed to the gateway as `--max-fallback-depth`) bounds the **total number of providers attempted per request, including the primary**, not the nesting depth of the tree. With the default, the gateway tries at most the primary plus two others before giving up, however the tree is nested. The [ModelProvider](../../resources/modelprovider.md#fallback-trees) page draws a tree with the cut.

This is the latency guarantee: each attempt is bounded by `gateway.providerFirstByteTimeout` (default `120s`), so no request waits more than `maxFallbackDepth × providerFirstByteTimeout` before a terminal error. As shipped the bound covers the whole upstream call, so it also ends a stream that runs longer than it; see [Streaming responses](request-handling.md#streaming-responses).

When the chain is exhausted or the cap is reached without a successful response, the gateway returns an error whose type reflects the failure classes recorded across the walk:

| Recorded across the walk | Response | `retryable` |
|---|---|---|
| Every candidate was budget-blocked or throttled | `429 budget_exhausted`, `Retry-After` the largest observed | `false` after a block; `true` when every candidate was throttled |
| Only budget outcomes, at least one of them failed closed | `503 budget_state_unavailable`, `Retry-After: 1` | `true` |
| Every attempt failed at the connect layer (connection error, DNS failure, TLS handshake failure) | `503 provider_unavailable` | `false` |
| Every attempt timed out before the first byte | `504 provider_timeout` | `false` |
| Any other mix, including any upstream error response (`5xx`, upstream `429`, `401`, `403`) | `502 provider_error` | `false` |

The three `provider_*` errors are not retryable because the gateway has already retried through the whole chain, and they carry the originally requested provider in `error.provider`; see [LLM Gateway error responses](../api/errors.md#llm-gateway-error-responses). A walk exhausted by budget outcomes is a budget error and never a `502`, because the caller needs the retry time and the operator's provider-error alerts should not fire for a policy outcome; see [Interaction with fallback](budgets-and-rate-limits.md#interaction-with-fallback).

## Crossing formats

A fallback edge may cross API formats: an `anthropic` provider may name an `openai` or `openai-compatible` fallback and the reverse. `google-vertex` stays same-type in both directions, because its model is part of the URL and its wire format is a third format, and an inbound `/v1/completions` request, the legacy completions format, never crosses. Rule 12 states this ([Cross-resource validation](../../resources/validation-and-defaulting.md#cross-resource-validation)).

The primary is always spoken to in the caller's format, so nothing changes until the walk reaches a candidate of the other format. There, and only there, the gateway rewrites the request into the candidate's format before forwarding and rewrites the response back, streaming or not, so the caller never sees a format it did not ask for. Translation adds no round trip and happens before the first byte, which keeps the [point of no return](request-handling.md#streaming-responses) where it is: a stream that has started does not fall back, translated or not.

### The model on the other side

A caller asks for `anthropic-shared/claude-sonnet-4-6`, and an `openai` fallback has no such model, so the mapped-model check would skip every cross-format candidate. The edge carries the mapping: each `spec.fallback[]` entry is a `FallbackReference` with a `name` and an optional `modelMap` from this provider's model ids to the fallback's ([ModelProvider](../../resources/modelprovider.md#fallback-trees)). The check tests the mapped model, or the requested model itself when the map has no entry (the same-type case, or a compatible provider that offers the same id). Rule 41 validates the map at reconcile time: every key is one of this provider's models and every value one of the fallback's. The mapped model is what the walk carries into the candidate: the budget gate, the request body, and spend accounting all see it.

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

A request that carries a feature the other format cannot express is not forwarded lossily. The candidate is ineligible **for this request**: no attempt slot is consumed, a `FallbackIneligible` event on the primary names the feature (`fallback 'openai-backup' skipped: request uses extended thinking, which openai cannot express`), and the walk continues with the next sibling. The features:

- Anthropic to OpenAI: extended `thinking`, server tools (`web_search` and the other server-side tools), `mcp_servers`, `document` and audio content blocks, `output_format` structured outputs.
- OpenAI to Anthropic: `n` greater than 1, `logprobs`, `response_format`, the legacy `functions` and `function_call` fields, audio content parts, and a request without `max_tokens` when the mapped model declares no `maxOutputTokens`.

The reconciler warns about the last one ahead of traffic: a crossing edge into an `anthropic` provider whose mapped models declare no `maxOutputTokens` gets a `Warning` event (`reason=MaxOutputTokensUnset`) on the primary at reconcile time. The edge stays valid, because same-format traffic and requests that carry their own `max_tokens` cross it.

This is the only eligibility check the reconciler cannot run ahead of time, because it depends on the request body. The event is the operator's signal that a chain was configured to cross and the traffic cannot.

### Streaming across the crossing

When the candidate answers with SSE, the relay translates event by event, still without buffering.

| OpenAI chunk | Anthropic event |
|---|---|
| The first chunk | `message_start` (with `input_tokens` zero, because OpenAI reports usage only at the end) and `content_block_start` |
| A text delta | `content_block_delta` with `text_delta` |
| A tool-call argument delta | `content_block_delta` with `input_json_delta` |
| The `finish_reason` chunk | `content_block_stop`, then `message_delta` with the mapped `stop_reason` and the final `output_tokens`, then `message_stop` |

| Anthropic event | OpenAI chunk |
|---|---|
| `text_delta` | a `delta.content` chunk |
| `input_json_delta` | a `delta.tool_calls` chunk |
| `message_delta` | the `finish_reason` chunk, then the usage chunk |
| `message_stop` | `[DONE]` |

### Usage, spend, and errors

Usage is read with the **serving candidate's** adapter from the untranslated upstream response, never from the translated one, and spend lands on the provider that served, at that provider's prices for the mapped model. Reading usage with the caller's adapter is correct only while formats match; crossing is what makes the distinction matter.

A non-fallbackable `4xx` from a cross-format candidate (`400`, `422`, other `4xx`) is relayed in the **caller's** error envelope, because the caller cannot parse the other format's; the status and message carry over. Fallbackable failures are classified exactly as before.

The response's `model` field is the raw id of the model that served, as it is for a same-type fallback: the caller learns which provider answered only from the model id, and crossing does not change that.
