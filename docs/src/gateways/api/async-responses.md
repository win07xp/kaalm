# Async webhook responses

When an AgentChannel has `spec.webhook.responseMode: async`, the gateway answers the webhook caller at once and delivers the agent's reply later, by callback or by polling. The agent's implementation is unchanged: it still receives `POST /v1/message` and returns a response envelope. Everything on this page happens gateway-side.

This page is the home of two mechanisms cited across the design: the gateway's [bounded retry schedule](#the-bounded-retry-schedule) and the per-request response ConfigMap described under [Response persistence](#response-persistence).

## The 202 contract

The webhook caller receives an immediate 202 response:

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
| `channelPath` | string | yes | The webhook path of the originating AgentChannel. Callers must preserve this value and pass it (URL-encoded) as the `channelPath` query parameter on poll requests. See [Polling fallback](#polling-fallback). |
| `status` | string | yes | Always `"accepted"` for the immediate 202. |
| `message` | string | no | Human-readable acknowledgement. |

The gateway creates the polling record before it returns the 202, so a returned 202 always means a queryable record exists. A `GET /v1/channels/responses/{requestId}` issued immediately afterward returns `202` until the agent's response or an error payload is ready, then `200`. If the record's `Create` fails (a transient apiserver failure), the inbound caller sees `503` and no `requestId`. Callers must not retain a `requestId` from any non-`202` response. A 202 that is later lost in flight is the separate case described under [Replica failure](#replica-failure).

## Callback delivery and retries

When `spec.webhook.callbackUrl` is configured, the gateway POSTs the payload to it. The payload is the agent's response, or one of the [error payloads](#error-payloads) when the agent never replied:

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

Before every attempt, initial and retry alike, the gateway re-resolves the `callbackUrl` host and checks it against the deny ranges and the configured `gateway.callbackUrl.allowlist`, signs the request with a fresh timestamp per [Callback authentication](#callback-authentication), and POSTs to the checked `IP:port` with the hostname kept for the `Host` header and TLS SNI. The pinned dial is what makes the re-check effective against DNS rebinding; the dialer is specified under [callbackUrl re-validation on delivery](../user/overview.md#callbackurl-re-validation-on-delivery). Every callback delivery ends in one of four outcomes, and every outcome except `delivered` stores the payload for polling.

![Flowchart of async callback delivery: a callbackUrl check, a pre-dial check, then a sign-and-POST loop of up to four attempts whose response decides between delivered, rejected, and retry, with exhausted and invalid as the other exits; every outcome except delivered ends at store for polling.](../../diagrams/async-callback-delivery.svg)

### Callback failure buckets

The four outcomes are the values of the `status` label on `kaalm_channel_callback_total` (see [User Gateway operations](../user/operations.md#observability)).

| Outcome | Trigger | Retried | Stored for polling | Signal |
|---|---|---|---|---|
| `delivered` | HTTP `2xx` | no | nothing | none |
| `rejected` | HTTP `401`, `403`, `404`, `405`, `410`, `415` | no | the payload | `Warning` event, `reason=CallbackRejected` |
| `exhausted` | DNS resolution failure, TCP connect error, TLS handshake failure, read timeout (`gateway.callbackReadTimeout`, default 10s), HTTP `408`, `429`, `422`, or any `5xx`, on all four attempts | 1s, 5s, 25s | the payload | metric only |
| `invalid` | A pre-dial check fails: the host resolves to a blocked range or outside the allowlist, or the `callbackAuth` Secret cannot be read | no | the payload, never a `callback_invalid` envelope | `Warning` event, `reason=CallbackInvalid` |

The retried class covers receivers that are transiently misconfigured: auth-secret rotation drift, an in-progress deploy, or overload. `422` is retried on purpose, because a schema deploy on the receiver can briefly change the body it accepts, and four attempts cover a typical rollout. The terminal class covers receivers that permanently reject the POST: `401`, `403`, `404`, and `410` say the credential or route is wrong, and `405` and `415` say the receiver refuses a JSON POST at that URL. Retrying either would change nothing. The `invalid` outcome never reaches a receiver, so nothing is signed and nothing is POSTed.

Persistent `rejected` or `invalid` outcomes are also reflected in `AgentChannel.status.conditions[type=PlatformConnected]` as `{status: False, reason: CallbackRejected}` or `{status: False, reason: CallbackInvalid}`. The `Warning` event is the per-occurrence signal; the condition is the persistent signal that survives event TTL.

### The bounded retry schedule

Three gateway pipelines share one bounded-retry vocabulary:

| Pipeline | Backoff Helm value | Default schedule | Per-attempt read bound |
|---|---|---|---|
| Agent delivery (`POST /v1/message` to the agent's Service) | `gateway.agentDeliveryRetryBackoff` | 1s, 5s, 25s; 4 attempts total | `gateway.agentReadTimeout` (default 10s) |
| Callback delivery (POST to `callbackUrl`) | `gateway.callbackRetryBackoff` | 1s, 5s, 25s; 4 attempts total | `gateway.callbackReadTimeout` (default 10s) |
| Response `Patch` (payload into the per-request ConfigMap) | reuses `gateway.callbackRetryBackoff` | 1s, 5s, 25s; capped at 4 attempts | not applicable (apiserver write) |

The schedules and the read timeouts are Helm-tunable defaults, not constants; see [Helm chart contents](../../operations/deployment.md#helm-chart-contents). The agent-delivery and callback-delivery pipelines are independent and ship with the same defaults.

A full run of either delivery pipeline takes between about 31s and about 71s of wall-clock. The inter-attempt delays add up to 31s (1 + 5 + 25), and each of the four attempts can add up to 10s more when it uses its full read timeout. The fast case is every attempt failing at once on a connect error or an immediate non-2xx.

An agent-delivery attempt counts as failed on a connection error, a non-2xx response, or a 200 with a malformed envelope (missing or non-string `content`); the agent-side contract is [`POST /v1/message`](agent-endpoints.md#post-v1message). The pipeline runs identically in sync and async modes, and its outcome is recorded in AgentChannel status conditions in either mode. On exhaustion:

- **Async mode**: the gateway delivers a `delivery_failed` error payload to `callbackUrl` (if configured, with the callback pipeline's own retries) or stores it at the polling endpoint under the original `requestId`.
- **Sync mode**: the gateway returns `502 Bad Gateway` with a `delivery_failed` error envelope. A sync caller whose HTTP timeout is shorter than the retry budget sees only its own timeout, and the 502 lands on a closed connection.

The agent-delivery pipeline reuses the same `messageId` across its attempts, so an agent that started work on an earlier attempt can receive the same message again. Agents deduplicate on `messageId`; see [The runtime contract](../../runtime/contract.md). A caller-side resubmission is different and gets a fresh `messageId`.

[`POST /v1/task/complete`](task-complete.md) uses a separate, tighter schedule for `StalePodCompletion` (100ms, 500ms, 2s; 3 attempts). It is not this schedule.

### Sync-mode reachability

In sync mode, `gateway.syncDeliveryDeadline` (Helm value, default 30s) bounds the caller-facing wall-clock. The clock starts at inbound webhook acceptance and includes activator wake time, delivery retries, and agent processing. When elapsed time would exceed the deadline, the gateway short-circuits the in-progress request, even mid-retry, with `504` carrying `error.type: sync_deadline_exceeded` and `retryable: true`. Async mode applies no deadline: the full retry budget runs, and callback and polling are receiver-driven.

![Timing diagram with three lanes on one axis from inbound webhook acceptance. The sync deadline lane fires 504 sync_deadline_exceeded at 30 seconds. The agent delivery lane can produce 502 delivery_failed between 31 and 71 seconds. The wake lane produces 504 wake_timeout at 120 seconds. The two later outcomes are grey because the deadline fires first.](../../diagrams/sync-reachability-timeline.svg)

| Outcome | Earliest | Latest | Sync mode under defaults | Async mode |
|---|---|---|---|---|
| `504 sync_deadline_exceeded` | 30s | 30s | fires | never (no deadline) |
| `502 delivery_failed` | about 31s | about 71s | unreachable: the deadline fires first | reachable, as an error payload |
| `504 wake_timeout` | 120s | 120s | unreachable: the deadline fires first | reachable, as an error payload |

`502 delivery_failed` becomes reachable in sync mode only when `syncDeliveryDeadline` is raised above the delivery budget. `wakeTimeout` is the Agent's own value or the class default, clamped to the class cap, and 120s when neither sets it (see [AgentClass](../../resources/agentclass.md)). For which channels belong on sync mode, and what a persistent `sync_deadline_exceeded` means, see [Reachability under default config](channel-webhook.md#reachability-under-default-config).

## Response persistence

Each async request is backed by a ConfigMap named `kaalm-async-{requestId}` in `kaalm-system`. The receiving replica creates it as an empty placeholder at 202-acceptance time, so the polling endpoint can answer `202` for in-flight requests. The same replica later `Patch`es the payload into it, but only when the payload will not be delivered by callback: no `callbackUrl` is configured, or the callback outcome was `rejected`, `exhausted`, or `invalid`.

A replica creates the placeholder only while the originating AgentChannel is live. Once a replica observes the channel move to `status.phase: Terminating` in its watch, it stops creating `kaalm-async-*` records for that channel and rejects the inbound webhook. This write gate lets the channel-delete finalizer sweep run once without racing an in-flight write.

**Labels and expiry.** Each ConfigMap is labeled with `kaalm.io/channel-namespace` and `kaalm.io/channel-name` to identify the originating AgentChannel, and carries its expiry in the `kaalm.io/expires-at` annotation. The expiry is set at placeholder creation, 1 hour from 202-acceptance, and is not reset by the payload `Patch`. The gateway enforces it on every poll read; see [TTL and retention](#ttl-and-retention).

**Cleanup.** These ConfigMaps carry no ownerRef: an ownerReference from `kaalm-system` to an AgentChannel in a user namespace is invalid, and the GC would delete the dependent at once. Why, and what the labels do instead, is specified under [Async response ConfigMaps are swept by label, not owned](../../runtime/child-resources.md#async-response-configmaps-are-swept-by-label-not-owned). Two controller-side paths delete them. For live channels, the [AgentChannelReconciler](../../controller/reconcilers.md#agentchannelreconciler) lists by the channel labels on every reconcile pass and deletes records whose `kaalm.io/expires-at` is in the past, so a record lingers in storage at most one requeue interval (one minute) past its expiry. On channel deletion, the [AgentChannel finalizer](../../controller/finalizers.md#agentchannel) waits for the gateway's disconnect confirmation and then deletes every record carrying that channel's labels, expired or not. The sweep is required because the expiry prune runs only for live channels; without it a deleted channel's records would be orphaned.

**RBAC.** The gateway's write verbs on this surface are `create` (the placeholder) and `patch` (the payload); `delete` is deliberately absent, so all cleanup is controller-side. See [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions).

**Capacity and the pending cap.** Live records accumulate at roughly the channel's async request rate multiplied by the 1-hour TTL, so the store targets low-to-moderate async rates. The channel labels double as the counting key for [`maxPendingAsyncResponses`](../../resources/agentchannel.md) (default 100): the gateway counts a channel's live records from its `kaalm-system` ConfigMap informer and rejects new async requests with HTTP `503` at the limit. High-QPS channels belong on sync mode fronted by an external queue.

**Storage format.** The payload is stored as a text value under the ConfigMap's `data` field (a JSON envelope), never `binaryData`: base64 would inflate a maximum-size (900 KiB) payload past the Kubernetes object cap of about 1 MiB.

**Replica-agnostic reads.** Any gateway replica serves poll requests by reading this ConfigMap. There is no in-memory routing and no per-replica state on the read path. The delivery and callback pipelines are replica-local; see [Replica failure](#replica-failure).

## Callback authentication

Every callback POST is signed using the AgentChannel's `spec.webhook.callbackAuth`, which [rule 25](../../resources/validation-and-defaulting.md#cross-resource-validation) requires whenever `callbackUrl` is set. The signing contract mirrors the [poll authentication](#poll-authentication) contract: same auth types, same `X-Kaalm-Timestamp` header, with a body-hash component added because callbacks have a body where polls do not. The signing material is loaded from the Secret referenced by `callbackAuth.secretRef` (or `callbackAuth.hmac.secretRef`), read under the per-channel scoped Role created by the [AgentChannelReconciler](../../controller/reconcilers.md#agentchannelreconciler).

- **`callbackAuth.type: bearer`**: the gateway sends `Authorization: Bearer <secret>` on the callback POST.
- **`callbackAuth.type: hmac`**: the gateway computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}\n{sha256(body)}"` (unix seconds, no trailing newline; the body hash is the lowercase hex sha256 of the raw POST body bytes). The hex-encoded digest goes in the configured `callbackAuth.hmac.header`; the timestamp goes in `X-Kaalm-Timestamp`. Receivers should reject timestamps with skew greater than 300s against their own wall clock.

The gateway signs every attempt with a fresh timestamp, so a delayed retry never carries a stale `X-Kaalm-Timestamp`. The contract applies to success payloads and to every error payload delivered by callback, so a forged POST cannot impersonate a delivery error. The `invalid` outcome is outside the contract by construction: the URL is rejected before dial, so there is nothing to sign.

## Failure modes

Two failures lose an in-flight async request silently, and they are indistinguishable on the wire. In both, the placeholder record exists but never receives its `Patch`. Pollers observe `202` until the 1-hour TTL flips the record to `404`, and no error envelope is ever stored. The [poll record lifecycle](#read-semantics) figure draws both as the red edge from Placeholder to Expired.

Callers should treat an unanswered poll past a sane bound (5 to 10 minutes for happy-path async, longer for known-slow agents) as failed, and resubmit the original webhook. The new submission gets a new `requestId` and is independent of the lost one. There is no work-claim and no peer-replica takeover: this is the async-mode trade-off for not running a durable queue, and the same trade-off bounds throughput (see the capacity note under [Response persistence](#response-persistence)).

### Replica failure

The placeholder ConfigMap is durable in etcd, but the per-request delivery pipeline (agent POST, retries, callback dispatch, retries) lives in memory on the replica that accepted the inbound webhook. If that replica dies between returning `202` and patching the ConfigMap (rolling restart, node drain, OOM kill, crash), the in-flight request is dropped. There is no per-request signal on the operator side.

### Response-patch failure

The `Patch` is an apiserver write and can fail transiently: apiserver unavailable, etcd unreachable, a `Patch` conflict, or RBAC drift on the gateway's `kaalm-system` ConfigMap surface. The gateway retries it on the [bounded retry schedule](#the-bounded-retry-schedule), capped at 4 attempts. If all fail, the in-memory payload is dropped, the gateway increments `kaalm_channel_async_patch_failed_total` (labeled by namespace), and logs at error level. The counter is the only operator-side signal; see [Recommended alerts](../../operations/observability.md#recommended-alerts).

## Error payloads

When the agent never replies, the gateway delivers an error payload instead of a response. It goes to `callbackUrl` on the same schedule as a success payload, and it is stored at the polling endpoint on the same 1-hour TTL when no `callbackUrl` is configured or the callback outcome was `rejected`, `exhausted`, or `invalid`.

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

| `error.type` | `message` | `retryable` | Fires when | Sync-mode form |
|---|---|---|---|---|
| `delivery_failed` | `Failed to deliver message to agent after 4 attempts` | `false` | The initial attempt and all 3 retries of `POST /v1/message` fail | `502`, but usually pre-empted by `504 sync_deadline_exceeded` (see [Sync-mode reachability](#sync-mode-reachability)) |
| `wake_timeout` | `Agent did not become ready within wakeTimeout (120s)` | `false` | The agent is `Hibernated` or `Hibernating` and does not become Ready within `wakeTimeout` | `504`, but pre-empted by `504 sync_deadline_exceeded` under defaults |
| `controller_unavailable` | `Controller activator endpoint unreachable; wake could not be triggered` | `true` | A message arrives for a `Hibernated` agent and the gateway cannot reach the controller's activator endpoint | `504` with `Retry-After: 5` |
| `response_too_large` | `Agent response body exceeded gateway.maxResponseBodyBytes (900 KiB); externalize large outputs and reference by URL` | `false` | The agent's response body exceeds `gateway.maxResponseBodyBytes` (default 900 KiB) | `413` |

The sync-mode status codes and envelope are specified in [User Gateway error responses](errors.md#user-gateway-error-responses).

**`delivery_failed`** is not retryable because the gateway has already made 4 attempts over about 31s to 71s. A failure that survives that budget is usually structural: a broken image, an agent crash loop, repeated 5xx, or a misconfigured per-Agent NetworkPolicy on the message path. Investigate `AgentChannel.status.conditions[type=PlatformConnected]` and the Agent Pod status rather than retrying at the caller.

**`wake_timeout`** is not retryable because exceeding `wakeTimeout` usually means a Pod-startup problem that repeats on retry: an image pull failure, an init container crash, OOM, or a bad spec. Tenants with transient timeouts should raise `wakeTimeout` instead of retrying.

**`controller_unavailable`** is retryable because the wake never reached the controller and the agent is still `Hibernated`, so a fresh attempt starts clean once the controller recovers. The triggers are a connection error, a 5xx after retry, or an mTLS handshake or SAN authorization failure. The `Retry-After: 5` on the sync form is fixed at 5 seconds, sized to typical controller restart and probe intervals; see [Failure modes](../user/operations.md#failure-modes). The mTLS and SAN cases count as transient because at runtime they arise from a peer mid-reload of a rotated leaf certificate. Persistent ones are deployment-time problems: both certificates come from the same `kaalm-ca-issuer`, and the chart install fails fast if the issuer is missing. See [Internal endpoint authentication](../../security/rbac.md#internal-endpoint-authentication) and [In-cluster TLS](../../security/tls.md#in-cluster-tls).

**`response_too_large`** applies in both modes and exists for two reasons: async responses are persisted to ConfigMaps, which are capped near 1 MiB, and every webhook response is buffered in gateway memory before forwarding, so an unbounded reply could OOM the gateway. Agents that need to return large outputs should externalize them and reference them by URL. The cap is specified under [Request flow](../user/overview.md#request-flow).

**`callback_invalid`** is not an error payload. It is the `invalid` callback outcome described under [Callback failure buckets](#callback-failure-buckets): the `callbackUrl` fails a pre-dial check, nothing is signed or POSTed, and the payload that was about to be delivered (the agent's response, or one of the error payloads above) is stored for polling instead. Polling callers never see a `callback_invalid` envelope. The `Warning` event with `reason=CallbackInvalid` is the only per-occurrence operator signal, and the `PlatformConnected` condition carries `reason: CallbackInvalid` when it persists. The deny ranges are loopback, link-local, RFC1918, unique-local IPv6, and cloud metadata, checked together with `gateway.callbackUrl.allowlist`; loopback, link-local, and cloud-metadata targets are refused even when allowlisted. The reconcile-time half of the same check is [rule 22](../../resources/validation-and-defaulting.md#cross-resource-validation).

## Polling fallback

**`GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}`**

**Caller.** The webhook caller, or any holder of the originating AgentChannel's `webhook.auth` credentials. The endpoint is served over HTTPS on the User Gateway listener (port 8080, TLS using `kaalm-gateway-tls`, the same certificate as the cluster listener), and external callers reach it through the cluster Ingress that fronts port 8080; see [TLS and Ingress](../user/overview.md#tls-and-ingress).

**Request.** `requestId` is the opaque identifier from the 202 response. `channelPath` is the webhook path of the originating AgentChannel, exactly the `channelPath` value from the 202 response body, URL-encoded. The gateway uses it to find the AgentChannel whose auth configuration authenticates the request. Callers must preserve both values verbatim and must not construct either.

**Response.** `200` with the callback payload (success or error) as the body, `202` with a `Retry-After` header while the agent has not replied, or one of the error codes under [Poll status codes](#poll-status-codes). The endpoint is read-only: polling does not affect delivery, and a poll never resets the TTL.

### Poll authentication

Poll requests carry no body, so the auth contract differs from the inbound webhook in what is signed:

- **`auth.type: bearer`**: the poll presents the same bearer token in `Authorization: Bearer ...` as the inbound webhook. The token is read from the Secret referenced by `AgentChannel.spec.webhook.auth.secretRef` (`name`, `key`); there is no inline token field on the AgentChannel spec. Bearer polls carry no timestamp and are not subject to the skew check.
- **`auth.type: hmac`**: the poll computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}"` (unix seconds, no trailing newline). It sends the bare lowercase hex digest in the configured `header`, the same header name as the inbound webhook, and the timestamp in a dedicated `X-Kaalm-Timestamp` header. `auth.hmac.signaturePrefix` and `auth.hmac.encoding` exist for inbound third-party-sender compatibility and do not apply here: polling is a Kaalm-canonical surface and always uses bare hex with no prefix. Polls with clock skew greater than 300s against the gateway's wall clock are rejected with `401 Unauthorized`.

`401 Unauthorized` is returned on any auth failure (missing or malformed credentials, signature mismatch, clock skew) and on a well-formed but unregistered `channelPath`. The gateway treats an unknown channel as an auth failure rather than a 404 so that the endpoint does not reveal which webhook paths exist, and by extension which tenant namespaces are hosted, since paths are prefixed `/channels/{namespace}/`. The [channel-match assertion](#channel-match-assertion) and the [threat-model row on cross-channel response retrieval](../../security/threat-model.md) follow the same rule. When the credentials authenticate correctly against `channelPath` but the stored `requestId` was originated by a different channel, the response is `404 Not Found`, the same code as an unknown `requestId`.

### Channel-match assertion

After authenticating the caller against the AgentChannel identified by `channelPath`, and before serving any response (including the empty-placeholder `202`), the gateway asserts that the `kaalm-async-{requestId}` ConfigMap's `kaalm.io/channel-namespace` and `kaalm.io/channel-name` labels match that same AgentChannel.

Without this check, a caller holding channel A's credentials plus any channel B `requestId` could tell `202` (placeholder present) from `404` (no record) and learn whether a B request is in flight, and could read B's payload once stored. `requestId` values are UUIDs, not secrets, so the labels are the only thing that binds a record to its channel. On the wire a mismatch is indistinguishable from an unknown `requestId`: both return `404 Not Found`, whether the record holds a placeholder or a patched payload. Mismatches are logged at the gateway with `reason=ChannelMismatch`.

### Read semantics

Any gateway replica accepts a poll and reads the `kaalm-async-{requestId}` ConfigMap in `kaalm-system`. With the channel-match assertion passing, `200` is returned when the payload field is present and `202` when it is not. An absent ConfigMap, a mismatched one, or one past its TTL returns `404`.

![State diagram of one polling record with four states, each carrying the status a poll returns: Absent 404, Placeholder 202, Patched 200, Expired 404. Create moves Absent to Placeholder, Patch moves Placeholder to Patched, one hour from Create moves either to Expired, prune moves Expired to Absent, and the finalizer sweep moves any state to Absent. The edge from Placeholder to Expired is red.](../../diagrams/async-poll-record-lifecycle.svg)

| State | Poll returns | Entered by | Left by |
|---|---|---|---|
| Absent | `404` | The reconciler's expiry prune, or the finalizer sweep from any state | The placeholder `Create`, synchronously before the inbound 202 |
| Placeholder | `202` with `Retry-After` | The placeholder `Create` | The payload `Patch`, or 1 hour from `Create` when the `Patch` never lands (the red edge) |
| Patched | `200` with the payload | The payload `Patch` | 1 hour from `Create` |
| Expired | `404` | 1 hour from `Create`, computed on every poll from `creationTimestamp` | The reconciler's expiry prune, or the finalizer sweep |

The red edge is both silent-loss failures under [Failure modes](#failure-modes). The finalizer sweep is not expiry-gated: on channel deletion it removes a Placeholder or Patched record still inside its TTL. Expired is a state the poller computes, not one the reconciler sets: the ConfigMap can still sit in etcd, and pruning is storage cleanup only.

### Polling cadence

Callers should poll no faster than every 2 seconds, with exponential backoff to about 30 seconds between attempts. Repeated `202` responses mean the agent has not replied; polling faster does not accelerate delivery, because the agent's response is independent of poll arrival.

The gateway makes the cadence machine-readable: every `202` carries `Retry-After` (integer delta-seconds, RFC 7231 § 7.1.3), computed from the elapsed time `t = now - placeholder.creationTimestamp`:

| Elapsed time `t` | `Retry-After` |
|---|---|
| `t < 2s` | 2 |
| `2s <= t < 6s` | 4 |
| `6s <= t < 14s` | 8 |
| `14s <= t < 30s` | 16 |
| `t >= 30s` | 30 |

A caller that always waits the hinted delay follows this curve without rolling its own backoff. Every replica reads the same ConfigMap, so a caller round-robining across replicas sees the same curve as one stuck to a single replica, with no per-replica state and no etcd write per poll. Per-caller rate limiting is not enforced at the gateway; operators that need a hard ceiling should rate-limit at the cluster Ingress fronting `:8080`, as for inbound webhooks (see [Scoping summary](../../concepts/vision-and-scope.md#scoping-summary)).

### Poll status codes

| Status | Retryable | Meaning |
|---|---|---|
| 200 | n/a | Response or error payload available; the body is the callback payload |
| 202 | n/a | Request accepted; agent response not available. `Retry-After` carries the cadence hint from [Polling cadence](#polling-cadence) |
| 400 | no | Missing or malformed `channelPath` query parameter |
| 401 | no | Auth failed (missing or malformed credentials, signature mismatch, clock skew over 300s on HMAC polls), or `channelPath` not registered to any AgentChannel |
| 404 | no | Unknown `requestId`, response expired (1-hour TTL from 202-acceptance), or the stored `requestId` originated by a different channel than `channelPath`, whether the record holds a placeholder or a payload |

The `401` response carries the structured `{ "error": { "type", "message", "retryable" } }` envelope from [User Gateway error responses](errors.md#user-gateway-error-responses), with the same `unauthorized` type and the same generic message as the inbound-webhook `401`, for the reason given under [Poll authentication](#poll-authentication). The `400` response carries the same envelope with `error.type: invalid_request`, the type name from the [LLM Gateway error responses](errors.md#llm-gateway-error-responses) table. `404` has an empty body.

### TTL and retention

Stored responses are retained for 1 hour from 202-acceptance. The clock starts when the placeholder ConfigMap is created and is not reset by the payload `Patch`, so the polling window for a `requestId` is 1 hour regardless of how long the agent takes to reply. The window left after the payload lands is therefore `1h - (wake + delivery + agent processing + callback retries)`.

The gateway enforces the TTL on every poll read: `404` is returned when `now - placeholder.creationTimestamp > 1h`, whether or not the reconciler has pruned the ConfigMap yet. Pruning is storage cleanup and never what a poller observes.

Neither delivery path is a durable queue. Callback delivery is best-effort within its retry budget, and polling is the receiver-driven fallback within the same 1-hour window. A receiver that misses the callback can still recover the payload by polling before the TTL; past it, the payload is gone. The TTL is fixed and deliberately not Helm-tunable. Agents that need to return results past 1 hour should externalize the result and reference it by URL, the same guidance as for [`response_too_large`](#error-payloads).
