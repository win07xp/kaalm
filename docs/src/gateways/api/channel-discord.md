# Discord channel

`POST /channels/{namespace}/{channel-path}` for an AgentChannel of `spec.type: discord` is the Discord application's **Interactions Endpoint URL**. Discord posts every interaction for the application to it: the verification `PING` when the operator saves the URL, and each slash command a person invokes. The gateway acknowledges within Discord's 3-second budget and sends the agent's reply later through the interaction's follow-up webhook.

The channel fields are in [Platform types](../../resources/agentchannel.md#discord). The adapter mechanics and the reply delivery buckets are in [The platform adapters](../user/platform-adapters.md#the-platform-adapters). This page is the wire contract on both sides.

## Path rules and exposure

`spec.discord.path` follows the [channel webhook path rules](channel-webhook.md#path-rules-and-exposure) unchanged, and the route is served on the same `:8080` listener behind the Ingress. Discord requires the registered URL to be reachable over HTTPS with a certificate it can verify, which the Ingress provides.

Apply the channel and wait for `Ready=True` before saving the URL in the Developer Portal. A path that is not routed answers `401`, which fails Discord's save-time check.

## Inbound request

Every request carries:

| Header | Value |
|---|---|
| `X-Signature-Ed25519` | Ed25519 signature, 64 bytes hex-encoded |
| `X-Signature-Timestamp` | Unix seconds, as a decimal string |
| `Content-Type` | `application/json` |

**Verification.** The gateway loads the channel's `publicKey`, checks `Ed25519.Verify(publicKey, timestamp || body, signature)` over the raw body bytes, and rejects a timestamp more than 300s from its own clock. A failed check, or a `publicKey` that is missing or not a 32-byte hex string, answers `401` with the [User Gateway error envelope](errors.md#user-gateway-error-responses) and records a `WebhookAuthFailed` health observation.

Discord's save-time check sends one valid `PING` and one request with an invalid signature and requires the `401` on the second, so the rejection is part of the contract. As shipped, that probe's `401` also counts as a health failure, so a newly registered channel reports `WebhookAuthFailed` for one health window; issue #228 tracks it.

**Body.** The interaction object. The fields the adapter reads:

```json
{
  "id": "1290000000000000001",
  "application_id": "1230000000000000000",
  "type": 2,
  "token": "aW50ZXJhY3Rpb246...",
  "guild_id": "123456789012345678",
  "channel_id": "987654321098765432",
  "member": { "user": { "id": "555555555555555555", "username": "dev" } },
  "data": {
    "name": "ask",
    "options": [ { "name": "message", "type": 3, "value": "Where is my order?" } ]
  },
  "locale": "en-US"
}
```

In a DM the sender is `user` rather than `member.user`.

## Inbound responses

| Condition | Response |
|---|---|
| `POST` body over `gateway.maxMessageBodyBytes` | `413 request_too_large`, before the path is resolved |
| Path not routed, or method not `POST` | `401 unauthorized` |
| Signature, timestamp, or `publicKey` rejected | `401 unauthorized` |
| Body is not JSON | `400 invalid_request` |
| `type` 1, PING | `200` `{"type": 1}`. No envelope, no health observation. |
| `type` 2, command in scope | `200` `{"type": 5}`, a deferred message: the person sees the bot thinking. One envelope is dispatched. |
| `type` 2, command out of scope | `200` `{"type": 4, "data": {"content": "This bot is not available here.", "flags": 64}}`, an ephemeral refusal for an interaction outside `guildId` or `allowedChannelIds`. No envelope. |
| `type` 3, message component | `200` `{"type": 6}`, a deferred update. Components are out of scope. |
| `type` 4, autocomplete | `200` `{"type": 8, "data": {"choices": []}}`. Autocomplete is out of scope. |
| `type` 5, modal submit | `200` `{"type": 4, "data": {"content": "Modals are not supported.", "flags": 64}}`. No envelope. |
| Any other `type` | `400 invalid_request` |

Every `200` is returned inside Discord's 3-second window, before the agent is involved. Refused and unknown interactions count on `kaalm_channel_messages_total` with `status="rejected"`.

## Normalization

An in-scope command becomes one envelope:

```json
{
  "messageId": "8a1c2e3d-...",
  "channelType": "discord",
  "channelId": "/channels/team-support/support-discord",
  "userId": "555555555555555555",
  "sessionId": "...",
  "content": "Where is my order?",
  "attachments": [
    { "type": "discord.attachment", "id": "1300000000000000000", "url": "https://cdn.discordapp.com/...", "filename": "receipt.png", "contentType": "image/png", "size": 48213 }
  ],
  "metadata": {
    "interactionId": "1290000000000000001",
    "applicationId": "1230000000000000000",
    "guildId": "123456789012345678",
    "channelId": "987654321098765432",
    "command": "ask",
    "options": { "message": "Where is my order?" },
    "locale": "en-US"
  }
}
```

| Field | Source |
|---|---|
| `content` | The string value of the option named by `spec.discord.contentOption` (default `message`), or the empty string when the command has no such option |
| `attachments` | One entry per option of the attachment type, resolved from `data.resolved.attachments`. The gateway never fetches the URL. |
| `metadata.options` | Every option by name, so a multi-option command remains usable |
| `sessionId` | Present only when `spec.session.enabled` is `true`, derived per [Session identity](agent-endpoints.md#session-identity-the-sessionid-derivation) from the channel path and the Discord user id |

## Reply requests

The gateway sends the reply to `gateway.platforms.discord.apiBaseUrl` (default `https://discord.com/api/v10`), split into chunks of at most 2000 characters. A chunk breaks at the last newline in the second half of its window when there is one, so a long reply reads as continued paragraphs. An empty reply is sent as the literal text `(empty reply)`. An error from the delivery pipeline is sent as the text `{error.type}: {error.message}`.

The interaction token authenticates the first two request shapes; no bot token is needed for them.

1. The first chunk replaces the deferred message:

   ```
   PATCH {apiBaseUrl}/webhooks/{application_id}/{token}/messages/@original
   Content-Type: application/json

   {"content": "<first chunk>"}
   ```

2. Each further chunk is a follow-up:

   ```
   POST {apiBaseUrl}/webhooks/{application_id}/{token}
   Content-Type: application/json

   {"content": "<next chunk>"}
   ```

The token is valid for 15 minutes from the interaction. The gateway switches to channel messages, one per chunk and authenticated with the credential Secret's `botToken`, in two cases: more than 15 minutes have elapsed since the interaction was received, or the first request answered `404`:

```
POST {apiBaseUrl}/channels/{channel_id}/messages
Authorization: Bot {botToken}
Content-Type: application/json

{"content": "<@555555555555555555> <chunk>", "allowed_mentions": {"users": ["555555555555555555"]}}
```

Without `botToken`, either case is a terminal refusal recorded as `CallbackRejected`. The buckets (delivered, terminal, retried), the retry schedule, and how a terminal refusal reaches channel health are in [Reply delivery](../user/platform-adapters.md#reply-delivery).
