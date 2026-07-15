# Error Reference

Both gateways report failures with the same structured error envelope, so agents and webhook callers can handle failures programmatically instead of parsing free-form text. This page defines that envelope, then gives the complete HTTP status code mapping for each gateway.

## The Error Envelope

Every gateway error response carries a single top-level `error` object:

```json
{
  "error": {
    "type": "...",
    "message": "...",
    "retryable": false
  }
}
```

- `error.type`: a stable, machine-readable string identifying the failure class. Branch on this field, never on `message`.
- `error.message`: a human-readable explanation. Free-form; do not parse it.
- `error.retryable`: whether the caller should retry the request. Each gateway's table below explains exactly what retrying means for each type (in particular, when a `Retry-After` header must be honored first).

The LLM Gateway envelope additionally carries an `error.provider` field on errors scoped to a single named provider; see the field rules after the LLM table.

Where a `Retry-After` header accompanies an error, it is always emitted as integer `delta-seconds` (RFC 7231 § 7.1.3), never as an HTTP-date. Clients should parse it as integer seconds.

## LLM Gateway Error Responses

When the LLM Gateway cannot fulfill a request, it returns the structured envelope above so agents can handle failures programmatically. For the full LLM Gateway request flow, including the budget checks and fallback logic that produce these errors, see [Request Flow](../llm/request-handling.md#request-flow); the underlying enforcement points live at [Rate Limiting](../llm/budgets-and-rate-limits.md#rate-limiting), [Budget State Management](../llm/budgets-and-rate-limits.md#budget-state-management), and [Fallback Logic](../llm/fallback.md).

**Error response body:**

```json
{
  "error": {
    "type": "budget_exhausted",
    "message": "Monthly budget for namespace team-support on provider anthropic-shared is exhausted (100% used)",
    "provider": "anthropic-shared",
    "retryable": true
  }
}
```

**HTTP status code mapping:**

| Status | `error.type` | Retryable | Trigger |
|---|---|---|---|
| 400 | `invalid_request` | no | Malformed request, unknown model, missing provider prefix |
| 401 | `unauthorized` | no | Authentication rejected at the gateway (see row notes) |
| 403 | `access_denied` | no | Provider denied by the tenancy chain (see row notes) |
| 413 | `request_too_large` | no | Inbound request body exceeded `gateway.maxLLMRequestBodyBytes` (default 4 MiB) |
| 429 | `rate_limited` | yes | Per-namespace rate limit exceeded; includes `Retry-After` header (short backoff, typically seconds) |
| 429 | `budget_exhausted` | yes | Budget blocked per policy; `Retry-After` header set to the start of the next budget period |
| 502 | `provider_error` | no | Upstream provider returned an error after the fallback chain was exhausted |
| 503 | `internal_unavailable` | yes | Gateway's `TokenReview` call to the apiserver failed (see row notes); `Retry-After: 1` |
| 503 | `provider_unavailable` | no | All providers (primary + fallback) unreachable |
| 504 | `provider_timeout` | no | Upstream provider timed out after the fallback chain was exhausted |

Row notes:

- **401 `unauthorized`.** Covers two causes: bearer-token `TokenReview` rejection (gateway-only tier), or source-IP → Pod cross-check failure (both auth modes; this is a defense-in-depth check that runs after auth succeeds, see [Namespace Identification](../llm/workload-identity.md)). mTLS handshake failures terminate at the TLS layer with no HTTP response, so they do not appear in this table.
- **403 `access_denied`.** For full-lifecycle workloads (Agent / AgentTask), the request was rejected by any of `spec.providers` (workload), `AgentClass.allowedProviders`, or `ModelProvider.allowedNamespaces`. Gateway-only-tier callers are rejected by `ModelProvider.allowedNamespaces` alone, since there is no Agent, AgentTask, or AgentClass to consult. See [Multi-tenancy](../../concepts/tenancy-and-tiers.md#multi-tenancy) for the canonical tenancy chain.
- **413 `request_too_large`.** Reduce prompt size, paginate batch calls, or externalize large attachments and reference them by URL. A fresh attempt with the same body hits the same condition. The limit is applied before provider routing, so `error.provider` is absent.
- **429 `budget_exhausted`.** Retryable only at or after the `Retry-After` moment, which is typically hours to days away.
- **502 `provider_error`, 503 `provider_unavailable`, 504 `provider_timeout`.** The gateway has already retried through the entire fallback chain on the agent's behalf, so these are not retryable: escalate to a different model/budget or operator intervention rather than retry. For `provider_timeout`, the per-attempt bound is `gateway.providerFirstByteTimeout` (default `120s`).
- **503 `internal_unavailable`.** The gateway's `TokenReview` call to the apiserver failed for a bearer-token request that missed the token cache. This affects the gateway-only tier only; mTLS callers and cached-token requests are unaffected (see [Failure Modes](../llm/operations.md#failure-modes)). Emitted with `Retry-After: 1` and clears when the apiserver recovers. The type name is deliberately reused from [`/v1/task/complete`](task-complete.md) and the [User Gateway table](errors.md#user-gateway-error-responses) below.

**Retry semantics.** The `error.retryable` field indicates whether the agent should retry the request, but with two important qualifications:

- Budget-exhausted requests are retryable but **only** at or after the `Retry-After` moment. Agents SHOULD honor `Retry-After` strictly and SHOULD NOT autopilot-retry on a short backoff schedule: a generic 429 retry loop that ignores `Retry-After` would burn against a still-exhausted budget and hit the same response.
- Fallback-exhausted errors (`provider_error`, `provider_unavailable`, `provider_timeout`) are not retryable. The gateway has already retried through the entire fallback chain on the agent's behalf, so a fresh agent-side retry within the same time horizon is unlikely to find a healthy provider. Agents should escalate to a different model/budget or operator intervention instead.

**`Retry-After` format.** Emitted as integer `delta-seconds` (RFC 7231 § 7.1.3) on every error that includes one. For `budget_exhausted` this can be a large value (for example, ~2,592,000 for a 30-day budget boundary). Clients should parse it as integer seconds, not as an HTTP-date.

**The `error.provider` field.** Present only on errors scoped to a single named provider: `access_denied` (403), `rate_limited` (429), and `budget_exhausted` (429). It is absent on pre-routing errors (`invalid_request`, `unauthorized`, `request_too_large`, `internal_unavailable`), where no provider has yet been resolved. On fallback-exhausted errors (`provider_error`, `provider_unavailable`, `provider_timeout`), `provider` carries the **originally-requested** provider, not the last fallback attempted, so callers see a stable identifier they can correlate to the qualified `provider/model` they sent.

## User Gateway Error Responses

When the User Gateway cannot deliver a webhook in **sync mode**, it returns the structured envelope to the webhook caller. The same error shapes are used in async mode but are delivered to `callbackUrl` or stored at the polling endpoint: see [Async Webhook Response](async-responses.md) for the async wire format and [Failure Modes](../user/operations.md#failure-modes) for the operational behavior behind each error type. One exception: `sync_deadline_exceeded` is **sync-only** by construction (async mode does not block on a wall-clock budget), mirroring the async-only carve-out for `callback_invalid` documented after the table.

**Error response body** (sync mode):

```json
{
  "error": {
    "type": "controller_unavailable",
    "message": "Controller activator endpoint unreachable; wake could not be triggered",
    "retryable": true
  }
}
```

**HTTP status code mapping (sync mode):**

| Status | `error.type` | Retryable | Trigger |
|---|---|---|---|
| 400 | `invalid_request` | no | Inbound body structurally malformed: not-JSON where JSON extraction is configured, or invalid UTF-8 on the raw-body path (see row notes) |
| 401 | `unauthorized` | no | Authentication rejected, or path not registered to a `Ready=True` AgentChannel; deliberately not disambiguated (see row notes) |
| 413 | `request_too_large` | no | Inbound webhook body exceeded `gateway.maxMessageBodyBytes` (default 1 MiB) |
| 413 | `response_too_large` | no | Agent reply exceeded `gateway.maxResponseBodyBytes` (default 900 KiB) |
| 502 | `delivery_failed` | no | Initial attempt and 3 retries (1s, 5s, 25s backoff; 4 total) all failed |
| 503 | `internal_unavailable` | yes | Gateway failed an internal apiserver write while accepting an async-mode webhook; `Retry-After: 1` (see row notes) |
| 504 | `wake_timeout` | no | Hibernated agent did not reach Ready within `wakeTimeout` |
| 504 | `controller_unavailable` | yes | Activator endpoint unreachable; gateway sets `Retry-After: 5` |
| 504 | `sync_deadline_exceeded` | yes | Sync-mode wall-clock (delivery + agent processing) exceeded `gateway.syncDeliveryDeadline` (default 30s) (see row notes) |

Row notes:

- **400 `invalid_request`.** Fires in one of two cases: (a) `webhook.userId.fromBody` or `webhook.content.fromBody` is configured on the AgentChannel but the inbound body is not parseable as JSON; (b) `webhook.content` is unconfigured and the raw inbound body contains invalid UTF-8 bytes (the raw-body path requires valid UTF-8; binary senders must configure `webhook.content` explicitly). The body is structurally malformed in either case, so retrying without changing it hits the same condition. The type name is reused from the [LLM Gateway error table](errors.md#llm-gateway-error-responses) above and matches the `400` envelope on [`/v1/task/complete`](task-complete.md).
- **401 `unauthorized`.** Covers missing or malformed credentials, signature mismatch, **or** a path that is not registered to a `Ready=True` AgentChannel. The cause is deliberately not disambiguated: see [Polling Fallback § 401](async-responses.md#polling-fallback) for the threat-model rationale and for the polling endpoint's looser ("any registered AgentChannel") registration check. The gateway MUST emit a generic `message` (for example `"authentication failed"`) and MUST NOT differentiate causes via `error.type`, `message`, response headers, or response timing.
- **413 `request_too_large`.** Reduce body size or split the message.
- **413 `response_too_large`.** Externalize large outputs and reference them by URL.
- **502 `delivery_failed`.** Every attempt failed with a connection error, a non-2xx response, or a 200 with a malformed envelope. See [`delivery_failed`](async-responses.md) for the canonical failure-mode breakdown.
- **503 `internal_unavailable`.** The failed write is the placeholder ConfigMap `Create` at 202-acceptance time: apiserver transiently unavailable, etcd unreachable, or a conflict. Parallel to the `/v1/task/complete` 503 (see [`POST /v1/task/complete`](task-complete.md)). Sync mode does no apiserver writes during request handling and is not affected.
- **504 `sync_deadline_exceeded`.** Sync-only by construction (async mode has no wall-clock budget). The failure is timing-related, not structural, so a fresh attempt within the deadline may succeed. **Persistent occurrence indicates a structural problem at the agent: switch the channel to `responseMode: async` for diagnosable error types and consult `PlatformConnected` for the cause** (see the reachability callout under [POST /channels/{namespace}/{channel-path}](channel-webhook.md)).

`Retry-After` is emitted as integer `delta-seconds`, the same convention as the LLM Gateway error table above.

**Why `callback_invalid` is not in the table.** It is **async-only** by construction: the failure mode is "the configured `callbackUrl` is rejected before dial", which has no sync analogue (sync responses do not use `callbackUrl`). When `callback_invalid` fires in async mode, the underlying agent response (or other error envelope) is stored at the polling endpoint, and `callback_invalid` itself is signaled only via a `Warning` event on the AgentChannel: it is not an `error.type` exposed to polling callers. See [Async Webhook Response § callback_invalid](async-responses.md).
