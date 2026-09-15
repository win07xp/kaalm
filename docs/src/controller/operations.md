# Errors, events, and testing

Reconcilers do the work; this page covers what happens around that work. How a failure is classified decides whether the controller retries, degrades, or gives up. What the controller tells you about that decision arrives through Kubernetes Events and Prometheus metrics. And all of it has to be testable without a real LLM provider in the loop.

Read [Operator structure](overview.md) and [Reconcilers](reconcilers.md#agentreconciler) first if you have not: the rules below refer to specific reconciler steps.

## Error handling

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

Handled by returning an error, which controller-runtime requeues with its default per-item exponential backoff (5 ms, doubling, capped at 1000 s) under an overall 10 requests per second bucket.

### Recoverable

The resource cannot do its job right now, but the configuration is valid and the situation may resolve on its own. Members:

- Referenced ModelProvider becomes unhealthy (transient connectivity / 5xx from the provider)
- Budget exhaustion

The Agent remains in its current phase with `Degraded` condition set. Reconciles continue on relevant resource events, which is why the AgentReconciler watches `ModelProvider` and re-queues on change rather than waiting out the periodic requeue.

One exclusion is deliberate and easy to get wrong. A ModelProvider whose `allowedNamespaces` stops including the Agent's namespace is **not** in this bucket. That is a class-vs-spec mismatch, not a transient outage: it is handled with `phase=Degraded` per [AgentReconciler step 2](reconcilers.md#agentreconciler), consistent with [Bucket 2: degrade-when-irreconcilable](change-propagation.md#bucket-2-degrade-when-irreconcilable). The distinction is that nothing will fix itself here: a human has to align the Agent or the ModelProvider spec.

### Terminal

The configuration cannot produce a working resource, and retrying will not change that. Members:

- Image pull failure after max retries
- PVC provisioning failure that exceeds retry budget
- Invalid configuration that cannot be corrected

Reconciling stops until the spec changes, because a spec change is the only thing that can plausibly fix the problem.

## Event emission

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

Standard controller-runtime metrics (reconcile counts, duration, queue depth) are emitted automatically. The following Kaalm-specific metrics are added.

### Gauges

- `kaalm_agents{phase,namespace}`: gauge of Agent count by phase and namespace
- `kaalm_tasks{phase,namespace}`: gauge of AgentTask count by phase and namespace
- `kaalm_channels{namespace,phase,ready,platform_connected}`: gauge of AgentChannel count
- `kaalm_provider_budget_canonical_usd{provider,namespace,period}`: gauge of the reconciler-summed canonical spend total

The phase-count gauges deliberately carry no `_total` suffix. OpenMetrics reserves it for counters, and promlint flags non-counter `_total` names. They are computed from the manager cache on every scrape (a resource the reconciler has not stamped yet counts as `Pending`), and every controller replica serves them from its own cache, so dashboards aggregate them with `max`, not `sum`.

`kaalm_channels` is rolled up by `status.phase` (`Active` | `Degraded` | `Failed` | `Terminating`, see [AgentChannelReconciler step 5](reconcilers.md#agentchannelreconciler)), `status.conditions[type=Ready]`, and `status.conditions[type=PlatformConnected]`. The two condition labels keep their `true` | `false` | `unknown` values. This surfaces both the bound-Agent state (through `phase`) and the tri-state `PlatformConnected` condition computed by [AgentChannelReconciler step 4](reconcilers.md#agentchannelreconciler).

`kaalm_provider_budget_canonical_usd` is written by [ModelProviderReconciler step 3](reconcilers.md#modelproviderreconciler) after pruning stale-replica partials. It is distinct from the gateway's per-replica `kaalm_llm_spend_usd_total` (the partials before reconciliation). Dashboards plot this gauge to show authoritative spend without summing across replicas.

### Counters

- `kaalm_hibernations_total{namespace}`: counter of hibernation events
- `kaalm_wakes_total{namespace,trigger}`: counter of wake events (trigger = `channel` | `annotation`)
- `kaalm_storage_migrated_objects_total{kind}`: counter of custom resources the storage-version migrator rewrote at the `v1beta1` storage version, by kind; zero on a fresh install, the number of pre-upgrade objects on the first leader start after an upgrade, and zero on every start after that ([API versioning and deprecation](../operations/api-versioning.md#storage-version-migration))

Budget policy actions are counted where they happen, on the gateway's request path: `kaalm_budget_threshold_events_total` in [LLM Gateway operations](../gateways/llm/operations.md#observability).

For gateway metrics (LLM and channel), see [LLM Gateway operations](../gateways/llm/operations.md#observability) and [User Gateway operations](../gateways/user/operations.md#observability).

## Testing strategy notes

The design assumes:

- Each reconciler is unit-testable by injecting a fake client.
- State machine transitions are table-testable.
- Integration tests use `envtest` for API server + etcd in-memory.
- End-to-end tests run against a k3d cluster with a stubbed LLM provider (an HTTP server that responds with canned completions and reports fake token counts).

The controller should not hardcode assumptions about real LLM providers. Testability depends on the gateway being swappable with a mock: because agents never talk to providers directly and all LLM traffic goes through the gateway, substituting a stub at that one point covers the whole system.
