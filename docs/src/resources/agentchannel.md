# AgentChannel

AgentChannel is a namespace-scoped resource that connects a running Agent to a user-facing communication channel. Three channel types exist: `webhook`, a generic inbound HTTP POST with configurable auth, and the platform adapters `discord` (the Discord Interactions endpoint) and `whatsapp` (the WhatsApp Cloud API webhook). All three are inbound HTTP; the gateway holds no persistent connection to any platform. The webhook spec follows; the two platform blocks are under [Platform types](#platform-types).

## Spec

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentChannel
metadata:
  name: support-webhook
  namespace: team-support
spec:
  # Required. An Agent in this namespace, never an AgentTask (rules 13, 14).
  agentRef:
    name: support-assistant

  # "webhook" (schema default) | "discord" | "whatsapp". Exactly the block
  # matching the type must be set (rule 39, apply time).
  type: webhook

  webhook:
    # Must begin with /channels/{namespace}/ for this namespace and be
    # unique in it (rule 15, reconcile time), and must not begin with /v1/
    # (rule 16, apply time).
    path: /channels/team-support/support-assistant
    # Required. "bearer" needs secretRef; "hmac" needs the hmac block
    # (apply time).
    auth:
      type: bearer
      secretRef: { name: webhook-secret, key: token }
      # type: hmac
      # hmac:
      #   header: "X-Hub-Signature-256"
      #   algorithm: sha256              # "sha256" (default) | "sha1"
      #   secretRef: { name: webhook-hmac-secret, key: secret }
      #   signaturePrefix: "sha256="     # optional, stripped before decoding
      #   encoding: hex                  # "hex" (default) | "base64"
    # Optional. Where the sender's identity comes from: a header, or a
    # dotted path into a JSON body. At most one of the two (apply time).
    userId:
      fromHeader: "X-User-Id"
      # fromBody: "user.id"
      fallback: "anonymous"
    # Optional. Where the message text comes from. Same fields. When neither
    # is set, the raw body is the content.
    content:
      fromHeader: "X-Message-Text"
      # fromBody: "message.text"
      # fallback: ""
    # "sync" (schema default) | "async".
    responseMode: sync
    # Async only. HTTPS, not internal address space (rule 22, reconcile
    # time), and callbackAuth is required with it (rule 25). Omitted in
    # async mode means responses are stored for polling.
    # callbackUrl: "https://my-service.example.com/agent-responses"
    # callbackAuth:
    #   type: hmac
    #   hmac:
    #     header: "X-Kaalm-Signature"
    #     algorithm: sha256
    #     secretRef: { name: callback-signing-secret, key: secret }
    # Async only. Schema default 100.
    maxPendingAsyncResponses: 100

  # Optional. When enabled, every envelope carries a deterministic
  # sessionId derived from the channel path and userId.
  session:
    enabled: true
```

`kubectl get ach` prints the Agent, the phase, and the `PlatformConnected` status.

### Extracting userId and content

`webhook.userId` and `webhook.content` share one extractor type:

| Field | Meaning |
|---|---|
| `fromHeader` | The named request header |
| `fromBody` | A dotted path into the JSON body: names of `[a-zA-Z_][a-zA-Z0-9_]*` joined by `.`, with no leading dot and no array indexing (`user.id`, `message.text`). The schema pattern rejects anything else at apply time. |
| `fallback` | Used when the header is absent, the path does not resolve, or the resolved value is the empty string |

At most one of `fromHeader` and `fromBody` may be set (apply time). What each outcome produces on the wire, including the raw-body path when `content` is unset, the UTF-8 requirement, and the `400` for a body `fromBody` cannot parse, is on [Channel webhook](../gateways/api/channel-webhook.md#request-body). When `userId` yields nothing and no `fallback` is set, the empty string is used, and with `session.enabled` every unattributed request then shares one session.

## Platform types

Two platform adapters exist beside the generic webhook: `discord` and `whatsapp`. Both are inbound HTTP. Discord delivers slash-command interactions to a registered URL and the gateway answers through the interaction's follow-up webhook; WhatsApp delivers Cloud API events to a registered URL and the gateway answers through the Graph API. Neither needs a persistent connection, which is what lets a stateless, multi-replica gateway serve them. Free-text bots that need the Discord Gateway WebSocket are not designed ([Beyond](../ROADMAP.md#beyond)).

What the two types share:

- **One inbound route.** A platform channel's `path` follows the same rules as a webhook path (rules 15 and 16). The gateway serves every type on the same route, resolves the channel per request, picks the adapter by `spec.type`, and routes only `Ready=True` channels.
- **One credential Secret.** `credentialsRef: {name}` names a Secret in the channel's namespace whose keys are fixed by the type (rule 40). The reconciler scopes the per-channel credential Role to it as it does for webhook auth Secrets, and the gateway holds the material in memory.
- **Reply through the platform, no record.** The platform is the caller, and neither platform keeps an inbound request open for the answer, so every reply goes back out through the platform's API. There is no `responseMode`, no `callbackUrl`, no polling record, and `maxPendingAsyncResponses` does not apply. As shipped the polling endpoint still accepts a platform channel's path as `channelPath` and fails on the missing webhook block; issue #238 tracks it.
- **The platform API base URL is a gateway-level value**, `gateway.platforms.discord.apiBaseUrl` and `gateway.platforms.whatsapp.apiBaseUrl` in the chart ([Helm chart contents](../operations/deployment.md#helm-chart-contents)), so a tenant cannot aim the gateway and a bearer token at a host of their choosing.
- **The envelope.** `channelType` is the type; `channelId` is the channel's `path`, so the session derivation is the same function of the same inputs; `userId` is the platform user; `attachments` carries platform media as references, never bytes; `metadata` carries the platform's identifiers. What the adapter needs to reply stays in the gateway's memory and never reaches the agent. The exact fields are on [Discord channel](../gateways/api/channel-discord.md#normalization) and [WhatsApp channel](../gateways/api/channel-whatsapp.md#normalization).
- **Text replies.** The adapter sends the agent's `content` as plain text, split at the platform's message length limit. Components and interactive messages are out of scope.
- **Errors reach the person.** When delivery ends in an error, the adapter sends `{error.type}: {error.message}` as the reply text, so the person who asked sees a failure rather than silence.

### Discord

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
  discord:
    # Rules 15 and 16. Register https://INGRESS_HOST/channels/team-support/support-discord
    # as the application's Interactions Endpoint URL.
    path: /channels/team-support/support-discord
    # Secret in this namespace. Keys: publicKey (required; the Ed25519
    # public key, hex), botToken (optional; see Replies).
    credentialsRef:
      name: discord-app-credentials
    # Optional scoping. Snowflakes: the schema pattern ^[0-9]{17,20}$.
    guildId: "123456789012345678"
    allowedChannelIds:
      - "987654321098765432"
    # The slash-command option whose string value becomes content. Schema
    # default "message"; pattern ^[-_a-z0-9]{1,32}$.
    contentOption: message
  session:
    enabled: true
```

- **Identity.** `userId` is the invoking user's id (`member.user.id` in a guild, `user.id` in a DM). With `session.enabled`, one person talking to the same channel from two Discord channels shares one session; the Discord channel id is in `metadata.channelId`.
- **Content.** The string value of the option named by `contentOption`, or the empty string when the command has no such option. Every option is in `metadata.options` by name, so a multi-option command remains usable.
- **Scoping.** With `guildId` set, interactions from other guilds and from DMs are refused; with `allowedChannelIds` set, interactions from other channels are refused. A refusal is an immediate ephemeral message, with no envelope and no health observation.
- **Replies** go through the interaction's follow-up webhook, whose token is valid for 15 minutes. With `botToken` in the Secret, the adapter switches to a channel message that mentions the user when the token has expired or the webhook answers `404`; without it, that case is a terminal failure. The async pipeline is bounded at ten minutes (#205), so in practice the switch is reached through the `404`.
- **Attachments.** Options of the attachment type become references carrying the CDN URL Discord supplies. The gateway does not fetch them.

### WhatsApp

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
  whatsapp:
    # Rules 15 and 16. Register https://INGRESS_HOST/channels/team-support/support-whatsapp
    # as the app's webhook callback URL, subscribed to the messages field.
    path: /channels/team-support/support-whatsapp
    # Secret in this namespace. Keys, all required: verifyToken (echoed in
    # the verification GET), appSecret (signs every event), accessToken
    # (bearer for the Graph API reply).
    credentialsRef:
      name: whatsapp-app-credentials
    # Required. The business number this channel answers as; events for
    # other numbers under the same app are acknowledged and dropped.
    # Schema pattern ^[0-9]+$.
    phoneNumberId: "106540352242922"
  session:
    enabled: true
```

- **Identity.** `userId` is the sender's `wa_id`. The profile name is in `metadata.profileName`.
- **Content by message type.** `text` gives the body; an `interactive` reply gives the chosen button or list row title; a media message gives its caption, or the empty string, plus one attachment reference with the media id, MIME type, and SHA-256; every other type gives the empty string. The raw message object is always in `metadata.message`.
- **Batches.** One inbound POST can carry several messages; the adapter emits one envelope per message, in order. Status updates and other numbers' messages are acknowledged and dropped.
- **Replies** are Graph API text messages to the sender. Meta accepts a free-form reply only within 24 hours of the person's last message; outside it the reply is refused and terminal, because the alternative, a template message, is not text the agent wrote.

The verification handshakes, the signature checks, and the exact reply requests are on [Discord channel](../gateways/api/channel-discord.md) and [WhatsApp channel](../gateways/api/channel-whatsapp.md); the reply buckets and retry schedule on [The platform adapters](../gateways/user/platform-adapters.md#the-platform-adapters).

## Status

```yaml
status:
  observedGeneration: 1
  phase: Active
  conditions:
    - type: Ready
      status: "True"
      reason: AgentReachable
      message: "channel is valid"
    - type: PlatformConnected
      status: "True"
      reason: WebhookReady
      message: "webhook delivery succeeded within the health window"
```

| Field | Meaning |
|---|---|
| `phase` | `Active`, `Degraded`, `Failed`, or `Terminating`. Unset until the first reconcile, and left unset by rule 28. Reflects the bound Agent: `Degraded` while the Agent is `Degraded` or `Failed`, `Failed` when `agentRef` does not resolve (rule 13). |
| `Ready` | `True` with `reason: AgentReachable` and the message `channel is valid` when every rule passes. `False` with the failing rule's reason: `AgentNotFound`, `AgentServiceDisabled`, `InvalidPath`, `PathConflict`, `InvalidCallbackUrl`, `CredentialsMissing`, `CredentialsInvalid`, `CallbackAuthMissing`, `CallbackAuthInvalid`, `SystemNamespaceForbidden`, or `InvalidReference` when the per-channel credential Role cannot be written. The gateway routes only `Ready=True` channels. |
| `PlatformConnected` | The tri-state in the next section. |

### The PlatformConnected tri-state

`PlatformConnected` is the gateway's view of the channel's recent inbound delivery health over a rolling window (`gateway.channelHealthWindow`, default `5m`), reduced across replicas by the reconciler:

| Status | Reason | Message |
|---|---|---|
| `True` | `WebhookReady` | `webhook delivery succeeded within the health window` |
| `False` | The most recent failure's reason: `WebhookAuthFailed`, `AgentNotReady`, `DispatchFailed`, `CallbackInvalid`, or `CallbackRejected` | The replica's last error text |
| `Unknown` | `NoRecentTraffic` | `no webhook traffic observed within the health window` |

The reason names are shared across channel types: `WebhookReady` means the inbound path works whatever the platform. The observations behind each reason, and the reduction, are on [Channel health tracking](../gateways/user/platform-adapters.md#channel-health-tracking).

### `phase` and `PlatformConnected`

The two are independent axes. `phase` follows the Agent; `PlatformConnected` follows inbound traffic. A channel can be `Degraded` with `PlatformConnected=Unknown` (the Agent is broken and nothing has arrived), and `Active` with `PlatformConnected=False` (the Agent is fine and recent requests failed auth). The reduction is drawn on [AgentChannelReconciler](../controller/reconcilers.md#agentchannelreconciler).

## Design notes

### Scope and ownership

- **Three types, all inbound HTTP.** What a type adds is knowledge of the platform's signature scheme, payload, and reply API; the path rules, the routing gate, the envelope, the session rule, and the health condition are the same.
- **AgentChannel has no child Pods.** The gateway watches AgentChannels and resolves each inbound request against them; the reconciler validates, scopes the credential Role, and reports status.
- **One AgentChannel per (Agent, channel) pair.** An Agent may have several channels; each is a separate resource.
- **The target is an Agent with a Service.** Tasks have no stable endpoint, and the gateway delivers through the ClusterIP Service (rules 13 and 14).
- **Credentials stay in the channel's namespace.** Unlike provider credentials in `kaalm-system`, webhook and platform Secrets live beside the channel. The gateway reads them under a per-channel Role scoped to those names and holds them in memory ([Dynamic per-namespace grants](../security/rbac.md#dynamic-per-namespace-grants-channel-credentials)).

### Authentication

`bearer` compares the `Authorization` header with the Secret value. `hmac` verifies a digest of the raw body from the configured header, with the prefix, encoding, and constant-time comparison specified on [Channel webhook](../gateways/api/channel-webhook.md#auth). HMAC is preferred where the calling platform signs payloads (GitHub, Stripe, Shopify, Twilio): it exposes no static token in every request. The polling endpoint reuses the same `auth` block with its own canonical string ([Polling fallback](../gateways/api/async-responses.md#polling-fallback)).

### Path scoping and routing

The path must begin with `/channels/{namespace}/` for the channel's own namespace and be unique within it (rule 15). CEL cannot express the prefix rule because `metadata.namespace` is not reachable from CRD validation, so the reconciler enforces it, and the gateway checks the prefix again on every request it resolves. Together with the `Ready=True` gate, a violating channel never receives traffic, and cross-tenant path conflicts are impossible at the routing layer ([Request flow](../gateways/user/overview.md#request-flow)).

### Sessions

With `session.enabled`, the gateway derives a deterministic `sessionId` from the channel path and `userId`, stable across replicas and restarts, so the gateway holds no session state; expiry and rotation are the agent's responsibility. The derivation and its published constant are on [Agent endpoints](../gateways/api/agent-endpoints.md#session-identity-the-sessionid-derivation).

### Async delivery

`responseMode: async` is for agents that take minutes to answer. The gateway answers `202` with a `requestId`, delivers in the background, and returns the reply to `callbackUrl` or stores it for polling. `maxPendingAsyncResponses` (default 100) caps a channel's in-flight responses: the gateway counts the channel's live records from its informer and answers `503` at the cap, an approximate bound under concurrent bursts and an exact enough guardrail for etcd. The full contract, the callback signing, the deny ranges and allowlist behind rule 22, and the record lifecycle are on [Async webhook responses](../gateways/api/async-responses.md). A hibernated Agent is woken before delivery in either mode ([The activator](../gateways/user/activation-and-activity.md#the-activator)).

### Observability

Delivery counts are not in status: a counter per delivery would mean a status write per message. Volume is the gateway metric `kaalm_channel_messages_total{channel_type,namespace,status}`; status carries health, not traffic.
