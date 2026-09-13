# Vision and scope

## What is Kaalm?

Kaalm is a Kubernetes-native platform that makes AI agents a first-class workload type. It provides a set of custom resources and a controller that manage the full lifecycle of agents, from deployment and hibernation through resumption and teardown, alongside one managed gateway Deployment with two call surfaces: an **LLM Gateway** for controlled access to AI model providers, and a **User Gateway** for connecting agents to user-facing channels: a generic webhook, Discord, and WhatsApp.

Kaalm is not an agent framework, an agent marketplace, or an IDE. It does not define how an agent thinks, which tools it uses, or how users talk to it at the application layer. It does govern how tool access is granted, brokered, and audited (the [tool plane](../gateways/tool-plane.md)), without ever shipping a tool itself. It defines how an agent is run: what image, under what isolation policy, against which LLM providers, with what lifecycle, within what cost guardrails, and over what user-facing channels.

## The problem

Without a platform abstraction, deploying an AI agent to Kubernetes is manual glue work. A team that wants to run an agent must assemble a Deployment or StatefulSet, a Service, a Secret for LLM API keys, a PVC for persistent memory, a custom proxy for token counting, and some mechanism for connecting the agent to the outside world. They must decide independently how to handle idle agents (keep paying for them, or build their own hibernation), how to get visibility into LLM spend, and often discover the cost only on the invoice, and how to isolate agents that execute untrusted code.

This problem is most acute in shared clusters with many agents. Consider a platform team offering a self-service "personal AI assistant" capability to an engineering organization: hundreds of developers, each with their own persistent agent, each using a shared LLM provider. Without a platform-level abstraction, the platform team has no way to enforce usage policies, limit per-user spend, or provide a consistent channel integration. Every agent is a bespoke Helm chart with custom secrets management.

Platform teams face a structural tension: they want to offer agents as a self-service capability to developers, but they need to enforce security and cost guardrails centrally. No core Kubernetes abstraction captures "agent" as a workload with these concerns built in.

## What Kaalm provides

Kaalm introduces six custom resources, in two tiers. Platform teams own the cluster-scoped policy resources: [AgentClass](../resources/agentclass.md), the runtime and isolation policy for a category of agents; [ModelProvider](../resources/modelprovider.md), an LLM provider with its credential, budgets, and fallback; and [ToolProvider](../resources/toolprovider.md), an MCP tool server with its credential. Developers own the namespaced workload resources: [Agent](../resources/agent.md), a long-running agent with its image, persistence, and hibernation settings; [AgentTask](../resources/agenttask.md), a Job-like one-shot agent with a completion condition and artifacts; and [AgentChannel](../resources/agentchannel.md), the connection from a running Agent to a user-facing channel. [Core concepts](core-concepts.md#the-custom-resources) introduces each one and draws how they reference each other.

The controller reconciles these resources into standard Kubernetes primitives (Pods, PVCs, Services, ServiceAccounts, NetworkPolicies, and cert-manager Certificates) while layering in agent-aware lifecycle logic: idle detection, hibernation, wake-on-demand, and task completion. It runs beside one shared gateway Deployment with two surfaces:

- The **LLM Gateway** mediates every agent-to-provider call and brokers tool calls. It provides spend visibility, budget guardrails (soft by default, with an opt-in hard cap), rate limiting, fallback routing, and credential isolation. It serves two tiers of caller, Kaalm-managed Pods and existing workloads with no Agent resource at all; the gateway-only tier routes through it only if the platform team's own egress policy says so ([Adoption tiers](tenancy-and-tiers.md#adoption-tiers)).
- The **User Gateway** receives inbound messages from a webhook caller or a channel platform, normalizes them into one envelope, and delivers them to the agent over mTLS ([User Gateway request flow](../gateways/user/overview.md#request-flow)).

## Budget visibility and guardrails

Kaalm tracks LLM token usage and spend per namespace through the LLM Gateway. At each call, the gateway checks the current budget state and enforces policies: degrading to a cheaper model as the ceiling approaches, and blocking requests when it is crossed.

Budget enforcement is approximate by default, on purpose. Each gateway replica counts in process and the replicas reconcile periodically rather than on every request, so spend can exceed a configured limit by a bounded amount under high concurrency near a threshold. That is the right tradeoff for most teams: continuous cross-replica synchronization adds latency and coordination cost that the accuracy gain rarely justifies. The mechanics and the bound are on [Budget state management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management).

The default mode is therefore spend visibility and soft guardrails. Providers whose invoice must not exceed a fixed number opt in to [hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement) per ModelProvider, which trades serialized admission near the ceiling for a stated spend guarantee. Provider-level account limits (Anthropic and OpenAI support account-level spend limits; on Vertex, GCP budgets provide alerts only, so a hard stop needs additional automation) remain sound defense in depth under either mode.

## Landscape positioning

Several projects overlap with parts of Kaalm's scope. Kaalm is additive to the ecosystem rather than a replacement for existing primitives. This section describes other people's software, so it ages faster than the rest of the book; it reflects the field as of August 2026, and the Agent Sandbox entry as of September 2026.

**Agent Sandbox (kubernetes-sigs, SIG Apps)** is a lower-level primitive: a `Sandbox` CRD (with `SandboxTemplate`, `SandboxClaim`, and `SandboxWarmPool`) for a single, stateful, isolated pod, with suspend and resume, warm pools, snapshot-based restore, and gVisor or Kata isolation. Its API reached 1.0 in August 2026, serving v1beta1 exclusively, and GKE ships a productized version. The Kubernetes project has stated that model access and budget controls are out of its scope, which is exactly the ground Kaalm's ModelProvider and ToolProvider occupy. Agent Sandbox is the natural runtime backend for Kaalm agents that need strong isolation. v1 stays on raw Pods and covers gVisor, Kata, and Kata-fronted microVMs through RuntimeClass; the [roadmap](../ROADMAP.md#next) carries the Sandbox backend. Two of its roadmap items, identity association and per-claim NetworkPolicy attachment, would overlap Kaalm's identity and egress mechanics at the pod layer; the policy semantics layered on them (which provider, whose budget, which tools) are not on its roadmap.

**kagent (CNCF Sandbox)** is a more opinionated agent framework: agents are declared as prompts plus tool references in an `Agent` CRD and executed by a built-in ADK-based engine, with per-agent `ModelConfig` resources and MCP server lifecycle tooling. It began as a DevOps-agent platform and is drifting general-purpose. Kaalm differs on both axes that matter here: any container satisfying a minimal [runtime contract](../runtime/contract.md) can be an Agent (no engine, no prompt schema), and model and tool access are cluster-scoped, budget-governed shared resources rather than per-agent configuration. kagent has no spend tracking, budget enforcement, hibernation, or task-completion semantics.

**KARS (Microsoft, released July 2026)** is the closest statement of Kaalm's thesis from a large vendor: an MIT-licensed "agent reference stack" for Kubernetes in which model, tool, memory, and MCP access are declared as policy CRDs and enforced by a router deployed alongside each agent pod, across several supported agent frameworks. The differences are structural: KARS is a reference stack assembled from components, enforces per-pod, and is oriented toward the Azure and Foundry ecosystem; Kaalm is a single small operator with a shared gateway, a two-tier resource model, and dollar-denominated budget enforcement, which KARS does not have.

**The gateway plane** projects govern traffic rather than workloads. agentgateway (Linux Foundation) is a data plane for LLM, MCP, and A2A traffic with credential injection and per-tool authorization on JWT claims; Envoy AI Gateway (CNCF ecosystem) provides CRD-native LLM routing with token-denominated quotas and MCP route filtering; LiteLLM is the widely deployed standalone proxy with dollar budgets in its own key-and-team tenancy model. All of these overlap Kaalm's gateway features, and none is an operator that runs the agent workload. Their budgets are token-denominated or keyed to their own tenancy rather than to Kubernetes namespaces, their grant subjects are keys or JWT claims rather than the workload's ServiceAccount or mTLS identity, and none can guarantee that the workload routes through them. For the workloads it manages, Kaalm can, because the same operator that runs the agent also writes its default-deny NetworkPolicy.

Kaalm models an agent as a long-lived Kubernetes workload: a pod with an identity, storage, and a lifecycle. Patterns that fan out very large numbers of sub-second agent invocations outgrow pod-per-agent economics, and purpose-built invocation fabrics (such as Google's Agent Substrate) exist for that regime. Kaalm's primary scenario, hundreds of long-lived agents that are idle most of the time, is the regime that pod-per-agent plus hibernation fits; swarm-scale invocation is out of scope.

Kaalm's differentiator is the combination none of its neighbors has: dollar-denominated budget enforcement per namespace, tool grants whose subject is the workload's Kubernetes identity, and egress policy written by the same operator that runs the workload, under a two-tier platform and developer model with native channel integration.

## Design principles

1. **General-purpose over framework-specific.** Any container that satisfies the [runtime contract](../runtime/contract.md) can be an Agent. No assumption about language, framework, or agent architecture.
2. **Two-tier platform and developer model.** Cluster-scoped resources (AgentClass, ModelProvider, ToolProvider) let platform teams set guardrails. Namespace-scoped resources (Agent, AgentTask, AgentChannel) let developers self-serve within them.
3. **Composable with the ecosystem.** Isolation comes from RuntimeClass (gVisor, Kata), not from a Kaalm-specific sandbox. MCP is the tool protocol, brokered by the gateway rather than reimplemented. No reinvention of primitives that already exist.
4. **Opinionated defaults, BYO alternatives.** A minimal runtime contract keeps the common case short. The [reference base images](../runtime/base-images.md) implement it so a first agent needs only a handler, the [starter templates](../runtime/starter-templates.md) are the next rung down, and a custom image is a first-class path.
5. **Policy at the boundary, not in the workload.** Budget guardrails, isolation policy, and provider access control live in cluster-scoped resources, not in individual Agent manifests.
6. **Kubernetes-native semantics.** Lifecycle mirrors familiar primitives: AgentClass is to Agent as StorageClass is to PVC; AgentTask is to Agent as Job is to Deployment.
7. **Tradeoffs are stated.** Where the system makes a tradeoff (soft budget limits by default, serialized admission near the ceiling under hard enforcement), each mode's exact bound is documented.

## Scope for v1

**In scope:**

- All six CRDs and the reconciling controller
- Agent and AgentTask lifecycle: idle detection, hibernation, wake-on-demand, timeout, artifact collection; see [Agent lifecycle](../controller/agent-lifecycle.md)
- LLM Gateway: TLS cluster-level proxy with spend tracking, budget guardrails (soft by default, [hard](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement) by opt-in per provider), rate limiting, fallback chains (crossing Anthropic and OpenAI formats), and provider credential isolation. Two authentication modes: mTLS for Kaalm-managed Pods and `TokenReview`-validated ServiceAccount tokens for existing workloads. See [Workload identity](../gateways/llm/workload-identity.md).
- Tool plane: gateway-brokered MCP access under ToolProvider grants; see [The tool plane](../gateways/tool-plane.md)
- User Gateway: channel integration through AgentChannel (generic webhook with sync and async response modes, Discord slash commands, WhatsApp Cloud API)
- RBAC, namespace scoping, and a documented [security model](../security/model.md)
- cert-manager-based TLS certificate lifecycle for the gateway and per-agent serving certs; see [Certificate lifecycle](../operations/deployment.md#certificate-lifecycle)
- Reference base images (Python and Go) and starter templates (one Go, one Python) that implement the runtime contract; see [Reference base images](../runtime/base-images.md) and [Starter templates](../runtime/starter-templates.md)
- Helm chart with a [tiered on-ramp](../operations/deployment.md#tiered-on-ramp), from gateway-only to the full agent lifecycle with channels
- Grafana dashboards and OpenTelemetry tracing; see [Dashboards](../operations/observability.md#dashboards) and [Tracing](../operations/observability.md#tracing)
- An optional operator console (default off): read-only fleet visibility plus one governed test-chat action; see [Console overview](../console/overview.md)

**Not in v1.** These are design-level deferrals; the [roadmap](../ROADMAP.md) says which of them are scheduled:

- Agent-to-agent communication and multi-agent orchestration
- Audit-log export
- Cost analytics and chargeback reporting
- Multi-cluster federation
- Advanced scheduling (GPU awareness, priority classes, preemption policies specific to agents)
- Serving the `google-vertex` provider type (the enum is reserved, no inbound path is routed) and any cross-format fallback involving it
- Agent Sandbox integration (an `agentSandbox` runtime backend) and a verified microVM isolation path; v1 covers gVisor and Kata through RuntimeClass
- The Discord Gateway WebSocket adapter for free-text message bots (a persistent connection per bot); the Discord adapter covers slash commands over HTTP
- Persona RBAC in the chart; the roles the [security model](../security/rbac.md#roles-for-people) specifies are written by the platform team (#244)

The v1 scope is narrow on purpose: get the workload abstraction, provider management, and channel integration right first. Everything else is an additive layer.

## Scoping summary

Every concern in the system has exactly one owner. This table is the quick reference for where each concern lives; the rest of the book expands on each row.

| Concern | Where it lives |
|---|---|
| Policy (who can use what, at what cost) | AgentClass, ModelProvider, ToolProvider (cluster-scoped) |
| Workload definition | Agent, AgentTask (namespace-scoped) |
| Channel integration | AgentChannel (namespace-scoped) |
| Lifecycle orchestration | Kaalm Controller (cluster-level) |
| Runtime isolation | RuntimeClass, selected by the AgentClass |
| LLM traffic and spend tracking | LLM Gateway in kaalm-system (shared) |
| Channel message routing | User Gateway in kaalm-system (shared) |
| Tool access | MCP, brokered by the gateway (the [tool plane](../gateways/tool-plane.md)) |
| External exposure | Kubernetes Ingress or Gateway API (user-managed, not Kaalm) |
| Observability | Controller and Gateway Prometheus metrics; see [Observability](../operations/observability.md) |
| In-cluster TLS issuance | cert-manager and trust-manager (prerequisite); see [Deployment](../operations/deployment.md) |
| Network policy enforcement | A CNI that enforces NetworkPolicy (prerequisite); see [Network policy](../security/model.md#network-policy) |
