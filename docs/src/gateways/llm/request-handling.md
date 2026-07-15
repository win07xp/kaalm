# Request Handling

Every LLM call an agent makes travels through the LLM Gateway. The agent never talks to Anthropic, OpenAI, or Vertex directly: it talks to the gateway using the provider's own native API format, and the gateway authenticates the caller, authorizes the model, injects the real provider credential, forwards upstream, and accounts for the tokens spent.

This page walks the request path end to end, then covers the three mechanisms it depends on: how a model name identifies a provider, how the gateway works out which API format it is looking at, and how streaming responses are relayed and metered.

## Request Flow

![Sequence diagram of one LLM call through the gateway. The agent container makes an HTTPS request to $AGENTRY_GATEWAY_ENDPOINT using the provider's native path and a qualified providerRef/modelId name; bodies over maxLLMRequestBodyBytes are rejected with 413 request_too_large at the listener, before namespace identification. The gateway authenticates first, via mTLS client-cert SAN or a TokenReview-verified bearer token, and only then cross-checks the Pod at the request's source IP against its informer cache, a note marking this as defense in depth rather than the identity mechanism. It then resolves the Pod's ownerRef to the Agent and reads spec.providers, validates the model against ModelProvider.models and the namespace against allowedNamespaces, applies the budget check (degrade rewrites the model name, block returns an error), and applies the per-namespace-per-model token-bucket rate limit. Step 7 reads the provider credential from a Secret in agentry-system; a highlighted note states the forwarded-header contract, which strips Authorization, x-api-key and api-key before injecting the credential, drops hop-by-hop headers per RFC 7230 section 6.1, and pins Accept-Encoding to identity because a relayed gzip would make usage extraction unreadable and zero all spend. The request is forwarded with the provider prefix stripped, falling back on failure. A second note marks that usage extraction happens on the return leg, naming the per-provider usage fields, before the gateway updates its in-process spend counter.](../../diagrams/llm-request-flow.svg)

Reading the diagram: two orderings carry the weight. Authentication precedes the source-IP cross-check (step 2), and the credential strip precedes the credential injection (step 7). Both are reversible-looking steps whose reversal would be a security bug, which is why they are drawn as separate arrows rather than folded together.

1. **Agent sends request.** The agent container makes an HTTPS request to `$AGENTRY_GATEWAY_ENDPOINT`, which resolves to the gateway Service in `agentry-system`. The agent uses the upstream provider's native API path (for example `/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible) and includes a qualified model name in the request body (see [Model Identification](#model-identification) below). Request bodies above `gateway.maxLLMRequestBodyBytes` (Helm-configurable, default 4 MiB) are rejected with `413 request_too_large` at the listener, before namespace identification. This is the same defense-in-depth pattern as the User Gateway's `gateway.maxMessageBodyBytes` cap on `:8080`: reject oversized bodies before spending any work on them. See [LLM Gateway Error Responses](../api/errors.md#llm-gateway-error-responses) for the wire contract.

2. **Namespace identification.** The gateway authenticates the request first, via mTLS client-cert SAN (Mode 1) or a `TokenReview`-verified bearer token (Mode 2), to establish the caller's namespace. It then cross-checks that the Pod at the request's source IP, looked up in its Pod informer cache, is in that namespace. Authentication establishes the claim; the source-IP check confirms the claim came from where it should have. See [Namespace Identification](workload-identity.md) for both auth modes and the cross-check.

3. **Provider routing.** The gateway resolves the Pod's ownerRef to the Agent resource, reads `spec.providers` to determine which ModelProviders this agent is allowed to use, and parses the `provider/model` name from the request to identify the target ModelProvider. See [Provider Routing](provider-routing.md).

4. **Gateway validates.** It confirms the requested model is listed in the target ModelProvider's `models` and that the namespace is in `allowedNamespaces`.

5. **Budget check.** The gateway reads the current budget state for the agent's namespace. If a `degrade` policy applies, it rewrites the model name in the request. If `block` applies, it returns an error to the agent. See [Budget State Management](budgets-and-rate-limits.md#budget-state-management).

6. **Rate limit check.** A per-(namespace, model) token-bucket rate limiter is applied on requests/min and tokens/min. See [Rate Limiting](budgets-and-rate-limits.md#rate-limiting).

7. **Route to upstream.** The gateway attaches the provider credential, read directly from Secrets in `agentry-system` (a static API-key header for Anthropic/OpenAI-style providers, an OAuth2 access token for Google Vertex; see [Credential Handling](provider-routing.md#credential-handling)), strips the provider prefix from the model name, and forwards the request under a strict **forwarded-header contract** (detailed below). If the upstream fails (connection error, 5xx, timeout), the gateway walks the fallback chain, trying the primary provider's `spec.fallback` entries, then each fallback's own fallback, up to `maxFallbackDepth` (default 3). See [Fallback Logic](fallback.md).

8. **Response returned.** The gateway relays the response to the agent container. For streaming responses (SSE), the gateway transparently relays each chunk as it arrives. See [Streaming Responses](#streaming-responses) below.

9. **Token counting.** The gateway extracts actual token usage from the provider response: `usage.input_tokens` / `usage.output_tokens` for Anthropic, `usage.prompt_tokens` / `usage.completion_tokens` for OpenAI, `usageMetadata.promptTokenCount` / `usageMetadata.candidatesTokenCount` for Google Vertex. For streaming responses, token usage is extracted from the usage-bearing SSE events instead (see [Streaming Responses](#streaming-responses)). Actual usage from the provider response is always preferred over pre-call estimation.

10. **Spend update.** The gateway updates the in-process spend counter for the namespace.

### The forwarded-header contract

Step 7 is the point where an agent's request becomes the gateway's request. Three header rules apply, and each exists for a concrete reason.

**All inbound authentication material is stripped before the provider credential is injected**: `Authorization`, `x-api-key`, and `api-key`. Injection alone does not displace them, because the header names differ per provider and per tier: Anthropic authenticates via `x-api-key`, while a gateway-only-tier caller arrives with `Authorization: Bearer <SA-token>`. Injecting the Anthropic key would leave the bearer token untouched. Without the explicit strip, a live audience-bound Kubernetes credential would be forwarded verbatim into third-party provider logs.

**Hop-by-hop headers are removed** per RFC 7230 §6.1: `Connection`, `TE`, `Upgrade`, and `Proxy-Authorization`. These are scoped to a single connection and must not be relayed across a proxy hop.

**`Accept-Encoding` is pinned to `identity`** so upstream response bodies arrive uncompressed. Go's transport only auto-decompresses gzip it negotiated itself, so relaying a caller's `Accept-Encoding: gzip` would make every response body opaque to usage extraction and silently zero all spend accounting. Pinning the header is what keeps step 9 able to read the response at all.

## Model Identification

Agents identify both the provider and the model in each LLM request using a **qualified model name** format: `{providerRef}/{modelId}`.

Examples:
- `anthropic-shared/claude-opus-4-6`: Claude Opus via the `anthropic-shared` ModelProvider
- `anthropic-shared/claude-sonnet-4-6`: Claude Sonnet via the same provider
- `openai-fallback/gpt-4o`: GPT-4o via the `openai-fallback` ModelProvider
- `local-vllm/llama-3-70b`: Llama 3 70B via a local vLLM instance registered as a ModelProvider

The gateway splits the model name on the **first** `/`: the prefix identifies the ModelProvider by `metadata.name`, and the suffix is the raw model ID that must appear in the ModelProvider's `models` list. Before forwarding upstream, the gateway strips the provider prefix and sends only the raw model ID, so the upstream Anthropic API receives `claude-opus-4-6`, not `anthropic-shared/claude-opus-4-6`.

This format uniquely identifies the (provider, model) pair and eliminates ambiguity when multiple ModelProviders offer models with similar names, for example a managed Anthropic endpoint and an OpenAI-compatible proxy both serving Claude models. The agent is always responsible for constructing the qualified `provider/model` name in its API calls.

Where the qualified name appears follows the upstream format: in the request body's `model` field for Anthropic, OpenAI, and OpenAI-compatible formats, and in the URL path's `{model}` segment (URL-encoded) for Google Vertex. See [Request Format Detection](#request-format-detection) below.

## Request Format Detection

The agent sends LLM requests using the upstream provider's native API format. The gateway detects the request format from the **URL path** the agent uses:

- `/v1/messages` -> Anthropic format
- `/v1/chat/completions` -> OpenAI / OpenAI-compatible format (also used by vLLM, Ollama, LiteLLM)
- `/v1/completions` -> OpenAI legacy completions format
- `…/models/{model}:generateContent` and `…/models/{model}:streamGenerateContent` -> Google Vertex (Gemini) format

The Vertex adapter matches on the `:generateContent` / `:streamGenerateContent` method suffix rather than a fixed prefix, because Vertex paths embed project and location segments. Vertex is also the one format that names the model in the **URL path** rather than the request body: the `{model}` segment carries the qualified `{providerRef}/{modelId}` name (URL-encoded), and the gateway rewrites it to the raw model ID before forwarding, which is the same strip-the-prefix step applied to body-carried model names. On `:streamGenerateContent` the adapter also guarantees `?alt=sse` is present (see [Streaming Responses](#streaming-responses)).

Each provider adapter registers the path patterns it recognizes; requests to unrecognized paths on the LLM listener are rejected with `400 invalid_request`. The gateway uses the detected format to parse the request (extracting the model name and other fields), then forwards to the upstream provider.

The gateway is **protocol-aware** in that it understands request/response shapes for supported provider types, which it needs for token extraction, model name parsing, and similar work. It does **not** translate between formats. Cross-format fallback, for example an Anthropic-format request falling back to an OpenAI-compatible endpoint, is not supported in v1. Fallback is restricted to providers of the same `spec.type`; see [Fallback Logic](fallback.md). This keeps the gateway's request path simple and avoids the large, error-prone surface area of bidirectional API translation (streaming, tool use, multimodal content, and so on).

## Streaming Responses

Most LLM usage involves streaming responses (Server-Sent Events, SSE), where the provider sends token-by-token output as a stream of chunks. The gateway supports streaming transparently.

![Sequence diagram of an SSE stream relayed through the gateway, using Anthropic's event names. Before forwarding, a note records the two adapter fixups: OpenAI-format streaming requests get stream_options include_usage injected when absent, since otherwise no usage is emitted at all, and Vertex :streamGenerateContent requests get ?alt=sse appended when absent, since otherwise Vertex returns a JSON-array stream and the SSE relay never engages. A divider marks that before the first chunk fallback is available, covering connection errors, timeouts before the first byte, and error responses returned before streaming begins. The provider then sends message_start carrying input_tokens, which the gateway relays. A second divider marks the first relayed chunk as the point of no return. Content chunks are relayed immediately in a loop with no buffering. If the stream completes, message_delta carries the cumulative output_tokens and message_stop carries no usage, and the gateway updates spend after the stream completes; a note records that a stream ending without usage metadata counts as zero spend and is logged at warning level. If the stream fails mid-way after the first chunk, the gateway closes the agent's SSE stream with an error event and does not fall back or retry, because the agent has already received partial output. A closing note records that budget is checked pre-call only, with no mid-stream enforcement.](../../diagrams/llm-streaming.svg)

Reading the diagram: the two dividers are the whole point. Everything above the second one is recoverable, because the agent has seen nothing yet. Everything below it is not, because the agent has already consumed bytes that a fallback provider would not have produced.

**Relay model.** The gateway acts as a pass-through proxy for SSE streams. When the upstream provider begins sending a streaming response (`Content-Type: text/event-stream`), the gateway relays each SSE chunk to the agent as it arrives. The gateway does not buffer the full response; chunks are forwarded immediately to preserve the low-latency benefit of streaming.

**Token counting.** The gateway inspects each SSE chunk as it relays it, accumulating usage metadata where the provider's stream format carries it. Per provider:

- **Anthropic**: `input_tokens` arrive on the `message_start` event and the cumulative `output_tokens` on the final `message_delta` event. `message_stop` carries no usage.
- **OpenAI-compatible**: a usage object appears in the final chunk preceding the `[DONE]` sentinel, but only when the request sets `stream_options: {"include_usage": true}`. The gateway therefore **injects that field into OpenAI-format streaming requests when absent**. The addition is backward-compatible: the extra terminal usage chunk has an empty `choices` array, which OpenAI client libraries tolerate, and it is relayed to the agent unchanged.
- **Google Vertex**: the adapter **appends `?alt=sse` to `:streamGenerateContent` requests when absent**, because Vertex otherwise returns a JSON-array stream rather than SSE, which would never engage the SSE relay. Usage arrives as `usageMetadata` (`promptTokenCount` / `candidatesTokenCount`) on the final streamed chunk.

The gateway extracts this data and updates spend counters after the stream completes, the same as step 9 in the non-streaming flow. A stream that ends without usage metadata (a misbehaving upstream) is counted as zero spend and logged at warning level. That is acceptable under the soft-guardrail budget model, and the log signal keeps it visible to operators.

**Budget checks.** Budget checks occur pre-call (step 5) using the last-known spend state, the same as non-streaming requests. No mid-stream budget enforcement is performed: once a stream has started, it runs to completion. This is the correct behavior, because aborting a stream mid-response would leave the agent with a partial, unusable response while still incurring provider charges for the full generation.

**Mid-stream failures.** If the upstream provider connection drops or errors mid-stream (after the first chunk has been relayed to the agent), the gateway closes the agent's SSE stream with an error event and does **not** attempt fallback. A partially-consumed stream cannot be retried: the agent has already received partial output, and replaying the request on a fallback provider would produce a different, potentially contradictory continuation. Fallback only applies to **pre-stream failures**: connection errors, timeouts before the first chunk, and error responses returned before streaming begins. See [Fallback Triggers](fallback.md#fallback-triggers).

**Provider adapter.** The `ProviderAdapter.ForwardRequest` method handles both streaming and non-streaming modes. The adapter detects streaming from the upstream response headers (`Content-Type: text/event-stream`, or `Transfer-Encoding: chunked` with SSE content) and returns a streaming reader that the gateway relays to the agent. Token extraction is adapter-specific: each adapter knows where usage metadata appears in its provider's SSE format. See [Provider Adapters](provider-routing.md#provider-adapters).
