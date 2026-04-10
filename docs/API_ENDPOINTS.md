# Agentry — Gateway HTTP Endpoints

This document defines the HTTP endpoints exposed by the Agentry Gateway and the contract for agent-implemented endpoints. For CRD specifications, see [API_RESOURCES.md](./API_RESOURCES.md).

All gateway-exposed endpoints authenticate requests via **source IP -> Pod resolution** — no API keys or tokens are exchanged. The gateway resolves the caller's Pod and namespace from its Pod informer cache. See [Namespace Identification](./GATEWAY_LLM.md#namespace-identification).

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
  "channelId": "/channels/support-assistant",
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
| `userId` | string | yes | Platform-specific user identifier |
| `sessionId` | string | no | Present when `AgentChannel.spec.session.enabled: true`. Deterministic: derived from `UUIDv5(channelId + ":" + userId)`, identical across replicas and gateway restarts. Session expiry is the agent's responsibility. |
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

**Callback delivery** (gateway POSTs to `spec.webhook.callbackUrl`):

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelId": "/channels/support-assistant",
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
  "channelId": "/channels/support-assistant",
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
  "channelId": "/channels/support-assistant",
  "error": {
    "type": "wake_timeout",
    "message": "Agent did not become ready within wakeTimeout (120s)",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:12:00Z"
}
```

Error payloads are delivered to `callbackUrl` with the same 3-retry / 1s-5s-25s backoff as successful responses. If no `callbackUrl` is configured, errors are stored at the polling endpoint under the original `requestId` and expire after 1 hour.

**`GET /v1/channels/{channelId}/responses/{requestId}`** (polling fallback):

Returns the agent's response or error payload if available. This endpoint is authenticated via the same mechanism as the webhook itself (`AgentChannel.spec.webhook.auth`).

| Status | Meaning |
|---|---|
| 200 | Response or error payload available; body contains the callback payload above |
| 202 | Request is still being processed |
| 404 | Unknown requestId or response expired |

Stored responses are retained for 1 hour, after which they are evicted. This is a polling fallback, not a durable queue — webhook callers that need guaranteed delivery should configure `callbackUrl`.

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
| 502 | `provider_error` | Upstream provider returned an error after exhausting fallback chain |
| 503 | `budget_exhausted` | Budget blocked per policy; `retryable: false` |
| 503 | `provider_unavailable` | All providers (primary + fallback) unreachable |
| 504 | `provider_timeout` | Upstream provider timed out after exhausting fallback chain |

The `error.retryable` field indicates whether the agent should retry the request. Rate-limited requests are retryable; budget-exhausted and access-denied are not.
