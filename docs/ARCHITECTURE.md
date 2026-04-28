# Agentry — Architecture Overview

This document describes the high-level architecture of Agentry: the control plane, the data plane, and the integration points with the surrounding ecosystem.

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
| [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) | The contract a container image must satisfy to run as an Agent or AgentTask |
| [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md) | Go and Python starter templates implementing the runtime contract |

## System Topology

```
                          ┌─────────────────────────────────────────┐
                          │              Kubernetes API             │
                          └─────────────────────────────────────────┘
                                           ▲
                                           │ watches / reconciles
                                           │
        ┌──────────────────────────────────┴──────────────────────────────────┐
        │             Agentry Controller   (agentry-system namespace)         │
        │  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐   │
        │  │ Agent Reconciler │  │ AgentTask Reconc │  │ Provider Reconc  │   │
        │  └──────────────────┘  └──────────────────┘  └──────────────────┘   │
        │  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐   │
        │  │AgentClass Reconc │  │AgentChannel Rec. │  │ Activator :9443  │   │
        │  └──────────────────┘  └──────────────────┘  │     (mTLS)       │   │
        │                                              └──────────────────┘   │
        └───────────────────────────────┬─────────────────────────────────────┘
                                        │ creates/updates
                                        ▼
        ┌──────────────────────────────────────────────────────────────────┐
        │                     Kubernetes Primitives                        │
        │  Pods / PVCs / Services / Secrets / ConfigMaps / NetworkPolicies │
        └──────────────────────────────────────────────────────────────────┘
                            │                            │
                            ▼                            ▼
        ┌────────────────────────────────┐  ┌────────────────────────────────┐
        │           Agent Pod            │  │   Existing workload Pod        │
        │       (Agentry-managed)        │  │     (gateway-only tier)        │
        │  ┌──────────────────────────┐  │  │  ┌──────────────────────────┐  │
        │  │ Agent Container          │  │  │  │ Workload (BYO Deployment)│  │
        │  │ • mTLS client cert       │  │  │  │ • Projected SA token     │  │
        │  │ • $AGENTRY_GATEWAY_ENDPT │  │  │  │   (audience:             │  │
        │  │ • POST /v1/message       │  │  │  │    agentry-gateway)      │  │
        │  │ • GET  /health (HTTPS)   │  │  │  │ • Calls gateway LLM only │  │
        │  └──────────────────────────┘  │  │  └──────────────────────────┘  │
        └────────────┬───────────────────┘  └────────────┬───────────────────┘
                  │ LLM (mTLS)        ▲ delivery       │ LLM (Bearer token,
                  ▼                   │ (TLS)          ▼   TokenReview-validated)
        ┌──────────────────────────────────────────────────────────────────┐
        │              Agentry Gateway (agentry-system namespace)          │
        │                                                                  │
        │   ┌─────────────────────────┐  ┌───────────────────────────┐    │
        │   │ LLM Gateway :8443 (TLS) │  │  User Gateway :8080 (TLS) │    │
        │   │                         │  │                            │    │
        │   │  Auth (mTLS / Bearer)   │  │  Webhook adapter           │    │
        │   │  Request validation     │  │  (v1: webhook only;        │    │
        │   │  Budget check           │  │   Discord, WhatsApp v1.1)  │    │
        │   │  Rate limiting          │  │                            │    │
        │   │  Fallback routing       │  │  Message normalization     │    │
        │   │  Provider adapters      │  │  Agent delivery            │    │
        │   │  Token counting         │  │  Response translation      │    │
        │   │  Spend reporting        │  │                            │    │
        │   └──────────┬──────────────┘  └───────────┬───────────────┘    │
        │              │                              │                    │
        │              ▼ (egress)                     ▲ (inbound)         │
        │       LLM Provider APIs              Webhook Callers              │
        │   (Anthropic, OpenAI, etc.)     (external systems, bots, etc.)  │
        └──────────────────────────────────────────────────────────────────┘

        Control-plane interactions between Controller and Gateway (both mTLS):
          Gateway → Controller :9443  — wake hibernated agents (activator)
          Controller → Gateway Pods :8443 — fan-out activity query (`GET /v1/activity`); see "Multi-replica state"
```

## Control Plane

The Agentry control plane consists of a single operator (Go, built on `controller-runtime`) running as a Deployment in a dedicated namespace (`agentry-system`). The operator hosts five reconcilers:

1. **Agent Reconciler** — watches `Agent` resources. Translates each Agent into a Pod, PVC, Service, and ConfigMap. Drives the persistent-agent state machine, including idle detection, hibernation, and wake-on-demand via the gateway activator.

2. **AgentTask Reconciler** — watches `AgentTask` resources. Creates a Pod to execute the task and monitors the completion condition. Two modes are supported: `agentReported` (the agent posts to the gateway's `POST /v1/task/complete`; the reconciler reads status and any declared artifacts from the per-task `{taskName}-completion` ConfigMap) and `exitCode` (success/failure inferred from the container exit code; no artifacts — declaring `spec.artifacts` under this mode is rejected by CRD CEL). Tears down resources on completion or timeout.

3. **ModelProvider Reconciler** — watches `ModelProvider` resources. Validates provider configuration, verifies the referenced Secret exists and is well-formed, maintains provider health status, and manages per-namespace spend state.

4. **AgentClass Reconciler** — watches `AgentClass` resources. Validates that referenced ModelProviders exist, maintains usage counts, and updates status conditions.

5. **AgentChannel Reconciler** — watches `AgentChannel` resources. Validates that the referenced Agent exists and has a Service, validates channel credentials, and monitors channel health. The gateway watches AgentChannel resources directly for platform connection management.

The controller does **not** host admission webhooks. Field-level validation uses CEL expressions in CRD schemas. Cross-resource validation (reference resolution, image allowlists, provider access) is handled at reconcile time and surfaced as status conditions rather than admission errors.

The controller exposes an internal ClusterIP Service (`agentry-controller.agentry-system.svc.cluster.local`, default port 9443) for the activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) and health/readiness probes. The activator endpoint requires **mTLS**: the controller serves TLS with the `agentry-controller-tls` certificate, and the gateway must present its `agentry-gateway-tls` client cert with a SAN matching the gateway Service DNS — any other CA-signed cert is rejected. Both certificates are issued by the `agentry-ca-issuer` `ClusterIssuer` (see [Deployment Model](#deployment-model)) and rotated by cert-manager. See [SECURITY.md § Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api). The activator handler is served on **every** controller replica, not only the leader: the handler patches `agentry.io/wake=true` on the target Agent, and the leader's existing Agent watch fires the manual-wake path in the reconciler. This keeps the Service round-robin behavior correct without any leader-aware endpoint plumbing. The gateway uses this Service to send wake requests when a channel message arrives for a hibernated agent. The activator returns 202 Accepted as soon as the wake annotation patch is committed; the gateway observes wake completion by polling the agent's Service for readiness, not by waiting on the activator response (see [GATEWAY_USER.md § Activator](./GATEWAY_USER.md#activator) steps 3–4).

The reverse direction — controller → gateway — is the **activity API** (`GET /v1/activity?namespace={ns}`), used by the AgentReconciler to read per-namespace last-activity timestamps for idle and hibernation transitions. It is served on the gateway's `:8443` LLM listener (**not** the User listener on `:8080`, so an Ingress fronting `:8080` cannot route untrusted traffic to it). The handshake uses `tls.VerifyClientCertIfGiven` so token-auth callers on adjacent paths can complete the handshake without a client cert; per-path HTTP middleware then enforces an mTLS-with-SAN check on `/v1/activity` — the controller presents `agentry-controller-tls`, and only client certs whose SAN matches the controller Service DNS are admitted (Agent/AgentTask certs are rejected with 403). The controller dials each gateway Pod IP directly rather than the Service, since activity timestamps are in-memory per replica — see [Multi-replica state](#the-agentry-gateway) below. See [GATEWAY_LLM.md § Per-path client-auth enforcement](./GATEWAY_LLM.md#per-path-client-auth-enforcement), [GATEWAY_USER.md § Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api), and [SECURITY.md § Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api).

Leader election is enabled so the operator can run with multiple replicas for availability.

The controller's RBAC surface (cluster-scoped CRD watches, internal-endpoint authentication) is documented in [SECURITY.md § Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator--activity-api).

## Data Plane

The data plane is what actually runs when an Agent is created. For each Agent in `Running` state, the controller provisions:

- **One Pod** containing the user's agent container. The Pod runs under the RuntimeClass specified by its AgentClass (runc, gVisor, or Kata).
- **One PVC** if the Agent spec requests persistence, mounted into the agent container at a configured path.
- **One Service** (ClusterIP) exposing the agent's HTTPS endpoint for intra-cluster traffic. The gateway uses this Service to deliver channel messages over TLS; direct external exposure remains the developer's responsibility.
- **One cert-manager `Certificate`** (and the Secret it writes) containing a per-agent TLS certificate and key (`usages: server auth, client auth` — the same cert serves the agent's HTTPS listener and is presented as a client cert on every agent→gateway call), signed by the `agentry-ca-issuer` `ClusterIssuer` (a `ClusterIssuer` is required because `Certificate` resources in user namespaces cannot reference a namespaced `Issuer` in `agentry-system` — see [Deployment Model](#deployment-model)). Mounted into the Pod for the agent's HTTPS listener. cert-manager handles rotation continuously; the Agentry CA bundle is projected into Pods at `/var/run/agentry/ca.crt` from the `agentry-ca` ConfigMap maintained by trust-manager, and kubelet refreshes the volume when the ConfigMap changes.
- **One ConfigMap** holding non-sensitive agent configuration (gateway endpoint, feature flags).
- **One NetworkPolicy** synthesized from the AgentClass network policy and the gateway's egress allow rule. This is the load-bearing primitive cited in the gateway architecture analysis ([GATEWAY_LLM.md § Architecture Option Analysis](./GATEWAY_LLM.md#architecture-option-analysis)) for keeping LLM credentials inside `agentry-system` — see [CONTROLLER_RECONCILERS.md § AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) step 6 for the full rule set. **NetworkPolicy enforcement by the cluster CNI is a required prerequisite of Agentry's trust model** — gateway → agent delivery is one-way TLS (the agent does not verify a client cert from the gateway), so the synthesized ingress rule is the sole access control on `/v1/message`. Likewise the synthesized egress rule is the sole control preventing agents from calling provider IPs directly. Clusters running default kindnet or default flannel do not enforce NetworkPolicy and are not supported deployment targets. See [SECURITY.md § In-cluster TLS](./SECURITY.md#in-cluster-tls-bidirectional) and [Recommendation #4](./SECURITY.md#recommendations-for-deployment).

There is no sidecar container. The **Agentry Gateway** in `agentry-system` handles all LLM traffic and inbound channel messages as a shared cluster-level service.

For AgentTask, the data plane is the same minus the Service (tasks do not typically receive channel messages); when `completion.condition: agentReported`, the reconciler also pre-creates a `{taskName}-completion` ConfigMap that the gateway updates with the completion payload (status plus declared artifact values). The `exitCode` mode produces no artifacts — `spec.artifacts` is rejected by CRD CEL when this mode is selected.

## The Agentry Gateway

The gateway is a replicated Deployment in `agentry-system` that serves two distinct roles on separate listeners:

**LLM Gateway** (outbound, agent → provider)
- Serves TLS on port 8443; agent containers connect via `$AGENTRY_GATEWAY_ENDPOINT` (HTTPS, always injected)
- Identifies the source namespace via mTLS client-cert SAN (Agentry-managed Agent/AgentTask Pods) or `TokenReview`-validated ServiceAccount bearer token (gateway-only tier), with a source-IP → Pod cross-check from the informer cache as defense in depth — see [Namespace Identification](./GATEWAY_LLM.md#namespace-identification)
- Resolves the target ModelProvider from the qualified `provider/model` name in the request and the Agent's `spec.providers`
- Detects the request format from the URL path (`/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible)
- Validates the requested model and checks namespace access
- Enforces soft budget guardrails and per-namespace rate limits
- Routes to the upstream provider; on failure, walks the fallback chain (same-type providers only, up to `maxFallbackDepth` depth; no cross-format translation)
- Extracts actual token usage from the provider response and updates spend counters
- Returns structured error responses (JSON with `error.type`) on failure — see [LLM Gateway Error Responses](./API_ENDPOINTS.md#llm-gateway-error-responses)

**User Gateway** (inbound, channel → agent)
- Watches `AgentChannel` resources directly to determine message routing
- Listens for inbound webhook events on port 8080 over TLS (serves `agentry-gateway-tls`; Ingress is configured for backend re-encrypt or TLS pass-through)
- Normalizes webhook payloads into the standard Agentry message envelope
- Looks up the AgentChannel resource to find the target Agent and its endpoint
- If the agent is `Hibernated`, the gateway signals the controller to wake it via the mTLS-authenticated activator endpoint and waits until the Pod is ready (bounded by `wakeTimeout`)
- Delivers the message to the agent container via `POST /v1/message`
- Supports both synchronous and asynchronous response modes per AgentChannel: sync (default) returns the agent's response as the webhook HTTP response; async returns 202 Accepted immediately and delivers the response via callback URL or polling endpoint
- v1 supports webhook channels only; Discord and WhatsApp adapters are planned for v1.1

LLM provider credentials are stored as Secrets in `agentry-system` and read directly by the gateway. They never leave `agentry-system` namespace.

The gateway's RBAC surface — including cluster-scoped `create` on `tokenreviews` for the gateway-only tier and the Pod/AgentChannel watches for routing — is documented in [SECURITY.md § Gateway ServiceAccount permissions](./SECURITY.md#gateway-serviceaccount-permissions).

**Multi-replica state.** The gateway runs as multiple replicas, and each piece of per-namespace state has a defined cross-replica reconciliation path rather than living per-replica only:

- **Spend counters**: each replica server-side-applies its partials to the `agentry-budget-{providerName}` ConfigMap in `agentry-system` (keyed by Pod name); the ModelProviderReconciler sums partials, prunes stale-replica entries, and writes a `_canonical` total that replicas re-initialize from on startup. Bounded overspend is accepted as a soft-guardrail trade-off. See [GATEWAY_LLM.md § Budget State Management](./GATEWAY_LLM.md#budget-state-management) and [CONTROLLER_RECONCILERS.md § ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler) step 3.
- **Rate-limit token buckets**: each replica divides the configured cluster-wide ceiling by the live replica count from its Pod informer and adjusts on the next refill cycle when replicas scale. See [GATEWAY_LLM.md § Rate Limiting](./GATEWAY_LLM.md#rate-limiting).
- **Activity timestamps**: kept in-memory per replica (no etcd writes per request); the AgentReconciler fans out to every gateway Pod IP for the namespace, takes the most-recent timestamp per agent, and caches the result in a short reconciler-local window to bound query load. See [CONTROLLER_RECONCILERS.md § AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) step 8 and [GATEWAY_USER.md § Activity Tracking API](./GATEWAY_USER.md#activity-tracking-api).

## Agent Runtime Contract

Agentry is BYO-image: any container can be an Agent provided it implements a small contract — an HTTPS health endpoint on `$AGENTRY_HEALTH_PORT`, graceful SIGTERM handling, authenticated calls to `$AGENTRY_GATEWAY_ENDPOINT` (mTLS for Agentry-managed Pods, `TokenReview`-validated SA token for the gateway-only tier), an optional `POST /v1/message` handler when an AgentChannel is in use, and `messageId`-based deduplication when hibernation is enabled.

The full contract — required env vars, TLS reload semantics, dedup buffer, optional heartbeat and task-completion endpoints — is specified in [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md). Working implementations of the contract ship as Go and Python starter templates — see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md).

## Integration Points

### Agent Sandbox (optional backend — v1.1)

An `AgentClass` will be able to specify `spec.runtime.backend: agentSandbox` (v1.1). When set, the Agent Reconciler will create `Sandbox` custom resources (from the SIG Apps Agent Sandbox project) instead of raw Pods. This will give Agentry agents access to Agent Sandbox's warm pools and enhanced isolation without reimplementing those features. In v1, the only supported backend is `pod` — the CRD schema rejects `agentSandbox` at apply time.

### MCP (Model Context Protocol)

Agentry does not mandate MCP but is compatible with it. Agent containers are free to connect to MCP servers for tool access. AgentClass may declare a list of `allowedMCPServers` that the agent container can reach via NetworkPolicy constraints. MCP server provisioning itself is out of scope for v1.

### LLM Providers

Agentry supports any HTTP-based LLM provider. Out of the box, the gateway understands Anthropic, OpenAI, Google Vertex, and OpenAI-compatible endpoints (including Ollama, vLLM, LiteLLM gateways). Adding a new provider type is a plugin-style extension in the gateway.

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

## Deployment Model

Agentry ships as a Helm chart that installs:

- The CRDs (AgentClass, ModelProvider, Agent, AgentTask, AgentChannel).
- The operator Deployment with RBAC, ServiceAccount, and leader election.
- The Agentry Gateway Deployment with its RBAC, ServiceAccount, default `replicas: 2` (overridable via the `gateway.replicas` Helm value), PodDisruptionBudget (`minAvailable: 1`), and `maxUnavailable: 1` rolling update strategy. Two replicas is the floor: at one replica `minAvailable: 1` blocks all voluntary eviction (node drains stall), and the multi-replica state model in §The Agentry Gateway (spend ConfigMap exchange, divide-by-replicas rate buckets, controller activity fan-out) assumes ≥2 replicas. The gateway's operational settings are exposed as Helm values — notably `gateway.maxFallbackDepth` (default: `3`), which sets the `AGENTRY_MAX_FALLBACK_DEPTH` environment variable on the gateway Deployment and controls the maximum fallback chain depth for LLM provider routing (see [Fallback Logic](./GATEWAY_LLM.md#fallback-logic)). Three additional optional values govern network reachability:
  - `gateway.callbackUrl.allowlist` — list of DNS-name suffixes or CIDR blocks. When set, **replaces** the default deny-internal rule for `AgentChannel.spec.webhook.callbackUrl` with an explicit allowlist; the AgentChannelReconciler and the gateway's delivery-time re-check admit only hosts that match one of the configured entries. Leaving it unset preserves the default: `https://` only, with loopback, link-local, RFC1918, unique-local IPv6, and cloud-metadata IPs denied. See [API_RESOURCES.md § Cross-Resource Validation rule 22](./API_RESOURCES.md#cross-resource-validation).
  - `controller.networkPolicy.dnsSelector` — object of the shape `{ namespaceLabels: {...}, podLabels: {...} }` used as the `namespaceSelector` and `podSelector` for the DNS egress rule on every synthesized per-agent NetworkPolicy. Defaults to `{ namespaceLabels: { "kubernetes.io/metadata.name": "kube-system" }, podLabels: { "k8s-app": "kube-dns" } }`, which matches kubeadm, EKS, GKE, AKS, and the upstream CoreDNS chart. Override for clusters that run DNS in a non-standard namespace or with custom labels — see [SECURITY.md § Protecting agent containers from LLM provider access](./SECURITY.md#protecting-agent-containers-from-llm-provider-access).
  - `gateway.externalHostnames` — list of additional DNS names appended to the `agentry-gateway-tls` Certificate's SAN list. Required when the User Gateway is exposed via TLS pass-through Ingress so that external clients see a cert whose SAN matches the public hostname they dialed. Unset by default — backend re-encrypt Ingress works without it because the Ingress controller dials the in-cluster Service DNS, which is already in the default SAN set. See [GATEWAY_USER.md § TLS and Ingress](./GATEWAY_USER.md#tls-and-ingress).
- A single default AgentClass (`standard`, runc) that platform teams can customize or delete. A `sandboxed` example manifest (gVisor `RuntimeClass`) ships in the chart's `examples/` directory; operators apply it after confirming the matching `RuntimeClass` is installed on the cluster, since shipping it as a live default would put any Agent that selected it into `Degraded` on clusters without gVisor.
- Optional: a sample ModelProvider manifest stub (keys not included) as a starting template.

**cert-manager and trust-manager are required dependencies.** The chart does not install the cert-manager or trust-manager controllers themselves (teams with an existing cert-manager deployment reuse them); it ships the `ClusterIssuer`, `Certificate`, and `Bundle` resources Agentry needs:
- A self-signed `ClusterIssuer` (`agentry-selfsigned`) creates a `Certificate` for the Agentry CA.
- A `ClusterIssuer` (`agentry-ca-issuer`) sourcing from the `agentry-ca` Secret in `agentry-system` signs all Agentry-issued leaf certs using the CA above. A `ClusterIssuer` is used (rather than a namespace-scoped `Issuer`) because per-Agent/AgentTask `Certificate` resources live in user namespaces and reference the same signing key; cert-manager's `issuerRef` cannot span namespaces to a namespaced `Issuer`.
- A `Certificate` for the gateway serving cert (`agentry-gateway-tls`) used by both listeners — the LLM listener on port 8443 and the User listener on port 8080 — both serving TLS from the same cert. External webhook traffic arrives via Ingress configured for backend re-encrypt (or TLS pass-through); there is no plaintext listener on the gateway.
- A `Certificate` for the controller's activator endpoint (`agentry-controller-tls`).
- One `Certificate` per Agent, created by the AgentReconciler at provisioning time and owned by the Agent resource via ownerRef.
- One `Certificate` per AgentTask (`usages: client auth` only — tasks have no Service, so the cert is used for outbound mTLS to the gateway and not for serving), created by the AgentTaskReconciler at provisioning time and owned by the AgentTask resource via ownerRef.
- A `trust-manager` `Bundle` resource (`agentry-ca`) that projects the Agentry CA as a ConfigMap into every namespace selected by `target.namespaceSelector.matchExpressions: [{ key: kubernetes.io/metadata.name, operator: NotIn, values: [kube-system, kube-public, kube-node-lease] }]`. This default covers all user namespaces (including future ones added after install) without requiring the operator to label namespaces. Agent and AgentTask Pods mount this ConfigMap at `/var/run/agentry/ca.crt` to verify the gateway's TLS cert. Platform teams that need a tighter projection (specific namespaces only) can override the selector via the Helm value `trustManager.bundleSelector` (an object with `matchLabels` / `matchExpressions` keys passed verbatim into the `Bundle`'s `target.namespaceSelector`).

Admission webhooks are not used; the cert-manager dependency is solely for TLS lifecycle management.

**An NP-enforcing CNI is a required prerequisite** alongside cert-manager and trust-manager. See the NetworkPolicy bullet under [Data Plane](#data-plane) above and [SECURITY.md § Network Policy](./SECURITY.md#network-policy).

The Helm chart supports a tiered on-ramp:
1. **Gateway only**: install the chart, configure a ModelProvider, and point existing workloads at the gateway for LLM traffic and spend tracking. No AgentClass or Agent resources required. Existing workloads authenticate to the gateway using their own projected ServiceAccount tokens — no client certificate is required in this tier. See [Namespace Identification](./GATEWAY_LLM.md#namespace-identification). Existing workloads must mount the `agentry-ca` ConfigMap (projected by trust-manager into every non-system namespace — see the trust-manager `Bundle` description above) and configure their HTTP client to trust it; otherwise calls to `$AGENTRY_GATEWAY_ENDPOINT` fail TLS verification. Access control in this tier is governed by `ModelProvider.spec.allowedNamespaces` plus `spec.models` only — AgentClass `allowedProviders` does not apply because there is no Agent resource to reconcile against. Platform teams who need class-scoped provider policy must use the full lifecycle tier.
2. **Full agent lifecycle**: configure AgentClasses, deploy Agents and AgentTasks with hibernation and wake-on-demand, and connect them to user-facing channels via AgentChannels (webhook in v1). Channel integration is included in this tier because wake-on-demand requires a channel to be fully testable. Agentry-managed Pods authenticate via mTLS using per-agent certificates issued by cert-manager. The LLM Gateway enforces the full routing chain (Agent → AgentClass `allowedProviders` → ModelProvider `allowedNamespaces`/`models`) for this tier.
