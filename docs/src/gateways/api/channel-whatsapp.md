# WhatsApp channel

`/channels/{namespace}/{channel-path}` for an AgentChannel of `spec.type: whatsapp` is the Meta app's **webhook callback URL** for the WhatsApp Cloud API. `GET` is the verification handshake Meta performs when the operator saves the URL; `POST` carries the events. The gateway answers each verified `POST` with `200` at once and sends the agent's reply later as a new message through the Graph API.

The channel fields are in [Platform types](../../resources/agentchannel.md#whatsapp). The adapter mechanics and the reply delivery buckets are in [The platform adapters](../user/platform-adapters.md#the-platform-adapters). This page is the wire contract on both sides.

## Path rules and exposure

`spec.whatsapp.path` follows the [channel webhook path rules](channel-webhook.md#path-rules-and-exposure) unchanged, and the route is served on the same `:8080` listener behind the Ingress. Meta requires HTTPS with a certificate it can verify.

Apply the channel and wait for `Ready=True` before saving the URL in the app dashboard. The verification `GET` against a path that is not routed answers `401`.

## Verification handshake

```
GET /channels/team-support/support-whatsapp?hub.mode=subscribe&hub.verify_token=<token>&hub.challenge=1158201444
```

| Condition | Response |
|---|---|
| Path not routed | `401 unauthorized` |
| `hub.mode=subscribe` and `hub.verify_token` equals the channel's `verifyToken` | `200`, `Content-Type: text/plain`, body is the `hub.challenge` value verbatim |
| `hub.verify_token` does not match, `hub.mode` is anything else, or `verifyToken` cannot be read | `403` with `error.type: unauthorized` |

The token comparison is constant-time. The handshake produces no envelope. A mismatch is recorded as a `WebhookAuthFailed` health observation. The `403` reuses the `unauthorized` type that every `401` on this listener carries; issue #232 tracks the mismatch.

## Inbound event

```
POST /channels/team-support/support-whatsapp
Content-Type: application/json
X-Hub-Signature-256: sha256=<hex HMAC-SHA256 over the raw body>
```

**Verification.** The gateway strips `sha256=`, decodes hex, and compares in constant time against HMAC-SHA256 of the raw body bytes keyed with the channel's `appSecret`. It is the [webhook `hmac` verifier](channel-webhook.md#auth) with the header, prefix, and encoding fixed. The signature covers no timestamp, so this surface has the same replay protection as the generic webhook, with `metadata.messageId` carrying Meta's message id for agents that deduplicate.

**Body.** The fields the adapter reads:

```json
{
  "object": "whatsapp_business_account",
  "entry": [ {
    "id": "102290129340398",
    "changes": [ {
      "field": "messages",
      "value": {
        "messaging_product": "whatsapp",
        "metadata": { "display_phone_number": "15550001234", "phone_number_id": "106540352242922" },
        "contacts": [ { "profile": { "name": "Dev" }, "wa_id": "15551234567" } ],
        "messages": [ {
          "from": "15551234567",
          "id": "wamid.HBgLMTU1NTEyMzQ1NjcVAgASGBQzQTdC...",
          "timestamp": "1756100000",
          "type": "text",
          "text": { "body": "Where is my order?" }
        } ]
      }
    } ]
  } ]
}
```

## Inbound responses

| Condition | Response |
|---|---|
| `POST` body over `gateway.maxMessageBodyBytes` | `413 request_too_large`, before the path is resolved |
| Path not routed, or method other than `GET` or `POST` | `401 unauthorized` |
| Signature rejected, or `appSecret` cannot be read | `401 unauthorized` |
| Body is not JSON | `400 invalid_request` |
| Signature verified and body parsed | `200`, empty body |

The `200` is written before any message is examined or delivered, so Meta's retry-on-failure never duplicates a message the gateway accepted. Behind it, every entry in `entry[].changes[].value.messages[]` whose `value.metadata.phone_number_id` equals the channel's `phoneNumberId` becomes one envelope, in order. Everything else is dropped and counted on `kaalm_channel_messages_total` with `status="rejected"`: `statuses` entries, messages for another number, and messages that fail to parse or carry no `from`. Dropped events are not health observations.

## Normalization

```json
{
  "messageId": "8a1c2e3d-...",
  "channelType": "whatsapp",
  "channelId": "/channels/team-support/support-whatsapp",
  "userId": "15551234567",
  "sessionId": "...",
  "content": "Where is my order?",
  "attachments": [],
  "metadata": {
    "messageId": "wamid.HBgLMTU1NTEyMzQ1NjcVAgASGBQzQTdC...",
    "timestamp": "1756100000",
    "phoneNumberId": "106540352242922",
    "displayPhoneNumber": "15550001234",
    "profileName": "Dev",
    "messageType": "text",
    "message": { "from": "15551234567", "id": "wamid....", "timestamp": "1756100000", "type": "text", "text": { "body": "Where is my order?" } }
  }
}
```

| Message `type` | `content` | `attachments` |
|---|---|---|
| `text` | `text.body` | `[]` |
| `interactive` | `interactive.button_reply.title` or `interactive.list_reply.title` | `[]` |
| `image`, `document`, `audio`, `video`, `sticker` | The media caption, or the empty string | One reference: `{ "type": "whatsapp.<type>", "id": "<media id>", "mimeType": "...", "sha256": "..." }`. The gateway does not download media. |
| Any other | The empty string | `[]` |

`metadata.message` is the raw message object in every case. `sessionId` is present only when `spec.session.enabled` is `true`, derived per [Session identity](agent-endpoints.md#session-identity-the-sessionid-derivation) from the channel path and the `wa_id`.

## Reply requests

The gateway splits the reply into chunks of at most 4096 characters, breaking at the last newline in the second half of each window when there is one, and sends one request per chunk, in order, to `gateway.platforms.whatsapp.apiBaseUrl` (the chart default names a Graph API version). An empty reply is sent as the literal text `(empty reply)`. An error from the delivery pipeline is sent as the text `{error.type}: {error.message}`.

```
POST {apiBaseUrl}/{phoneNumberId}/messages
Authorization: Bearer {accessToken}
Content-Type: application/json

{
  "messaging_product": "whatsapp",
  "recipient_type": "individual",
  "to": "15551234567",
  "type": "text",
  "text": { "preview_url": false, "body": "<chunk>" }
}
```

A `200` carries `{"messages": [{"id": "wamid...."}]}`.

| Meta answers | Bucket |
|---|---|
| `400` with error code `131047`: free-form reply more than 24 hours after the person's last message | Terminal. There is no fallback, because the alternative, a template message, is not text the agent wrote. |
| `401`: bad `accessToken` | Terminal |
| `429`, or `400` with error code `130429`: rate limited | Retried |

The buckets, the retry schedule, and how a terminal refusal reaches channel health are in [Reply delivery](../user/platform-adapters.md#reply-delivery).
