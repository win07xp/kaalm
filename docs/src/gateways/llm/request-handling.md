# Request handling

Every LLM call an agent makes travels through the LLM Gateway. The agent never talks to Anthropic or OpenAI directly: it talks to the gateway using the provider's own native API format, and the gateway authenticates the caller, authorizes the model, injects the real provider credential, forwards upstream, and accounts for the tokens spent.

This page walks the request path, then covers how a model name identifies a provider, how the gateway detects the API format, and how streaming responses are relayed and metered.

## Request flow

![Sequence diagram of one LLM call through the gateway, one message per step: the agent's request with a qualified model, authentication and the source-IP cross-check against the informer cache, path and body checks, route authorization against the cache, budget admission, the rate limit, request preparation, the forward to the upstream provider with a fallback walk on failure, the relay back to the agent, and the usage read and spend settle.](../../diagrams/llm-request-flow.svg)

1. **The agent sends the request.** The agent makes an HTTPS request to `$KAALM_GATEWAY_ENDPOINT` on the upstream provider's native path (`/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible) with a qualified model name in the request body; see [Model identification](#model-identification).

2. **Authentication and cross-check.** The gateway authenticates the request by mTLS client-cert SAN (Mode 1) or a `TokenReview`-verified bearer token (Mode 2). That establishes the caller's namespace and, for Mode 1, the workload's kind and name. It then checks that the Pod at the request's source IP is in that namespace. See [Workload identity](workload-identity.md).

3. **Path, body, and model name.** The path selects the request format; any other path is rejected with `400 invalid_request` (see [Request format detection](#request-format-detection)). Bodies above `gateway.maxLLMRequestBodyBytes` (default 4 MiB) are rejected with `413 request_too_large`. The body must be JSON whose `model` field is a qualified `{providerRef}/{modelId}` name, or the request is rejected with `400 invalid_request`. See [LLM Gateway error responses](../api/errors.md#llm-gateway-error-responses) for the wire contract.

4. **Route authorization.** For a workload caller, the Agent or AgentTask named by the SAN must exist and list the provider in `spec.providers`. Its AgentClass must exist, admit the workload's namespace in `allowedNamespaces` when the class sets one, and list the provider in `allowedProviders`. Each failure is `403 access_denied`. The class namespace check runs once per request, before any fallback. The provider must then exist (`400 invalid_request`), the caller's namespace must be in its `allowedNamespaces` (`403 access_denied`), and the model must be in its `spec.models` (`400 invalid_request`). The namespace check precedes the model check so a namespace not authorized for a provider never learns which models it hosts. Gateway-only-tier callers carry no workload and start at the provider check. The full chain is under [Provider routing and adapters](provider-routing.md#mtls-tier-kaalm-managed-pods).

5. **Budget admission.** The gateway applies the highest policy at or below the namespace's utilization, read from the last-known spend state with no pre-call cost estimation. None of the denials starts the fallback walk, so a capped namespace never drains a fallback provider's budget.

   | Outcome | Effect |
   |---|---|
   | `warn` | Logged; the request proceeds |
   | `degrade` | The model is rewritten to the policy's `degradeTo` |
   | `block` | `429 budget_exhausted`, `Retry-After` set to the next period boundary |
   | throttled (hard mode, boundary region) | `429 budget_throttled`, `Retry-After: 1` |
   | failed closed (hard mode, boundary region) | `503 budget_state_unavailable`, `Retry-After: 1` |

   See [Budget state management](budgets-and-rate-limits.md#budget-state-management) and [Hard enforcement](budgets-and-rate-limits.md#hard-enforcement).

6. **Rate limit.** Two buckets per (namespace, model), each sized from the provider's limit divided across the live gateway replicas, admit or reject the request. The token bucket (`tokensPerMinute`) is checked first and admits while it is above 0, consuming nothing. The request bucket (`requestsPerMinute`) then takes one token. A refusal from either is `429 rate_limited` with a computed `Retry-After`, and it does not fall back. See [Rate limiting](budgets-and-rate-limits.md#rate-limiting).

7. **Request preparation.** The gateway strips the provider prefix so the upstream sees the raw model id, applies the format's adapter fixups (see [Streaming responses](#streaming-responses)), and rewrites the headers under the [forwarded-header contract](#the-forwarded-header-contract). Traffic from an Agent caller counts as activity for [hibernation](../../controller/hibernation-and-wake.md#activity-detection); task Pods do not hibernate.

8. **Forward, with fallback.** The gateway attaches the provider's credential from its Secret in `kaalm-system` (see [Credential handling](provider-routing.md#credential-handling)) and forwards. `gateway.providerFirstByteTimeout` (default `120s`) bounds each attempt twice: the wait for the first response byte, and then the longest gap between response bytes. A response that keeps sending bytes is never cut, however long it runs. On a fallbackable failure the gateway walks `spec.fallback` up to `maxFallbackDepth`; see [Fallback logic](fallback.md).

9. **Response relay.** A buffered response is returned whole; an SSE response is relayed chunk by chunk as it arrives (see [Streaming responses](#streaming-responses)). A response from a fallback candidate of another format is translated back into the caller's format; see [Crossing formats](fallback.md#crossing-formats).

10. **Usage and spend.** The gateway reads token usage from the response with the serving provider's adapter (see [Provider adapters](provider-routing.md#provider-adapters)); for a stream, from the usage-bearing SSE events. Spend is settled on the provider that served, at its prices for the model, and under hard enforcement the same step releases the admission slot. The same step subtracts the input plus output tokens from the step 6 token bucket, the primary provider's bucket for the requested model, even when a fallback served the call; see [The token limit](budgets-and-rate-limits.md#the-token-limit).

### The forwarded-header contract

Step 7 rewrites the request headers under four rules:

| Rule | Headers | Why |
|---|---|---|
| Strip inbound authentication material before injecting the provider credential | `Authorization`, `x-api-key`, `api-key` | Anthropic authenticates with `x-api-key`, while a gateway-only-tier caller arrives with `Authorization: Bearer <SA-token>`. Injecting the Anthropic key alone would leave the bearer token in place and forward a live, audience-bound Kubernetes credential into third-party provider logs. |
| Drop hop-by-hop headers (RFC 7230 §6.1) | `Connection`, `Keep-Alive`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`, `Proxy-Authorization`, and any header the `Connection` header names | They are scoped to a single connection and must not be relayed across a proxy hop. |
| Pin `Accept-Encoding` to `identity` | `Accept-Encoding` | Relaying a caller's `Accept-Encoding: gzip` would make every response body opaque to usage extraction and silently zero all spend accounting. |
| Reset per-hop framing | `Host`, `Content-Length` | The upstream `Host` comes from the provider's endpoint, and the length from the rewritten body. |

## Model identification

Agents identify both the provider and the model in each LLM request with a **qualified model name**, `{providerRef}/{modelId}`:

- `anthropic-shared/claude-opus-4-6`: Claude Opus through the `anthropic-shared` ModelProvider
- `local-vllm/llama-3-70b`: Llama 3 70B through a local vLLM instance registered as a ModelProvider

The gateway splits the name on the **first** `/`: the prefix identifies the ModelProvider by `metadata.name`, and the suffix is the raw model id that must appear in the provider's `models` list. Before forwarding, the gateway strips the prefix, so the upstream Anthropic API receives `claude-opus-4-6`, not `anthropic-shared/claude-opus-4-6`.

The qualified name picks out the (provider, model) pair when several ModelProviders offer similar model names, for example a managed Anthropic endpoint and an OpenAI-compatible proxy both serving Claude models. It travels in the request body's `model` field for every served format.

## Request format detection

The agent sends LLM requests in the upstream provider's native API format, and the gateway detects the format from the URL path:

- `/v1/messages`: Anthropic format
- `/v1/chat/completions`: OpenAI and OpenAI-compatible format (also used by vLLM, Ollama, LiteLLM)
- `/v1/completions`: OpenAI legacy completions format

These three paths are the whole served inbound surface; any other path on the cluster listener is rejected with `400 invalid_request`. The format selects the adapter (see [Provider adapters](provider-routing.md#provider-adapters)).

### The google-vertex type is reserved

The ModelProvider API accepts `spec.type: google-vertex`, but the type is not served: no Vertex-format inbound path is routed on the cluster listener, cross-format fallback cannot translate into it ([rule 12](../../resources/validation/providers.md) keeps `google-vertex` chains same-type), and the gateway does not mint the OAuth2 tokens the platform requires in place of static API keys. Only the controller's liveness probe does, to check the provider is reachable; see [The google-vertex probe](../../controller/reconcilers/modelprovider.md#the-google-vertex-probe).

Google renamed the platform from Vertex AI to the Gemini Enterprise Agent Platform with the wire API unchanged, so `google-vertex` remains the stable enum value. Serving it is a backlog item on the [roadmap](../../ROADMAP.md#beyond).

The gateway understands the request and response formats of the supported provider types, for usage extraction and model name parsing. It translates between formats only for a fallback candidate of a different `spec.type`: the request is rewritten into the candidate's format before the first byte, and the response, streaming or not, is rewritten back; see [Crossing formats](fallback.md#crossing-formats). Everywhere else the request passes through: the primary and same-type fallbacks are spoken to in the caller's format.

## Streaming responses

The gateway detects a streaming response (Server-Sent Events, SSE) from the upstream `Content-Type: text/event-stream` and relays each chunk to the agent as it arrives, without buffering.

![Sequence diagram of an SSE stream relayed through the gateway with Anthropic event names. Before the first chunk a pre-stream failure walks the fallback chain. The provider's message_start event carries input_tokens and is relayed. After the first relayed chunk there is no fallback: content chunks are relayed unbuffered, and either the stream completes with message_delta carrying output_tokens and message_stop, after which the gateway settles spend, or the upstream fails or goes idle mid-stream, the gateway sends the agent an error event and ends the stream, and it settles the usage accumulated so far.](../../diagrams/llm-streaming.svg)

**Adapter fixup.** For an OpenAI or OpenAI-compatible streaming request, the adapter injects `stream_options: {"include_usage": true}` when it is absent. Without it the stream carries no usage at all. The extra terminal usage chunk has an empty `choices` array, which OpenAI client libraries tolerate, and is relayed to the agent unchanged. Anthropic streams carry usage without a fixup.

**Usage.** For Anthropic, `input_tokens` arrive on `message_start` and the cumulative `output_tokens` on the final `message_delta`. For OpenAI-compatible formats, a usage object appears in the final chunk before the `[DONE]` sentinel. Spend is settled, and the tokens are debited from the token bucket, after the stream ends, as in step 10. A successful response that carries no usage, streamed or not, settles at zero spend, and the gateway logs a warning and increments `kaalm_llm_usage_missing_total{provider,model}`. A truncated stream that ends before any usage arrives counts too. Under soft enforcement that is an accepted approximation; under [hard enforcement](budgets-and-rate-limits.md#hard-enforcement) it is one of the stated limits of the guarantee, because no gateway-side cap can meter spend the provider never reports.

**Budget checks.** Budget admission happens before the call (step 5), as for non-streaming requests. There is no mid-stream enforcement: aborting a stream would leave the agent with a partial, unusable response while the provider still charges for the full generation.

**Mid-stream failures.** If the upstream connection drops or errors after the first chunk has been relayed, the gateway sends one error event, settles the usage accumulated so far, and ends the agent's stream. The response status is already committed, so the event is how the agent tells a truncated stream from a complete one. It does not fall back or retry: the agent has already received partial output, and replaying the request on another provider would produce a different, possibly contradictory continuation. Fallback applies only to failures before the stream starts; see [Fallback triggers](fallback.md#fallback-triggers).

A stream also ends this way when the provider goes silent for longer than the gap bound in step 8. The event is in the caller's format, whatever the serving provider's format is, and carries `provider_timeout` for the idle bound or `provider_error` for any other read failure. The [Mid-stream error event](../api/errors.md#mid-stream-error-event) reference shows the event in both formats.

**Cross-format usage.** For a cross-format candidate, usage is read from the upstream's own events before translation. See [Provider adapters](provider-routing.md#provider-adapters).
