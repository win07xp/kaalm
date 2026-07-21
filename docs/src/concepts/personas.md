# Personas and the Primary Scenario

Every design decision in Kaalm traces back to two people and one deployment. This page introduces them. If you understand who Priya and Dev are, what each of them cares about, and why a cluster full of personal agents (rather than a single production agent) is the workload Kaalm is built around, the rest of the design will read as a series of natural consequences.

## The primary scenario: a shared cluster for personal agents

The motivating deployment for Kaalm is a **shared Kubernetes cluster running hundreds of personal long-lived agents**, each owned by a different user.

Consider an engineering organization where every developer has their own persistent AI assistant. The platform team configures one [`AgentClass`](../resources/agentclass.md) (`personal-standard`), one [`ModelProvider`](../resources/modelprovider.md) with a per-namespace monthly budget, and provisions namespaces for each user. Developers deploy their [`Agent`](../resources/agent.md) and optionally connect it to their preferred channels via webhooks (with platform-specific adapters like Discord planned for v1.1). They write one manifest; they never touch RuntimeClasses, PodSecurityContexts, or API keys.

The platform team has full visibility into LLM spend per namespace. Idle agents hibernate automatically overnight and wake when the first message arrives. The platform can serve hundreds of these agents on a reasonably sized cluster because hibernated agents consume no compute.

This scenario, not an individual team's single production agent, is the central design driver for Kaalm's two-tier model, its [hibernation lifecycle](../controller/hibernation-and-wake.md#hibernation-mechanics), and its channel integration.

## Priya, the platform engineer

Priya runs the internal Kubernetes platform for a mid-sized engineering org. She supports hundreds of developers across many teams. She is responsible for cluster security, cost control, and the self-service experience of her platform. She does **not** write agents herself; she provisions the capability and hands it off.

Her concerns:

- LLM spend should be visible and bounded per user/team with clear guardrails.
- Agents that run untrusted LLM-generated code need strong isolation.
- She wants to offer a small number of well-defined agent configurations ("paved paths") rather than let every team invent their own.
- She needs to answer to security and finance about what's running and what it costs.

In Kaalm terms, Priya owns the cluster-scoped resources: she defines the agent classes and model providers that everyone else consumes. Her half of the two-tier model is the subject of [AgentClass](../resources/agentclass.md) and [ModelProvider](../resources/modelprovider.md).

## Dev, the application developer

Dev works on a product team. He wants to ship agents as part of his product (a customer support agent, an internal coding assistant, a ticket-triage bot) or simply have his own personal AI assistant accessible on his preferred channels. He knows Kubernetes at a `kubectl apply -f` level but doesn't want to learn RuntimeClasses, PodSecurityContexts, or PVC reclaim policies.

His concerns:

- Fast iteration: he wants to deploy, test, tear down, redeploy.
- His agent should be reachable via webhooks (and in the future via Discord, WhatsApp), and remember context across conversations.
- He wants to use the LLM providers his platform team has approved without managing API keys himself.
- For some use cases, he needs a one-shot agent that does a task and self-terminates.

Dev's half of the two-tier model is the namespaced resources: [Agent](../resources/agent.md) for the long-lived assistant, [AgentTask](../resources/agenttask.md) for the one-shot job, and [AgentChannel](../resources/agentchannel.md) for reaching it from the outside world. Everything Priya locked down (isolation, budgets, credentials) is invisible to him: he names a class, names a provider, and ships.

## Why this scenario drives the design

A single production agent would be easy: one team, one namespace, hand-tuned. Hundreds of personal agents, each owned by a different user, force the properties that define Kaalm:

- **Two tiers.** Priya's paved paths must be reusable by hundreds of developers who never see the details, so class and provider configuration is cluster-scoped and consumed by reference.
- **Hibernation.** Personal agents are idle most of the day. The cluster only stays reasonably sized if idle agents cost nothing.
- **Channels.** A personal assistant is only useful if its owner can reach it from wherever they already are, without each user building their own ingress.

The full set of acceptance scenarios that make these flows concrete (S1 through S15) lives in [the personas and use cases appendix](../appendix/scenarios.md).
