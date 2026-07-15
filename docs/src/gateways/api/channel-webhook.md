# Channel Webhook

`POST /channels/{namespace}/{channel-path}` is the inbound webhook entry point. External webhook callers (other systems, bots, platform integrations) POST to this URL to deliver a message to an Agent via the bound AgentChannel.

## Path shape and exposure

The path shape is fixed:

- `spec.webhook.path` must begin with `/channels/{namespace}/`, where `{namespace}` is the AgentChannel's own namespace. This is reconcile-time rule 15: violations are `Ready=False, reason=InvalidPath` and the path is never registered by the gateway.
- The path must not begin with `/v1/` (apply-time CRD CEL, rule 16).

See [Cross-Resource Validation rules 15-16](../../resources/validation-and-defaulting.md#cross-resource-validation) and [Reserved Gateway Paths](overview.md#reserved-gateway-paths).

The endpoint is served on the User Gateway listener (`:8080`) with TLS using `agentry-gateway-tls`. External callers reach it through the cluster Ingress that fronts `:8080` (see [TLS and Ingress](../user/overview.md#tls-and-ingress)).

The gateway does not impose its own per-channel or per-IP rate limit on inbound webhooks. The only inbound controls are the body-size cap (`gateway.maxMessageBodyBytes`, see Request body below) and per-channel auth. Inbound rate limiting belongs at the user-provisioned Ingress: per [Scoping Summary](../../concepts/vision-and-scope.md#scoping-summary), external exposure is user-managed. The per-(namespace, model) LLM rate limits in [Rate Limiting](../llm/budgets-and-rate-limits.md#rate-limiting) bound provider load downstream of delivery but do not throttle inbound POSTs themselves.

## Routing gate

Routing is gated on `AgentChannel.status.conditions[type=Ready].status == True`. Channels that fail validation (invalid path prefix, path conflict, bad agentRef, missing auth Secret, invalid `callbackUrl`) receive no traffic, and POSTs to their paths return `401` (see the sync-mode status table below; the rationale is identical to the [polling-endpoint § 401](async-responses.md#polling-fallback)).

## Auth

Auth is configured per AgentChannel via `spec.webhook.auth`:

- **`auth.type: bearer`**: the caller sends `Authorization: Bearer <secret>`. The token is read from the Secret referenced by `auth.secretRef` (`name`, `key`).
- **`auth.type: hmac`**: the caller computes `HMAC(algorithm, secret, body)` over the **raw POST body bytes** and presents the digest in the configured `auth.hmac.header`. The digest is encoded per `auth.hmac.encoding`: `hex` is the default (the gateway emits lowercase and compares case-insensitively against the caller-supplied digest), or `base64` (standard, not URL-safe; e.g. for Shopify's `X-Shopify-Hmac-Sha256`). It may be prefixed per `auth.hmac.signaturePrefix` (default `""`; e.g. `"sha256="` for GitHub's `X-Hub-Signature-256: sha256=<hex>` shape). The gateway strips the configured prefix from the header value, decodes per the configured encoding, and constant-time-compares against the locally-computed digest.

Unlike the [callback signing contract](async-responses.md) and the [polling contract](async-responses.md#polling-fallback), the inbound HMAC input is the body alone, with no timestamp prefix. The reason is interoperability: the AgentChannel must act as a generic receiver for arbitrary third-party webhook senders that follow their own conventions (GitHub's body-only `X-Hub-Signature-256`, Shopify's base64 `X-Shopify-Hmac-Sha256`, and so on). Requiring an Agentry-defined `X-Agentry-Timestamp` would foreclose those integrations. The trade-off is that the gateway does not bound replay at the protocol layer on this surface: see the row titled "Captured inbound webhook is replayed against the gateway" in [Threat Model](../../security/threat-model.md) for the cost-replay and side-effect-replay mitigations.

The field-level wire spec lives in [AgentChannel](../../resources/agentchannel.md).

## Request body

The body is passed through unchanged at the wire level: the gateway does not validate or rewrite the caller's payload. The gateway extracts `userId` and `content` per `AgentChannel.spec.webhook.userId` and `webhook.content` (`fromHeader` or `fromBody`, with optional `fallback`) and normalizes the request into the [`POST /v1/message`](agent-endpoints.md#post-v1message) envelope before delivery to the agent. See [Request Flow](../user/overview.md#request-flow) step 3 for the normalization step and the malformed-JSON-on-`fromBody` rejection.

Two edge cases on content extraction:

- When `webhook.content` is unconfigured, the gateway uses the raw inbound body, JSON-encoded as a string, as `content`. This raw-body path requires valid UTF-8: bodies containing invalid UTF-8 bytes are rejected with `400 invalid_request` before normalization (`error.message` names the invalid byte offset).
- Operators with binary senders (e.g., protobuf-payload webhooks) must configure `webhook.content` explicitly, typically `fromHeader`, so the gateway never tries to UTF-8-decode the body.

Body bytes above `gateway.maxMessageBodyBytes` (Helm-configurable; see [Request Flow](../user/overview.md#request-flow) step 2) are rejected with `413` before normalization or auth. The size check is applied at the listener level on the raw request frame, **before path resolution**. Oversized POSTs to any path on `:8080` (registered or not) therefore yield `413`, preserving the path-existence threat model documented in [Polling Fallback § 401](async-responses.md#polling-fallback): an attacker must not be able to distinguish "registered channel" from "unregistered path" by sending oversize bodies and observing `413` vs `401`.

## Response: sync mode

Sync mode (`AgentChannel.spec.webhook.responseMode: sync`) is the default. On success, the endpoint returns `200` with the agent's response body verbatim; see [`POST /v1/message`](agent-endpoints.md#post-v1message) for the response envelope shape. Error responses:

| Status | Type | Retryable | Meaning |
|---|---|---|---|
| 400 | `invalid_request` | no | Body structurally malformed (two cases, below) |
| 401 | | no | Auth failed, or path not registered to a `Ready=True` AgentChannel |
| 413 | `request_too_large` / `response_too_large` | no | Inbound body or agent reply over its size cap (below) |
| 502 | `delivery_failed` | no | All agent-delivery attempts failed (below) |
| 504 | `wake_timeout` | no | Hibernated agent did not reach Ready within `wakeTimeout` |
| 504 | `controller_unavailable` | yes | Activator endpoint unreachable; gateway sets `Retry-After: 5` |
| 504 | `sync_deadline_exceeded` | yes | Sync-mode wall-clock exceeded `gateway.syncDeliveryDeadline` (below) |

- **400 `invalid_request`**: one of (a) `webhook.userId.fromBody` or `webhook.content.fromBody` is configured but the inbound body is not parseable as JSON; (b) `webhook.content` is unconfigured and the raw inbound body contains invalid UTF-8 bytes (the raw-body path requires valid UTF-8; binary senders must configure `webhook.content` explicitly). The body is structurally malformed in either case; retrying without changing it hits the same condition. See [User Gateway Error Responses](errors.md#user-gateway-error-responses).
- **401**: auth failed (missing or malformed credentials, signature mismatch) **or** the path is not registered to a `Ready=True` AgentChannel. The same code covers both, mirroring the [polling-endpoint 401 contract](async-responses.md#polling-fallback), to avoid revealing which webhook paths and tenant namespaces are hosted.
- **413**: either `request_too_large` (inbound body exceeded `gateway.maxMessageBodyBytes`, default 1 MiB; see [User Gateway Error Responses](errors.md#user-gateway-error-responses)) or `response_too_large` (agent reply exceeded `gateway.maxResponseBodyBytes`, default 900 KiB; the same ceiling applies in sync and async modes, see [`response_too_large`](async-responses.md)).
- **502 `delivery_failed`**: the initial attempt and 3 retries (1s, 5s, 25s backoff; 4 attempts total) all failed: connection error, non-2xx, or 200 with a malformed envelope. See [`delivery_failed`](async-responses.md) for the canonical failure-mode breakdown.
- **504 `sync_deadline_exceeded`**: the total sync-mode wall-clock (delivery retry pipeline plus agent processing) exceeded `gateway.syncDeliveryDeadline` (default 30s). The failure is timing-related, not structural, so a fresh attempt within the deadline may succeed. This code is sync-only by construction; async mode is unaffected (callback and polling are receiver-driven and not bounded by a wall-clock budget).

The error envelope shape for `400`/`401`/`413`/`502`/`504` is the structured `{ "error": { "type", "message", "retryable" } }` object documented in [User Gateway Error Responses](errors.md#user-gateway-error-responses).

### Reachability under default config

`gateway.syncDeliveryDeadline` (default 30s) is tighter than both the agent-delivery retry budget and the default `wakeTimeout` (120s), so `502 delivery_failed` and `504 wake_timeout` are practically unreachable on the sync path under defaults: `504 sync_deadline_exceeded` fires first. The full budget arithmetic is worked through in [Async Webhook Response](async-responses.md).

This is intentional positioning. Sync mode is for fast webhooks where the agent is `Running` (not hibernated) and replies within seconds. Channels backing hibernated agents (`hibernationEnabled: true`), slow-startup agents, or known-long agent processing should use `responseMode: async` with `callbackUrl` or polling, where the deadline does not apply and the full retry and wake budgets are reachable, with `delivery_failed` and `wake_timeout` delivered via the async error-payload schemas in [Async Webhook Response](async-responses.md). See [Request Flow step 6a](../user/overview.md#request-flow) for the deadline-enforcement mechanics.

`retryable: true` on `sync_deadline_exceeded` is best-effort guidance for the happy-path case (transient slowness, where a fresh attempt within the deadline may succeed). Persistent `sync_deadline_exceeded` on a channel indicates a structural problem at the agent (crash loop, broken image, slow startup); the channel should switch to `responseMode: async` to surface diagnosable error types (`delivery_failed`, `wake_timeout`) and consult `AgentChannel.status.conditions[type=PlatformConnected]` for the cause.

## Response: async mode

In async mode (`AgentChannel.spec.webhook.responseMode: async`), the gateway returns `202 Accepted` with a `requestId` envelope as soon as the per-request placeholder ConfigMap is created, and handles delivery, callback, and polling in the background. The full contract (202 body, callback signing, polling endpoint, per-error-type payloads, TTL semantics) is in [Async Webhook Response](async-responses.md).

Three points on how the inbound POST behaves in async mode:

- The sync-mode inbound-only error codes (`400 invalid_request`, `401`, `413 request_too_large`, and `503 internal_unavailable`) still apply in async mode at the inbound POST, before the 202 is returned. Auth, body-size, and (for `fromBody`-configured channels) inbound JSON-parse checks run on every inbound request regardless of `responseMode`.
- The placeholder ConfigMap `Create` runs synchronously before the 202, so a transient apiserver failure presents as `503` to the inbound caller, not as a callback or polling envelope, since neither has been wired up at that point.
- The agent-reply `413` `response_too_large` condition does not reach the inbound POST in async mode by construction (the agent has not yet been dispatched when the 202 is returned); when it fires later in the async flow it is delivered via callback or polling per [Async Webhook Response](async-responses.md).
