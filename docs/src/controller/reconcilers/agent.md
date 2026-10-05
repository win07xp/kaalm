# AgentReconciler

## What it's for

The AgentReconciler provisions a persistent Agent and implements its [Agent lifecycle](../agent-lifecycle.md). It checks the Agent against its AgentClass and providers, creates the Certificate, children, and Pod, replaces the Pod on spec drift, derives the phase from the Pod, handles [wake requests](../hibernation-and-wake.md#wake-trigger), and reads gateway activity to move the Agent between `Running`, `Idle`, and `Hibernating`. It also reports provider health and budget exhaustion as conditions. On delete it terminates the Pod, applies the class's `pvcRetention`, and releases the [finalizer](../finalizers.md#agent).

## What it owns and watches

The reconciler creates these objects in the Agent's namespace, each with a controller `ownerReference` to the Agent: the Certificate, ServiceAccount, Service, PVC, NetworkPolicy, CiliumNetworkPolicy, and Pod. It also creates a Role and RoleBinding for the class's pull Secrets and another pair for the Secrets that `spec.env` reads, so the operator can read those Secrets before the Pod exists. [Child resources](../../runtime/child-resources.md#agent-child-resources) gives each child's name and the condition for it to exist, says what happens to it on hibernation and deletion, and draws which object each ownerRef names.

It reads the AgentClass, the ModelProviders and ToolProviders the Agent references, the Secrets, PVC, and ConfigMap that the [ready gates](#ready-gates) name, and the gateway's activity data. What re-runs an Agent is listed under [What each reconciler watches](../reconcilers.md#what-each-reconciler-watches).

### Agent certificate

The Certificate is named `{agentName}-tls` in the Agent's namespace and owned by the Agent, so it is garbage-collected on Agent deletion.

| Field | Value |
|---|---|
| `spec.issuerRef` | `{ name: "kaalm-ca-issuer", kind: "ClusterIssuer" }` |
| `spec.secretName` | `{agentName}-tls-{uid}`, the output Secret cert-manager creates in the Agent's namespace. `{uid}` is the first eight characters of the Agent's `metadata.uid`, so the Secret for `support-assistant` is named like `support-assistant-tls-3f9c2a1e`. |
| `spec.dnsNames` | `{agentName}.{namespace}.svc.cluster.local`, `{agentName}.{namespace}.svc`, `{agentName}.{namespace}` |
| `spec.duration`, `spec.renewBefore` | The chart values `controller.certificate.duration` and `controller.certificate.renewBefore`; the defaults are under [Rotation defaults](../../security/tls.md#rotation-defaults) |
| `spec.usages` | `server auth`, `client auth`: the same cert is the agent's serving cert and its mTLS client cert |

The Secret name depends only on the Agent's name and UID, so it is the same on every pass. An Agent deleted and re-created with the same name gets a different Secret name. An AgentTask's Certificate follows the same rule ([AgentTask certificate](agenttask.md#agenttask-certificate)).

The Pod mounts the Secret that the Certificate's `spec.secretName` names, which the reconciler reads from the Certificate on every pass before it creates a Pod. It never updates an existing Certificate, so a Certificate that carries another `secretName` keeps it, and so do the Pods created from it, including after a wake or a drift replacement. A running Agent Pod whose TLS volume names a different Secret than the Certificate is replaced through the drift cap ([A Pod that mounts another TLS Secret is replaced](../change-propagation.md#a-pod-that-mounts-another-tls-secret-is-replaced)). A running AgentTask Pod is not compared, and a retry's Pod mounts the name the Certificate holds at that time.

### Child-resource convergence

The NetworkPolicy's rules are the table under [What the synthesized NetworkPolicy protects](../../runtime/child-resources.md#what-the-synthesized-networkpolicy-protects). [Spec change handling](../change-propagation.md#spec-change-handling) covers which children a change converges in place and which it replaces.

The CiliumNetworkPolicy is read from the apiserver and not watched, because the kind exists only on Cilium clusters, so the reconciler notices a change to it on the Agent's next pass. On a cluster whose CNI probe answers `False`, it never reads or writes the kind ([FQDN egress policy](../../runtime/child-resources.md#fqdn-egress-policy)).

### Child ownership

Children are found by a predictable name, so a workload name could otherwise steer the controller onto an object it does not control, such as a platform NetworkPolicy. Before a reconciler updates or deletes a child, or treats an existing child as already converged, it checks that the child's controller `ownerReference` points at the workload; a create that fails with `AlreadyExists` reads the object and applies the same check. This covers every child of an Agent, the Certificate, the pull-Secret and env-Secret Roles and RoleBindings, and the [AgentTask children](agenttask.md#task-child-resource-convergence), including the [completion mailbox](agenttask.md#completion-mailbox-and-per-task-role).

A child that fails the check is never written or deleted. The workload gets `Ready=False, reason=ChildConflict`, whose message names the child's kind and name, a `Warning` event with the same reason when the reason or message first appears, and a requeue every 30 seconds, since the conflicting object carries no owner reference to this workload and so raises no watch event for it. The pass ends before the Pod is converged, so no Pod is created, and a Pod that is already running is left in place. Deleting the conflicting object lets the next pass create the child and continue. When the object belongs to an Agent or AgentTask of the same name, delete one of the two workloads instead; see [An Agent and an AgentTask cannot share a name](../../runtime/child-resources.md#an-agent-and-an-agenttask-cannot-share-a-name).

One case is not a conflict: a per-Agent PVC with no controller at all, which is how `pvcRetention: Retain` leaves it when its Agent is deleted, is reused as it is by a new Agent of the same name, the successor the policy keeps it for. Mounting it grants nothing that `spec.persistence.existingClaim` does not.

### Injected environment and probes

| Variable | When | Value |
|---|---|---|
| `KAALM_HEALTH_PORT` | always | The port the agent serves its HTTPS health and message endpoint on (default 8080) |
| `KAALM_GATEWAY_ENDPOINT` | always | The HTTPS URL of the gateway Service in the operator namespace on 8443, the base for every agent-to-gateway call. Injected whether or not `spec.providers` is set |
| `KAALM_OPERATOR_NAMESPACE` | always | The namespace the gateway runs in, from which the container builds the gateway Service DNS names it accepts on `POST /v1/message` ([The runtime contract](../../runtime/contract.md) item 4) |
| `KAALM_CA_CERT` | always | The path of the Kaalm CA bundle projected into the Pod |
| `KAALM_TLS_CERT`, `KAALM_TLS_KEY` | always | The paths of the agent's certificate and key from the [Agent certificate](#agent-certificate) |
| `KAALM_HANDLER_PATH` | when `spec.handler` is set | `/opt/kaalm/handler`, where the handler ConfigMap is mounted read-only; absent otherwise, which is how a base image knows to serve its default handler |
| `KAALM_MEMORY_DIR` | when `spec.persistence.enabled` is set | `spec.persistence.mountPath` (default `/var/agent/memory`), the directory the PVC is mounted at, so the state the runtime writes lands on the volume under any mount path; absent otherwise, and the runtime keeps its own default |

The controller also injects liveness and readiness probes over HTTPS on `GET /livez` and `GET /readyz` at `KAALM_HEALTH_PORT`, the paths pinned by [The runtime contract](../../runtime/contract.md) item 1.

## What it checks

The first failing check in the following table sets `Ready=False`, or the phase `Degraded`, with its reason code, and the later checks do not run. A `Hibernating` or `Hibernated` Agent is checked through the cross-checks only.

| Check | Reason when it fails | Rule |
|---|---|---|
| The Agent is not in the operator namespace | `SystemNamespaceForbidden` | [28](../../resources/validation/names-and-tasks.md) |
| `agentClassRef` names an AgentClass | `InvalidReference` | [1](../../resources/validation/references-and-access.md) |
| The Agent matches its class and providers: namespace, image, providers, tools, persistence, hibernation, and handler mount | Phase `Degraded` with the first violation's reason; rule 47 (`NamespaceNotAllowed`) is checked first, so it is the reported reason when several checks fail. The reasons are under [Degraded](../agent-lifecycle.md#degraded) | [2 to 5, 24, 26, 29, 30, 35 to 38, and 47](../../resources/validation-and-defaulting.md#where-each-rule-is-enforced) |
| The class's `allowedCIDRs` entries are well-formed | `InvalidReference` | [19](../../resources/validation/class-policy.md) |
| The Agent or the class sets an image | `InvalidReference` | |
| `existingClaim` names a PVC in the Agent's namespace | `ExistingClaimNotFound` | [27](../../resources/validation/references-and-access.md) |
| Each `imagePullSecrets` entry of the class exists in the Agent's namespace | `ImagePullSecretMissing` | [23](../../resources/validation/references-and-access.md) |
| Each Secret that `spec.env` reads exists and carries the opt-in label | `SecretNotOptedIn`, or `InvalidReference` for a `secretKeyRef` with no name | [48](../../resources/validation/references-and-access.md) |
| The handler ConfigMap exists in the Agent's namespace | `HandlerConfigMapNotFound` | [31](../../resources/validation/references-and-access.md) |
| The Certificate is Ready | `CertificateNotReady`; a `Pending` Agent moves to `Provisioning` | |
| Every child's name is free or controlled by the Agent, the Roles and the Certificate included | `ChildConflict`; see [Child ownership](#child-ownership) | |

The `allowedCIDRs` through handler ConfigMap rows are the [ready gates](#ready-gates). The `ChildConflict` check has no single place in the order: the pass checks each Role before the gate that reads its Secrets, and the Certificate before it checks readiness, so a conflict there is reported ahead of the later gates, and it checks the other children after the Certificate. The figure shows one whole pass, including the Pod and activity work that follows the checks; its step numbers belong to the figure.

![Flowchart of one AgentReconciler pass as three rows of cascades. Steps 1 to 4: kaalm.io/wake=true on an Agent that is not Hibernating ends the pass (Hibernated goes to Resuming, any other phase removes it with WakeIgnored); a leftover wake-trigger annotation with no wake request ends the pass, removing it and requeueing; the kaalm-system namespace ends it with Ready=False SystemNamespaceForbidden; a missing AgentClass with Ready=False InvalidReference; then the Degraded condition is set from budget state; a failed cross-check ends the pass with phase=Degraded; a Degraded Agent with every check clear restores preDegradedPhase. Steps 5 to 7: Hibernating or Hibernated drives or holds and ends the pass; a malformed class allowedCIDRs entry, a missing image, existingClaim, pull Secret, or handler ConfigMap, or an env Secret that is missing or lacks the workload label, sets Ready=False, requeueing in 30s for an unwatched object; a Certificate that is not Ready moves a Pending Agent to Provisioning and requeues in 5s. Steps 8 to 11: converge the ServiceAccount, Service, PVC, and NetworkPolicy; then the Pod: none creates it and sets Provisioning, and a create the API server rejects also sets Ready=False PodCreateRejected and requeues in 30s; a terminal Pod is deleted and replaced in Provisioning, and so is a Pod with spec drift or a TLS Secret mismatch when a maxUnavailableOnDrift slot is free; spec drift or a TLS Secret mismatch with no free slot sets PodUpToDate ReplacementPending and keeps the current Pod; a crash loop or ImagePullBackOff sets Failed; a Ready Pod sets Running or leaves Idle, otherwise Provisioning, with a woken Agent in Resuming instead of Provisioning on each of those branches; evaluate activity for Running or Idle; write status if changed.](../../diagrams/agent-reconcile-pass.svg)

### Ready gates

The `allowedCIDRs` and image gates carry no requeue, because a fix to the class or the Agent re-enqueues the Agent. The other four requeue on a timer ([Timing](#timing)). Before it reads the pull Secrets and the env Secrets, the reconciler ensures the per-workload Role for each, because the operator holds no standing read of Secrets in user namespaces ([Operator ServiceAccount](../../security/rbac.md#operator-serviceaccount)).

While any gate holds, the pass ends before the Pod is converged and before activity is evaluated. A running Pod stays in place and is not replaced, the Agent makes no `Running` to `Idle` or `Idle` to `Hibernating` transition, and a crash-looping Pod does not set `Failed`. [Rule 48](../../resources/validation/references-and-access.md) states what this means for a Secret that loses its label.

## What it reports

- **`status.phase`** follows the [Agent lifecycle](../agent-lifecycle.md), which lists each transition and its trigger. The reconciler derives `Provisioning`, `Running`, and `Failed` from the Pod.
- **`Ready`** is `True` with `reason: PodRunning` when the Pod is Ready, and `False` with a reason from [What it checks](#what-it-checks) or from the Pod. [Agent status](../../resources/agent.md#status) lists the Pod-derived reasons. When the API server refuses the Pod create, the Pod step sets `False` with `PodCreateRejected` and an empty `status.podName` ([Error handling](../operations.md#error-handling) says which errors count).
- **Conditions that leave the phase unchanged.** `ProvidersReady` is `True` with `AllProvidersHealthy`, or `False` with `ClassConstraintViolation` for a provider outside the class allowlist or missing, or `ProviderUnhealthy` for one that is not `Ready`. `Degraded` carries `BudgetExhausted` while a referenced provider reports the namespace budget-blocked; it is set before the cross-checks run, so an Agent in phase `Degraded` still shows it ([Error handling](../operations.md#error-handling)). `PodUpToDate` reports a drift replacement as `Replacing` or `ReplacementPending`, and `Current` otherwise ([Drift replacements are capped per class](../change-propagation.md#drift-replacements-are-capped-per-class)). `IdleDetection` is `False` with `Disabled` while the effective `idleTimeout` is zero. `GatewayReachable` exists only while the activity read evaluates the Agent ([Gateway unavailability](../hibernation-and-wake.md#when-activity-data-is-missing)).
- **Events.** A failed check raises a `Warning` event with its reason, once when the reason first appears on `Ready`, or once on entry to `Degraded`; the Certificate wait raises none. An Agent whose `spec.resources` sets `claims` raises an advisory `ResourceClaimsIgnored` Warning without changing `Ready` ([rule 53](../../resources/validation/class-policy.md)). Each phase change raises a `PhaseChanged` event. [Event emission](../operations.md#event-emission) lists every Agent event.

## Timing

- **A Secret, PVC, or ConfigMap that appears, or a Secret that gets the label.** It shows within 30 seconds, because the reconciler does not watch these objects and the gate requeues every 30 seconds. An edit to the Agent or its class re-runs the pass at once.
- **A Certificate that is still issuing.** The pass requeues every 5 seconds, and the Certificate turning Ready re-runs it at once, so the Pod follows within 5 seconds.
- **A `ChildConflict`.** It re-checks every 30 seconds, because the conflicting object raises no watch event.
- **A check with no timed re-check.** `SystemNamespaceForbidden` has none, and a missing AgentClass clears when the class is created.
- **A change to the class, a provider, or a tool provider.** It reaches the Agent at once ([AgentClass change handling](../change-propagation.md#agentclass-change-handling)). A `Degraded` Agent returns to its prior phase on the next pass after every mismatch clears.
- **The Pod.** Its deletion or turning Ready re-runs the pass at once through the owned-Pod watch.
- **A rejected Pod create.** It re-checks every 30 seconds, because a RuntimeClass, quota, or webhook raises no watch event, so a fixed cause takes effect within 30 seconds. An edit to the Agent or its class retries at once.
- **A drifted Agent waiting for a slot.** It re-runs as soon as another Agent of the class frees a slot, and within 30 seconds otherwise.
- **Idle and hibernation.** A `Running` or `Idle` Agent with a nonzero effective `idleTimeout` re-runs every 15 seconds, and each pass reads activity data up to 15 seconds old, so a transition can lag its trigger by up to about 30 seconds (the per-namespace cache is specified under [Multi-replica fan-out](../../gateways/user/activation-and-activity.md#multi-replica-fan-out)). While no gateway replica answers, the phase is kept and the requeue backs off from 30 seconds to 5 minutes, and a gateway Pod turning Ready re-runs the Agent at once. While a restarted gateway has no record of the Agent, the pass re-checks every 30 seconds ([Gateway unavailability](../hibernation-and-wake.md#when-activity-data-is-missing)).
- **A wake request.** It shows at once. A request that lands while the Agent is `Hibernating` waits until the Pod is gone ([Manual wake](../hibernation-and-wake.md#manual-wake)).

A failed apiserver call, other than a rejected Pod create, returns the error and retries with backoff. The cadence of every reconciler is under [Reconcile interval and performance](../overview.md#reconcile-interval-and-performance).

## Design choices

- **The gates run before the Certificate and the children**, so a missing dependency shows as a condition and not as a Pod wedged in `ImagePullBackOff` or `ContainerCreating`, and a malformed class `allowedCIDRs` entry shows as a condition and not as a NetworkPolicy write the apiserver rejects on every pass.
- **Pod creation waits for the Certificate**, so the Pod never hangs on its projected Secret mount.
- **The issuer is a `ClusterIssuer`**, because cert-manager does not resolve a namespaced `Issuer` across namespaces ([Trust chain](../../security/tls.md#trust-chain)).
