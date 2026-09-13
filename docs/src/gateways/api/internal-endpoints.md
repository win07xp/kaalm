# Internal endpoints

Internal means Kaalm's own components only. These four endpoints are mTLS-only, and each requires a specific peer SAN: the controller's for the activity and channel-health endpoints, the console's for test-chat and spend. Agent and AgentTask certificates are rejected with `403 access_denied`, and a request without a certificate with `401 unauthorized` ([Internal endpoint authentication](../../security/rbac.md#internal-endpoint-authentication)). The gateway does not run the source-IP cross-check on these paths, and the two `GET` endpoints accept any HTTP method; issue #237 tracks both.

All four are served on the cluster listener, `:8443`, never on the Ingress-fronted user listener ([Why two listeners](../overview.md#why-two-listeners-and-a-separate-health-port)). The controller's own internal endpoint, the activator the gateway calls to wake a hibernated Agent, is on the controller Service and is specified on [The activator](../user/activation-and-activity.md#the-activator).

## GET /v1/activity

The [AgentReconciler](../../controller/reconcilers.md#agentreconciler) reads per-namespace last-activity timestamps here for idle and hibernation transitions. The caller presents `kaalm-controller-tls`, whose SAN is the controller Service DNS.

**Request:**

```
GET /v1/activity?namespace=team-support
```

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
| `replicaStartedAt` | timestamp | When this gateway replica started. The controller uses it when no replica has a record for an Agent: a missing record counts as silence only once some replica has been up for at least `idleTimeout` ([Activity tracking API](../user/activation-and-activity.md#activity-tracking-api)) |
| `agents` | map | Keys are Agent names in the requested namespace; values are per-source last-activity timestamps as observed by this replica |
| `gatewayTraffic` | timestamp or null | The last LLM proxy request or delivered channel message this replica observed for the agent, test chats included. Tool calls do not count. `null` if none since the replica started. |
| `heartbeat` | timestamp or null | The last `POST /v1/agent/heartbeat` this replica received from the agent. `null` if none since the replica started. |

Both sources are always returned. The controller applies `Agent.spec.lifecycle.activitySource` after merging timestamps across replicas. The per-Pod-IP fan-out and the `ServerName` override it needs are on [Activity tracking API](../user/activation-and-activity.md#activity-tracking-api).

**Response codes:** `200 OK`. `400 invalid_request` when `namespace` is missing.

## GET /v1/channels/health

The [AgentChannelReconciler](../../controller/reconcilers.md#agentchannelreconciler) reads per-channel health observations here to set `status.conditions[type=PlatformConnected]`. The caller presents `kaalm-controller-tls`.

**Request:**

```
GET /v1/channels/health?namespace=team-support
```

**Response body:**

```json
{
  "windowSeconds": 300,
  "replicaStartedAt": "2026-04-29T12:00:00Z",
  "channels": {
    "/channels/team-support/support-assistant": {
      "state": "success",
      "reason": "WebhookReady",
      "timestamp": "2026-04-29T12:48:11Z",
      "lastError": null
    },
    "/channels/team-support/personal-assistant": {
      "state": "failure",
      "reason": "WebhookAuthFailed",
      "timestamp": "2026-04-29T12:46:02Z",
      "lastError": "webhook auth validation failed: 401 Unauthorized"
    },
    "/channels/team-support/new-channel": {
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
| `windowSeconds` | int | The rolling window this replica observes, from the Helm value `gateway.channelHealthWindow` (default `5m`, reported as `300`). Echoed so the controller needs no separate channel for the value. |
| `replicaStartedAt` | timestamp | When this replica started. The controller uses it to tell "no in-window traffic" from "the replica has not been up for a full window" when `state` is `empty`. |
| `channels` | map | Keys are the channel paths registered on this gateway; values are per-channel records as observed by this replica |
| `state` | string | `success` when any in-window observation succeeded; `failure` when the in-window list is non-empty and holds only failures; `empty` when this replica has no in-window observation |
| `reason` | string or null | For `success`, the most recent success's reason (`WebhookReady`). For `failure`, the most recent failure's reason: `WebhookAuthFailed`, `AgentNotReady`, `DispatchFailed`, `CallbackInvalid`, or `CallbackRejected`. `null` when `empty`. |
| `timestamp` | timestamp or null | The most recent in-window observation behind `state`. `null` when `empty`. |
| `lastError` | string or null | The most recent in-window failure's message, when there is one. It is set on a `success` record too when a failure also fell inside the window. |

The third channel in the example shows `state: "empty"`: this replica has no in-window observation for that path. The controller decides whether the channel is silent (`Unknown` with `reason=NoRecentTraffic`) or observation is incomplete (the existing condition is kept) by comparing `replicaStartedAt` with the window and consulting the other replicas ([Channel health tracking](../user/platform-adapters.md#channel-health-tracking)).

**Response codes:** `200 OK`. `400 invalid_request` when `namespace` is missing. Only channels whose path is under `/channels/{namespace}/` for the requested namespace are returned.

## POST /v1/test-chat

The optional [console](../../console/overview.md) delivers one operator-authored message to one Agent here and returns the reply. The caller presents `kaalm-console-tls`, whose SAN is the console Service DNS. The gateway does not re-authorize the person behind the request: the console runs `TokenReview` and `SubjectAccessReview` before calling ([Authentication](../../console/overview.md#authentication)), and possession of the console SAN carries that authorization, the same trust class as the controller on the two endpoints above.

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
| `userId` | string | yes | The console-authenticated identity, placed in the envelope's `userId` verbatim so the message is attributable to a person |
| `content` | string | yes | The message text |

The gateway builds a [`POST /v1/message` envelope](agent-endpoints.md#request-body-sent-by-the-gateway) with a fresh `messageId`, `channelType: "console"`, `channelId: "/console/{namespace}/{agent}"`, the given `userId`, a `sessionId` always derived per [Session identity](agent-endpoints.md#session-identity-the-sessionid-derivation), and empty `attachments` and `metadata`. Delivery is the sync channel path end to end: wake-on-demand, the four-attempt delivery schedule, envelope validation, and the `gateway.syncDeliveryDeadline` bound. A delivered test chat counts as `gatewayTraffic` for the Agent's activity. It is never a channel-health observation, because no AgentChannel is involved.

**Response:** `200 OK` with the agent's reply envelope verbatim.

| Status | `error.type` | Raised when |
|---|---|---|
| `400` | `invalid_request` | The method is not `POST`, the body is not JSON, or any of the four fields is missing or empty |
| `404` | `invalid_request` | The named Agent does not exist |
| `413` | `request_too_large` | The body exceeds `gateway.maxMessageBodyBytes`, the same cap as channel intake |
| `413` | `response_too_large` | The reply exceeds `gateway.maxResponseBodyBytes` |
| `502` | `delivery_failed` | Every delivery attempt failed |
| `504` | `wake_timeout`, `controller_unavailable`, `sync_deadline_exceeded` | As in [User Gateway error responses](errors.md#user-gateway-error-responses) |

## GET /v1/spend

The optional [console](../../console/overview.md) reads one namespace's current-period spend by workload here. The caller presents `kaalm-console-tls`, as for test-chat. Any single replica answers authoritatively: each holds the folded union of its own live counters and every peer's latest published partial, current to within one publish interval ([Per-workload spend](../llm/budgets-and-rate-limits.md#per-workload-spend)).

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
| `workloads` | map | USD as decimal strings per workload: `agent/{name}` and `task/{name}` from the attested certificate SAN, and `(unattributed)` for gateway-only-tier callers. The rows sum to the namespace figure in `ModelProvider.status.budgetUsage`, to within one publish and one reconcile interval. |

**Response codes:** `200 OK`. `400 invalid_request` when `namespace` is missing or the method is not `GET`.
