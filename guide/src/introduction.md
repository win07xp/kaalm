# Introduction

Agentry runs AI agents as a first-class workload type on Kubernetes: you describe
an agent in YAML, and the operator gives it a Pod, an identity, a channel to talk
through, and guardrails on what it may spend.

This guide is task-oriented: it gets you from an empty cluster to a running,
reachable agent, then covers the day-to-day operations around it. It deliberately
does not explain how the system works inside. That is the job of the companion
design book, which documents every resource, the security model, and the wire
contracts in full. When a page here touches a concept the design book owns, it
links there.

## Who this guide is for

- A platform engineer installing Agentry and offering agent classes and model
  providers to teams.
- A developer deploying an agent, connecting it to a webhook, or running a
  one-shot task.

## What you need before starting

- A Kubernetes cluster (v1.30 or newer) with a NetworkPolicy-enforcing CNI.
- cert-manager and trust-manager installed (the [Installation](getting-started/installation.md)
  page covers this).
- An API key for at least one LLM provider.
