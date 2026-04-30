# Agentry — Architecture Overview

This document describes the high-level architecture of Agentry: the control plane, the data plane, and the integration points with the surrounding ecosystem. Agentry is single-cluster in v1; multi-cluster federation is out of scope.

## Documentation Map

| Document | Contents |
|---|---|
| [VISION.md](./VISION.md) | Problem statement, design principles, v1 scope |
| [STORIES.md](./STORIES.md) | Personas and user scenarios driving the design |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | This file — system topology, control/data plane, deployment |
| [API_RESOURCES.md](./API_RESOURCES.md) | CRD specs: AgentClass, ModelProvider, Agent, AgentTask, AgentChannel |
| [API_ENDPOINTS.md](./API_ENDPOINTS.md) | Gateway HTTP endpoints and agent-implemented contracts |
| [GATEWAY_LLM.md](./GATEWAY_LLM.md) | LLM Gateway: routing, budget, fallback, TLS, credentials |
| [GATEWAY_USER.md](./GATEWAY_USER.md) | User Gateway: webhook delivery, activator, activity tracking |
| [CONTROLLER_RECONCILERS.md](./CONTROLLER_RECONCILERS.md) | Operator structure, five reconcilers, error handling |
| [CONTROLLER_LIFECYCLE.md](./CONTROLLER_LIFECYCLE.md) | State machines for Agent and AgentTask, finalizers |
| [SECURITY.md](./SECURITY.md) | Trust model, RBAC, credential lifecycle, TLS, isolation |
| [DEPLOYMENT.md](./DEPLOYMENT.md) | Helm chart contents, prerequisites, certificate lifecycle, tiered on-ramp |
| [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) | The contract a container image must satisfy to run as an Agent or AgentTask |
| [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md) | Go and Python starter templates implementing the runtime contract |
| [OBSERVABILITY.md](./OBSERVABILITY.md) | Aggregated metrics catalog, dashboards, alerting (TODO) |

## System Topology

```mermaid
flowchart LR
    %% ── Inbound external ─────────────────────────────────
    webhooks["📨 Webhook Callers<br/>(external systems, bots)"]

    %% ── Agentry Gateway (agentry-system) ─────────────────
    subgraph gw["🌐 Agentry Gateway · agentry-system"]
        direction TB
        usergw["<b>User Gateway</b> :8080 (TLS)<br/>Webhook adapter • Normalize<br/>Wake on hibernate"]
        llmgw["<b>LLM Gateway</b> :8443 (TLS)<br/>Auth (mTLS / SA Bearer)<br/>Budget • Rate limits • Fallback<br/>Provider adapters"]
    end

    %% ── User namespaces (agent + workload pods) ──────────
    subgraph users["👥 User namespaces"]
        direction TB
        agentC["🤖 <b>Agent Pod</b><br/>(Agentry-managed)<br/>mTLS client cert<br/>POST /v1/message · GET /health"]
        agentTaskC["📋 <b>AgentTask Pod</b><br/>(Agentry-managed)<br/>mTLS client cert<br/>Ephemeral · no inbound endpoint"]
        workloadC["📦 <b>Workload Pod</b><br/>(gateway-only, BYO)<br/>Projected SA token<br/>Calls LLM Gateway only"]
    end

    %% ── Outbound external ────────────────────────────────
    providers["☁️ LLM Provider APIs<br/>(Anthropic, OpenAI, …)"]

    %% ── Controller + apiserver (control plane) ───────────
    subgraph ctrl["⚙️ Agentry Controller · agentry-system"]
        direction TB
        recs["<b>Reconcilers</b><br/>Agent • AgentTask<br/>ModelProvider • AgentClass<br/>AgentChannel"]
        activator["<b>Activator</b> :9443 (mTLS)"]
    end
    apiserver[("🗄️ Kubernetes API")]

    %% ── Data plane (thick) ───────────────────────────────
    webhooks == "inbound" ==> usergw
    usergw == "deliver (mTLS)<br/>POST /v1/message" ==> agentC
    agentC & agentTaskC == "LLM (mTLS)" ==> llmgw
    workloadC == "LLM (SA Bearer)" ==> llmgw
    llmgw == "egress" ==> providers

    %% ── Lifecycle (thin) ─────────────────────────────────
    ctrl -- "watches / reconciles" --> apiserver
    apiserver -- "Pods • PVCs • Services • ConfigMaps<br/>NetworkPolicies • ServiceAccounts" --> users

    %% ── Internal mTLS RPCs (dashed) ──────────────────────
    gw -. "POST /v1/activate/{ns}/{name} (mTLS)" .-> activator
    ctrl -. "GET /v1/activity (mTLS)" .-> llmgw
    ctrl -. "GET /v1/channels/health (mTLS)" .-> llmgw
    agentC -. "POST /v1/agent/heartbeat (mTLS)" .-> llmgw
    agentTaskC -. "POST /v1/task/complete (mTLS)" .-> llmgw

    %% ── Styling (high contrast) ──────────────────────────
    classDef extNode fill:#FFE4B5,stroke:#8B4513,stroke-width:2px,color:#3a1f00
    classDef gwNode  fill:#90EE90,stroke:#1B5E20,stroke-width:2px,color:#0b3d0b
    classDef ctlNode fill:#87CEEB,stroke:#0D47A1,stroke-width:2px,color:#0a2540
    classDef podNode fill:#E6E6FA,stroke:#4A148C,stroke-width:2px,color:#2a0a4a
    classDef k8sNode fill:#FFD580,stroke:#7a3f00,stroke-width:2px,color:#3a1f00

    class webhooks,providers extNode
    class usergw,llmgw gwNode
    class recs,activator ctlNode
    class agentC,agentTaskC,workloadC podNode
    class apiserver k8sNode
```

The dashed edges are internal mTLS RPCs. Unlike the LLM proxy — which accepts either an Agent/AgentTask client cert or a `TokenReview`-validated SA bearer token (gateway-only tier) — these endpoints reject the SA-bearer path entirely. Listener middleware enforces a controller SAN on `/v1/activity` and `/v1/channels/health`, an Agent-or-AgentTask SAN on `/v1/task/complete` and `/v1/agent/heartbeat` (with handler-level caller-type checks layered on — e.g., `/v1/task/complete` returns `403` if the calling Pod is not an AgentTask), and a gateway SAN on the activator. Four are served on the Gateway's `:8443` listener (activity, channel health, `/v1/task/complete`, `/v1/agent/heartbeat`); the activator wake is served on the Controller's `:9443` (see [Control Plane](#control-plane)). [SAN-based authorization](./SECURITY.md#internal-endpoint-authentication-activator-activity-channel-health) is implemented as per-path middleware; the consolidated path → auth mapping for the gateway listener appears under [The Agentry Gateway](#the-agentry-gateway).

## Control Plane

The Agentry control plane consists of a single operator (Go, built on `controller-runtime`) running as a Deployment in a dedicated namespace (`agentry-system`). The operator hosts five reconcilers:

1. [**Agent Reconciler**](./CONTROLLER_RECONCILERS.md#agentreconciler) — watches `Agent` resources. Translates each Agent into a Pod, PVC, Service, and ConfigMap, and drives the [persistent-agent state machine](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode) (idle detection, hibernation, wake-on-demand).

2. [**AgentTask Reconciler**](./CONTROLLER_RECONCILERS.md#agenttaskreconciler) — watches `AgentTask` resources. Creates a Pod to execute the task, monitors the [completion condition](./CONTROLLER_LIFECYCLE.md#agenttask) (`agentReported` or `exitCode`), and tears down resources on completion or timeout.

3. [**ModelProvider Reconciler**](./CONTROLLER_RECONCILERS.md#modelproviderreconciler) — watches `ModelProvider` resources. Validates provider configuration, verifies the referenced Secret exists and is well-formed, maintains provider health status, and manages per-namespace spend state.

4. [**AgentClass Reconciler**](./CONTROLLER_RECONCILERS.md#agentclassreconciler) — watches `AgentClass` resources. Validates that referenced ModelProviders exist, maintains usage counts, and updates status conditions.

5. [**AgentChannel Reconciler**](./CONTROLLER_RECONCILERS.md#agentchannelreconciler) — watches `AgentChannel` resources. Validates that the referenced Agent exists and has a Service, validates channel credentials, and populates `status.conditions[type=PlatformConnected]` (observational only) by polling the gateway via [`GET /v1/channels/health`](./API_ENDPOINTS.md#get-v1channelshealth-internal--controller-use-only). The gateway gates webhook routing on `Ready` alone; `PlatformConnected` is for user/operator visibility. See [GATEWAY_USER.md § Channel Health Tracking](./GATEWAY_USER.md#channel-health-tracking) for the rolling-window tri-state contract and the per-replica reduction rules.

The controller does **not** host admission webhooks. Field-level validation uses CEL expressions in CRD schemas; [cross-resource validation](./API_RESOURCES.md#cross-resource-validation) (reference resolution, image allowlists, provider access) is handled at reconcile time and surfaced as status conditions rather than admission errors.

The controller exposes an internal ClusterIP Service (`agentry-controller.agentry-system.svc.cluster.local`, default port 9443) for the activator endpoint (`POST /v1/activate/{namespace}/{agentName}`). The same listener on each controller Pod also serves `/healthz` and `/readyz` for kubelet probes, which target the Pod directly rather than going through the Service. The activator endpoint requires [**mTLS**](./SECURITY.md#internal-endpoint-authentication-activator-activity-channel-health): the controller serves TLS with the `agentry-controller-tls` certificate, and the gateway must present its `agentry-gateway-tls` client cert with a SAN matching the gateway Service DNS — any other CA-signed cert is rejected. The listener uses the same [mixed client-auth pattern as the gateway's `:8443`](./GATEWAY_LLM.md#per-path-client-auth-enforcement) — `tls.VerifyClientCertIfGiven` at the handshake so cert-less kubelet probes succeed, with per-path HTTP middleware enforcing mTLS-with-SAN on `/v1/activate` only. Both certificates are issued by the `agentry-ca-issuer` `ClusterIssuer` (see [Deployment Model](#deployment-model)) and rotated by cert-manager.

The activator handler is served on **every** controller replica, not only the leader: the handler patches `agentry.io/wake=true` on the target Agent, and the leader's existing Agent watch fires the manual-wake path in the reconciler. This keeps the Service round-robin behavior correct without any leader-aware endpoint plumbing.

The gateway uses this Service to send wake requests when a channel message arrives for a hibernated agent. The activator returns 202 Accepted as soon as the wake annotation patch is committed; the gateway observes wake completion by polling the agent's Service for readiness, not by waiting on the [activator response](./GATEWAY_USER.md#activator) (steps 3–4).

The reverse direction — controller → gateway — is the [**activity API**](./GATEWAY_USER.md#activity-tracking-api) (`GET /v1/activity?namespace={ns}`), used by the AgentReconciler to read per-namespace last-activity timestamps for idle and hibernation transitions. It is served on the gateway's `:8443` LLM listener (**not** the User listener on `:8080`, so an Ingress fronting `:8080` cannot route untrusted traffic to it). Per-path middleware on this listener enforces [mTLS-with-SAN](./GATEWAY_LLM.md#per-path-client-auth-enforcement) on `/v1/activity` — the controller presents `agentry-controller-tls`, and only client certs whose SAN matches the controller Service DNS are admitted (Agent/AgentTask certs are rejected with 403). The controller dials each gateway Pod IP directly rather than the Service, since [activity timestamps are in-memory per replica](#the-agentry-gateway). To keep TLS verification working when the dial address is a Pod IP rather than a name, the controller sets `ServerName` on the TLS handshake to the gateway Service DNS (`agentry-gateway.agentry-system.svc`); the gateway's `agentry-gateway-tls` cert SAN covers the Service DNS only, not per-Pod IPs. The same pattern is reused by the channel-health fan-out — see [GATEWAY_USER.md § Channel Health Tracking](./GATEWAY_USER.md#channel-health-tracking).

Leader election is enabled so the operator can run with multiple replicas for availability.

The controller's [RBAC surface](./SECURITY.md#operator-serviceaccount) covers cluster-scoped CRD watches, child-resource management (including cluster-wide Pod read/list/watch for managing Agent/AgentTask Pods in user namespaces and for fanning out activity and channel-health queries to gateway Pods in `agentry-system`), and dynamic per-channel and per-task `Role`/`RoleBinding`s in user namespaces.

**Multi-tenancy.** v1 assumes a single platform team owns the cluster-scoped policy resources (AgentClass, ModelProvider) while individual tenants operate at namespace boundaries via Agent, AgentTask, and AgentChannel. Cross-tenant access to providers is gated by `ModelProvider.allowedNamespaces` and `AgentClass.allowedProviders` — both must pass for an Agent to use a provider (see [API_RESOURCES.md § AgentClass](./API_RESOURCES.md#agentclass)). Webhook paths on AgentChannel are namespace-prefixed by a CRD-level CEL rule (`/channels/{namespace}/...`), so cross-tenant path collisions are impossible by construction (see [API_RESOURCES.md § AgentChannel](./API_RESOURCES.md#agentchannel)).

## Data Plane

The data plane is what actually runs when an Agent is created. For each Agent in `Running` state, the controller provisions:

- **One Pod** containing the user's agent container. The Pod runs under the [RuntimeClass](./SECURITY.md#runtimeclass) specified by its AgentClass (runc, gVisor, or Kata).
- **One PVC** if the [Agent spec requests persistence](./API_RESOURCES.md#spec-2), mounted into the agent container at a configured path.
- **One Service** (ClusterIP) if [`spec.service.enabled`](./API_RESOURCES.md#spec-2) (default `true`), exposing the agent's HTTPS endpoint for intra-cluster traffic. The gateway uses this Service to deliver channel messages via [`POST /v1/message`](./API_ENDPOINTS.md#post-v1message-agent-only--agent-implemented) over TLS; direct external exposure remains the developer's responsibility. Agents with the Service disabled are outbound-only — they have no inbound delivery path and cannot be referenced by an AgentChannel (validated by AgentChannelReconciler with `Ready=False, reason=AgentServiceDisabled`).
- **One [cert-manager `Certificate`](./SECURITY.md#lifecycle-of-an-agent-tls-serving-certificate)** (and the Secret it writes) holding a per-agent TLS cert (`server auth, client auth`) signed by the `agentry-ca-issuer` `ClusterIssuer` and rotated continuously by cert-manager; the same cert serves the agent's HTTPS listener and is presented client-side on every agent→gateway call. The Agentry CA bundle is projected into Pods at `/var/run/agentry/ca.crt` from the `agentry-ca` ConfigMap maintained by trust-manager, and kubelet refreshes the volume when the ConfigMap changes.
- **One ConfigMap** holding non-sensitive agent configuration (gateway endpoint, feature flags).
- **One ServiceAccount** (`agent-{agentName}`, no RoleBindings by default — the agent has no Kubernetes API access unless the platform team or developer explicitly grants it; see [SECURITY.md § Agent Pod ServiceAccount](./SECURITY.md#agent-pod-serviceaccount)).
- **One NetworkPolicy** synthesized from the AgentClass network policy and the gateway's egress allow rule. This is the load-bearing primitive cited in the [gateway architecture analysis](./GATEWAY_LLM.md#architecture-option-analysis) for keeping LLM credentials inside `agentry-system` — see the [full rule set](./CONTROLLER_RECONCILERS.md#agentreconciler) (AgentReconciler step 6). **NetworkPolicy enforcement by the cluster CNI is a required prerequisite of Agentry's trust model.** On the message path, the synthesized ingress rule is **layered with the [agent-side mTLS check on `POST /v1/message`](./SECURITY.md#in-cluster-tls-bidirectional)** (specified in [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md), bullet 4) — a misconfigured per-Agent NP does not open delivery to arbitrary in-cluster callers. The synthesized egress rule remains the sole control preventing agents from calling provider IPs directly, which is why CNI enforcement of NetworkPolicy is still a hard prerequisite. Clusters running default kindnet or default flannel do not enforce NetworkPolicy and are not supported deployment targets. See also [Recommendation #4](./SECURITY.md#recommendations-for-deployment).

There is no sidecar container. The **Agentry Gateway** in `agentry-system` handles all LLM traffic and inbound channel messages as a shared cluster-level service.

For each AgentTask, the controller provisions a parallel set of resources tailored to its ephemeral, no-inbound nature (see [AgentTaskReconciler](./CONTROLLER_RECONCILERS.md#agenttaskreconciler) for the authoritative step list):

- **One Pod** containing the user's task container, under the AgentClass [RuntimeClass](./SECURITY.md#runtimeclass).
- **One PVC** if the task spec requests persistence.
- **One [cert-manager `Certificate`](./SECURITY.md#lifecycle-of-an-agenttask-tls-client-certificate)** (and its Secret) holding a per-task TLS cert with `usages: client auth` only — the task uses it to authenticate outbound calls (LLM proxy, `/v1/task/complete`); there is no server-auth EKU because the task does not expose an HTTPS listener.
- **One ServiceAccount** (the Pod's identity for any in-cluster API access).
- **One NetworkPolicy** synthesized from the AgentClass and the gateway's egress allow rule.
- When [`completion.condition: agentReported`](./CONTROLLER_LIFECYCLE.md#agenttask), additionally: a pre-created `{taskName}-completion` ConfigMap (initial `data: {}`) where the gateway writes the completion payload, plus a per-task `Role` and `RoleBinding` granting the gateway ServiceAccount name-scoped `update`/`patch` on that ConfigMap.

There is no Service (tasks do not receive channel messages and have no stable endpoint) and no generic configuration ConfigMap (task config is delivered via env vars and Pod spec). All resources are owner-referenced to the AgentTask for cascade GC.

## The Agentry Gateway

The gateway is a replicated Deployment in `agentry-system`. It exposes two TLS listeners — `:8443` for the LLM proxy and internal mTLS endpoints (agent/controller → gateway), `:8080` for inbound channel webhooks — together hosting three classes of traffic, grouped below by call class:

[**LLM Gateway**](./GATEWAY_LLM.md#llm-gateway--request-flow) (outbound, agent → provider).
- Serves TLS on port 8443; agent containers connect via `$AGENTRY_GATEWAY_ENDPOINT` (HTTPS, always injected)
- [Identifies the source namespace](./GATEWAY_LLM.md#namespace-identification) via mTLS client-cert SAN (Agentry-managed Agent/AgentTask Pods) or `TokenReview`-validated ServiceAccount bearer token (gateway-only tier), with a source-IP → Pod cross-check from the informer cache as defense in depth
- [Resolves the target ModelProvider](./GATEWAY_LLM.md#provider-routing) from the qualified `provider/model` name and the Agent's `spec.providers`, then [validates model and namespace access](./GATEWAY_LLM.md#request-format-detection); gateway-only-tier callers (no Agent resource) are gated by `ModelProvider.allowedNamespaces` alone, since there is no Agent or AgentClass to consult
- Enforces soft budget guardrails and per-namespace rate limits
- Routes to the upstream provider; on failure, walks the [fallback chain](./GATEWAY_LLM.md#fallback-logic)
- [Extracts token usage](./GATEWAY_LLM.md#provider-adapters) and [updates spend counters](./GATEWAY_LLM.md#budget-state-management)
- Returns structured [error responses](./API_ENDPOINTS.md#llm-gateway-error-responses) (JSON with `error.type`) on failure

[**User Gateway**](./GATEWAY_USER.md#user-gateway--request-flow) (inbound, channel → agent).
- Watches `AgentChannel` resources directly (rather than reading routing data published by the controller) so inbound webhook routing reflects channel changes without controller-mediated propagation latency. The gateway [routes only to channels](./GATEWAY_USER.md#user-gateway--request-flow) whose `status.conditions[Ready].status == True` (step 4), so controller validation (path conflicts, bad references) still gates traffic
- Listens on port 8080 over [TLS](./GATEWAY_USER.md#tls-and-ingress) (backend re-encrypt vs TLS pass-through)
- Hosts two path families on `:8080`: webhook intake under `/channels/{namespace}/...` and the async polling fallback at `GET /v1/channels/responses/{requestId}` — see [API_ENDPOINTS.md § Reserved Gateway Paths](./API_ENDPOINTS.md#reserved-gateway-paths) and [§ Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed)
- Normalizes inbound webhook payloads into the Agentry message envelope and resolves the target Agent via the AgentChannel resource
- On delivery, resolves the Agent's Service endpoints; if the Service has no endpoints (a hibernated Agent's Service is retained but unpopulated — the gateway uses endpoint-absence as the hibernation signal rather than watching Agent resources, see [CONTROLLER_LIFECYCLE.md § Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection) and [GATEWAY_USER.md § Activator](./GATEWAY_USER.md#activator)), signals the controller to wake the agent via the [mTLS-authenticated activator endpoint](./GATEWAY_USER.md#activator) and waits until the Pod is ready (bounded by `wakeTimeout`)
- Delivers the message to the agent container via [`POST /v1/message`](./API_ENDPOINTS.md#post-v1message-agent-only--agent-implemented)
- Supports per-AgentChannel sync (default) and async response modes — [configuration](./API_RESOURCES.md#spec-4) and [callback/polling protocol](./API_ENDPOINTS.md#async-webhook-response-gateway-managed)
- v1 supports webhook channels only; Discord and WhatsApp adapters are planned for v1.1

**Gateway-Internal API** (mTLS on `:8443`) — these endpoints are not part of the LLM proxy or webhook flows but share the LLM Gateway listener so that mTLS-authenticated callers (agents, AgentTasks, controller) reach them without standing up a separate listener. [Reserved](./API_ENDPOINTS.md#reserved-gateway-paths) under the `/v1/` prefix.
- [`POST /v1/task/complete`](./API_ENDPOINTS.md#post-v1taskcomplete-agenttask-only) (agent → gateway): the AgentTask agent reports completion; the gateway updates the pre-existing `{taskName}-completion` ConfigMap referenced under [Data Plane](#data-plane).
- [`POST /v1/agent/heartbeat`](./API_ENDPOINTS.md#post-v1agentheartbeat-agent-only) (agent → gateway): liveness signal feeding the `heartbeat` activity source.
- [`GET /v1/activity?namespace={ns}`](./GATEWAY_USER.md#activity-tracking-api) (controller → gateway): per-namespace activity timestamps for idle and hibernation transitions.
- [`GET /v1/channels/health`](./API_ENDPOINTS.md#get-v1channelshealth-internal--controller-use-only) (controller → gateway): per-channel platform-connection health.

**`:8443` listener auth profile** (consolidated; all paths share the same listener and serve TLS, while client-auth requirements vary per path):

| Path family | Client auth |
|---|---|
| LLM proxy (`/v1/messages`, `/v1/chat/completions`, provider-specific paths) | mTLS with Agent/AgentTask SAN, **or** `TokenReview`-validated SA bearer token (gateway-only tier) |
| `POST /v1/task/complete`, `POST /v1/agent/heartbeat` | mTLS, Agent/AgentTask SAN required |
| `GET /v1/activity`, `GET /v1/channels/health` | mTLS, Controller SAN required (Agent/AgentTask certs rejected with 403) |

The Agent/AgentTask SAN distinction is not enforced at the listener middleware; per-endpoint caller-type enforcement happens at the handler (e.g., `/v1/task/complete` returns `403` if the calling Pod is not an AgentTask). See [API_ENDPOINTS.md § /v1/task/complete](./API_ENDPOINTS.md#post-v1taskcomplete-agenttask-only) and [§ /v1/agent/heartbeat](./API_ENDPOINTS.md#post-v1agentheartbeat-agent-only).

The mTLS-with-SAN enforcement is implemented as [per-path middleware on the listener](./GATEWAY_LLM.md#per-path-client-auth-enforcement); a routing bug here would let agent-cert holders reach controller-only paths, so the path → auth mapping is the most security-load-bearing detail in the gateway.

**`:8080` listener auth profile.** The User Gateway listener serves only externally-reachable channel traffic and uses **per-AgentChannel** webhook auth (bearer or HMAC, configured on each AgentChannel) on both path families:

| Path family | Client auth |
|---|---|
| Webhook intake (`/channels/{namespace}/{channel-path}`) | Per-AgentChannel `spec.webhook.auth` (bearer **or** HMAC) |
| Async polling fallback (`GET /v1/channels/responses/{requestId}`) | Same auth as the originating AgentChannel; channel-match asserted against the stored response |

See [API_ENDPOINTS.md § Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) for the polling endpoint's channel-match check. No mTLS-authenticated paths live on `:8080`; the listener split keeps an Ingress fronting `:8080` from routing untrusted traffic to controller-only endpoints.

[LLM provider credentials](./SECURITY.md#lifecycle-of-an-llm-api-key) are stored as Secrets in `agentry-system` and read directly by the gateway. They never leave the `agentry-system` namespace.

The gateway's [RBAC surface](./SECURITY.md#gateway-serviceaccount-permissions) covers cluster-scoped `create` on `tokenreviews` for the gateway-only tier, the cluster-wide Pod watch (used for the source-IP → Pod cross-check called out under [LLM Gateway](#the-agentry-gateway) as defense in depth) and the cluster-wide AgentChannel watch for routing, per-task `Role`/`RoleBinding`s granting name-scoped `update`/`patch` on each `{taskName}-completion` ConfigMap, and per-AgentChannel `Role`s granting name-scoped `get, list, watch` on each AgentChannel's webhook auth Secret in the agent's namespace, bound to both the gateway and controller ServiceAccounts (the Roles and RoleBindings are issued at reconcile time and cascade-deleted with their owners).

**Multi-replica state.** The gateway runs as multiple replicas. Each piece of cross-replica state uses one of four reconciliation strategies — cross-replica ConfigMap with a controller-side reducer (spend), local recomputation against a cluster-wide ceiling (rate limits), per-replica in-memory state merged controller-side at read time (activity, channel health), or per-request cross-replica ConfigMap with controller-side TTL pruning (async webhook responses) — across five pieces of state:

- [**Spend counters**](./GATEWAY_LLM.md#budget-state-management): each replica server-side-applies its partials to the `agentry-budget-{providerName}` ConfigMap in `agentry-system` (keyed by Pod name); the [ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler) sums partials, prunes stale-replica entries, and writes a `_canonical` total that replicas re-initialize from on startup (step 3). Bounded overspend is accepted as a soft-guardrail trade-off.
- [**Rate-limit token buckets**](./GATEWAY_LLM.md#rate-limiting): each replica divides the configured cluster-wide ceiling by the live replica count from its Pod informer and adjusts on the next refill cycle when replicas scale.
- [**Activity timestamps**](./GATEWAY_USER.md#activity-tracking-api): kept in-memory per replica (no etcd writes per request). The gateway records two signal sources separately per agent — `gatewayTraffic` (LLM-gateway requests and inbound channel-message deliveries observed by that replica) and `heartbeat` (agent-emitted heartbeats); the controller selects which to use per-Agent via `spec.lifecycle.activitySource` (`gatewayTraffic`, `agentHeartbeat`, or `both`, where `both` takes the max of the two timestamps) when evaluating idle and hibernation transitions. The [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) enumerates gateway Pods via its Pod informer in `agentry-system` and fans out one `GET /v1/activity?namespace={ns}` per Pod IP, takes the most-recent timestamp per agent per source across responses, and caches the result in a short reconciler-local window to bound query load (step 8). Activity state is ephemeral; each replica's response includes its own `startedAt`, which the controller compares against the Agent's `status.phaseTransitionTime` — a recently-restarted replica's missing data is treated as unknown while fresher data from peers is still used, and idle/hibernation transitions are deferred only when no replica has been up for `idleTimeout` — meaning a synchronized gateway restart defers all such transitions for `idleTimeout` post-restart (see [CONTROLLER_LIFECYCLE.md § Activity Detection](./CONTROLLER_LIFECYCLE.md#activity-detection)). Replicas that fail to respond (connection refused, timeout) are skipped; the controller uses the data from the remaining replicas. If every replica is unreachable, the controller preserves the agent's current phase rather than transitioning.
- [**Channel-health observations**](./GATEWAY_USER.md#channel-health-tracking): kept in-memory per replica using the same strategy as activity (no etcd writes per request). Each replica maintains a bounded in-window observation list per registered channel path; the [AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler) (step 4) fans out `GET /v1/channels/health` to every gateway Pod IP and reduces the per-replica `success`/`failure`/`empty` states into `status.conditions[type=PlatformConnected]` using the tri-state rule documented in [GATEWAY_USER.md § Channel Health Tracking](./GATEWAY_USER.md#channel-health-tracking). Replica-startup and all-replicas-unreachable handling mirror the activity path.
- [**Async webhook responses**](./GATEWAY_USER.md#user-gateway--request-flow): for AgentChannels in `responseMode: async`, the receiving replica writes the agent's response (or a delivery error) to a per-request ConfigMap `agentry-async-{requestId}` in `agentry-system`, labeled with the originating channel's namespace and name and annotated with a 1-hour expiry. Any replica serves `GET /v1/channels/responses/{requestId}` by reading this ConfigMap, so polling is replica-agnostic and no in-memory routing is required. The [AgentChannelReconciler](./CONTROLLER_RECONCILERS.md#agentchannelreconciler) prunes expired entries by label selector on reconcile passes; on AgentChannel delete, the [finalizer](./CONTROLLER_LIFECYCLE.md#finalizers) sweeps the channel's remaining `agentry-async-*` ConfigMaps in one shot — load-bearing because Kubernetes GC does not follow cross-namespace ownerRefs.

## Observability

Both the controller and the gateway expose Prometheus metrics on dedicated ports. The metric catalogs and emit-points are documented per-component:

- [Controller metrics](./CONTROLLER_RECONCILERS.md#observability) — reconcile counts/duration/queue depth, agent/task phase counts, hibernation/wake/budget events.
- [LLM Gateway metrics](./GATEWAY_LLM.md#observability) — request counts/duration, token usage, spend, fallback events, budget utilization.
- [User Gateway metrics](./GATEWAY_USER.md#observability) — channel message counts/duration, hibernation wakes triggered.

Aggregated dashboards, alerting recommendations, and log/trace conventions will live in [OBSERVABILITY.md](./OBSERVABILITY.md) (TODO).

## Agent Runtime Contract

Agentry is BYO-image: any container can be an Agent provided it implements a small contract — an HTTPS health endpoint on `$AGENTRY_HEALTH_PORT`, graceful SIGTERM handling, authenticated mTLS calls to `$AGENTRY_GATEWAY_ENDPOINT`, an optional `POST /v1/message` handler when an AgentChannel is in use, and `messageId`-based deduplication when hibernation is enabled.

The full contract — required env vars, TLS reload semantics, dedup buffer, optional heartbeat and task-completion endpoints — is specified in [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md). Working implementations of the contract ship as [Go and Python starter templates](./STARTER_TEMPLATES.md).

## Integration Points

### Agent Sandbox (optional backend — v1.1)

An `AgentClass` will be able to specify [`spec.runtime.backend: agentSandbox`](./API_RESOURCES.md#spec) (v1.1). When set, the Agent Reconciler will create `Sandbox` custom resources (from the SIG Apps Agent Sandbox project) instead of raw Pods. This will give Agentry agents access to Agent Sandbox's warm pools and enhanced isolation without reimplementing those features. In v1, the only supported backend is `pod` — the CRD schema rejects `agentSandbox` at apply time.

### MCP (Model Context Protocol)

Agentry does not mandate MCP but is compatible with it. Agent containers are free to connect to MCP servers for tool access. AgentClass governs egress to MCP servers via [`network.egress.allowedCIDRs`](./API_RESOURCES.md#spec) (portable, every NP-capable CNI) and `network.egress.allowedHosts` (FQDN-policy CNIs only, e.g. Cilium / Calico Enterprise) — see [API_RESOURCES.md § AgentClass design notes](./API_RESOURCES.md#design-notes). MCP server provisioning itself is out of scope for v1.

### LLM Providers

Agentry supports any HTTP-based LLM provider. Out of the box, the gateway understands Anthropic, OpenAI, Google Vertex, and OpenAI-compatible endpoints (including Ollama, vLLM, LiteLLM gateways). Adding a new provider type is a [plugin-style extension in the gateway](./GATEWAY_LLM.md#provider-adapters).

### Channel Platforms

The User Gateway ships with a **generic webhook adapter** in v1 (inbound HTTP POST with configurable auth). Discord and WhatsApp adapters are planned for v1.1 — they require persistent connections and platform-specific reconnection logic. Additional platform adapters follow the same plugin pattern as LLM provider adapters.

## Scoping Summary

| Concern | Where it lives |
|---|---|
| Policy (who can use what, at what cost) | AgentClass, ModelProvider (cluster-scoped) |
| Workload definition | Agent, AgentTask (namespace-scoped) |
| Channel integration | AgentChannel (namespace-scoped) |
| Lifecycle orchestration | Agentry Controller (cluster-level) |
| Runtime isolation | RuntimeClass via AgentClass, or Sandbox backend |
| LLM traffic / spend tracking | LLM Gateway in agentry-system (shared) |
| Channel message routing | User Gateway in agentry-system (shared) |
| Tool access | MCP (external, not managed by Agentry in v1) |
| External exposure | Ingress/Gateway (user-managed, not Agentry) |
| Observability | Controller + Gateway Prometheus metrics; see Observability section |
| In-cluster TLS issuance | cert-manager + trust-manager (prerequisite); see [DEPLOYMENT.md](./DEPLOYMENT.md) |
| Network policy enforcement | NP-capable CNI (prerequisite); see [SECURITY.md § Network Policy](./SECURITY.md#network-policy) |

## Deployment Model

Agentry ships as a Helm chart. **cert-manager, trust-manager, and an NP-enforcing CNI are required prerequisites** — see [SECURITY.md § Network Policy](./SECURITY.md#network-policy) and the NetworkPolicy bullet under [Data Plane](#data-plane). The chart supports a two-tier on-ramp: a **gateway-only** tier (existing workloads point at the gateway via projected ServiceAccount tokens for LLM traffic and spend tracking, with no AgentClass or Agent resources; provider access is gated by `ModelProvider.allowedNamespaces` alone — see [LLM Gateway](#the-agentry-gateway)) and a **full agent lifecycle** tier (Agents, AgentTasks, AgentChannels, hibernation and wake-on-demand, mTLS via per-agent certificates).

At a type level, the chart deploys:

- 5 CRDs (AgentClass, ModelProvider, Agent, AgentTask, AgentChannel)
- Controller and Gateway Deployments (the Gateway with a PodDisruptionBudget and a rolling-update strategy), plus their ServiceAccounts, ClusterRoles, and ClusterRoleBindings
- cert-manager `ClusterIssuer`s (a self-signed root and `agentry-ca-issuer`) and `Certificate`s for the gateway and controller serving certs (per-Agent and per-AgentTask `Certificate`s are issued at reconcile time, not by the chart)
- A trust-manager `Bundle` projecting the `agentry-ca` ConfigMap into non-system namespaces
- A default `standard` AgentClass and an optional `sandboxed` AgentClass example

For the full chart contents, the certificate inventory, the operational Helm values (`gateway.replicas`, `gateway.callbackUrl.allowlist`, `controller.networkPolicy.dnsSelector`, `gateway.externalHostnames`, `gateway.maxFallbackDepth`, `gateway.channelHealthWindow`, `trustManager.bundleSelector`), and the per-tier setup details, see [DEPLOYMENT.md](./DEPLOYMENT.md).
