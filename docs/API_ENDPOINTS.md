# Agentry — Gateway HTTP Endpoints

This document defines the HTTP endpoints exposed by the Agentry Gateway and the contract for agent-implemented endpoints. For CRD specifications, see [API_RESOURCES.md](./API_RESOURCES.md).

The LLM proxy endpoints accept either **mTLS** (Agentry-managed Agent/AgentTask Pods) or a **`TokenReview`-validated ServiceAccount bearer token** (gateway-only tier), with a **source-IP → Pod cross-check** in both modes. The agent-only internal endpoints below — `POST /v1/task/complete` and `POST /v1/agent/heartbeat` — are **mTLS-only**: there is no SA-bearer alternative, and gateway-only-tier workloads (which have no AgentTask or Agent identity) cannot reach them. See [Namespace Identification](./GATEWAY_LLM.md#namespace-identification) for the full flow and [Agent→Gateway Authentication](./SECURITY.md#agentgateway-authentication) for the threat-model analysis.

---

## Reserved Gateway Paths

The following path prefixes are reserved for gateway-internal use and must not be used as `AgentChannel.spec.webhook.path` values. These paths conflict with gateway-internal endpoints — `/v1/` is served on the LLM Gateway listener at `:8443` for gateway-internal calls and on the User Gateway listener at `:8080` for the async polling endpoint, and is otherwise reserved on `:8080` against webhook path collisions:

- `/v1/` — all current and future gateway-internal endpoints (LLM proxy paths, task completion, heartbeat, async polling, channel health, activity). The controller's `POST /v1/activate/{namespace}/{agentName}` lives on the controller Service (port 9443), not the gateway, and is therefore unreachable from a webhook path regardless of this rule.

AgentChannels whose `spec.webhook.path` begins with `/v1/` are rejected at reconcile time with `Ready=False, reason=ReservedPath`. The recommended developer convention is to use `/channels/` as a prefix (e.g., `/channels/team-support/support-assistant`).

---

## LLM Proxy Endpoints

The LLM proxy accepts agent requests on the upstream provider's native API paths and forwards them to the resolved ModelProvider. Recognized path patterns:

- `/v1/messages` — Anthropic format
- `/v1/chat/completions` — OpenAI / OpenAI-compatible format (also vLLM, Ollama, LiteLLM)
- Provider-specific paths — see [GATEWAY_LLM.md § Request Format Detection](./GATEWAY_LLM.md#request-format-detection) for the full mapping

Request and response bodies are passthrough to the upstream provider's native format — Agentry adds no envelope of its own. The gateway injects the provider API key, strips the `provider/` prefix from the model name, and relays the response (including SSE streams transparently). Errors returned by the gateway itself (budget exhaustion, rate limit, fallback exhaustion) use the structured envelope documented in [LLM Gateway Error Responses](#llm-gateway-error-responses) below.

Auth, namespace identification, provider routing, budget enforcement, fallback, and streaming behavior are documented in [GATEWAY_LLM.md](./GATEWAY_LLM.md). The per-path auth profile for these endpoints is consolidated in [ARCHITECTURE.md § The Agentry Gateway](./ARCHITECTURE.md#the-agentry-gateway) (`:8443` listener auth profile table).

---

## `POST /v1/task/complete` (AgentTask only)

Called by the agent container to report task completion. The gateway updates the pre-existing `{taskName}-completion` ConfigMap in the task's namespace — created by the [AgentTaskReconciler](./CONTROLLER_RECONCILERS.md#agenttaskreconciler) at task provisioning time, owned by the AgentTask for cascade deletion — using the `update, patch`-only RBAC scoped to that exact ConfigMap name (see [Gateway ServiceAccount permissions](./SECURITY.md#gateway-serviceaccount-permissions)). The AgentTaskReconciler watches the ConfigMap for changes to drive the Completing transition — see [AgentTask State Machine](./CONTROLLER_LIFECYCLE.md#agenttask).

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
| `artifacts` | map[string]string | no | Key-value pairs matching the names declared in `spec.artifacts`. The reconciler validates that all declared artifact names are present. |

**Response:** `200 OK` with empty body on success. `403 Forbidden` if the calling Pod is not associated with an AgentTask. `413 Payload Too Large` if any single artifact value exceeds 4 KiB or the sum of all value bytes exceeds 32 KiB. Sizes are measured in UTF-8 bytes against the value strings only; key bytes are not counted (keys are bounded by Kubernetes ConfigMap key naming rules). Large artifacts should be stored externally and referenced by URL in the value.

---

## `POST /v1/agent/heartbeat` (Agent only)

Called by the agent container to signal liveness for idle detection. Only meaningful when [`spec.lifecycle.activitySource`](./API_RESOURCES.md#agent) is `agentHeartbeat` or `both`. The gateway updates the agent's last-activity timestamp in its in-memory activity store. See [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) for how the controller uses activity data.

**Request body:** empty or `{}`.

**Response:** `200 OK` with empty body. `403 Forbidden` if the calling Pod is not associated with an Agent.

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
| `messageId` | string (UUID) | yes | Unique identifier for this message, generated by the gateway. Agents with `hibernationEnabled: true` MUST deduplicate on this value — the gateway's agent-delivery retry pipeline (up to 3 retries with 1s/5s/25s backoff — see [`delivery_failed`](#async-webhook-response-gateway-managed)) reuses the same `messageId` across attempts, and an earlier attempt may have reached the agent even when the gateway's read of the response failed. Caller retries (e.g., a webhook caller resending after a sync-mode 504) are delivered as a fresh message with a new `messageId`. See [RUNTIME_CONTRACT.md item 7](./RUNTIME_CONTRACT.md). |
| `channelType` | string | yes | Platform type: `"webhook"` in v1 (Discord, WhatsApp in v1.1) |
| `channelId` | string | yes | Platform-specific channel identifier |
| `userId` | string | yes | Platform-specific user identifier. Extracted per `AgentChannel.spec.webhook.userId` config (`fromHeader` or `fromBody`); falls back to the configured `fallback` value (or empty string if unconfigured). When `session.enabled: true` and userId is empty, all unattributed requests share a session. |
| `sessionId` | string | no | Present when `AgentChannel.spec.session.enabled: true`. Computed as `UUIDv5(namespace: f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d, name: channelId + ":" + userId)` — identical across replicas and gateway restarts. Session expiry is the agent's responsibility. |
| `content` | string | yes | The user's message text |
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

**Agent → gateway:** `200 OK` with the response envelope is expected. Any other status (including 5xx and connection errors) is a `delivery_failed` signal that feeds the gateway's agent-delivery retry pipeline (same schedule in both sync and async modes — see [`delivery_failed`](#async-webhook-response-gateway-managed)); on retry exhaustion, sync callers receive `502` with the `delivery_failed` envelope and async callers receive the same payload via callback or polling. Failures are recorded in AgentChannel status conditions in either case.

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

The gateway retries callback delivery up to 3 times with exponential backoff (1s, 5s, 25s — 4 attempts total over ~31s). If no `callbackUrl` is configured on the AgentChannel, or all retries fail, the response is stored at the polling endpoint under the original `requestId` and expires after 1 hour — same mechanism as the error-payload storage described later in this section.

**Callback authentication** — every callback POST is signed using the AgentChannel's `spec.webhook.callbackAuth` (required by [API_RESOURCES.md § Cross-Resource Validation rule 25](./API_RESOURCES.md#cross-resource-validation) whenever `callbackUrl` is set). The signing contract mirrors the [polling endpoint's caller-auth contract](#polling-fallback) below — same auth types, same `X-Agentry-Timestamp` header, with a body-hash component added because callbacks have a body where polls do not. The signing material is loaded from the Secret referenced by `callbackAuth.secretRef` (or `callbackAuth.hmac.secretRef`), held by the gateway via the per-channel scoped Role created by [AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler):

- **`callbackAuth.type: bearer`** — gateway sends `Authorization: Bearer <secret>` on the callback POST.
- **`callbackAuth.type: hmac`** — gateway computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}\n{sha256(body)}"` (unix seconds, no trailing newline; body hash is the lowercase hex sha256 of the raw POST body bytes). The hex-encoded digest goes in the configured `callbackAuth.hmac.header`; `timestamp` goes in `X-Agentry-Timestamp`. Receivers should reject timestamps with skew greater than 300s against their own wall clock.

This contract applies uniformly to success payloads (above) and to every error payload delivered via callback (`delivery_failed`, `wake_timeout`, `controller_unavailable`, `response_too_large`) — error payloads are signed identically so a forged POST cannot impersonate a delivery error. `callback_invalid` is deliberately excluded: the URL itself is the failure mode, so the payload has no callback delivery path and is delivered exclusively via the polling endpoint (see [`callback_invalid` description below](#async-webhook-response-gateway-managed)).

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

`delivery_failed` is returned when the initial attempt and all 3 retries to `POST /v1/message` to the agent's Service fail (connection error, non-200 response). The retry schedule is 1s, 5s, 25s backoff between attempts (4 total POSTs over ~31s); this pipeline runs in both sync and async modes (see [GATEWAY_USER.md § Activator](./GATEWAY_USER.md#activator)) and is independent of the callback-delivery retry pipeline that uses the same numbers by coincidence. `wake_timeout` is returned when the agent is `Hibernated` and fails to become Ready within `wakeTimeout`:

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

`response_too_large` is returned when the agent's response body exceeds `gateway.maxAsyncResponseBytes` (default 900 KiB; see [GATEWAY_USER.md § Request Flow](./GATEWAY_USER.md#user-gateway--request-flow) step 6a for the size cap rationale). The cap exists for two reasons: async responses are persisted to ConfigMaps in `agentry-system` (Kubernetes object cap near 1 MiB), **and** all webhook responses — sync and async alike — are buffered in gateway memory before forwarding, so an unbounded agent reply could OOM the gateway. The same 900 KiB ceiling therefore applies in both modes; the config knob name (`maxAsyncResponseBytes`) reflects when the cap was first introduced for the ConfigMap-storage path and is preserved for backwards compatibility. Agents that need to return large outputs should externalize them and reference by URL:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "response_too_large",
    "message": "Agent response body exceeded gateway.maxAsyncResponseBytes (900 KiB); externalize large outputs and reference by URL",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:13:14Z"
}
```

`callback_invalid` is returned when the configured `callbackUrl` host fails re-resolution against the deny ranges (loopback, link-local, RFC1918, unique-local IPv6, cloud metadata) or the configured `gateway.callbackUrl.allowlist` immediately before a delivery attempt — defeating DNS rebinding between reconcile-time validation and delivery (see [AgentChannel rule 22](./API_RESOURCES.md#cross-resource-validation)). Because the URL itself is rejected, this payload is **delivered only via the polling endpoint** — there is no callback target to POST to. The AgentChannel additionally receives a `Warning` event with `reason=CallbackInvalid`:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "callback_invalid",
    "message": "callbackUrl host failed re-resolution against blocked IP ranges; delivery skipped",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:13:42Z"
}
```

Error payloads are delivered to `callbackUrl` with the same 3-retry / 1s-5s-25s backoff as successful responses. If no `callbackUrl` is configured, all 4 callback attempts fail (initial + 3 retries), or delivery is rejected as `callback_invalid` — errors are stored at the polling endpoint under the original `requestId` and expire after 1 hour. This mirrors the success-path retry-exhaustion behavior at the top of this section.

### Polling Fallback

**`GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}`**

Served over HTTPS on the User Gateway listener (port 8080, TLS using `agentry-gateway-tls` — same certificate used by the LLM listener). External callers reach it through the cluster Ingress that fronts port 8080; see [GATEWAY_USER.md § TLS and Ingress](./GATEWAY_USER.md#tls-and-ingress).

Returns the agent's response or error payload if available. The `channelPath` query parameter is the URL-encoded webhook path of the originating AgentChannel — exactly the `channelPath` value the caller received in the 202 response body. The gateway uses it to look up the AgentChannel's auth configuration and authenticate the request. Callers must preserve it verbatim from the 202 response; it should not be constructed independently.

Poll requests carry no body, so the auth contract differs slightly from the original webhook:

- **`auth.type: bearer`** — the poll request presents the same bearer token in `Authorization: Bearer …` as the inbound webhook. The token is read from the Secret referenced by `AgentChannel.spec.webhook.auth.secretRef` (`name`, `key`); there is no inline token field on the AgentChannel spec.
- **`auth.type: hmac`** — the poll request computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}"` (unix seconds, no trailing newline), sends the hex-encoded digest in the configured `header` (same header name as the original webhook), and presents the timestamp in a dedicated `X-Agentry-Timestamp` header. The HMAC input is **not** the request body (poll GETs have none). Requests with clock skew greater than 300s against the gateway's wall clock are rejected with `401 Unauthorized`.

**Channel-match assertion on response retrieval:** after authenticating the caller against the AgentChannel identified by `channelPath`, and *before* returning the stored payload, the gateway asserts that the `agentry-async-{requestId}` ConfigMap's `agentry.io/channel-namespace` and `agentry.io/channel-name` labels match that same AgentChannel. This prevents a caller authenticated for channel A from retrieving a response that was stored by channel B — `requestId`s are UUIDs but are not secrets, so without this check an attacker holding channel A's credentials plus any channel B `requestId` could read B's response. On the wire a mismatch is indistinguishable from "unknown `requestId`" — both return `404 Not Found` — so the endpoint does not leak the existence of cross-channel responses to a credentialed attacker. Channel mismatches are logged at the gateway with `reason=ChannelMismatch` for operator debugging.

`401 Unauthorized` is returned on any auth failure (missing or malformed credentials, signature mismatch, clock skew) **and on a well-formed but unregistered `channelPath`** — the gateway treats an unknown channel as an auth failure rather than a 404 to avoid revealing which webhook paths exist (and, by extension, which tenant namespaces are hosted, since paths are `/channels/{namespace}/...`-prefixed). This mirrors the channel-match assertion above and the [SECURITY.md threat-model row on cross-channel response retrieval](./SECURITY.md#threat-model). When the credentials authenticate correctly against `channelPath` but the stored `requestId` was originated by a different channel, the response is `404 Not Found` — same code as "unknown `requestId`".

Any gateway replica accepts this request. The replica reads the `agentry-async-{requestId}` ConfigMap in `agentry-system`: the ConfigMap is created as an empty placeholder when the originating replica returns `202` to the inbound webhook (so the polling record is queryable as soon as the caller has the `requestId`) and is patched with the response payload (or an error envelope) when the agent reply is ready. A `200` is returned only when the payload field is present and the channel-match assertion above passes; a missing payload field returns `202`; an absent ConfigMap returns `404`.

| Status | Meaning |
|---|---|
| 200 | Response or error payload available; body contains the callback payload above |
| 202 | Request accepted; agent response not yet available |
| 400 | Missing or malformed `channelPath` query parameter |
| 401 | Auth failed (missing or malformed credentials, signature mismatch, clock skew > 300s, or `channelPath` not registered to any AgentChannel) |
| 404 | Unknown `requestId`, response expired (1-hour TTL), **or** stored `requestId` originated by a different channel than `channelPath` (channel-match failure — see assertion above) |

Stored responses are retained for 1 hour in the ConfigMap, after which they are pruned by the [AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler). Neither path is a durable queue: callback delivery is best-effort (3 retries with 1s/5s/25s backoff), and the polling endpoint is the receiver-driven fallback within the same 1-hour TTL — receivers that miss the callback retries can still recover the response by polling on timeout, but past the TTL the response is gone.

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
  "startedAt": "2026-04-05T06:00:00Z",
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
| `startedAt` | timestamp | When this gateway replica started. The controller compares this to each Agent's `status.phaseTransitionTime` to detect post-restart "data is unknown" windows — see [GATEWAY_USER.md § Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api) |
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
| 403 | `access_denied` | Namespace not in `allowedNamespaces`, model not in Agent's providers |
| 429 | `rate_limited` | Per-namespace rate limit exceeded; includes `Retry-After` header |
| 429 | `budget_exhausted` | Budget blocked per policy; includes `Retry-After` header set to the start of the next budget period |
| 502 | `provider_error` | Upstream provider returned an error after exhausting fallback chain |
| 503 | `provider_unavailable` | All providers (primary + fallback) unreachable |
| 504 | `provider_timeout` | Upstream provider timed out after exhausting fallback chain |

The `error.retryable` field indicates whether the agent should retry the request. Rate-limited requests are retryable (short backoff). Budget-exhausted requests include a `Retry-After` header indicating when the next budget period starts, but `retryable` is `false` — the agent should not retry before that time. Access-denied requests are not retryable.

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
| 413 | `response_too_large` | no | Agent reply exceeded `gateway.maxAsyncResponseBytes` (default 900 KiB). Externalize large outputs and reference by URL |
| 502 | `delivery_failed` | no | Agent unreachable after 3 retries (1s, 5s, 25s backoff; 4 attempts total) |
| 504 | `wake_timeout` | no | Hibernated agent did not reach Ready within `wakeTimeout` |
| 504 | `controller_unavailable` | yes | Activator endpoint unreachable; gateway sets `Retry-After: 5` |

`callback_invalid` does not appear above because it is **async-only** by construction: the failure mode is "the configured `callbackUrl` is rejected before dial", which has no sync analogue (sync responses do not use `callbackUrl`). The async `callback_invalid` payload is delivered exclusively via the polling endpoint — see [Async Webhook Response § callback_invalid](#async-webhook-response-gateway-managed).
