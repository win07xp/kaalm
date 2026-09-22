# Error reference

Both gateways report failures with one structured envelope, so agents and webhook callers branch on a stable type instead of parsing text. This page defines the envelope and then lists every status the gateway raises itself, per listener.

## The error envelope

Every gateway error is a JSON object with a single top-level `error`:

```json
{
  "error": {
    "type": "budget_exhausted",
    "message": "namespace budget exhausted: team-support on provider anthropic-shared (100% used)",
    "provider": "anthropic-shared",
    "retryable": false
  }
}
```

| Field | Present | Meaning |
|---|---|---|
| `type` | always | A stable string naming the failure class. Branch on this field. |
| `message` | always | Free text for a person. Do not parse it. |
| `retryable` | always | Whether a prompt retry can succeed. When a `Retry-After` header is present, retry no sooner than it says. |
| `provider` | LLM proxy and tool broker only, on errors raised after the provider is known | The ModelProvider or ToolProvider name the caller asked for |

`Retry-After`, when present, is integer seconds, never an HTTP date.

Two responses on the `:8443` listener are not in this envelope. A JSON-RPC header mismatch on the tool broker is a JSON-RPC error object with code `-32020` and HTTP status `400` ([The tool plane](../tool-plane.md)). Upstream provider errors relayed through the LLM proxy are the provider's own body.

## LLM Gateway error responses

The `:8443` listener raises these on the three LLM proxy paths and on `/v1/mcp/{toolProvider}`. The enforcement points that produce them are in [Request flow](../llm/request-handling.md#request-flow), [Budgets and rate limits](../llm/budgets-and-rate-limits.md), [Fallback logic](../llm/fallback.md), and [The tool plane](../tool-plane.md).

| Status | `error.type` | `retryable` | `Retry-After` | Raised when |
|---|---|---|---|---|
| 400 | `invalid_request` | no | | The body is not JSON, `model` is not `{providerRef}/{modelId}`, the provider type is not built into this gateway, or the broker request is not a single JSON-RPC message on `/v1/mcp/{toolProvider}` |
| 401 | `unauthorized` | no | | No client certificate and no bearer token, a bearer token from a Kaalm-managed Pod, a token `TokenReview` rejects, or a source IP outside the authenticated namespace |
| 403 | `invalid_cert` | no | | A client certificate whose SAN is not an Agent or AgentTask identity |
| 403 | `access_denied` | no | | The tenancy chain denies the provider ([Multi-tenancy](../../concepts/tenancy-and-tiers.md#multi-tenancy)); on the broker, the workload is not found, a namespace or class gate denies the ToolProvider, or the caller sends a session id another caller owns |
| 403 | `tool_denied` | no | | The tool is outside the workload's grant, or the JSON-RPC method is outside the broker's allowlist |
| 405 | `invalid_request` | no | | A method other than `POST` on `/v1/mcp/{toolProvider}` |
| 413 | `request_too_large` | no | | The body exceeds `gateway.maxLLMRequestBodyBytes` (default 4 MiB) on a proxy path, or the broker's own cap |
| 413 | `response_too_large` | no | | The tool provider's response exceeds the broker's response cap |
| 429 | `rate_limited` | yes | `1` | The per-namespace bucket for the model or tool provider is empty |
| 429 | `budget_exhausted` | no after a block; yes after throttles only | seconds to the next period, or the throttle's `1` | The provider, or the provider and every fallback, is budget-blocked or throttled |
| 429 | `budget_throttled` | yes | `1` | Hard enforcement: the boundary admission slot is held by another request |
| 500 | `internal_unavailable` | yes | | The broker could not re-encode a `tools/list` response |
| 502 | `provider_error` | no | | The fallback walk ended with a mixed or unclassified failure, or a fallback response could not be translated |
| 503 | `internal_unavailable` | yes | `1` | `TokenReview` failed for a bearer token that missed the cache |
| 503 | `provider_unavailable` | no | | Every attempt in the fallback walk failed to connect |
| 503 | `budget_state_unavailable` | yes | `1` | Hard enforcement: the replica cannot verify budget state and fails closed |
| 503 | `tool_unavailable` | yes | | The tool server is unreachable, its credential is unavailable, or its response is unusable |
| 504 | `provider_timeout` | no | | Every attempt in the fallback walk timed out |
| 504 | `tool_timeout` | no | | The brokered call exceeded the upstream timeout |

Notes on the rows:

- **401 and the TLS layer.** A handshake failure produces no HTTP response. The `401` rows are all raised after a successful handshake, by the [per-path middleware](../listener-tls.md#per-path-client-auth-enforcement).
- **`budget_exhausted`.** `retryable` is `false` after a block: a retry cannot succeed before the period resets or the ceiling is raised, and `Retry-After` gives the seconds to the next period, usually hours or days. When every budget outcome in the [fallback walk](../llm/fallback.md#depth-cap-semantics) was a throttle, not a block, `retryable` is `true` and `Retry-After` says when. A generic short-backoff retry loop burns against a still-exhausted budget. `budget_throttled` is the opposite case: the slot frees as soon as the in-flight request settles, so a short backoff is correct.
- **`budget_state_unavailable`.** A hard-enforcement replica could not publish its spend or refresh its peer view within the staleness window, and refuses to spend blind. It clears on the first successful exchange ([Hard enforcement](../llm/budgets-and-rate-limits.md#hard-enforcement)).
- **`provider_error`, `provider_unavailable`, `provider_timeout`.** The gateway has already walked the entire fallback chain, so `retryable` is `false`: escalate to another model or budget rather than retry. The per-attempt bound behind `provider_timeout` is `gateway.providerFirstByteTimeout` ([Failure modes](../llm/operations.md#failure-modes)).
- **`tool_denied` and `access_denied`.** The namespace and class gates on the broker reuse `access_denied`, exactly as the LLM tenancy chain does. `tool_denied` names a per-tool narrowing miss or a disallowed method, so tool-level policy is auditable on its own.
- **`retryable` on the broker.** The broker sets `retryable` to `true` on every `503`, on every response with a `Retry-After`, and on its `500`, and `false` otherwise. The rule is by status rather than by cause, so a `503` for an unusable upstream response is marked retryable; issue #232 tracks it.
- **`provider`.** The proxy sets it once the model name is parsed, so it is absent on the `401`, `403 invalid_cert`, `413`, and `503 internal_unavailable` rows and on the not-JSON and unqualified-model `400` rows. On fallback-exhausted errors it carries the provider the caller asked for, not the last fallback attempted. The broker sets it on every error it raises.

## User Gateway error responses

The `:8080` listener raises these on `/channels/*`, in sync mode as the response and in async mode before the `202`. After the `202`, the same types are delivered to the callback URL or stored for polling ([Async webhook responses](async-responses.md)). The console's `POST /v1/test-chat` raises the delivery rows on `:8443` ([Internal endpoints](internal-endpoints.md#post-v1test-chat)).

| Status | `error.type` | `retryable` | `Retry-After` | Raised when |
|---|---|---|---|---|
| 400 | `invalid_request` | no | | The body cannot be read, `fromBody` extraction is configured and the body is not JSON, the raw-body path finds invalid UTF-8, or a platform event body is not JSON |
| 401 | `unauthorized` | no | | Auth failed (a WhatsApp verification `GET` with the wrong `hub.verify_token` included), the path is not registered to a `Ready=True` channel, the channel is `Terminating`, or the method is not one the channel type serves |
| 413 | `request_too_large` | no | | A `POST` body exceeds `gateway.maxMessageBodyBytes` (default 1 MiB) |
| 413 | `response_too_large` | no | | The agent's reply exceeds `gateway.maxResponseBodyBytes` (default 900 KiB) |
| 502 | `delivery_failed` | no | | The referenced Agent does not exist, or every delivery attempt failed |
| 503 | `internal_unavailable` | yes | `5` | Async accept: the channel is at its pending cap, or the polling record could not be created |
| 503 | `internal_unavailable` | yes | `1` | Polling: the record could not be read |
| 504 | `wake_timeout` | no | | The hibernated Agent did not become reachable within `wakeTimeout` |
| 504 | `controller_unavailable` | yes | `5` | The Agent is hibernated and the activator is unreachable or not configured |
| 504 | `sync_deadline_exceeded` | yes | | Sync mode: the request outlived `gateway.syncDeliveryDeadline` (default 30s) |

Notes on the rows:

- **`401 unauthorized`.** Every cause in the row answers with the same status and the same body, message `auth failed or path not registered` included, so a caller cannot tell a registered path from any other. The listener writes no `403`.
- **`413 request_too_large`.** The cap is applied to the raw `POST` body before the path is resolved, so an oversized `POST` to any path under `/channels/` answers `413` whether or not the path exists. A `GET` has no body and is not capped.
- **`502 delivery_failed`.** With an Agent present, the gateway made the initial attempt and three retries at 1s, 5s, and 25s, and each failed with a connection error, a non-2xx status, or a `200` with an unusable envelope. With no Agent, no attempt is made and the message is `referenced Agent not found`.
- **`503` on async accept.** Both triggers run before the `202`, so a `202` always implies a polling record exists. The pending cap is `spec.webhook.maxPendingAsyncResponses` (default 100).
- **`504 controller_unavailable`.** Wake-on-demand needs the controller's activator. The gateway answers this both when the activator call fails and when no activator endpoint is configured.
- **`504 sync_deadline_exceeded`.** Sync-only: async mode has no wall-clock budget. Under default settings this fires before `wake_timeout` or `delivery_failed` can ([Sync-mode reachability](async-responses.md#sync-mode-reachability)).

The polling endpoint additionally answers `404` with type `invalid_request` for a malformed `requestId`, `400` for a missing `channelPath`, and an empty-body `404` for an unknown, expired, or foreign record ([Polling fallback](async-responses.md#polling-fallback)).

`callback_invalid` is not a row. It is async-only, fires before the callback is dialed, and is signaled by a `Warning` event on the AgentChannel while the payload is stored for polling ([Async webhook responses](async-responses.md)).
