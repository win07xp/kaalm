# Agent endpoints

Two endpoints make up the gateway's contract with long-running agent containers. They point in opposite directions:

- `POST /v1/agent/heartbeat` is served by the gateway and called by the agent, to signal liveness.
- `POST /v1/message` is served by the agent and called by the gateway, to deliver channel messages.

Both are for Pods backed by an Agent resource. AgentTask Pods are rejected on the heartbeat and are never delivered to.

## POST /v1/agent/heartbeat

An Agent container calls this to signal liveness for idle detection. Authentication is mTLS with the Agent's certificate under the [agent-report regime](../listener-tls.md#per-path-client-auth-enforcement): the certificate must be present, its SAN must be an Agent identity, and the source IP must resolve to a Pod in that namespace. Any HTTP method is accepted; issue #237 tracks method enforcement.

The gateway records every heartbeat as the agent's last-activity timestamp in its in-memory activity store, with no API server write. It does not consult the Agent's [`spec.lifecycle.activitySource`](../../resources/agent.md): the controller applies that filter when it merges the per-replica timestamps ([Activity detection](../../controller/hibernation-and-wake.md#activity-detection)), so a heartbeat from an agent set to `gatewayTraffic` is recorded and then ignored. An image that heartbeats on a timer therefore keeps an agent set to `agentHeartbeat` or `both` from ever going idle; the starter templates heartbeat on a timer and are meant for the default `gatewayTraffic` ([The heartbeat toggle and the hibernation footgun](../../runtime/starter-templates.md#the-heartbeat-toggle-and-the-hibernation-footgun)).

**Request body:** empty or `{}`.

**Response:** `200 OK` with an empty body.

**Errors** carry the [error envelope](errors.md#the-error-envelope):

| Status | `error.type` | Raised when |
|---|---|---|
| `401` | `unauthorized` | No client certificate, or the source IP does not resolve to a Pod in the SAN's namespace in the gateway's informer cache |
| `403` | `invalid_cert` | The certificate's SAN is not an Agent or AgentTask identity |
| `403` | `access_denied` | The certificate is an AgentTask identity. The message is `AgentTask callers are not accepted on this path`. |

Unlike [`/v1/task/complete`](task-complete.md), the cross-check here has no live API-server fallback: heartbeats repeat, so a call dropped during informer lag (a heartbeat in the first hundred milliseconds of a Pod's life can see a `401`) is recovered by the next one. Frequency is the agent's choice; every 30 to 60 seconds is a reasonable default. The gateway applies no rate limit to this path.

## POST /v1/message

This endpoint is served by the agent container, not by the gateway. The User Gateway calls it to deliver normalized channel messages ([Request flow](../user/overview.md#request-flow)). An agent that is the target of an AgentChannel must serve it on `$KAALM_HEALTH_PORT` (default 8080), the same port as its probes.

### Requirements on the agent

The agent is the server, so the [runtime contract](../../runtime/contract.md) puts three obligations on it:

- Serve TLS on `$KAALM_HEALTH_PORT` with the certificate at `$KAALM_TLS_CERT` and `$KAALM_TLS_KEY`, and reload it on rotation ([item 4](../../runtime/contract.md#4-message-endpoint)).
- Verify the gateway's client certificate per path, not at the handshake: `401 Unauthorized` when no client certificate was presented, `403 Forbidden` when its SAN is not the gateway Service DNS (`kaalm-gateway.{operatorNamespace}.svc.cluster.local` or `kaalm-gateway.{operatorNamespace}.svc`, built from the injected `$KAALM_OPERATOR_NAMESPACE`). [Client-certificate verification](../../runtime/contract.md#client-certificate-verification-on-v1message) says why the handshake cannot do it.
- Deduplicate on `messageId`, and persist the dedup buffer across Pod restarts when hibernation is enabled ([item 7](../../runtime/contract.md#7-message-deduplication)).

### How the gateway calls it

| Property | Value |
|---|---|
| URL | `https://{agentName}.{namespace}.svc.cluster.local:{port}/v1/message`, where the port is `spec.service.port` when set and 8080 otherwise |
| Client certificate | `kaalm-gateway-tls` |
| Headers | `Content-Type: application/json`; `traceparent` and `tracestate` when tracing is enabled ([item 8](../../runtime/contract.md#8-trace-context-propagation)). No `Authorization` header. |
| Attempts | Four: immediately, then after 1s, 5s, and 25s |
| Per-attempt bound | The delivery context: the sync deadline in sync mode, the async pipeline bound in async mode |
| Response cap | `gateway.maxResponseBodyBytes` (default 900 KiB). A reply over the cap is `response_too_large` and is not retried. |

A connection error, a non-2xx status, or a `2xx` with an unusable envelope is retried on the schedule. The same `messageId` is sent on every attempt.

### Request body (sent by the gateway)

```json
{
  "messageId": "550e8400-e29b-41d4-a716-446655440000",
  "channelType": "webhook",
  "channelId": "/channels/team-support/support-assistant",
  "userId": "caller-id-from-header-or-body",
  "sessionId": "optional-session-uuid",
  "content": "Hello, I need help with my order",
  "attachments": [],
  "metadata": {}
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `messageId` | string (UUID) | yes | Generated by the gateway and reused across delivery retries; the agent deduplicates on it |
| `channelType` | string | yes | `"webhook"`, `"discord"`, `"whatsapp"`, or `"console"` for a [test chat](internal-endpoints.md#post-v1test-chat) |
| `channelId` | string | yes | The AgentChannel's path, for every type; a platform's own room identifiers are in `metadata` |
| `userId` | string | yes | The sender's identity as the channel extracts it; see below |
| `sessionId` | string | no | Present when `AgentChannel.spec.session.enabled` is `true`; see [Session identity](#session-identity-the-sessionid-derivation) |
| `content` | string | yes | The message text, per `AgentChannel.spec.webhook.content` (`fromHeader`, `fromBody`, or the raw body when unset) or the platform adapter's rule ([Extracting userId and content](../../resources/agentchannel.md#extracting-userid-and-content)) |
| `attachments` | array | no | Attachment references, never bytes; see the contract note below |
| `metadata` | map | no | Platform-specific fields (for example `guildId` for Discord); see the contract note below |

**`userId` extraction.** Per `AgentChannel.spec.webhook.userId` (`fromHeader` or `fromBody`), falling back to the configured `fallback` value, or the empty string when neither yields one. When `session.enabled` is `true` and `userId` is empty, every unattributed request shares one session.

**Contract for `attachments` and `metadata`.** The webhook adapter always sends `[]` and `{}`. The Discord and WhatsApp adapters fill them with references and identifiers ([Discord channel](channel-discord.md#normalization), [WhatsApp channel](channel-whatsapp.md#normalization)).

### Session identity: the sessionId derivation

When `AgentChannel.spec.session.enabled` is `true`, the gateway computes a deterministic `sessionId` for each message:

```
sessionId = UUIDv5(namespace: f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d, name: channelId + ":" + userId)
```

The namespace constant `f6a7d3c2-1b4e-5f8a-9c0d-2e3f4a5b6c7d` is a purpose-generated UUID published as part of the Kaalm API. It is identical across installations and versions and never changes: a change would invalidate the session state agents key by `sessionId` in their PVCs.

Because the derivation is a pure function of `channelId` and `userId`, the id is stable across gateway replicas and restarts, and the gateway holds no session state. Session expiry and rotation are the agent's responsibility. When `session.enabled` is `false`, the envelope carries no `sessionId`.

### Response body (returned by the agent)

```json
{
  "content": "I'd be happy to help! Can you share your order number?",
  "attachments": [],
  "metadata": {}
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `content` | string | yes | The agent's reply text |
| `attachments` | array | no | Passed through unchanged. Webhook callers receive them as opaque JSON; the Discord and WhatsApp adapters send text only and ignore them. |
| `metadata` | map | no | Passed through unchanged, as `attachments` is |

### What the gateway does with the answer

`200 OK` with a JSON envelope whose `content` is a string is a delivery. Anything else, a non-2xx status, a connection error, an unparseable body, or a `200` with a missing or non-string `content`, is one failed attempt, and the schedule above continues. After the fourth failure the message is `delivery_failed`.

In sync mode the webhook caller then receives `502 delivery_failed`, though under default settings `504 sync_deadline_exceeded` fires first ([Reachability under default config](channel-webhook.md#reachability-under-default-config)). In async mode the same payload is sent by callback or stored for polling, and a platform channel sends it as the reply text. Each outcome is recorded as a channel-health observation.
