# Discord Channel

*(Since v0.7.0.)* `POST /channels/{namespace}/{channel-path}` for an AgentChannel of `spec.type: discord` is the Discord application's **Interactions Endpoint URL**. Discord POSTs every interaction for the application to it: the verification `PING` when the operator saves the URL, and each slash command a person invokes. The gateway acknowledges within Discord's 3-second budget and delivers the agent's reply later through the interaction's follow-up webhook.

The channel fields are in [Platform types](../../resources/agentchannel.md#discord); the adapter mechanics and the reply delivery buckets are in [The platform adapters](../user/platform-adapters.md#the-platform-adapters). This page is the wire contract on both sides.

## Path shape and exposure

`spec.discord.path` follows the webhook path rules unchanged: it must begin with `/channels/{namespace}/` (rule 15) and must not begin with `/v1/` (rule 16). The endpoint is served on the User Gateway listener (`:8080`) behind the cluster Ingress, exactly as the [channel webhook](channel-webhook.md#path-shape-and-exposure) is. Discord requires the registered URL to be reachable over HTTPS with a certificate it can verify, which the Ingress provides.

Routing is gated on `AgentChannel.status.conditions[type=Ready].status == True`, so apply the channel and wait for `Ready` before saving the URL in the Developer Portal; a URL that answers `401` fails Discord's save-time check.

## Inbound request

Every request carries:

| Header | Value |
|---|---|
| `X-Signature-Ed25519` | Ed25519 signature, 64 bytes hex-encoded |
| `X-Signature-Timestamp` | Unix seconds, as a decimal string |
| `Content-Type` | `application/json` |

**Verification.** The gateway checks `Ed25519.Verify(publicKey, timestamp || body, signature)` with the raw body bytes and the channel's `publicKey`, then rejects a timestamp more than 300s from its own clock. Either failure is `401` with the generic [User Gateway error envelope](errors.md#user-gateway-error-responses), the same `401` an unregistered path gets. Discord's save-time check sends one valid `PING` and one request with an invalid signature and requires the `401` on the second, so the rejection is part of the contract, not only a defense.

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

In a DM the sender is `user` rather than `member.user`. Bodies above `gateway.maxMessageBodyBytes` are `413` before anything else, as on every `:8080` path.

## Inbound responses

The response depends on the interaction `type`:

| Interaction | Response | Meaning |
|---|---|---|
| `1` PING | `200` `{"type": 1}` | Verification handshake. No envelope, no health observation. |
| `2` APPLICATION_COMMAND, in scope | `200` `{"type": 5}` | Deferred channel message: the person sees the bot thinking; the reply arrives through the follow-up webhook. The envelope is dispatched. |
| `2` APPLICATION_COMMAND, out of scope | `200` `{"type": 4, "data": {"content": "This bot is not available here.", "flags": 64}}` | Ephemeral refusal for an interaction outside `guildId` or `allowedChannelIds`. No envelope. |
| `3` MESSAGE_COMPONENT | `200` `{"type": 6}` | Deferred update, nothing else: components are out of scope. |
| `4` APPLICATION_COMMAND_AUTOCOMPLETE | `200` `{"type": 8, "data": {"choices": []}}` | No choices: autocomplete is out of scope. |
| `5` MODAL_SUBMIT | `200` `{"type": 4, "data": {"content": "Modals are not supported.", "flags": 64}}` | Ephemeral, no envelope. |
| any | `401` | Signature or timestamp rejected, or the path is not registered to a `Ready=True` channel |
| any | `413` `request_too_large` | Body over `gateway.maxMessageBodyBytes` |

Every `200` above is returned inside Discord's 3-second window, before the agent is involved.

## Normalization

An in-scope command becomes one envelope:

```json
{
  "messageId": "8a1c2e3d-...",
  "channelType": "discord",
  "channelId": "/channels/team-support/support-discord",
  "userId": "555555555555555555",
  "sessionId": "…",
  "content": "Where is my order?",
  "attachments": [
    { "type": "discord.attachment", "id": "1300000000000000000", "url": "https://cdn.discordapp.com/…", "filename": "receipt.png", "contentType": "image/png", "size": 48213 }
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

`content` is the string value of the option named by `spec.discord.contentOption` (default `message`), or the empty string when the command has no such option. `attachments` has one entry per option of the attachment type, resolved from `data.resolved.attachments`; the gateway never fetches the URL. `metadata.options` carries every option by name so a multi-option command remains usable. `sessionId` follows the [session derivation](agent-endpoints.md#session-identity-the-sessionid-derivation) with `channelId` the channel path and `userId` the Discord user.

## Reply requests

The gateway sends the reply to `gateway.platforms.discord.apiBaseUrl` (default `https://discord.com/api/v10`). The interaction token authenticates these requests; no bot token is needed.

1. The first 2000 characters replace the deferred message:

   ```
   PATCH {apiBaseUrl}/webhooks/{application_id}/{token}/messages/@original
   Content-Type: application/json

   {"content": "<first chunk>"}
   ```

2. Each further 2000-character chunk is a follow-up:

   ```
   POST {apiBaseUrl}/webhooks/{application_id}/{token}
   Content-Type: application/json

   {"content": "<next chunk>"}
   ```

The token is valid for 15 minutes from the interaction; after that both requests answer `404`. When the credential Secret carries `botToken`, a `404` on the first request switches the reply to channel messages, one per chunk:

```
POST {apiBaseUrl}/channels/{channel_id}/messages
Authorization: Bot {botToken}
Content-Type: application/json

{"content": "<@555555555555555555> <chunk>", "allowed_mentions": {"users": ["555555555555555555"]}}
```

Without `botToken`, the `404` is terminal. The buckets (delivered, terminal, retried), the retry schedule, and how a terminal refusal reaches channel health are in [Reply delivery](../user/platform-adapters.md#reply-delivery). An error payload from the async pipeline is sent through the same requests as the text `"{error.type}: {error.message}"`.
