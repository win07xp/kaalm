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
4. **AgentChannel lookup**: the gateway finds the AgentChannel resource matching the webhook path, which identifies the target Agent and namespace. The gateway routes webhook traffic **only** to AgentChannels whose `status.conditions[type=Ready].status == True`; `Ready=False` channels (including a `PathConflict` loser that has not yet been deleted) receive no traffic regardless of their order in the gateway's watch event stream. On a path collision, the channel with the earliest `creationTimestamp` is the one that reaches `Ready=True` — the AgentChannelReconciler marks the newer collider as `Ready=False, reason=PathConflict` — so the gateway's Ready filter naturally selects the same winner the reconciler does. See [AgentChannel](./API_RESOURCES.md#agentchannel) for session configuration and webhook path uniqueness constraints.
   If the AgentChannel has `session.enabled: true`, the gateway generates a deterministic `sessionId` from the message's `channelId` and `userId`: `sessionId = UUIDv5(namespace: f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d, name: channelId + ":" + userId)`. The namespace constant `f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d` is a purpose-generated UUID published as part of the Agentry API specification; it is identical across all installations and versions and must not change after v1 ships. This ID is stable across gateway replicas and restarts — no gateway-side session state is required. Session expiry and rotation are the agent's responsibility using its PVC state.
5. **Activator check**: if the Agent is `Hibernated`, the gateway signals the controller to wake it and waits up to `wakeTimeout` for the Pod to become Ready. In sync mode, the webhook caller blocks during this wait. In async mode, the gateway has already returned 202 (see step 5a). See [Activator](#activator) below.
5a. **Async early return** (async mode only): if `AgentChannel.spec.webhook.responseMode` is `async`, the gateway returns HTTP 202 Accepted immediately after normalization (step 3) with a body containing `requestId` (a UUID) and `channelId` (the originating AgentChannel's webhook path). Steps 5-7 proceed asynchronously — the webhook caller does not block. The `requestId` is opaque; callers must not parse or construct it. The `channelId` must be preserved by the caller and passed back as the `channelPath` query parameter on any poll request — see [API_ENDPOINTS.md § Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the full schema.

On poll (`GET /v1/channels/responses/{requestId}?channelPath=...`), any gateway replica reads the response from the `agentry-async-{requestId}` ConfigMap in `agentry-system`. The `channelPath` query parameter is used to look up AgentChannel auth config for authenticating the poll request. After authentication, the gateway asserts that the ConfigMap's `agentry.io/channel-namespace` / `agentry.io/channel-name` labels match the authenticated channel before returning the payload — a `requestId` stored by one channel cannot be read via another channel's credentials; mismatches return `403 Forbidden`. See [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the response schemas and the channel-match assertion.
6. **Message delivery**: the gateway posts the normalized envelope to `POST /v1/message` on the Agent's ClusterIP Service over HTTPS (or the override endpoint in `AgentChannel.spec.agentEndpoint`). The gateway verifies the agent's TLS certificate against the Agentry CA (`agentry-ca`, managed by cert-manager — see [GATEWAY_LLM.md § TLS on the LLM Gateway Listener](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener)).
6a. **Async delivery retry**: in async mode, if `POST /v1/message` fails (connection error, non-200 response), the gateway retries up to 3 times with exponential backoff (1s, 5s, 25s). If all retries fail, the gateway delivers a `delivery_failed` error payload to `callbackUrl` (if configured, with retries) or stores it at the polling endpoint under the original `requestId`. See [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the error payload schema. In sync mode, non-200 responses are treated as delivery failures recorded in AgentChannel status conditions.

**Async response persistence**: when the receiving replica completes an async delivery and has the agent's response ready (or produces an error), it writes the response payload to a ConfigMap named `agentry-async-{requestId}` in `agentry-system`. The gateway already has full ConfigMap access in `agentry-system`, so no additional RBAC is required. Each ConfigMap is labeled with `agentry.io/channel-namespace` and `agentry.io/channel-name` to identify the originating AgentChannel, and annotated with a 1-hour expiry. Cross-namespace ownerRefs are not used (Kubernetes GC does not follow them); instead, the AgentChannelReconciler prunes expired ConfigMaps for live channels on its reconcile passes (label selector: `agentry.io/channel-namespace={ns},agentry.io/channel-name={name}`, annotation expiry past). When an AgentChannel is deleted, its finalizer sweeps all of that channel's `agentry-async-*` ConfigMaps in one shot before removing the finalizer — see [Finalizers](./CONTROLLER_LIFECYCLE.md#finalizers); without this, cross-namespace ownerRefs would leave them orphaned until annotation expiry. Any gateway replica can serve poll requests by reading from this ConfigMap — no replica-affinity or in-memory routing is required.
7. **Response (sync mode, default)**: the agent returns a response envelope; the gateway returns it as the webhook HTTP response body.
8. **Response (async mode)**: the agent returns a response envelope; the gateway POSTs it to the configured `callbackUrl` (with retries) or stores it for polling retrieval via `GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}` — see [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed).

   **`callbackUrl` re-validation on delivery**: before each POST attempt to `callbackUrl` (both the initial attempt and each retry), the gateway re-resolves the host and re-checks it against the blocked IP ranges from [AgentChannel validation rule 22](./API_RESOURCES.md#cross-resource-validation). This defeats DNS rebinding between reconcile-time validation and delivery. If the host now resolves to a blocked range (loopback, link-local, RFC1918, unique-local IPv6, cloud metadata, or anything outside `gateway.callbackUrl.allowlist` when set), the delivery attempt is not dialed — the gateway instead stores a `callback_invalid` error payload at the polling endpoint (or drops the callback if polling is unavailable) and records the AgentChannel `Warning` event `reason=CallbackInvalid`. The error is delivered to the polling endpoint per async rules; retries are not attempted for a host that fails validation.

---

## TLS and Ingress

The User Gateway listener on `:8080` serves TLS using the same `agentry-gateway-tls` Certificate as the LLM listener — both listeners share a single cert whose SAN set covers the gateway's in-cluster Service DNS names. No plaintext path exists on the gateway; all webhook, activator, activity, and async-polling traffic is TLS end-to-end.

**Listener separation**: port 8080 serves only externally-reachable channel traffic — webhook intake under `/channels/*` and the async polling fallback under `/v1/channels/responses/*`. All internal mTLS-authenticated endpoints (`/v1/activity`, `/v1/channels/health`) live on the LLM Gateway listener on `:8443`. This split ensures that an Ingress fronting 8080 cannot route untrusted traffic to an endpoint whose authorization assumes a controller-SAN client cert. See [GATEWAY_LLM.md § TLS on the LLM Gateway Listener](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener) for the 8443 endpoint set.

**Recommended Ingress configuration**: external webhook traffic arrives at a cluster Ingress that terminates TLS with the cluster's public certificate and then connects to the gateway backend. Two Ingress modes are supported; operators pick one:

- **Backend re-encrypt (HTTPS-to-HTTPS)** — the Ingress controller speaks HTTPS to the gateway on port 8080, presenting the Agentry CA as the backend CA bundle (or disabling verification if the controller trusts cluster-internal names). This is the recommended default because it works with off-the-shelf Ingress controllers (NGINX, Traefik, HAProxy, most cloud LB Ingress classes).
- **TLS pass-through** — the Ingress forwards raw TLS bytes to the gateway without terminating, so the external client speaks TLS directly with the gateway. This preserves end-to-end TLS with the gateway's cert but requires the Ingress controller to support pass-through SNI routing and requires clients to trust the gateway's cert chain. Operators using pass-through must set the Helm value `gateway.externalHostnames` to add the public hostname to the gateway cert's SAN list — the default SAN set covers only in-cluster Service DNS, which would fail verification for an external client dialing the public hostname. See [DEPLOYMENT.md § Helm Chart Contents](./DEPLOYMENT.md#helm-chart-contents).

Internal callers inside the cluster verify the gateway's cert against `agentry-ca` (projected by `trust-manager` into every namespace) when dialing the gateway directly. Cluster-local webhook producers and the gateway's own async-poll reads dial the User listener on `:8080`; the controller's activity fan-out dials the LLM listener on `:8443` (see [§ Activity Tracking API](#activity-tracking-api)).

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

The webhook adapter's `SendReply` applies the `callbackUrl` allowlist/blocklist check before dialing — see step 8 in [Request Flow](#user-gateway--request-flow) and [rule 22](./API_RESOURCES.md#cross-resource-validation). The check is inside `SendReply` (not in a shared middleware) so each adapter can apply transport-appropriate rules if v1.1 adapters call into non-HTTP destinations.

---

## Activator

When an Agent is in the `Hibernated` phase, its Service has no endpoints. The gateway serves as the activator:

1. A channel message arrives at the User Gateway targeting a hibernated Agent (via AgentChannel).
2. The gateway calls the controller's activator endpoint (`POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service) to signal a wake request. **This call is mTLS over HTTPS.** The controller serves its activator endpoint with a cert-manager-issued `Certificate` (`agentry-controller-tls`) signed by the same `ClusterIssuer` (`agentry-ca-issuer`) that signs the gateway cert, so the gateway verifies the controller's cert against the Agentry CA and the controller verifies the gateway's client cert against the same CA. See [Activator Authentication](#activator-authentication).
3. The activator handler (served on every controller replica) patches `agentry.io/wake=true` on the target Agent via the apiserver. The leader's existing Agent watch observes the annotation and runs the manual-wake path in `AgentReconciler` step 9, which transitions the Agent from `Hibernated` to `Resuming` and recreates the Pod. The handler does not need to be on the leader — any replica that receives the POST can patch the annotation, and the leader picks it up through the watch. See [Agent State Machine](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode) for the full lifecycle and [CONTROLLER_RECONCILERS.md § Operator Structure](./CONTROLLER_RECONCILERS.md#operator-structure) for the handler wiring.
4. The gateway waits for the Pod to become Ready (bounded by `spec.lifecycle.wakeTimeout`, which defaults from AgentClass), then delivers the message. If the timeout is exceeded:
   - **Sync mode**: the gateway returns HTTP 504 to the webhook caller.
   - **Async mode**: the gateway delivers a `wake_timeout` error payload to `callbackUrl` (if configured, with retries) or stores it at the polling endpoint under the original `requestId`. The error expires after 1 hour, same as successful responses. See [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the error payload schema.
5. **Controller unreachable**: if the gateway cannot reach the controller's activator endpoint at all (connection refused, TLS handshake fails, client-cert authorization rejection, or 5xx after one internal retry), the wake cannot be attempted. The Agent remains `Hibernated`. In sync mode, the gateway returns `504 Gateway Timeout` with an error body carrying `error.type: controller_unavailable` and `retryable: true`. In async mode, the gateway delivers a `controller_unavailable` error payload to `callbackUrl` (with retries) or stores it at the polling endpoint. See [Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed). Already-`Running` agents are unaffected — this failure mode only impacts wake-on-demand.

**Sync-mode retry risk**: in sync mode, if the wake takes longer than the webhook caller's HTTP timeout (commonly 30-60s, shorter than the default `wakeTimeout` of 2 minutes), the caller receives 504 and will typically retry the webhook call. The gateway treats the retry as a new delivery and posts the message to the agent again — potentially with the same `messageId` if the caller preserves it. Agents with `hibernationEnabled: true` must deduplicate on `messageId` — see [Agent Runtime Contract](./RUNTIME_CONTRACT.md).

### Activator Authentication

The activator endpoint authenticates callers via **mTLS** — there is no shared-secret layer on top of TLS. The controller's activator listener requires a client certificate on every connection:

- The gateway presents its `agentry-gateway-tls` cert as the client cert when calling `POST /v1/activate/{namespace}/{agentName}`.
- The controller verifies the client cert against the Agentry CA (`agentry-ca`) and authorizes the request only if the cert's SAN matches the gateway Service DNS (`agentry-gateway.agentry-system.svc.cluster.local` or `.svc`). Any other SAN — even one signed by `agentry-ca` — is rejected with `403 Forbidden`.

cert-manager rotates both certs continuously from `agentry-ca-issuer`; there is no separate Secret to manage or rotate. See [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api) for the matching SAN authorization rules on the reverse direction (controller → gateway activity API).

---

## Activity Tracking API

The gateway maintains per-agent activity timestamps in-memory, updated on every LLM request, channel message delivery, and agent heartbeat. This avoids per-request etcd writes: v1 targets 1000 Agents/AgentTasks per cluster, and the in-memory store is deliberately designed to scale an order of magnitude higher without a design change as future versions grow the target. The controller uses this data to evaluate idle and hibernation transitions — see [Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection).

The gateway exposes an internal endpoint for the controller to query activity state. The endpoint serves **HTTPS** using the gateway's `agentry-gateway-tls` Certificate and **requires an mTLS client cert on this path**. The controller presents its `agentry-controller-tls` cert; the gateway verifies against `agentry-ca` and authorizes only if the client cert's SAN matches the controller Service DNS (`agentry-controller.agentry-system.svc.cluster.local` or `.svc`). There is no separate shared-secret or bearer-token layer on top of the mTLS tunnel. See [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api).

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

The `startedAt` field indicates when the gateway started. The controller uses this to detect gateway restarts — if the gateway started more recently than an agent's `status.phaseTransitionTime` (a dedicated Agent status field set by the AgentReconciler on every phase change — see [API_RESOURCES.md § Agent design notes](./API_RESOURCES.md#design-notes)), missing activity data is treated as "unknown" rather than "no activity".

**Multi-replica fan-out**: because each gateway replica maintains its own in-memory activity store (updated only by the traffic it handles), querying the gateway ClusterIP Service (which round-robins to one replica) would return only that replica's view — agents whose last request landed on a different replica would appear idle. The controller instead queries all gateway Pod IPs directly in parallel: it enumerates gateway Pods via its Pod informer (matching the gateway label selector in `agentry-system`) and issues one `GET /v1/activity?namespace={ns}` request per Pod IP. It takes the **most recent timestamp per agent per source** across all responses. Replicas that are unreachable (connection refused, timeout) are skipped; data from the remaining replicas is used. The `startedAt` field in each response is evaluated per-replica for restart detection — if one replica has restarted more recently than an agent's `status.phaseTransitionTime`, only that replica's data is treated as unknown. See [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) for the reconciler implementation detail.

**TLS verification on per-Pod-IP dials**: the gateway cert's SAN list covers the gateway's Service DNS (`agentry-gateway.agentry-system.svc.cluster.local`, `.svc`, `localhost`) — not Pod IPs, which would be impractical to enroll. The controller's HTTP transport sets `tls.Config.ServerName = "agentry-gateway.agentry-system.svc.cluster.local"` for these per-Pod-IP dials, so SAN verification succeeds against the Service DNS while the dial target remains the Pod IP. Cert authenticity is unchanged — verification chains to `agentry-ca` and the SAN match is performed against the explicit `ServerName`. Without this override, every fan-out dial would fail TLS verification because the Pod IP does not appear in the cert's SAN.

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
| Gateway replica not ready (listener, informer, or cert not loaded) | Replica removed from Service endpoints until readiness passes. See [GATEWAY_LLM.md § Gateway Readiness](./GATEWAY_LLM.md#gateway-readiness) |
| Channel credential invalid | AgentChannel marked `Ready=False`; platform connection drops |
| Agent Pod not ready (resuming) | User Gateway holds or retries message delivery up to configured timeout |
| Controller unreachable | Wake-on-demand fails; sync callers receive `504` with `error.type: controller_unavailable`; async stores a `controller_unavailable` error at the polling endpoint or delivers it to `callbackUrl`. Already-`Running` agents are unaffected. See [Activator § controller unreachable](#activator) |
| Async response ConfigMap not found | Poll returns 404; caller should retry or use `callbackUrl` for guaranteed delivery |
