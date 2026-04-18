# Agentry — Vision & Problem Statement

> **Note:** "Agentry" is a working codename. Replace throughout once a final name is selected.

## What is Agentry?

Agentry is a Kubernetes-native platform that makes AI agents a first-class workload type. It provides a set of custom resources and a controller that manage the full lifecycle of agents — from deployment and hibernation through resumption and teardown — alongside two managed gateway types: an **LLM Gateway** (TLS-secured) for controlled access to AI model providers, and a **User Gateway** for connecting agents to user-facing channels via webhooks (Discord, WhatsApp, and other platform-specific adapters are planned for v1.1).

Agentry is **not** an agent framework, an agent marketplace, or an IDE. It does not define how an agent thinks, which tools it uses, or how users talk to it at the application layer. It defines how an agent is **run**: what image, under what isolation policy, against which LLM providers, with what lifecycle, within what cost guardrails, and over what user-facing channels.

## The Problem

Deploying an AI agent to Kubernetes today is manual glue work. A team that wants to run an agent must assemble a Deployment or StatefulSet, a Service, a Secret for LLM API keys, a PVC for persistent memory, a custom proxy for token counting, and some mechanism for connecting the agent to the outside world (Slack, a webhook, a web client). They must decide independently how to handle idle agents (keep paying for them, or build their own hibernation), how to get visibility into LLM spend (usually they don't, and get surprised by the bill), and how to isolate agents that execute untrusted code.

This problem is most acute in **shared clusters with many agents**. Consider a platform team offering a self-service "personal AI assistant" capability to an engineering organization — hundreds of developers, each with their own persistent agent, each using a shared LLM provider. Without a platform-level abstraction, the platform team has no way to enforce usage policies, limit per-user spend, or provide a consistent channel integration. Every agent is a bespoke Helm chart with custom secrets management.

Platform teams face a structural tension: they want to offer agents as a self-service capability to developers, but they need to enforce security and cost guardrails centrally. Today there is no Kubernetes abstraction that captures "agent" as a workload with these concerns built in.

## What Agentry Provides

Agentry introduces five custom resources:

- **AgentClass** (cluster-scoped) — a policy resource, analogous to StorageClass, that defines the runtime configuration, isolation level, resource limits, and allowed providers for a category of agents. Platform teams own these.
- **ModelProvider** (cluster-scoped) — a managed abstraction over an LLM provider that holds API keys, provides visibility into token usage and spend, handles fallback, and can be shared across namespaces under policy control. Platform teams own these.
- **Agent** (namespace-scoped) — a developer-facing workload resource that describes a single agent: its image, its persistence needs, which AgentClass it belongs to, which ModelProviders it uses (if any), and its lifecycle mode (persistent or task). Developers own these.
- **AgentTask** (namespace-scoped) — a Job-like resource for ephemeral, goal-driven agents with a defined completion condition and artifact collection. Developers own these.
- **AgentChannel** (namespace-scoped) — a resource that connects a running Agent to a user-facing communication channel (Discord, WhatsApp, iMessage, a generic webhook, etc.). Developers own these.

The controller reconciles these resources into standard Kubernetes primitives — Pods, PVCs, Services, ConfigMaps — while layering in agent-aware lifecycle logic (idle detection, hibernation, wake-on-demand, task completion semantics) and managing two shared gateway components:

- The **LLM Gateway**: a replicated proxy Deployment in `agentry-system` that mediates all agent-to-provider traffic. It provides spend visibility, soft budget guardrails, rate limiting, fallback routing, and credential isolation. Using it is optional per agent.
- The **User Gateway**: a listener on the same gateway Deployment that receives inbound webhook messages, normalizes them into a standard envelope, and delivers them to the agent's HTTP endpoint. Discord, WhatsApp, and other platform-specific adapters are planned for v1.1.

## Budget Visibility and Guardrails

Agentry tracks LLM token usage and spend per namespace through the LLM Gateway. At each API call, the gateway checks the current budget state and enforces policies: degrading to a cheaper model as the budget ceiling is approached, and blocking requests when it is exceeded.

Budget enforcement in Agentry is **intentionally approximate**. The gateway maintains an in-process counter and updates it synchronously on each request, but in a multi-replica gateway deployment, counters are reconciled periodically rather than on every request. As a result, spend can exceed configured limits by a bounded amount under high concurrency near a budget threshold. This is the right tradeoff for most teams: hard enforcement at the cost of per-request aggregator synchronization adds latency that is rarely worth it.

Agentry's budget feature is best understood as **spend visibility and soft guardrails** rather than a hard financial cap. Teams that require hard caps should implement them at the provider level (Anthropic, OpenAI, and Vertex all support account-level spend limits).

## Landscape Positioning

Several projects overlap with parts of Agentry's scope. Agentry is designed to be additive to the ecosystem rather than replacing existing primitives.

**Agent Sandbox (SIG Apps)** is a lower-level primitive. It provides a Sandbox CRD for a single, stateful, isolated pod with features like pause/resume, warm pools, and optional gVisor/Kata isolation. Agent Sandbox is an excellent *runtime backend* for Agentry agents that need strong isolation — Agentry can create Sandbox resources rather than raw Pods when configured to do so. Agent Sandbox does not provide agent-level abstractions (no concept of a persistent vs. task agent, no ModelProvider management, no platform/developer split, no channel integration).

**KAgent (CNCF Sandbox)** is a more opinionated framework with a specific runtime (Python ADK or Go ADK), a built-in tool set oriented around DevOps/infrastructure agents, and per-agent `ModelConfig` resources. KAgent is a strong choice for teams wanting a batteries-included DevOps agent platform. Agentry is more general-purpose: any container satisfying a minimal runtime contract can be an Agent, and ModelProvider is a cluster-scoped shared resource with centralized budget control rather than a per-agent configuration.

**KubeClaw / Sympozium** focuses on fleet orchestration of agents that administer the cluster itself, with a sidecar-per-skill pattern and ephemeral RBAC. Its scope is narrower (cluster administration agents) and its architectural pattern (sidecar skills) is more opinionated.

Agentry's differentiator is the **generalized, policy-driven workload abstraction** for agents, with a clean two-tier platform/developer model, centralized provider management with spend visibility, and a native user-facing channel integration.

## Design Principles

1. **General-purpose over framework-specific.** Any container that satisfies the runtime contract can be an Agent. No assumption about language, framework, or agent architecture.
2. **Two-tier platform/developer model.** Cluster-scoped resources (AgentClass, ModelProvider) let platform teams set guardrails. Namespace-scoped resources (Agent, AgentTask, AgentChannel) let developers self-serve within those guardrails.
3. **Composable with the ecosystem.** Agent Sandbox can be used as a runtime backend. MCP can be used for tool integration. No reinvention of primitives that already exist.
4. **Opinionated defaults, BYO escape hatches.** A minimal runtime contract makes the simple case simple. Reference base images (Python and Go) are planned for a future release. Custom images are a first-class path.
5. **Policy at the boundary, not in the workload.** Budget guardrails, isolation policy, and provider access control live in cluster-scoped resources, not in individual Agent manifests.
6. **Kubernetes-native semantics.** Lifecycle mirrors familiar primitives: AgentClass is to Agent as StorageClass is to PVC; AgentTask is to Agent as Job is to Deployment.
7. **Honest about tradeoffs.** Where the system makes a tradeoff (soft budget limits, approximate enforcement under concurrency), this is documented explicitly rather than obscured.

## Scope for v1

**In scope:**
- All five CRDs and the reconciling controller
- Persistent and task-mode agent lifecycle (including idle detection, hibernation, wake-on-demand, timeout, artifact collection)
- LLM Gateway: TLS-secured cluster-level proxy with spend tracking, soft budget guardrails, rate limiting, same-type fallback chains (no cross-format translation), and provider credential isolation
- User Gateway: channel integration via AgentChannel (generic webhook in v1 with sync and async response modes; Discord and WhatsApp adapters in v1.1)
- RBAC, namespace scoping, and a documented security model
- Helm chart with tiered on-ramp (gateway-only → full agent lifecycle with channels)

**Out of scope for v1** (may land in later versions):
- Agent-to-agent communication and multi-agent orchestration
- Observability stack (traces, cost dashboards, audit export)
- A web UI or dashboard
- Multi-cluster federation
- Advanced scheduling (GPU-awareness, priority classes, preemption policies specific to agents)
- Hard budget enforcement (synchronous per-request aggregation)
- Cross-format provider fallback (e.g., Anthropic → OpenAI translation)
- Agent Sandbox integration (`agentSandbox` runtime backend) — v1.1
- Platform-specific channel adapters (Discord, WhatsApp) — v1.1

The v1 scope is deliberately narrow: get the workload abstraction, provider management, and channel integration right first. Everything else is an additive layer.
