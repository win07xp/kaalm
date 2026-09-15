# Platform adapters and channel health

A **platform adapter** is the code that turns one external messaging platform into Kaalm's message envelope, and turns an agent's reply back into something that platform understands. The User Gateway holds one adapter per channel type, and every AgentChannel names the type it uses.

This page covers the adapter interface, the three adapters, and the health signal the gateway derives from adapter traffic so the controller can report whether a channel is delivering.

## What ships

Three adapters: the **generic webhook adapter** (inbound HTTP POST with configurable auth), the **Discord adapter** (the Interactions endpoint), and the **WhatsApp adapter** (the Cloud API webhook).

All three are inbound HTTP, the pattern the User Gateway is built around: authenticate a caller, normalize a payload, deliver it to an agent, return or dispatch the reply. The one Discord mode that needs a persistent connection, the Gateway WebSocket that free-text message bots use, is not designed (see [the roadmap](../../ROADMAP.md#beyond)): it would need one replica to hold each bot's connection, sharding and resume logic, and connection-event health, none of which the HTTP adapters need.

## The platform adapter interface

Platform adapters follow a plugin pattern so a type can be added without changing the gateway's route. The route resolves the channel and applies the size check and the `Ready` gate, then hands a platform channel to its adapter:

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

The Discord and WhatsApp adapters implement it. The generic webhook adapter predates the interface and stays inline in the route: its inbound half is [Request flow](overview.md#request-flow) steps 4 and 5, and its reply half is the async callback of step 10, which applies the `callbackUrl` re-check and the `callbackAuth` signing that platform replies skip (see [Reply delivery](#reply-delivery)). Each message an adapter returns runs its own pipeline (wake, deliver, reply) in the background, with no `kaalm-async` record because nobody polls for it. The platform adapters have no sync mode: neither platform keeps an inbound request open for the answer.

---

## The platform adapters

The Discord and WhatsApp adapters share one design: verify the platform's signature, answer the platform at once, and deliver the reply later through the platform's API. The AgentChannel fields are in [Platform types](../../resources/agentchannel.md#platform-types); the wire contracts are [Discord channel](../api/channel-discord.md) and [WhatsApp channel](../api/channel-whatsapp.md). This section is the mechanism.

### Inbound

![Flowchart of every check on an inbound Discord interaction or WhatsApp event in the order the adapter runs them, as three rows. Route, shared with webhook channels: body within maxMessageBodyBytes, else 413 request_too_large; path registered to a Ready=True channel, else 401. Verify: a WhatsApp verification GET answers 200 with hub.challenge when hub.verify_token matches, else 403; otherwise the platform signature must be valid, else 401, and the body must parse, else 400. Kind, scope, acknowledge: a Discord PING answers 200 PONG; a Discord component, autocomplete, or modal gets a fixed answer and no envelope; a request out of scope by guildId, allowedChannelIds, or phone_number_id gets an ephemeral refusal or a 200 with no envelope and a rejected count; otherwise the adapter acknowledges with a Discord deferred response or a WhatsApp 200 and hands one envelope per message to the background pipeline.](../../diagrams/platform-channel-intake.svg)

The acknowledgement divides the flow. Everything before it happens with the platform waiting (Discord allows 3 seconds, WhatsApp expects a prompt `200`); everything after it happens with the platform gone, which is why both adapters are async only and why an error at the end of the pipeline travels back as a reply rather than as a status code.

1. **Size check and routing gate**, unchanged from [Request flow](overview.md#request-flow), steps 2 and 3: `413` above `gateway.maxMessageBodyBytes`, `401` for a path not registered to a `Ready=True` channel. The adapter is chosen by the channel's `spec.type`.
2. **Verification handshakes.** A WhatsApp verification `GET` is answered with its `hub.challenge` when `hub.mode` is `subscribe` and `hub.verify_token` matches the channel's `verifyToken`, else `403`. A Discord `PING` (interaction type 1) is answered with `PONG` after its signature is checked, so a `PING` with a bad signature is `401`, which Discord sends on purpose during URL registration and expects rejected. Both prove the URL to the platform when the operator saves it and go no further: no envelope, and no health observation unless verification fails, which is recorded like any inbound auth failure.
3. **Authenticate.** Discord: Ed25519 verification of `X-Signature-Ed25519` over `X-Signature-Timestamp` concatenated with the raw body, using the channel's `publicKey`; a timestamp more than 300s from the gateway's clock is rejected, the same replay bound the [polling endpoint](../api/async-responses.md#polling-fallback) uses. WhatsApp: HMAC-SHA256 over the raw body with `appSecret`, compared in constant time against `X-Hub-Signature-256` after stripping `sha256=`; it is the webhook adapter's `hmac` path with the header, prefix, and encoding fixed. Failures are `401` and a `failure` observation with `reason: WebhookAuthFailed`. A body that does not parse is `400`.
4. **Kind.** Only a Discord application command (type 2) and a WhatsApp message produce an envelope. A Discord component interaction gets a deferred update, an autocomplete gets an empty choice list, and a modal submit gets a fixed ephemeral refusal; none is an observation.
5. **Scope and filter.** Discord: with `guildId` or `allowedChannelIds` set, a command from elsewhere is answered at once with a fixed ephemeral message and produces no envelope. WhatsApp: `statuses` entries, events for a `phone_number_id` other than the channel's, and messages without a sender are acknowledged and dropped. Neither is a health observation; each counts on `kaalm_channel_messages_total` with `status="rejected"`.
6. **Acknowledge.** Discord: a deferred channel message (response type 5), which the person sees as the bot thinking. WhatsApp: `200` with an empty body, sent as soon as the event parses and before any filtering or delivery, so Meta's retry-on-failure never duplicates a message the gateway has accepted. The platform's request ends here. A replica dying between the acknowledgement and delivery loses the message, the same [replica-failure limitation](../api/async-responses.md#replica-failure) async webhook channels have.
7. **Normalize, activate, deliver.** One envelope per message ([Platform types](../../resources/agentchannel.md#platform-types) has the field rules), then [Request flow](overview.md#request-flow) steps 7 and 8 in async mode: the full wake budget and the full delivery retry budget apply, and no sync deadline does. As shipped the pipeline runs under the same fixed 10-minute ceiling as the webhook async pipeline. A channel whose Agent no longer exists records `AgentNotReady` and still runs the reply half, so the person sees the failure.

### Reply delivery

![Sequence diagram of a platform channel message after intake. A person sends a slash command or a message; the platform sends a signed POST to /channels/{namespace}/{path} on the User Gateway, which runs the intake checks and acknowledges. The gateway wakes the Agent if needed and POSTs /v1/message to the Agent Service with up to four attempts, receiving a reply envelope or an error. The gateway then calls SendReply against the platform API with up to four attempts per chunk. On 2xx the reply appears in the chat; on a terminal status, or when the retries are exhausted, the gateway records CallbackRejected and drops the payload.](../../diagrams/platform-channel-flow.svg)

`SendReply` for a platform channel POSTs to the platform API at the gateway-level base URL (`gateway.platforms.<type>.apiBaseUrl`). That URL is operator-set, so it is trusted the way a ModelProvider endpoint is: the `callbackUrl` deny ranges and allowlist do not apply (an in-cluster mock is a legitimate target), `http://` is accepted, and `CallbackInvalid` cannot occur on this path. TLS is verified against the callback trust pool: the system roots, plus `kaalm-ca` when `gateway.trustClusterCAForCallbacks` is set. Redirects are refused.

Each request runs the [bounded retry schedule](../api/async-responses.md#the-bounded-retry-schedule) of the callback pipeline (`gateway.callbackRetryBackoff`; as shipped each request is bounded by `gateway.agentReadTimeout` rather than `gateway.callbackReadTimeout`, both 10s by default) and lands in one of three buckets:

| Bucket | Trigger | Effect |
|---|---|---|
| Delivered | `2xx` | The reply is in the chat. `kaalm_channel_callback_total{status="delivered"}`. |
| Terminal | `400`, `401`, `403`, `404`, `405`, `410`, `415` | No retry. A `failure` observation with `reason: CallbackRejected`, a `Warning` event `reason=CallbackRejected` on the AgentChannel naming the status and the platform's error, and the payload is dropped: there is no polling store for a platform channel. `status="rejected"`. |
| Retried | Connect and TLS errors, read timeouts, and every other status (`408`, `422`, `429`, `5xx`) | 1s, 5s, 25s; 4 attempts. Exhaustion is recorded as the terminal bucket is, with `status="exhausted"`. |

`400` is terminal here where the [callback buckets](../api/async-responses.md#callback-failure-buckets) leave it unlisted, because on this path the platform has validated the body and will validate it the same way again; the two platform conditions that matter both arrive as `400` (Discord's `Unknown Interaction`, Meta's error `131047` for a reply outside the 24-hour window). The one exception is Meta's rate-limit code `130429`, which also arrives inside a `400` and is retried.

The two adapters differ only in what a reply is:

- **Discord** edits the interaction's original deferred message with the first 2000 characters of the reply (`PATCH .../messages/@original`) and posts each further 2000-character chunk as a follow-up. The interaction token is valid for 15 minutes. Past that window, or on a `404` from the first request, the adapter switches the whole reply to channel messages posted with the bot token from the credential Secret's `botToken` key, each chunk prefixed with a mention of the user and each run on the same schedule. Without `botToken` the switch is a terminal refusal.
- **WhatsApp** posts one text message per 4096-character chunk to `/{phoneNumberId}/messages` with the `accessToken` as bearer, in order, each delivered before the next is sent. An unreadable `accessToken` is a terminal refusal.

An error from the async pipeline (`delivery_failed`, `wake_timeout`, `controller_unavailable`, `response_too_large`) is sent as the text `"{error.type}: {error.message}"` through the same path, so the person sees a failure rather than silence. An empty agent reply is sent as `(empty reply)`. Neither counts as a success on channel health; the failure that produced it was already recorded.

---

## Channel health tracking

The gateway keeps per-channel delivery health in memory, per replica, from inbound requests on channels of every type and from outbound reply attempts (callback POSTs for async webhook channels with `callbackUrl` set, platform replies for Discord and WhatsApp channels). The controller reads it with [GET /v1/channels/health](../api/internal-endpoints.md#get-v1channelshealth) to set `status.conditions[type=PlatformConnected]` on each AgentChannel. No etcd write happens per request.

`PlatformConnected` is a **rolling-window** condition, not a last-result condition: it reflects what the channel has done in the last `gateway.channelHealthWindow` (default `5m`). A long-silent channel therefore never looks healthy on the strength of a delivery hours or days ago.

### What a replica records

Each replica keeps a list of in-window observations per registered channel path, at most 256, each `{ result: success | failure, reason, timestamp, lastError? }`. Entries older than the window are dropped on insertion and ignored on read.

| Reason | Result | Recorded when |
|---|---|---|
| `WebhookReady` | success | `POST /v1/message` returned `2xx`, on any channel type |
| `WebhookAuthFailed` | failure | inbound auth failed: webhook bearer or HMAC, a Discord signature or timestamp, a WhatsApp signature or verify token |
| `AgentNotReady` | failure | the referenced Agent does not exist, or the activator is unreachable or not configured |
| `DispatchFailed` | failure | the delivery retry schedule was exhausted |
| `CallbackInvalid` | failure | a webhook `callbackUrl` failed the pre-dial check, or its `callbackAuth` Secret could not be read |
| `CallbackRejected` | failure | a callback POST was terminally refused (`401`, `403`, `404`, `405`, `410`, `415`), or a platform reply was terminally refused or exhausted its retries |

Not recorded: a verification handshake that passes (it proves the URL, not delivery), a scope refusal (the platform sent something the channel is configured not to accept), a queued-but-undelivered message, and a callback attempt that is still being retried. Only the delivered `2xx` is a success, so a channel whose messages are accepted but never reach the agent reports `DispatchFailed`, not success. `CallbackInvalid` cannot occur for a platform channel: its reply destination is operator-set and never re-checked against the deny ranges.

From its list, each replica reports one state per channel:

| State | Meaning | Reported with |
|---|---|---|
| `success` | at least one in-window success | the newest success's reason and timestamp, plus the newest failure's `lastError` if any |
| `failure` | in-window observations exist and all are failures | the newest failure's reason, `lastError`, and timestamp |
| `empty` | no in-window observations | nothing |

Each response also carries `replicaStartedAt`. A replica younger than the window has not been alive long enough to observe a full one, so its `empty` is not evidence that the channel is silent, only that this replica cannot prove silence. The controller treats it the way the [activity API](activation-and-activity.md#activity-tracking-api) treats a young replica.

### How the controller reduces it

The `AgentChannelReconciler` queries every gateway Pod IP in parallel, with the same per-Pod-IP TLS handling (`ServerName` set to the gateway Service DNS) and the same unreachable-replica skip as the [activity-API fan-out](activation-and-activity.md#activity-tracking-api), then tries four rules in order.

![Flowchart of the reduction from per-replica health states to the PlatformConnected condition, as two rows. Collect: if no replica is reachable, keep the existing condition; otherwise collect state and replicaStartedAt from each reachable replica. Reduce, in order: any replica reports success, so True with reason WebhookReady; else any replica reports failure, so False with the newest failure's reason; else one replica has been up a full window and every replica is empty, so Unknown with reason NoRecentTraffic; else keep the existing condition.](../../diagrams/channel-health-reduction.svg)

| Rule | When | Condition written |
|---|---|---|
| 1 | any reachable replica reports `success` | `True`, `reason=WebhookReady` |
| 2 | else any reachable replica reports `failure` | `False`, the newest failure's reason and `lastError` |
| 3 | else at least one reachable replica has been up the full window, and every reachable replica reports `empty` | `Unknown`, `reason=NoRecentTraffic` |
| 4 | else, including when no replica is reachable | nothing; the existing condition stays |

Rule 3 needs both halves because an `empty` from a young replica proves nothing. Rule 4 writes nothing on purpose: it is the all-replicas-unreachable path too, and writing a state there would flap the condition on every coordinated gateway restart. Nothing in the reduction is type-specific, and the reason names are shared: `WebhookReady` is the inbound-works signal for every channel type. A persistent-connection adapter would be the case that adds connection-event observations (handshake completed, disconnect with reason); the HTTP adapters have no connection to observe.
