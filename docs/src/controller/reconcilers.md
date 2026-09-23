# Reconcilers

The operator hosts six reconcilers, one per CRD. This page is the implementation spec for each: what it watches, and what every reconciliation pass does, in the order it does it.

Read them in dependency order. `AgentClassReconciler`, `ModelProviderReconciler`, and `ToolProviderReconciler` validate the platform-level resources that the workload reconcilers depend on. `AgentReconciler` is the most complex: it creates and reconciles the full child-resource tree for a persistent agent. `AgentTaskReconciler` mirrors it for one-shot work, and `AgentChannelReconciler` creates no Pods at all: it validates, scopes credential access, and reports status.

For the state machines these reconcilers drive, see [Agent lifecycle](agent-lifecycle.md) and [AgentTask lifecycle](task-lifecycle.md). For the CRDs they implement, see [Resource overview](../resources/overview.md). Every reconciler adds its finalizer on the first pass and hands a resource with a deletion timestamp to its delete path; see [Finalizers](finalizers.md).

## What each reconciler watches

| Reconciler | Owned children | Other watches | Fires on |
|---|---|---|---|
| AgentClass | none | ModelProvider and ToolProvider, indexed by `allowedProviders` and `allowedToolProviders`; Agent and AgentTask, by their `agentClassRef` | any change, to re-validate references and refresh the usage counts |
| ModelProvider | none | Agent, AgentTask, and AgentClass, by their provider references; the `kaalm-budget-{name}` ConfigMap in `kaalm-system`; Secrets in `kaalm-system`, by a list-and-filter map; every other ModelProvider that declares a fallback | workload changes release the delete hold; the ConfigMap drives the budget fold; a credential or a fallback provider created after its provider recovers it |
| ToolProvider | none | Secrets in `kaalm-system`, by a list-and-filter map; Agent, AgentTask, and AgentClass, by their tool references | a credential created after its provider recovers it; workload changes release the delete hold |
| Agent | Pod, Service, ServiceAccount, PVC, NetworkPolicy, Certificate | AgentClass and ToolProvider (spec generation changes only), ModelProvider (spec changes and changes to the set of budget-blocked namespaces), each indexed by the referencing field | class, provider, and tool-provider drift, without a periodic requeue |
| AgentTask | Pod, PVC, ConfigMap, NetworkPolicy, ServiceAccount, Role, RoleBinding, Certificate | AgentClass, indexed by `agentClassRef` | class drift only; a ModelProvider or ToolProvider change does not re-enqueue tasks |
| AgentChannel | Role, RoleBinding | Agent, by `agentRef` | the bound Agent's phase changes |

The cert-manager output Secret of a Certificate carries an ownerRef to the Certificate, set by cert-manager, never to the Agent or AgentTask. The predicates on the Agent watches matter at scale: a class or tool provider re-enqueues its Agents only when its spec generation changes, and a model provider only when its spec, its set of blocked namespaces, or its `Ready` status or reason changes, so the spend counters the gateway publishes every ten seconds and the in-use counts the class reconciler writes do not re-enqueue the fleet.

## AgentClassReconciler

AgentClass has no owned child resources, so a pass validates the class, counts its users, and writes status.

1. Validate references and network fields, collecting every problem: a listed `allowedProviders` or `allowedToolProviders` entry that does not exist, an `allowedCIDRs` entry that does not parse ([rule 19](../resources/validation-and-defaulting.md#cross-resource-validation)), and an `allowedHosts` entry that is not a valid DNS name (rule 20). Any problem sets `Ready=False` with a message listing them all, sorted. The reason is `InvalidCIDR` when an `allowedCIDRs` entry is malformed, since that problem sorts first, and `InvalidReference` otherwise.
2. Write the `FQDNPolicySupported` condition on every pass. With `allowedHosts` empty it is `True, reason=NoHostsRequested`. With hosts listed, the cached result of the startup CNI probe decides: `True`, or `False, reason=FQDNPolicyUnsupported` with a `Warning` event of the same reason. `allowedHosts` is never synthesized into a NetworkPolicy; the condition and the event are its only effect, and the class still becomes `Ready=True`.
3. Count the Agents and AgentTasks referencing the class into `status.agentsInUse` and `status.tasksInUse`, and write status.

On delete, the class is held while either count is above zero; see [Cluster-scoped resources](finalizers.md#cluster-scoped-resources).

### CNI FQDN-policy probe

The probe runs once at controller startup and its result is cached for the process lifetime. It checks the apiserver's discovery API for CRDs that indicate FQDN egress support:

- Cilium: presence of `ciliumnetworkpolicies.cilium.io` (v2), which supports `toFQDNs`.
- Calico Enterprise: presence of `networkpolicies.crd.projectcalico.org` with the Enterprise licensing CRD (`licensekeys.crd.projectcalico.org`). Open-source Calico does not support FQDN egress.

If neither is present, FQDN policy is unsupported. Unknown CNIs count as unsupported, so the controller never generates policy the CNI cannot enforce. Because the probe runs only at startup, a CNI change is not picked up until the controller restarts; operators who change their CNI roll the controller Deployment afterwards.

## ModelProviderReconciler

1. **Credentials.** Read the Secret named by `spec.credentialsRef` from the operator namespace only. A missing Secret or a missing or empty key sets `Ready=False, reason=CredentialsMissing` and ends the pass. The reconciler watches the Secrets in the operator namespace, so a credential created or rotated afterwards re-enqueues every provider that names it.
2. **Configuration validation.** Four checks run together and the first problem class decides the reason: the fallback chain ([Fallback chain validation](#fallback-chain-validation)), the degrade targets (every `budget.policies[].degradeTo` must name a model in `spec.models`, else `reason=InvalidDegradeTarget`), hard-budget pricing ([rule 33](../resources/validation-and-defaulting.md#cross-resource-validation), else `reason=HardBudgetUnpriced`), and the advisory [cost sanity check](#cost-sanity-on-degradeto). Any problem sets `Ready=False` and ends the pass.
3. **Gateway mirror.** List the gateway Pods in `kaalm-system` and set `GatewayReachable=True` when at least one is Ready, else `False` with the message `no Ready gateway Pods in kaalm-system`. The condition is cluster-wide and mirrored onto every ModelProvider for `kubectl describe`. As shipped it is refreshed on every pass of this reconciler, not on gateway Pod events.
4. **Budget fold.** Reduce the gateway replicas' partial spend into the canonical totals; see [Budget reconciliation](#budget-reconciliation). The same step folds the per-agent spend counters from `kaalm-agentspend-{name}` and writes `status.clusterSpentUSD` and the `kaalm_provider_budget_canonical_usd` gauge.
5. **Liveness probe**, when `healthCheck.enabled` (the default): see [Liveness probe](#liveness-probe).
6. **Ready.** Set `Ready=True, reason=CredentialsValid`.

The pass requeues at `healthCheck.intervalSeconds` (default 60) when the probe ran, and every minute otherwise for a provider with a budget period, so the fold keeps running with the probe disabled. The reconciler never distributes credentials to agent Pods: the gateway reads them from `kaalm-system` itself.

### Liveness probe

The probe uses the provider's model-list endpoint, which requires authentication but consumes no tokens: Anthropic, OpenAI, and OpenAI-compatible adapters send `GET /v1/models`. As shipped there is no probe for `google-vertex`: the checker reports the probe skipped and the condition is `Healthy=Unknown, reason=ProbeSkipped`, so a Vertex provider has no liveness signal and no credential-rejection detection.

| Outcome | Conditions | Requeue |
|---|---|---|
| `2xx` | `Healthy=True, reason=UpstreamReachable` | `intervalSeconds` |
| `401` or `403` | `Healthy=False` and `Ready=False`, both `reason=CredentialsInvalid`; the pass ends | `intervalSeconds` |
| other error, network failure, or `5xx` | `Healthy=False, reason=ProviderUnhealthy` and a `Warning` event | `intervalSeconds` |
| skipped | `Healthy=Unknown, reason=ProbeSkipped` | as for a healthy provider |

A failing provider is probed at the same fixed interval as a healthy one; as shipped there is no backoff. The 401 and 403 classification matches the credential-problem class in [Fallback triggers](../gateways/llm/fallback.md#fallback-triggers).

Probe TLS trust is the system roots plus whatever the chart configures: `controller.trustClusterCAForProbes` adds the cluster CA and `controller.probeCA` an operator bundle, mirroring the gateway's upstream pair, so an in-cluster endpoint under a private CA can probe `Healthy` instead of failing every handshake ([Deployment](../operations/deployment.md)). The pool follows rotation without a restart. The same pool serves the ToolProvider probe.

### Budget reconciliation

The reconciler reads the per-replica partial spend counters from the provider's budget ConfigMap in `kaalm-system`; see [Budget state management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management) for the format.

Before summing, it cross-references the ConfigMap keys against the current gateway Pod names, from the same Pod list step 3 reads, and prunes stale entries left by scaled-down or replaced replicas, folding each pruned current-period key into the `_retired` accumulator first so published spend survives rollouts (required under [hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement)). It sums the remaining partials plus `_retired`, writes the canonical total to the `_canonical` key, and updates `status.budgetUsage` per namespace. A replica's `_marginExceeded` flag sets the `BoundaryMarginRaised` condition, cleared with `reason=MarginSufficient` when no replica reports it; the matching `Warning` event fires once, when the condition first turns on.

On budget period rollover, the previous period's totals are archived to status and its keys deleted as they are met; see [The reducer](../gateways/llm/budgets-and-rate-limits.md#the-reducer). Rollover is processed on the next pass after the boundary, so the lag from the boundary to the archive write is at most one requeue interval, which is acceptable for a display and restart-seed value that per-request enforcement never waits on.

### Fallback chain validation

The reconciler walks the full fallback chain, following each provider's `spec.fallback` up to `maxFallbackDepth`, and confirms no circular references, that every referenced provider exists, and that every edge satisfies rule 12 (the same `spec.type`, or a crossing the gateway translates: `anthropic` against `openai` or `openai-compatible`; `google-vertex` same-type only) and rule 41 (every `modelMap` key names one of the provider's own models and every value one of the fallback's). A violation sets `Ready=False`, with `reason=InvalidModelMap` for a bad map and `reason=FallbackIneligible` otherwise. A change to any ModelProvider re-enqueues every other provider that declares a fallback, so a chain recovers when a missing provider is created or a bad one is fixed. A cross-format hop into an Anthropic model with no `maxOutputTokens` also emits a `MaxOutputTokensUnset` warning. The depth cap is a gateway-level setting; the reconciler validates the chain structure regardless of the cap, since the cap can change without re-reconciling providers.

The design also calls for a reconcile-time scan of each fallback candidate's `allowedNamespaces` and `spec.models` against the primary's callers and models, emitting `FallbackIneligible` for a candidate that can never be used. As shipped that scan does not exist: the gateway's runtime [FallbackIneligible](../gateways/llm/fallback.md) event is the only signal, discovered at request time.

### Cost sanity on degradeTo

For each policy whose `action: degrade`, the reconciler computes `avgCost(model) = (costPer1MInputTokens + costPer1MOutputTokens) / 2` for the target and compares it with every other model in `spec.models`. If the target is not strictly the cheapest, it emits a `Warning` event with `reason=DegradeTargetNotCheapest` naming the cheaper alternative.

The check is advisory: it never sets `Ready=False`, because platform teams may have non-cost reasons (latency, capability, quality) to prefer a degrade target. A degrade policy pointing at a more expensive model is almost always a misconfiguration, and the reconciler catches it earlier than a monthly bill would. The check skips a policy with no `degradeTo`; a `degradeTo` naming a model that does not exist gets both the `InvalidDegradeTarget` failure and, when applicable, the cost warning.

## ToolProviderReconciler

Reconciliation is the ModelProviderReconciler's pass without budgets, fallback, and the gateway mirror:

1. **Credentials.** Resolve `spec.credentialsRef` only when set, and only from the operator namespace: a same-named Secret in a tenant namespace never satisfies the ref. A missing Secret or a missing or empty key sets `Ready=False, reason=CredentialsMissing` and ends the pass. A nil ref is valid, since unauthenticated servers exist; the probe then carries no credential, and the Ready message says so.
2. **Liveness probe**, when `healthCheck.enabled` (a nil block defaults to enabled, as on ModelProvider), bounded by `healthCheck.timeoutSeconds` (default 10s). The probe speaks MCP in whichever revision the server does; the sequence is specified under [Protocol revisions](../gateways/tool-plane.md#protocol-revisions). Success sets `Healthy=True, reason=UpstreamReachable` and records the negotiated revision in `status.mcpRevision`; a failed probe keeps the last recorded revision. A `401` or `403` anywhere in the sequence sets `Healthy=False` and `Ready=False`, both `reason=CredentialsInvalid`, and ends the pass. Any other failure sets `Healthy=False, reason=ProviderUnhealthy` with a `Warning` event. The probe requeues at `healthCheck.intervalSeconds` (default 60) whatever its outcome. The probe trust pool is the ModelProvider's.
3. **Ready.** Set `Ready=True, reason=CredentialsValid`.

On delete, the reconciler holds the ToolProvider while any Agent, AgentTask, or AgentClass references it ([Cluster-scoped resources](finalizers.md#cluster-scoped-resources)). The grant checks of rules 35 to 38 run on the workload reconcilers, not here, exactly as the provider checks of rules 3 to 5 do. The broker on the gateway enforces the grants at call time; see [The broker](../gateways/tool-plane.md#the-broker).

## AgentReconciler

This is the most complex reconciler. It implements the [Agent lifecycle](agent-lifecycle.md), and the numbered steps below are the order one pass runs them. Steps 1 to 7 can each end the pass; only a pass that reaches step 8 touches a child resource.

![Flowchart of one AgentReconciler pass as three rows of cascades. Steps 1 to 3: a kaalm.io/wake=true annotation ends the pass (Hibernated goes to Resuming, any other phase removes it with WakeIgnored); the kaalm-system namespace ends it with Ready=False SystemNamespaceForbidden; a missing AgentClass with Ready=False InvalidReference; a failed cross-check with phase=Degraded; a Degraded Agent with every check clear restores preDegradedPhase. Steps 4 to 7: the Degraded condition is set from budget state; Hibernating or Hibernated drives or holds and ends the pass; a malformed class allowedCIDRs entry, a missing image, existingClaim, pull Secret, or handler ConfigMap sets Ready=False, requeueing in 30s for an unwatched object; a Certificate that is not Ready sets Provisioning and requeues in 5s. Steps 8 to 11: converge the ServiceAccount, Service, PVC, and NetworkPolicy; then the Pod: none creates it in Provisioning, terminal or spec drift deletes it in Provisioning, a crash loop or ImagePullBackOff sets Failed, a Ready Pod sets Running or leaves Idle, otherwise Provisioning; evaluate activity for Running or Idle; write status if changed.](../diagrams/agent-reconcile-pass.svg)

### Reconciliation steps

1. **Wake annotation.** If `kaalm.io/wake` is `"true"`, handle it and end the pass: a `Hibernated` Agent moves to `Resuming`, stamps `status.lastActivityTime`, and has the annotation removed only after that status write commits, so a failed write leaves the wake observable; any other phase has the annotation removed at once, with a `WakeIgnored` warning except in `Resuming`. See [Manual wake](hibernation-and-wake.md#manual-wake).
2. **Guards.** An Agent in the operator namespace sets `Ready=False, reason=SystemNamespaceForbidden` and ends the pass: a certificate issued there could carry a SAN that collides with the gateway or controller Service identity ([rule 28](../resources/validation-and-defaulting.md#cross-resource-validation)); the same guard runs first in the AgentTask and AgentChannel reconcilers. A missing AgentClass sets `Ready=False, reason=InvalidReference`. Otherwise the effective spec is derived: class defaults and caps applied to image, resources, and the idle and hibernation timings ([AgentClass](../resources/agentclass.md)). The `ProvidersReady` condition is then set from the referenced providers: `True, reason=AllProvidersHealthy` when each is in the class allowlist, exists, and is `Ready`, otherwise `False` with `ClassConstraintViolation` or `ProviderUnhealthy` ([Agent status](../resources/agent.md)). It never changes the phase.
3. **Degrade gate.** Every class-versus-spec cross-check runs together: rules 2, 3, 4, 5, 24, 26, 29, 30, and 35 to 38. Any violation enters or refreshes `Degraded` and ends the pass; when every violation has cleared, the prior phase is restored and the pass requeues. The reasons, the `preDegradedPhase` bookkeeping, and what happens to the Pod meanwhile are specified under [Degraded](agent-lifecycle.md#degraded). Because this gate runs before the hibernation branch and the Pod, `Pending` and `Hibernated` Agents degrade too.
4. **Budget condition.** Set or clear the `Degraded` condition with `reason=BudgetExhausted` from the referenced providers' `status.budgetUsage`, without touching the phase; see [Error handling](operations.md#error-handling).
5. **Hibernation branch.** A `Hibernating` Agent has its Pod deleted and settles `Hibernated` once it is gone; a `Hibernated` Agent holds with no Pod. Either ends the pass. See [Hibernation mechanics](hibernation-and-wake.md#hibernation-mechanics).
6. **Ready gates.** Five checks block Pod creation without degrading, each setting `Ready=False` and, for the last three, requeueing in 30 seconds because Secrets, PVCs, and ConfigMaps are not watched: a malformed class `allowedCIDRs` entry (`reason=InvalidReference`, rule 19), no image (`reason=InvalidReference`), a missing `existingClaim` (`reason=ExistingClaimNotFound`, rule 27), a missing `imagePullSecrets` entry (`reason=ImagePullSecretMissing`, rule 23, read under the [per-workload pull-Secret Role](../security/rbac.md#operator-serviceaccount) the reconciler ensures first), and a missing handler ConfigMap (`reason=HandlerConfigMapNotFound`, rule 31). See [Ready gates](#ready-gates).
7. **Certificate.** Ensure the per-Agent `Certificate` and gate Pod creation on its readiness: while cert-manager issues it the phase is `Provisioning` with `Ready=False, reason=CertificateNotReady`, requeued every five seconds. See [Agent certificate](#agent-certificate).
8. **Children.** Converge the ServiceAccount, Service, PVC, and NetworkPolicy, each read from the informer before any write. See [Child-resource convergence](#child-resource-convergence).
9. **Pod.** Converge the Pod and derive the phase from it: create it when missing (`Provisioning`); replace it when terminal or when the spec hash drifts (`Provisioning`); set `Failed` on a persistent crash loop or an image pull failure; set `Running` when it is Ready, except that an `Idle` Agent stays `Idle`; otherwise `Provisioning`. Environment variables and probes are injected at creation. See [Injected environment and probes](#injected-environment-and-probes).
10. **Activity.** For a `Running` or `Idle` Agent with an effective `idleTimeout` above zero, read the gateway's activity data through the per-namespace cache and drive the idle and hibernation transitions. See [Activity detection](hibernation-and-wake.md#activity-detection).
11. **Status.** Write status only when the pass changed it. On any change to `status.phase`, `status.phaseTransitionTime` is set in the same write and never on condition-only or metadata-only updates; the activity decision reads it.

### Agent certificate

The Certificate is named `{agentName}-tls` in the Agent's namespace, owned by the Agent through `ownerReferences` so it is garbage-collected on Agent deletion.

| Field | Value |
|---|---|
| `spec.issuerRef` | `{ name: "kaalm-ca-issuer", kind: "ClusterIssuer" }` |
| `spec.secretName` | `{agentName}-tls`, the output Secret cert-manager creates in the Agent's namespace |
| `spec.dnsNames` | `{agentName}.{namespace}.svc.cluster.local`, `{agentName}.{namespace}.svc`, `{agentName}.{namespace}` |
| `spec.duration`, `spec.renewBefore` | `2160h` (90 days), `720h` (30 days), chart defaults |
| `spec.usages` | `server auth`, `client auth`: the same cert is the agent's serving cert and its mTLS client cert |

A `ClusterIssuer` is used because cert-manager does not resolve a namespaced `Issuer` across namespaces; the chart installs `kaalm-ca-issuer` sourcing from the `kaalm-ca` Secret in cert-manager's cluster resource namespace (chart value `certManager.clusterResourceNamespace`, default `cert-manager`). Pod creation is gated on `Certificate.status.conditions[type=Ready]` so the Pod never hangs on its projected Secret mount. Rotation is transparent: cert-manager renews per `renewBefore`, kubelet propagates the new Secret contents into the projected volume, and the agent reloads through the file-watch pattern ([Starter templates](../runtime/starter-templates.md)). The trust chain is under [In-cluster TLS](../security/tls.md#in-cluster-tls).

### Ready gates

The five gates run before the Certificate and the children, so a missing dependency surfaces as a condition rather than as a Pod wedged in `ImagePullBackOff` or `ContainerCreating`, and a malformed class `allowedCIDRs` entry surfaces as a condition rather than as a NetworkPolicy write the apiserver rejects on every pass. A fix to the class re-enqueues its Agents through the class watch, so the class gate carries no requeue. The other four are resolved in the Agent's namespace: the reconciler never copies Secrets from `kaalm-system` into user namespaces, and the handler ConfigMap is developer-owned, mounted without an ownerRef and never content-tracked. The class-level `imagePullSecrets` list is the only source of pull Secrets; there is no Agent-level field.

### Child-resource convergence

Every child is read from the informer before it is written, and status is written only when a pass changed it: an Agent reconciles on its periodic requeue and on every event from its children, and a create that expected `AlreadyExists`, an unconditional NetworkPolicy update, and a status write per pass were three apiserver writes per agent per pass that changed nothing.

- **Pod.** Created with `restartPolicy: Always` and the spec hash stamped as the `kaalm.io/pod-spec-hash` annotation. Replacement is decided by comparing that annotation against the re-derived hash, never by a DeepEqual against the live Pod; the hash inputs and the drift path are under [Spec change handling](change-propagation.md#spec-change-handling). A missing Pod in a Pod-bearing phase and a terminal Pod are both recreate triggers, event-driven through the owned-Pod watch; see [Involuntary Pod disruption](agent-lifecycle.md#involuntary-pod-disruption). Crash-loop detection reads `containerStatuses[].restartCount` and `CrashLoopBackOff`, not Pod phase.
- **Service.** Created only when `spec.service.enabled`: ClusterIP, named `{agentName}`, with `port: spec.service.port` (default 8080) and `targetPort` the literal port the controller injects as `KAALM_HEALTH_PORT`. The two are decoupled so a developer can change the Service-facing port without changing the in-Pod listen port. Port drift is converged in place.
- **ServiceAccount.** Named `agent-{agentName}`, with no RoleBindings ([Agent Pod ServiceAccount](../security/rbac.md#agent-pod-serviceaccount)), created before the Pod.
- **PVC.** Created when persistence is enabled and no `existingClaim` is set; see [Child resources](../runtime/child-resources.md).
- **NetworkPolicy.** One per Agent, converged in place, combining: (a) egress to the gateway Service on 8443; (b) DNS egress to `kube-system` on UDP and TCP 53, a fixed rule (the chart value `controller.networkPolicy.dnsSelector` is accepted but not applied, see [Deployment](../operations/deployment.md)); (c) ingress from the gateway on `KAALM_HEALTH_PORT`; (d) `AgentClass.spec.network.egress.allowedCIDRs` as `ipBlock` rules; (e) ingress from the namespace's other agent Pods on `KAALM_HEALTH_PORT`, only when `allowSameNamespaceIngress: true`. `allowedHosts` is never synthesized; see AgentClassReconciler step 2.

There is no per-Agent configuration ConfigMap. Non-sensitive config is delivered as the env vars injected at Pod creation, and config changes are Pod-replacing spec drift by design; repointing `spec.handler` is spec drift too ([Handler update semantics](../runtime/base-images.md#handler-update-semantics)).

### Injected environment and probes

| Variable | When | Value |
|---|---|---|
| `KAALM_HEALTH_PORT` | always | the port the agent serves its HTTPS health and message endpoint on (default 8080) |
| `KAALM_GATEWAY_ENDPOINT` | always | the HTTPS URL of the gateway Service in the operator namespace on 8443, the base for every agent-to-gateway call, injected whether or not `spec.providers` is set |
| `KAALM_OPERATOR_NAMESPACE` | always | the namespace the gateway runs in, which is how the container builds the gateway Service DNS it accepts on `POST /v1/message` ([The runtime contract](../runtime/contract.md) item 4) |
| `KAALM_CA_CERT` | always | the path of the Kaalm CA bundle projected into the Pod |
| `KAALM_TLS_CERT`, `KAALM_TLS_KEY` | always | the paths of the agent's certificate and key from step 7 |
| `KAALM_HANDLER_PATH` | when `spec.handler` is set | `/opt/kaalm/handler`, where the handler ConfigMap is mounted read-only; absent otherwise, which is how a base image knows to serve its default handler |
| `KAALM_MEMORY_DIR` | when `spec.persistence.enabled` is set | `spec.persistence.mountPath`, the directory the PVC is mounted at (default `/var/agent/memory`), so state the runtime writes lands on the volume under any mount path; absent otherwise, and the runtime keeps its own default |

The handler mount sits outside `/var/run/kaalm/`, which belongs to the projected TLS volume and its rotation watch. The controller also injects liveness and readiness probes with `httpGet.scheme: HTTPS` on `GET /livez` and `GET /readyz` at `KAALM_HEALTH_PORT`, the paths pinned by [The runtime contract](../runtime/contract.md) item 1; Kubernetes httpGet probes do not verify TLS certificates, so the probe needs no CA.

### Activity fan-out

The activity read is cached per namespace for a fixed 15-second window, a controller constant: the first reconcile of any Agent in a namespace within the window fans out to every gateway Pod IP, and every other reconcile in that namespace reads the cache. That turns the load from one call per Agent per replica into one per namespace per replica per window. The window is well below any practical `idleTimeout` (the chart's `standard` class ships 30 minutes), so staleness cannot delay a transition. The fan-out, the merge, and the decision are specified under [Multi-replica fan-out](../gateways/user/activation-and-activity.md#multi-replica-fan-out) and [Activity detection](hibernation-and-wake.md#activity-detection).

## AgentTaskReconciler

The reconciler implements the [AgentTask lifecycle](task-lifecycle.md). One pass:

1. **Phase bookkeeping.** A task with no phase becomes `Pending`. A `Failed` task with no `completionTime` is a retry interrupted mid-flight and resumes at `Provisioning`. A terminal task (`Succeeded`, `Failed` with `completionTime`, `TimedOut`) goes to the TTL path: past `ttlSecondsAfterFinished` it is set `Terminating` and deleted, otherwise the pass requeues for the remaining time.
2. **Guards.** The operator namespace sets `Ready=False, reason=SystemNamespaceForbidden`; a missing AgentClass sets `Ready=False, reason=InvalidReference`. Neither is terminal.
3. **Pre-Pod checks**, run only while no Pod exists and the phase is `Pending` or `Provisioning`, so a retry is validated against the class as it now stands. A class-versus-spec violation under rules 2, 4, 5, 24, and 35 to 38 settles the task as terminal `Failed` with the violation's reason (`ClassConstraintViolation`, `PersistenceNotAllowed`, or `ToolNotInCatalog`), since AgentTask has no `Degraded` phase. A missing `imagePullSecrets` entry, read under the task's pull-Secret Role as for an Agent, sets `Ready=False, reason=ImagePullSecretMissing` and requeues in 30 seconds, and an empty image or a malformed class `allowedCIDRs` entry (rule 19) sets `Ready=False, reason=InvalidReference`; none is terminal.
4. **Certificate.** Ensure the per-task Certificate and hold in `Provisioning` with `Ready=False, reason=CertificateNotReady`, requeued every five seconds, until it is Ready. See [AgentTask certificate](#agenttask-certificate).
5. **Children.** Converge the ServiceAccount, NetworkPolicy, the PVC when persistence is enabled, and, for `agentReported` tasks only, the completion mailbox with its Role and RoleBinding. See [Completion mailbox and per-task Role](#completion-mailbox-and-per-task-role).
6. **Pod.** Create the Pod with `restartPolicy: Never`, the same injected environment as an Agent and no probes, and stamp `status.currentPodUID` from the Create response in the same status write for `agentReported` tasks. See [Task child-resource convergence](#task-child-resource-convergence).
7. **Drive the lifecycle.** Readiness, the provisioning deadline, the completion mailbox, Pod loss, timeouts, retries, and settlement are specified on [AgentTask lifecycle](task-lifecycle.md).

On an AgentClass change the reconciler does not disturb a task that has a Pod; the new invariants apply at the next Pod creation, which is a retry or a new task. See [AgentTask handling](change-propagation.md#agenttask-handling-no-degraded-phase).

### AgentTask certificate

Named `{taskName}-tls` in the task's namespace, owner-referenced to the AgentTask. It differs from the Agent certificate in two fields: `spec.dnsNames` is the single SAN `{taskName}.{namespace}.task.kaalm.io`, a non-Service pattern the gateway's SAN parser recognizes as an AgentTask identity ([Workload identity](../gateways/llm/workload-identity.md)), and `spec.usages` is `client auth` only, since tasks have no inbound TLS listener. Issuer, secret name pattern, duration, and renewal match the Agent's. Pod creation waits on the Certificate for the same reason as for Agents, and for a task the wait also keeps a slow issuance from counting against `backoffLimit`.

### Completion mailbox and per-task Role

For `agentReported` tasks the reconciler pre-creates the empty `{taskName}-completion` ConfigMap in the task's namespace with `data: {}`, owned by the AgentTask, then ensures a per-task `Role` and `RoleBinding` granting the gateway ServiceAccount (`kaalm-system/kaalm-gateway`) `update, patch` on that one name (`resourceNames: ["{taskName}-completion"]`). The Role and RoleBinding are owned by the AgentTask too.

The verb set is `update, patch` and not `create` because RBAC `resourceNames` does not constrain `create`: granting it would widen the gateway's access to every ConfigMap in the namespace. Pre-creating the resource here and granting only name-scoped mutate verbs makes the scoping enforceable, the same pattern as the per-channel Role below; see [Gateway ServiceAccount](../security/rbac.md#gateway-serviceaccount-permissions). `exitCode` tasks have no completion endpoint and skip the mailbox, the Role, and the UID stamp.

### Task child-resource convergence

Task Pods are created with `restartPolicy: Never`: the reconciler performs retries through `backoffLimit`, so a kubelet in-place restart would bypass `status.retries` and blur the one-run-per-`currentPodUID` gate, and `exitCode` completion depends on the Pod phase reaching `Succeeded` or `Failed`, which it never does under `Always` or `OnFailure`. No liveness or readiness probe is injected: tasks have no Service, and liveness is governed by `spec.completion.timeout` and the provisioning deadline.

The injected environment is the Agent's set; `KAALM_GATEWAY_ENDPOINT` is always present so a task can call [POST /v1/task/complete](../gateways/api/task-complete.md) even when it makes no LLM calls, while heartbeats are Agent-only ([The runtime contract](../runtime/contract.md) item 5). The ServiceAccount is `task-{taskName}` with no bindings. The NetworkPolicy is the Agent's without the gateway ingress rule, since a task is not a delivery target: `policyTypes: [Ingress, Egress]` with an explicit `ingress: []`, and the same egress rules (gateway on 8443, DNS, `allowedCIDRs`).

## AgentChannelReconciler

The reconciler creates no Pods. It validates the channel, scopes credential access, reduces the gateway's health and the bound Agent's phase into status, and prunes the channel's async records. One pass:

1. **Guard.** A channel in the operator namespace sets `Ready=False, reason=SystemNamespaceForbidden` and ends the pass.
2. **Agent.** Resolve `agentRef` in the channel's namespace; it must name an Agent, not an AgentTask, since tasks have no stable Service. A missing Agent sets `status.phase: Failed` and `Ready=False, reason=AgentNotFound`, and the pass ends.
3. **Validation**, in order, the first failure setting `Ready=False` with its reason: the Agent must have `spec.service.enabled: true` (`AgentServiceDisabled`); the path for the channel's type must be set and begin with `/channels/{namespace}/` (`InvalidPath`, [rule 15](../resources/validation-and-defaulting.md#cross-resource-validation)); no older channel in the namespace may register the same path, older by `creationTimestamp` and then by name (`PathConflict`, rule 15); the per-channel credential Role must exist ([Per-channel credential Role](#per-channel-credential-role)); every referenced Secret and key must exist (`CredentialsMissing`) and a Discord `publicKey` must decode to 32 bytes of hex (`CredentialsInvalid`); and a `callbackUrl`, when set, must pass the callback policy of rule 22 (`InvalidCallbackUrl`), where a host that does not resolve is not a failure. A failed validation still reduces the phase and requeues in one minute.
4. **Ready.** Set `Ready=True, reason=AgentReachable`.
5. **Channel health.** Reduce the gateway replicas' health reports into `status.conditions[type=PlatformConnected]`; see [Channel health poll](#channel-health-poll).
6. **Phase.** Reduce the Agent's phase into `status.phase`; see [Channel phase reduction](#channel-phase-reduction).
7. **Prune.** Delete this channel's expired async records; see [Async ConfigMap pruning](#async-configmap-pruning).

Status is written only when the pass changed it, and every pass requeues in one minute. The reconciler watches no Secrets, since its Secret access is scoped per channel, so a credential fixed in place is noticed by a later pass, not by an event. The gateway's side of a channel, from intake to delivery, is under [Request flow](../gateways/user/overview.md#request-flow).

### Per-channel credential Role

The Role is named `kaalm-channel-{channelName}-creds` and lives in the channel's namespace. It must exist before the reconciler reads any Secret, because neither the reconciler nor the gateway has blanket Secret-read RBAC in user namespaces: the Role is what gives both access to the specific Secrets.

The Role grants `get, watch`, `resourceNames`-scoped to every Secret the channel references. `list` is omitted because `resourceNames` cannot constrain a plain list request. The scoped Secrets are the inbound `spec.webhook.auth` Secret (`secretRef.name` for `bearer`, `hmac.secretRef.name` for `hmac`), the `spec.webhook.callbackAuth` Secret when `callbackUrl` is set, or, for a platform channel, the single `spec.discord.credentialsRef` or `spec.whatsapp.credentialsRef` Secret. A Secret referenced twice is listed once.

Two RoleBindings bind it, to the gateway ServiceAccount (`kaalm-system/kaalm-gateway`) and to the controller ServiceAccount (`kaalm-system/kaalm-controller`). When the desired `resourceNames` set changes, the reconciler updates the Role so neither retains access to a Secret it no longer needs. Role and bindings are read from the informer before any write, and all three are owned by the channel and cascade-delete with it. The Secret checks that follow are the runtime half of [rule 25](../resources/validation-and-defaulting.md#cross-resource-validation) and the platform key sets of rule 40; an unusable inbound or platform Secret reports `CredentialsMissing`, and an unusable `callbackAuth` Secret reports `CallbackAuthMissing` (the Secret or key does not exist) or `CallbackAuthInvalid` (the key is empty, or the block names no Secret for its type).

### Channel health poll

The reconciler fans `GET /v1/channels/health?namespace={ns}` out to every gateway Pod IP in parallel over mTLS with the controller's client certificate, skips unreachable replicas, and caches the result per namespace for the same fixed 15-second window as the activity read, so a burst of channel reconciles in one namespace produces one fan-out. The endpoint is specified under [GET /v1/channels/health](../gateways/api/internal-endpoints.md#get-v1channelshealth), and the four-rule reduction into `PlatformConnected` under [How the controller reduces it](../gateways/user/platform-adapters.md#how-the-controller-reduces-it).

### Channel phase reduction

`Active` and `Degraded` are a memoryless reduction of the bound Agent's phase, recomputed on every pass; `Failed` means `agentRef` does not resolve, and `Terminating` is set once by the delete path. The phase is one of three separate axes: `Ready` reports validation, `PlatformConnected` reports delivery health, and the phase reports the Agent.

![AgentChannel phase state machine, one trigger per edge. The initial pseudo-state enters Active when the Agent resolves and Failed when the Agent is not found. Active moves to Degraded when the Agent is Failed or Degraded, and Degraded back to Active when the Agent recovers. Active and Degraded move to Failed when the Agent is deleted, and Failed returns to Active when the Agent is recreated. Any phase moves to Terminating on deletion.](../diagrams/agentchannel-phase-reduction.svg)

| Channel phase | When |
|---|---|
| `Failed` | `agentRef` does not resolve. Recovers when the Agent is created. |
| `Degraded` | the Agent is `Failed` or `Degraded` |
| `Active` | every other Agent phase, including `Pending`, `Provisioning`, `Hibernating`, `Hibernated`, `Resuming`, `Terminating`, and an Agent whose phase is unset. Transient unavailability surfaces through `PlatformConnected`, not the phase. |
| `Terminating` | set by this reconciler's delete path before the [delete handshake](finalizers.md#agentchannel) |

The gateway gates routing admission on `Ready` and observes Agent availability through delivery outcomes; the phase exists for `kubectl describe`.

### Async ConfigMap pruning

The reconciler lists the `kaalm-async-*` ConfigMaps in `kaalm-system` carrying this channel's labels (`kaalm.io/channel-namespace`, `kaalm.io/channel-name`) and deletes those whose `kaalm.io/expires-at` annotation is in the past; a record with a missing or unparseable annotation is left alone. The 1-hour TTL is enforced here because a cross-namespace ownerRef cannot express the linkage; see [Response persistence](../gateways/api/async-responses.md#response-persistence). Pruning runs on every pass, so a record lingers at most one requeue interval past its expiry. The delete-time finalizer sweeps every record of the channel, expired or not; see [Finalizers](finalizers.md#agentchannel).
