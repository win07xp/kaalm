# Summary

[Introduction](introduction.md)
[Implementation roadmap](ROADMAP.md)

# Orientation

- [Vision and scope](concepts/vision-and-scope.md)
- [Core concepts](concepts/core-concepts.md)
- [Personas and the primary scenario](concepts/personas.md)
- [System architecture](concepts/system-architecture.md)
- [Multi-tenancy and adoption tiers](concepts/tenancy-and-tiers.md)

# Resource Model

- [Resource overview](resources/overview.md)
- [AgentClass](resources/agentclass.md)
- [ModelProvider](resources/modelprovider.md)
- [ToolProvider](resources/toolprovider.md)
- [Agent](resources/agent.md)
- [AgentTask](resources/agenttask.md)
- [AgentChannel](resources/agentchannel.md)
- [Validation and defaulting](resources/validation-and-defaulting.md)

# Agent Runtime

- [The runtime contract](runtime/contract.md)
- [Reference base images](runtime/base-images.md)
- [Starter templates](runtime/starter-templates.md)
- [Child resources](runtime/child-resources.md)

# The Gateways

- [Gateway overview](gateways/overview.md)
  - [Cluster listener TLS](gateways/listener-tls.md)
- [HTTP API](gateways/api/overview.md)
  - [Channel webhook](gateways/api/channel-webhook.md)
  - [Discord channel](gateways/api/channel-discord.md)
  - [WhatsApp channel](gateways/api/channel-whatsapp.md)
  - [Task completion](gateways/api/task-complete.md)
  - [Agent endpoints](gateways/api/agent-endpoints.md)
  - [Async webhook responses](gateways/api/async-responses.md)
  - [Internal endpoints](gateways/api/internal-endpoints.md)
  - [Error reference](gateways/api/errors.md)
- [LLM Gateway](gateways/llm/overview.md)
  - [Request handling](gateways/llm/request-handling.md)
  - [Workload identity](gateways/llm/workload-identity.md)
  - [Provider routing and adapters](gateways/llm/provider-routing.md)
  - [Budgets and rate limits](gateways/llm/budgets-and-rate-limits.md)
  - [Fallback logic](gateways/llm/fallback.md)
  - [LLM Gateway operations](gateways/llm/operations.md)
- [The tool plane](gateways/tool-plane.md)
- [User Gateway](gateways/user/overview.md)
  - [Platform adapters and channel health](gateways/user/platform-adapters.md)
  - [Activation and activity tracking](gateways/user/activation-and-activity.md)
  - [User Gateway operations](gateways/user/operations.md)

# The Controller

- [Operator Structure](controller/overview.md)
- [Reconcilers](controller/reconcilers.md)
- [Agent Lifecycle](controller/agent-lifecycle.md)
- [Hibernation and Wake](controller/hibernation-and-wake.md)
- [Change Propagation](controller/change-propagation.md)
- [AgentTask Lifecycle](controller/task-lifecycle.md)
- [Finalizers](controller/finalizers.md)
- [Errors, Events, and Testing](controller/operations.md)

# The Console

- [Console Overview](console/overview.md)

# Security

- [Security Model and Isolation](security/model.md)
- [RBAC and Authentication](security/rbac.md)
- [Credential Handling](security/credentials.md)
- [TLS and Certificates](security/tls.md)
- [Threat Model](security/threat-model.md)

# Operations

- [Deployment](operations/deployment.md)
- [API Versioning and Deprecation](operations/api-versioning.md)
- [Observability](operations/observability.md)
- [Load and Scale](operations/load-and-scale.md)

---

- [Acceptance Scenarios](appendix/scenarios.md)
- [Scenario Coverage](appendix/scenario-coverage.md)
- [Lifecycles at a Glance](appendix/lifecycles.md)
