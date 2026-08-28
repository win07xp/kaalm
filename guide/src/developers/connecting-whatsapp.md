# Connecting WhatsApp

A WhatsApp channel lets customers message your agent on a WhatsApp business
number. Meta delivers each message to the user gateway over HTTPS, the
gateway delivers it to your agent, and the reply goes back to the customer as
a message from the business number. Nothing in your cluster keeps a
connection open to Meta: the gateway answers Meta's webhook calls and makes
one Graph API call per reply.

## Before you begin

- A running Agent with `service.enabled: true` (the default). See
  [Your First Agent](first-agent.md).
- The user gateway reachable from the internet over HTTPS at a hostname with
  a certificate Meta can verify. See
  [Exposing it outside the cluster](connecting-a-channel.md#exposing-it-outside-the-cluster).
- A Meta app with the WhatsApp product added, a business phone number, and
  a permanent access token for a system user with the
  `whatsapp_business_messaging` permission. Meta's
  [Cloud API getting started](https://developers.facebook.com/docs/whatsapp/cloud-api/get-started)
  covers that setup.
- Permission to create Secrets in your agent's namespace.

You need three values from the app: the **app secret** (App settings, Basic),
the **phone number ID** (WhatsApp, API Setup), and the **access token**. You
choose a fourth yourself: the **verify token**, any string, which Meta echoes
back when it verifies your webhook URL.

## Store the credentials

```bash
kubectl create secret generic support-whatsapp-creds \
  --namespace team-support \
  --from-literal=verifyToken=<a string you choose> \
  --from-literal=appSecret=<app secret> \
  --from-literal=accessToken=<access token>
```

All three keys are required; a channel whose Secret is missing one reports
`Ready=False` with reason `CredentialsMissing`.

## Create the channel

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentChannel
metadata:
  name: support-whatsapp
  namespace: team-support
spec:
  agentRef:
    name: support-assistant
  type: whatsapp
  session:
    enabled: true
  whatsapp:
    path: /channels/team-support/support-whatsapp
    credentialsRef:
      name: support-whatsapp-creds
    # The business number this channel answers as. Events for other numbers
    # under the same app are acknowledged and dropped.
    phoneNumberId: "106540352242922"
```

The `path` follows the same rules as a webhook channel: it starts with
`/channels/{namespace}/` and never with `/v1/`. Apply the manifest and wait
for `Ready`:

```bash
kubectl get agentchannel support-whatsapp -n team-support -o wide
```

## Point Meta at the channel

In the app dashboard, under WhatsApp, Configuration, set the webhook
**Callback URL** to your gateway hostname plus the channel path, and the
**Verify token** to the value you stored:

```
https://bots.example.com/channels/team-support/support-whatsapp
```

When you click **Verify and save**, Meta sends a `GET` with your verify
token and a challenge; the gateway echoes the challenge and Meta accepts
the URL. Then subscribe the webhook to the **messages** field. Do this only
after the channel is `Ready`: an unregistered path answers `401` and the
verification fails.

## Try it

Send a message to the business number from a phone. Meta posts the event,
the gateway acknowledges it at once, delivers it to your agent, and posts
the reply back to the customer. With `session.enabled: true`, every message
from the same phone number carries the same session ID, so the agent can
keep a conversation going.

The agent receives a message envelope with `channelType: whatsapp`, the
customer's WhatsApp ID as `userId`, the message text as `content` (for a
button or list reply, the chosen title; for a photo or document, the
caption), and the customer's profile name, the message ID, and the raw
message under `metadata`. Media arrives as a reference with the media ID
and MIME type; the gateway does not download it.

## The 24-hour window

Meta lets a business send free-form text only within 24 hours of the
customer's last message. A reply the agent produces after that is refused
by Meta and the channel's `PlatformConnected` condition reports
`CallbackRejected` with Meta's error code (`131047`). Kaalm does not send
template messages; if your agent can take longer than a day to answer, it
should say so within the window.

## When something goes wrong

`kubectl describe agentchannel support-whatsapp -n team-support` shows the
`PlatformConnected` condition, the gateway's view of recent traffic:

- `WebhookAuthFailed`: Meta's signatures do not verify, or the verify token
  did not match. The `appSecret` or `verifyToken` in the Secret does not
  match the app.
- `CallbackRejected`: Meta refused the reply. The message names the HTTP
  status and Meta's error code: `131047` is the 24-hour window, a `401` is
  a bad or expired `accessToken`.
- `DispatchFailed` or `AgentNotReady`: the agent did not answer. The
  customer sees the error as the reply, for example `delivery_failed: ...`,
  rather than silence.

Delivery receipts for the agent's replies arrive at the same URL as
`statuses` events; the gateway acknowledges and drops them, so they never
reach the agent.

---

*How this works: design book pages Resources, AgentChannel (the Platform
types section), Gateways, User, Platform Adapters (the inbound steps and reply
delivery), and Gateways, API, WhatsApp Channel (the wire contract on both
sides).*
