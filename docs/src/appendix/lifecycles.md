# Lifecycles at a glance

Every state machine in Kaalm is drawn once, on the page that specifies it. This page indexes them so you can find the states a thing can be in without knowing the book's layout.

## Workload phase machines

| Kind | Phases | Drawn on |
|---|---|---|
| Agent | `Pending`, `Provisioning`, `Running`, the idle cycle `Idle`, `Hibernating`, `Hibernated`, `Resuming`, and the any-phase branches to `Degraded`, `Failed`, and `Terminating` | [Agent lifecycle](../controller/agent-lifecycle.md), with every transition trigger |
| AgentTask | `Pending`, `Provisioning`, `Running`, `Completing`, then `Succeeded` or `TimedOut`, which are terminal, or `Failed`, which is terminal only after `completionTime` is stamped (a `Failed` task with retries left returns to `Provisioning`); plus `Terminating`. No `Degraded`: an irreconcilable task fails | [AgentTask lifecycle](../controller/task-lifecycle.md) |
| AgentChannel | `Active` and `Degraded` are a memoryless reduction of the bound Agent's phase, recomputed every pass; `Failed` means `agentRef` does not resolve; `Terminating` is set once by the delete path; unset before the first reconcile | [Channel phase reduction](../controller/reconcilers.md#channel-phase-reduction) |

The CRD schema does not constrain `status.phase`: the field is a plain string with no enum, because only the reconcilers write it.

## Platform resources without a phase

`AgentClass`, `ModelProvider`, and `ToolProvider` have no `status.phase`: they are configuration, not workloads, so nothing about them starts, idles, or terminates. Their observed state is carried by conditions: `Ready` on all three; `FQDNPolicySupported` on AgentClass ([AgentClassReconciler](../controller/reconcilers.md#agentclassreconciler)); `Healthy` on both providers; `GatewayReachable` and `BoundaryMarginRaised` on ModelProvider ([ModelProviderReconciler](../controller/reconcilers.md#modelproviderreconciler), [ModelProvider status](../resources/modelprovider.md#status)).

ModelProvider carries one state machine, per namespace and period: the budget state `Normal`, `Throttled`, `Blocked`, monotonic within a period and reset only by the rollover, drawn under [ModelProvider status](../resources/modelprovider.md#status).

## Supporting lifecycles

| Lifecycle | Drawn on |
|---|---|
| Finalizer and delete sequences for every kind | [Finalizers](../controller/finalizers.md) |
| Pod replacement on spec and class changes, by phase | [Change propagation](../controller/change-propagation.md#spec-change-handling) |
| The wake-failure state machine | [Wake-failure state machine](../controller/hibernation-and-wake.md#wake-failure-state-machine) |
| The `PlatformConnected` tri-state and its cross-replica reduction | [The PlatformConnected tri-state](../resources/agentchannel.md#the-platformconnected-tri-state), [Channel health tracking](../gateways/user/platform-adapters.md#channel-health-tracking) |
| Async webhook response records: created on `202`, consumed by polling or callback, pruned by TTL or the channel finalizer | [Response persistence](../gateways/api/async-responses.md#response-persistence) |
| MCP session ownership | [Session ownership](../gateways/tool-plane.md#session-ownership-legacy-revisions) |
| The CA: renewal with key reuse, and the re-key window | [CA renewal and re-key](../security/tls.md#ca-renewal-and-re-key) |
| The per-Agent serving certificate and the per-AgentTask client certificate | [Lifecycle of an Agent TLS serving certificate](../security/tls.md#lifecycle-of-an-agent-tls-serving-certificate), [Lifecycle of an AgentTask TLS client certificate](../security/tls.md#lifecycle-of-an-agenttask-tls-client-certificate) |
| The LLM API key, the tool server credential, and the channel credential | [Credential handling](../security/credentials.md) |
| Console login sessions, with the five-minute re-review | [Authentication](../console/overview.md#authentication) |
| Storage-version migration, once per leader term | [Storage-version migration](../operations/api-versioning.md#storage-version-migration) |

## How the machines are enforced

Phase transitions are written only by the reconcilers; the pass structure that gates them, including which validation rules block a Pod before it exists, is drawn in [Reconcilers](../controller/reconcilers.md) and [Validation and defaulting](../resources/validation-and-defaulting.md). The recurring pattern on an Agent: `phase=Degraded` marks a mismatch the developer can fix, is recoverable, and records `preDegradedPhase`; the `Degraded` condition with `reason=BudgetExhausted` reports a runtime problem and leaves the phase alone; `Failed` marks what cannot be fixed in place ([Degraded](../controller/agent-lifecycle.md#degraded), [Failed](../controller/agent-lifecycle.md#failed)).
