# Connecting Discord

A Discord channel lets people talk to your agent with a slash command. Discord
sends each command to the user gateway over HTTPS, the gateway delivers it to
your agent, and the reply appears in the Discord channel where the command was
typed. No bot process runs in your cluster and nothing keeps a connection open
to Discord: the gateway only answers requests Discord makes.

## Before you begin

- A running Agent with `service.enabled: true` (the default). See
  [Your first agent](first-agent.md).
- The user gateway reachable from the internet over HTTPS at a hostname with a
  certificate Discord can verify. Discord refuses a self-signed endpoint. See
  [Exposing it outside the cluster](connecting-a-channel.md#exposing-it-outside-the-cluster).
- A Discord application. Create one at the
  [Discord Developer Portal](https://discord.com/developers/applications), add
  a bot to it, and invite it to your server with the `applications.commands`
  scope.
- Permission to create Secrets in your agent's namespace.

## Register a slash command

Discord has no UI for commands; you register them through its API with the
bot token. Register one guild command named `ask` with a string option named
`message`. Guild commands are available at once; global commands take up to an
hour to appear.

```bash
export APP_ID=APPLICATION_ID
export GUILD_ID=SERVER_ID
export BOT_TOKEN=BOT_TOKEN

curl -sS -X POST "https://discord.com/api/v10/applications/$APP_ID/guilds/$GUILD_ID/commands" \
  -H "Authorization: Bot $BOT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"ask","description":"Ask the agent","options":[{"name":"message","description":"What to ask","type":3,"required":true}]}'
```

Replace `APPLICATION_ID` and `SERVER_ID` with the ids from the portal and
`BOT_TOKEN` with the bot token. The option name matters: the gateway takes
the value of the option named by the channel's `contentOption` (default
`message`) as the message text.

## Store the credentials

Copy the application's **Public Key**, a hex string, from the portal's
General Information page. Create a Secret next to your agent with that key.
Add `botToken` if you expect the agent to take longer than 15 minutes to
answer; see [Slow agents](#slow-agents).

```bash
kubectl create secret generic support-discord-creds \
  --namespace team-support \
  --from-literal=publicKey=PUBLIC_KEY
```

## Create the channel

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentChannel
metadata:
  name: support-discord
  namespace: team-support
spec:
  agentRef:
    name: support-assistant
  type: discord
  session:
    enabled: true
  discord:
    path: /channels/team-support/support-discord
    credentialsRef:
      name: support-discord-creds
    guildId: "123456789012345678"
```

`guildId` is optional: with it set, commands from any other server get a
short refusal and never reach the agent. The `path` follows the same rules
as a webhook channel: it starts with `/channels/{namespace}/` and never with
`/v1/`. Apply the manifest and wait for the `Phase` column to read `Active`:

```bash
kubectl get agentchannel support-discord -n team-support
```

A channel whose Secret is missing the key, or whose key is not a valid
Ed25519 public key, reports `Ready=False` with reason `CredentialsMissing` or
`CredentialsInvalid`. Fix the Secret; the reconciler re-checks within a
minute.

## Point Discord at the channel

In the portal's General Information page, set **Interactions Endpoint URL**
to your gateway hostname plus the channel path:

```
https://bots.example.com/channels/team-support/support-discord
```

When you save, Discord sends a verification `PING` and a deliberately
badly-signed request; the gateway answers the first with `PONG` and the second
with `401`, and Discord accepts the URL. Save only after the channel is
`Active`: an unregistered path answers `401` to everything and the save fails.
As shipped the badly-signed probe is recorded as a `WebhookAuthFailed`
failure, so `PlatformConnected` reads `False` for up to the health window
(five minutes by default) after a successful save; the first real command
clears it.

## Try it

In your server, type `/ask message: Where is my order?`. The bot shows as
thinking at once; the reply replaces that message when the agent answers.
With `session.enabled: true`, every command from the same person carries the
same session ID, so the agent can keep a conversation going.

The agent receives a message envelope with `channelType: discord`, the
person's Discord user ID as `userId`, and the option's text as `content`; the
design book's Discord channel page lists every envelope field.

![Sequence diagram of a platform channel message after intake. A person sends a slash command or a message; the platform sends a signed POST to /channels/{namespace}/{path} on the User Gateway, which runs the intake checks and acknowledges. The gateway wakes the Agent if needed and POSTs /v1/message to the Agent Service with up to four attempts, receiving a reply envelope or an error. The gateway then calls SendReply against the platform API with up to four attempts per chunk. On 2xx the reply appears in the chat; on a terminal status, or when the retries are exhausted, the gateway records CallbackRejected and drops the payload.](../diagrams/platform-channel-flow.svg)

## Slow agents

Discord's reply token lasts 15 minutes from the command. If your agent can
take longer, add `botToken` to the credential Secret:

```bash
kubectl create secret generic support-discord-creds \
  --namespace team-support \
  --from-literal=publicKey=PUBLIC_KEY \
  --from-literal=botToken=BOT_TOKEN
```

When the token has expired, the gateway posts the reply as a normal message
in the channel, mentioning the person who asked. Without `botToken`, a late
reply is dropped and the channel's `PlatformConnected` condition reports
`CallbackRejected`.

## When something goes wrong

`kubectl describe agentchannel support-discord -n team-support` shows the
`PlatformConnected` condition, the gateway's view of recent traffic:

- `WebhookAuthFailed`: Discord's signatures do not verify. The `publicKey` in
  the Secret does not match the application, or the interactions URL points
  at a different channel. Within five minutes of saving the URL, it is the
  save-time probe, not a misconfiguration.
- `CallbackRejected`: Discord refused the reply. The message names the HTTP
  status; a `404` means the reply token had expired (see
  [Slow agents](#slow-agents)).
- `DispatchFailed` or `AgentNotReady`: the agent did not answer. The person
  sees the error as the bot's reply, for example
  `delivery_failed: ...`, rather than silence.

A command Discord shows as "The application did not respond" never reached
the gateway, or reached it after Discord's 3-second window. Check the
Ingress first.

---

*How this works: design book pages Resources, AgentChannel (the Platform
types section), Gateways, User, Platform adapters and channel health (the inbound steps and reply
delivery), and Gateways, API, Discord channel (the wire contract on both
sides).*
