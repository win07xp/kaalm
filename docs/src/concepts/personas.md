# Personas and the primary scenario

Every design decision in Kaalm traces back to two people and one deployment. This page introduces them. If you understand who Priya and Dev are, what each of them cares about, and why a cluster full of personal agents (rather than a single production agent) is the workload Kaalm is built around, the rest of the design reads as a series of consequences.

## The primary scenario: a shared cluster for personal agents

The motivating deployment for Kaalm is a shared Kubernetes cluster running hundreds of personal long-lived agents, each belonging to a different user.

Consider an engineering organization where every developer has their own persistent AI assistant. The platform team configures one [AgentClass](../resources/agentclass.md) (`personal-standard`) that allows hibernation and persistence, one [ModelProvider](../resources/modelprovider.md) with a per-namespace monthly budget, and a namespace per user. Developers deploy their [Agent](../resources/agent.md) with hibernation enabled and optionally connect it to their preferred channels over webhooks, Discord, or WhatsApp. They write one manifest; they never touch RuntimeClasses, PodSecurityContexts, or API keys.

The platform team has full visibility into LLM spend per namespace. Idle agents hibernate overnight and wake when the first message arrives. The platform can serve hundreds of these agents on a moderately sized cluster because a hibernated agent consumes no compute.

This scenario, not an individual team's single production agent, is the central design driver for Kaalm's two-tier model, its [hibernation lifecycle](../controller/hibernation-and-wake.md#hibernation-mechanics), and its channel integration. As shipped, the chart's `standard` class sets the lifecycle timings but does not allow hibernation or persistence, so the class for this scenario is one the platform team writes (#243).

## Priya, the platform engineer

Priya runs the internal Kubernetes platform for a mid-sized engineering org. She supports hundreds of developers across many teams. She is responsible for cluster security, cost control, and the self-service experience of her platform. She does not write agents herself; she provisions the capability and hands it off.

Her concerns:

- LLM spend must be visible and bounded per user or team, with clear guardrails.
- Agents that run untrusted LLM-generated code need strong isolation.
- She wants to offer a small number of well-defined agent configurations ("paved paths") rather than let every team invent their own.
- She needs to answer to security and finance about what is running and what it costs.

In Kaalm terms, Priya creates the cluster-scoped resources: the agent classes, model providers, and tool providers everyone else consumes. Her half of the two-tier model is the subject of [AgentClass](../resources/agentclass.md), [ModelProvider](../resources/modelprovider.md), and [ToolProvider](../resources/toolprovider.md).

## Dev, the application developer

Dev works on a product team. He wants to ship agents as part of his product (a customer support agent, an internal coding assistant, a ticket-triage bot) or have his own personal AI assistant on his preferred channels. He knows Kubernetes at a `kubectl apply -f` level but does not want to learn RuntimeClasses, PodSecurityContexts, or PVC reclaim policies.

His concerns:

- Fast iteration: deploy, test, tear down, redeploy.
- His agent must be reachable over webhooks, Discord, or WhatsApp, and remember context across conversations.
- He wants to use the LLM providers his platform team has approved without managing API keys himself.
- For some use cases, he needs a one-shot agent that does a task and reports its result.

Dev's half of the two-tier model is the namespaced resources: [Agent](../resources/agent.md) for the long-lived assistant, [AgentTask](../resources/agenttask.md) for the one-shot job, and [AgentChannel](../resources/agentchannel.md) for reaching it from outside. What Priya set (isolation, budgets, credentials) is out of his reach by resource scope: credentials stay in `kaalm-system`, and the class and provider objects are cluster-scoped. Whether he can edit them is a question of the RBAC the platform team writes, which the chart does not ship ([Roles for people](../security/rbac.md#roles-for-people)). He names a class, names a provider, and ships.

## Why this scenario drives the design

A single production agent would need none of this: one team, one namespace, hand-tuned. Hundreds of personal agents, each belonging to a different user, force the properties that define Kaalm:

- **Two tiers.** Priya's paved paths must be reusable by hundreds of developers who never see the details, so class and provider configuration is cluster-scoped and consumed by reference. The split is drawn on [Core concepts](core-concepts.md#the-custom-resources), whose first figure labels the two scopes by who manages them.
- **Hibernation.** Personal agents are idle most of the day. The cluster stays moderately sized only if an idle agent costs nothing.
- **Channels.** A personal assistant is useful only if its owner can reach it from wherever they already are, without each user building their own ingress.

The acceptance scenarios that make these flows concrete (S1 to S24) are in [Acceptance scenarios](../appendix/scenarios.md).
