# Platform Adapters and Channel Health

A **platform adapter** is the code that turns one external messaging platform into Kaalm's internal message envelope, and turns an agent's reply back into something that platform understands. The User Gateway holds one adapter per channel type, and every AgentChannel names the type it uses.

This page covers the adapter interface, the three adapters, and the health signal the gateway derives from adapter traffic so the controller can report whether a channel is actually working.

## What ships

Three adapters: the **generic webhook adapter** (inbound HTTP POST with configurable auth, the adapter v1 shipped with) and, since v0.7.0, the **Discord adapter** (the Interactions endpoint) and the **WhatsApp adapter** (the Cloud API webhook).

All three are inbound HTTP, which is the pattern the User Gateway is built around: authenticate a caller, normalize a payload, deliver it to an agent, return or dispatch the reply. The v1 design deferred the platform adapters on the assumption that they needed persistent connections. That holds for one Discord mode only, the Gateway WebSocket that free-text message bots need; slash commands and WhatsApp events arrive by HTTP, with a signature to verify and an API to answer through. The WebSocket mode stays undesigned (see [the roadmap](../../ROADMAP.md#beyond)): it would need one replica to own each bot's connection, sharding and resume logic, and connection-event health, none of which the HTTP adapters need.

## The ChannelAdapter interface

Platform adapters follow a plugin pattern so a type can be added without reshaping the gateway. The route resolves the channel and applies the size check and the `Ready` gate, then hands a platform channel to its adapter:

```go
type platformAdapter interface {
    // The spec.type value and the envelope's channelType.
    Type() string
    // The inbound half: handshake, signature check, scope, acknowledgement.
    // Writes the platform's response and returns the messages to dispatch.
    Handle(ctx, w, r, channel, body) inboundResult
    // The reply half: text (the agent's content, or an error rendered as
    // text) out through the platform API. Returns the callback outcome.
    SendReply(ctx, channel, message, text) string
}
```

Each message the adapter returns runs its own pipeline (wake, deliver, reply) in the background. The generic webhook adapter predates the interface and stays inlined in the route; the Discord and WhatsApp adapters implement it.

### SendReply and the two response modes

`SendReply` is the async delivery path. When `responseMode: async`, the gateway calls `SendReply` to POST the agent's response to the configured `callbackUrl` after the agent has processed the message.

For sync mode, `SendReply` is **not** called. The response is returned inline as the HTTP response body.

The Discord and WhatsApp adapters use `SendReply` for every response, because neither platform keeps an inbound request open for the answer: Discord expects an acknowledgement within 3 seconds and the reply later, WhatsApp expects a bare `200` and the reply as a new outbound message. The platform adapters have no sync mode; see [The platform adapters](#the-platform-adapters).

### What the webhook adapter's SendReply does before dialing

The webhook adapter's `SendReply` applies the `callbackUrl` allowlist/blocklist check before dialing, and then applies the `callbackAuth` signing step. See step 8 in [Request Flow](overview.md#request-flow), [rule 22](../../resources/validation-and-defaulting.md#cross-resource-validation) (URL constraints), [rule 25](../../resources/validation-and-defaulting.md#cross-resource-validation) (signing required), and [Callback authentication](../api/async-responses.md) (wire contract).

Both checks live inside `SendReply` rather than in shared middleware. That placement is deliberate: it lets each adapter apply its own rules. The platform adapters skip both, because their destination is an operator-set base URL rather than a tenant-supplied one; see [Reply delivery](#reply-delivery).

---

## The platform adapters

*(Since v0.7.0.)* The Discord and WhatsApp adapters share one shape: verify the platform's signature, answer the platform at once, and deliver the reply later through the platform's API. The AgentChannel fields are in [Platform types](../../resources/agentchannel.md#platform-types); the wire contracts are [Discord Channel](../api/channel-discord.md) and [WhatsApp Channel](../api/channel-whatsapp.md). This section is the mechanism.

![Sequence diagram of a platform channel message. A person sends a slash command or a message; the platform POSTs it, signed, to /channels/{ns}/{path} on the User Gateway. Step 1 runs the size check, the Ready-only routing gate, and signature verification, returning 413 or 401; a note records that Discord uses Ed25519 over timestamp plus body with a 300-second skew bound, WhatsApp uses HMAC-SHA256 over the body with the app secret, and that a Discord PING and a WhatsApp verification GET are answered here and go no further. Step 2 scopes and filters: a Discord interaction outside guildId or allowedChannelIds gets an ephemeral refusal and no envelope; WhatsApp statuses and events for another phone number get 200 and are dropped. The gateway then acknowledges, a Discord deferred response of type 5 or a WhatsApp 200 with an empty body, and a highlighted note marks that the platform is now gone and everything below runs in the async pipeline with no kaalm-async record because nobody polls for it. Step 3 normalizes one envelope per message, wakes the agent if needed, and delivers POST /v1/message over mTLS with four attempts at 1s, 5s and 25s, receiving a reply envelope or an error payload. Step 4 is SendReply through the platform API, the Discord follow-up webhook or a POST to /{phoneNumberId}/messages, ending in one of three arms: 2xx, and the reply shows up in the chat; terminal on 400, 401, 403, 404, 405, 410 or 415, recorded as CallbackRejected on channel health with a Warning event and the payload dropped; or retried on connect errors, timeouts, 408, 429 and 5xx on the callback schedule.](../../diagrams/platform-channel-flow.svg)

Reading the diagram: the acknowledgement is the hinge. Everything above it happens with the platform waiting (Discord gives 3 seconds, WhatsApp expects a prompt 200); everything below it happens with the platform gone, which is why both adapters are async only and why an error at the end of the pipeline has to travel back as a reply rather than as a status code.

### Inbound

1. **Size check and routing gate**, unchanged from [Request Flow](overview.md#request-flow) steps 2 and 4: `413` above `gateway.maxMessageBodyBytes`, `401` for a path not registered to a `Ready=True` channel. The adapter is chosen by the channel's `spec.type`.
2. **Verification handshakes.** A Discord `PING` (interaction type 1) is answered with `PONG`; a WhatsApp verification `GET` is answered with its `hub.challenge` when `hub.verify_token` matches. Both prove the URL to the platform when the operator saves it, and go no further: no envelope, no observation on channel health. Both are still authenticated: a `PING` with a bad signature is `401` (Discord sends one on purpose during URL registration and expects the rejection), and a `GET` with the wrong verify token is `403`.
3. **Authenticate.** Discord: Ed25519 verification of `X-Signature-Ed25519` over `X-Signature-Timestamp` concatenated with the raw body, using the channel's `publicKey`; a timestamp more than 300s from the gateway's clock is rejected, the same replay bound the [polling endpoint](../api/async-responses.md#polling-fallback) uses. WhatsApp: HMAC-SHA256 over the raw body with `appSecret`, compared constant-time against `X-Hub-Signature-256` after stripping `sha256=`; it is the webhook adapter's `hmac` path with the header, prefix, and encoding fixed. Failures are `401` and a `failure` observation with `reason: WebhookAuthFailed`.
4. **Scope and filter.** Discord: with `guildId` or `allowedChannelIds` set, an interaction from elsewhere is answered immediately with a fixed ephemeral message and produces no envelope. WhatsApp: `statuses` entries and events for a `phone_number_id` other than the channel's are acknowledged and dropped. Neither is a health observation; both count on `kaalm_channel_messages_total` with `status="rejected"`.
5. **Acknowledge.** Discord: a deferred channel message (response type 5), which the person sees as the bot thinking. WhatsApp: `200` with an empty body. The platform's request ends here. For WhatsApp the `200` is sent before delivery is attempted, so Meta's retry-on-failure never duplicates a message the gateway has already accepted; the cost is that a replica dying between the `200` and delivery loses the message, the same [replica-failure limitation](../api/async-responses.md#replica-failure-v1-limitation) async webhook channels have.
6. **Normalize**, one envelope per message ([Platform types](../../resources/agentchannel.md#platform-types) has the field rules), then **activate and deliver** exactly as [Request Flow](overview.md#request-flow) steps 5 to 6a in async mode: the full wake budget and the full delivery retry budget apply, and no sync deadline does.

### Reply delivery

`SendReply` for a platform channel POSTs to the platform API at the gateway-level base URL (`gateway.platforms.<type>.apiBaseUrl`). That URL is operator-set, so it is trusted the way a ModelProvider endpoint is: the `callbackUrl` deny ranges and allowlist do not apply (an in-cluster mock is a legitimate target), `http://` is accepted, and `CallbackInvalid` cannot occur on this path. TLS is verified against the gateway's system trust store.

Delivery uses the [bounded retry schedule](../api/async-responses.md#the-bounded-retry-schedule) of the callback pipeline (`gateway.callbackRetryBackoff`, `gateway.callbackReadTimeout`) and lands in one of three buckets:

| Bucket | Trigger | Effect |
|---|---|---|
| Delivered | `2xx` | The reply is in the chat. `kaalm_channel_callback_total{status="delivered"}`. |
| Terminal | `400`, `401`, `403`, `404`, `405`, `410`, `415` | No retry. `failure` observation with `reason: CallbackRejected`, a `Warning` event `reason=CallbackRejected` on the AgentChannel naming the status and the platform's error code, and the payload is dropped: there is no polling store for a platform channel. |
| Retried | Connect and TLS errors, read timeouts, `408`, `429`, `5xx` | 1s, 5s, 25s; 4 attempts total. Exhaustion is recorded like a terminal refusal. |

`400` is terminal here where the callback table leaves it unlisted, because on this path the platform has validated the body and will validate it the same way again; the two platform conditions that matter both arrive as `400` (Discord's `Unknown Interaction`, Meta's error `131047` for a reply outside the 24-hour window). The one exception is Meta's rate-limit code `130429`, which also arrives inside a `400` and is retried.

The two adapters differ only in what a reply is:

- **Discord** edits the interaction's original deferred message with the first 2000 characters of the reply (`PATCH .../messages/@original`) and posts each further 2000-character chunk as a follow-up. The interaction token is valid for 15 minutes; a reply request after that is `404`. When the channel's credential Secret carries `botToken`, a `404` on the first reply request switches the whole reply to channel messages posted with the bot token (each chunk prefixed with a mention of the user), one more run of the schedule. Without `botToken`, the `404` is terminal.
- **WhatsApp** posts one text message per 4096-character chunk to `/{phoneNumberId}/messages` with the `accessToken` as bearer, in order, waiting for each to be delivered before sending the next.

An error payload from the async pipeline (`delivery_failed`, `wake_timeout`, `controller_unavailable`, `response_too_large`) is sent as the text `"{error.type}: {error.message}"` through the same path, so the person sees a failure rather than silence. It is not counted as a success on channel health; the failure that produced it already was recorded.

---

## Channel Health Tracking

The gateway maintains per-channel delivery health in-memory (per replica), populated as the gateway processes inbound requests on channels of every type and outbound reply attempts (callback POSTs for async webhook channels with `callbackUrl` set, platform replies for Discord and WhatsApp channels). The controller queries this state via `GET /v1/channels/health` to populate `status.conditions[type=PlatformConnected]` on each AgentChannel. See [GET /v1/channels/health](../api/internal-endpoints.md#get-v1channelshealth) for the endpoint shape.

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
- **Not an observation:** a verification handshake (a Discord `PING`, a WhatsApp verification `GET`) proves the URL, not delivery, and a scope refusal (a Discord interaction outside the channel's guild or channels, a WhatsApp event for another phone number) is the platform sending something the channel was configured not to accept. Neither is recorded. A handshake that fails signature or token verification is a `failure` with `reason: WebhookAuthFailed`, like any other inbound auth failure.

Two callback-side outcomes also contribute to channel health. Both indicate a structural problem the operator (or the receiver) must fix, which is why they surface on the channel rather than being swallowed by the retry pipeline:

- Outbound callback attempts that fail the deny-range / allowlist re-check immediately before dial are recorded as `failure` with `reason: CallbackInvalid`.
- Callback POSTs terminally rejected by the receiver (HTTP `401`/`403`/`404`/`405`/`410`/`415`, the terminal bucket in [Callback failure modes](../api/async-responses.md)), and platform replies terminally refused by the platform (the terminal bucket in [Reply delivery](#reply-delivery)), are recorded as `failure` with `reason: CallbackRejected`. `CallbackInvalid` cannot occur for a platform channel: its reply destination is operator-set and never re-checked against the deny ranges.

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

### The platform adapters and health

The Discord and WhatsApp adapters produce the same observations as the webhook adapter: an inbound request past auth and dispatch is a `success`, a failure before dispatch carries its reason, a terminal reply refusal is `CallbackRejected`. Nothing in the reduction is type-specific, and the reason names are shared: `WebhookReady` is the inbound-works signal for every type. A persistent-connection adapter, if one is ever designed, is the case that would add connection-event observations (handshake completed, disconnect with reason) to this list; the HTTP adapters have no connection to observe.
