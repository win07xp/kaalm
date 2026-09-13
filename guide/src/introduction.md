# Introduction

Kaalm (Kubernetes AI/Agent Loop Manager) runs AI agents as a first-class workload type on Kubernetes: you describe
an agent in YAML, and the operator gives it a Pod, an identity, a channel to talk
through, and guardrails on what it may spend.

This guide is task-oriented. It does not explain how the system works inside;
that is the job of the companion design book, which documents every resource,
the security model, and the wire contracts in full. Pages here end with a short
"How this works" note naming the design-book chapters that cover them.

## The mental model, in six resources

Three are cluster-scoped and managed by the platform team:

- **AgentClass** is a policy template: which images may run, how much storage an
  agent may claim, what lifecycle limits apply. Think of it as a runtime class
  for agents.
- **ModelProvider** is a managed LLM provider: the credential (kept out of team
  namespaces), the model catalog with prices, which namespaces may use it, and
  the budget attached to that use.
- **ToolProvider** is a managed reference to an external MCP tool server,
  the same idea applied to tools: the credential stays out of team
  namespaces, an optional declared catalog bounds what can be called, and an
  allowlist decides which namespaces may use it.

Three are namespaced and managed by application teams:

- **Agent** is a long-lived, stateful agent: it gets a Pod, optional persistent
  storage, and hibernation when idle.
- **AgentTask** is a run-to-completion agent: it does one job, reports a result,
  and is cleaned up.
- **AgentChannel** connects an Agent to the outside world: an inbound
  webhook, a Discord slash command, or a WhatsApp business number.

An agent's LLM calls and brokered tool calls go through the Kaalm gateway,
which injects the credential server-side, so API keys never appear in an
agent's namespace, container, or environment. A class can also permit direct
egress for the traffic that does not go through the gateway.

## Which chapters are for you

- **Platform engineer** (you install Kaalm and offer classes and providers to
  teams): read Getting started, then For platform teams. Observing the
  platform comes next, when you want the fleet on a
  screen, dashboards, and traces.
- **Agent developer** (someone already runs Kaalm for you; you deploy agents):
  skim the mental model in the preceding section, then start at [Your first agent](developers/first-agent.md).

## What you need before starting

- A Kubernetes cluster (v1.28 or newer) with a CNI that enforces NetworkPolicy.
- cert-manager and trust-manager (the [Installation](getting-started/installation.md)
  page covers the required flags).
- An API key for at least one LLM provider.
