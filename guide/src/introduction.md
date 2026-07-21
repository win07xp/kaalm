# Introduction

Agentry runs AI agents as a first-class workload type on Kubernetes: you describe
an agent in YAML, and the operator gives it a Pod, an identity, a channel to talk
through, and guardrails on what it may spend.

This guide is task-oriented. It does not explain how the system works inside;
that is the job of the companion design book, which documents every resource,
the security model, and the wire contracts in full. Pages here end with a short
"How this works" block linking into the design book for the curious.

## The mental model, in five resources

Two are cluster-scoped and owned by the platform team:

- **AgentClass** is a policy template: which images may run, how much storage an
  agent may claim, what lifecycle limits apply. Think of it as a runtime class
  for agents.
- **ModelProvider** is a managed LLM provider: the credential (kept out of team
  namespaces), the model catalog with prices, which namespaces may use it, and
  the budget attached to that use.

Three are namespaced and owned by application teams:

- **Agent** is a long-lived, stateful agent: it gets a Pod, optional persistent
  storage, and hibernation when idle.
- **AgentTask** is a run-to-completion agent: it does one job, reports a result,
  and is cleaned up.
- **AgentChannel** connects an Agent to the outside world through an inbound
  webhook.

Every LLM call an agent makes goes through the Agentry gateway, which injects
the provider credential server-side, so API keys never appear in an agent's
namespace, container, or environment.

## Which chapters are for you

- **Platform engineer** (you install Agentry and offer classes and providers to
  teams): read Getting Started, then the For Platform Teams part.
- **Agent developer** (someone already runs Agentry for you; you deploy agents):
  skim the mental model above, then start at [Your First Agent](developers/first-agent.md).

## What you need before starting

- A Kubernetes cluster (v1.30 or newer) with a NetworkPolicy-enforcing CNI.
- cert-manager and trust-manager (the [Installation](getting-started/installation.md)
  page covers the required flags).
- An API key for at least one LLM provider.
