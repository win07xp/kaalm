# User Gateway

The User Gateway is the listener that delivers channel messages from user-facing platforms to agent containers. Alongside message delivery it hosts two subsystems: the **activator**, which wakes hibernated Agents on demand, and **activity tracking**, which tells the controller when an Agent last did any work. This page covers the end-to-end request flow and the listener's TLS and Ingress setup. Three sub-pages carry the rest: [Platform adapters and channel health](platform-adapters.md) covers the adapter interface and how per-channel delivery health becomes an AgentChannel condition, [Activation and activity tracking](activation-and-activity.md#the-activator) covers wake-on-demand and the activity API the controller polls, and [User Gateway operations](operations.md#observability) covers metrics and failure modes.

For the LLM Gateway (provider routing, budget, fallback) and the shared gateway architecture rationale, see [LLM Gateway](../llm/overview.md#why-a-shared-gateway). For the HTTP endpoint contracts agents implement, see [HTTP API](../api/overview.md).

---

## Request flow

A webhook call passes through ten steps: the intake checks, normalization, the async accept, activation, delivery, and the response. Sync mode returns the agent's reply inline. Async mode returns `202` at step 6 and delivers the reply out of band.

![Flowchart of every check on POST /channels/{namespace}/{path} in the order the User Gateway runs them, as three rows. Intake: body within maxMessageBodyBytes, else 413 request_too_large; path registered to a Ready=True AgentChannel, else 401; channel not Terminating, else 401; bearer or HMAC auth passes, else 401. Envelope and target: body normalizes, else 400 invalid_request; referenced Agent exists, else 502 delivery_failed; then responseMode: sync wakes if needed, delivers, and answers inline. Async accept: pending records below maxPendingAsyncResponses, else 503 internal_unavailable; placeholder ConfigMap created, else 503 internal_unavailable; then 202 with requestId and channelPath.](../../diagrams/user-webhook-intake.svg)

Every check in the figure fires before the Agent is dialed, so a rejected call never wakes an Agent. [Channel webhook](../api/channel-webhook.md) is the wire contract for each status. The steps below say what each check reads and why it sits where it does.

### 1. Webhook event arrives

An external system POSTs to the channel's path, for example `/channels/team-support/support-assistant`. The path rules are rules 15 and 16 under [Cross-resource validation](../../resources/validation-and-defaulting.md#cross-resource-validation).

### 2. Payload size check

The gateway reads the body through a cap of `gateway.maxMessageBodyBytes` (default 1 MiB) and answers `413 request_too_large` when the body overflows it. The check runs on the raw frame before path resolution, so an oversized POST to any path on `:8080` gets `413` whether or not the path exists. A caller cannot tell a registered channel from an unregistered path by sending oversized bodies and comparing `413` with `401`, which preserves the path-existence rule stated under [Polling fallback](../api/async-responses.md#polling-fallback).

### 3. AgentChannel lookup

The gateway resolves the path to the AgentChannel registered at it. Only channels with `status.conditions[type=Ready].status: True` are registered, so a `PathConflict` loser or an `InvalidPath` channel receives no traffic however its watch events were ordered. The gateway also refuses any path that does not begin with the channel's own `/channels/{namespace}/` prefix, the gateway-side half of rule 15, independent of the reconciler's `Ready=False, reason=InvalidPath` status. An unregistered path is `401`, the same code as failed auth, so the response never reveals which paths are hosted. A channel in `status.phase: Terminating` also answers `401`: it accepts no new work, which is what lets the delete-time sweep under [Response persistence](../api/async-responses.md#response-persistence) run once.

On a path collision the channel with the earliest `creationTimestamp` is the only one that reaches `Ready=True`: the [AgentChannelReconciler](../../controller/reconcilers.md#agentchannelreconciler) marks the newer collider `Ready=False, reason=PathConflict`, and the Ready filter selects the same winner. See [AgentChannel](../../resources/agentchannel.md) for the path uniqueness constraints.

### 4. Authentication

The gateway verifies the request with the channel's own scheme. A webhook channel uses bearer token or HMAC signature verification per `spec.webhook.auth` ([Auth](../api/channel-webhook.md#auth)). A Discord or WhatsApp channel uses the platform's fixed signature scheme with the channel's credential Secret ([Platform adapters](platform-adapters.md#inbound)). Failure is `401` and a channel-health failure record.

### 5. Normalization

The adapter translates the webhook payload into the Kaalm message envelope:

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

| Field | Source |
|---|---|
| `messageId` | a fresh UUID for each inbound request |
| `channelId` | the channel's path |
| `userId` | `spec.webhook.userId` (`fromHeader` or `fromBody`), then its `fallback`, then the empty string |
| `content` | `spec.webhook.content`, resolved the same way; when neither extractor is set, the raw body JSON-encoded as a string, which must be valid UTF-8 |
| `attachments`, `metadata` | `[]` and `{}` from the webhook adapter; the Discord and WhatsApp adapters fill them per platform ([Discord channel](../api/channel-discord.md#normalization), [WhatsApp channel](../api/channel-whatsapp.md#normalization)) |
| `sessionId` | present when `session.enabled: true`, derived as described under Session ID derivation |

A body that cannot be parsed as JSON when a `fromBody` extractor is configured, or that is not valid UTF-8 on the raw-body path, is `400 invalid_request`. [Request body](../api/channel-webhook.md#request-body) has the wire detail and [AgentChannel](../../resources/agentchannel.md) the extractor configuration. After normalization the gateway resolves the channel's `agentRef`; an Agent that no longer exists is `502 delivery_failed`.

#### Session ID derivation

If the AgentChannel has `session.enabled: true`, the gateway derives a deterministic `sessionId` from the envelope's `channelId` and `userId`:

```
sessionId = UUIDv5(namespace: <fixed published namespace UUID>, name: channelId + ":" + userId)
```

The namespace is a fixed UUID published as part of the Kaalm API, identical across installations and versions; [POST /v1/message](../api/agent-endpoints.md#post-v1message) states the constant and its stability guarantee. Because the derivation is a pure function of the envelope, the id is stable across gateway replicas and restarts, and the gateway holds no session state. Session expiry and rotation are the agent's responsibility, using its PVC state.

### 6. Async accept (async mode only)

When `spec.webhook.responseMode` is `async`, the gateway answers `202 Accepted` here, with `requestId` and `channelPath` in the body, and runs steps 7 to 10 in the background. The mode and the `channelPath` come from the resolved channel, so this is the earliest point the `202` can be sent. Two checks precede it, and both answer `503 internal_unavailable` with `Retry-After: 5`: the channel's live records must be below [`maxPendingAsyncResponses`](../../resources/agentchannel.md) (default 100), and the empty placeholder ConfigMap `kaalm-async-{requestId}` must be created in `kaalm-system`. A returned `202` therefore always means a queryable polling record exists. The `202` body, the record, the poll endpoint, its authentication, the channel-match assertion, and the 1-hour TTL are specified under [The 202 contract](../api/async-responses.md#the-202-contract) and [Polling fallback](../api/async-responses.md#polling-fallback).

The background pipeline has no sync deadline. As shipped it runs under a fixed 10-minute ceiling, larger than the default budgets it contains: a 2-minute wake, then up to 71 seconds each of delivery retries and callback retries. A `wakeTimeout` large enough to reach the ceiling is cut off there and reported as `wake_timeout`.

### 7. Activator check

If the Agent's phase is `Hibernated` or `Hibernating`, the gateway calls the controller's activator over mTLS, then polls a TCP connect to the Agent Service every two seconds until it succeeds or the Agent's effective `wakeTimeout` elapses (the class default and cap apply, and 2 minutes when neither the Agent nor the class sets it). In sync mode the caller waits through this. An unreachable activator is `controller_unavailable`; an elapsed timeout is `wake_timeout`. The wake sequence and its failure arms are on [The activator](activation-and-activity.md#the-activator).

### 8. Message delivery

The gateway posts the normalized envelope to `POST /v1/message` on the Agent's ClusterIP Service over bidirectional mTLS: it verifies the agent's certificate against the Kaalm CA (`kaalm-ca`, managed by cert-manager, see [TLS on the cluster listener](../listener-tls.md)) and presents its per-Deployment certificate, which the agent checks for a SAN match on the gateway Service DNS ([The runtime contract](../../runtime/contract.md), item 4). Delivery connections are pooled per Agent and reused across messages until idle for the transport's idle timeout. Each TCP connect is bounded by `gateway.agentDeliveryConnectTimeout` and dialed again on a fresh connection while the attempt's `gateway.agentReadTimeout` budget lasts, so a dropped packet costs one connect bound, not a whole attempt.

A failed attempt (a connection error, a non-2xx response, or a 200 with a malformed envelope) is retried on the bounded schedule specified under [The bounded retry schedule](../api/async-responses.md#the-bounded-retry-schedule): 1s, 5s, 25s, four attempts in all, each bounded by `gateway.agentReadTimeout`. An oversized reply is not retried and is `response_too_large`. On exhaustion the outcome is `delivery_failed`, returned inline in sync mode and delivered as an error payload in async mode. The outcome is recorded for [channel health](platform-adapters.md) in either mode.

![Sequence diagram of delivery and response after intake. The webhook caller POSTs to the User Gateway on :8080, which runs the intake checks. In async mode the gateway creates the placeholder ConfigMap and answers 202 with requestId and channelPath. If the Agent is Hibernated or Hibernating the gateway POSTs /v1/activate/{namespace}/{name} to the controller activator on :9443 and polls the Agent Service for reachability up to wakeTimeout. The gateway POSTs /v1/message to the Agent Service with up to four attempts and receives the response envelope. In sync mode it answers 200 within syncDeliveryDeadline; in async mode with a callbackUrl it sends a signed POST with up to four attempts; otherwise it patches the ConfigMap with the payload.](../../diagrams/user-webhook-flow.svg)

### 9. Response (sync mode, default)

The gateway returns the agent's response envelope as the webhook response body. Everything from step 7 on, wake included, runs under `gateway.syncDeliveryDeadline` (default 30s). When the deadline elapses mid-pipeline the gateway answers `504 sync_deadline_exceeded` with `retryable: true`: the failure is timing, not structure, and a fresh attempt within the deadline can succeed. The deadline gives sync callers a fixed upper bound without changing the retry pipeline. It is tighter than the delivery budget and the default `wakeTimeout`, so `delivery_failed` and `wake_timeout` are practically unreachable in sync mode under defaults. [Reachability under default config](../api/channel-webhook.md#reachability-under-default-config) states which mode fits which agent, and [Sync-mode reachability](../api/async-responses.md#sync-mode-reachability) draws the three bounds on one time axis. The status table is under [Response: sync mode](../api/channel-webhook.md#response-sync-mode).

### 10. Response (async mode)

The gateway POSTs the payload, the agent's response or an error payload, to `callbackUrl` when one is configured. Every attempt is signed per [Callback authentication](../api/async-responses.md#callback-authentication) and retried on the same bounded schedule. When no `callbackUrl` is set, or the callback outcome was `rejected`, `exhausted`, or `invalid`, the gateway `Patch`es the payload into the placeholder ConfigMap, where [Polling fallback](../api/async-responses.md#polling-fallback) serves it. Persistence, cleanup, capacity, and the replica-local limitation are specified under [Response persistence](../api/async-responses.md#response-persistence) and [Replica failure](../api/async-responses.md#replica-failure).

#### `callbackUrl` re-validation on delivery

Before each POST attempt to `callbackUrl`, the initial attempt and each retry, the gateway re-resolves the host and re-checks it against the blocked IP ranges from [AgentChannel validation rule 22](../../resources/validation-and-defaulting.md#cross-resource-validation). This defeats DNS rebinding between reconcile-time validation and delivery, but only if the range check and the dial operate on the same address.

The delivery transport therefore uses a custom dialer that resolves the host once, range-checks the resolved IP, and dials that pinned `IP:port`, keeping the URL's hostname for the `Host` header and TLS SNI. The hostname is never handed back to the HTTP client to resolve on its own, because an independent dial-time resolution could be rebound to a blocked address after the check passed. Connections to a callback host are pooled and reused across attempts and messages. A pooled connection was dialed the same way, and the re-resolution and re-check still run before every attempt, so a host that has come to resolve to a blocked range is never posted to, on a new connection or a pooled one.

If the host now resolves to a blocked range (loopback, link-local, RFC1918, unique-local IPv6, cloud metadata, or anything outside `gateway.callbackUrl.allowlist` when set), the attempt is not dialed. The gateway records the `invalid` callback outcome, emits the AgentChannel `Warning` event `reason=CallbackInvalid`, and stores the payload it was about to deliver at the polling endpoint. No `callback_invalid` envelope is ever stored, and no retry is attempted for a host that fails the check; see [Callback failure buckets](../api/async-responses.md#callback-failure-buckets).

---

## TLS and Ingress

The User Gateway listener on `:8080` serves TLS using the same `kaalm-gateway-tls` Certificate as the cluster listener: both listeners share a single cert whose SAN set covers the gateway's in-cluster Service DNS names. No plaintext path exists on the gateway; all webhook, activator, activity, and async-polling traffic is TLS end-to-end.

Port 8080 serves only externally reachable channel traffic, namely webhook intake under `/channels/*` and the async polling fallback under `/v1/channels/responses/*`; every mTLS-authenticated internal endpoint lives on the cluster listener on `:8443`. The split and its reason are stated under [The :8080 listener profile](../overview.md#the-8080-listener-profile).

**Recommended Ingress configuration**: external webhook traffic arrives at a cluster Ingress that terminates TLS with the cluster's public certificate and then connects to the gateway backend. Two Ingress modes are supported; operators pick one:

- **Backend re-encrypt (HTTPS-to-HTTPS)**: the Ingress controller speaks HTTPS to the gateway on port 8080, presenting the Kaalm CA as the backend CA bundle (or disabling verification if the controller trusts cluster-internal names). This is the recommended default because it works with off-the-shelf Ingress controllers (NGINX, Traefik, HAProxy, most cloud LB Ingress classes).
- **TLS pass-through**: the Ingress forwards raw TLS bytes to the gateway without terminating, so the external client speaks TLS directly with the gateway. This preserves end-to-end TLS with the gateway's cert but requires the Ingress controller to support pass-through SNI routing and requires clients to trust the gateway's cert chain. Operators using pass-through must set the Helm value `gateway.externalHostnames` to add the public hostname to the gateway cert's SAN list: the default SAN set covers only in-cluster Service DNS, which would fail verification for an external client dialing the public hostname. See [Helm chart contents](../../operations/deployment.md#helm-chart-contents).

Internal callers inside the cluster verify the gateway's cert against `kaalm-ca` (projected by `trust-manager` into every namespace) when dialing the gateway directly. Cluster-local webhook producers and async-polling callers dial the User listener on `:8080`; the controller's activity fan-out dials the cluster listener on `:8443` (see [Activity tracking API](activation-and-activity.md#activity-tracking-api)).
