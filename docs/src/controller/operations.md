# Errors, events, and testing

Reconcilers do the work; this page covers what happens around it. How a failure is classified decides whether the controller retries, degrades, gates, or gives up. The controller reports that decision through Kubernetes Events and Prometheus metrics. All of it is testable without a real LLM provider in the loop.

## Error handling

The controller classifies a failure in the order below, and the class decides the response.

![Flowchart of how the controller classifies a failure, as two rows. Retry or degrade: an apiserver or probe transport error returns the error and requeues with backoff; a provider budget blocking the namespace sets the Degraded condition with the phase unchanged; a class or provider no longer admitting the spec sets phase Degraded with preDegradedPhase kept. Gate or fail: a missing dependency such as an image, Secret, PVC, ConfigMap, or Certificate sets Ready=False, and all but a missing image requeue in 30 or 5 seconds; a Pod create or a child write the API server rejects sets Ready=False PodCreateRejected or ChildWriteRejected and requeues in 30 seconds; a Pod that cannot run because of a crash loop or image pull sets phase Failed until the Pod recovers; an AgentTask out of retries settles phase Failed, terminal.](../diagrams/controller-error-buckets.svg)

| Class | Members | Response |
|---|---|---|
| Transient | apiserver conflicts and transport errors; a failed status write; a child write or a Pod create that fails for any reason other than a rejection, such as a timeout, a `429`, a server error, or an admission webhook that cannot be reached | return the error; controller-runtime requeues with per-item exponential backoff (5 ms, doubling, capped at 1000 s). Nothing is written to status, so an outage shows only in the controller log |
| Recoverable | a referenced ModelProvider reports the Agent's namespace as budget-blocked | the `Degraded` condition with `reason=BudgetExhausted` on the Agent, phase unchanged; cleared when the provider reports the namespace unblocked or stops tracking a budget ([Budget reconciliation](reconcilers/modelprovider.md#budget-reconciliation)), driven by the ModelProvider watch. This is the condition's only member. A provider that is not `Ready` shows on the Agent's `ProvidersReady` condition instead, which leaves the phase unchanged ([Agent status](../resources/agent.md)) |
| Irreconcilable | a class or provider no longer admits the workload's stored spec | `phase=Degraded` for an Agent, terminal `Failed` for an AgentTask; see [Degraded](agent-lifecycle.md#degraded). A ModelProvider's `allowedNamespaces` dropping the Agent's namespace is here, not in the recoverable class: nothing fixes itself, a human aligns one side |
| Gated | a missing image or a malformed class `allowedCIDRs` entry; a missing `existingClaim`, pull Secret, or handler ConfigMap, or an env Secret that is missing or lacks the workload label; a Certificate still being issued; a provider probe failure; a Pod create the API server rejects with `Forbidden`, `Invalid`, or `BadRequest`, such as a missing RuntimeClass, a ResourceQuota, or an admission webhook or PodSecurity denial; a create, update, or delete of a child other than the Pod, or a retrying AgentTask's delete of its old Pod, that the API server rejects with one of those errors | `Ready=False` naming the gate, except a provider probe failure, which sets only `Healthy=False`. The phase is untouched, except for a Certificate wait and a rejected Pod create. An Agent with no Pod waits in `Provisioning` (`Resuming` if woken), and an Agent with a Pod keeps the phase its Pod gives ([Certificate wait with a running Pod](reconcilers/agent.md#certificate-wait-with-a-running-pod)). A `Pending` AgentTask waiting on its Certificate moves to `Provisioning`. A rejected Pod create takes the phase a normal create sets. A rejected child write leaves an Agent's phase alone. The image and `allowedCIDRs` gates have no requeue, because an edit to the Agent or class re-runs them. The others requeue in 30 seconds (the probe interval for a provider, 5 for an AgentTask's Certificate; the Agent's Certificate requeue is on the page linked above). A failed provider probe never returns an error, so it gets the fixed interval, not backoff. A rejected Pod create sets `Ready=False, reason=PodCreateRejected`, and a rejected child write sets `reason=ChildWriteRejected`; see [A rejected Pod create](#a-rejected-pod-create) and [A rejected child write](#a-rejected-child-write) |
| Failed Pod | a container in `CrashLoopBackOff` at five restarts or in `ImagePullBackOff` | `phase=Failed` for an Agent, re-derived on every pass and cleared when the Pod recovers; see [Failed](agent-lifecycle.md#failed). For an AgentTask a provisioning failure is a retry or, with `backoffLimit` spent, the terminal `Failed` |

There is no bucket that stops reconciling: every class keeps the resource reconciling on its events and cadence, and a spec change re-enters the pass like any other event.

### A rejected Pod create

When the API server rejects a Pod create with `Forbidden`, `Invalid`, or `BadRequest`:

- The `Ready` message is the API server's error, so the cause shows in `kubectl describe` without the controller log.
- The phase follows a normal create: `Provisioning`, or `Resuming` for a woken Agent.
- The controller re-checks every 30 seconds, because a RuntimeClass, a quota, or a webhook raises no watch event.
- An AgentTask fails the attempt once its Pod create has been rejected for five minutes ([The clock starts at Ready](task-lifecycle.md#the-clock-starts-at-ready)).

### A rejected child write

When the API server rejects a create, update, or delete of a child other than the Pod, or a retrying AgentTask's delete of its old Pod, with `Forbidden`, `Invalid`, or `BadRequest`, the workload gets `Ready=False, reason=ChildWriteRejected`. The child can be the Certificate, ServiceAccount, Service, PVC, NetworkPolicy, FQDN policy, a Secret-access Role or RoleBinding, or a task's completion mailbox.

- The `Ready` message names the operation, the child's kind and name, and the API server's error, so you know which object to look at without the controller log.
- The pass stops before the Pod is converged, so no Pod is created and a running Pod stays. An Agent keeps its phase, so a `Running` Agent can show `Ready=False`. An AgentTask with no Pod waits in `Provisioning`.
- The controller re-checks every 30 seconds, because a quota or a webhook raises no watch event.
- An AgentTask with no Pod fails the attempt once a write has been rejected for five minutes ([The clock starts at Ready](task-lifecycle.md#the-clock-starts-at-ready)). An AgentTask with a Pod keeps running ([Timing](reconcilers/agenttask.md#timing)). An AgentTask mid-retry waits in `Failed` with no `completionTime` and has no deadline ([Retry mechanics](task-lifecycle.md#retry-mechanics)).

An AgentChannel reports the same reason when the API server refuses a write of one of its Roles or RoleBindings, and re-checks every 30 seconds ([Per-channel credential Roles](reconcilers/agentchannel.md#per-channel-credential-roles)).

## Event emission

The controller emits these Events. Each reason is a stable string; the message carries the detail. An Event reports either a state or an occurrence:

- **A state** needs a person to fix or notice something: a validation failure, a rejected credential, an advisory finding, a phase transition, budget exhaustion, or a blocked delete. The condition or phase is the durable record, and the Event announces that the state began. It fires once, on the rising edge of the condition or of the `Ready=False` reason, and only after the status write that records it succeeds, so a failed write sends nothing and the retry sends the Event once. Every `Ready=False` reason below follows this rule unless its row says otherwise. A state that other people's actions can extend announces each new finding instead of only the rising edge: the [fallback eligibility scan](reconcilers/modelprovider.md#reconcile-time-fallback-eligibility-scan) does, because a new team's Agents can add a finding without any edit to the providers.
- **An occurrence** is one failed attempt of a check that keeps running. It fires on every attempt, and client-go's Event aggregation merges repeats into one Event with a count and a fresh timestamp, which keeps it visible past the one-hour Event TTL during a long outage. `ProviderUnhealthy` is an occurrence, so every failing probe pass sends it. `PodDisrupted`, `SpecDrift`, and `WakeIgnored` are occurrences too: each fires once per action.

| Resource | Reason | Type | When |
|---|---|---|---|
| Agent | `PhaseChanged` | Normal | every phase transition; the message names the old and new phase and, where the transition has one, the cause. An Agent's first phase, `Pending`, is not a transition and emits nothing |
| Agent | `BudgetExhausted` | Warning | a referenced provider's budget first reports the Agent's namespace as blocked; not repeated while the block holds, and fired again if it clears and returns |
| Agent | `Hibernated`, `Woken` | Normal | the Pod is gone after hibernation; a wake annotation is honored |
| Agent | `WakeIgnored` | Warning | a wake annotation on a non-`Hibernated` Agent, except in `Hibernating` (kept until `Hibernated`) and `Resuming` |
| Agent | `PodDisrupted`, `SpecDrift`, `SpecDriftPending` | Warning, Normal, Normal | a terminal Pod is replaced; a spec hash change, or a Certificate that names a different TLS Secret than the Pod mounts, replaces the Pod after a `maxUnavailableOnDrift` slot is free; a drifted Agent starts waiting for one |
| Agent | the Degraded reason (`NamespaceNotAllowed`, `ClassConstraintViolation`, `PersistenceNotAllowed`, `HibernationNotAllowed`, `HibernationRequiresPersistence`, `HandlerMountNotAllowed`, `ToolNotInCatalog`) | Warning | the first entry into `Degraded`, once per entry |
| Agent | `SystemNamespaceForbidden`, `InvalidReference`, `ExistingClaimNotFound`, `ImagePullSecretMissing`, `SecretNotOptedIn` (rule 48), `HandlerConfigMapNotFound` | Warning | a reconcile-time validation failure sets `Ready=False` with the reason |
| Agent, AgentTask | `PodCreateRejected` | Warning | the API server rejects the Pod create and `Ready=False` first takes this reason; not repeated on each 30-second re-check, and not when only the message changes, as quota counts do |
| Agent, AgentTask | `ChildWriteRejected` | Warning | the API server rejects a child write and `Ready=False` first takes this reason; not repeated on each 30-second re-check, and not when only the message changes |
| Agent, AgentTask | `ChildConflict` | Warning | a child object that the workload would own already exists and is not owned by it; fires again when the message changes, such as when a different child conflicts |
| Agent, AgentTask, AgentClass | `ResourceClaimsIgnored` | Warning | the object's resources block sets container `claims` ([rule 53](../resources/validation/class-policy.md)); `Ready` is unaffected. The message names the field and the claims. It is emitted when the claims first appear or change, not on every pass, and once more after a controller restart or leader change while they remain. An Agent warns about its own `spec.resources` only, and claims in class defaults warn on the class. A terminal AgentTask does not emit it, and the class emits it only after its status write succeeds |
| AgentTask | `TaskSucceeded`; `TaskFailed`, `TimeoutExceeded`, `TimeoutSucceeded`, and the provisioning and class reasons | Normal; Warning | the task settles, or a retry starts; see [Event reasons](task-lifecycle.md#event-reasons) |
| AgentTask | `SystemNamespaceForbidden`, `InvalidReference`, `ImagePullSecretMissing`, `SecretNotOptedIn` (rule 48) | Warning | a pre-Pod reconcile-time gate sets `Ready=False` with the reason; see [Event reasons](task-lifecycle.md#event-reasons) |
| ModelProvider | `ProviderUnhealthy` | Warning | every probe pass that fails for a reason other than the credential |
| ModelProvider | `CredentialsMissing`, `SecretNotOptedIn` ([rule 49](../resources/validation/providers.md#provider-credentials)), `EndpointHostNotApproved` (rule 50), `FallbackIneligible` (the structural check, rules 11 and 12), `InvalidDegradeTarget` (rule 18), `InvalidModelMap` (rule 41), `HardBudgetUnpriced` (rule 33), `InvalidNamespacePattern` (rule 51), `CredentialsInvalid` (the probe: the provider rejected the credential; the condition carries the message) | Warning | every `Ready=False` reason a reconcile sets |
| ModelProvider | `DegradeTargetNotCheapest` | Warning | the degrade-target cost check turns its condition `True`; the condition message lists every `degradeTo` target that costs more than another priced model, and a changed list while the condition stays `True` sends nothing |
| ModelProvider | `MaxOutputTokensUnset` | Warning | the cross-format fallback check turns its condition `True`: an edge into an Anthropic model with no `maxOutputTokens`. The condition message lists the models, and a changed list while the condition stays `True` sends nothing |
| ModelProvider | `FallbackIneligible` | Warning | the advisory [eligibility scan](reconcilers/modelprovider.md#reconcile-time-fallback-eligibility-scan) finds a fallback candidate that a caller's namespace or model can never reach (this scan leaves `Ready` alone), and the finding is new: the previous condition did not list it. A pass with the same or fewer findings sends nothing. The gateway records the same reason at request time ([Recommended alerts](../operations/observability.md#recommended-alerts) lists the gateway's) |
| ModelProvider | `ObservedTrafficExceededMargin` | Warning | a gateway replica first raises the boundary margin flag, which turns the `BoundaryMarginRaised` condition `True`; the condition carries the same reason |
| ToolProvider | `ProviderUnhealthy` | Warning | every probe pass that fails for a reason other than the credential |
| ToolProvider | `CredentialsMissing`, `SecretNotOptedIn` (rule 49), `EndpointHostNotApproved` (rule 50), `InvalidNamespacePattern` (rule 51) | Warning | a reconcile-time validation failure sets `Ready=False` with the reason. Rules 49 and 50 apply only when `credentialsRef` is set; rule 51 applies either way |
| ToolProvider | `CredentialsInvalid` | Warning | the probe gets a `401` or `403` from the tool server and sets `Ready=False` with the reason |
| AgentChannel | `CallbackHostUnresolved` | Warning | the `callbackUrl` host does not resolve at reconcile time; the channel stays `Ready=True` ([rule 22](../resources/validation/channels.md)). Emitted when the unresolved host first appears or changes, not on every pass |
| AgentChannel | `SecretNotOptedIn`, `CallbackHostNotApproved` | Warning | a referenced Secret has not opted in ([rule 45](../resources/validation/channels.md)), or a bearer `callbackAuth` Secret does not approve the `callbackUrl` host (rule 46). The message names the Secret and never its keys |
| AgentChannel | `SystemNamespaceForbidden`, `AgentNotFound`, and every other `Ready=False` reason `validateChannel` returns (`AgentServiceDisabled`, `InvalidPath`, `PathConflict`, `ChildConflict`, `ChildWriteRejected`, `CredentialsMissing`, `CredentialsInvalid`, `CallbackAuthMissing`, `CallbackAuthInvalid`, and `InvalidCallbackUrl`) | Warning | a reconcile-time validation failure sets `Ready=False` with the reason |
| AgentClass | `FQDNPolicyUnsupported` | Warning | the `FQDNPolicySupported` condition first turns `False`: `allowedHosts` is set on a CNI without FQDN egress, so the hosts are ignored |
| AgentClass | `BelowRestrictedBaseline` | Warning | the `SecurityBaseline` condition first turns `False` because a `security` field falls below the restricted Pod Security Standard |
| AgentClass | `DeprecatedFieldSet` | Warning | the `DeprecatedFields` condition first turns `True` because the class sets a deprecated field (`network.allowHostNetwork: true`); a steady finding sends nothing |
| AgentClass | `InvalidReference`, `InvalidCIDR`, `InvalidNamespacePattern` (rule 51), `InvalidImagePattern` (rule 52) | Warning | a reconcile-time validation failure sets `Ready=False` with the reason |
| ModelProvider, ToolProvider, AgentClass | `DeletionBlocked` | Warning | the finalizer's delete hold first appears, naming a referrer ([Cluster-scoped resources](finalizers.md#cluster-scoped-resources)) |

## Observability

The chart serves the controller's Prometheus metrics on `:8080/metrics` over plain HTTP ([Endpoints](../operations/observability.md#endpoints)). Standard controller-runtime metrics (reconcile counts, duration, queue depth) are emitted automatically, and the leader is the only replica with non-zero values for them. The Kaalm-specific metrics:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `kaalm_agents` | gauge | `phase`, `namespace` | Agent count by phase |
| `kaalm_tasks` | gauge | `phase`, `namespace` | AgentTask count by phase |
| `kaalm_channels` | gauge | `namespace`, `phase`, `ready`, `platform_connected` | AgentChannel count by `status.phase`, the `Ready` condition, and the tri-state `PlatformConnected` condition, the last two as `true`, `false`, or `unknown` |
| `kaalm_provider_budget_canonical_usd` | gauge | `provider`, `namespace`, `period` | the current period's canonical spend total, one series per namespace, that the ModelProvider fold writes. Series for past periods are deleted ([Budget reconciliation](reconcilers/modelprovider.md#budget-reconciliation)). It is distinct from the gateway's per-replica `kaalm_llm_spend_usd_total` partials |
| `kaalm_hibernations_total` | counter | `namespace` | hibernations completed, counted after the `Hibernated` status write succeeds |
| `kaalm_wakes_total` | counter | `namespace`, `trigger` | wakes honored. `trigger` is `channel` for a wake the activator requested on a channel message, `annotation` for a manual wake ([Manual wake](hibernation-and-wake.md#manual-wake)) |
| `kaalm_storage_migrated_objects_total` | counter | `kind` | custom resources the storage-version migrator rewrote at `v1beta1`: zero on a fresh install, the pre-upgrade object count on the first leader start after an upgrade, zero after that ([Storage version migration](../operations/api-versioning.md#storage-version-migration)) |

The three phase gauges carry no `_total` suffix, which OpenMetrics reserves for counters. A resource whose `status.phase` is still empty counts as `Pending`. Every replica reports the same fleet counts, so dashboards aggregate the gauges with `max`, not `sum`. Budget policy actions are counted where they happen, on the gateway's request path, as `kaalm_budget_threshold_events_total` ([LLM Gateway operations](../gateways/llm/operations.md#observability)); the channel metrics are under [User Gateway operations](../gateways/user/operations.md#observability).

## Testing strategy notes

- Reconcilers run under envtest, a real `kube-apiserver` and `etcd`, with the fake client used only for isolated helpers. The suite injects fakes at the three points where a reconciler would leave the cluster: the `ProviderHealthChecker` and `ToolHealthChecker` interfaces behind the probes, and the `ActivityClient` behind the activity and channel-health fan-outs. State machine transitions are table tests over those fakes.
- `make cover-check` gates union coverage at 85 percent, the same gate CI runs.
- CI also runs `make test-race`, the same unit and envtest suites under the race detector, in its own job so the coverage job's time does not change. A data race in a reconciler or a test fails CI, and the command reproduces it locally.
- End-to-end tests run against a k3d cluster with a stub LLM provider (an HTTP server answering canned completions with fake token counts) and mock MCP, Discord, and WhatsApp servers; [Scenario coverage](../appendix/scenario-coverage.md) maps them to the acceptance scenarios.

The controller holds no assumptions about specific LLM providers. Because agents never talk to providers directly and all LLM traffic goes through the gateway, substituting a stub at that one point covers the whole system.
