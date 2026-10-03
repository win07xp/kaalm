# AgentTaskReconciler

## What it's for

The AgentTaskReconciler runs one [AgentTask](../../resources/agenttask.md) to completion. It validates the task against its class, creates the Certificate, children, and Pod, drives the task through the [AgentTask lifecycle](../task-lifecycle.md) to a terminal phase, and deletes the settled task when its TTL expires. On delete, the [finalizer](../finalizers.md#agenttask) deletes the Pod, waits for it to go, and releases; cascade garbage collection removes the other children.

## What it owns and watches

Every object the reconciler creates carries a controller ownerRef to the AgentTask: the Certificate, ServiceAccount, NetworkPolicy, Pod, a PVC when persistence is enabled, a CiliumNetworkPolicy when the class lists `allowedHosts` and the CNI supports FQDN policies, and, for `agentReported` tasks, the completion ConfigMap with its Role and RoleBinding. Two more Roles let the controller read the pull Secrets and env Secrets ([Operator ServiceAccount](../../security/rbac.md#operator-serviceaccount)). [AgentTask child resources](../../runtime/child-resources.md#agenttask-child-resources) lists each name and when it exists.

It reads the task's AgentClass, ModelProviders, and ToolProviders, and watches its children and the AgentClass spec; see [What each reconciler watches](../reconcilers.md#what-each-reconciler-watches).

### AgentTask certificate

The Certificate `{taskName}-tls` differs from the [Agent certificate](agent.md#agent-certificate) in two fields: `spec.dnsNames` is the single task SAN ([Workload identity](../../gateways/llm/workload-identity.md)), and `spec.usages` is `client auth` only, since a task has no TLS listener. Until the Certificate is Ready, the task holds in `Provisioning` with `Ready=False, reason=CertificateNotReady` and has no Pod. The provisioning deadline starts at Pod creation, so a slow issuance never counts against `backoffLimit`.

### Completion mailbox and per-task Role

For `agentReported` tasks the reconciler pre-creates the empty `{taskName}-completion` ConfigMap, then a Role and RoleBinding that grant the gateway ServiceAccount (`kaalm-system/kaalm-gateway`) `update, patch` on that one name. The Role has no `create`, which `resourceNames` cannot scope, so pre-creating the ConfigMap is what makes the scoping enforceable ([Gateway ServiceAccount](../../security/rbac.md#gateway-serviceaccount-permissions)). `exitCode` tasks skip the mailbox and the Role and have no `status.currentPodUID`.

### Task child-resource convergence

After the Certificate is Ready, the reconciler converges the children, then creates the Pod with the Agent's injected environment ([Injected environment and probes](agent.md#injected-environment-and-probes)). The task's NetworkPolicy has the Agent's egress rules and no ingress allow rule ([AgentTask child resources](../../runtime/child-resources.md#agenttask-child-resources)).

## What it checks

The first failing check sets `Ready=False` with its reason code, and the later checks do not run.

| Check | Reason when it fails | Rule |
|---|---|---|
| The task is not in the operator namespace | `SystemNamespaceForbidden` | [28](../../resources/validation/names-and-tasks.md) |
| `agentClassRef` names an AgentClass | `InvalidReference` | [1](../../resources/validation/references-and-access.md) |
| The class admits the task's namespace | `NamespaceNotAllowed` | [47](../../resources/validation/class-policy.md) |
| The class allows the image, and each model provider exists, is allowed, and admits the namespace | `ClassConstraintViolation` | [2](../../resources/validation/class-policy.md), [3 to 5](../../resources/validation/references-and-access.md) |
| Each tool grant resolves, is allowed, and names cataloged tools | `ClassConstraintViolation`, `ToolNotInCatalog` | [35 to 38](../../resources/validation/references-and-access.md) |
| The class allows the task's persistence | `PersistenceNotAllowed` | [24](../../resources/validation/class-policy.md) |
| The pull-Secret Role can be written, and each Secret exists | `ChildConflict`, `ImagePullSecretMissing` | [23](../../resources/validation/references-and-access.md) |
| The env-Secret Role can be written, and each Secret exists with the workload label | `ChildConflict`, `SecretNotOptedIn`, or `InvalidReference` for a `secretKeyRef` with no name | [48](../../resources/validation/references-and-access.md) |
| The task or the class sets an image | `InvalidReference` | |
| The class `allowedCIDRs` entries are well-formed | `InvalidReference` | [19](../../resources/validation/class-policy.md) |
| The Certificate and child names are free or controlled by the task | `ChildConflict` | [Child ownership](agent.md#child-ownership) |

The first two checks run until the task settles. The others, except `ChildConflict`, run only while the task has no Pod and is `Pending` or `Provisioning`, so a retry is validated against the class as it now stands, a task with a Pod finishes under the class it started with ([AgentTask handling](../change-propagation.md#agenttask-handling-no-degraded-phase)), and a finished task needs no `kaalm.io/workload-secret` label on its env Secrets. A failure of rules 47, 2 to 5, 35 to 38, or 24 (`NamespaceNotAllowed`, `ClassConstraintViolation`, `ToolNotInCatalog`, `PersistenceNotAllowed`) settles the task `Failed`, because an AgentTask has no `Degraded` phase to recover from. Any other failed check keeps the phase and is not terminal.

## What it reports

- **`status.phase`** follows the [state machine](../task-lifecycle.md#state-machine).
- **`Ready`** is `True` with `PodRunning` once the Pod is Ready. Otherwise it is `False` with a reason from [What it checks](#what-it-checks), `CertificateNotReady`, `PodProvisioning`, `PodTerminating` while a retry waits for the old Pod to go, or the failure or settling reason.
- **`Completed`** is set on settling: `True` for `Succeeded`, `False` otherwise ([Status](../../resources/agenttask.md#status)).
- **Events** are listed under [Event reasons](../task-lifecycle.md#event-reasons).

## Timing

- **A completion, an `exitCode` Pod exit, a failed `agentReported` Pod, or Pod loss.** Each shows at once, because the reconciler watches the completion ConfigMap and the Pod. An `agentReported` Pod that exits 0 without reporting leaves the task `Running` until its timeout, or indefinitely when it has none ([The class bounds timeout and retention](../../resources/agenttask.md#the-class-bounds-timeout-and-retention)).
- **A running task.** The pass is requeued for the timeout deadline, measured from `status.startTime`.
- **Waiting on the Certificate, a terminating Pod, or a Pod that is not Ready.** The task re-checks every 5 seconds, and at once when the Certificate or the Pod changes. A Pod not Ready five minutes after creation fails the attempt with `ProvisioningDeadlineExceeded` at the next re-check, and the task retries while `backoffLimit` allows.
- **Waiting on a Secret or a conflicting object.** The task re-checks every 30 seconds, because neither raises an event.
- **A missing class, an empty image, or a malformed class CIDR.** No timed re-check: these clear when the task or the class changes. `SystemNamespaceForbidden` has no timed re-check and does not clear; create the task in another namespace.
- **A settled task.** The pass is requeued for the remaining TTL ([rule 43](../../resources/validation/class-policy.md)), then the task goes `Terminating` and is deleted. With no TTL, the task stays.

## Design choices

- **`restartPolicy: Never`**, so a crash is a counted retry, not a kubelet restart ([exitCode](../task-lifecycle.md#exitcode)).
- **No probes**, because a task has no Service; the completion timeout ([rule 42](../../resources/validation/class-policy.md)) and the provisioning deadline bound liveness.
