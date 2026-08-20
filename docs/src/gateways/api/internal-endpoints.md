# Internal Endpoints

"Internal" here means Kaalm's own components only: these four endpoints are mTLS-only, each additionally requires a specific peer SAN (the controller's for the activity and channel-health endpoints, the console's for test-chat and spend; Agent and AgentTask client certs are rejected with `403`), and their authentication model is defined in [Internal Endpoint Authentication](../../security/rbac.md#internal-endpoint-authentication).

All four endpoints share a deliberate placement decision: they are served on the cluster listener (port 8443), not the User listener (port 8080). Port 8080 only serves inbound webhook traffic (`/channels/*`) and the async polling fallback (`/v1/channels/responses/*`); mTLS-authenticated internal endpoints live on 8443. This listener split ensures that an Ingress fronting 8080 cannot route untrusted traffic to an endpoint whose authorization assumes a controller-SAN client cert.

## GET /v1/activity

Called by the [AgentReconciler](../../controller/reconcilers.md#agentreconciler) to read per-namespace last-activity timestamps for idle and hibernation transitions. Authenticated via **mTLS**: the caller must present the controller's `kaalm-controller-tls` client cert, verified against `kaalm-ca`, with a SAN that matches the controller Service DNS. There is no bearer-token or HMAC alternative. See [Internal Endpoint Authentication](../../security/rbac.md#internal-endpoint-authentication).

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
| `replicaStartedAt` | timestamp | When this gateway replica started. The controller compares this to each Agent's `status.phaseTransitionTime` to detect post-restart "data is unknown" windows; see [Activity Tracking API](../user/activation-and-activity.md#activity-tracking-api) |
| `agents` | map | Keys are Agent names in the requested namespace; values are per-source last-activity timestamps as observed by this replica |
| `gatewayTraffic` | timestamp or null | Last LLM-gateway request or inbound channel-message delivery this replica observed for the agent. `null` if no traffic since the replica started |
| `heartbeat` | timestamp or null | Last `POST /v1/agent/heartbeat` this replica received from the agent. `null` if none since the replica started |

Both signal sources are always returned. The controller applies the `Agent.spec.lifecycle.activitySource` filter (selecting `gatewayTraffic`, `heartbeat`, or the max of both) **after** merging timestamps across replicas. See [Activity Tracking API](../user/activation-and-activity.md#activity-tracking-api) for the per-Pod-IP fan-out, the per-replica restart-detection logic, and the `tls.Config.ServerName` override required to make per-Pod-IP dialing work against a Service-DNS-scoped SAN.

**Response codes:** `200 OK` on success. `400 Bad Request` if the `namespace` parameter is missing. TLS handshake failures or SAN-authorization mismatches terminate the request at the TLS layer or with `403 Forbidden`. Only agents in the requested namespace are returned.

## GET /v1/channels/health

Called by the `AgentChannelReconciler` to populate `status.conditions[type=PlatformConnected]` on AgentChannel resources. This endpoint is internal and authenticated via **mTLS**: the caller must present the controller's `kaalm-controller-tls` client cert, verified against `kaalm-ca`, with a SAN that matches the controller Service DNS. There is no bearer token or HMAC header. See [Internal Endpoint Authentication](../../security/rbac.md#internal-endpoint-authentication).

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
| `phase` | string | `"Active"` \| `"Degraded"` \| `"Failed"`: mirrors `AgentChannel.status.phase` as seen from the gateway |
| `state` | string | `"success"` \| `"failure"` \| `"empty"`. Computed from the replica's in-window observation list: `success` if any in-window observation succeeded; `failure` if the in-window list is non-empty and contains only failures; `empty` if no in-window observations exist on this replica |
| `reason` | string or null | For `success`, the most recent success's reason (typically `WebhookReady`). For `failure`, the most recent failure's reason: one of `WebhookAuthFailed`, `AgentNotReady`, `DispatchFailed`, `CallbackInvalid`, `CallbackRejected`. `null` when `state: "empty"` |
| `timestamp` | timestamp or null | Time of the most recent in-window observation contributing to `state` (most recent success for `success`; most recent failure for `failure`). `null` when `state: "empty"` |
| `lastError` | string or null | Most recent error message seen by the gateway for this channel within the window; `null` if no error |

The third channel in the example (`new-channel`) shows `state: "empty"`: this replica has no in-window observations for that path. The controller decides whether this means the channel is genuinely silent (`Unknown` with `reason=NoRecentTraffic`) or whether observation is incomplete (preserve existing condition) by comparing `replicaStartedAt` to the window length and checking other replicas. See [Channel Health Tracking](../user/platform-adapters.md#channel-health-tracking) and [AgentChannelReconciler](../../controller/reconcilers.md#agentchannelreconciler) step 4.

**Response codes:** `200 OK` on success. `400 Bad Request` if the `namespace` parameter is missing. TLS handshake failures or SAN-authorization mismatches terminate the request at the TLS layer or with `403 Forbidden`. Only channels whose target Agent is in the requested namespace are returned.

## POST /v1/test-chat

Called by the optional [console](../../console/overview.md) (since the v0.5.0 design) to deliver one operator-authored test message to one agent and return the reply. Authenticated via **mTLS**: the caller must present the console's `kaalm-console-tls` client cert, verified against `kaalm-ca`, with a SAN matching the console Service DNS (`kaalm-console.kaalm-system.svc.cluster.local` or `.svc`). The gateway does not re-authorize the human behind the request: the console performs the `TokenReview` and `SubjectAccessReview` before calling ([Authentication](../../console/overview.md#authentication)), and possession of the console SAN carries that authorization, the same trust class as the controller on the two endpoints above.

**Request:**

```json
{
  "namespace": "team-support",
  "agent": "support-assistant",
  "userId": "priya@example.com",
  "content": "are you alive?"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `namespace` | string | yes | The target Agent's namespace |
| `agent` | string | yes | The target Agent's name |
| `userId` | string | yes | The console-authenticated identity, placed in the delivery envelope's `userId` verbatim so the message is attributable to a person |
| `content` | string | yes | The message text |

The gateway builds a standard [`POST /v1/message` envelope](agent-endpoints.md#request-body-sent-by-the-gateway): a fresh `messageId`, `channelType: "console"`, `channelId: "/console/{namespace}/{agent}"`, the given `userId`, a `sessionId` that is always derived per [Session identity](agent-endpoints.md#session-identity-the-sessionid-derivation), and empty `attachments` and `metadata`. Delivery is the sync channel path end to end: wake-on-demand for a hibernated agent, the agent-delivery retry pipeline, response validation, and the `syncDeliveryDeadline` bound.

**Response:** `200 OK` with the agent's reply envelope verbatim (`content`, plus pass-through `attachments` and `metadata`). Failures use the [User Gateway error mapping](errors.md#user-gateway-error-responses) exactly as sync mode does: `502` for `delivery_failed`; `504` for `wake_timeout`, `controller_unavailable`, and `sync_deadline_exceeded`; `413` for `response_too_large`. `400 Bad Request` for a missing field, and `404 Not Found` when the named Agent does not exist. As on the two endpoints above, a request without a client cert is rejected `401 Unauthorized` and a non-matching SAN `403 Forbidden`.

## GET /v1/spend

Called by the optional [console](../../console/overview.md) (since the v0.5.0 per-workload spend ledger) to read one namespace's current-period spend broken down by workload. Authenticated via **mTLS** with the console SAN, exactly as `POST /v1/test-chat` above. Any single gateway replica answers authoritatively: every replica holds the folded union of its own live counters and every peer's latest published partial, current to within one publish interval. See [Per-Workload Spend](../llm/budgets-and-rate-limits.md#per-workload-spend) for the ledger this reads.

**Request:**

```
GET /v1/spend?namespace=team-support
```

**Response body:**

```json
{
  "providers": {
    "anthropic-shared": {
      "period": "2026-08",
      "workloads": {
        "agent/support-assistant": "1.23",
        "task/fix-42": "0.40",
        "(unattributed)": "0.10"
      }
    }
  }
}
```

| Field | Type | Description |
|---|---|---|
| `providers` | map | Keys are ModelProvider names with spend in the namespace this period; a namespace with no spend returns an empty map |
| `period` | string | The provider's current budget period key |
| `workloads` | map | USD as decimal strings per workload: `agent/{name}` and `task/{name}` from the attested certificate SAN, and `(unattributed)` for gateway-only-tier token callers. The rows sum to the namespace figure in `ModelProvider.status.budgetUsage`, to within one publish and one reconcile interval |

**Response codes:** `200 OK` on success. `400 Bad Request` if the `namespace` parameter is missing. As on every endpoint above, a request without a client cert is rejected `401 Unauthorized` and a non-matching SAN `403 Forbidden`.
