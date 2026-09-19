# Request handling

Every LLM call an agent makes travels through the LLM Gateway. The agent never talks to Anthropic or OpenAI directly: it talks to the gateway using the provider's own native API format, and the gateway authenticates the caller, authorizes the model, injects the real provider credential, forwards upstream, and accounts for the tokens spent.

This page walks the request path end to end, then covers the three mechanisms it depends on: how a model name identifies a provider, how the gateway works out which API format it is looking at, and how streaming responses are relayed and metered.

## Request flow

![Sequence diagram of one LLM call through the gateway, one message per step: the agent's request with a qualified model, authentication and the source-IP cross-check against the informer cache, path and body checks, route authorization against the cache, budget admission, the rate limit, request preparation, the forward to the upstream provider with a fallback walk on failure, the relay back to the agent, and the usage read and spend settle.](../../diagrams/llm-request-flow.svg)

1. **The agent sends the request.** The agent container makes an HTTPS request to `$KAALM_GATEWAY_ENDPOINT`, which resolves to the gateway Service in `kaalm-system`, on the upstream provider's native path (`/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible) with a qualified model name in the request body; see [Model identification](#model-identification).

2. **Authentication and cross-check.** The gateway authenticates the request by mTLS client-cert SAN (Mode 1) or a `TokenReview`-verified bearer token (Mode 2), which establishes the caller's namespace and, for Mode 1, the workload's kind and name. It then checks that the Pod at the request's source IP, looked up in its Pod informer cache, is in that namespace. Authentication establishes the claim; the cross-check confirms the claim came from where it should have. See [Workload identity](workload-identity.md).

3. **Path, body, and model name.** The path selects the request format; any other path is rejected with `400 invalid_request` (see [Request format detection](#request-format-detection)). Bodies above `gateway.maxLLMRequestBodyBytes` (default 4 MiB) are rejected with `413 request_too_large` before the body is parsed. The body must be JSON whose `model` field is a qualified `{providerRef}/{modelId}` name, or the request is rejected with `400 invalid_request`. See [LLM Gateway error responses](../api/errors.md#llm-gateway-error-responses) for the wire contract.

4. **Route authorization.** For a workload caller, the Agent or AgentTask named by the SAN must exist and list the provider in `spec.providers`, and its AgentClass must list the provider in `allowedProviders`; each failure is `403 access_denied`. The provider must then exist (`400 invalid_request`), the caller's namespace must be in its `allowedNamespaces` (`403 access_denied`), and the model must be in its `spec.models` (`400 invalid_request`). The namespace check precedes the model check so a namespace not authorized for a provider never learns which models it hosts. Gateway-only-tier callers carry no workload and start at the provider check. The full chain is under [Provider routing and adapters](provider-routing.md#mtls-tier-kaalm-managed-pods).

5. **Budget admission.** The gateway reads the last-known spend state for the namespace, with no pre-call cost estimation, and applies the highest policy at or below the utilization. None of the denials starts the fallback walk, so a capped namespace never drains a fallback provider's budget.

   | Outcome | Effect |
   |---|---|
   | `warn` | Logged; the request proceeds |
   | `degrade` | The model is rewritten to the policy's `degradeTo` |
   | `block` | `429 budget_exhausted`, `Retry-After` set to the next period boundary |
   | throttled (hard mode, boundary region) | `429 budget_throttled`, `Retry-After: 1` |
   | failed closed (hard mode, boundary region) | `503 budget_state_unavailable`, `Retry-After: 1` |

   See [Budget state management](budgets-and-rate-limits.md#budget-state-management) and [Hard enforcement](budgets-and-rate-limits.md#hard-enforcement).

6. **Rate limit.** A token bucket per (namespace, model), sized from the provider's `requestsPerMinute` divided across the live gateway replicas, admits or rejects the request with `429 rate_limited` and `Retry-After: 1`. The rejection does not fall back. `tokensPerMinute` is accepted by the ModelProvider schema but is not enforced. See [Rate limiting](budgets-and-rate-limits.md#rate-limiting).

7. **Request preparation.** The gateway strips the provider prefix so the upstream sees the raw model id, applies the format's adapter fixups (see [Streaming responses](#streaming-responses)), and rewrites the headers under the [forwarded-header contract](#the-forwarded-header-contract). Traffic from an Agent caller is recorded as activity for [hibernation](../../controller/hibernation-and-wake.md#activity-detection); task Pods do not hibernate and are not tracked.

8. **Forward, with fallback.** The gateway attaches the provider's credential, read from its Secret in `kaalm-system` (see [Credential handling](provider-routing.md#credential-handling)), and forwards. Each attempt is bounded by `gateway.providerFirstByteTimeout` (default `120s`), which as shipped covers the whole upstream call from connect through the last response byte. On a fallbackable failure the gateway walks `spec.fallback` up to `maxFallbackDepth`; see [Fallback logic](fallback.md).

9. **Response relay.** A buffered response is returned whole; an SSE response is relayed chunk by chunk as it arrives (see [Streaming responses](#streaming-responses)). A response from a fallback candidate of another format is translated back into the caller's format; see [Crossing formats](fallback.md#crossing-formats).

10. **Usage and spend.** The gateway reads token usage from the response with the serving provider's adapter: `usage.input_tokens` and `usage.output_tokens` for Anthropic, `usage.prompt_tokens` and `usage.completion_tokens` for OpenAI. For a stream, usage comes from the usage-bearing SSE events. Actual usage from the response is always used over any estimate. Spend is settled on the provider that served, at its prices for the model, into the in-process counter, and under hard enforcement the same step releases the admission slot.

### The forwarded-header contract

At step 7 an agent's request becomes the gateway's request. Four header rules apply:

| Rule | Headers | Why |
|---|---|---|
| Strip inbound authentication material before injecting the provider credential | `Authorization`, `x-api-key`, `api-key` | The header names differ per provider and per tier: Anthropic authenticates with `x-api-key`, while a gateway-only-tier caller arrives with `Authorization: Bearer <SA-token>`. Injecting the Anthropic key would leave the bearer token in place and forward a live, audience-bound Kubernetes credential into third-party provider logs. |
| Drop hop-by-hop headers (RFC 7230 §6.1) | `Connection`, `TE`, `Upgrade`, `Proxy-Authorization`, and any header the `Connection` header names | They are scoped to a single connection and must not be relayed across a proxy hop. |
| Pin `Accept-Encoding` to `identity` | `Accept-Encoding` | Go's transport only auto-decompresses gzip it negotiated itself. Relaying a caller's `Accept-Encoding: gzip` would make every response body opaque to usage extraction and silently zero all spend accounting. |
| Reset per-hop framing | `Host`, `Content-Length` | The upstream `Host` comes from the provider's endpoint, and the length from the rewritten body. |

## Model identification

Agents identify both the provider and the model in each LLM request with a **qualified model name**, `{providerRef}/{modelId}`:

- `anthropic-shared/claude-opus-4-6`: Claude Opus through the `anthropic-shared` ModelProvider
- `anthropic-shared/claude-sonnet-4-6`: Claude Sonnet through the same provider
- `openai-fallback/gpt-4o`: GPT-4o through the `openai-fallback` ModelProvider
- `local-vllm/llama-3-70b`: Llama 3 70B through a local vLLM instance registered as a ModelProvider

The gateway splits the name on the **first** `/`: the prefix identifies the ModelProvider by `metadata.name`, and the suffix is the raw model id that must appear in the provider's `models` list. Before forwarding, the gateway strips the prefix and sends only the raw id, so the upstream Anthropic API receives `claude-opus-4-6`, not `anthropic-shared/claude-opus-4-6`.

The qualified name identifies the (provider, model) pair without ambiguity when several ModelProviders offer models with similar names, for example a managed Anthropic endpoint and an OpenAI-compatible proxy both serving Claude models. The agent constructs the qualified name in every call, and it travels in the request body's `model` field for every served format.

## Request format detection

The agent sends LLM requests in the upstream provider's native API format, and the gateway detects the format from the URL path:

- `/v1/messages`: Anthropic format
- `/v1/chat/completions`: OpenAI and OpenAI-compatible format (also used by vLLM, Ollama, LiteLLM)
- `/v1/completions`: OpenAI legacy completions format

These three paths are the whole served inbound surface; any other path on the cluster listener is rejected with `400 invalid_request`. The detected format selects the adapter that parses the request, extracts usage from the response, and applies the format's fixups.

### The google-vertex type is reserved

The ModelProvider API accepts `spec.type: google-vertex`, but the type is not served: no Vertex-format inbound path is routed on the cluster listener, cross-format fallback cannot translate into it ([rule 12](../../resources/validation-and-defaulting.md#cross-resource-validation) keeps `google-vertex` chains same-type), and the gateway does not mint the OAuth2 tokens the platform requires in place of static API keys. The adapter's outbound pieces (usage extraction from `usageMetadata`, URL-path model rewriting, and the `?alt=sse` streaming fixup) remain in the code but are reachable from no inbound path.

Google has renamed the platform from Vertex AI to the Gemini Enterprise Agent Platform with the wire API carried forward unchanged, so `google-vertex` remains the stable enum value. Serving it is a backlog item on the [roadmap](../../ROADMAP.md#beyond).

The gateway is **protocol-aware** in that it understands request and response formats for the supported provider types, which it needs for usage extraction, model name parsing, and similar work. It translates between formats in one situation only: a fallback candidate of a different `spec.type`, where the request is rewritten into the candidate's format before the first byte and the response, streaming or not, is rewritten back; see [Crossing formats](fallback.md#crossing-formats). Everywhere else the request path is passthrough: the primary is always spoken to in the caller's format, and same-type fallbacks are too.

## Streaming responses

Most LLM usage involves streaming responses (Server-Sent Events, SSE), where the provider sends token-by-token output as a stream of chunks. The gateway detects streaming from the upstream response's `Content-Type: text/event-stream` and relays each chunk to the agent as it arrives, without buffering the response.

![Sequence diagram of an SSE stream relayed through the gateway with Anthropic event names. Before the first chunk a pre-stream failure walks the fallback chain. The provider's message_start event carries input_tokens and is relayed. After the first relayed chunk there is no fallback: content chunks are relayed unbuffered, and either the stream completes with message_delta carrying output_tokens and message_stop, after which the gateway settles spend, or the upstream fails mid-stream, the agent's stream ends, and the gateway settles the usage accumulated so far.](../../diagrams/llm-streaming.svg)

**Adapter fixups.** Before forwarding a streaming request the format's adapter adjusts it so the stream will carry usage:

| Format | Fixup when absent | Why |
|---|---|---|
| OpenAI and OpenAI-compatible | `stream_options: {"include_usage": true}` is injected | Without it the stream carries no usage at all. The extra terminal usage chunk has an empty `choices` array, which OpenAI client libraries tolerate, and is relayed to the agent unchanged. |
| google-vertex (not served) | `?alt=sse` is appended to `:streamGenerateContent` | Without it Vertex returns a JSON-array stream rather than SSE and the relay never engages. |

**Usage.** The gateway inspects each chunk as it relays it and accumulates usage where the format carries it. For Anthropic, `input_tokens` arrive on `message_start` and the cumulative `output_tokens` on the final `message_delta`; `message_stop` carries no usage. For OpenAI-compatible formats, a usage object appears in the final chunk before the `[DONE]` sentinel. Spend is settled after the stream ends, the same as step 10. A stream that ends without usage settles at zero spend, with no log or metric. Under soft enforcement that is an accepted approximation; under [hard enforcement](budgets-and-rate-limits.md#hard-enforcement) it is one of the stated limits of the guarantee, because no gateway-side cap can meter spend the provider never reports.

**Budget checks.** Budget admission happens before the call (step 5), the same as for non-streaming requests. There is no mid-stream enforcement: once a stream has started, it runs to completion, because aborting it mid-response would leave the agent with a partial, unusable response while the provider still charges for the full generation.

**Mid-stream failures.** If the upstream connection drops or errors after the first chunk has been relayed, the gateway logs a warning, settles the usage accumulated so far, and ends the agent's stream. It sends no error event, because the response status is already committed, and it does not fall back or retry: the agent has already received partial output, and replaying the request on another provider would produce a different, possibly contradictory continuation. Fallback applies only to pre-stream failures: connection errors, timeouts before the first chunk, and error responses returned before streaming begins. See [Fallback triggers](fallback.md#fallback-triggers). The `gateway.providerFirstByteTimeout` bound covers the whole upstream call as shipped, so a stream that runs longer than it ends the same way.

**Provider adapter.** Usage extraction is adapter-specific: each adapter's `accumulateStreamUsage` knows where usage appears in its format's SSE events, and for a cross-format candidate the usage is read from the upstream's own events before translation. See [Provider adapters](provider-routing.md#provider-adapters).
