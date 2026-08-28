# Connecting Discord

A Discord channel lets people talk to your agent with a slash command. Discord
sends each command to the user gateway over HTTPS, the gateway delivers it to
your agent, and the reply appears in the Discord channel where the command was
typed. No bot process runs in your cluster and nothing keeps a connection open
to Discord: the gateway only answers requests Discord makes.

## Before you begin

- A running Agent with `service.enabled: true` (the default). See
  [Your First Agent](first-agent.md).
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
export APP_ID=<application id>
export GUILD_ID=<your server id>
export BOT_TOKEN=<bot token from the portal>

curl -sS -X POST "https://discord.com/api/v10/applications/$APP_ID/guilds/$GUILD_ID/commands" \
  -H "Authorization: Bot $BOT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"ask","description":"Ask the agent","options":[{"name":"message","description":"What to ask","type":3,"required":true}]}'
```

The option name matters: the gateway takes the value of the option named by
the channel's `contentOption` (default `message`) as the message text.

## Store the credentials

Copy the application's **Public Key** from the portal's General Information
page. Create a Secret next to your agent with that key. Add `botToken` if you
expect the agent to take longer than 15 minutes to answer; see
[Slow agents](#slow-agents).

```bash
kubectl create secret generic support-discord-creds \
  --namespace team-support \
  --from-literal=publicKey=<public key, hex>
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
    # Optional. Commands from any other server get a short refusal and never
    # reach the agent.
    guildId: "123456789012345678"
```

The `path` follows the same rules as a webhook channel: it starts with
`/channels/{namespace}/` and never with `/v1/`. Apply the manifest and wait
for `Ready`:

```bash
kubectl get agentchannel support-discord -n team-support -o wide
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
`Ready`: an unregistered path answers `401` to everything and the save fails.

## Try it

In your server, type `/ask message: Where is my order?`. The bot shows as
thinking at once; the reply replaces that message when the agent answers.
With `session.enabled: true`, every command from the same person carries the
same session ID, so the agent can keep a conversation going.

The agent receives a message envelope with `channelType: discord`, the
person's Discord user ID as `userId`, the option's text as `content`, and the
command name, guild, channel, and other options under `metadata`. Attachments
arrive as references with a CDN URL; the gateway does not download them.

## Slow agents

Discord's reply token lasts 15 minutes from the command. If your agent can
take longer, add `botToken` to the credential Secret:

```bash
kubectl create secret generic support-discord-creds \
  --namespace team-support \
  --from-literal=publicKey=<public key, hex> \
  --from-literal=botToken=<bot token>
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
  at a different channel.
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
types section), Gateways, User, Platform Adapters (the inbound steps and reply
delivery), and Gateways, API, Discord Channel (the wire contract on both
sides).*
