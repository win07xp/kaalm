# Errors, events, and testing

Reconcilers do the work; this page covers what happens around it. How a failure is classified decides whether the controller retries, degrades, gates, or gives up. What the controller reports about that decision arrives through Kubernetes Events and Prometheus metrics. All of it is testable without a real LLM provider in the loop.

Read [Operator structure](overview.md) and [Reconcilers](reconcilers.md#agentreconciler) first: the rules here refer to reconciler steps.

## Error handling

The controller classifies a failure in the order below, and the class decides the response.

![Flowchart of how the controller classifies a failure, as two rows. Retry or degrade: an apiserver or probe transport error returns the error and requeues with backoff; a provider budget blocking the namespace sets the Degraded condition with the phase unchanged; a class or provider no longer admitting the spec sets phase Degraded with preDegradedPhase kept. Gate or fail: a missing dependency such as an image, Secret, PVC, ConfigMap, or Certificate sets Ready=False and requeues in 30 or 5 seconds; a Pod that cannot run because of a crash loop or image pull sets phase Failed until the Pod recovers; an AgentTask out of retries settles phase Failed, terminal.](../diagrams/controller-error-buckets.svg)

| Class | Members | Response |
|---|---|---|
| Transient | apiserver conflicts and transport errors; a failed status or child write | return the error; controller-runtime requeues with per-item exponential backoff (5 ms, doubling, capped at 1000 s) under a 10 requests per second bucket, none of which the controller overrides |
| Recoverable | a referenced ModelProvider reports the Agent's namespace as budget-blocked | the `Degraded` condition with `reason=BudgetExhausted` on the Agent, phase unchanged; cleared when the provider reports the namespace unblocked, driven by the ModelProvider watch. This is the condition's only member. A provider that is not `Ready` shows on the Agent's `ProvidersReady` condition instead, which leaves the phase unchanged ([Agent status](../resources/agent.md)) |
| Irreconcilable | a class or provider no longer admits the workload's stored spec | `phase=Degraded` for an Agent, terminal `Failed` for an AgentTask; see [Degraded](agent-lifecycle.md#degraded). A ModelProvider's `allowedNamespaces` dropping the Agent's namespace is here, not in the recoverable class: nothing fixes itself, a human aligns one side |
| Gated | a missing image, `existingClaim`, pull Secret, or handler ConfigMap; a Certificate still being issued; a provider probe failure | `Ready=False` naming the gate, requeued in 30 seconds (5 for the Certificate, the probe interval for a provider); the phase is untouched. A failed provider probe never returns an error, so it gets the fixed interval, not backoff |
| Failed Pod | a container in `CrashLoopBackOff` at five restarts or in `ImagePullBackOff` | `phase=Failed` for an Agent, re-derived on every pass and cleared when the Pod recovers; see [Failed](agent-lifecycle.md#failed). For an AgentTask a provisioning failure is a retry or, with `backoffLimit` spent, the terminal `Failed` |

There is no bucket that stops reconciling: every class keeps the resource reconciling on its events and cadence, and a spec change re-enters the pass like any other event.

## Event emission

The controller emits these Events. Each reason is a stable string; the message carries the detail. Every event tied to a status change (a phase transition, a new `Ready=False` reason, budget exhaustion, a blocked delete) is emitted only after the status write that records the change succeeds, and only when the change is new, not on every pass that finds the same state again: a status write lost to a conflict does not report a change twice, and one that never happened is never reported.

| Resource | Reason | Type | When |
|---|---|---|---|
| Agent | `PhaseChanged` | Normal | every phase transition, with a message naming the cause where the transition has one (for example `phase changed from Running to Idle: no activity for 30m0s`). An Agent's first phase, `Pending`, is not a transition and emits nothing |
| Agent | `BudgetExhausted` | Warning | a referenced provider's budget first reports the Agent's namespace as blocked; not repeated while the block holds, and fired again if it clears and returns |
| Agent | `Hibernated`, `Woken` | Normal | the Pod is gone after hibernation; a wake annotation is honored |
| Agent | `WakeIgnored` | Warning | a wake annotation on a non-`Hibernated` Agent, except in `Hibernating` (kept until `Hibernated`) and `Resuming` |
| Agent | `PodDisrupted`, `SpecDrift`, `SpecDriftPending` | Warning, Normal, Normal | a terminal Pod is replaced; a spec hash change replaces the Pod after a `maxUnavailableOnDrift` slot is free; a drifted Agent starts waiting for one |
| Agent | the Degraded reason (`ClassConstraintViolation`, `PersistenceNotAllowed`, `HibernationNotAllowed`, `HibernationRequiresPersistence`, `HandlerMountNotAllowed`, `ToolNotInCatalog`) | Warning | the first entry into `Degraded`, once per entry |
| Agent | `SystemNamespaceForbidden`, `InvalidReference`, `ExistingClaimNotFound`, `ImagePullSecretMissing`, `HandlerConfigMapNotFound` | Warning | a reconcile-time validation failure sets `Ready=False` with the reason, when the reason first appears on `Ready` |
| AgentTask | `TaskSucceeded`; `TaskFailed`, `TimeoutExceeded`, `TimeoutSucceeded`, and the provisioning and class reasons | Normal; Warning | the task settles, or a retry starts (message suffix `retrying (n/limit)`); see [Event reasons](task-lifecycle.md#event-reasons) |
| AgentTask | `SystemNamespaceForbidden`, `InvalidReference`, `ImagePullSecretMissing` | Warning | a pre-Pod reconcile-time gate sets `Ready=False` with the reason, when the reason first appears on `Ready`; see [Event reasons](task-lifecycle.md#event-reasons) |
| ModelProvider | `ProviderUnhealthy` | Warning | a probe fails for a reason other than the credential |
| ModelProvider | `CredentialsMissing` | Warning | a reconcile-time validation failure sets `Ready=False` with the reason, when the reason first appears on `Ready` |
| ModelProvider | `DegradeTargetNotCheapest`, `MaxOutputTokensUnset` | Warning | the degrade-target cost check, when its condition first turns `True`; a cross-format fallback into an Anthropic model with no `maxOutputTokens` |
| ModelProvider | `ObservedTrafficExceededMargin` | Warning | a gateway replica first raises the boundary margin flag; the `BoundaryMarginRaised` condition carries the same reason |
| ToolProvider | `ProviderUnhealthy` | Warning | a probe fails for a reason other than the credential |
| ToolProvider | `CredentialsMissing` | Warning | a reconcile-time validation failure sets `Ready=False` with the reason, when the reason first appears on `Ready` |
| ToolProvider | `CredentialsInvalid` | Warning | the probe's `Healthy` condition first enters `CredentialsInvalid` (a `401` or `403` from the tool server), not on every pass while the credential stays rejected; a rejection after a recovery fires it again |
| AgentChannel | `CallbackHostUnresolved` | Warning | the `callbackUrl` host does not resolve at reconcile time; the channel stays `Ready=True` ([rule 22](../resources/validation-and-defaulting.md#cross-resource-validation)). Emitted when the unresolved host first appears or changes, not on every pass |
| AgentChannel | `SystemNamespaceForbidden`, `AgentNotFound`, and every other `Ready=False` reason `validateChannel` returns (`AgentServiceDisabled`, `InvalidPath`, `PathConflict`, `ChildConflict`, `InvalidReference`, `CredentialsMissing`, and the other Secret and callback reasons) | Warning | a reconcile-time validation failure sets `Ready=False` with the reason, when the reason first appears on `Ready` |
| AgentClass | `FQDNPolicyUnsupported` | Warning | `allowedHosts` is set on a CNI without FQDN egress, so the hosts are ignored |
| AgentClass | `InvalidReference`, `InvalidCIDR` | Warning | a reconcile-time validation failure sets `Ready=False` with the reason, when the reason first appears on `Ready` |
| ModelProvider, ToolProvider, AgentClass | `DeletionBlocked` | Warning | the finalizer's delete hold first appears, naming a referrer ([Cluster-scoped resources](finalizers.md#cluster-scoped-resources)) |

Events are how `kubectl describe` reports state changes, and an operator debugging a stuck resource reaches for it before metrics or logs. Two signals are conditions only, not Events: a ModelProvider's or ToolProvider's `CredentialsInvalid`, the probe's auth failure (the `ProviderUnhealthy` Warning above covers only a non-credential probe failure), and `FallbackIneligible`, which is a gateway Event at request time, not a controller one ([Recommended alerts](../operations/observability.md#recommended-alerts) lists the gateway's).

## Observability

The chart serves the controller's Prometheus metrics on `:8080/metrics` over plain HTTP ([Endpoints](../operations/observability.md#endpoints)); the binary keeps metrics off unless a bind address is passed. Standard controller-runtime metrics (reconcile counts, duration, queue depth) are emitted automatically, and the leader is the only replica with non-zero values for them. The Kaalm-specific metrics:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `kaalm_agents` | gauge | `phase`, `namespace` | Agent count by phase |
| `kaalm_tasks` | gauge | `phase`, `namespace` | AgentTask count by phase |
| `kaalm_channels` | gauge | `namespace`, `phase`, `ready`, `platform_connected` | AgentChannel count by `status.phase`, the `Ready` condition, and the tri-state `PlatformConnected` condition, the last two as `true`, `false`, or `unknown` |
| `kaalm_provider_budget_canonical_usd` | gauge | `provider`, `namespace`, `period` | the canonical spend total the ModelProvider fold writes ([step 4](reconcilers.md#modelproviderreconciler)), distinct from the gateway's per-replica `kaalm_llm_spend_usd_total` partials |
| `kaalm_hibernations_total` | counter | `namespace` | hibernations completed |
| `kaalm_wakes_total` | counter | `namespace`, `trigger` | wakes honored. `trigger` is `channel` for a wake the activator requested on a channel message, `annotation` for a manual wake ([Manual wake](hibernation-and-wake.md#manual-wake)) |
| `kaalm_storage_migrated_objects_total` | counter | `kind` | custom resources the storage-version migrator rewrote at `v1beta1`: zero on a fresh install, the pre-upgrade object count on the first leader start after an upgrade, zero after that ([Storage version migration](../operations/api-versioning.md#storage-version-migration)) |

The three phase gauges carry no `_total` suffix, which OpenMetrics reserves for counters. They are computed from the manager cache on every scrape, and a resource whose `status.phase` is still empty counts as `Pending`. Every replica serves them from its own cache, so dashboards aggregate them with `max`, not `sum`. Budget policy actions are counted where they happen, on the gateway's request path, as `kaalm_budget_threshold_events_total` ([LLM Gateway operations](../gateways/llm/operations.md#observability)); the channel metrics are under [User Gateway operations](../gateways/user/operations.md#observability).

## Testing strategy notes

- Reconcilers run under envtest, a real `kube-apiserver` and `etcd` launched from `KUBEBUILDER_ASSETS`, with the fake client used only for isolated helpers. The suite injects fakes at the three points where a reconciler would leave the cluster: the `ProviderHealthChecker` and `ToolHealthChecker` interfaces behind the probes, and the `ActivityClient` behind the activity and channel-health fan-outs.
- State machine transitions are table tests over those fakes.
- `make cover-check` gates union coverage at 85 percent, run with `GOWORK=off` and excluding the e2e packages, the same gate CI runs.
- End-to-end tests run against a k3d cluster with a stub LLM provider (an HTTP server answering canned completions with fake token counts) and mock MCP, Discord, and WhatsApp servers; [Scenario coverage](../appendix/scenario-coverage.md) maps them to the acceptance scenarios.

The controller holds no assumptions about specific LLM providers. Because agents never talk to providers directly and all LLM traffic goes through the gateway, substituting a stub at that one point covers the whole system.
