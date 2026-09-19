# Channel webhook

`POST /channels/{namespace}/{channel-path}` is the inbound entry point for an AgentChannel of `spec.type: webhook`. An external system posts to it, and the gateway authenticates the request, normalizes it into a message, delivers the message to the bound Agent, and answers with the reply or a `202`.

The Discord and WhatsApp channel types share the route and the path rules but replace everything after the lookup; their contracts are [Discord channel](channel-discord.md) and [WhatsApp channel](channel-whatsapp.md).

## Path rules and exposure

`spec.webhook.path` must begin with `/channels/{namespace}/`, where `{namespace}` is the AgentChannel's own namespace, must not collide with another channel's path, and must not begin with `/v1/` ([Reserved gateway paths](overview.md#reserved-gateway-paths), rules 15 and 16).

The route is served on the user listener, `:8080`, with TLS from `kaalm-gateway-tls`. External callers reach it through the Ingress the operator provisions ([TLS and Ingress](../user/overview.md#tls-and-ingress)). The gateway applies no per-channel or per-IP rate limit on this route: the inbound controls are the body cap and the channel's auth, and rate limiting belongs at the Ingress. The LLM rate limits bound provider load after delivery, not inbound requests.

## Routing gate

The gateway routes a path only while its AgentChannel is `Ready=True` and not `Terminating`. A channel that fails validation (bad path, path conflict, missing Agent, missing auth Secret, invalid `callbackUrl`) receives no traffic, and a request to its path answers `401` exactly as an unregistered path does.

## Auth

`spec.webhook.auth` selects one of two schemes ([Authentication](../../resources/agentchannel.md#authentication)):

| `auth.type` | The caller sends | The gateway checks |
|---|---|---|
| `bearer` | `Authorization: Bearer {token}` | Equality with the value at `auth.secretRef` |
| `hmac` | A digest of the raw body in the header named by `auth.hmac.header` | HMAC with `auth.hmac.algorithm` over the raw body bytes, keyed with the Secret value, compared in constant time |

For `hmac`, the gateway strips `auth.hmac.signaturePrefix` from the header value (default empty; `sha256=` for GitHub's `X-Hub-Signature-256`), then decodes it per `auth.hmac.encoding`: `hex` (default, case-insensitive) or standard `base64` (Shopify's `X-Shopify-Hmac-Sha256`).

The digest covers the body alone, with no timestamp, unlike the gateway's own [callback signing](async-responses.md#callback-authentication). The channel is a generic receiver for third-party senders that follow their own conventions, and a Kaalm-defined timestamp header would exclude them. The cost is that this surface has no protocol-level replay bound; the mitigations are in [Channels and webhooks](../../security/threat-model.md#channels-and-webhooks).

## Request body

The gateway does not validate or rewrite the payload. It extracts `userId` and `content` per `spec.webhook.userId` and `spec.webhook.content` (`fromHeader` or `fromBody`, each with an optional `fallback`) and builds the [`POST /v1/message`](agent-endpoints.md#post-v1message) envelope ([Extracting userId and content](../../resources/agentchannel.md#extracting-userid-and-content)).

Two edge cases:

- When `spec.webhook.content` is unset, `content` is the raw body as a JSON string. The raw body must be valid UTF-8; otherwise the request is `400 invalid_request` and the message names the offending byte offset.
- A sender with a binary payload must set `spec.webhook.content`, usually `fromHeader`, so the gateway never decodes the body.

**Body size.** A `POST` body above `gateway.maxMessageBodyBytes` (default 1 MiB) answers `413 request_too_large`. The cap is applied to the raw body before the path is resolved, so an oversized `POST` to any path under `/channels/` answers `413` whether or not the path exists, and an attacker cannot tell a registered path from an unregistered one by sending oversized bodies.

## Response: sync mode

Sync mode (`spec.webhook.responseMode: sync`, the default) answers `200` with the agent's reply body verbatim ([Response body](agent-endpoints.md#response-body-returned-by-the-agent)). The errors it raises are the rows of [User Gateway error responses](errors.md#user-gateway-error-responses) that apply to a `POST`:

| Status | `error.type` | Raised when |
|---|---|---|
| 400 | `invalid_request` | The body cannot be read, `fromBody` is configured and the body is not JSON, or the raw-body path finds invalid UTF-8 |
| 401 | `unauthorized` | Auth failed, or the path is not routed |
| 413 | `request_too_large` | The body exceeds `gateway.maxMessageBodyBytes` |
| 413 | `response_too_large` | The reply exceeds `gateway.maxResponseBodyBytes` (default 900 KiB) |
| 502 | `delivery_failed` | The Agent does not exist, or every delivery attempt failed |
| 504 | `wake_timeout` | The hibernated Agent did not become reachable within `wakeTimeout` |
| 504 | `controller_unavailable` | The Agent is hibernated and the activator is unreachable or not configured; `Retry-After: 5` |
| 504 | `sync_deadline_exceeded` | The request outlived `gateway.syncDeliveryDeadline` |

The `401` body is the same on every branch: the message is `auth failed or path not registered` whether no channel owns the path, the method is not `POST`, or the credential is wrong. A caller cannot enumerate registered paths from it.

### Reachability under default config

`gateway.syncDeliveryDeadline` (default 30s) is shorter than the delivery retry budget (1s, 5s, and 25s between four attempts) and the default `wakeTimeout` (120s). Under defaults, a sync caller therefore sees `504 sync_deadline_exceeded` before `502 delivery_failed` or `504 wake_timeout` can fire. The arithmetic is drawn on one time axis in [Sync-mode reachability](async-responses.md#sync-mode-reachability).

This is the intended positioning. Sync mode suits a `Running` agent that replies within seconds. A channel that backs a hibernated agent, a slow-starting image, or long processing belongs in `responseMode: async`, where no wall-clock bound applies and `delivery_failed` and `wake_timeout` arrive as [error payloads](async-responses.md#error-payloads) with their causes.

`retryable: true` on `sync_deadline_exceeded` covers transient slowness. When it persists, the agent has a structural problem (crash loop, broken image, slow startup): switch the channel to async mode to see the diagnosable type, and read `status.conditions[type=PlatformConnected]` for the cause.

## Response: async mode

Async mode (`spec.webhook.responseMode: async`) answers `202 Accepted` with a `requestId` as soon as the per-request polling record is created, and runs delivery, callback, and polling in the background. The full contract is [Async webhook responses](async-responses.md).

Before the `202`, the inbound `POST` behaves as in sync mode:

- The `400`, `401`, and `413 request_too_large` rows apply unchanged. Auth, the body cap, and `fromBody` parsing run on every request.
- Two checks run after normalization and can answer `503 internal_unavailable` with `Retry-After: 5`: the channel already holds `spec.webhook.maxPendingAsyncResponses` (default 100) pending responses, or the polling record could not be created. A `202` therefore always implies a record a poll can find.
- `413 response_too_large`, `502`, and the `504` rows cannot reach the inbound caller, because the Agent has not been contacted when the `202` is sent. They are delivered later by callback or polling.
