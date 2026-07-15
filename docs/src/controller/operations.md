# Errors, Events, and Testing

Reconcilers do the work; this page covers what happens around that work. How a failure is classified decides whether the controller retries, degrades, or gives up. What the controller tells you about that decision arrives through Kubernetes Events and Prometheus metrics. And all of it has to be testable without a real LLM provider in the loop.

Read [Operator Structure](overview.md) and the [Reconciler Responsibilities](reconcilers.md#agentreconciler) first if you have not: the rules below refer to specific reconciler steps.

## Error Handling

Errors are classified into three categories. The category determines the controller's response, so classifying correctly matters more than the specific error type.

| Bucket | Controller response |
|---|---|
| **Transient** | Retry with backoff |
| **Recoverable** | Set `Degraded` condition, continue reconciling |
| **Terminal** | Set `Failed` phase, stop reconciling except on spec change |

### Transient

Something failed, but the same operation will probably succeed shortly. Members:

- API server conflicts (409)
- Transient Pod failures (crashloop with recent start)
- Network errors talking to ModelProvider for health checks

Handled by returning a `Requeue` result with exponential backoff (250ms -> 30s max).

### Recoverable

The resource cannot do its job right now, but the configuration is valid and the situation may resolve on its own. Members:

- Referenced ModelProvider becomes unhealthy (transient connectivity / 5xx from the provider)
- Budget exhaustion

The Agent remains in its current phase with `Degraded` condition set. Reconciles continue on relevant resource events, which is why the AgentReconciler watches `ModelProvider` and re-queues on change rather than waiting out the periodic requeue.

One exclusion is deliberate and easy to get wrong. A ModelProvider whose `allowedNamespaces` stops including the Agent's namespace is **not** in this bucket. That is a class-vs-spec mismatch, not a transient outage: it is handled via `phase=Degraded` per [AgentReconciler step 2](reconcilers.md#agentreconciler), consistent with [Per-Agent and Per-Task Child Resources bucket 2](../runtime/child-resources.md). The distinction is that nothing will fix itself here: a human has to align the Agent or the ModelProvider spec.

### Terminal

The configuration cannot produce a working resource, and retrying will not change that. Members:

- Image pull failure after max retries
- PVC provisioning failure that exceeds retry budget
- Invalid configuration that cannot be corrected

Reconciling stops until the spec changes, because a spec change is the only thing that can plausibly fix the problem.

## Event Emission

The controller emits Kubernetes Events for:

- Phase transitions (`Normal`, reason=`PhaseChanged`, message includes old->new).
- Provider errors (`Warning`, reason=`ProviderUnhealthy` or `BudgetExhausted`).
- Validation failures caught at reconcile time (`Warning`, reason=`InvalidReference`).
- Hibernation/wake events (`Normal`, reason=`Hibernated` / `Woken`).
- Task completion (`Normal`, reason=`TaskSucceeded` or `TaskFailed`).

Events are critical for `kubectl describe` usability. Err toward emitting events on every meaningful state change: an operator debugging a stuck Agent reaches for `kubectl describe` before they reach for metrics or logs, and an event that was never emitted is a dead end.

Individual reconcilers emit further reasons of their own beyond this core set, for example `FQDNPolicyUnsupported` from the [AgentClassReconciler](reconcilers.md#agentclassreconciler) and `FallbackIneligible` / `DegradeTargetNotCheapest` from the [ModelProviderReconciler](reconcilers.md#modelproviderreconciler).

## Observability

The controller exposes Prometheus metrics on `:8080/metrics` (standard controller-runtime port).

Standard controller-runtime metrics (reconcile counts, duration, queue depth) are emitted automatically. The following Agentry-specific metrics are added.

### Gauges

- `agentry_agents{phase,namespace}`: gauge of Agent count by phase and namespace
- `agentry_tasks{phase,namespace}`: gauge of AgentTask count by phase and namespace
- `agentry_channels{namespace,phase,ready,platform_connected}`: gauge of AgentChannel count
- `agentry_provider_budget_canonical_usd{provider,namespace,period}`: gauge of the reconciler-summed canonical spend total

The phase-count gauges deliberately carry no `_total` suffix. OpenMetrics reserves it for counters, and promlint flags non-counter `_total` names.

`agentry_channels` is rolled up by `status.phase` (`Active` | `Degraded` | `Failed` | `Terminating`, see [AgentChannelReconciler step 5](reconcilers.md#agentchannelreconciler)), `status.conditions[type=Ready]`, and `status.conditions[type=PlatformConnected]`. The two condition labels keep their `true` | `false` | `unknown` values. This surfaces both the bound-Agent state (via `phase`) and the tri-state `PlatformConnected` condition computed by [AgentChannelReconciler step 4](reconcilers.md#agentchannelreconciler).

`agentry_provider_budget_canonical_usd` is written by [ModelProviderReconciler step 3](reconcilers.md#modelproviderreconciler) after pruning stale-replica partials. It is distinct from the gateway's per-replica `agentry_llm_spend_usd_total` (the partials before reconciliation). Dashboards plot this gauge to show authoritative spend without summing across replicas.

### Counters

- `agentry_hibernations_total{namespace}`: counter of hibernation events
- `agentry_wakes_total{namespace,trigger}`: counter of wake events (trigger = `channel` | `annotation`)
- `agentry_budget_threshold_events_total{provider,namespace,action}`: counter of budget policy triggers (action = `degrade` | `block` | `warn`)

For gateway metrics (LLM and channel), see [LLM Gateway Operations](../gateways/llm/operations.md#observability) and [User Gateway Operations](../gateways/user/operations.md#observability).

## Testing Strategy Notes

While detailed test guidance lives in the (deferred) contribution guide, the design assumes:

- Each reconciler is unit-testable by injecting a fake client.
- State machine transitions are table-testable.
- Integration tests use `envtest` for API server + etcd in-memory.
- End-to-end tests run against a kind cluster with a stubbed LLM provider (an HTTP server that responds with canned completions and reports fake token counts).

The controller should not hardcode assumptions about real LLM providers. Testability depends on the gateway being swappable with a mock: because agents never talk to providers directly and all LLM traffic goes through the gateway, substituting a stub at that one seam covers the whole system.
