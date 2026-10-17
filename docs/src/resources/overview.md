# Resource overview

This part of the book is the field reference for the six custom resources. There is one page per kind, followed by the [cross-resource validation rules](validation-and-defaulting.md#cross-resource-validation) and the [defaulting behavior](validation/schema-and-defaulting.md#defaulting) that tie them together. The HTTP endpoints agent containers call are in [HTTP API](../gateways/api/overview.md).

All resources live in one API group, `kaalm.io`, served at one version, `v1beta1`, which is the storage version and the compatibility contract ([API versioning and deprecation](../operations/api-versioning.md)).

## Resource summary

| Kind | Short name | Scope | Owner | Page |
|---|---|---|---|---|
| `AgentClass` | `ac` | Cluster | Platform | [AgentClass](agentclass.md) |
| `ModelProvider` | `mp` | Cluster | Platform | [ModelProvider](modelprovider.md) |
| `ToolProvider` | `tp` | Cluster | Platform | [ToolProvider](toolprovider.md) |
| `Agent` | `ag` | Namespace | Developer | [Agent](agent.md) |
| `AgentTask` | `at` | Namespace | Developer | [AgentTask](agenttask.md) |
| `AgentChannel` | `ach` | Namespace | Developer | [AgentChannel](agentchannel.md) |

The Owner column is the intended split of responsibility: developers' namespaced resources reference the platform team's cluster-scoped ones. What each kind is for, and which tier uses it, is on [Core concepts](../concepts/core-concepts.md#the-custom-resources), together with the reference graph that shows which spec field carries each reference.
