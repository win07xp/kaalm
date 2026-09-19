# Gateway overview

The gateway is a single replicated Deployment in `kaalm-system`. It hosts two logical gateways, the [LLM Gateway](llm/request-handling.md#request-flow) for outbound agent-to-provider traffic and the [User Gateway](user/overview.md#request-flow) for inbound channel traffic, plus the [tool plane](tool-plane.md)'s broker. Its two TLS listeners split by exposure, not by subsystem. The **cluster listener** (`:8443`) serves every in-cluster caller: the LLM proxy, the tool broker, and the internal endpoints that agents, the controller, and the console call. The **user listener** (`:8080`) serves the Ingress-fronted surface: inbound channel webhooks and the async polling endpoint. A separate health port serves kubelet probes, and a metrics port serves Prometheus.

![The single gateway Deployment in kaalm-system and its four ports, with a trust boundary. Outside it, external webhook callers reach an Ingress, which routes to the :8080 user listener and has no route to :8443. Inside the cluster, Agent and AgentTask Pods reach the :8443 cluster listener with mTLS or a bearer token, the controller and the console reach it with their own mTLS certificates, the kubelet probes :8081, and Prometheus scrapes :9090.](../diagrams/gateway-listener-ports.svg)

## Ports

| Port | Listener | Who calls it | Client auth | Paths |
|---|---|---|---|---|
| `:8443` | Cluster listener, TLS | Agent and AgentTask Pods, gateway-only-tier workloads, the controller, the console | Per path ([the `:8443` listener profile](#the-8443-listener-profile)) | The LLM proxy, `/v1/mcp/*`, the six internal endpoints |
| `:8080` | User listener, TLS, Ingress-fronted | External webhook callers and channel platforms through the Ingress | Per AgentChannel ([the `:8080` listener profile](#the-8080-listener-profile)) | `/channels/*`, `/v1/channels/responses/*` |
| `:8081` | Health, TLS, no client auth | The kubelet | None | `/healthz`, `/readyz` ([Gateway readiness](llm/operations.md#gateway-readiness)) |
| `:9090` | Metrics, plaintext | Prometheus, in-cluster | None | `/metrics` ([Metrics](../operations/observability.md#metrics)) |

A fifth port, off by default, serves Go profiles when `gateway.pprofPort` is set ([Profiling](../operations/observability.md#profiling)).

### Why two listeners and a separate health port

The split is a security boundary. `:8080` hosts no mTLS-authenticated path, so an Ingress fronting it cannot reach an endpoint whose authorization assumes a controller or console certificate; those endpoints exist on `:8443` alone, which no Ingress routes to ([Controller-only paths on the same socket](listener-tls.md#controller-only-paths-on-the-same-socket)).

Probes get their own port for the same reason. The controller's `:9443` lets cert-less kubelet probes share the socket with mTLS paths, because that port is reachable only inside the cluster ([Control plane](../concepts/system-architecture.md#control-plane)). Both gateway listeners are reachable beyond that boundary: `:8080` sits behind a user-provisioned Ingress, and `:8443` is reachable by every mTLS-authenticated agent. A cert-less probe path on either would be a cert-less path an outsider could reach, so probes terminate on `:8081`, outside both listener auth profiles.

## The call surfaces

The Deployment serves four kinds of request. Agents send two of them, LLM calls and tool calls, which the gateway forwards to a provider or an MCP server. Channel platforms send the third, a message for an agent, which the gateway delivers and answers. The fourth is the internal API: agents report to it, and the controller and the console read from and act through it.

![The gateway in the middle, with three colored flows. Brown, the User Gateway: a channel platform sends a message to :8080, the gateway delivers it to the agent Pod over mTLS, and the reply goes back to the platform. Purple, the LLM proxy and tool broker: Agent and AgentTask Pods call :8443, and the gateway forwards to LLM providers and MCP servers with the credential injected. Blue, the internal API: Pods report heartbeats and task completion to :8443, the controller reads activity and channel health from it, and the console sends test chats and reads spend through it.](../diagrams/gateway-call-surfaces.svg)

| Surface | Listener | Direction | What the gateway does | Specified on |
|---|---|---|---|---|
| LLM proxy | `:8443` | Agent to provider | Identifies the caller ([Workload identity](llm/workload-identity.md)), resolves the provider and model, applies the three tenancy gates, budgets, and rate limits, forwards with the credential injected, walks the fallback chain on failure, and meters usage and spend | [LLM Gateway](llm/overview.md), [Request handling](llm/request-handling.md#request-flow) |
| Tool broker | `:8443` | Agent to MCP server | The same caller identity, then the tool grant chain, credential injection, per-call audit, and rate limits | [The tool plane](tool-plane.md) |
| User Gateway | `:8080` | Platform to agent, and the reply back | Authenticates per AgentChannel, normalizes the message, wakes a hibernated Agent through the controller, delivers over mTLS, and returns the reply synchronously, by callback, by polling, or through the platform's API | [User Gateway](user/overview.md#request-flow), [Platform adapters and channel health](user/platform-adapters.md), [Async webhook responses](api/async-responses.md) |
| Internal API | `:8443` | Agent to gateway, controller and console to gateway | Task completion and heartbeats in; activity, channel health, and spend out; test chats delivered on the console's behalf | [Internal endpoints](#internal-endpoints) |

The LLM proxy and the tool broker share one caller-identity step (an mTLS SAN for Kaalm-managed Pods, a `TokenReview`-validated bearer token for the gateway-only tier) and differ only in what they authorize afterwards. The User Gateway depends on the controller for wake-on-demand: while the activator endpoint is unreachable, a hibernated Agent cannot receive a message and the caller gets `controller_unavailable` ([The activator](user/activation-and-activity.md#the-activator)).

## The :8443 listener profile

All paths on `:8443` share one TLS socket. The handshake accepts a client certificate when one is offered and completes without one, and middleware then applies one of four regimes per path:

| Regime | Paths | Client auth |
|---|---|---|
| Dual mode | `/v1/messages`, `/v1/chat/completions`, `/v1/completions`, `/v1/mcp/*` | mTLS with an Agent or AgentTask SAN, or a `TokenReview`-validated ServiceAccount bearer token (gateway-only tier), then the source-IP cross-check |
| Agent report | `/v1/agent/heartbeat` (Agent only), `/v1/task/complete` (AgentTask only) | mTLS with an Agent or AgentTask SAN, the kind checked against the path, then the source-IP cross-check. No bearer fallback. |
| Controller only | `/v1/activity`, `/v1/channels/health` | mTLS with the controller Service SAN. No cross-check. |
| Console only | `/v1/test-chat`, `/v1/spend` | mTLS with the console Service SAN. No cross-check. |

Every regime answers `401 unauthorized` when the required credential is absent or fails, `403 invalid_cert` when a certificate is presented whose SAN is not a recognized identity, and `403 access_denied` when the identity is recognized but not the one the path accepts. The branch-by-branch mechanics and the figures are on [Per-path client auth enforcement](listener-tls.md#per-path-client-auth-enforcement); the SAN shapes, the `TokenReview` flow, and the cross-check are on [Workload identity](llm/workload-identity.md).

The dual-mode rows share one profile on purpose: establishing who is calling is plane-independent, so the middleware carries no plane-specific logic, and what differs per plane is authorization, the provider chain versus the [tool grant chain](tool-plane.md#grants), enforced by each handler. `/v1/task/complete` adds handler checks of its own, on the task's completion mode, its phase, and the calling Pod's identity ([Task completion](api/task-complete.md)).

A routing bug in the path-to-regime mapping would let agent-certificate holders reach controller-only paths, so that mapping is the detail the gateway's authorization depends on most.

## The :8080 listener profile

The user listener serves only externally reachable channel traffic and uses per-AgentChannel auth on both path families:

| Path family | Client auth |
|---|---|
| Channel intake, `/channels/{namespace}/{channel-path}` | The channel's `spec.webhook.auth` (bearer or HMAC), or the platform's signature scheme for Discord and WhatsApp channels |
| Async polling, `GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}` | The originating AgentChannel's `spec.webhook.auth`, and the record must belong to that channel ([Polling fallback](api/async-responses.md#polling-fallback)) |

No mTLS-authenticated path lives on `:8080`, and the listener requests no client certificate.

## Internal endpoints

Six endpoints on `:8443` are for Kaalm's own components. They share the cluster listener so that mTLS-authenticated callers reach them without a third listener, and they live under the reserved `/v1/` prefix ([Reserved gateway paths](api/overview.md#reserved-gateway-paths)).

| Endpoint | Direction | Purpose |
|---|---|---|
| [`POST /v1/task/complete`](api/task-complete.md) | AgentTask Pod to gateway | The task reports its result; the gateway writes it to the task's completion ConfigMap ([Child resources](../runtime/child-resources.md)) |
| [`POST /v1/agent/heartbeat`](api/agent-endpoints.md#post-v1agentheartbeat) | Agent Pod to gateway | Liveness signal for the `agentHeartbeat` activity source |
| [`GET /v1/activity`](api/internal-endpoints.md#get-v1activity) | Controller to gateway | Per-namespace activity timestamps for idle and hibernation transitions |
| [`GET /v1/channels/health`](api/internal-endpoints.md#get-v1channelshealth) | Controller to gateway | Per-channel health observations for `PlatformConnected` |
| [`POST /v1/test-chat`](api/internal-endpoints.md#post-v1test-chat) | Console to gateway | One operator-authored message delivered to one Agent, sync path end to end |
| [`GET /v1/spend`](api/internal-endpoints.md#get-v1spend) | Console to gateway | One namespace's current-period spend by workload |

## Credentials and RBAC

LLM and tool credentials are Secrets in `kaalm-system`, read by the gateway and never leaving the namespace ([Credential handling](../security/credentials.md)). The gateway ServiceAccount holds four kinds of grant ([Gateway ServiceAccount permissions](../security/rbac.md#gateway-serviceaccount-permissions)):

| Grant | Scope | Used for |
|---|---|---|
| `create` on `tokenreviews` | Cluster | Bearer-token callers in the gateway-only tier |
| `get, list, watch` on the Kaalm CRDs and Pods; `patch` on AgentChannels; `get` on Services; `create, patch` on Events | Cluster | Provider and channel routing, the source-IP cross-check, delivery endpoints, the channel delete handshake, and channel events |
| `get, watch` on Secrets; `get, list, watch, create, patch` on ConfigMaps | `kaalm-system` only | Credentials, the budget exchange, async response records |
| Per-channel and per-task Roles | One user namespace each, `resourceNames`-scoped | `get, watch` on a channel's credential Secrets; `update, patch` on one task's completion ConfigMap, never `create` |

The per-namespace Roles are created by the reconcilers and carry an ownerRef to the AgentChannel or AgentTask, so they are collected with it.

## Multi-replica state

The gateway runs as two or more replicas with no shared memory. Each piece of cross-replica state uses one of four strategies, and the page that specifies the state carries the mechanics:

| State | Strategy | Reduced by | Specified on |
|---|---|---|---|
| Spend counters | Each replica writes its partial to a per-provider ConfigMap in `kaalm-system` | The ModelProviderReconciler sums the partials into a canonical total that replicas re-initialize from | [Budget state management](llm/budgets-and-rate-limits.md#budget-state-management) |
| Rate-limit token buckets | Each replica divides the cluster-wide ceiling by the live replica count | Nothing; local recomputation on scale | [Rate limiting](llm/budgets-and-rate-limits.md#rate-limiting) |
| Activity timestamps | In memory per replica, no etcd writes | The AgentReconciler fans out `GET /v1/activity` to every replica and merges | [Activity tracking API](user/activation-and-activity.md#activity-tracking-api) |
| Channel-health observations | In memory per replica, no etcd writes | The AgentChannelReconciler fans out `GET /v1/channels/health` and reduces to `PlatformConnected` | [Channel health tracking](user/platform-adapters.md#channel-health-tracking) |
| Async webhook responses | One ConfigMap per request in `kaalm-system`, so any replica can answer a poll | The AgentChannelReconciler prunes expired records | [Async webhook responses](api/async-responses.md) |
