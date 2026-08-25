# WhatsApp Channel

*(Since v0.7.0.)* `/channels/{namespace}/{channel-path}` for an AgentChannel of `spec.type: whatsapp` is the Meta app's **webhook callback URL** for the WhatsApp Cloud API. `GET` is the verification handshake Meta performs when the operator saves the URL; `POST` carries the events. The gateway answers each `POST` with `200` at once and delivers the agent's reply later as a new message through the Graph API.

The channel fields are in [Platform types](../../resources/agentchannel.md#whatsapp); the adapter mechanics and the reply delivery buckets are in [The platform adapters](../user/platform-adapters.md#the-platform-adapters). This page is the wire contract on both sides.

## Path shape and exposure

`spec.whatsapp.path` follows the webhook path rules unchanged: it must begin with `/channels/{namespace}/` (rule 15) and must not begin with `/v1/` (rule 16). The endpoint is served on the User Gateway listener (`:8080`) behind the cluster Ingress, exactly as the [channel webhook](channel-webhook.md#path-shape-and-exposure) is; Meta requires HTTPS with a certificate it can verify.

Routing is gated on `AgentChannel.status.conditions[type=Ready].status == True`, so apply the channel and wait for `Ready` before saving the URL in the app dashboard; the verification `GET` against a path that is not yet routed answers `401`.

## Verification handshake

```
GET /channels/team-support/support-whatsapp?hub.mode=subscribe&hub.verify_token=<token>&hub.challenge=1158201444
```

| Condition | Response |
|---|---|
| `hub.mode=subscribe` and `hub.verify_token` equals the channel's `verifyToken` | `200`, `Content-Type: text/plain`, body is the `hub.challenge` value verbatim |
| `hub.verify_token` does not match, or `hub.mode` is anything else | `403` |
| Path not registered to a `Ready=True` channel | `401` |

The comparison is constant-time. The handshake produces no envelope and no health observation; a mismatched token is a `failure` with `reason: WebhookAuthFailed`.

## Inbound event

```
POST /channels/team-support/support-whatsapp
Content-Type: application/json
X-Hub-Signature-256: sha256=<hex HMAC-SHA256 over the raw body>
```

**Verification.** The gateway strips `sha256=`, decodes hex, and constant-time-compares against HMAC-SHA256 of the raw body bytes keyed with the channel's `appSecret`. It is the webhook adapter's `hmac` verifier with the header, prefix, and encoding fixed. Failure is `401` with the generic [User Gateway error envelope](errors.md#user-gateway-error-responses). The signature covers no timestamp, so this surface has the same replay posture as the [generic webhook](channel-webhook.md#auth): bounded by budgets, deduplicated by the agent if it cares, with `metadata.messageId` carrying Meta's message ID for that purpose.

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

Bodies above `gateway.maxMessageBodyBytes` are `413` before anything else, as on every `:8080` path.

## Inbound responses

| Condition | Response |
|---|---|
| Signature verified | `200`, empty body, always: whether the event carried messages, only `statuses`, or messages for another `phone_number_id`. The `200` is sent before any delivery is attempted, so Meta's retry-on-failure never duplicates a message the gateway accepted. |
| Signature rejected | `401` |
| Path not registered to a `Ready=True` channel | `401` |
| Body over `gateway.maxMessageBodyBytes` | `413` `request_too_large` |

What the `200` covers: every message in `entry[].changes[].value.messages[]` whose `value.metadata.phone_number_id` equals the channel's `phoneNumberId` becomes one envelope, in order; `statuses` entries and other numbers' messages are dropped. Dropped events count on `kaalm_channel_messages_total` with `status="rejected"` and are not health observations.

## Normalization

```json
{
  "messageId": "8a1c2e3d-...",
  "channelType": "whatsapp",
  "channelId": "/channels/team-support/support-whatsapp",
  "userId": "15551234567",
  "sessionId": "…",
  "content": "Where is my order?",
  "attachments": [],
  "metadata": {
    "messageId": "wamid.HBgLMTU1NTEyMzQ1NjcVAgASGBQzQTdC...",
    "timestamp": "1756100000",
    "phoneNumberId": "106540352242922",
    "displayPhoneNumber": "15550001234",
    "profileName": "Dev",
    "messageType": "text",
    "message": { "from": "15551234567", "id": "wamid.…", "timestamp": "1756100000", "type": "text", "text": { "body": "Where is my order?" } }
  }
}
```

`content` by message type: `text` gives `text.body`; `interactive` gives `interactive.button_reply.title` or `interactive.list_reply.title`; `image`, `document`, `audio`, `video`, and `sticker` give the media caption or the empty string, with one attachment reference `{ "type": "whatsapp.<type>", "id": "<media id>", "mimeType": "…", "sha256": "…" }` (the gateway does not download media); every other type gives the empty string. `metadata.message` is the raw message object in every case. `sessionId` follows the [session derivation](agent-endpoints.md#session-identity-the-sessionid-derivation) with `channelId` the channel path and `userId` the `wa_id`.

## Reply requests

The gateway sends one request per 4096-character chunk, in order, to `gateway.platforms.whatsapp.apiBaseUrl` (the chart default names a Graph API version):

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

A `200` carries `{"messages": [{"id": "wamid.…"}]}`. Meta refuses a free-form reply more than 24 hours after the person's last message with `400` and error code `131047`; that is terminal, and there is no fallback, because the alternative (a template message) is not text the agent wrote. A bad `accessToken` is `401`, also terminal. Rate limiting (`429`, or `400` with error code `130429`) is retried. The buckets, the retry schedule, and how a terminal refusal reaches channel health are in [Reply delivery](../user/platform-adapters.md#reply-delivery). An error payload from the async pipeline is sent through the same request as the text `"{error.type}: {error.message}"`.
