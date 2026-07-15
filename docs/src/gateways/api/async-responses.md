# Async Webhook Responses

When an AgentChannel has `spec.webhook.responseMode: async`, the gateway handles the asynchronous response flow. The agent's implementation is unchanged. It still receives `POST /v1/message` and returns a response envelope. The async behavior is entirely gateway-side.

This page is the canonical reference for two mechanisms cited across the design: the gateway's bounded retry schedule (see [The Bounded Retry Schedule](#the-bounded-retry-schedule)) and the per-request response ConfigMap (see [Response Persistence](#response-persistence)).

## The 202 Contract

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
| `channelPath` | string | yes | The webhook path of the originating AgentChannel. Callers must preserve this value and pass it (URL-encoded) as the `channelPath` query parameter on poll requests. See [Polling Fallback](#polling-fallback) below. |
| `status` | string | yes | Always `"accepted"` for the immediate 202. |
| `message` | string | no | Human-readable acknowledgement. |

The gateway creates the polling record before returning this response. A `GET /v1/channels/responses/{requestId}` issued immediately afterward will therefore return `202` until the agent's response or a delivery error is ready, then `200`. A `5xx` from the inbound webhook means the polling record was not created; callers must not retain the `requestId` from a non-`202` response. See [Replica Failure](#replica-failure-v1-limitation) below for the orthogonal failure mode where the 202 succeeded but the pipeline was later lost.

The polling record is a per-request placeholder ConfigMap, and its `Create` runs synchronously before the 202 is returned. If that `Create` fails (a transient apiserver failure), the inbound caller sees `503`, not a callback or polling envelope, since neither has been wired up at that point. A returned `202` therefore always implies a queryable polling record exists. Full mechanics in [Response Persistence](#response-persistence).

## Callback Delivery and Retries

When `spec.webhook.callbackUrl` is configured, the gateway POSTs the agent's response to it:

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

The gateway retries callback delivery up to 3 times with exponential backoff (1s, 5s, 25s; 4 attempts total). If no `callbackUrl` is configured on the AgentChannel, all retries fail, or callback delivery is rejected as `callback_invalid` (see [Error Payloads](#error-payloads)), the response is stored at the polling endpoint under the original `requestId`.

The polling-record TTL is bounded at 1 hour from 202-acceptance, meaning the placeholder `Create` timestamp, not the time the payload is patched in. The remaining retrievable window after the patch is therefore `1h - (delivery time + agent processing + callback-retry budget)`. Error-payload storage shares the same TTL semantics (see [Error Payloads](#error-payloads)).

![Flowchart of async callback delivery. A ready payload with no callbackUrl configured goes straight to the polling store. Otherwise the gateway re-resolves the callbackUrl host against the deny ranges and allowlist before dialing; a host that now fails is BYPASSED as callback_invalid with no retry and no fresh envelope, emitting a CallbackInvalid Warning while the underlying payload still reaches polling. A host that passes is signed per callbackAuth with a fresh timestamp on every attempt and POSTed to the pinned IP:port, and the attempt lands in one of three buckets: delivered on 2xx, TERMINAL on 401, 403, 404, 405, 410 or 415 with a CallbackRejected Warning and no retry, or RETRIED on connect errors, TLS failures, read timeouts, 408, 429, 422 and 5xx, which backs off 1s, 5s and 25s for 4 attempts total. Every non-delivered path converges on patching the payload into the per-request ConfigMap, which has its own 4-attempt budget and drops the payload silently if exhausted.](../../diagrams/async-callback-delivery.svg)

Reading the diagram: the three buckets differ in *behavior*, not in destination. `RETRIED` and `TERMINAL` both end at the polling store and differ only in whether the retry budget is spent first. `BYPASSED` is the asymmetric one: it is the only arm where the error type that triggers it (`callback_invalid`) is never the error type that gets stored.

### The Bounded Retry Schedule

Three gateway pipelines share one bounded-retry vocabulary:

| Pipeline | Backoff Helm value | Default schedule | Per-attempt read bound |
|---|---|---|---|
| Agent delivery (`POST /v1/message` to the agent's Service) | `gateway.agentDeliveryRetryBackoff` | 1s, 5s, 25s; 4 attempts total | `gateway.agentReadTimeout` (default 10s) |
| Callback delivery (POST to `callbackUrl`) | `gateway.callbackRetryBackoff` | 1s, 5s, 25s; 4 attempts total | `gateway.callbackReadTimeout` (default 10s) |
| Response `Patch` (payload into the per-request ConfigMap) | reuses `gateway.callbackRetryBackoff` | 1s, 5s, 25s; capped at 4 attempts | not applicable (apiserver write) |

The backoff schedules and the 10s read timeouts are Helm-tunable defaults, not constants; see [Helm Chart Contents](../../operations/deployment.md#helm-chart-contents). The agent-delivery and callback-delivery pipelines are independent. They merely ship with the same default schedule and may diverge in future tuning. The two read timeouts also share the same 10s default.

**Arithmetic.** A full run of either delivery pipeline costs ~31s of inter-attempt delay (1+5+25) plus up to ~40s of per-attempt cost when every attempt hits its 10s read timeout. Worst-case wall-clock is ~71s; best-case is ~31s when each attempt fails fast (connect error / immediate non-2xx).

**Agent-delivery failure signals.** An agent-delivery attempt counts as failed on a connection error, a non-2xx response, or a 200 with a malformed envelope (missing or non-string `content`); see the agent-side contract at [`POST /v1/message`](agent-endpoints.md#post-v1message).

**Both modes, recorded either way.** The agent-delivery pipeline runs identically in sync and async modes (see [Activator](../user/activation-and-activity.md#the-activator)), and its outcomes are recorded in AgentChannel status conditions in either mode. On exhaustion:

- **Async mode**: the gateway delivers a `delivery_failed` error payload to `callbackUrl` (if configured, with the callback pipeline's own retries) or stores it at the polling endpoint under the original `requestId`.
- **Sync mode**: the gateway returns `502 Bad Gateway` with a `delivery_failed` error envelope. Sync callers whose HTTP timeouts are shorter than the cumulative retry budget see only their own timeout; the 502 lands on a closed connection.

**`messageId` redelivery.** The agent-delivery pipeline reuses the same `messageId` across its attempts. If an earlier attempt actually reached the agent and started side-effecting work but the gateway's read of the response failed, the next retry redelivers the same `messageId`. Caller-side retries are different: a resubmitted webhook gets a fresh `messageId`. The pipeline applies to every agent, hibernated or not, so all agents must deduplicate on `messageId`; see the [runtime contract](../../runtime/contract.md).

**Not this schedule.** [`POST /v1/task/complete`](task-complete.md) uses a separate, much tighter bounded backoff for `StalePodCompletion` (100ms, 500ms, 2s; 3 attempts). Do not conflate the two.

### Sync-Mode Reachability

In sync mode, `gateway.syncDeliveryDeadline` (Helm value, default 30s) bounds the total caller-facing wall-clock. The clock starts at inbound webhook acceptance and includes activator wake time, delivery retries, and agent processing. When elapsed time would exceed the deadline, the gateway short-circuits the in-progress request, even mid-retry, with `504` carrying `error.type: sync_deadline_exceeded` and `retryable: true`. The failure is timing-related, not structural. Async mode applies no such deadline: the full retry budget runs to completion, and callback and polling are receiver-driven, not bounded by the original caller's HTTP timeout.

![Timing diagram on a single time axis starting at inbound webhook acceptance. The sync caller's request is in flight for the first 30 seconds, at which point syncDeliveryDeadline fires 504 sync_deadline_exceeded. The agent-delivery lane runs its four attempts with 1s, 5s and 25s backoff, and 502 delivery_failed can only land in a band from about 31 seconds (every attempt failing fast) to about 71 seconds (every attempt burning its full 10s agentReadTimeout). The wake lane waits for Pod Ready until 504 wake_timeout at 120 seconds. Everything after the 30-second mark is shaded as unreachable on the sync path under defaults, because even the earliest possible delivery_failed arrives after the caller has already received sync_deadline_exceeded.](../../diagrams/sync-reachability-timeline.svg)

Under default configuration the deadline (30s) is tighter than both the delivery retry budget (~31s to ~71s wall-clock) and the default `wakeTimeout` (120s, which exceeds the deadline by 4x). So `502 delivery_failed` and `504 wake_timeout` are practically unreachable on the sync path under defaults; `504 sync_deadline_exceeded` fires first. A sync caller under defaults will never observe `wake_timeout` and instead sees `sync_deadline_exceeded` mid-wake. `502 delivery_failed` becomes reachable in sync mode only when `syncDeliveryDeadline` is raised above the delivery-retry budget.

This is intentional positioning. Sync mode is for fast webhooks where the agent is `Running` (not hibernated) and replies within seconds. Channels backing hibernated agents (`hibernationEnabled: true`), slow-startup agents, or known-long agent processing should use `responseMode: async` with `callbackUrl` or polling. There the deadline does not apply, the full retry and wake budgets are reachable, and `delivery_failed` and `wake_timeout` are delivered via the async error-payload schemas in [Error Payloads](#error-payloads).

`retryable: true` on `sync_deadline_exceeded` is best-effort guidance for the happy-path case: transient slowness, where a fresh attempt within the deadline may succeed. Persistent `sync_deadline_exceeded` on a channel indicates a structural problem at the agent (crash loop, broken image, slow startup). The channel should switch to `responseMode: async` to surface diagnosable error types (`delivery_failed`, `wake_timeout`) and consult `AgentChannel.status.conditions[type=PlatformConnected]` for the cause.

`wakeTimeout` defaults to 120s (`2m`), inherited from `AgentClass.spec.lifecycle.defaultWakeTimeout` and capped by `maxWakeTimeout`; see [AgentClass](../../resources/agentclass.md).

## Response Persistence

Each async request is backed by a ConfigMap named `agentry-async-{requestId}` in `agentry-system`. The receiving replica creates it as an empty placeholder at 202-acceptance time, so the polling endpoint can return `202` for in-flight requests. The replica later `Patch`es the same ConfigMap with the agent's response (or an error envelope), but only when the response will not be delivered via callback: either no `callbackUrl` is configured, or the bounded callback-retry schedule was exhausted.

**Labels and expiry.** Each ConfigMap is labeled with `agentry.io/channel-namespace` and `agentry.io/channel-name` to identify the originating AgentChannel, and carries its expiry in the `agentry.io/expires-at` annotation. The expiry is set at placeholder creation (1 hour from 202-acceptance) and is not reset by the payload `Patch`.

**Read-side TTL enforcement.** The gateway enforces the 1-hour TTL on every poll read: `404` is returned when `now - placeholder.creationTimestamp > 1h`, regardless of whether the reconciler has yet pruned the ConfigMap. Reconciler pruning is storage cleanup only. It is not the mechanism that drives the `200`/`202` to `404` transition observed by polling callers.

**Why no ownerRef.** A cross-namespace ownerReference from these ConfigMaps back to the AgentChannel is invalid in Kubernetes. The AgentChannel lives in a user namespace while the ConfigMaps live in `agentry-system`, and the GC would resolve the owner in the dependent's namespace, treat it as missing, and delete the dependent immediately, emitting an `OwnerRefInvalidNamespace` event. These ConfigMaps therefore carry no ownerRef at all. Linkage is by the channel labels, and the label linkage plus reconciler sweep is the only cleanup path.

**Reconciler pruning (live channels).** The [AgentChannelReconciler](../../controller/reconcilers.md#agentchannelreconciler) prunes expired ConfigMaps on every reconcile pass, listing by the label selector `agentry.io/channel-namespace={ns},agentry.io/channel-name={name}` and deleting entries whose `agentry.io/expires-at` annotation is in the past. Worst-case lingering in storage is bounded by the reconciler's requeue interval (default 5 minutes) past the annotated expiry.

**Finalizer sweep (channel deletion).** When an AgentChannel is deleted, its finalizer sweeps all of that channel's `agentry-async-*` ConfigMaps in one shot. The handoff, detailed in [Finalizers](../../controller/finalizers.md):

1. The reconciler sets `status.phase = Terminating` on the AgentChannel.
2. The gateway sees the phase change via its watch and drops the platform connection.
3. The gateway writes an `agentry.io/channel-disconnected: "true"` annotation on the AgentChannel to confirm disconnection.
4. The reconciler waits for the annotation, with a bounded timeout of 30s if the gateway is unavailable.
5. The reconciler deletes all matching `agentry-async-*` ConfigMaps in one shot.
6. The reconciler removes the finalizer and the resource is deleted.

Running the sweep after the disconnect confirmation (step 4) eliminates the race where in-flight async-response writes from gateway replicas could orphan ConfigMaps created after the sweep. The sweep is load-bearing: the reconciler's expiry prune runs only for live channels, so without the sweep a deleted channel's ConfigMaps would be orphaned indefinitely. Nothing prunes them, and the 1-hour annotation is only enforced when serving reads.

**RBAC shape.** Storing these ConfigMaps in `agentry-system`, where the gateway already has the ConfigMap access it needs, is why no additional per-channel Role is required for async response writes. The gateway's write verbs on this surface are `create` (the placeholder) and `patch` (the payload); `delete` is deliberately absent. Reads (`get`, `list`, `watch`) are shared with the gateway's other `agentry-system` ConfigMap uses. All cleanup is controller-side: the reconciler's expiry prune and the finalizer sweep. See [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions).

**Capacity and the pending cap.** Live records accumulate at roughly the channel's async request rate multiplied by the 1-hour TTL, so the store targets low-to-moderate async rates. The same channel labels double as the counting key for [`maxPendingAsyncResponses`](../../resources/agentchannel.md) (default 100): the gateway counts a channel's live `agentry-async-{requestId}` ConfigMaps via its `agentry-system` ConfigMap informer, using that label selector, and rejects new async requests with HTTP `503` when the count is at the limit. High-QPS channels belong on sync mode fronted by an external queue.

**Storage format.** The payload is stored as a text value under the ConfigMap's `data` field (a JSON envelope), never `binaryData`: base64 would inflate a maximum-size (900 KiB) payload past the ~1 MiB Kubernetes object cap.

**Replica-agnostic reads.** Any gateway replica can serve poll requests by reading this ConfigMap. There is no in-memory routing and no per-replica state on the read path. The delivery and callback pipelines themselves are replica-local; see [Replica Failure](#replica-failure-v1-limitation) below.

## Callback Authentication

Every callback POST is signed using the AgentChannel's `spec.webhook.callbackAuth`, which is required by [Cross-Resource Validation rule 25](../../resources/validation-and-defaulting.md#cross-resource-validation) whenever `callbackUrl` is set. The signing contract mirrors the [polling endpoint's caller-auth contract](#polling-fallback) below: same auth types, same `X-Agentry-Timestamp` header, with a body-hash component added because callbacks have a body where polls do not. The signing material is loaded from the Secret referenced by `callbackAuth.secretRef` (or `callbackAuth.hmac.secretRef`), held by the gateway via the per-channel scoped Role created by the [AgentChannelReconciler](../../controller/reconcilers.md#agentchannelreconciler).

- **`callbackAuth.type: bearer`**: the gateway sends `Authorization: Bearer <secret>` on the callback POST.
- **`callbackAuth.type: hmac`**: the gateway computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}\n{sha256(body)}"` (unix seconds, no trailing newline; the body hash is the lowercase hex sha256 of the raw POST body bytes). The hex-encoded digest goes in the configured `callbackAuth.hmac.header`; the timestamp goes in `X-Agentry-Timestamp`. Receivers should reject timestamps with skew greater than 300s against their own wall clock.

This contract applies uniformly to success payloads and to every error payload delivered via callback (`delivery_failed`, `wake_timeout`, `controller_unavailable`, `response_too_large`). Error payloads are signed identically, so a forged POST cannot impersonate a delivery error. `callback_invalid` is deliberately excluded from the callback-signing contract: the URL is rejected before dial, so there is no callback target to sign for. The underlying payload (the agent's response, or whatever error envelope was about to be delivered) is stored at the polling endpoint instead; see [`callback_invalid`](#error-payloads).

## Failure Modes

### Replica Failure (v1 Limitation)

The placeholder ConfigMap is durable in etcd, but the per-request delivery pipeline (agent POST + retry + callback dispatch + retry) lives in-memory on the replica that accepted the inbound webhook. If that replica dies between returning `202` and patching the ConfigMap with the response (rolling restart, node drain, OOM kill, crash), the in-flight request is dropped silently. Pollers observe `202` indefinitely until the 1-hour TTL flips them to `404`, and no `delivery_failed` envelope is ever stored. There is no work-claim or peer-replica takeover in v1. This is the documented async-mode trade-off for not running a durable queue. The same trade-off bounds throughput; see the capacity note in [Response Persistence](#response-persistence).

Callers SHOULD treat an unanswered poll past a sane bound (5 to 10 minutes for happy-path async, longer for known-slow agents) as failed, and resubmit the original webhook with fresh inputs. The new submission gets a new `requestId` and is independent of the lost one.

### Response-Patch Failure (v1 Limitation)

Patching the placeholder ConfigMap with the agent's response (or the corresponding error envelope) is itself an apiserver write and can fail transiently. The `Patch` fires when callback is not being used: either no `callbackUrl` is configured on the AgentChannel, or the bounded callback-retry schedule was exhausted. Failure causes include a transiently unavailable apiserver, unreachable etcd, a `Patch` conflict, and RBAC drift on the gateway's `agentry-system` ConfigMap surface.

The gateway retries the `Patch` on the same 1s, 5s, 25s backoff schedule as callback delivery, capped at 4 attempts total, keeping the bounded-retry vocabulary uniform across the agent-delivery, callback, and Patch pipelines (see [The Bounded Retry Schedule](#the-bounded-retry-schedule)). If all 4 attempts fail, the in-memory payload is dropped, the gateway emits the Prometheus counter `agentry_channel_async_patch_failed_total` (labeled by namespace), and logs at error level. See [Observability](../../operations/observability.md) for the operator-side signal that this v1 limitation has fired.

The caller's wire-level experience is identical to a replica death: pollers observe `202` until the 1-hour TTL flips to `404`, and no error envelope is ever stored. Caller-side mitigation mirrors the replica-failure case above. Treat unanswered polls past the same 5-to-10-minute bound (or the agent's known-slow upper bound) as failed and resubmit; the new submission gets a new `requestId`.

### Callback Failure Buckets

A callback POST's outcome is classified into one of three buckets. The figure under [Callback Delivery and Retries](#callback-delivery-and-retries) shows where each bucket is decided and where each one lands:

| Bucket | Triggers | Behavior |
|---|---|---|
| Retried | TCP connect error, TLS handshake failure, per-attempt read timeout (`gateway.callbackReadTimeout`, default 10s, same default as the agent-delivery side), HTTP `408`, HTTP `429`, HTTP `422`, all HTTP `5xx` | Retried on the 1s/5s/25s schedule, 4 attempts total; after the 4th attempt fails, the response is stored at the polling endpoint |
| Terminal | HTTP `401`, `403`, `404`, `405`, `410`, `415` | No retry; payload stored at the polling endpoint immediately; `Warning` event with `reason=CallbackRejected` |
| Bypassed | `callback_invalid`: the URL fails the deny-range / allowlist re-check before dial | No retry and no fresh `callback_invalid` envelope; the underlying payload is preserved at the polling endpoint |

**Retried.** Callback receivers are external systems that may be transiently misconfigured (auth-secret rotation drift, in-progress deploys, transient overload), so an early-attempt failure in this class is often resolved within the retry budget. `422 Unprocessable Entity` is included intentionally: schema deploys on the receiver side can drift the accepted body shape briefly, and the 4-attempt window covers typical rollouts.

**Terminal.** The credential and route codes (`401`, `403`, `404`, `410`) indicate the callback target is permanently rejecting Agentry's POST (credential mismatch, route removed, gone), and retrying burns gateway work without changing the outcome. `405` (receiver permanently rejects POST on this URL) and `415` (receiver permanently rejects JSON content-type) are structural rejections of the gateway's POST shape and similarly will not heal via retry. The payload that would have been delivered (success response or error envelope) is patched into the per-request ConfigMap so the caller can recover it by polling within the 1-hour TTL. Persistent occurrence is additionally reflected in `AgentChannel.status.conditions[type=PlatformConnected]` as `{status: False, reason: CallbackRejected}`, parallel to `CallbackInvalid`. The `Warning` event is the per-occurrence signal; the status condition is the persistent signal that survives event TTL.

**Bypassed.** With `callback_invalid` there is no callback target to retry against, since the URL is rejected before dial. See [`callback_invalid`](#error-payloads) below for the full mechanism.

## Error Payloads

Error payloads are sent to `callbackUrl`, or stored at the polling endpoint on callback failure:

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

**`delivery_failed`** is returned when the initial attempt and all 3 retries of `POST /v1/message` to the agent's Service fail. The failure signals, schedule, and arithmetic are canonical in [The Bounded Retry Schedule](#the-bounded-retry-schedule) above. In sync mode, `gateway.syncDeliveryDeadline` may short-circuit the pipeline mid-retry with `504 sync_deadline_exceeded` (see [Sync-Mode Reachability](#sync-mode-reachability) and [User Gateway Error Responses](errors.md#user-gateway-error-responses)); async mode runs the full retry budget without a deadline.

`retryable: false` mirrors the rationale used for `wake_timeout` below. The gateway has already burned 4 attempts with exponential backoff over ~31s to ~71s wall-clock, so the failure is typically structural (broken image, agent crash loop, repeated 5xx, or a misconfigured per-Agent NetworkPolicy on the message path) rather than transient cluster pressure, and a fresh caller-side retry within the same time horizon is unlikely to succeed. Tenants seeing `delivery_failed` should investigate the AgentChannel's `status.conditions[type=PlatformConnected]` and the Agent Pod status rather than retrying at the caller. This is the asymmetry with `controller_unavailable` below, where the wake itself never reached the controller and a fresh attempt has a clean substrate.

**`wake_timeout`** is returned when the agent is `Hibernated` and fails to become Ready within `wakeTimeout`:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "wake_timeout",
    "message": "Agent did not become ready within wakeTimeout (120s)",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:12:00Z"
}
```

`retryable: false` reflects the design assumption that exceeding `wakeTimeout` indicates a structural Pod-startup problem (image pull failure, init container crash, OOM, bad spec) rather than transient cluster pressure; the same failure is expected on retry. Tenants experiencing transient timeouts should raise `wakeTimeout` rather than retrying at the caller. This is the asymmetry with `controller_unavailable` below, where the wake itself never reached the controller and retry is expected to succeed once the controller recovers.

**`controller_unavailable`** is returned when a message arrives for a `Hibernated` agent and the gateway cannot reach the controller's activator endpoint (connection error, 5xx after retry, or mTLS handshake / SAN authorization failure). The wake could not even be attempted, so the agent remains `Hibernated`:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "controller_unavailable",
    "message": "Controller activator endpoint unreachable; wake could not be triggered",
    "retryable": true
  },
  "failedAt": "2026-04-05T12:12:00Z"
}
```

Unlike `wake_timeout` (the agent was asked to wake but did not become ready in time), `controller_unavailable` means the wake request itself never reached the controller. Retrying the original webhook is expected to succeed once the controller recovers, hence `retryable: true`. The sync-webhook equivalent is HTTP `504 Gateway Timeout` with the same error body and a `Retry-After: 5` header (5 seconds, fixed in v1, sized to typical controller-restart and probe intervals); see [Failure Modes](../user/operations.md#failure-modes).

`retryable: true` covers the mTLS / SAN-authorization failure modes too. At runtime those are operationally transient (for example, a peer mid-reload of a freshly rotated leaf certificate) rather than structural. Persistent mTLS / SAN failures on this path are deployment-time concerns: gateway and controller TLS certs are cert-manager-managed from the same `agentry-ca-issuer` (see [Internal Endpoint Authentication](../../security/rbac.md#internal-endpoint-authentication) and [§ In-cluster TLS](../../security/tls.md#in-cluster-tls)), and the chart install fails fast if the issuer is not present. This is the asymmetry with `wake_timeout` above, where the failure typically reflects a per-Pod structural problem the same Agent will hit on retry.

**`response_too_large`** is returned when the agent's response body exceeds `gateway.maxResponseBodyBytes` (default 900 KiB; see [Request Flow](../user/overview.md#request-flow) step 6a for the size cap rationale). The cap exists for two reasons and applies uniformly to both modes: async responses are persisted to ConfigMaps in `agentry-system` (Kubernetes object cap near 1 MiB), and all webhook responses, sync and async alike, are buffered in gateway memory before forwarding, so an unbounded agent reply could OOM the gateway. Agents that need to return large outputs should externalize them and reference by URL:

```json
{
  "requestId": "550e8400-e29b-41d4-a716-446655440001",
  "channelPath": "/channels/team-support/support-assistant",
  "error": {
    "type": "response_too_large",
    "message": "Agent response body exceeded gateway.maxResponseBodyBytes (900 KiB); externalize large outputs and reference by URL",
    "retryable": false
  },
  "failedAt": "2026-04-05T12:13:14Z"
}
```

**`callback_invalid`** fires when the configured `callbackUrl` host fails re-resolution against the deny ranges (loopback, link-local, RFC1918, unique-local IPv6, cloud metadata) or the configured `gateway.callbackUrl.allowlist` immediately before a delivery attempt. This defeats DNS rebinding between reconcile-time validation and delivery (see [AgentChannel rule 22](../../resources/validation-and-defaulting.md#cross-resource-validation)). On the accept path the same resolution is pinned into the dial: the transport dials the range-checked `IP:port` directly (the URL's hostname is preserved for the `Host` header and TLS SNI) and never re-resolves the hostname itself; see [Request Flow](../user/overview.md#request-flow) step 8 for the dialer mechanism.

The URL is rejected before dial, so there is no callback target to POST to. The payload that would have been delivered (the agent's response on the success path, or the corresponding error envelope such as `delivery_failed` or `wake_timeout` on the error path) is stored at the polling endpoint under the original `requestId` instead, sharing the same shape and 1-hour-from-202-acceptance TTL as the no-`callbackUrl` and retries-exhausted paths above. The AgentChannel additionally receives a `Warning` event with `reason=CallbackInvalid`; this Event is the only operator-side signal that callback was skipped. `callback_invalid` does not appear as an `error.type` in the polling payload. Polling callers see whatever payload would have been sent on the callback (a success response or a different `error.type`), not a `callback_invalid` envelope. Repeated `callback_invalid` is additionally reflected in `AgentChannel.status.conditions[type=PlatformConnected]` as `{status: False, reason: CallbackInvalid}` so persistent callback misconfiguration is visible in status. The `Warning` event is the per-occurrence signal; the status condition is the persistent signal that survives event TTL.

Error payloads (`delivery_failed`, `wake_timeout`, `controller_unavailable`, `response_too_large`) are delivered to `callbackUrl` with the same 3-retry / 1s-5s-25s backoff as successful responses. If no `callbackUrl` is configured, all 4 callback attempts fail (initial + 3 retries), or callback delivery is rejected as `callback_invalid`, errors are stored at the polling endpoint under the original `requestId`, sharing the same 1-hour-from-202-acceptance TTL as the success path (see [Callback Delivery and Retries](#callback-delivery-and-retries) for the clock-origin detail). This mirrors the success-path retry-exhaustion behavior.

## Polling Fallback

**`GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}`**

Served over HTTPS on the User Gateway listener (port 8080, TLS using `agentry-gateway-tls`, the same certificate used by the LLM listener). External callers reach it through the cluster Ingress that fronts port 8080; see [TLS and Ingress](../user/overview.md#tls-and-ingress).

The endpoint returns the agent's response or error payload if available. The `channelPath` query parameter is the URL-encoded webhook path of the originating AgentChannel, exactly the `channelPath` value the caller received in the 202 response body. The gateway uses it to look up the AgentChannel's auth configuration and authenticate the request. Callers must preserve the value verbatim from the 202 response and URL-encode it when assembling the query string; it should not be constructed independently.

### Poll Authentication

Poll requests carry no body, so the auth contract differs slightly from the original webhook:

- **`auth.type: bearer`**: the poll request presents the same bearer token in `Authorization: Bearer ...` as the inbound webhook. The token is read from the Secret referenced by `AgentChannel.spec.webhook.auth.secretRef` (`name`, `key`); there is no inline token field on the AgentChannel spec. Bearer poll requests carry no timestamp and are not subject to the 300s skew check below.
- **`auth.type: hmac`**: the poll request computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}"` (unix seconds, no trailing newline). It sends the bare lowercase hex digest in the configured `header` (the same header name as the original webhook; `auth.hmac.signaturePrefix` and `auth.hmac.encoding` exist for inbound third-party-sender compatibility and do not apply here, since polling is an Agentry-canonical surface and always uses bare hex with no prefix), and presents the timestamp in a dedicated `X-Agentry-Timestamp` header. The HMAC input is not the request body (poll GETs have none). HMAC poll requests with clock skew greater than 300s against the gateway's wall clock are rejected with `401 Unauthorized`; bearer poll requests are not subject to this check.

`401 Unauthorized` is returned on any auth failure (missing or malformed credentials, signature mismatch, clock skew) and on a well-formed but unregistered `channelPath`. The gateway treats an unknown channel as an auth failure rather than a 404 to avoid revealing which webhook paths exist (and, by extension, which tenant namespaces are hosted, since paths are `/channels/{namespace}/...`-prefixed). This mirrors the channel-match assertion below and the [threat-model row on cross-channel response retrieval](../../security/threat-model.md). When the credentials authenticate correctly against `channelPath` but the stored `requestId` was originated by a different channel, the response is `404 Not Found`, the same code as an unknown `requestId`.

### Channel-Match Assertion

After authenticating the caller against the AgentChannel identified by `channelPath`, and before serving any response (including the empty-placeholder `202`), the gateway asserts that the `agentry-async-{requestId}` ConfigMap's `agentry.io/channel-namespace` and `agentry.io/channel-name` labels match that same AgentChannel.

This prevents a caller authenticated for channel A from probing channel B's `requestId` lifecycle. `requestId`s are UUIDs but are not secrets, so without this check an attacker holding channel A's credentials plus any channel B `requestId` could observe `202` (placeholder present) versus `404` (no record) and learn whether a B request is in flight, in addition to reading B's payload once stored. On the wire a mismatch is indistinguishable from an unknown `requestId`; both return `404 Not Found`, so the endpoint does not leak the existence of cross-channel responses or in-flight requests to a credentialed attacker. Channel mismatches are logged at the gateway with `reason=ChannelMismatch` for operator debugging.

### Read Semantics

Any gateway replica accepts this request. The replica reads the `agentry-async-{requestId}` ConfigMap in `agentry-system`: the ConfigMap is created as an empty placeholder when the originating replica returns `202` to the inbound webhook (so the polling record is queryable as soon as the caller has the `requestId`) and is patched with the response payload (or an error envelope) when the agent reply is ready. See [Response Persistence](#response-persistence) for the storage mechanics, including why the payload lives under `data` rather than `binaryData`.

The channel-match assertion above fires on every ConfigMap-present branch, not just the payload-present one, so cross-channel `requestId` existence is never wire-observable. A present ConfigMap whose channel labels do not match `channelPath` returns `404` regardless of whether the payload field has been patched in. With the assertion passing, `200` is returned when the payload field is present; `202` when it is not. An absent ConfigMap returns `404`.

![State diagram of one agentry-async-{requestId} polling record. From Absent, the placeholder Create at 202-acceptance moves it to Placeholder, where polls return 202 with a backoff-aware Retry-After. The payload Patch moves it to Patched, where polls return 200. Either state ages into Expired once now minus creationTimestamp exceeds one hour, which polls report as 404; this is enforced read-side on every poll, not by the reconciler's pruner. The reconciler prune or the finalizer sweep then returns the record to Absent. A red edge runs straight from Placeholder to Expired: the silent-loss path taken when the payload Patch never lands, whether because the accepting replica died or because all four Patch attempts failed.](../../diagrams/async-poll-record-lifecycle.svg)

Reading the diagram: the two v1 silent-loss limitations ([Replica Failure](#replica-failure-v1-limitation) and [Response-Patch Failure](#response-patch-failure-v1-limitation)) are not separate transitions. They are the *same* red edge, reached whenever the `Patch` never lands, and they are indistinguishable on the wire.

### Polling Cadence

Callers SHOULD poll no faster than every 2 seconds, with exponential backoff to ~30 seconds between attempts. Repeated `202` responses indicate the agent has not yet replied. Aggressive polling does not accelerate delivery, since the agent response is independent of poll arrival.

To make the cadence machine-readable, the gateway emits `Retry-After` (integer delta-seconds, RFC 7231 § 7.1.3) on every `202` polling response, backoff-aware: `2s, 4s, 8s, 16s, 30s`, capped at `30s` for subsequent polls within the same `requestId`. A strict-honoring caller that always waits the hinted delay follows the documented exp-backoff curve without rolling its own backoff; a lenient caller still sees the curve as advisory prose above. The gateway computes the header from elapsed time since the placeholder ConfigMap was created, `t = now - placeholder.creationTimestamp`, using this mapping:

| Elapsed time `t` | `Retry-After` |
|---|---|
| `t < 2s` | 2 |
| `2s <= t < 6s` | 4 |
| `6s <= t < 14s` | 8 |
| `14s <= t < 30s` | 16 |
| `t >= 30s` | 30 |

Every replica reads the same ConfigMap, so the schedule is replica-agnostic. A caller round-robining across replicas observes the same backoff curve as one stuck to a single replica. No per-replica state, no etcd write per poll. Per-caller rate-limiting is not enforced at the gateway; operators that need a hard ceiling should rate-limit at the cluster Ingress fronting `:8080`, parallel to the inbound-webhook posture (per [Scoping Summary](../../concepts/vision-and-scope.md#scoping-summary)).

### Poll Status Codes

| Status | Retryable | Meaning |
|---|---|---|
| 200 | n/a | Response or error payload available; body contains the callback payload above |
| 202 | n/a | Request accepted; agent response not yet available. Gateway emits `Retry-After` as a backoff-aware cadence hint (`2s`, `4s`, `8s`, `16s`, `30s`, capped at 30s for subsequent polls), matching the documented exp-backoff curve. Caller should poll after the hinted delay (see [Polling Cadence](#polling-cadence)) |
| 400 | no | Missing or malformed `channelPath` query parameter |
| 401 | no | Auth failed (missing or malformed credentials, signature mismatch, clock skew > 300s on HMAC-mode requests only, or `channelPath` not registered to any AgentChannel) |
| 404 | no | Unknown `requestId`, response expired (1-hour TTL from 202-acceptance), or stored `requestId` originated by a different channel than `channelPath` (channel-match failure, applying whether the ConfigMap holds a placeholder or a patched response) |

The `401` response carries the structured `{ "error": { "type", "message", "retryable" } }` envelope documented in [User Gateway Error Responses](errors.md#user-gateway-error-responses), with the same `unauthorized` `error.type` and the same generic-`message` / no-cause-disambiguation contract as the inbound-webhook `401`, by design (the threat-model rationale in [Poll Authentication](#poll-authentication) applies on both surfaces). The `400` response carries the same envelope with `error.type: invalid_request`, reusing the type name from the [LLM Gateway Error Responses](errors.md#llm-gateway-error-responses) table. `404` follows standard HTTP semantics with an empty body.

### TTL and Retention

Stored responses are retained for 1 hour from 202-acceptance. The polling-record TTL clock starts when the placeholder ConfigMap is created and is not reset by the payload `Patch`, so the per-`requestId` polling window is bounded at 1 hour regardless of how long the agent takes to reply. The gateway enforces this on every poll read: `404` is returned when `now - placeholder.creationTimestamp > 1h`, regardless of whether the [AgentChannelReconciler](../../controller/reconcilers.md#agentchannelreconciler) has yet pruned the ConfigMap. Reconciler pruning is storage cleanup, not the mechanism that drives the `200`/`202` to `404` transition observed by polling callers.

Neither path is a durable queue. Callback delivery is best-effort (3 retries with 1s/5s/25s backoff), and the polling endpoint is the receiver-driven fallback within the same 1-hour TTL. Receivers that miss the callback retries can still recover the response by polling on timeout, but past the TTL the response is gone. The 1-hour TTL is fixed in v1 and intentionally not Helm-tunable: polling is a receiver-driven fallback within a bounded window, not a durable response queue. Agents that need to return results past 1h should externalize the result and reference by URL (the same guidance as [`response_too_large`](#error-payloads)).
