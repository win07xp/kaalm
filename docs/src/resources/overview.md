# Resource Overview

This part of the book covers the custom resources Kaalm provides: their spec and status schemas, and the rationale for the design choices behind them. There is one page per CRD ([AgentClass](agentclass.md), [ModelProvider](modelprovider.md), [Agent](agent.md), [AgentTask](agenttask.md), [AgentChannel](agentchannel.md)), followed by the [cross-resource validation rules](validation-and-defaulting.md#cross-resource-validation) and [defaulting behavior](validation-and-defaulting.md#defaulting) that tie them together. The specs on these pages are the canonical field reference for implementation.

For the HTTP endpoints that agent containers call (task completion, heartbeat, message delivery, async webhook), see [HTTP API](../gateways/api/overview.md).

All resources live in one API group and version:

- API group: `kaalm.io`
- API version: `v1alpha1` (v1 API stability is not a goal for the initial release)

## Resource Summary

| Kind | Scope | Owner | Purpose |
|---|---|---|---|
| `AgentClass` | Cluster | Platform | Runtime policy template for a category of agents |
| `ModelProvider` | Cluster | Platform | Managed LLM provider with spend tracking and access controls |
| `Agent` | Namespace | Developer | A persistent agent workload |
| `AgentTask` | Namespace | Developer | An ephemeral, goal-driven agent workload |
| `AgentChannel` | Namespace | Developer | A connection between a running Agent and a user-facing channel |

The Owner column reflects the intended split of responsibility: platform teams manage the cluster-scoped policy resources (AgentClass, ModelProvider), while developers create the namespaced workload resources (Agent, AgentTask, AgentChannel) that reference them.

For how these five resources reference each other, including which spec field carries each reference, see the [CRD reference graph](../concepts/core-concepts.md#the-five-custom-resources).
