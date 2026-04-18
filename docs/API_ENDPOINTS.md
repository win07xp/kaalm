# Agentry — Gateway HTTP Endpoints

This document defines the HTTP endpoints exposed by the Agentry Gateway and the contract for agent-implemented endpoints. For CRD specifications, see [API_RESOURCES.md](./API_RESOURCES.md).

All gateway-exposed endpoints authenticate requests via **source IP -> Pod resolution** — no API keys or tokens are exchanged. The gateway resolves the caller's Pod and namespace from its Pod informer cache. See [Namespace Identification](./GATEWAY_LLM.md#namespace-identification).

---

## Reserved Gateway Paths

The following path prefixes are reserved for gateway-internal use and must not be used as `AgentChannel.spec.webhook.path` values. These paths conflict with gateway-internal endpoints (served on the LLM Gateway listener at port 8443 for agent-initiated calls, and reserved on the User Gateway listener to prevent webhook path collisions):

- `/v1/` — all current and future gateway-internal endpoints (task completion, heartbeat, async polling, channel health, activator)

AgentChannels whose `spec.webhook.path` begins with `/v1/` are rejected at reconcile time with `Ready=False, reason=ReservedPath`. The recommended developer convention is to use `/channels/` as a prefix (e.g., `/channels/team-support/support-assistant`).

---

## `POST /v1/task/complete` (AgentTask only)

Called by the agent container to report task completion. The gateway writes the payload to a ConfigMap named `{taskName}-completion` in the task's namespace (owned by the AgentTask for cascade deletion). The AgentTaskReconciler watches for this ConfigMap to drive the Completing transition — see [AgentTask State Machine](./CONTROLLER_LIFECYCLE.md#agenttask).

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

**Response:** `200 OK` with empty body on success. `400 Bad Request` if the calling Pod is not associated with an AgentTask. `413 Payload Too Large` if any single artifact value exceeds 4 KiB or total artifacts exceed 32 KiB (large artifacts should be stored externally and referenced by URL in the value).

---

## `POST /v1/agent/heartbeat` (Agent only)

Called by the agent container to signal liveness for idle detection. Only meaningful when `spec.lifecycle.activitySource` is `agentHeartbeat` or `both`. The gateway updates the agent's last-activity timestamp in its in-memory activity store. See [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) for how the controller uses activity data.

**Request body:** empty or `{}`.

**Response:** `200 OK` with empty body. `400 Bad Request` if the calling Pod is not associated with an Agent.

Heartbeat frequency is the agent's choice. A reasonable default is every 30-60 seconds. The gateway coalesces rapid heartbeats in memory — no etcd or API server writes occur per heartbeat.

---

## Manual Wake Annotation

Agents in the `Hibernated` phase can be manually woken by applying the annotation:

```
kubectl annotate agent <name> agentry.io/wake=true
```

The AgentReconciler watches for this annotation. When observed on a `Hibernated` Agent, the reconciler transitions the Agent to `Resuming`, recreates the Pod, and removes the annotation after processing. This provides an escape hatch when no AgentChannel is configured or for operational use cases (e.g., pre-warming an agent before business hours).

---

## `POST /v1/message` (Agent only — agent-implemented)

This endpoint is **implemented by the agent container**, not by the gateway. The User Gateway calls it to deliver normalized channel messages — see [User Gateway Request Flow](./GATEWAY_USER.md#user-gateway--request-flow). Agents that use AgentChannel must expose this endpoint on `$AGENTRY_HEALTH_PORT` (default 8080).

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
| `messageId` | string (UUID) | yes | Unique identifier for this message, for deduplication |
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

**Response codes:** `200 OK` with the response envelope. In sync mode, the gateway returns the response body as the webhook HTTP response. Non-200 responses are treated as delivery failures and recorded in AgentChannel status conditions.

---

## Async Webhook Response (gateway-managed)

When an AgentChannel has `spec.webhook.responseMode: async`, the gateway handles the asynchronous response flow. The agent's implementation is unchanged — it still receives `POST /v1/message` and returns a response envelope. The async behavior is entirely gateway-side.

**Webhook caller receives (immediate 202 response):**

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "status": "accepted",
  "message": "Message accepted for processing"
}
```

The `requestId` is an **opaque string** — callers must treat it as an identifier to use in poll requests and must not attempt to parse, construct, or reverse-engineer its structure. Internally it encodes routing affinity so the gateway can consistently route poll requests to the correct in-memory store across replicas (see [GATEWAY_USER.md — Async shard routing](./GATEWAY_USER.md#user-gateway--request-flow)).

**Callback delivery** (gateway POSTs to `spec.webhook.callbackUrl`):

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelId": "/channels/team-support/support-assistant",
  "response": {
    "content": "I've analyzed the issue and opened a PR with the fix.",
    "attachments": [],
    "metadata": {}
  },
  "completedAt": "2026-04-05T12:10:42Z"
}
```

The gateway retries callback delivery up to 3 times with exponential backoff (1s, 5s, 25s). If all retries fail, the response is stored for polling retrieval.

**Error callback payload** (sent to `callbackUrl` or stored at the polling endpoint on failure):

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelId": "/channels/team-support/support-assistant",
  "error": {
    "type": "delivery_failed",
    "message": "Failed to deliver message to agent after 3 attempts",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:11:07Z"
}
```

`delivery_failed` is returned when all 3 attempts to `POST /v1/message` to the agent's Service fail (connection error, non-200 response). `wake_timeout` is returned when the agent is `Hibernated` and fails to become Ready within `wakeTimeout`:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelId": "/channels/team-support/support-assistant",
  "error": {
    "type": "wake_timeout",
    "message": "Agent did not become ready within wakeTimeout (120s)",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:12:00Z"
}
```

Error payloads are delivered to `callbackUrl` with the same 3-retry / 1s-5s-25s backoff as successful responses. If no `callbackUrl` is configured, errors are stored at the polling endpoint under the original `requestId` and expire after 1 hour.

**`GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}`** (polling fallback):

Returns the agent's response or error payload if available. The `channelPath` query parameter is the URL-encoded webhook path of the originating AgentChannel (i.e., the value of `channelId` from the 202 response body). The gateway uses it to look up the AgentChannel's auth configuration and authenticate the request via the same mechanism as the original webhook (`AgentChannel.spec.webhook.auth`). Callers must preserve the `channelId` value from the 202 response and pass it as `channelPath` on poll — it should not be constructed independently.

Any gateway replica accepts this request. The receiving replica extracts the shard index from the opaque `requestId`, routes internally to the owning replica if needed, and falls back to the `agentry-async-{requestId}` ConfigMap in `agentry-system` if the owning replica is unreachable or the in-memory response is unavailable (e.g., after a scale event). Callers always receive a consistent response regardless of which gateway replica they reach — replica topology is invisible.

| Status | Meaning |
|---|---|
| 200 | Response or error payload available; body contains the callback payload above |
| 202 | Request is still being processed |
| 404 | Unknown requestId, response expired, or owning replica crashed before writing the durability ConfigMap |

Stored responses are retained for 1 hour (in-memory on the owning replica and in the durability ConfigMap), after which they are evicted. This is a polling fallback, not a durable queue — webhook callers that need guaranteed delivery should configure `callbackUrl`.

---

## `GET /v1/channels/health` (internal — controller use only)

Called by the `AgentChannelReconciler` to populate `status.conditions[type=PlatformConnected]` on AgentChannel resources. This endpoint is internal and authenticated via the same HMAC scheme as the activator and activity endpoints (shared key from `agentry-activator-key` Secret) — see [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api).

**Request:**

```
GET /v1/channels/health?namespace=team-support
Authorization: Bearer <HMAC(timestamp:namespace)>
X-Agentry-Timestamp: <unix-timestamp>
```

**Response body:**

```json
{
  "channels": {
    "/channels/team-support/support-assistant": {
      "phase": "Active",
      "platformConnected": true,
      "lastError": null
    },
    "/channels/team-support/personal-assistant": {
      "phase": "Degraded",
      "platformConnected": false,
      "lastError": "webhook auth validation failed: 401 Unauthorized"
    }
  }
}
```

| Field | Type | Description |
|---|---|---|
| `channels` | map | Keys are webhook paths as registered in the gateway; values are health records for each channel |
| `phase` | string | `"Active"` \| `"Degraded"` \| `"Failed"` — mirrors `AgentChannel.status.phase` as seen from the gateway |
| `platformConnected` | boolean | `true` if the gateway considers the channel endpoint healthy and ready to receive messages |
| `lastError` | string or null | Most recent error seen by the gateway for this channel; null if no error |

**Response codes:** `200 OK` on success. `400 Bad Request` if the `namespace` parameter is missing. `401 Unauthorized` if the HMAC signature is invalid or the timestamp is stale (>30s). Only channels whose target Agent is in the requested namespace are returned.

---

## LLM Gateway Error Responses

When the LLM Gateway cannot fulfill a request, it returns a structured error response so agents can handle failures programmatically. For the full LLM Gateway request flow including budget checks and fallback that produce these errors, see [GATEWAY_LLM.md](./GATEWAY_LLM.md#llm-gateway--request-flow).

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
| 413 | `payload_too_large` | Artifact payload exceeds size limits (task completion only) |
| 429 | `rate_limited` | Per-namespace rate limit exceeded; includes `Retry-After` header |
| 429 | `budget_exhausted` | Budget blocked per policy; includes `Retry-After` header set to the start of the next budget period |
| 502 | `provider_error` | Upstream provider returned an error after exhausting fallback chain |
| 503 | `provider_unavailable` | All providers (primary + fallback) unreachable |
| 504 | `provider_timeout` | Upstream provider timed out after exhausting fallback chain |

The `error.retryable` field indicates whether the agent should retry the request. Rate-limited requests are retryable (short backoff). Budget-exhausted requests include a `Retry-After` header indicating when the next budget period starts, but `retryable` is `false` — the agent should not retry before that time. Access-denied requests are not retryable.
