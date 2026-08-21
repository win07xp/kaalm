# Resource Overview

This part of the book covers the custom resources Kaalm provides: their spec and status schemas, and the rationale for the design choices behind them. There is one page per CRD ([AgentClass](agentclass.md), [ModelProvider](modelprovider.md), [ToolProvider](toolprovider.md), [Agent](agent.md), [AgentTask](agenttask.md), [AgentChannel](agentchannel.md)), followed by the [cross-resource validation rules](validation-and-defaulting.md#cross-resource-validation) and [defaulting behavior](validation-and-defaulting.md#defaulting) that tie them together. The specs on these pages are the canonical field reference for implementation.

For the HTTP endpoints that agent containers call (task completion, heartbeat, message delivery, async webhook), see [HTTP API](../gateways/api/overview.md).

All resources live in one API group, served at two versions since v0.6.0:

- API group: `kaalm.io`
- API versions: `v1beta1`, the storage version and the compatibility contract, and `v1alpha1`, deprecated, still served, and converted by the controller. The two schemas are identical, so the field reference on these pages applies to both. The versioning rules and the deprecation window are on [API Versioning and Deprecation](../operations/api-versioning.md).

## Resource Summary

| Kind | Scope | Owner | Purpose |
|---|---|---|---|
| `AgentClass` | Cluster | Platform | Runtime policy template for a category of agents |
| `ModelProvider` | Cluster | Platform | Managed LLM provider with spend tracking and access controls |
| `ToolProvider` | Cluster | Platform | Managed external tool server with brokered access (since v0.4.0) |
| `Agent` | Namespace | Developer | A persistent agent workload |
| `AgentTask` | Namespace | Developer | An ephemeral, goal-driven agent workload |
| `AgentChannel` | Namespace | Developer | A connection between a running Agent and a user-facing channel |

The Owner column reflects the intended split of responsibility: platform teams manage the cluster-scoped policy resources (AgentClass, ModelProvider, ToolProvider), while developers create the namespaced workload resources (Agent, AgentTask, AgentChannel) that reference them.

For how these six resources reference each other, including which spec field carries each reference, see the [CRD reference graph](../concepts/core-concepts.md#the-custom-resources).
