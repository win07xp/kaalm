# Agent Endpoints

Two endpoints make up the gateway's contract with long-running agent containers. They point in opposite directions:

- `POST /v1/agent/heartbeat` is implemented by the gateway and called by the agent, to signal liveness.
- `POST /v1/message` is implemented by the agent and called by the gateway, to deliver channel messages.

Both are restricted to Pods backed by an Agent resource. AgentTask Pods are rejected (see the callout under [`POST /v1/agent/heartbeat`](#post-v1agentheartbeat) below).

## POST /v1/agent/heartbeat

Called by the agent container to signal liveness for idle detection. It is only meaningful when the Agent's [`spec.lifecycle.activitySource`](../../resources/agent.md) is `agentHeartbeat` or `both`.

The gateway accepts heartbeats unconditionally: every heartbeat updates the agent's last-activity timestamp in the gateway's in-memory activity store, regardless of the Agent's `activitySource` setting. Per-Agent filtering by `activitySource` happens controller-side at merge time (see [`/v1/activity` response semantics](internal-endpoints.md#get-v1activity) and [Activity Detection](../../controller/hibernation-and-wake.md#activity-detection)). Heartbeats from a `gatewayTraffic`-only agent are therefore recorded at the gateway and silently dropped at merge, which means starter templates may emit heartbeats unconditionally without consulting the Agent CR.

**Footgun: unconditional heartbeats under a non-default `activitySource`.** The safe-by-default behavior above only holds because `gatewayTraffic` is the default. `agentHeartbeat` and `both` are intended for custom agent images that gate heartbeat emission on actual work. If an image emits an unconditional periodic heartbeat (as the starter templates do, every 30s, in agent mode only; task-mode runtimes send no heartbeats) and the Agent is set to `agentHeartbeat` or `both`, the last-activity timestamp stays permanently fresh and the agent will never transition to `Idle` or `Hibernated`. Starter-template-based images should leave `activitySource` at the default `gatewayTraffic`. See the [`activitySource` design note](../../resources/agent.md) and [Starter Templates](../../runtime/starter-templates.md).

**Request body:** empty or `{}`.

**Response:** `200 OK` with empty body.

**Error responses.** Errors carry the structured `{ "error": { "type", "message", "retryable" } }` envelope, reusing the type vocabulary from [`/v1/task/complete`](task-complete.md) and the [User Gateway error table](errors.md#user-gateway-error-responses):

| Status | `error.type` | `retryable` | When |
|---|---|---|---|
| `401 Unauthorized` | `unauthorized` | `false` | The source-IP → Pod cross-check fails: the source IP does not resolve to any Pod in the cert-SAN-derived namespace via the gateway's informer cache. Same envelope shape as the [LLM Gateway 401 row](errors.md#llm-gateway-error-responses). |
| `403 Forbidden` | `access_denied` | `false` | The calling Pod is not associated with an Agent. This includes the AgentTask-SAN-at-listener / Agent-only-at-handler case: an AgentTask Pod's cert passes the TLS listener but this handler accepts Agents only. See [The Kaalm Gateway](../overview.md). |

There is **no** live API-server fallback on this endpoint, unlike [`/v1/task/complete`](task-complete.md). Heartbeats are periodic, so a single call dropped during informer lag is recovered on the next heartbeat tick, and the per-call API cost of a fallback is not justified here. Custom agents that emit a heartbeat within the first ~100ms of startup may observe a `401` for this reason; the standard advice is to either delay the first heartbeat past informer-lag or accept the missed tick.

**Frequency.** Heartbeat frequency is the agent's choice. A reasonable default is every 30-60 seconds. The gateway coalesces rapid heartbeats in memory: no etcd or API server writes occur per heartbeat.

## POST /v1/message

This endpoint is **implemented by the agent container**, not by the gateway. The User Gateway calls it to deliver normalized channel messages (see [User Gateway Request Flow](../user/overview.md#request-flow)). Agents that use AgentChannel must expose this endpoint on `$KAALM_HEALTH_PORT` (default 8080).

### Agent-side auth contract

Because the agent is the server here, the agent is responsible for authenticating the gateway:

- The `/v1/message` listener must terminate TLS using the agent's cert-manager-issued cert (`$KAALM_TLS_CERT` / `$KAALM_TLS_KEY`).
- It must request client certificates with `tls.Config.ClientAuth = tls.VerifyClientCertIfGiven` (or equivalent), with `ClientCAs` loaded from `$KAALM_CA_CERT`.
- It must enforce per-path at the handler: `/v1/message` returns `401 Unauthorized` when no client cert was presented, and `403 Forbidden` when the peer cert's SAN does not match the gateway Service DNS (`kaalm-gateway.kaalm-system.svc.cluster.local` or `kaalm-gateway.kaalm-system.svc`).

Enforcement cannot live at the TLS handshake (`RequireAndVerifyClientCert`): the kubelet's HTTP probes share the same port and present no client certificate, so a handshake-level requirement would fail every probe and leave the agent permanently unready. This is the same `VerifyClientCertIfGiven` + per-path pattern the controller `:9443` and gateway `:8443` listeners use. Layered with the synthesized NetworkPolicy gateway → agent ingress allow rule, this is what keeps the message path safe under a misconfigured per-Agent NetworkPolicy. See [The Runtime Contract item 4](../../runtime/contract.md) and [In-cluster TLS](../../security/tls.md#in-cluster-tls).

Both endpoints on this page, the port they share with the kubelet probes, and the auth on every other agent-to-gateway call are drawn together in [Communication summary](../../runtime/contract.md#communication-summary).

### Request body (sent by the gateway)

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
| `messageId` | string (UUID) | yes | Unique identifier for this message, generated by the gateway. **All agents MUST deduplicate on this value**; see below. |
| `channelType` | string | yes | Platform type: `"webhook"` in v1 (Discord, WhatsApp in v1.1) |
| `channelId` | string | yes | Platform-specific channel identifier |
| `userId` | string | yes | Platform-specific user identifier, extracted per AgentChannel config; see below |
| `sessionId` | string | no | Deterministic session identifier, present when `AgentChannel.spec.session.enabled: true`; see [Session identity](#session-identity-the-sessionid-derivation) below |
| `content` | string | yes | The user's message text, populated per `AgentChannel.spec.webhook.content` extraction (`fromHeader`, `fromBody`, or raw-body fallback when unconfigured); see [AgentChannel design notes](../../resources/agentchannel.md) |
| `attachments` | array | no | List of attachment objects (platform-specific schema); see the v1 contract note below |
| `metadata` | map | no | Platform-specific fields (e.g. `guildId` for Discord); see the v1 contract note below |

**`messageId` deduplication (mandatory).** All agents implementing `/v1/message` MUST deduplicate on `messageId`. The gateway's agent-delivery retry pipeline (up to 3 retries with 1s/5s/25s backoff, see [`delivery_failed`](async-responses.md)) reuses the same `messageId` across attempts for every agent, and an earlier attempt may have reached the agent even when the gateway's read of the response failed. An in-memory LRU is sufficient for non-hibernated agents; agents with `hibernationEnabled: true` must additionally persist the dedup buffer across pod restarts. Caller retries (for example, a webhook caller resending after a sync-mode 504) are delivered as a fresh message with a new `messageId`. See [The Runtime Contract item 7](../../runtime/contract.md).

**`userId` extraction.** Extracted per the `AgentChannel.spec.webhook.userId` config (`fromHeader` or `fromBody`); falls back to the configured `fallback` value, or the empty string if unconfigured. When `session.enabled: true` and `userId` is empty, all unattributed requests share a session.

**v1 contract for `attachments` and `metadata`.** The generic webhook adapter always passes `[]` and `{}` respectively; the v1.1 Discord/WhatsApp adapters will fill these per platform. See [Request Flow step 3](../user/overview.md#request-flow) and [AgentChannel content extraction](../../resources/agentchannel.md).

### Session identity: the sessionId derivation

When `AgentChannel.spec.session.enabled: true`, the gateway computes a **deterministic** `sessionId` for each message:

```
sessionId = UUIDv5(namespace: f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d, name: channelId + ":" + userId)
```

The namespace constant `f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d` is a purpose-generated UUID published as part of the Kaalm API specification. It is identical across all installations and versions. **This constant must not change after v1 ships**: any change would invalidate existing session state in agent PVCs, because agents key their conversation state by `sessionId`.

Because the derivation is a pure function of `channelId` and `userId`, the resulting ID is stable across gateway replicas and gateway restarts, and no gateway-side session state is required. Session expiry and rotation are the agent's responsibility: the agent uses its PVC to track conversation state and decides when a "session" is over. When `session.enabled: false`, no `sessionId` is included in the envelope.

### Response body (returned by the agent)

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
| `attachments` | array | no | Attachments to send in the reply. Passed through by the gateway as-is (see the agent → gateway contract below); v1 generic webhook callers receive them as opaque JSON, and v1.1 platform adapters interpret per platform |
| `metadata` | map | no | Optional platform-specific reply metadata. Pass-through identical to `attachments` above |

### Agent → gateway

`200 OK` with a JSON-parseable response envelope containing the required `content` field is expected. The gateway validates the envelope shape before forwarding: a non-2xx status, a connection error, an unparseable body, or a 200 with a missing or non-string `content` field all feed the gateway's agent-delivery retry pipeline as a `delivery_failed` signal, with the same schedule in both sync and async modes (see [`delivery_failed`](async-responses.md)).

On retry exhaustion, sync callers receive `502` with the `delivery_failed` envelope (under default config, `504 sync_deadline_exceeded` fires first; `502 delivery_failed` is reached only when `syncDeliveryDeadline` is raised above the delivery-retry budget, see the [reachability callout](channel-webhook.md) under the sync-mode response table). Async callers receive the same payload via callback or polling. Failures are recorded in AgentChannel status conditions in either case.

Optional fields (`attachments`, `metadata`) are not validated by the gateway and pass through as-is.

### Gateway → webhook caller (sync mode only)

The gateway returns the agent's response body verbatim with `200 OK` on success. On failure it returns a structured error envelope using the [User Gateway Error Responses](errors.md#user-gateway-error-responses) status mapping: `502` for `delivery_failed`; `504` for `wake_timeout`, `controller_unavailable`, and `sync_deadline_exceeded`; `413` for `response_too_large`.

Sync callers face a delivery-retry budget on the order of half a minute to just over a minute before `delivery_failed` is returned; the arithmetic behind that range is worked through, and drawn on a single time axis, in [Sync-Mode Reachability](async-responses.md#sync-mode-reachability). The `gateway.syncDeliveryDeadline` Helm knob (default 30s) bounds the total sync-mode wall-clock: the gateway short-circuits with `504 sync_deadline_exceeded` (`retryable: true`) if delivery plus agent processing would exceed the deadline, giving callers a deterministic upper SLA. Callers needing a tighter SLA should prefer `responseMode: async` with `callbackUrl` or polling; the async budget is unchanged and is not bounded by `syncDeliveryDeadline`.

Channels backing hibernated agents should default to `responseMode: async` regardless of caller SLA: `wakeTimeout` (default 120s) exceeds `syncDeliveryDeadline` (default 30s) by 4x, so a sync caller under defaults will never observe `wake_timeout` and will instead see `sync_deadline_exceeded` mid-wake. The full sync-reachability argument, with the timeline figure, lives at [Sync-Mode Reachability](async-responses.md#sync-mode-reachability).
