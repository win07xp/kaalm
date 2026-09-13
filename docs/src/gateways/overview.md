# Gateway overview

The gateway is a single replicated Deployment in `kaalm-system`. It hosts two logical gateways, the [LLM Gateway](llm/request-handling.md#request-flow) for outbound agent-to-provider traffic and the [User Gateway](user/overview.md#request-flow) for inbound channel traffic, plus the [tool plane](tool-plane.md)'s broker. Its two TLS listeners split by exposure, not by subsystem: the **cluster listener** (`:8443`) serves every in-cluster caller across three surfaces (the LLM proxy, the tool broker on `/v1/mcp/*`, and the internal mTLS endpoints), while the **user listener** (`:8080`) serves the Ingress-fronted surface (inbound channel webhooks and the async response polling endpoint). A separate health port serves kubelet probes, and a metrics port serves Prometheus ([Ports](#ports)).

![The single gateway Deployment in kaalm-system and its four ports, with a trust boundary. Outside it, external webhook callers reach an Ingress, which routes to the :8080 user listener and has no route to :8443. Inside the cluster, Agent and AgentTask Pods reach the :8443 cluster listener with mTLS or a bearer token, the controller reaches it with its own mTLS certificate, the kubelet probes :8081, and Prometheus scrapes :9090.](../diagrams/gateway-listener-ports.svg)

## Ports

| Port | Listener | Who calls it | Client auth | Paths |
|---|---|---|---|---|
| `:8443` | Cluster listener, TLS | Agent and AgentTask Pods, gateway-only-tier workloads, the controller | Per path: mTLS SAN, bearer token, or controller SAN ([the `:8443` listener profile](#the-8443-listener-profile)) | LLM proxy, `/v1/mcp/*`, the four internal endpoints |
| `:8080` | User listener, TLS, Ingress-fronted | External webhook callers through the Ingress; platform webhooks | Per AgentChannel ([the `:8080` listener profile](#the-8080-listener-profile)) | `/channels/*`, `/v1/channels/responses/*` |
| `:8081` | Health, TLS, no client auth | The kubelet | None | `/healthz`, `/readyz` ([Gateway readiness](llm/operations.md#gateway-readiness)) |
| `:9090` | Metrics, plaintext | Prometheus, in-cluster | None | `/metrics` ([Metrics](../operations/observability.md#metrics)) |

A fifth port, off by default, serves Go profiles when `gateway.pprofPort` is set ([Profiling](../operations/observability.md#profiling)).

### Why two listeners and a separate health port

The listeners split by exposure, not by subsystem, and the split is a security boundary. `:8080` hosts no mTLS-authenticated path, so an Ingress fronting it cannot reach an endpoint whose authorization assumes a controller-SAN client certificate; the controller-only endpoints exist on `:8443` alone, which no Ingress routes to ([Controller-only paths on the same socket](listener-tls.md#controller-only-paths-on-the-same-socket)).

Probes get their own port for the same reason. The controller's `:9443` lets cert-less kubelet probes share the socket with mTLS paths, because that port is reachable only inside the cluster ([Control plane](../concepts/system-architecture.md#control-plane)). Both gateway listeners are reachable beyond that boundary: `:8080` sits behind a user-provisioned Ingress in the full-lifecycle tier, and `:8443` is reachable by every mTLS-authenticated agent. A cert-less probe path on either would be a cert-less path an outsider could reach, so probes terminate on `:8081`, outside both listener auth profiles.


## The call surfaces

The Deployment serves four kinds of request. Agents send two of them, LLM calls and tool calls, which the gateway forwards to a provider or an MCP server. Channel platforms send the third, a message for an agent, which the gateway delivers and answers. The fourth is the internal API: agents report to it, and the controller reads from it.

![The gateway in the middle, with three colored flows. Brown, the User Gateway: a channel platform sends a message to :8080, the gateway delivers it to the agent Pod over mTLS, and the reply goes back to the platform. Purple, the LLM proxy and tool broker: Agent and AgentTask Pods call :8443, and the gateway forwards to LLM providers and MCP servers with the credential injected. Blue, the internal API: Pods report heartbeats and task completion to :8443, and the controller reads activity and channel health from it.](../diagrams/gateway-call-surfaces.svg)

| Surface | Listener | Direction | What the gateway does | Specified on |
|---|---|---|---|---|
| LLM proxy | `:8443` | Agent to provider | Identifies the caller ([Workload identity](llm/workload-identity.md)), resolves the provider and model, applies the three tenancy gates, budgets, and rate limits, forwards with the credential injected, walks the fallback chain on failure, and meters usage and spend | [LLM Gateway](llm/overview.md), [Request handling](llm/request-handling.md#request-flow) |
| Tool broker | `:8443` | Agent to MCP server | The same caller identity, then the tool grant chain, credential injection, per-call audit, and rate limits | [The tool plane](tool-plane.md) |
| User Gateway | `:8080` | Platform to agent, and the reply back | Authenticates per AgentChannel, normalizes the message, wakes a hibernated Agent through the controller, delivers over mTLS, and returns the reply synchronously, by callback, by polling, or through the platform's API | [User Gateway](user/overview.md#request-flow), [Platform adapters and channel health](user/platform-adapters.md), [Async webhook responses](api/async-responses.md) |
| Internal API | `:8443` | Agent to gateway, controller to gateway | Task completion and heartbeats in; activity and channel health out | [Internal endpoints](#internal-endpoints) |

The LLM proxy and the tool broker share one caller-identity step (an mTLS SAN for Kaalm-managed Pods, a `TokenReview`-validated bearer token for the gateway-only tier) and differ only in what they authorize afterwards. The User Gateway depends on the controller for wake-on-demand: while the activator endpoint is unreachable, a hibernated Agent cannot receive a message and the caller gets `controller_unavailable` ([The activator](user/activation-and-activity.md#the-activator)).

## The :8443 listener profile

All paths on `:8443` share the same listener and serve TLS; client-auth requirements vary per path.

| Path family | Client auth |
|---|---|
| LLM proxy (`/v1/messages`, `/v1/chat/completions`, provider-specific paths) | The dual-mode caller-identity profile: mTLS with Agent/AgentTask SAN, **or** `TokenReview`-validated SA bearer token (gateway-only tier) |
| Tool plane (`/v1/mcp/*`; see [The tool plane](tool-plane.md)) | The same dual-mode caller-identity profile |
| `POST /v1/task/complete` | mTLS, Agent/AgentTask SAN admitted at listener; AgentTask only at handler (Agent callers rejected with 403) |
| `POST /v1/agent/heartbeat` | mTLS, Agent/AgentTask SAN admitted at listener; Agent only at handler (AgentTask callers rejected with 403) |
| `GET /v1/activity`, `GET /v1/channels/health` | mTLS, Controller SAN required (Agent/AgentTask certs rejected with 403) |

The LLM proxy and tool plane rows deliberately share one authentication profile: establishing the caller's identity is plane-independent, so the dual-mode middleware carries no plane-specific logic, and what differs per plane is authorization (the provider chain versus the [tool grant chain](tool-plane.md#grants)), enforced by each handler.

See [Task completion](api/task-complete.md) and [POST /v1/agent/heartbeat](api/agent-endpoints.md#post-v1agentheartbeat) for the handler-level caller-type checks. `/v1/task/complete` additionally enforces a Pod-identity check (the calling Pod's UID must match `AgentTask.status.currentPodUID`; a mismatch is rejected with `403 StalePodCompletion`) and a terminal-phase check (`status.phase ∈ {Succeeded, Failed, TimedOut}` is rejected with `403 TaskAlreadyCompleted`). Both are gated **after** the AgentTask SAN admission and the `agentReported` mode check; see [Child resources](../runtime/child-resources.md) for the field-stamping protocol.

The mTLS-with-SAN enforcement is implemented as [per-path middleware on the listener](listener-tls.md#per-path-client-auth-enforcement). The listener handshake uses `tls.Config.ClientAuth = tls.VerifyClientCertIfGiven`, so bearer-token callers on the LLM proxy paths can complete the TLS handshake without a client cert. The handlers on mTLS-required paths then reject a request missing a client cert with `401` and a cert whose SAN does not match with `403`. A routing bug here would let agent-cert holders reach controller-only paths, so the path-to-auth mapping is the detail the gateway's authorization depends on most. The auth modes themselves (SAN shapes, the `TokenReview` flow, and the cross-checks) are explained under [workload identity](llm/workload-identity.md).

## The :8080 listener profile

The User Gateway listener serves only externally-reachable channel traffic and uses **per-AgentChannel** webhook auth (bearer or HMAC, configured on each AgentChannel) on both path families:

| Path family | Client auth |
|---|---|
| Webhook intake (`/channels/{namespace}/{channel-path}`) | Per-AgentChannel `spec.webhook.auth` (bearer **or** HMAC) |
| Async polling endpoint (`GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}`) | The originating AgentChannel's `spec.webhook.auth` policy (see below) |

For the polling endpoint, the caller supplies the originating AgentChannel's path. The gateway picks that channel's `spec.webhook.auth` policy and asserts it matches the stored response's channel labels; see [Async webhook responses](api/async-responses.md) for the channel-match check. No mTLS-authenticated paths live on `:8080`; the listener split keeps an Ingress fronting `:8080` from routing untrusted traffic to controller-only endpoints.

## Internal endpoints

The Gateway-Internal API endpoints are not part of the LLM proxy or webhook flows. They share the `:8443` cluster listener so that mTLS-authenticated callers (agents, AgentTasks, controller) reach them without standing up a separate listener. They are [reserved](api/overview.md#reserved-gateway-paths) under the `/v1/` prefix:

- [`POST /v1/task/complete`](api/task-complete.md) (agent to gateway): the AgentTask agent reports completion; the gateway updates the pre-existing per-task completion ConfigMap referenced under [Child resources](../runtime/child-resources.md).
- [`POST /v1/agent/heartbeat`](api/agent-endpoints.md#post-v1agentheartbeat) (agent to gateway): liveness signal feeding the agent-heartbeat activity source.
- [`GET /v1/activity?namespace={ns}`](user/activation-and-activity.md#activity-tracking-api) (controller to gateway): per-namespace activity timestamps for idle and hibernation transitions.
- [`GET /v1/channels/health`](api/internal-endpoints.md#get-v1channelshealth) (controller to gateway): per-channel platform-connection health.


## Credentials and RBAC

LLM and tool credentials are Secrets in `kaalm-system`, read by the gateway and never leaving the namespace ([Credential handling](../security/credentials.md#lifecycle-of-an-llm-api-key)). The gateway ServiceAccount holds four kinds of grant ([Gateway ServiceAccount permissions](../security/rbac.md#gateway-serviceaccount-permissions)):

| Grant | Scope | Used for |
|---|---|---|
| `create` on `tokenreviews` | Cluster | Bearer-token callers in the gateway-only tier |
| `get, list, watch` on the Kaalm CRDs, Pods, and Services | Cluster | Provider and channel routing, the source-IP to Pod cross-check, delivery endpoints |
| Secrets and ConfigMaps | `kaalm-system` only | Credentials, the budget exchange, async response records |
| Per-channel and per-task Roles | One user namespace each, `resourceNames`-scoped | Reading a channel's auth Secrets; writing one task's completion ConfigMap (`update, patch`, never `create`) |

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
