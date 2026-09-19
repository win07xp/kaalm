# System architecture

Kaalm has two long-running components, and everything else follows from how they divide the work:

- The **Kaalm Controller** watches custom resources and drives Kubernetes objects to match them. It never sits on a request path.
- The **Kaalm Gateway** sits on every request path. Agent traffic to LLM providers and tool servers goes through it, inbound channel messages arrive at it, and agents report back to it.

Both run in the `kaalm-system` namespace. Agent and AgentTask Pods run in user namespaces and hold no provider credentials of their own.

This page is the component map: how the pieces connect, what the Helm chart installs, what the controller does, and where Kaalm meets the ecosystem around it. Each mechanism it names is specified on the page it links to.

## System topology

The data plane is every path a request takes, and the gateway is on all of them.

![The data plane. Webhook callers and channel platforms reach the Kaalm Gateway on :8080. The gateway delivers to an Agent Pod with POST /v1/message over mTLS and posts async replies to callback URLs over HTTPS. Agent and AgentTask Pods call the gateway on :8443 with mTLS, and a gateway-only-tier Workload Pod calls the same port with a bearer token. The gateway forwards to LLM providers and MCP servers.](../diagrams/system-data-plane.svg)

In the figure, provider and tool-server egress leaves only from the gateway. For Kaalm-managed Pods that is enforced by the NetworkPolicy the controller writes; for gateway-only-tier Pods it holds only if the platform team writes one ([Adoption tiers](tenancy-and-tiers.md#adoption-tiers)).

The control plane is the controller, the API server, and the objects the controller materializes.

![The control plane. The controller's six reconcilers watch and reconcile through the Kubernetes API, which creates the child objects in user namespaces. The API server calls the controller's conversion webhook on :9444 for v1alpha1 objects. The gateway calls the controller's activator on :9443 over mTLS, and the reconcilers call the gateway's activity and channel-health endpoints on :8443 over mTLS.](../diagrams/system-control-plane.svg)

The child objects are the Pod, PVC, Service, ServiceAccount, NetworkPolicy, and Certificate per Agent, the same set less the Service per AgentTask, plus the per-channel and per-task Roles and RoleBindings and the task completion ConfigMap ([Child resources](../runtime/child-resources.md)).

### The internal endpoints

Seven endpoints are for Kaalm's own components. Every one requires a client certificate issued by the Kaalm CA whose SAN names the expected caller, and none accepts the bearer token the LLM proxy accepts, so a gateway-only workload can reach the proxy and nothing else.

| Endpoint | Listener | Caller |
|---|---|---|
| `POST /v1/agent/heartbeat`, `POST /v1/task/complete` | Gateway `:8443` | Agent and AgentTask Pods |
| `GET /v1/activity`, `GET /v1/channels/health` | Gateway `:8443` | Controller |
| `POST /v1/test-chat`, `GET /v1/spend` | Gateway `:8443` | Console |
| `POST /v1/activate/{namespace}/{agentName}` | Controller `:9443` | Gateway |

One per-path middleware admits the Agent and AgentTask SAN family on the two report paths and checks the kind against the path, so an Agent calling `/v1/task/complete` is rejected with `403` before the handler runs. The path-to-regime table is [The :8443 listener profile](../gateways/overview.md#the-8443-listener-profile); the SAN shapes are on [Workload identity](../gateways/llm/workload-identity.md); the endpoints themselves are on [Internal endpoints](../gateways/api/internal-endpoints.md) and [The activator](../gateways/user/activation-and-activity.md#the-activator).

## Deployment model

Kaalm ships as a Helm chart. cert-manager, trust-manager, and a CNI that enforces NetworkPolicy are required prerequisites, not optional add-ons ([Network policy](../security/model.md#network-policy)).

The chart deploys both the controller and the gateway in both [adoption tiers](tenancy-and-tiers.md#adoption-tiers). The install is the same; what differs is which custom resources the platform team creates, and therefore which reconcilers do work.

- **Gateway-only tier.** Needs only ModelProviders and their Secrets in `kaalm-system`. The Agent, AgentTask, and AgentChannel reconcilers idle, the AgentClassReconciler reconciles only the chart's default class, and no per-workload certificate is issued.
- **Full lifecycle tier.** Adds AgentClasses, Agents, AgentTasks, and AgentChannels, which exercise those reconcilers and per-Pod mTLS with cert-manager-issued certificates.

Both tiers depend on cert-manager for the gateway and controller serving certificates and on trust-manager for CA bundle projection, which is why the prerequisites are unconditional.

The chart deploys:

- The six CRDs introduced under [The custom resources](core-concepts.md#the-custom-resources).
- The controller and gateway Deployments, each at two replicas with a PodDisruptionBudget and a rolling-update strategy; the controller adds preferred pod anti-affinity. The chart refuses a replica count below two. The reasons differ per component and are on [The two Deployments](../operations/deployment.md#the-two-deployments).
- A ServiceAccount, ClusterRole, and ClusterRoleBinding per Deployment, plus namespaced Roles in `kaalm-system` for the grants that stay there ([RBAC and authentication](../security/rbac.md)).
- The cert-manager `ClusterIssuer`s (a self-signed root and the Kaalm CA issuer) and `Certificate`s for the gateway and controller serving certificates. Per-Agent and per-AgentTask certificates are issued at reconcile time, not by the chart.
- A trust-manager `Bundle` projecting the Kaalm CA into every namespace.
- One AgentClass, `standard`.

The issuers, the certificates, and the Bundle are the chart's half of the trust chain that lets every in-cluster component verify every other ([In-cluster TLS](../security/tls.md#in-cluster-tls)).

An optional third Deployment, the console, is off by default. It reads the Kubernetes API, reads spend from the gateway, and has one write-shaped action, test-chat. It sits outside the two-replica floor and changes nothing about the division of work ([Console overview](../console/overview.md)).

The full chart contents, the certificate inventory, the Helm values, and the per-tier setup are on [Deployment](../operations/deployment.md).

## Control plane

The controller is one Go binary built on `controller-runtime`, running as a Deployment in `kaalm-system`. It hosts six reconcilers, one per CRD, specified on [Reconcilers](../controller/reconcilers.md):

| Reconciler | Watches | Does |
|---|---|---|
| [Agent](../controller/reconcilers.md#agentreconciler) | Agent | Provisions the [child-resource set](../runtime/child-resources.md) and drives the [Agent lifecycle](../controller/agent-lifecycle.md): idle detection, hibernation, wake |
| [AgentTask](../controller/reconcilers.md#agenttaskreconciler) | AgentTask | Provisions the task's child set, watches the [completion condition](../controller/task-lifecycle.md), and settles the task |
| [ModelProvider](../controller/reconcilers.md#modelproviderreconciler) | ModelProvider | Validates the spec, resolves the credential, probes the upstream, folds spend into status |
| [ToolProvider](../controller/reconcilers.md#toolproviderreconciler) | ToolProvider | Resolves the optional credential and probes the server with MCP |
| [AgentClass](../controller/reconcilers.md#agentclassreconciler) | AgentClass | Validates references and egress entries and counts the workloads in use |
| [AgentChannel](../controller/reconcilers.md#agentchannelreconciler) | AgentChannel | Validates the channel and sets `Ready`, which gates routing; reduces the gateway's health observations into `PlatformConnected` |

Leader election is on, so the reconcilers run on one replica and the Deployment survives a replica loss. Three listeners run on every replica, leader or not: the activator on `:9443`, the CRD conversion webhook on `:9444`, and metrics on `:8080`; a plain-HTTP probe listener on `:8081` serves the kubelet ([The binary](../controller/overview.md#the-binary)).

### No admission webhooks

The controller hosts no validating or mutating webhook. Field-level checks are CRD schema and CEL, and cross-resource checks run at reconcile time and surface as status ([Validation and defaulting](../resources/validation-and-defaulting.md#cross-resource-validation)). The conversion webhook on `:9444` translates between API versions and enforces nothing ([No admission webhooks](../controller/overview.md#no-admission-webhooks)).

### The activator endpoint

The controller Service exposes `POST /v1/activate/{namespace}/{agentName}` on `:9443`, beside the conversion and metrics ports. The gateway calls it when a channel message arrives for a hibernated Agent. The listener admits only a client certificate whose SAN is the gateway Service DNS; the same listener answers `/healthz` and `/readyz` without a certificate, while the kubelet's probes target the separate `:8081` listener.

The handler runs on every replica, not only the leader, because it does no wake work itself: it patches a wake annotation on the Agent and answers `202 Accepted`, and the leader's Agent watch does the rest. The gateway then watches for the Agent's Service to accept connections rather than waiting on the controller ([The activator](../gateways/user/activation-and-activity.md#the-activator), [Activator handler](../controller/overview.md#activator-handler-served-on-every-replica)).

### The activity API

In the other direction, the AgentReconciler reads per-namespace last-activity timestamps from the gateway's `GET /v1/activity`, and the AgentChannelReconciler reads health observations from `GET /v1/channels/health`. Both are on the gateway's `:8443` listener, never the Ingress-fronted `:8080`, and admit only the controller's SAN. Because both stores are in memory per gateway replica, the controller dials every replica's Pod IP and merges; the Pod list comes from its `kaalm-system` Pod informer, which is why the controller holds a cluster-wide Pod watch ([Activity tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api), [Channel health tracking](../gateways/user/platform-adapters.md#channel-health-tracking)).

### RBAC surface

The controller's ServiceAccount holds cluster-wide watches on the six CRDs, cluster-wide management of the child object kinds including Pods and ConfigMaps, and the per-channel, per-task, and per-workload Roles and RoleBindings it writes in user namespaces; two namespaced Roles in `kaalm-system` carry Leases and Events, and the read of provider credential Secrets. The full grant list and its reasoning are on [Operator ServiceAccount](../security/rbac.md#operator-serviceaccount).

## Integration points

Kaalm stops at four boundaries rather than reimplementing what the ecosystem provides.

### Runtime isolation

An AgentClass selects the workload backend with [`spec.runtime.backend`](../resources/agentclass.md#spec). `pod` is the only supported value, and the schema rejects anything else at apply time. Isolation comes from `spec.runtime.runtimeClassName`, which names a RuntimeClass the cluster provides: gVisor, Kata, and Kata-fronted microVMs are all RuntimeClasses, so the operator reimplements none of them. A Sandbox-backed backend is on the [roadmap](../ROADMAP.md#next).

### MCP

Kaalm does not mandate MCP, but MCP is the tool protocol it brokers. The gateway's [tool plane](../gateways/tool-plane.md) carries tool traffic with credential injection, tenancy gates, and per-call audit. Direct egress is the supported alternative: an agent container may connect to an MCP server itself, and what Kaalm governs then is egress, through the class's [`network.egress.allowedCIDRs`](../resources/agentclass.md#spec). `allowedHosts` is validated and reported but synthesizes no policy (#193). Kaalm brokers access to tool servers; it does not run them.

### LLM providers

Any HTTP-based LLM provider works. Out of the box the gateway understands Anthropic, OpenAI, and OpenAI-compatible endpoints (Ollama, vLLM, and LiteLLM included); the `google-vertex` type is reserved in the enum and not served ([roadmap](../ROADMAP.md#beyond)). A new provider type is an adapter in the gateway ([Provider adapters](../gateways/llm/provider-routing.md#provider-adapters)).

### Channel platforms

The User Gateway ships three adapters: the generic webhook, Discord, and WhatsApp. All three are inbound HTTP; nothing holds a persistent connection. A platform adapter follows the same pattern as a provider adapter ([Platform adapters](../gateways/user/platform-adapters.md)).
