# Agentry — Gateway HTTP Endpoints

This document defines the HTTP endpoints exposed by the Agentry Gateway and the contract for agent-implemented endpoints. For CRD specifications, see [API_RESOURCES.md](./API_RESOURCES.md).

The LLM proxy endpoints accept either **mTLS** (Agentry-managed Agent/AgentTask Pods) or a **`TokenReview`-validated ServiceAccount bearer token** (gateway-only tier), with a **source-IP → Pod cross-check** in both modes. The agent-only internal endpoints below — `POST /v1/task/complete` and `POST /v1/agent/heartbeat` — are **mTLS-only**: there is no SA-bearer alternative, and gateway-only-tier workloads (which have no AgentTask or Agent identity) cannot reach them. The controller-only endpoints — `GET /v1/activity` and `GET /v1/channels/health` — are likewise **mTLS-only** and additionally require the controller's SAN; Agent/AgentTask client certs are rejected with `403`. See [Namespace Identification](./GATEWAY_LLM.md#namespace-identification) for the full flow and [Agent→Gateway Authentication](./SECURITY.md#agentgateway-authentication) for the threat-model analysis.

Kubelet liveness/readiness probe endpoints (`/healthz`, `/readyz`) terminate on a separate internal health port, not on `:8443` or `:8080`, and are documented in [GATEWAY_LLM.md § Gateway Readiness](./GATEWAY_LLM.md#gateway-readiness) — they are out of scope for this document.

---

## Reserved Gateway Paths

The following path prefixes are reserved for gateway-internal use and must not be used as `AgentChannel.spec.webhook.path` values. These paths conflict with gateway-internal endpoints — `/v1/` is served on the LLM Gateway listener at `:8443` for gateway-internal calls and on the User Gateway listener at `:8080` for the async polling endpoint, and is otherwise reserved on `:8080` against webhook path collisions:

- `/v1/` — all current and future gateway-internal endpoints (LLM proxy paths, task completion, heartbeat, async polling, channel health, activity). The controller's `POST /v1/activate/{namespace}/{agentName}` lives on the controller Service (port 9443), not the gateway, and is therefore unreachable from a webhook path regardless of this rule.

AgentChannels whose `spec.webhook.path` begins with `/v1/` are rejected at apply time by the CRD CEL validation in [Cross-Resource Validation rule 16](./API_RESOURCES.md#cross-resource-validation). Rule 16 is subsumed by rule 15 (the enforced `/channels/{namespace}/` prefix already excludes `/v1/`) and is retained as defense in depth — the resource is rejected at the API server before the AgentChannelReconciler ever observes it, so no `Ready=False` status is set. The recommended developer convention is to use `/channels/` as a prefix (e.g., `/channels/team-support/support-assistant`).

---

## LLM Proxy Endpoints

The LLM proxy accepts agent requests on the upstream provider's native API paths and forwards them to the resolved ModelProvider. Recognized path patterns:

- `/v1/messages` — Anthropic format
- `/v1/chat/completions` — OpenAI / OpenAI-compatible format (also vLLM, Ollama, LiteLLM)
- Provider-specific paths — see [GATEWAY_LLM.md § Request Format Detection](./GATEWAY_LLM.md#request-format-detection) for the full mapping

Request and response bodies are passthrough to the upstream provider's native format — Agentry adds no envelope of its own. The gateway injects the provider API key, strips the `provider/` prefix from the model name, and relays the response (including SSE streams transparently). Errors returned by the gateway itself (budget exhaustion, rate limit, fallback exhaustion) use the structured envelope documented in [LLM Gateway Error Responses](#llm-gateway-error-responses) below.

Auth, namespace identification, provider routing, budget enforcement, fallback, and streaming behavior are documented in [GATEWAY_LLM.md](./GATEWAY_LLM.md). The per-path auth profile for these endpoints is consolidated in [ARCHITECTURE.md § The Agentry Gateway](./ARCHITECTURE.md#the-agentry-gateway) (`:8443` listener auth profile table).

---

## `POST /channels/{namespace}/{channel-path}` (external — webhook caller)

The inbound webhook entry point. External webhook callers (other systems, bots, platform integrations) POST to this URL to deliver a message to an Agent via the bound AgentChannel. The path shape is fixed by CRD CEL: `spec.webhook.path` must begin with `/channels/{namespace}/` where `{namespace}` is the AgentChannel's own namespace, and must not begin with `/v1/` — see [Cross-Resource Validation rules 15-16](./API_RESOURCES.md#cross-resource-validation) and [Reserved Gateway Paths](#reserved-gateway-paths). Served on the User Gateway listener (`:8080`) with TLS using `agentry-gateway-tls`; external callers reach it through the cluster Ingress that fronts `:8080` (see [GATEWAY_USER.md § TLS and Ingress](./GATEWAY_USER.md#tls-and-ingress)). The gateway does not impose its own per-channel or per-IP rate limit on inbound webhooks — only the body-size cap (`gateway.maxMessageBodyBytes`, see Request body below) and per-channel auth. Inbound rate limiting belongs at the user-provisioned Ingress (per [ARCHITECTURE.md § Scoping Summary](./ARCHITECTURE.md#scoping-summary), external exposure is user-managed); the per-(namespace, model) LLM rate limits in [GATEWAY_LLM.md § Rate Limiting](./GATEWAY_LLM.md#rate-limiting) bound provider load downstream of delivery but do not throttle inbound POSTs themselves.

Routing is gated on `AgentChannel.status.conditions[type=Ready].status == True` — channels that fail validation (path conflict, bad agentRef, missing auth Secret, invalid `callbackUrl`) receive no traffic and POSTs to their paths return `401` (see status table below; rationale identical to [polling-endpoint § 401](#polling-fallback)).

**Auth.** Per-AgentChannel `spec.webhook.auth`:

- **`auth.type: bearer`** — caller sends `Authorization: Bearer <secret>`. The token is read from the Secret referenced by `auth.secretRef` (`name`, `key`).
- **`auth.type: hmac`** — caller computes `HMAC(algorithm, secret, body)` over the **raw POST body bytes** and presents the digest in the configured `auth.hmac.header`, encoded per `auth.hmac.encoding` (`hex` default, lowercase case-insensitive compare; or `base64` standard not URL-safe — e.g. for Shopify's `X-Shopify-Hmac-Sha256`) and optionally prefixed per `auth.hmac.signaturePrefix` (default `""`; e.g. `"sha256="` for GitHub's `X-Hub-Signature-256: sha256=<hex>` shape). The gateway strips the configured prefix from the header value, decodes per the configured encoding, and constant-time-compares against the locally-computed digest. Unlike the [callback signing contract](#async-webhook-response-gateway-managed) and the [polling contract](#polling-fallback), the inbound HMAC input is the body alone — no timestamp prefix — so that the AgentChannel can act as a generic receiver for arbitrary third-party webhook senders that follow their own conventions (GitHub's body-only `X-Hub-Signature-256`, Shopify's base64 `X-Shopify-Hmac-Sha256`, etc.); requiring an Agentry-defined `X-Agentry-Timestamp` would foreclose those integrations. The trade-off is that the gateway does not bound replay at the protocol layer on this surface — see the inbound-replay row in [SECURITY.md § Threat Model](./SECURITY.md#threat-model) for the cost-replay and side-effect-replay mitigations. Field-level wire spec in [API_RESOURCES.md § AgentChannel](./API_RESOURCES.md#agentchannel).

**Request body.** Passed through unchanged at the wire level — the gateway does not validate or rewrite the caller's payload. The gateway extracts `userId` and `content` per `AgentChannel.spec.webhook.userId` and `webhook.content` (`fromHeader` or `fromBody`, with optional `fallback`) and normalizes the request into the [`POST /v1/message`](#post-v1message-agent-only--agent-implemented) envelope before delivery to the agent — see [GATEWAY_USER.md § Request Flow](./GATEWAY_USER.md#user-gateway--request-flow) step 3 for the normalization step and the malformed-JSON-on-`fromBody` rejection. When `webhook.content` is unconfigured, the gateway uses the raw inbound body, JSON-encoded as a string, as `content`. Body bytes above `gateway.maxMessageBodyBytes` (Helm-configurable; see [GATEWAY_USER.md § Request Flow](./GATEWAY_USER.md#user-gateway--request-flow) step 2a) are rejected with `413` before normalization or auth. The size check is applied at the listener level on the raw request frame, **before path resolution** — so oversized POSTs to any path on `:8080` (registered or not) yield `413`, preserving the path-existence threat model documented in [Polling Fallback § 401](#polling-fallback) (an attacker must not be able to distinguish "registered channel" from "unregistered path" by sending oversize bodies and observing `413` vs `401`).

**Response — sync mode** (`AgentChannel.spec.webhook.responseMode: sync`, the default):

| Status | Meaning |
|---|---|
| 200 | Agent returned `200`; gateway forwards the agent's response body verbatim — see [`POST /v1/message`](#post-v1message-agent-only--agent-implemented) for the response envelope shape |
| 400 | `invalid_request` — `webhook.userId.fromBody` or `webhook.content.fromBody` is configured on the AgentChannel but the inbound body is not parseable as JSON. `retryable: false` (the body is structurally malformed; retrying without changing the body hits the same condition). See [User Gateway Error Responses](#user-gateway-error-responses) |
| 401 | Auth failed (missing or malformed credentials, signature mismatch) **or** the path is not registered to a `Ready=True` AgentChannel — same code for both, mirroring the [polling-endpoint 401 contract](#polling-fallback), to avoid revealing which webhook paths and tenant namespaces are hosted |
| 413 | `request_too_large` — inbound body exceeded `gateway.maxMessageBodyBytes` (default 1 MiB; see [User Gateway Error Responses](#user-gateway-error-responses)); **or** `response_too_large` — agent reply exceeded `gateway.maxResponseBodyBytes` (default 900 KiB; same ceiling in sync and async modes — see [`response_too_large`](#async-webhook-response-gateway-managed)) |
| 502 | `delivery_failed` — initial attempt and 3 retries (1s, 5s, 25s backoff; 4 total) all failed: connection error, non-2xx, or 200 with malformed envelope; see [`delivery_failed`](#async-webhook-response-gateway-managed) for the canonical failure-mode breakdown |
| 504 | `wake_timeout` (hibernated agent did not reach Ready within `wakeTimeout`) **or** `controller_unavailable` (activator endpoint unreachable; gateway sets `Retry-After: 5`) |

The error envelope shape for `400`/`401`/`413`/`502`/`504` is the structured `{ "error": { "type", "message", "retryable" } }` object documented in [User Gateway Error Responses](#user-gateway-error-responses).

**Response — async mode** (`AgentChannel.spec.webhook.responseMode: async`): the gateway returns `202 Accepted` with a `requestId` envelope as soon as the per-request placeholder ConfigMap is created, and handles delivery, callback, and polling in the background. Full contract — 202 body, callback signing, polling endpoint, per-error-type payloads, TTL semantics — in [Async Webhook Response](#async-webhook-response-gateway-managed). Sync-mode inbound-only error codes (`400 invalid_request`, `401`, and `413 request_too_large`) still apply in async mode at the inbound POST, before the 202 is returned — auth, body-size, and (for `fromBody`-configured channels) inbound JSON-parse checks run on every inbound request regardless of `responseMode`. The agent-reply `413` `response_too_large` condition does not reach the inbound POST in async mode by construction (the agent has not yet been dispatched when the 202 is returned); when it fires later in the async flow it is delivered via callback or polling per [Async Webhook Response](#async-webhook-response-gateway-managed).

---

## `POST /v1/task/complete` (AgentTask only)

Called by the agent container of an AgentTask with `completion.condition: agentReported` to report task completion. The gateway updates the pre-existing `{taskName}-completion` ConfigMap in the task's namespace — created by the [AgentTaskReconciler](./CONTROLLER_RECONCILERS.md#agenttaskreconciler) at task provisioning time, owned by the AgentTask for cascade deletion — using the `update, patch`-only RBAC scoped to that exact ConfigMap name (see [Gateway ServiceAccount permissions](./SECURITY.md#gateway-serviceaccount-permissions)). The gateway always patches and does not deduplicate calls; the AgentTaskReconciler watches the ConfigMap for changes and transitions to `Completing` on the first observed payload, so duplicate or rapidly-successive calls within one Pod's lifecycle are last-write-wins against the reconciler's read. Re-completion across a `backoffLimit` retry is the supported multi-call path: the reconciler resets the ConfigMap to `data: {}` before the replacement Pod runs, so the new Pod's `/v1/task/complete` lands on a fresh mailbox. See [AgentTask State Machine](./CONTROLLER_LIFECYCLE.md#agenttask) (transitions and retry mechanics).

**Request body:**

```json
{
  "status": "success",
  "message": "PR opened successfully",
  "artifacts": {
    "pr-url": "https://github.com/acme/widgets/pull/587",
    "summary": "Fixed null pointer in WidgetService.get(). Added regression test."
  }
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `status` | string | yes | `"success"` or `"failure"` |
| `message` | string | no | Human-readable completion message |
| `artifacts` | map[string]string | no | Key-value pairs matching the names declared in `spec.artifacts`. The gateway validates that the payload's artifact names match `spec.artifacts` exactly (every declared name present, no undeclared names) and returns `400 invalid_request` on mismatch. |

**Response:** `200 OK` with empty body on success. `400 Bad Request` if the request body is not valid JSON, the required `status` field is missing or has a value other than `"success"` or `"failure"`, `artifacts` is present but is not an object of string-to-string entries, or the artifact names in `artifacts` do not match `AgentTask.spec.artifacts` exactly (a declared name is missing from the payload, or an undeclared name appears in it — `error.message` names the offending key, e.g. `"missing declared artifact: pr-url"` or `"undeclared artifact in payload: extra-key"`). The gateway resolves `spec.artifacts` from its existing cluster-wide `AgentTask` watch — the same cache it uses for the `exitCode` short-circuit below — so the name check runs synchronously and the agent learns of the mismatch before exiting rather than discovering it later via `AgentTask.status`. The gateway validates the body before patching the ConfigMap, so a `400` means no state change. `403 Forbidden` in two cases: (a) the calling Pod is not associated with an AgentTask, or (b) the calling AgentTask has `completion.condition: exitCode` — `exitCode` tasks signal completion via container exit and have no gateway-side completion mailbox (the per-task completion ConfigMap and the per-task `update,patch` Role are only provisioned in `agentReported` mode — see [Per-Agent and Per-Task Child Resources](./ARCHITECTURE.md#per-agent-and-per-task-child-resources) and [AgentTask completion detection](./CONTROLLER_LIFECYCLE.md#agenttask)). The gateway resolves the calling task via the same `AgentTask` watch and short-circuits case (b) with `reason=TaskNotAgentReported` before attempting any patch. `413 Payload Too Large` if any single artifact value exceeds 4 KiB (`error.message` names the offending artifact key) or the sum of `message` plus all artifact value bytes exceeds 32 KiB (`error.message` names the combined-budget overflow). Sizes are measured in UTF-8 bytes against the `message` and `artifacts` value strings only; artifact key bytes are not counted (keys are bounded by Kubernetes ConfigMap key naming rules). The combined cap exists because the body is buffered in gateway memory before validation and then patched into the per-task ConfigMap, which has the standard ~1 MiB Kubernetes object limit. Large artifacts should be stored externally and referenced by URL in the value. The AgentTaskReconciler defensively re-validates artifact names against `spec.artifacts` when reading the ConfigMap (cheap loop, belt-and-suspenders against any future RBAC drift on the per-task `update,patch` Role); the reconciler remains the final authority on `AgentTask` state, but under normal operation the gateway-side check makes the reconciler's re-check a no-op. Error responses (`400`, `403`, `413`) carry the structured `{ "error": { "type", "message", "retryable" } }` envelope — same shape as [User Gateway Error Responses](#user-gateway-error-responses) — with `retryable: false` on all three (a duplicate `/v1/task/complete` from the same Pod hits the same condition). `error.type` is `invalid_request` for 400, `access_denied` for 403, and `request_too_large` for 413, reusing the type names from the [LLM Gateway](#llm-gateway-error-responses) and [User Gateway](#user-gateway-error-responses) error tables; `error.message` carries the human-readable diagnostic noted above (offending artifact name for the name-match `400`, offending artifact key for the 4 KiB cap, combined-budget overflow for the 32 KiB cap, or the specific 400/403 reason).

---

## `POST /v1/agent/heartbeat` (Agent only)

Called by the agent container to signal liveness for idle detection. Only meaningful when [`spec.lifecycle.activitySource`](./API_RESOURCES.md#agent) is `agentHeartbeat` or `both`. The gateway accepts heartbeats unconditionally and updates the agent's last-activity timestamp in its in-memory activity store; per-Agent filtering by `activitySource` happens controller-side at merge time (see [`/v1/activity` response semantics](#get-v1activity-internal--controller-use-only) and [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection)), so heartbeats from `gatewayTraffic`-only agents are recorded at the gateway and silently dropped at merge — starter templates may emit heartbeats unconditionally without consulting the Agent CR.

**Request body:** empty or `{}`.

**Response:** `200 OK` with empty body. `403 Forbidden` if the calling Pod is not associated with an Agent (which includes the AgentTask-SAN-at-listener / Agent-only-at-handler case — see [ARCHITECTURE.md § The Agentry Gateway](./ARCHITECTURE.md#the-agentry-gateway)). Error responses carry the structured `{ "error": { "type", "message", "retryable" } }` envelope — `error.type: access_denied`, `retryable: false` — reusing the type vocabulary from [`/v1/task/complete`](#post-v1taskcomplete-agenttask-only) and the [User Gateway error table](#user-gateway-error-responses).

Heartbeat frequency is the agent's choice. A reasonable default is every 30-60 seconds. The gateway coalesces rapid heartbeats in memory — no etcd or API server writes occur per heartbeat.

---

## `POST /v1/message` (Agent only — agent-implemented)

This endpoint is **implemented by the agent container**, not by the gateway. The User Gateway calls it to deliver normalized channel messages — see [User Gateway Request Flow](./GATEWAY_USER.md#user-gateway--request-flow). Agents that use AgentChannel must expose this endpoint on `$AGENTRY_HEALTH_PORT` (default 8080).

**Agent-side auth contract.** The agent's `/v1/message` listener must terminate TLS using its cert-manager-issued cert (`$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY`), require a client certificate (`tls.RequireAndVerifyClientCert` or equivalent) with `ClientCAs` loaded from `$AGENTRY_CA_CERT`, and reject any peer cert whose SAN does not match the gateway Service DNS — `agentry-gateway.agentry-system.svc.cluster.local` or `agentry-gateway.agentry-system.svc`. Cert-less connections fail at the TLS handshake; cert-bearing connections with a mismatched SAN return `403 Forbidden` before the handler runs. Layered with the synthesized NetworkPolicy gateway → agent ingress allow rule, this is what keeps the message path safe under a misconfigured per-Agent NetworkPolicy. See [RUNTIME_CONTRACT.md bullet 4](./RUNTIME_CONTRACT.md) and [SECURITY.md § In-cluster TLS](./SECURITY.md#in-cluster-tls-bidirectional).

**Request body (sent by the gateway):**

```json
{
  "messageId": "550e8400-e29b-41d4-a716-446655440000",
  "channelType": "webhook",
  "channelId": "/channels/team-support/support-assistant",
  "userId": "caller-id-from-header-or-body",
  "sessionId": "optional-session-uuid",
  "content": "Hello, I need help with my order",
  "attachments": [],
  "metadata": {}
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `messageId` | string (UUID) | yes | Unique identifier for this message, generated by the gateway. **All agents implementing `/v1/message` MUST deduplicate on this value** — the gateway's agent-delivery retry pipeline (up to 3 retries with 1s/5s/25s backoff — see [`delivery_failed`](#async-webhook-response-gateway-managed)) reuses the same `messageId` across attempts for every agent, and an earlier attempt may have reached the agent even when the gateway's read of the response failed. An in-memory LRU is sufficient for non-hibernated agents; agents with `hibernationEnabled: true` must additionally persist the dedup buffer across pod restarts. Caller retries (e.g., a webhook caller resending after a sync-mode 504) are delivered as a fresh message with a new `messageId`. See [RUNTIME_CONTRACT.md item 7](./RUNTIME_CONTRACT.md). |
| `channelType` | string | yes | Platform type: `"webhook"` in v1 (Discord, WhatsApp in v1.1) |
| `channelId` | string | yes | Platform-specific channel identifier |
| `userId` | string | yes | Platform-specific user identifier. Extracted per `AgentChannel.spec.webhook.userId` config (`fromHeader` or `fromBody`); falls back to the configured `fallback` value (or empty string if unconfigured). When `session.enabled: true` and userId is empty, all unattributed requests share a session. |
| `sessionId` | string | no | Present when `AgentChannel.spec.session.enabled: true`. Computed as `UUIDv5(namespace: f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d, name: channelId + ":" + userId)` — identical across replicas and gateway restarts. Session expiry is the agent's responsibility. |
| `content` | string | yes | The user's message text — populated per `AgentChannel.spec.webhook.content` extraction (`fromHeader`, `fromBody`, or raw-body fallback when unconfigured); see [API_RESOURCES.md § AgentChannel design notes](./API_RESOURCES.md#agentchannel) |
| `attachments` | array | no | List of attachment objects (platform-specific schema) |
| `metadata` | map | no | Platform-specific fields (e.g., `guildId` for Discord) |

**Response body (returned by the agent):**

```json
{
  "content": "I'd be happy to help! Can you share your order number?",
  "attachments": [],
  "metadata": {}
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `content` | string | yes | The agent's reply text |
| `attachments` | array | no | Attachments to send in the reply |
| `metadata` | map | no | Optional platform-specific reply metadata |

**Agent → gateway:** `200 OK` with a JSON-parseable response envelope containing the required `content` field is expected. The gateway validates the envelope shape before forwarding: a non-2xx status, a connection error, an unparseable body, or a 200 with a missing/non-string `content` field all feed the gateway's agent-delivery retry pipeline as a `delivery_failed` signal (same schedule in both sync and async modes — see [`delivery_failed`](#async-webhook-response-gateway-managed)). On retry exhaustion, sync callers receive `502` with the `delivery_failed` envelope and async callers receive the same payload via callback or polling. Failures are recorded in AgentChannel status conditions in either case. Optional fields (`attachments`, `metadata`) are not validated by the gateway and pass through as-is.

**Gateway → webhook caller (sync mode only):** the gateway returns the agent's response body verbatim with `200 OK` on success. On failure it returns a structured error envelope using the [User Gateway Error Responses](#user-gateway-error-responses) status mapping below: `502` for `delivery_failed`, `504` for `wake_timeout` and `controller_unavailable`, `413` for `response_too_large`.

---

## Async Webhook Response (gateway-managed)

When an AgentChannel has `spec.webhook.responseMode: async`, the gateway handles the asynchronous response flow. The agent's implementation is unchanged — it still receives `POST /v1/message` and returns a response envelope. The async behavior is entirely gateway-side.

**Webhook caller receives (immediate 202 response):**

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "status": "accepted",
  "message": "Message accepted for processing"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `requestId` | string (UUID) | yes | Opaque identifier for this async request. Callers must use it as-is in poll requests and must not parse or construct it independently. |
| `channelPath` | string | yes | The webhook path of the originating AgentChannel. Callers must preserve this value and pass it (URL-encoded) as the `channelPath` query parameter on poll requests — see [Polling Fallback](#polling-fallback) below. |
| `status` | string | yes | Always `"accepted"` for the immediate 202. |
| `message` | string | no | Human-readable acknowledgement. |

The gateway creates the polling record before returning this response, so a `GET /v1/channels/responses/{requestId}` issued immediately afterward will return `202` until the agent's response or a delivery error is ready (then `200`). A `5xx` from the inbound webhook means the polling record was not created — callers must not retain the `requestId` from a non-`202` response.

**Callback delivery** (gateway POSTs to `spec.webhook.callbackUrl`):

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "response": {
    "content": "I've analyzed the issue and opened a PR with the fix.",
    "attachments": [],
    "metadata": {}
  },
  "completedAt": "2026-04-05T12:10:42Z"
}
```

The gateway retries callback delivery up to 3 times with exponential backoff (1s, 5s, 25s — 4 attempts total over ~31s). If no `callbackUrl` is configured on the AgentChannel, all retries fail, or callback delivery is rejected as `callback_invalid` (see [below](#async-webhook-response-gateway-managed)), the response is stored at the polling endpoint under the original `requestId`. The polling-record TTL is bounded at 1 hour from 202-acceptance (the placeholder `Create` timestamp — not from when the payload is patched in), so the remaining retrievable window after the patch is `1h − (delivery time + agent processing + callback-retry budget)` — same TTL semantics as the error-payload storage described later in this section.

**Callback failure modes.** A callback POST is treated as failed for retry purposes on any of: TCP connect error, TLS handshake failure, per-attempt read timeout (`gateway.callbackReadTimeout`, default 10s — same default as the agent-delivery side), or any non-2xx response status. Both `4xx` and `5xx` are retried on the 1s/5s/25s schedule rather than treated as terminal — callback receivers are external systems that may be transiently misconfigured (auth-secret rotation drift, in-progress deploys), so an early-attempt `4xx` is often resolved by a later attempt within the ~31s budget. After the 4th attempt fails the response is stored at the polling endpoint as described above. The one failure mode that bypasses retry entirely is `callback_invalid` — the URL fails the deny-range / allowlist re-check before dial, so there is no callback target to retry against (see [`callback_invalid` description below](#async-webhook-response-gateway-managed)).

**Callback authentication** — every callback POST is signed using the AgentChannel's `spec.webhook.callbackAuth` (required by [API_RESOURCES.md § Cross-Resource Validation rule 25](./API_RESOURCES.md#cross-resource-validation) whenever `callbackUrl` is set). The signing contract mirrors the [polling endpoint's caller-auth contract](#polling-fallback) below — same auth types, same `X-Agentry-Timestamp` header, with a body-hash component added because callbacks have a body where polls do not. The signing material is loaded from the Secret referenced by `callbackAuth.secretRef` (or `callbackAuth.hmac.secretRef`), held by the gateway via the per-channel scoped Role created by [AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler):

- **`callbackAuth.type: bearer`** — gateway sends `Authorization: Bearer <secret>` on the callback POST.
- **`callbackAuth.type: hmac`** — gateway computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}\n{sha256(body)}"` (unix seconds, no trailing newline; body hash is the lowercase hex sha256 of the raw POST body bytes). The hex-encoded digest goes in the configured `callbackAuth.hmac.header`; `timestamp` goes in `X-Agentry-Timestamp`. Receivers should reject timestamps with skew greater than 300s against their own wall clock.

This contract applies uniformly to success payloads (above) and to every error payload delivered via callback (`delivery_failed`, `wake_timeout`, `controller_unavailable`, `response_too_large`) — error payloads are signed identically so a forged POST cannot impersonate a delivery error. `callback_invalid` is deliberately excluded from the callback-signing contract because the URL is rejected before dial, so there is no callback target to sign for; the underlying payload (the agent's response, or whatever error envelope was about to be delivered) is stored at the polling endpoint instead. See [`callback_invalid` description below](#async-webhook-response-gateway-managed).

**Error callback payload** (sent to `callbackUrl` or stored at the polling endpoint on failure):

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "delivery_failed",
    "message": "Failed to deliver message to agent after 4 attempts",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:11:07Z"
}
```

`delivery_failed` is returned when the initial attempt and all 3 retries to `POST /v1/message` to the agent's Service fail (connection error, non-2xx response, or — per the [Agent → gateway contract](#post-v1message-agent-only--agent-implemented) — a 200 with a malformed envelope). The retry schedule is 1s, 5s, 25s backoff between attempts (4 total POSTs over ~31s); this pipeline runs in both sync and async modes (see [GATEWAY_USER.md § Activator](./GATEWAY_USER.md#activator)) and is independent of the callback-delivery retry pipeline. The two pipelines ship with the same default schedule (1s/5s/25s) but are independently tunable Helm values and may diverge in future tuning.

`retryable: false` mirrors the rationale used for `wake_timeout` below — the gateway has already burned 4 attempts with exponential backoff over ~31s, so the failure is typically structural (broken image, agent crash loop, repeated 5xx, or a misconfigured per-Agent NetworkPolicy on the message path) rather than transient cluster pressure, and a fresh caller-side retry within the same time horizon is unlikely to succeed. Tenants seeing `delivery_failed` should investigate the AgentChannel's `status.conditions[type=PlatformConnected]` and the Agent Pod status rather than retrying at the caller. This is the asymmetry with `controller_unavailable` below, where the wake itself never reached the controller and a fresh attempt has a clean substrate.

`wake_timeout` is returned when the agent is `Hibernated` and fails to become Ready within `wakeTimeout`:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "wake_timeout",
    "message": "Agent did not become ready within wakeTimeout (120s)",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:12:00Z"
}
```

`retryable: false` reflects the design assumption that exceeding `wakeTimeout` indicates a structural Pod-startup problem (image pull failure, init container crash, OOM, bad spec) rather than transient cluster pressure — the same failure is expected on retry. Tenants experiencing transient timeouts should raise `wakeTimeout` rather than retrying at the caller. This is the asymmetry with `controller_unavailable` below, where the wake itself never reached the controller and retry is expected to succeed once the controller recovers.

`controller_unavailable` is returned when a message arrives for a `Hibernated` agent and the gateway cannot reach the controller's activator endpoint (connection error, 5xx after retry, or mTLS handshake / SAN authorization failure). The wake could not even be attempted, so the agent remains `Hibernated`:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "controller_unavailable",
    "message": "Controller activator endpoint unreachable; wake could not be triggered",
    "retryable": true
  },
  "failedAt": "2026-04-05T12:12:00Z"
}
```

Unlike `wake_timeout` (agent was asked to wake but did not become ready in time), `controller_unavailable` indicates the wake request itself never reached the controller — retrying the original webhook is expected to succeed once the controller recovers, hence `retryable: true`. The sync-webhook equivalent is HTTP `504 Gateway Timeout` with the same error body and a `Retry-After: 5` header (5 seconds, fixed in v1, sized to typical controller-restart and probe intervals); see [GATEWAY_USER.md § Failure Modes](./GATEWAY_USER.md#failure-modes).

`retryable: true` covers the mTLS / SAN-authorization failure modes too — at runtime those are operationally transient (cert-manager is mid-rotation across the brief overlap window where leaves trust both the old and new CA) rather than structural. Persistent mTLS / SAN failures on this path are deployment-time concerns: gateway and controller TLS certs are cert-manager-managed from the same `agentry-ca-issuer` (see [SECURITY.md § Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator-activity-channel-health) and [§ In-cluster TLS](./SECURITY.md#in-cluster-tls-bidirectional)), and the chart install fails fast if the issuer is not present. This is the asymmetry with `wake_timeout` above, where the failure typically reflects a per-Pod structural problem the same Agent will hit on retry.

`response_too_large` is returned when the agent's response body exceeds `gateway.maxResponseBodyBytes` (default 900 KiB; see [GATEWAY_USER.md § Request Flow](./GATEWAY_USER.md#user-gateway--request-flow) step 6a for the size cap rationale). The cap exists for two reasons and applies uniformly to both modes: async responses are persisted to ConfigMaps in `agentry-system` (Kubernetes object cap near 1 MiB), **and** all webhook responses — sync and async alike — are buffered in gateway memory before forwarding, so an unbounded agent reply could OOM the gateway. Agents that need to return large outputs should externalize them and reference by URL:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "response_too_large",
    "message": "Agent response body exceeded gateway.maxResponseBodyBytes (900 KiB); externalize large outputs and reference by URL",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:13:14Z"
}
```

`callback_invalid` fires when the configured `callbackUrl` host fails re-resolution against the deny ranges (loopback, link-local, RFC1918, unique-local IPv6, cloud metadata) or the configured `gateway.callbackUrl.allowlist` immediately before a delivery attempt — defeating DNS rebinding between reconcile-time validation and delivery (see [AgentChannel rule 22](./API_RESOURCES.md#cross-resource-validation)). The URL is rejected before dial, so there is no callback target to POST to. The payload that would have been delivered — the agent's response on the success path, or the corresponding error envelope (`delivery_failed`, `wake_timeout`, etc.) on the error path — is stored at the polling endpoint under the original `requestId` instead, sharing the same shape and 1-hour-from-202-acceptance TTL as the no-`callbackUrl` and retries-exhausted paths above. The AgentChannel additionally receives a `Warning` event with `reason=CallbackInvalid`; this Event is the only operator-side signal that callback was skipped. `callback_invalid` does not appear as an `error.type` in the polling payload — polling callers see whatever payload would have been sent on the callback (a success response or a different `error.type`), not a `callback_invalid` envelope.

Error payloads (`delivery_failed`, `wake_timeout`, `controller_unavailable`, `response_too_large`) are delivered to `callbackUrl` with the same 3-retry / 1s-5s-25s backoff as successful responses. If no `callbackUrl` is configured, all 4 callback attempts fail (initial + 3 retries), or callback delivery is rejected as `callback_invalid` — errors are stored at the polling endpoint under the original `requestId`, sharing the same 1-hour-from-202-acceptance TTL as the success path (see the [callback-retry exhaustion paragraph above](#async-webhook-response-gateway-managed) for the clock-origin detail). This mirrors the success-path retry-exhaustion behavior at the top of this section.

### Polling Fallback

**`GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}`**

Served over HTTPS on the User Gateway listener (port 8080, TLS using `agentry-gateway-tls` — same certificate used by the LLM listener). External callers reach it through the cluster Ingress that fronts port 8080; see [GATEWAY_USER.md § TLS and Ingress](./GATEWAY_USER.md#tls-and-ingress).

Returns the agent's response or error payload if available. The `channelPath` query parameter is the URL-encoded webhook path of the originating AgentChannel — exactly the `channelPath` value the caller received in the 202 response body. The gateway uses it to look up the AgentChannel's auth configuration and authenticate the request. Callers must preserve the value verbatim from the 202 response and URL-encode it when assembling the query string; it should not be constructed independently.

Poll requests carry no body, so the auth contract differs slightly from the original webhook:

- **`auth.type: bearer`** — the poll request presents the same bearer token in `Authorization: Bearer …` as the inbound webhook. The token is read from the Secret referenced by `AgentChannel.spec.webhook.auth.secretRef` (`name`, `key`); there is no inline token field on the AgentChannel spec. Bearer poll requests carry no timestamp and are not subject to the 300s skew check below.
- **`auth.type: hmac`** — the poll request computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}"` (unix seconds, no trailing newline), sends the bare lowercase hex digest in the configured `header` (same header name as the original webhook; `auth.hmac.signaturePrefix` and `auth.hmac.encoding` exist for inbound third-party-sender compatibility and do not apply here — polling is an Agentry-canonical surface and always uses bare hex with no prefix), and presents the timestamp in a dedicated `X-Agentry-Timestamp` header. The HMAC input is **not** the request body (poll GETs have none). HMAC poll requests with clock skew greater than 300s against the gateway's wall clock are rejected with `401 Unauthorized`; bearer poll requests are not subject to this check.

**Channel-match assertion on response retrieval:** after authenticating the caller against the AgentChannel identified by `channelPath`, and *before serving any response (including the empty-placeholder `202`)*, the gateway asserts that the `agentry-async-{requestId}` ConfigMap's `agentry.io/channel-namespace` and `agentry.io/channel-name` labels match that same AgentChannel. This prevents a caller authenticated for channel A from probing channel B's `requestId` lifecycle — `requestId`s are UUIDs but are not secrets, so without this check an attacker holding channel A's credentials plus any channel B `requestId` could observe `202` (placeholder present) versus `404` (no record) and learn whether a B request is in flight, in addition to reading B's payload once stored. On the wire a mismatch is indistinguishable from "unknown `requestId`" — both return `404 Not Found` — so the endpoint does not leak the existence of cross-channel responses or in-flight requests to a credentialed attacker. Channel mismatches are logged at the gateway with `reason=ChannelMismatch` for operator debugging.

`401 Unauthorized` is returned on any auth failure (missing or malformed credentials, signature mismatch, clock skew) **and on a well-formed but unregistered `channelPath`** — the gateway treats an unknown channel as an auth failure rather than a 404 to avoid revealing which webhook paths exist (and, by extension, which tenant namespaces are hosted, since paths are `/channels/{namespace}/...`-prefixed). This mirrors the channel-match assertion above and the [SECURITY.md threat-model row on cross-channel response retrieval](./SECURITY.md#threat-model). When the credentials authenticate correctly against `channelPath` but the stored `requestId` was originated by a different channel, the response is `404 Not Found` — same code as "unknown `requestId`".

Any gateway replica accepts this request. The replica reads the `agentry-async-{requestId}` ConfigMap in `agentry-system`: the ConfigMap is created as an empty placeholder when the originating replica returns `202` to the inbound webhook (so the polling record is queryable as soon as the caller has the `requestId`) and is patched with the response payload (or an error envelope) when the agent reply is ready. The channel-match assertion above fires on every ConfigMap-present branch — not just the payload-present one — so cross-channel `requestId` existence is never wire-observable: a present ConfigMap whose channel labels do not match `channelPath` returns `404` regardless of whether the payload field has been patched in. With the assertion passing, `200` is returned when the payload field is present; `202` when it is not. An absent ConfigMap returns `404`.

| Status | Meaning |
|---|---|
| 200 | Response or error payload available; body contains the callback payload above |
| 202 | Request accepted; agent response not yet available |
| 400 | Missing or malformed `channelPath` query parameter |
| 401 | Auth failed (missing or malformed credentials, signature mismatch, clock skew > 300s on HMAC-mode requests only, or `channelPath` not registered to any AgentChannel) |
| 404 | Unknown `requestId`, response expired (1-hour TTL from 202-acceptance), **or** stored `requestId` originated by a different channel than `channelPath` (channel-match failure — see assertion above; applies whether the ConfigMap holds a placeholder or a patched response) |

The `401` response carries the structured `{ "error": { "type", "message", "retryable" } }` envelope documented in [User Gateway Error Responses](#user-gateway-error-responses) — same `unauthorized` `error.type` and same generic-`message` / no-cause-disambiguation contract as the inbound-webhook `401`, by design (the threat-model rationale in this section's `401` paragraph above applies on both surfaces). The `400` response carries the same envelope with `error.type: invalid_request` and `retryable: false`, reusing the type name from the [LLM Gateway Error Responses](#llm-gateway-error-responses) table; `404` follows standard HTTP semantics with an empty body.

Stored responses are retained for 1 hour from 202-acceptance — the polling-record TTL clock starts when the placeholder ConfigMap is created and is **not** reset by the payload `Patch`, so the per-`requestId` polling window is bounded at 1 hour regardless of how long the agent takes to reply. The gateway enforces this on every poll read — `404` is returned when `now − placeholder.creationTimestamp > 1h`, regardless of whether the [AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler) has yet pruned the ConfigMap. Reconciler pruning is storage cleanup, not the mechanism that drives the `200`/`202` → `404` transition observed by polling callers. Neither path is a durable queue: callback delivery is best-effort (3 retries with 1s/5s/25s backoff), and the polling endpoint is the receiver-driven fallback within the same 1-hour TTL — receivers that miss the callback retries can still recover the response by polling on timeout, but past the TTL the response is gone.

---

## `GET /v1/activity` (internal — controller use only)

**Served on the LLM Gateway listener (port 8443), not the User listener.** Same listener-split rationale as `/v1/channels/health` below — an Ingress fronting `:8080` must not be able to route untrusted traffic to an endpoint whose authorization assumes a controller-SAN client cert.

Called by the [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) to read per-namespace last-activity timestamps for idle and hibernation transitions. Authenticated via **mTLS** — the caller must present the controller's `agentry-controller-tls` client cert, verified against `agentry-ca`, with a SAN that matches the controller Service DNS. There is no bearer-token or HMAC alternative. See [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator-activity-channel-health).

**Request:**

```
GET /v1/activity?namespace=team-support
```

The request carries no auth header; authentication is the mTLS client cert presented on the TLS handshake.

**Response body:**

```json
{
  "replicaStartedAt": "2026-04-05T06:00:00Z",
  "agents": {
    "support-assistant": {
      "gatewayTraffic": "2026-04-05T11:58:22Z",
      "heartbeat": "2026-04-05T11:57:10Z"
    },
    "code-helper": {
      "gatewayTraffic": "2026-04-05T11:45:10Z",
      "heartbeat": null
    }
  }
}
```

| Field | Type | Description |
|---|---|---|
| `replicaStartedAt` | timestamp | When this gateway replica started. The controller compares this to each Agent's `status.phaseTransitionTime` to detect post-restart "data is unknown" windows — see [GATEWAY_USER.md § Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api) |
| `agents` | map | Keys are Agent names in the requested namespace; values are per-source last-activity timestamps as observed by this replica |
| `gatewayTraffic` | timestamp or null | Last LLM-gateway request or inbound channel-message delivery this replica observed for the agent. `null` if no traffic since the replica started |
| `heartbeat` | timestamp or null | Last `POST /v1/agent/heartbeat` this replica received from the agent. `null` if none since the replica started |

Both signal sources are always returned. The controller applies the `Agent.spec.lifecycle.activitySource` filter (selecting `gatewayTraffic`, `heartbeat`, or the max of both) **after** merging timestamps across replicas — see [GATEWAY_USER.md § Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api) for the per-Pod-IP fan-out, the per-replica restart-detection logic, and the `tls.Config.ServerName` override required to make per-Pod-IP dialing work against a Service-DNS-scoped SAN.

**Response codes:** `200 OK` on success. `400 Bad Request` if the `namespace` parameter is missing. TLS handshake failures or SAN-authorization mismatches terminate the request at the TLS layer or with `403 Forbidden`. Only agents in the requested namespace are returned.

---

## `GET /v1/channels/health` (internal — controller use only)

**Served on the LLM Gateway listener (port 8443), not the User listener.** Port 8080 only serves inbound webhook traffic (`/channels/*`) and the async polling fallback (`/v1/channels/responses/*`); mTLS-authenticated internal endpoints live on 8443 alongside `/v1/activity`. This listener split ensures that an Ingress fronting 8080 cannot route untrusted traffic to an endpoint whose authorization assumes a controller-SAN client cert.

Called by the `AgentChannelReconciler` to populate `status.conditions[type=PlatformConnected]` on AgentChannel resources. This endpoint is internal and authenticated via **mTLS** — the caller must present the controller's `agentry-controller-tls` client cert, verified against `agentry-ca`, with a SAN that matches the controller Service DNS. There is no bearer token or HMAC header. See [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator-activity-channel-health).

**Request:**

```
GET /v1/channels/health?namespace=team-support
```

The request carries no auth header; authentication is the mTLS client cert presented on the TLS handshake.

**Response body:**

```json
{
  "windowSeconds": 300,
  "replicaStartedAt": "2026-04-29T12:00:00Z",
  "channels": {
    "/channels/team-support/support-assistant": {
      "phase": "Active",
      "state": "success",
      "reason": "WebhookReady",
      "timestamp": "2026-04-29T12:48:11Z",
      "lastError": null
    },
    "/channels/team-support/personal-assistant": {
      "phase": "Degraded",
      "state": "failure",
      "reason": "WebhookAuthFailed",
      "timestamp": "2026-04-29T12:46:02Z",
      "lastError": "webhook auth validation failed: 401 Unauthorized"
    },
    "/channels/team-support/new-channel": {
      "phase": "Active",
      "state": "empty",
      "reason": null,
      "timestamp": null,
      "lastError": null
    }
  }
}
```

| Field | Type | Description |
|---|---|---|
| `windowSeconds` | int | Length of the rolling health window observed by this replica, sourced from the Helm value `gateway.channelHealthWindow` (default `300`). Echoed in every response so the controller does not need a separate channel for the value |
| `replicaStartedAt` | timestamp | When this gateway replica started. Used by the controller to determine whether `state: "empty"` means "no in-window traffic" (replica has been up the full window) or "insufficient observation time" (replica started less than `windowSeconds` ago) |
| `channels` | map | Keys are webhook paths as registered in the gateway; values are per-channel health records as observed by this replica |
| `phase` | string | `"Active"` \| `"Degraded"` \| `"Failed"` — mirrors `AgentChannel.status.phase` as seen from the gateway |
| `state` | string | `"success"` \| `"failure"` \| `"empty"`. Computed from the replica's in-window observation list: `success` if any in-window observation succeeded; `failure` if the in-window list is non-empty and contains only failures; `empty` if no in-window observations exist on this replica |
| `reason` | string or null | For `success`, the most recent success's reason (typically `WebhookReady`). For `failure`, the most recent failure's reason — one of `WebhookAuthFailed`, `AgentNotReady`, `DispatchFailed`. `null` when `state: "empty"` |
| `timestamp` | timestamp or null | Time of the most recent in-window observation contributing to `state` (most recent success for `success`; most recent failure for `failure`). `null` when `state: "empty"` |
| `lastError` | string or null | Most recent error message seen by the gateway for this channel within the window; `null` if no error |

The third channel in the example (`new-channel`) shows `state: "empty"`: this replica has no in-window observations for that path. The controller decides whether this means the channel is genuinely silent (`Unknown` with `reason=NoRecentTraffic`) or whether observation is incomplete (preserve existing condition) by comparing `replicaStartedAt` to the window length and checking other replicas — see [GATEWAY_USER.md § Channel Health Tracking](./GATEWAY_USER.md#channel-health-tracking) and [CONTROLLER_RECONCILERS.md § AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler) step 4.

**Response codes:** `200 OK` on success. `400 Bad Request` if the `namespace` parameter is missing. TLS handshake failures or SAN-authorization mismatches terminate the request at the TLS layer or with `403 Forbidden`. Only channels whose target Agent is in the requested namespace are returned.

---

## LLM Gateway Error Responses

When the LLM Gateway cannot fulfill a request, it returns a structured error response so agents can handle failures programmatically. For the full LLM Gateway request flow including budget checks and fallback that produce these errors, see [GATEWAY_LLM.md § Request Flow](./GATEWAY_LLM.md#llm-gateway--request-flow); the underlying enforcement points live at [Rate Limiting](./GATEWAY_LLM.md#rate-limiting), [Budget State Management](./GATEWAY_LLM.md#budget-state-management), and [Fallback Logic](./GATEWAY_LLM.md#fallback-logic).

**Error response body:**

```json
{
  "error": {
    "type": "budget_exhausted",
    "message": "Monthly budget for namespace team-support on provider anthropic-shared is exhausted (100% used)",
    "provider": "anthropic-shared",
    "retryable": false
  }
}
```

**HTTP status code mapping:**

| Status | `error.type` | Meaning |
|---|---|---|
| 400 | `invalid_request` | Malformed request, unknown model, missing provider prefix |
| 401 | `unauthorized` | Authentication rejected at the gateway: bearer-token `TokenReview` rejection (gateway-only tier) or source-IP → Pod cross-check failure (both auth modes; defense-in-depth check after auth succeeds — see [GATEWAY_LLM.md § Namespace Identification](./GATEWAY_LLM.md#namespace-identification)). mTLS handshake failures terminate at the TLS layer with no HTTP response and do not appear in this table |
| 403 | `access_denied` | Provider denied. **Full-lifecycle workloads (Agent / AgentTask):** rejected by any of `spec.providers` (workload), `AgentClass.allowedProviders`, or `ModelProvider.allowedNamespaces`. **Gateway-only-tier callers:** rejected by `ModelProvider.allowedNamespaces` alone (no Agent / AgentTask / AgentClass to consult) — see [ARCHITECTURE.md § Multi-tenancy](./ARCHITECTURE.md#multi-tenancy) for the canonical tenancy chain |
| 429 | `rate_limited` | Per-namespace rate limit exceeded; includes `Retry-After` header |
| 429 | `budget_exhausted` | Budget blocked per policy; includes `Retry-After` header set to the start of the next budget period |
| 502 | `provider_error` | Upstream provider returned an error after exhausting fallback chain |
| 503 | `provider_unavailable` | All providers (primary + fallback) unreachable |
| 504 | `provider_timeout` | Upstream provider timed out after exhausting fallback chain |

The `error.retryable` field indicates whether the agent should retry the request. Rate-limited requests are retryable (short backoff). Budget-exhausted requests include a `Retry-After` header indicating when the next budget period starts, but `retryable` is `false` — the agent should not retry before that time. Access-denied and authentication-failure (`401 unauthorized`) requests are not retryable.

The `error.provider` field is present only on errors scoped to a single named provider — `access_denied` (403), `rate_limited` (429), `budget_exhausted` (429) — and is absent on pre-routing errors (`invalid_request`, `unauthorized`) where no provider has yet been resolved. On fallback-exhausted errors (`provider_error`, `provider_unavailable`, `provider_timeout`) `provider` carries the **originally-requested** provider, not the last fallback attempted, so callers see a stable identifier they can correlate to the qualified `provider/model` they sent.

---

## User Gateway Error Responses

When the User Gateway cannot deliver a webhook in **sync mode**, it returns a structured error envelope to the webhook caller. The same error shapes are used in async mode but are delivered to `callbackUrl` or stored at the polling endpoint — see [Async Webhook Response](#async-webhook-response-gateway-managed) above for the async wire format and [GATEWAY_USER.md § Failure Modes](./GATEWAY_USER.md#failure-modes) for the operational behavior behind each error type.

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

| Status | `error.type` | Retryable | Notes |
|---|---|---|---|
| 400 | `invalid_request` | no | `webhook.userId.fromBody` or `webhook.content.fromBody` is configured on the AgentChannel but the inbound body is not parseable as JSON. The body is structurally malformed; retrying without changing it hits the same condition. Reuses the type name from the [LLM Gateway error table](#llm-gateway-error-responses) and matches the `400` envelope on [`/v1/task/complete`](#post-v1taskcomplete-agenttask-only) |
| 401 | `unauthorized` | no | Authentication rejected (missing or malformed credentials, signature mismatch) **or** the path is not registered to a `Ready=True` AgentChannel. The cause is deliberately not disambiguated — see [Polling Fallback § 401](#polling-fallback) for the threat-model rationale and the polling endpoint's looser ("any registered AgentChannel") registration check. The gateway MUST emit a generic `message` (e.g. `"authentication failed"`) and MUST NOT differentiate causes via `error.type`, `message`, response headers, or response timing |
| 413 | `request_too_large` | no | Inbound webhook body exceeded `gateway.maxMessageBodyBytes` (default 1 MiB). Reduce body size or split the message |
| 413 | `response_too_large` | no | Agent reply exceeded `gateway.maxResponseBodyBytes` (default 900 KiB). Externalize large outputs and reference by URL |
| 502 | `delivery_failed` | no | Initial attempt and 3 retries (1s, 5s, 25s backoff; 4 total) all failed: connection error, non-2xx, or 200 with malformed envelope; see [`delivery_failed`](#async-webhook-response-gateway-managed) for the canonical failure-mode breakdown |
| 504 | `wake_timeout` | no | Hibernated agent did not reach Ready within `wakeTimeout` |
| 504 | `controller_unavailable` | yes | Activator endpoint unreachable; gateway sets `Retry-After: 5` |

`callback_invalid` does not appear above because it is **async-only** by construction: the failure mode is "the configured `callbackUrl` is rejected before dial", which has no sync analogue (sync responses do not use `callbackUrl`). When `callback_invalid` fires in async mode, the underlying agent response (or other error envelope) is stored at the polling endpoint and `callback_invalid` itself is signaled only via a `Warning` event on the AgentChannel — it is not an `error.type` exposed to polling callers. See [Async Webhook Response § callback_invalid](#async-webhook-response-gateway-managed).
