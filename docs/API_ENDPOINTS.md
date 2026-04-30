# Agentry — Gateway HTTP Endpoints

This document defines the HTTP endpoints exposed by the Agentry Gateway and the contract for agent-implemented endpoints. For CRD specifications, see [API_RESOURCES.md](./API_RESOURCES.md).

Agent→gateway endpoints authenticate via **mTLS** (Agentry-managed Agent/AgentTask Pods) or a **`TokenReview`-validated ServiceAccount bearer token** (gateway-only tier). A **source-IP → Pod cross-check** runs in both modes as defense in depth. See [Namespace Identification](./GATEWAY_LLM.md#namespace-identification) for the full flow and [Agent→Gateway Authentication](./SECURITY.md#agentgateway-authentication) for the threat-model analysis.

---

## Reserved Gateway Paths

The following path prefixes are reserved for gateway-internal use and must not be used as `AgentChannel.spec.webhook.path` values. These paths conflict with gateway-internal endpoints (served on the LLM Gateway listener at port 8443 for agent-initiated calls, and reserved on the User Gateway listener to prevent webhook path collisions):

- `/v1/` — all current and future gateway-internal endpoints (task completion, heartbeat, async polling, channel health, activity). The controller's `POST /v1/activate/{namespace}/{agentName}` lives on the controller Service (port 9443), not the gateway, and is therefore unreachable from a webhook path regardless of this rule.

AgentChannels whose `spec.webhook.path` begins with `/v1/` are rejected at reconcile time with `Ready=False, reason=ReservedPath`. The recommended developer convention is to use `/channels/` as a prefix (e.g., `/channels/team-support/support-assistant`).

---

## `POST /v1/task/complete` (AgentTask only)

Called by the agent container to report task completion. The gateway updates the pre-existing `{taskName}-completion` ConfigMap in the task's namespace — created by the AgentTaskReconciler at task provisioning time, owned by the AgentTask for cascade deletion — using the `update, patch`-only RBAC scoped to that exact ConfigMap name (see [Gateway ServiceAccount permissions](./SECURITY.md#gateway-serviceaccount-permissions)). The AgentTaskReconciler watches the ConfigMap for changes to drive the Completing transition — see [AgentTask State Machine](./CONTROLLER_LIFECYCLE.md#agenttask).

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

**Response:** `200 OK` with empty body on success. `403 Forbidden` if the calling Pod is not associated with an AgentTask. `413 Payload Too Large` if any single artifact value exceeds 4 KiB or total artifacts exceed 32 KiB (large artifacts should be stored externally and referenced by URL in the value).

---

## `POST /v1/agent/heartbeat` (Agent only)

Called by the agent container to signal liveness for idle detection. Only meaningful when `spec.lifecycle.activitySource` is `agentHeartbeat` or `both`. The gateway updates the agent's last-activity timestamp in its in-memory activity store. See [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) for how the controller uses activity data.

**Request body:** empty or `{}`.

**Response:** `200 OK` with empty body. `403 Forbidden` if the calling Pod is not associated with an Agent.

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
  "channelId": "/channels/team-support/support-assistant",
  "status": "accepted",
  "message": "Message accepted for processing"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `requestId` | string (UUID) | yes | Opaque identifier for this async request. Callers must use it as-is in poll requests and must not parse or construct it independently. |
| `channelId` | string | yes | The webhook path of the originating AgentChannel. Callers must preserve this value and pass it (URL-encoded) as the `channelPath` query parameter on poll requests — see [polling fallback](#async-webhook-response-gateway-managed) below. |
| `status` | string | yes | Always `"accepted"` for the immediate 202. |
| `message` | string | no | Human-readable acknowledgement. |

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

`controller_unavailable` is returned when a message arrives for a `Hibernated` agent and the gateway cannot reach the controller's activator endpoint (connection error, 5xx after retry, or mTLS handshake / SAN authorization failure). The wake could not even be attempted, so the agent remains `Hibernated`:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelId": "/channels/team-support/support-assistant",
  "error": {
    "type": "controller_unavailable",
    "message": "Controller activator endpoint unreachable; wake could not be triggered",
    "retryable": true
  },
  "failedAt": "2026-04-05T12:12:00Z"
}
```

Unlike `wake_timeout` (agent was asked to wake but did not become ready in time), `controller_unavailable` indicates the wake request itself never reached the controller — retrying the original webhook is expected to succeed once the controller recovers, hence `retryable: true`. The sync-webhook equivalent is HTTP `504 Gateway Timeout` with the same error body; see [GATEWAY_USER.md § Failure Modes](./GATEWAY_USER.md#failure-modes).

Error payloads are delivered to `callbackUrl` with the same 3-retry / 1s-5s-25s backoff as successful responses. If no `callbackUrl` is configured, errors are stored at the polling endpoint under the original `requestId` and expire after 1 hour.

**`GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}`** (polling fallback):

Served over HTTPS on the User Gateway listener (port 8080, TLS using `agentry-gateway-tls` — same certificate used by the LLM listener). External callers reach it through the cluster Ingress that fronts port 8080; see [GATEWAY_USER.md § TLS and Ingress](./GATEWAY_USER.md#tls-and-ingress).

Returns the agent's response or error payload if available. The `channelPath` query parameter is the URL-encoded webhook path of the originating AgentChannel (i.e., the value of `channelId` from the 202 response body). The gateway uses it to look up the AgentChannel's auth configuration and authenticate the request. Callers must preserve the `channelId` value from the 202 response and pass it as `channelPath` on poll — it should not be constructed independently.

Poll requests carry no body, so the auth contract differs slightly from the original webhook:

- **`auth.type: bearer`** — the poll request presents the same bearer token in `Authorization: Bearer …` as the inbound webhook. The token is read from the Secret referenced by `AgentChannel.spec.webhook.auth.secretRef` (`name`, `key`); there is no inline token field on the AgentChannel spec.
- **`auth.type: hmac`** — the poll request computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}"` (unix seconds, no trailing newline), sends the hex-encoded digest in the configured `header` (same header name as the original webhook), and presents the timestamp in a dedicated `X-Agentry-Timestamp` header. The HMAC input is **not** the request body (poll GETs have none). Requests with clock skew greater than 300s against the gateway's wall clock are rejected with `401 Unauthorized`.

**Channel-match assertion on response retrieval:** after authenticating the caller against the AgentChannel identified by `channelPath`, and *before* returning the stored payload, the gateway asserts that the `agentry-async-{requestId}` ConfigMap's `agentry.io/channel-namespace` and `agentry.io/channel-name` labels match that same AgentChannel. This prevents a caller authenticated for channel A from retrieving a response that was stored by channel B — `requestId`s are UUIDs but are not secrets, so without this check an attacker holding channel A's credentials plus any channel B `requestId` could read B's response. A mismatch returns `403 Forbidden` with no body.

`401 Unauthorized` is returned on any auth failure; `403 Forbidden` is returned if the presented credentials are valid for a different channel than `channelPath`, **or** if the `requestId`'s stored response was originated by a different channel than `channelPath`.

Any gateway replica accepts this request. The replica reads the response from the `agentry-async-{requestId}` ConfigMap in `agentry-system`.

| Status | Meaning |
|---|---|
| 200 | Response or error payload available; body contains the callback payload above |
| 202 | Request is still being processed |
| 404 | Unknown requestId or response expired (1-hour TTL) |

Stored responses are retained for 1 hour in the ConfigMap, after which they are pruned by the AgentChannelReconciler. This is a polling fallback, not a durable queue — webhook callers that need guaranteed delivery should configure `callbackUrl`.

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
