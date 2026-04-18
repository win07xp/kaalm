# Agentry — User Gateway Design

This document covers the User Gateway: the listener responsible for delivering channel messages from user-facing platforms to agent containers, including the activator (wake-on-demand) and activity tracking subsystems.

For the LLM Gateway (provider routing, budget, fallback) and the shared gateway architecture rationale, see [GATEWAY_LLM.md](./GATEWAY_LLM.md#why-a-shared-gateway). For the HTTP endpoint contracts agents implement, see [API_ENDPOINTS.md](./API_ENDPOINTS.md).

---

## User Gateway — Request Flow

1. **Webhook event arrives**: an external system POSTs to the gateway's webhook endpoint (e.g., `/channels/team-support/support-assistant`).
2. **Webhook adapter authenticates**: the gateway verifies the request using the configured auth method — bearer token validation or HMAC signature verification — from the AgentChannel's webhook auth config. See [AgentChannel webhook auth types](./API_RESOURCES.md#agentchannel) for configuration.
2a. **Payload size check**: the gateway rejects webhook payloads exceeding `maxMessageBodyBytes` (default: 1 MiB, configurable via Helm value `gateway.maxMessageBodyBytes`) with HTTP 413 before normalization. This prevents oversized payloads from consuming gateway resources or being forwarded to agent containers.
3. **Normalization**: the adapter translates the webhook payload into the Agentry message envelope. The `userId` is resolved using `AgentChannel.spec.webhook.userId` config (`fromHeader` or `fromBody`); if neither is configured or the value is absent, the configured `fallback` is used (empty string if omitted). See [AgentChannel](./API_RESOURCES.md#agentchannel) for extraction configuration.
   ```json
   {
     "messageId": "uuid",
     "channelType": "webhook",
     "channelId": "/channels/team-support/support-assistant",
     "userId": "caller-id-extracted-per-config",
     "content": "Hello, I need help with my order",
     "attachments": [],
     "metadata": {}
   }
   ```
4. **AgentChannel lookup**: the gateway finds the AgentChannel resource matching the webhook path, which identifies the target Agent and namespace. If multiple AgentChannels claim the same path (a transient conflict before the reconciler marks the newer one as `Ready=False`), the gateway routes to the AgentChannel with the earliest `creationTimestamp`. See [AgentChannel](./API_RESOURCES.md#agentchannel) for session configuration and webhook path uniqueness constraints.
   If the AgentChannel has `session.enabled: true`, the gateway generates a deterministic `sessionId` from the message's `channelId` and `userId`: `sessionId = UUIDv5(namespace: f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d, name: channelId + ":" + userId)`. The namespace constant `f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d` is a purpose-generated UUID published as part of the Agentry API specification; it is identical across all installations and versions and must not change after v1 ships. This ID is stable across gateway replicas and restarts — no gateway-side session state is required. Session expiry and rotation are the agent's responsibility using its PVC state.
5. **Activator check**: if the Agent is `Hibernated`, the gateway signals the controller to wake it and waits up to `wakeTimeout` for the Pod to become Ready. In sync mode, the webhook caller blocks during this wait. In async mode, the gateway has already returned 202 (see step 5a). See [Activator](#activator) below.
5a. **Async early return** (async mode only): if `AgentChannel.spec.webhook.responseMode` is `async`, the gateway returns HTTP 202 Accepted with a `requestId` (a UUID) immediately after normalization (step 3). Steps 5-7 proceed asynchronously — the webhook caller does not block. The `requestId` is an opaque string; callers must not attempt to parse or construct it.

On poll (`GET /v1/channels/responses/{requestId}?channelPath=...`), any gateway replica reads the response from the `agentry-async-{requestId}` ConfigMap in `agentry-system`. The `channelPath` query parameter is used to look up AgentChannel auth config for authenticating the poll request. See [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the response schemas.
6. **Message delivery**: the gateway posts the normalized envelope to `POST /v1/message` on the Agent's ClusterIP Service over HTTPS (or the override endpoint in `AgentChannel.spec.agentEndpoint`). The gateway verifies the agent's TLS certificate against the operator-managed CA.
6a. **Async delivery retry**: in async mode, if `POST /v1/message` fails (connection error, non-200 response), the gateway retries up to 3 times with exponential backoff (1s, 5s, 25s). If all retries fail, the gateway delivers a `delivery_failed` error payload to `callbackUrl` (if configured, with retries) or stores it at the polling endpoint under the original `requestId`. See [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the error payload schema. In sync mode, non-200 responses are treated as delivery failures recorded in AgentChannel status conditions.

**Async response persistence**: when the receiving replica completes an async delivery and has the agent's response ready (or produces an error), it writes the response payload to a ConfigMap named `agentry-async-{requestId}` in `agentry-system`. The gateway already has full ConfigMap access in `agentry-system`, so no additional RBAC is required. Each ConfigMap is labeled with `agentry.io/channel-namespace` and `agentry.io/channel-name` to identify the originating AgentChannel, and annotated with a 1-hour expiry. Cross-namespace ownerRefs are not used (Kubernetes GC does not follow them); instead, the AgentChannelReconciler prunes expired ConfigMaps in `agentry-system` using label selectors on its reconcile passes. Any gateway replica can serve poll requests by reading from this ConfigMap — no replica-affinity or in-memory routing is required.
7. **Response (sync mode, default)**: the agent returns a response envelope; the gateway returns it as the webhook HTTP response body.
8. **Response (async mode)**: the agent returns a response envelope; the gateway POSTs it to the configured `callbackUrl` (with retries) or stores it for polling retrieval via `GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}` — see [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed).

---

## Platform Adapters

v1 ships with the **generic webhook adapter** only (inbound HTTP POST with configurable auth). Discord and WhatsApp adapters are deferred to v1.1 — they require persistent connections (Discord WebSocket, WhatsApp Cloud API registration), platform-specific reconnection logic, and API versioning, which adds significant implementation surface. The webhook adapter is stateless and covers the core channel integration pattern.

Platform adapters follow a plugin pattern for future extensibility:

```go
type ChannelAdapter interface {
    Type() string
    Authenticate(req *http.Request, credentials Credentials) error
    ParseInbound(req *http.Request) (MessageEnvelope, error)
    FormatOutbound(envelope MessageEnvelope) ([]byte, error)
    SendReply(ctx context.Context, envelope MessageEnvelope, credentials Credentials) error
}
```

The `SendReply` method is used for async response delivery: when `responseMode: async`, the gateway calls `SendReply` to POST the agent's response to the configured `callbackUrl` after the agent has processed the message. For sync mode, `SendReply` is not called — the response is returned inline as the HTTP response body. Discord and WhatsApp adapters (v1.1) will use `SendReply` for all responses since those platforms are inherently asynchronous.

---

## Activator

When an Agent is in the `Hibernated` phase, its Service has no endpoints. The gateway serves as the activator:

1. A channel message arrives at the User Gateway targeting a hibernated Agent (via AgentChannel).
2. The gateway calls the controller's activator endpoint (`POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service) to signal a wake request.
3. The controller transitions the Agent from `Hibernated` to `Resuming` and recreates the Pod. See [Agent State Machine](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode) for the full lifecycle.
4. The gateway waits for the Pod to become Ready (bounded by `spec.lifecycle.wakeTimeout`, which defaults from AgentClass), then delivers the message. If the timeout is exceeded:
   - **Sync mode**: the gateway returns HTTP 504 to the webhook caller.
   - **Async mode**: the gateway delivers a `wake_timeout` error payload to `callbackUrl` (if configured, with retries) or stores it at the polling endpoint under the original `requestId`. The error expires after 1 hour, same as successful responses. See [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the error payload schema.

**Sync-mode retry risk**: in sync mode, if the wake takes longer than the webhook caller's HTTP timeout (commonly 30-60s, shorter than the default `wakeTimeout` of 2 minutes), the caller receives 504 and will typically retry the webhook call. The gateway treats the retry as a new delivery and posts the message to the agent again — potentially with the same `messageId` if the caller preserves it. Agents with `hibernationEnabled: true` must deduplicate on `messageId` — see [Agent Runtime Contract](./ARCHITECTURE.md#agent-runtime-contract).

### Activator Authentication

The activator endpoint is authenticated to prevent unauthorized wake-ups from arbitrary Pods in the cluster. See [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api) for the full HMAC scheme.

The operator generates a shared HMAC key on installation and stores it in a Secret in `agentry-system` (`agentry-activator-key`). Both the controller and gateway read this Secret:

- The gateway includes an `Authorization: Bearer <HMAC(timestamp:namespace:agentName)>` header with each activation request, plus an `X-Agentry-Timestamp` header.
- The controller validates the HMAC signature and rejects requests with timestamps older than 30 seconds (replay window).

This ensures only the gateway (which holds the shared key) can trigger agent wake-ups.

**HMAC key rotation**: the `agentry-activator-key` Secret stores two fields: `current-key` and `previous-key` (base64-encoded random bytes). When the operator rotates the key (configurable interval, default: 30 days):
1. The operator writes the new key to `current-key` and moves the old value to `previous-key`.
2. Both the gateway and controller watch the Secret. When they pick up the change, they accept HMAC signatures validated against **either** key.
3. After a configurable transition window (default: 60 seconds, `hmacKeyTransitionWindow`), the operator removes `previous-key`.

This key-ring approach eliminates the failure window that would otherwise occur between the two components independently picking up the new Secret — during the transition window, either key is valid. If `previous-key` is absent (first install or after the transition window has elapsed), only `current-key` is used for both signing and verification.

---

## Activity Tracking API

The gateway maintains per-agent activity timestamps in-memory, updated on every LLM request, channel message delivery, and agent heartbeat. This avoids per-request etcd writes, which is critical at scale (hundreds of thousands of agents). The controller uses this data to evaluate idle and hibernation transitions — see [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection).

The gateway exposes an internal endpoint for the controller to query activity state. This endpoint uses the same HMAC authentication as the activator endpoint (shared key from `agentry-activator-key` Secret) — see [Activator Authentication](#activator-authentication).

**`GET /v1/activity?namespace={ns}`**

Returns a JSON object containing the gateway's startup timestamp and a map of agent names to their last-activity timestamps, broken out by signal source, for the given namespace:

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

The gateway tracks both signal sources (gateway-observed LLM and channel traffic, and agent heartbeats) separately per agent and always returns both. The controller applies the `activitySource` filter (from `Agent.spec.lifecycle.activitySource`) after merging results across replicas — selecting `gatewayTraffic`, `heartbeat`, or the max of both depending on the setting. The gateway does not need to read Agent specs to perform this filtering; the controller owns the policy.

A `null` value for a source means the gateway has no record of that signal type for the agent since its last restart.

The `startedAt` field indicates when the gateway started. The controller uses this to detect gateway restarts — if the gateway started more recently than an agent's last phase transition, missing activity data is treated as "unknown" rather than "no activity".

**Multi-replica fan-out**: because each gateway replica maintains its own in-memory activity store (updated only by the traffic it handles), querying the gateway ClusterIP Service (which round-robins to one replica) would return only that replica's view — agents whose last request landed on a different replica would appear idle. The controller instead queries all gateway Pod IPs directly in parallel: it enumerates gateway Pods via its Pod informer (matching the gateway label selector in `agentry-system`) and issues one `GET /v1/activity?namespace={ns}` request per Pod IP. It takes the **most recent timestamp per agent per source** across all responses. Replicas that are unreachable (connection refused, timeout) are skipped; data from the remaining replicas is used. The `startedAt` field in each response is evaluated per-replica for restart detection — if one replica has restarted more recently than an agent's last phase transition, only that replica's data is treated as unknown. See [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) for the reconciler implementation detail.

The controller queries all replica Pod IPs on each reconcile for agents in `Running` or `Idle` phase to evaluate idle and hibernation transitions. If all replicas are unreachable, the controller preserves the agent's current phase — no idle transitions are made without activity data.

Activity data is ephemeral: it is lost on gateway restart. The gateway includes its `startedAt` timestamp in the `/v1/activity` response so the controller can detect this condition. After a gateway restart:
- The controller defers idle and hibernation transitions for agents whose last phase transition predates the gateway's `startedAt`, treating missing data as "unknown" until the gateway has been running for at least `idleTimeout`.
- Agents that are actively sending traffic re-establish their activity timestamps immediately.
- Agents that are truly idle will transition to `Idle` after `idleTimeout` elapses from the gateway's startup, which is the correct behavior.

---

## Observability

The gateway exposes Prometheus metrics on `:9090/metrics`:

- `agentry_channel_messages_total{channel_type,namespace,status}`
- `agentry_channel_message_duration_seconds{channel_type}`
- `agentry_channel_wake_total{namespace}` (count of hibernation wakes triggered)

For LLM Gateway metrics, see [GATEWAY_LLM.md](./GATEWAY_LLM.md#observability).

---

## Failure Modes

| Failure | Behavior |
|---|---|
| All gateway replicas down | Webhook callers receive 503; controller cannot wake hibernated agents |
| Channel credential invalid | AgentChannel marked `Ready=False`; platform connection drops |
| Agent Pod not ready (resuming) | User Gateway holds or retries message delivery up to configured timeout |
| Async response ConfigMap not found | Poll returns 404; caller should retry or use `callbackUrl` for guaranteed delivery |
