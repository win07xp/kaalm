# Platform Adapters and Channel Health

A **platform adapter** is the code that turns one external messaging platform into Kaalm's internal message envelope, and turns an agent's reply back into something that platform understands. The User Gateway holds one adapter per channel type, and every AgentChannel names the type it uses.

This page covers the adapter interface v1 ships, and the health signal the gateway derives from adapter traffic so the controller can report whether a channel is actually working.

## What v1 ships

v1 ships with the **generic webhook adapter** only: inbound HTTP POST with configurable auth.

Discord and WhatsApp adapters are deferred to v1.1. They require persistent connections (Discord WebSocket, WhatsApp Cloud API registration), platform-specific reconnection logic, and API versioning, which adds significant implementation surface. The webhook adapter, by contrast, is stateless and still covers the core channel integration pattern: authenticate a caller, normalize a payload, deliver it to an agent, return or dispatch the reply.

## The ChannelAdapter interface

Platform adapters follow a plugin pattern so v1.1 can add types without reshaping the gateway:

```go
type ChannelAdapter interface {
    Type() string
    Authenticate(req *http.Request, credentials Credentials) error
    ParseInbound(req *http.Request) (MessageEnvelope, error)
    FormatOutbound(envelope MessageEnvelope) ([]byte, error)
    SendReply(ctx context.Context, envelope MessageEnvelope, credentials Credentials) error
}
```

### SendReply and the two response modes

`SendReply` is the async delivery path. When `responseMode: async`, the gateway calls `SendReply` to POST the agent's response to the configured `callbackUrl` after the agent has processed the message.

For sync mode, `SendReply` is **not** called. The response is returned inline as the HTTP response body.

Discord and WhatsApp adapters (v1.1) will use `SendReply` for all responses, since those platforms are inherently asynchronous: there is no inbound HTTP request left open to answer.

### What the webhook adapter's SendReply does before dialing

The webhook adapter's `SendReply` applies the `callbackUrl` allowlist/blocklist check before dialing, and then applies the `callbackAuth` signing step. See step 8 in [Request Flow](overview.md#request-flow), [rule 22](../../resources/validation-and-defaulting.md#cross-resource-validation) (URL constraints), [rule 25](../../resources/validation-and-defaulting.md#cross-resource-validation) (signing required), and [Callback authentication](../api/async-responses.md) (wire contract).

Both checks live inside `SendReply` rather than in shared middleware. That placement is deliberate: it lets each adapter apply transport-appropriate rules if v1.1 adapters call into non-HTTP destinations, where an HTTP-shaped middleware check would not apply.

---

## Channel Health Tracking

The gateway maintains per-channel delivery health in-memory (per replica), populated as the gateway processes incoming webhook requests and (for async-mode channels with `callbackUrl` set) outbound callback dispatch attempts. The controller queries this state via `GET /v1/channels/health` to populate `status.conditions[type=PlatformConnected]` on each AgentChannel. See [GET /v1/channels/health](../api/internal-endpoints.md#get-v1channelshealth) for the endpoint shape.

`PlatformConnected` is a **rolling-window** condition, not a "last result" condition: it reflects whether the channel has had observed inbound activity in the last `N` (a Helm-configured window, `gateway.channelHealthWindow`, default `5m`). This avoids a long-silent channel appearing permanently healthy purely on the strength of a successful delivery hours or days ago.

### Per-replica observation list

Each replica keeps a bounded list of in-window observations per registered channel path, each shaped:

```
{ result: success | failure, reason, timestamp, lastError? }
```

Entries older than the window are dropped on insertion or on read.

What counts as what:

- **`success`** requires both: webhook auth validation passed **and** the message was dispatched to the target agent (`POST /v1/message` returned 2xx, or, in async mode, was queued for the retry pipeline).
- **`failure`** covers failures past the auth step but before agent dispatch (for example, agent not Ready, route resolution failed), recorded with the corresponding reason.

Two callback-side outcomes also contribute to channel health. Both indicate a structural problem the operator (or the receiver) must fix, which is why they surface on the channel rather than being swallowed by the retry pipeline:

- Outbound callback attempts that fail the deny-range / allowlist re-check immediately before dial are recorded as `failure` with `reason: CallbackInvalid`.
- Callback POSTs terminally rejected by the receiver (HTTP `401`/`403`/`404`/`405`/`410`/`415`, the terminal bucket in [Callback failure modes](../api/async-responses.md)) are recorded as `failure` with `reason: CallbackRejected`.

Transient callback delivery failures (the retried bucket) are recoverable via the retry pipeline and are **not** recorded.

No etcd writes are performed per request.

### Per-replica state

From its in-window list, each replica computes one of three states per channel:

- `success`: at least one in-window observation has `result: success`. Reported alongside the most recent success's `reason`/`timestamp` and the most recent failure's `lastError` if any (informational).
- `failure`: the in-window list is non-empty and contains only failures. Reported with the most recent failure's `reason`/`lastError`/`timestamp`.
- `empty`: no in-window observations.

### Replica-startup handling

Each replica reports its `replicaStartedAt` in the response. A replica with `now - replicaStartedAt < N` has not been alive long enough to observe a full window; its `empty` state is therefore not evidence that the channel is silent, only that this replica cannot prove silence. The controller treats the `replicaStartedAt` flag the same way the activity-API path does (see [§ Activity Tracking API](activation-and-activity.md#activity-tracking-api)): `empty` from a not-yet-full-window replica does not contribute to a silence determination.

### Multi-replica fan-out and reduction

The `AgentChannelReconciler` queries every gateway Pod IP in parallel, same shape as the [activity-API fan-out](activation-and-activity.md#activity-tracking-api), including the per-Pod-IP TLS handling (`ServerName` override against the gateway Service DNS) and the unreachable-replica skip. The controller reduces the per-replica states into the AgentChannel condition as follows:

![Activity diagram of the four-rule reduction from per-replica health states to the PlatformConnected condition. The AgentChannelReconciler fans GET /v1/channels/health out to every gateway Pod IP, skips unreachable replicas, and collects a state (success, failure or empty) plus a replicaStartedAt from each reachable replica. The rules are then tried in strict priority order. Rule 1: if any reachable replica reports success, PlatformConnected is True with reason WebhookReady and the most recent success's metadata. Rule 2: otherwise, if any reachable replica reports failure, PlatformConnected is False carrying the most recent failure's reason and lastError, one of WebhookAuthFailed, AgentNotReady, DispatchFailed, CallbackInvalid or CallbackRejected. Rule 3: otherwise, if at least one replica has been up for the full channelHealthWindow AND every reachable replica reports empty, PlatformConnected is Unknown with reason NoRecentTraffic; a note explains that both halves are required, because a replica younger than the window cannot prove silence. Rule 4: otherwise the controller preserves the existing condition and writes nothing, a deliberate no-op that is also the all-replicas-unreachable path, since writing a state here would flap the condition on every coordinated gateway restart.](../../diagrams/channel-health-reduction.svg)

Reading the diagram: the cascade is ordered, so each rule reads "and none of the rules above matched". Two arms carry most of the subtlety. Rule 3 is the only one with a compound precondition, because an `empty` from a freshly-started replica is not evidence of silence. Rule 4 is the only arm that writes nothing at all, which is what keeps a coordinated restart from flapping the condition.

1. Any replica reports `success` ⇒ `PlatformConnected = True` with `reason = WebhookReady` and the most recent success's metadata.
2. Else any replica reports `failure` ⇒ `PlatformConnected = False` with the most recent failure's reason (`WebhookAuthFailed`, `AgentNotReady`, `DispatchFailed`, `CallbackInvalid`, `CallbackRejected`).
3. Else at least one replica has been up the full window AND every reachable replica reports `empty` ⇒ `PlatformConnected = Unknown` with `reason = NoRecentTraffic`.
4. Else (no replica has full-window coverage AND no in-window observations exist anywhere) ⇒ preserve the existing condition. This mirrors the activity-API "all replicas unreachable" rule and avoids flapping during a coordinated gateway restart.

If all replicas are unreachable, the existing condition is preserved (rule 4 path).

### Reuse in v1.1

For v1.1+ persistent-connection channels (Discord, WhatsApp), the same per-replica list and reduction will be reused: gateway-side connection liveness produces `success`/`failure` observations on connection events (handshake completed, disconnect with reason) rather than per-request inbound deliveries. The tri-state condition semantics and reason-code shape do not change.
