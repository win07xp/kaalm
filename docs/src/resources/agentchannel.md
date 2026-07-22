# AgentChannel

AgentChannel is a namespace-scoped resource that connects a running Agent to a user-facing communication channel. In v1, the only supported channel type is **webhook** (generic inbound HTTP POST with configurable auth). Discord, WhatsApp, and other platform-specific adapters are deferred to v1.1: they require persistent connections and platform-specific reconnection logic that adds significant implementation surface.

## Spec

```yaml
apiVersion: kaalm.io/v1alpha1
kind: AgentChannel
metadata:
  name: support-discord
  namespace: team-support
spec:
  # The Agent this channel delivers messages to (required).
  agentRef:
    name: support-assistant

  # Channel platform type. v1 supports: "webhook"
  # Discord and WhatsApp adapters are planned for v1.1.
  type: webhook

  # Webhook-specific configuration.
  webhook:
    # The gateway exposes this path externally (requires an Ingress pointing
    # at the gateway Service in kaalm-system).
    # Must begin with /channels/{namespace}/. Enforced at reconcile time by
    # the AgentChannelReconciler (Ready=False, reason=InvalidPath on violation)
    # and independently by the gateway, which refuses to register
    # non-conforming paths. CRD CEL cannot express this rule: metadata.namespace
    # is not reachable from CRD validation rules at any scope (only
    # metadata.name / metadata.generateName are). See rule 15 in
    # Cross-Resource Validation.
    path: /channels/team-support/support-assistant
    # Auth type: "bearer" or "hmac". CRD schema enforces:
    #   x-kubernetes-validations:
    #     - rule: "self.type == 'bearer' ? has(self.secretRef) : true"
    #       message: "secretRef is required when auth type is bearer"
    #     - rule: "self.type == 'hmac' ? has(self.hmac) : true"
    #       message: "hmac block is required when auth type is hmac"
    auth:
      type: bearer                              # "bearer" | "hmac"
      secretRef: { name: webhook-secret, key: token }   # required for bearer
      # For HMAC signature verification (e.g., GitHub, Stripe, Shopify, Twilio):
      # type: hmac
      # hmac:
      #   header: "X-Hub-Signature-256"         # request header containing the signature
      #   algorithm: sha256                      # "sha256" | "sha1"
      #   secretRef: { name: webhook-hmac-secret, key: secret }
      #   # Optional. Literal prefix the gateway strips off the header value
      #   # before decoding the digest (default ""). Set to "sha256=" for
      #   # GitHub's X-Hub-Signature-256: sha256=<hex> shape.
      #   signaturePrefix: "sha256="
      #   # Optional. Digest encoding in the header (default "hex", lowercase
      #   # case-insensitive compare). Use "base64" (standard, not URL-safe)
      #   # for senders like Shopify's X-Shopify-Hmac-Sha256. These two fields
      #   # apply to inbound webhook verification only; the polling endpoint
      #   # always uses bare hex (see Polling Fallback).
      #   encoding: hex                          # "hex" | "base64"
    # How the webhook adapter extracts the userId for session tracking.
    # At most one of fromHeader / fromBody may be set; both are optional.
    # When omitted, userId defaults to the empty string, so all unattributed
    # requests share one session if session.enabled is true.
    # CRD schema enforces the mutex via:
    #   x-kubernetes-validations:
    #     - rule: "!has(self.fromHeader) || !has(self.fromBody)"
    #       message: "at most one of fromHeader or fromBody may be set"
    userId:
      fromHeader: "X-User-Id"       # read userId from this request header
      # fromBody: ".user.id"        # alternative: dotted JSON path into the body (see design notes)
      fallback: "anonymous"         # value when userId cannot be resolved
    # How the webhook adapter extracts the message content for delivery to
    # the agent's /v1/message envelope. At most one of fromHeader / fromBody
    # may be set; both are optional. When neither is set, the gateway uses
    # the raw inbound body, JSON-encoded as a string, as `content`,
    # preserving the generic-webhook story for senders whose body shape
    # Kaalm has no a-priori knowledge of. The raw-body path requires
    # valid UTF-8; non-UTF-8 bodies are rejected with 400 invalid_request,
    # and binary senders must configure fromHeader/fromBody explicitly.
    # CRD schema enforces the mutex
    # via the same CEL pattern used for userId above:
    #   x-kubernetes-validations:
    #     - rule: "!has(self.fromHeader) || !has(self.fromBody)"
    #       message: "at most one of fromHeader or fromBody may be set"
    content:
      fromHeader: "X-Message-Text"        # read content from this request header
      # fromBody: ".message.text"         # alternative: dotted JSON path into the body (see design notes)
      # fallback: ""                      # value when extraction fails (header absent or path does not resolve)
    # Response mode: "sync" (default) or "async".
    # sync: gateway blocks until the agent responds and returns the response
    #   as the webhook HTTP response body.
    # async: gateway returns 202 Accepted immediately with a requestId,
    #   delivers the message to the agent, and posts the agent's response
    #   to callbackUrl (if configured) or makes it available for polling.
    responseMode: sync
    # Required when responseMode is "async" and push-based delivery is desired.
    # The gateway POSTs the agent's response to this URL when it becomes available.
    # If omitted in async mode, responses are stored and retrievable via polling
    # at GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}.
    #
    # Constraints (enforced by AgentChannelReconciler; see rules 22 and 25 below):
    #   - scheme must be https://
    #   - the host must not resolve to loopback (127.0.0.0/8, ::1),
    #     link-local (169.254.0.0/16, fe80::/10), RFC1918 private ranges
    #     (10/8, 172.16/12, 192.168/16), unique-local IPv6 (fc00::/7), or
    #     cloud metadata IPs (169.254.169.254, fd00:ec2::254)
    #   - callbackAuth (below) must also be set; every callback POST is signed
    # The check is re-run at each delivery attempt (DNS is re-resolved then to
    # defeat rebinding); see the User Gateway request flow. Platform teams
    # that need callbacks into the cluster can open this deny-internal
    # default for specific targets with the Helm value
    # gateway.callbackUrl.allowlist (DNS-name suffixes or CIDR blocks);
    # loopback and the cloud-metadata IPs stay refused even when listed.
    # callbackUrl: "https://my-service.example.com/agent-responses"
    #
    # Required when callbackUrl is set (enforced by rule 25 below). Defines the
    # signing material the gateway applies to every callback POST so receivers
    # can reject forged callbacks. Same shape as `auth` above; bearer adds an
    # Authorization header, hmac signs a canonical string of
    # "{requestId}\n{timestamp}\n{sha256(body)}"; see the async response reference
    # § Callback delivery for the wire contract.
    # callbackAuth:
    #   type: hmac
    #   hmac:
    #     header: "X-Kaalm-Signature"
    #     algorithm: sha256
    #     secretRef: { name: callback-signing-secret, key: secret }
    # Maximum number of in-flight async responses before the gateway rejects
    # new async requests with HTTP 503. Bounds ConfigMap creation in kaalm-system.
    maxPendingAsyncResponses: 100

  # Optional: session configuration. When enabled, the gateway generates a
  # deterministic sessionId from (channelId, userId) and includes it in the
  # message envelope so the agent can maintain conversation context.
  # Session expiry/rotation is the agent's responsibility using its PVC state.
  session:
    enabled: true
```

## Future platform types (v1.1+)

When Discord and WhatsApp adapters are added in v1.1, the spec will support platform-specific configuration blocks:

```yaml
spec:
  type: discord
  credentialsRef:
    name: discord-bot-credentials
    key: bot-token
  discord:
    guildId: "123456789012345678"
    allowedChannelIds:
      - "987654321098765432"
```

These types require persistent connections (Discord WebSocket, WhatsApp Cloud API registration) and platform-specific reconnection logic, which is why they are deferred.

## Status

```yaml
status:
  observedGeneration: 1
  phase: Active     # Active | Degraded | Failed | Terminating (unset until first reconcile)
  conditions:
    - type: Ready
      status: "True"
      reason: AgentReachable
    - type: PlatformConnected
      status: "True"
      reason: WebhookReady
      message: "1 successful inbound request in the last 5m"
```

### The PlatformConnected tri-state

`PlatformConnected` is a **tri-state** condition reflecting the gateway's view of the channel's recent inbound delivery health, evaluated over a rolling window (`gateway.channelHealthWindow`, default `5m`):

- `status: "True"`, `reason: WebhookReady`: at least one in-window inbound request succeeded (auth passed + message dispatched to the agent).
- `status: "False"` with `reason` one of `WebhookAuthFailed`, `AgentNotReady`, `DispatchFailed`, `CallbackInvalid`, `CallbackRejected`: the in-window observations are all failures, and the reason reflects the most recent failure. The two `Callback*` reasons come from outbound callback dispatch on async channels: `CallbackInvalid` when the `callbackUrl` fails the pre-dial deny-range / allowlist re-check, `CallbackRejected` when the receiver terminally rejects the POST. See [Async Webhook Response](../gateways/api/async-responses.md).
- `status: "Unknown"`, `reason: NoRecentTraffic`: no in-window observations exist on any replica that has been up the full window. This distinguishes a truly idle channel from one that was last known healthy hours ago.

The reduction across gateway replicas is performed by the `AgentChannelReconciler`. v1.1+ persistent-connection channels will reuse the same tri-state contract, with reasons sourced from gateway-side connection events. See [Channel Health Tracking](../gateways/user/platform-adapters.md#channel-health-tracking) for the per-replica state model and cross-replica reduction.

### phase vs PlatformConnected

`status.phase` and `conditions[type=PlatformConnected]` are orthogonal signals:

- `phase: Degraded` reflects the **bound Agent's** state. The `AgentChannelReconciler` sets it when the referenced Agent transitions to a non-serving phase (e.g., `Failed`); see [AgentChannelReconciler step 5](../controller/reconcilers.md#agentchannelreconciler). A channel whose Agent is broken cannot deliver messages regardless of inbound auth state.
- `conditions[type=PlatformConnected]` reflects **inbound webhook** delivery health (auth pass/fail, dispatch success/failure), evaluated over the rolling window described above.

The two can disagree without contradiction: a channel can be `phase: Degraded` (Agent broken) while `PlatformConnected=Unknown` (no recent inbound to observe), and a channel with a healthy Agent (`phase: Active`) can still report `PlatformConnected=False` if recent inbound requests failed auth or dispatch.

## Design Notes

### Scope and ownership

- **v1 supports webhook only.** Discord, WhatsApp, and other platform-specific adapters are planned for v1.1. The webhook type is stateless and covers the core channel integration pattern without requiring persistent platform connections.
- **AgentChannel owns no Pod resources.** The gateway watches AgentChannel resources directly and manages webhook endpoints based on their specs. The reconciler's role is validation and status reporting.
- **One AgentChannel per (Agent, channel) pair.** An Agent may have multiple AgentChannels (e.g., both a Discord channel and a webhook). Each is a separate resource.
- **AgentChannel references Agent only, not AgentTask.** Tasks are ephemeral and lack a stable Service endpoint. The `agentRef` field must point to an `Agent` resource.
- **The agent's Service must be enabled** (`spec.service.enabled: true`) for AgentChannel to function, because the gateway delivers messages via the ClusterIP Service.
- **Credentials stay in the agent's namespace.** Unlike LLM provider credentials (which live in `kaalm-system`), channel credentials (webhook auth tokens, etc.) are stored in the agent's namespace for organizational isolation. They are typically created by the platform team or a provisioning service, not by the developer. The gateway reads them via scoped RBAC and holds them in-process for the channel adapter.

### Authentication

- **Webhook auth types**: `bearer` validates a static token from the `Authorization` header against the value in `secretRef`. `hmac` validates a request body signature: the gateway reads the signature from the configured `header` (e.g., `X-Hub-Signature-256`), strips any configured `hmac.signaturePrefix` (e.g., `"sha256="` for GitHub), decodes per `hmac.encoding` (`hex` default; `base64` for senders like Shopify), computes `HMAC(algorithm, secret, request_body)` using the shared secret from `hmac.secretRef`, and constant-time-compares the values. HMAC is preferred for integrations where the calling platform signs payloads (GitHub, Stripe, Shopify, Twilio): it avoids exposing a static token in every request.
- **Poll-endpoint auth** (async response mode): `GET /v1/channels/responses/{requestId}` reuses the same `webhook.auth` configuration, but the HMAC input differs because poll GETs have no body. For `auth.type: hmac`, the caller computes `HMAC(algorithm, secret, canonicalString)` where `canonicalString = "{requestId}\n{timestamp}"` (unix seconds, LF delimiter, no trailing newline), sends the bare lowercase hex digest in the configured `header` (the `hmac.signaturePrefix` and `hmac.encoding` fields apply to inbound verification only: polling is an Kaalm-canonical surface and always uses bare hex with no prefix), and presents the timestamp in `X-Kaalm-Timestamp`. The gateway rejects requests whose timestamp differs from its wall clock by more than 300s to bound replay. For `auth.type: bearer`, the poll presents the same bearer token in `Authorization: Bearer …`. See [Async Webhook Response § polling fallback](../gateways/api/async-responses.md).

### Path scoping and routing

- **Webhook path namespace scoping**: `spec.webhook.path` must begin with `/channels/{namespace}/` where `{namespace}` is the AgentChannel's own namespace. This is enforced at reconcile time by the AgentChannelReconciler (`Ready=False, reason=InvalidPath`) and re-checked by the gateway at path registration. CRD CEL cannot express the rule, because `metadata.namespace` is not reachable from CRD validation rules at any scope (only `metadata.name`/`metadata.generateName` are). Because the gateway routes only `Ready=True` channels **and** refuses non-conforming paths outright, a violating AgentChannel never receives traffic, so cross-tenant path conflicts remain impossible at the routing layer.
- **Within a namespace, paths must be unique** (see validation rules 15-16 in [Cross-Resource Validation](validation-and-defaulting.md#cross-resource-validation)). The gateway routes webhook traffic only to AgentChannels with `status.conditions[type=Ready].status == True`. `Ready=False` channels (including the `PathConflict` loser) receive no traffic; see [Request Flow](../gateways/user/overview.md#request-flow).

### Extracting userId and content

- **userId extraction**: the webhook adapter resolves `userId` using `webhook.userId` config (`fromHeader` or `fromBody`). At most one may be set; the CRD schema enforces this via CEL (`!has(self.fromHeader) || !has(self.fromBody)`), rejecting invalid combinations at apply time. If both are absent, the adapter uses the empty string. When `session.enabled: true`, this means all requests that cannot be attributed to a user share a single session, so set `fallback` explicitly to control this behavior. See the `webhook.userId` spec block above.
- **`fromBody` syntax, a dotted JSON path**: `webhook.userId.fromBody` and `webhook.content.fromBody` accept a **strict subset** of jq-style paths, not the full jq language. Supported: object property access (`.foo.bar.baz`, leading dot required, property names match `[a-zA-Z_][a-zA-Z0-9_]*`) and numeric array indexing (`.foo[0]`). **Not supported**: filters, slicing, `[]` flatten, recursive descent (`..`), pipes, comparisons, or any jq expression. Invalid paths are rejected at apply time via CRD CEL on both fields (`x-kubernetes-validations` with `rule: "self.matches('^(\\.[a-zA-Z_][a-zA-Z0-9_]*(\\[[0-9]+\\])?)+$')"`, message `"fromBody must be a dotted JSON path: .foo.bar or .foo[0]"`). The narrow grammar keeps the extraction implementation small and unambiguous across language ports; senders whose payloads require richer extraction should pre-process upstream or configure `fromHeader` instead.
- **content extraction**: the webhook adapter resolves `content` using `webhook.content` config (`fromHeader` or `fromBody`). At most one may be set; the CRD schema enforces this via CEL (`!has(self.fromHeader) || !has(self.fromBody)`), mirroring the `userId` rule, rejecting invalid combinations at apply time. **If neither is configured**, the gateway uses the raw inbound body, JSON-encoded as a string, as `content`, preserving the generic-webhook story for callers whose body shape Kaalm has no a-priori knowledge of (this is the structural reason the design supports any third-party webhook sender out of the box). The raw-body path requires valid UTF-8: bodies containing invalid UTF-8 bytes are rejected with `400 Bad Request` and `error.type: invalid_request` (`error.message` names the invalid byte offset). Operators with binary senders (e.g., protobuf-payload webhooks) must configure `webhook.content` explicitly, typically `fromHeader`, so the gateway never tries to UTF-8-decode the body. **If `fromBody` is configured but the inbound body cannot be parsed as JSON**, the gateway rejects the request with `400 Bad Request` and `error.type: invalid_request`; this applies symmetrically to `userId.fromBody`, since both extractions share the parse step. **If `fromBody` is configured and the body parses but the path does not resolve, or `fromHeader` is configured and the header is absent**, the configured `fallback` is used (empty string if omitted). Per-channel `attachments` and `metadata` extraction are out of scope in v1: the generic webhook adapter populates them as `[]` and `{}` respectively; v1.1 platform-specific adapters (Discord, WhatsApp) populate them via adapter code, not per-channel CRD config. See [POST /channels/{namespace}/{channel-path}](../gateways/api/channel-webhook.md) for the wire contract.

### Sessions

- **Session management is opt-in.** When `session.enabled: true`, the gateway generates a **deterministic `sessionId`** from the message's `channelId` and `userId`: `sessionId = UUIDv5(namespace: kaalm-session-ns, name: channelId + ":" + userId)`, where `kaalm-session-ns` is a fixed published namespace UUID, identical across all installations and versions; the constant is specified in [POST /v1/message](../gateways/api/agent-endpoints.md#post-v1message). This ID is stable across gateway replicas and restarts, so no gateway-side session state is required. Session expiry and rotation are the agent's responsibility: the agent uses its PVC to track conversation state and decides when a "session" is over. When `session.enabled: false`, no `sessionId` is included in the envelope. **The namespace constant must not change after v1 ships**: any change would invalidate existing session state in agent PVCs.

### Async delivery

- **Async response mode** (`spec.webhook.responseMode: async`): designed for agents that take minutes to respond (e.g., coding agents, research agents). The async/sync distinction is handled entirely by the gateway. See [Async Webhook Response](../gateways/api/async-responses.md) for the response schemas.
- **`maxPendingAsyncResponses`** (default: 100) caps the number of in-flight async responses per AgentChannel. The gateway counts a channel's live `kaalm-async-{requestId}` ConfigMaps via its `kaalm-system` ConfigMap informer (label selector on `kaalm.io/channel-namespace` / `kaalm.io/channel-name`) and rejects new async requests with HTTP 503 when the count is at the limit. Informer-based counting makes the cap replica-agnostic without new coordination state, at the cost of being approximate under concurrent bursts (multiple replicas can admit simultaneously before their informers observe each other's placeholder `Create`s; the overshoot is bounded by in-flight acceptances per informer-lag window, acceptable for an etcd-pressure guardrail). This bounds the number of `kaalm-async-{requestId}` ConfigMaps created in `kaalm-system` per channel, preventing unbounded etcd pressure from burst traffic or slow agents.
- **Wake-on-demand integration**: if the target Agent is `Hibernated` when a webhook message arrives, the gateway wakes it via the [Activator](../gateways/user/activation-and-activity.md#the-activator) before delivering the message. In sync mode, the webhook caller blocks until the agent is ready and responds, or receives a timeout error if `wakeTimeout` is exceeded. In async mode, the gateway returns 202 immediately and handles wake + delivery asynchronously.

### Observability

- **Delivery counts are not in status.** Per-message counters require either a status patch on every delivery (high etcd write pressure at scale) or a separate in-memory accumulation mechanism. Instead, delivery volume is tracked via the Prometheus metric `kaalm_channel_messages_total{channel_type,namespace,status}` exposed by the gateway. Status reflects channel health (phase, conditions), not traffic volume.
