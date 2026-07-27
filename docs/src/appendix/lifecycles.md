# Lifecycles at a Glance

Every state machine in Kaalm is drawn exactly once, on the page that specifies it. This index exists because those pages are spread across the book and a reader asking "what states can this thing be in?" should not need to know the book's layout to find out.

## Workload phase machines

- **Agent**: `Pending`, `Provisioning`, `Running`, the idle/hibernate cycle (`Idle`, `Hibernating`, `Hibernated`, `Resuming`), and the any-phase branches to `Degraded`, `Failed`, and `Terminating`. Drawn in [Agent Lifecycle](../controller/agent-lifecycle.md), which also specifies every transition trigger.
- **AgentTask**: `Pending` through `Provisioning`, `Running`, and `Completing` into the terminal `Succeeded`, `Failed`, or `TimedOut`, plus `Terminating`. No `Degraded`: an irreconcilable task fails. Drawn in [AgentTask Lifecycle](../controller/task-lifecycle.md).
- **AgentChannel**: `Active`, `Degraded`, `Failed`, `Terminating`, unset before the first reconcile. Not an independent lifecycle but a memoryless reduction of the bound Agent's phase, recomputed every pass. Drawn in [Channel phase reduction](../controller/reconcilers.md#channel-phase-reduction).

## Platform resources without a phase

`AgentClass` and `ModelProvider` have no `status.phase` by design: they are configuration, not workloads, so nothing about them starts, idles, or terminates. Their observed state is carried entirely by conditions (`Ready` on both; `Healthy` and `GatewayReachable` on ModelProvider), documented on [AgentClass](../resources/agentclass.md#status) and [ModelProvider](../resources/modelprovider.md#status).

ModelProvider does carry one true state machine, per namespace and period: the budget enforcement state `Normal`, `Throttled`, `Blocked`, monotonic within a period and reset only by the rollover. Drawn in [ModelProvider § Status](../resources/modelprovider.md#status).

## Supporting lifecycles

- **Async webhook response records** (`kaalm-async-*` ConfigMaps): created on `202 Accepted`, consumed by polling or callback delivery, pruned by TTL or the channel finalizer. Drawn in [Async Webhook Response](../gateways/api/async-responses.md).
- **Certificates**: the CA, gateway, per-Agent, and per-AgentTask certificate lifecycles, including rotation and the re-key dual-trust window. Drawn in [TLS](../security/tls.md).
- **Channel credentials**: creation, scoping, and rotation of AgentChannel auth material. Drawn in [Credentials](../security/credentials.md).

## How the machines are enforced

Phase transitions are written only by the reconcilers; the full pass structure that gates them (including which validation rules block a Pod before it exists) is drawn in [Reconcilers](../controller/reconcilers.md) and [Validation and Defaulting](../resources/validation-and-defaulting.md). The recurring pattern worth internalizing once: `phase=Degraded` marks a mismatch the developer can fix, is recoverable, and records `preDegradedPhase`; `Ready=False` reports a condition and leaves the phase alone; `Failed` marks what cannot be fixed in place, on Agents and AgentTasks alike.
