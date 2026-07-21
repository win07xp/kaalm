# User Gateway Operations

This page covers how the User Gateway is monitored in production and how it behaves when its dependencies fail. Read [Request Flow](overview.md#request-flow) first: the metrics and failure modes here name specific steps of that flow.

## Observability

The gateway exposes Prometheus metrics on `:9090/metrics`.

**Message delivery.**

- `kaalm_channel_messages_total{channel_type,namespace,status}`
- `kaalm_channel_message_duration_seconds{channel_type}`

**Wake-on-demand.** These two metrics are meant to be read together. `kaalm_channel_wake_total` tells you how often the activator fired; `kaalm_channel_wake_duration_seconds` tells you how long each wake took and how it ended. Together they make wake-on-demand latency observable end to end, which is what lets you put an SLO on the hard control-plane dependency called out in [The Kaalm Gateway](../overview.md).

- `kaalm_channel_wake_total{namespace}` (count of hibernation wakes triggered)
- `kaalm_channel_wake_duration_seconds{namespace,result}`, a histogram of time from `POST /v1/activate` to either Agent ready or wake timeout, with `result ∈ {ready, controller_unavailable, wake_timeout}`

**Async callback delivery.** A counter and a histogram covering the push half of async mode:

- `kaalm_channel_callback_total{namespace,status}`
- `kaalm_channel_callback_duration_seconds{namespace}`

The `status` label is `delivered`, `exhausted`, `rejected`, or `invalid`:

| `status` | Meaning |
|---|---|
| `delivered` | Receiver answered 2xx. |
| `exhausted` | Retry schedule burned; response stored at the polling endpoint. |
| `rejected` | Terminal receiver rejection (`CallbackRejected` bucket). |
| `invalid` | Pre-dial deny-range / allowlist re-check failed (`CallbackInvalid`). |

`exhausted` is the visible form of the best-effort callback semantic: the push gave up, but the payload is still retrievable by polling. See [Request Flow steps 5a, 6a, 8](overview.md#request-flow) and [Callback failure modes](../api/async-responses.md).

**Response size rejections.**

- `kaalm_channel_response_too_large_total{namespace,mode}`, a counter of agent responses rejected for exceeding the configured size limit. `mode ∈ {sync, async}` separates the size-limit signal by response mode.

**Async patch failures.**

- `kaalm_channel_async_patch_failed_total{namespace}`, a counter of async response-`Patch` pipelines that exhausted all 4 attempts and dropped the payload.

This is the operator-side signal that the v1 silent-loss limitation fired. From the caller's side the loss is invisible: pollers observe `202` until the TTL flips the record to `404` with no stored envelope ever appearing. The metric is the only place the drop surfaces, so any sustained nonzero rate warrants an alert. See [Response-Patch failure semantics](../api/async-responses.md) and [Recommended alerts](../../operations/observability.md#recommended-alerts).

For LLM Gateway metrics, see [LLM Gateway Operations](../llm/operations.md#observability).

## Failure Modes

| Failure | Behavior |
|---|---|
| All gateway replicas down | Inbound webhooks fail at the Ingress; agent LLM calls fail; channel-driven wakes cannot be triggered; controller defers idle/hibernation transitions and sets `GatewayReachable=False`. |
| Gateway replica not ready (listener, informer, or cert not loaded) | Replica removed from Service endpoints until readiness passes. |
| Channel credential invalid | AgentChannel marked `Ready=False`; platform connection drops. |
| Agent Pod not ready (resuming) | User Gateway holds or retries message delivery up to configured timeout. |
| Controller unreachable | Wake-on-demand fails; sync callers get `504` `controller_unavailable`; async gets the same error via callback or polling. |
| Sync-mode wall-clock budget exceeded | Sync callers get `504` `sync_deadline_exceeded`, `retryable: true`. Async mode unaffected. |
| Async response ConfigMap not found | Poll returns `404`: the response is unknown or has expired past the 1-hour TTL. |

**All gateway replicas down.** Inbound webhooks fail at the user-provisioned Ingress, which has no ready backend, so callers see the Ingress's own 502/503 rather than anything Kaalm produced. LLM calls from agents fail. Channel-driven wakes cannot be triggered at all, because wakes originate at the gateway. The controller defers idle and hibernation transitions and sets `GatewayReachable=False` on affected Agents: with no activity data it cannot safely conclude an Agent is idle. See [Activity Detection](../../controller/hibernation-and-wake.md#activity-detection).

**Gateway replica not ready.** The replica is removed from Service endpoints until readiness passes, so traffic lands only on replicas that can actually serve it. See [Gateway Readiness](../llm/operations.md#gateway-readiness).

**Controller unreachable.** Wake-on-demand fails. Sync callers receive `504` with `error.type: controller_unavailable`; in async mode the gateway stores a `controller_unavailable` error at the polling endpoint or delivers it to `callbackUrl`. Already-`Running` agents are unaffected, since they need no wake. See [Activator § controller unreachable](activation-and-activity.md#the-activator).

**Sync-mode wall-clock budget exceeded.** When total wall-clock exceeds `gateway.syncDeliveryDeadline` (default 30s) mid-retry, sync callers receive `504` with `error.type: sync_deadline_exceeded` and `retryable: true`. Async mode is unaffected because it has no wall-clock budget. See [Request Flow step 6a](overview.md#request-flow).

**Async response ConfigMap not found.** A poll returns `404` when the response is unknown or has expired past the 1-hour TTL. Configuring `callbackUrl` makes the gateway push the response with retries, but that push is best-effort, not durable; receivers that miss it can still poll within the TTL. See [Async Webhook Response](../api/async-responses.md).
