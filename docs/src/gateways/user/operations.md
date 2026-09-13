# User Gateway operations

This page covers how the User Gateway is monitored in production and how it behaves when its dependencies fail. Read [Request flow](overview.md#request-flow) first: the metrics and failure modes here name steps of that flow.

## Observability

The gateway serves Prometheus metrics on `:9090/metrics` ([Endpoints](../../operations/observability.md#endpoints)). The channel metrics:

| Metric | Type | Labels | Increments when |
|---|---|---|---|
| `kaalm_channel_messages_total` | counter | `channel_type`, `namespace`, `status` | a message finishes the delivery pipeline (step 8 of the request flow), or a platform event is rejected before it |
| `kaalm_channel_message_duration_seconds` | histogram | `channel_type` | the same, with the whole pipeline's duration, wake included |
| `kaalm_channel_wake_total` | counter | `namespace` | the activator is called for a `Hibernated` Agent |
| `kaalm_channel_wake_duration_seconds` | histogram | `namespace`, `result` | a wake ends, measured from the activator call: `result` is `ready`, `controller_unavailable`, or `wake_timeout` |
| `kaalm_channel_delivery_attempts_total` | counter | `namespace`, `outcome` | every `POST /v1/message` attempt, retries included |
| `kaalm_channel_callback_total` | counter | `namespace`, `status` | an outbound reply ends: an async webhook callback, or a Discord or WhatsApp reply |
| `kaalm_channel_callback_duration_seconds` | histogram | `namespace` | the same, with the reply's duration across its attempts |
| `kaalm_channel_response_too_large_total` | counter | `namespace`, `mode` | an agent reply exceeds `gateway.maxResponseBodyBytes`: `mode` is `sync` for the blocking webhook and test-chat path, `async` for async webhooks and platform channels |
| `kaalm_channel_async_patch_failed_total` | counter | `namespace` | the response `Patch` into the polling record fails on all four attempts and the payload is dropped |

Label vocabularies:

- **`channel_type`** is `webhook`, `discord`, or `whatsapp`, and `console` for [test-chat](../api/internal-endpoints.md#post-v1test-chat) deliveries.
- **`status` on messages** is `delivered` or the failing error type: `delivery_failed`, `wake_timeout`, `controller_unavailable`, or `response_too_large`. For a platform channel it is also `rejected`, an inbound event that produced no envelope (a Discord interaction out of scope, a WhatsApp event for another number, a status callback, or an unparseable message), which is counted here and never becomes a health observation. A sync request cut off by `gateway.syncDeliveryDeadline` is counted under the failure that was in progress, or not at all when the deadline fell inside a backoff, so `sync_deadline_exceeded` never appears here.
- **`outcome` on delivery attempts** is `ok` or the layer that failed: `dns`, `connect` (refused, reset, unreachable, or every connect within the attempt hit its bound), `tls`, `timeout` (the per-attempt read deadline), `status` (the agent answered outside 2xx), `malformed` (2xx with an unusable envelope), `too_large` (not retried), `canceled` (the delivery's own context ended), or `other`. Each failed attempt is also logged at warning level with the namespace, agent, message id, attempt number, outcome, and error.
- **`status` on callbacks** is `delivered`, `rejected`, `exhausted`, or `invalid`, specified with their triggers under [Callback failure buckets](../api/async-responses.md#callback-failure-buckets) and, for platform replies, [Reply delivery](platform-adapters.md#reply-delivery). `exhausted` on a webhook callback means the payload is still retrievable by polling; on a platform reply it means the payload is dropped.

The wake counter and histogram read together: how often the activator fired, how long each wake took, and how it ended, which is what an SLO on the wake-on-demand dependency needs. `kaalm_channel_async_patch_failed_total` is the operator-side signal that the [replica-local drop](../api/async-responses.md#response-patch-failure) fired; any sustained nonzero rate warrants an alert ([Recommended alerts](../../operations/observability.md#recommended-alerts)). As shipped the counter is not raised when the pipeline's 10-minute ceiling cuts the `Patch` off mid-backoff, so that drop leaves no signal.

For LLM Gateway metrics, see [LLM Gateway operations](../llm/operations.md#observability).

## Failure modes

| Failure | What happens | What the operator sees |
|---|---|---|
| All gateway replicas down | Inbound webhooks fail at the user-provisioned Ingress, which has no ready backend, so callers see the Ingress's own `502` or `503`. Agent LLM calls fail. Channel-driven wakes cannot be triggered, since wakes originate at the gateway. | `GatewayReachable=False` on affected Agents; the controller defers idle and hibernation transitions ([Activity detection](../../controller/hibernation-and-wake.md#activity-detection)). |
| Gateway replica not ready | The replica is removed from the Service endpoints while its readiness probe fails. As shipped `/readyz` answers `ok` as soon as the health listener is up; the four checks under [Gateway readiness](../llm/operations.md#gateway-readiness) are the design, not the probe. | Pod readiness only. |
| Channel credential invalid | The AgentChannel is `Ready=False`, the gateway stops resolving its path, and inbound requests answer `401`. | The `Ready` condition; `kaalm_channel_messages_total` stops moving for the channel. |
| Agent hibernated | The gateway calls the activator and waits for the Service, up to `wakeTimeout`. An Agent that is not ready for any other reason gets the four-attempt delivery retry and then `delivery_failed`. | `kaalm_channel_wake_total` and the wake histogram; `delivery_attempts_total{outcome}` names the failing layer. |
| Controller unreachable | Wake-on-demand fails and the Agent stays `Hibernated`; `Running` Agents are unaffected. | `status="controller_unavailable"` on messages and `result="controller_unavailable"` on wakes. The wire form is under [The activator](activation-and-activity.md#the-activator). |
| Sync deadline exceeded | The caller gets `504 sync_deadline_exceeded`, `retryable: true`; async mode has no deadline. | No dedicated metric; the in-progress failure is counted, or none is. See [Request flow](overview.md#request-flow), step 9. |
| Async response record missing | A poll answers `404`: the `requestId` is unknown, belongs to another channel, or is past the 1-hour TTL. | Nothing; polling is receiver-driven. See [Poll status codes](../api/async-responses.md#poll-status-codes). |
| Async payload dropped | A replica died between the `202` and the `Patch`, or the `Patch` retries were exhausted. Pollers see `202` until the TTL turns the record to `404`. | `kaalm_channel_async_patch_failed_total` for the exhausted case; nothing for a replica death. See [Replica failure](../api/async-responses.md#replica-failure). |
