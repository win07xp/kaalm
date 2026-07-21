# System Architecture

Kaalm has two long-running components, and everything else follows from how they divide the work:

- The **Kaalm Controller** watches custom resources and drives Kubernetes objects to match them. It never sits on a request path.
- The **Kaalm Gateway** sits on every request path. Agent traffic to LLM providers goes through it, inbound webhooks arrive at it, and agents report back to it.

Both run in the `kaalm-system` namespace. Agent and AgentTask Pods run in user namespaces and hold no provider credentials of their own.

This page shows how those pieces connect, what the Helm chart installs, what the controller does, and where Kaalm meets the ecosystem around it.

## System Topology

![Kaalm system topology: webhook callers reach the Kaalm Gateway Pod in kaalm-system, which proxies LLM traffic from Agent, AgentTask, and Workload Pods in user namespaces out to LLM provider APIs and posts async responses to callback URLs. The Kaalm Controller reconciles against the Kubernetes API, which materializes per-workload objects in user namespaces, and controller and gateway exchange internal mTLS RPCs.](../diagrams/system-topology.svg)

### Reading the diagram

The edges come in three weights, and the weight tells you what kind of traffic it is.

**Thick edges are the data plane.** External webhook callers reach the gateway on `:8080`. The gateway delivers messages to an Agent Pod over mTLS with `POST /v1/message`, and posts async replies out to `callbackUrl` over HTTPS. In the other direction, Agent and AgentTask Pods send LLM traffic to the gateway on `:8443` authenticated by mTLS, while gateway-only-tier Workload Pods send the same traffic to the same port authenticated by a ServiceAccount bearer token. Only the gateway egresses to the provider APIs.

**Thin edges are lifecycle.** The controller watches and reconciles against the Kubernetes API, and the API server is what actually materializes the per-workload objects in user namespaces: Pods, PVCs, Services, ConfigMaps, NetworkPolicies, ServiceAccounts, and Certificates.

**Dashed edges are internal RPCs**, covered next.

### The internal RPCs

The dashed edges are internal mTLS RPCs. Every one of them requires mTLS-with-SAN, enforced by per-path middleware on the listener, and every one of them rejects the SA-bearer alternative that the LLM proxy accepts. That last point is the important one: a gateway-only workload holding a valid ServiceAccount token can reach the LLM proxy, but it cannot reach any of these paths, because bearer tokens are not accepted there at all.

"mTLS-with-SAN" means the caller must present a client certificate issued by the Kaalm CA *and* the certificate's SAN must identify the expected caller. The certificate proves the caller is part of the system; the SAN proves *which* part. For the SAN shapes, how the gateway maps them to a namespace and workload, and the SA-bearer mode they exclude, see [Namespace identification](../gateways/llm/workload-identity.md).

The five RPCs split across two listeners:

| RPC | Listener | Caller |
|---|---|---|
| `GET /v1/activity` | Gateway `:8443` | Controller |
| `GET /v1/channels/health` | Gateway `:8443` | Controller |
| `POST /v1/task/complete` | Gateway `:8443` | AgentTask Pod |
| `POST /v1/agent/heartbeat` | Gateway `:8443` | Agent Pod |
| `POST /v1/activate` | Controller `:9443` | Gateway |

Four are served on the gateway's `:8443` listener; the activator wake is the odd one out, served on the controller's `:9443`.

The `:8443` paths share one listener, so they share one admission step, and the SAN policy differs per path. Note that `/v1/task/complete` and `/v1/agent/heartbeat` both admit the Agent/AgentTask SAN family at the listener and then split by caller type at the handler, so an Agent calling `/v1/task/complete` gets past admission and is rejected in the handler. For the consolidated path to SAN mapping on `:8443`, including that handler-level split layered on the shared admission, see [The Kaalm Gateway](../gateways/overview.md). For the activator's SAN policy, see [Control Plane](#control-plane) below. The underlying [per-path middleware](../gateways/llm/listener-tls.md#per-path-client-auth-enforcement) pattern and the reasoning behind it are in [Internal Endpoint Authentication](../security/rbac.md#internal-endpoint-authentication).

## Deployment Model

Kaalm ships as a Helm chart. **cert-manager, trust-manager, and an NP-enforcing CNI are required prerequisites**, not optional add-ons: see [Network Policy](../security/model.md#network-policy) and the NetworkPolicy bullet under [Per-Agent and Per-Task Child Resources](../runtime/child-resources.md).

The chart deploys both the controller and the gateway in both [Adoption Tiers](tenancy-and-tiers.md#adoption-tiers). The install is the same; what differs is which custom resources the platform team creates, and therefore which reconcilers are operationally exercised.

- **Gateway-only tier.** Requires only `ModelProvider`s and provider Secrets in `kaalm-system`. The Agent, AgentTask, and AgentChannel reconcilers idle with no resources to reconcile; the AgentClassReconciler reconciles only the chart-shipped default class; per-Agent and per-AgentTask `Certificate` issuance is never exercised. This tier's egress responsibility is stated under [Adoption Tiers](tenancy-and-tiers.md#adoption-tiers).
- **Full lifecycle tier.** Additionally creates AgentClasses, Agents, AgentTasks, and AgentChannels, exercising those reconcilers and per-Pod mTLS via cert-manager.

Both tiers depend on cert-manager (for the gateway and controller serving certs) and on trust-manager (for CA bundle projection into user namespaces). That is why the prerequisites are unconditional even though the tiers look very different in practice.

At a type level, the chart deploys:

- The five CRDs introduced under [Custom Resources](core-concepts.md#the-five-custom-resources) (AgentClass, ModelProvider, Agent, AgentTask, AgentChannel)
- Controller and Gateway Deployments. Both default to two replicas with a PodDisruptionBudget, rolling-update strategy, and pod anti-affinity. The chart enforces a **floor of 2 replicas** on both, so that the wake-on-demand "hard control-plane dependency" claim under [The Kaalm Gateway](../gateways/overview.md) survives voluntary disruptions and single-replica involuntary failures. See [Deployment](../operations/deployment.md) for the operational rationale and the chart-level enforcement.
- Per-Deployment `ServiceAccount`s, `ClusterRole`s, and `ClusterRoleBinding`s, plus companion namespaced `Role`s/`RoleBinding`s in `kaalm-system` for the grants that must not be cluster-wide (Leases, Secrets, ConfigMaps: see [RBAC Model](../security/rbac.md))
- cert-manager `ClusterIssuer`s (a self-signed root and the Kaalm CA issuer) and `Certificate`s for the gateway and controller serving certs. Per-Agent and per-AgentTask `Certificate`s are issued at reconcile time, not by the chart.
- A trust-manager `Bundle` projecting the Kaalm CA bundle into non-system namespaces
- A default `standard` AgentClass and an optional `sandboxed` AgentClass example

Those last three bullets are the chart's half of the trust chain that lets every in-cluster component verify every other; how the chain is rooted and how workloads consume it is in [In-cluster TLS](../security/tls.md#in-cluster-tls).

For the full chart contents, the certificate inventory, the operational Helm values, and the per-tier setup details, see [Deployment](../operations/deployment.md).

## Control Plane

The Kaalm control plane is a single operator (Go, built on `controller-runtime`) running as a Deployment in the `kaalm-system` namespace. It hosts five reconcilers, one per CRD:

1. [**Agent Reconciler**](../controller/reconcilers.md#agentreconciler) watches `Agent` resources. It provisions the [per-Agent child-resource set](../runtime/child-resources.md) and drives the [persistent-agent state machine](../controller/agent-lifecycle.md): idle detection, hibernation, wake-on-demand.

2. [**AgentTask Reconciler**](../controller/reconcilers.md#agenttaskreconciler) watches `AgentTask` resources. It provisions the [per-AgentTask child-resource set](../runtime/child-resources.md) to execute the task, monitors the [completion condition](../controller/task-lifecycle.md) (`agentReported` or `exitCode`), and tears down resources on completion or timeout.

3. [**ModelProvider Reconciler**](../controller/reconcilers.md#modelproviderreconciler) watches `ModelProvider` resources. It validates provider configuration, verifies the referenced Secret exists and is well-formed, maintains provider health status, and manages per-namespace spend state.

4. [**AgentClass Reconciler**](../controller/reconcilers.md#agentclassreconciler) watches `AgentClass` resources. It validates that referenced ModelProviders exist, maintains usage counts, and updates status conditions.

5. [**AgentChannel Reconciler**](../controller/reconcilers.md#agentchannelreconciler) watches `AgentChannel` resources. It validates that the referenced Agent exists with a Service enabled, and validates channel credentials and `callbackUrl` (per [validation rule 22](../resources/validation-and-defaulting.md#cross-resource-validation)). It sets `status.conditions[type=Ready]` from those validations, which is the gate the gateway uses to admit webhook traffic. It separately populates `status.conditions[type=PlatformConnected]`, which is observational only, by polling the gateway via [`GET /v1/channels/health`](../gateways/api/internal-endpoints.md#get-v1channelshealth).

On that last point, the two conditions are not interchangeable: the gateway gates webhook routing on `Ready` alone, while `PlatformConnected` exists for user and operator visibility. See [Channel Health Tracking](../gateways/user/platform-adapters.md#channel-health-tracking) for the rolling-window tri-state contract and the per-replica reduction rules.

### No admission webhooks

The controller does **not** host admission webhooks. Field-level validation uses CEL expressions in CRD schemas. [Cross-resource validation](../resources/validation-and-defaulting.md#cross-resource-validation), meaning reference resolution, image allowlists, and provider access, is handled at reconcile time and surfaced as status conditions rather than admission errors.

### The activator endpoint

The controller exposes an internal ClusterIP Service for the activator endpoint, `POST /v1/activate/{namespace}/{agentName}`, on port `:9443`. The same listener on each controller Pod also serves `/healthz` and `/readyz` for kubelet probes, which target the Pod directly rather than going through the Service.

The activator endpoint requires [**mTLS**](../security/rbac.md#internal-endpoint-authentication): the controller admits only client certificates whose SAN matches the gateway Service DNS. Both controller and gateway present TLS certs, one `Certificate` per Deployment, shared across replicas, with Service DNS in the SAN, issued by the Kaalm CA `ClusterIssuer` (see [Deployment Model](#deployment-model)) and rotated by cert-manager. Cert-less kubelet probes coexist with the mTLS-with-SAN gate on the same listener via the handshake mode documented in [Internal Endpoint Authentication](../security/rbac.md#internal-endpoint-authentication) and [Per-path client-auth enforcement](../gateways/llm/listener-tls.md#per-path-client-auth-enforcement).

The activator handler is served on **every** controller replica, not only the leader. This works because the handler does not do the wake itself: it patches a wake annotation on the target Agent, and the leader's existing Agent watch fires the manual-wake path in the reconciler. Any replica can write the annotation, so Service round-robin stays correct with no leader-aware endpoint plumbing.

The gateway uses this Service to send wake requests when a channel message arrives for a hibernated agent. The activator returns `202 Accepted` as soon as the wake annotation patch is committed. It does not wait for the Pod to come up, and neither does the caller: the gateway observes wake completion by polling the agent's Service for readiness, not by waiting on the [activator response](../gateways/user/activation-and-activity.md#the-activator) (steps 3-4).

### The activity API

The reverse direction, controller to gateway, is the [**activity API**](../gateways/user/activation-and-activity.md#activity-tracking-api), `GET /v1/activity?namespace={ns}`. The AgentReconciler uses it to read per-namespace last-activity timestamps for idle and hibernation transitions.

It is served on the gateway's `:8443` LLM listener, **not** the User listener on `:8080`, so that an Ingress fronting `:8080` cannot route untrusted traffic to it. Per-path middleware enforces mTLS-with-SAN on `/v1/activity`: only the controller's SAN is admitted, and Agent/AgentTask certs are rejected with `403`.

The controller dials each gateway Pod IP directly rather than the Service, because [activity timestamps are in-memory per replica](../gateways/overview.md) and a Service-routed request would reach only one of them. Replica IPs are enumerated from the controller's gateway-Pod informer over `kaalm-system`. That informer also backs the channel-health fan-out, and it is the operational reason for the cluster-wide Pod watch in the [RBAC surface](#rbac-surface) below. Dialing a Pod IP against a Service-DNS-scoped SAN needs a specific TLS-handshake detail: see [Activity Tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api). The same fan-out pattern is reused by channel-health, see [Channel Health Tracking](../gateways/user/platform-adapters.md#channel-health-tracking).

Leader election is enabled so the operator can run with multiple replicas for availability.

### RBAC surface

The controller's [RBAC surface](../security/rbac.md#operator-serviceaccount) covers:

- **Cluster-scoped CRD watches.**
- **Child-resource management**, including cluster-wide Pod read/list/watch. This is needed twice over: to manage Agent/AgentTask Pods in user namespaces, and to fan out activity and channel-health queries to gateway Pods in `kaalm-system`.
- **Scoped ConfigMap read/write/delete in `kaalm-system`**: the per-provider budget ConfigMap, plus the per-request async-response ConfigMaps, which the AgentChannelReconciler prunes on expiry and sweeps in its finalizer. Those ConfigMaps carry no ownerRef and are linked to their channel by labels instead, because a cross-namespace ownerReference would be invalid and would get them garbage-collected immediately: see [Async Webhook Response](../gateways/api/async-responses.md).
- **Dynamic per-channel and per-task `Role`/`RoleBinding`s in user namespaces.**

## Integration Points

Kaalm deliberately stops at four boundaries rather than reimplementing what the ecosystem already provides.

### Agent Sandbox (optional backend, v1.1)

An `AgentClass` will be able to specify [`spec.runtime.backend: agentSandbox`](../resources/agentclass.md#spec) (v1.1). When set, the Agent Reconciler will create `Sandbox` custom resources (from the SIG Apps Agent Sandbox project) instead of raw Pods. This will give Kaalm agents access to Agent Sandbox's warm pools and enhanced isolation without reimplementing those features.

In v1, the only supported backend is `pod`. The CRD schema rejects `agentSandbox` at apply time, so there is no silent fallback.

### MCP (Model Context Protocol)

Kaalm does not mandate MCP but is compatible with it. Agent containers are free to connect to MCP servers for tool access.

What Kaalm does govern is egress to those servers, via the AgentClass:

- [`network.egress.allowedCIDRs`](../resources/agentclass.md#spec) is portable and works on every NP-capable CNI.
- `network.egress.allowedHosts` is FQDN-based and works only on FQDN-policy CNIs, for example Cilium or Calico Enterprise.

See [AgentClass design notes](../resources/agentclass.md#design-notes). MCP server provisioning itself is out of scope for v1.

### LLM Providers

Kaalm supports any HTTP-based LLM provider. Out of the box, the gateway understands Anthropic, OpenAI, Google Vertex, and OpenAI-compatible endpoints (including Ollama, vLLM, and LiteLLM gateways). Adding a new provider type is a [plugin-style extension in the gateway](../gateways/llm/provider-routing.md#provider-adapters).

### Channel Platforms

The User Gateway ships with a **generic webhook adapter** in v1: inbound HTTP POST with configurable auth. Discord and WhatsApp adapters are planned for v1.1, deferred because they require persistent connections and platform-specific reconnection logic rather than a simple request/response intake. Additional platform adapters follow the same plugin pattern as LLM provider adapters.
