# AgentTaskReconciler

## What it's for

The AgentTaskReconciler runs one [AgentTask](../../resources/agenttask.md) to completion. It validates the task against its class, creates the Certificate, children, and Pod, drives the task through the [AgentTask lifecycle](../task-lifecycle.md) to a terminal phase, and deletes the settled task when its TTL expires. On delete, the [finalizer](../finalizers.md#agenttask) deletes the Pod, waits for it to go, and releases; cascade garbage collection removes the other children.

## What it owns and watches

Every object the reconciler creates carries a controller ownerRef to the AgentTask: the Certificate, ServiceAccount, NetworkPolicy, Pod, a PVC when persistence is enabled, a CiliumNetworkPolicy when the class lists `allowedHosts` and the CNI supports FQDN policies, and, for `agentReported` tasks, the completion ConfigMap with its Role and RoleBinding. Two more Roles let the controller read the pull Secrets and env Secrets ([Operator ServiceAccount](../../security/rbac.md#operator-serviceaccount)). [AgentTask child resources](../../runtime/child-resources.md#agenttask-child-resources) lists each name and when it exists.

It reads the task's AgentClass, ModelProviders, and ToolProviders, and watches its children and the AgentClass spec; see [What each reconciler watches](../reconcilers.md#what-each-reconciler-watches).

### AgentTask certificate

The Certificate `{taskName}-tls` differs from the [Agent certificate](agent.md#agent-certificate) in two fields: `spec.dnsNames` is the single task SAN ([Workload identity](../../gateways/llm/workload-identity.md)), and `spec.usages` is `client auth` only, since a task has no TLS listener. Until the Certificate is Ready, the task holds in `Provisioning` with `Ready=False, reason=CertificateNotReady` and has no Pod. The provisioning deadline starts at Pod creation, or at the first rejected Pod create or child write ([The clock starts at Ready](../task-lifecycle.md#the-clock-starts-at-ready)), so a slow issuance never counts against `backoffLimit`.

While the task has a Pod (`Provisioning` with a Pod, or `Running`), a deleted Certificate is re-created on the pass its deletion triggers, with the same `spec.secretName`, because that name depends only on the task's name and UID. cert-manager writes to the Secret the Pod already mounts, so renewals keep reaching a long-running task. The re-created Certificate does not hold the task: the phase and `Ready` stay as they are while cert-manager issues it, because the Pod keeps the certificate already in its volume. The reconciler does not compare the running Pod with the Certificate. If the deleted Certificate named another Secret, the Pod keeps its certificate until it expires or the task retries ([A Pod that mounts another TLS Secret is replaced](../change-propagation.md#a-pod-that-mounts-another-tls-secret-is-replaced)). A Certificate name held by another object is covered under [Task child-resource convergence](#task-child-resource-convergence).

### Completion mailbox and per-task Role

For `agentReported` tasks the reconciler pre-creates the empty `{taskName}-completion` ConfigMap, then a Role and RoleBinding that grant the gateway ServiceAccount (`kaalm-system/kaalm-gateway`) `update, patch` on that one name. The Role has no `create`, which `resourceNames` cannot scope, so pre-creating the ConfigMap is what makes the scoping enforceable ([Gateway ServiceAccount](../../security/rbac.md#gateway-serviceaccount-permissions)). `exitCode` tasks skip the mailbox and the Role and have no `status.currentPodUID`.

### Task child-resource convergence

After the Certificate is Ready, the reconciler converges the children, then creates the Pod with the Agent's injected environment ([Injected environment and probes](agent.md#injected-environment-and-probes)). The task's NetworkPolicy has the Agent's egress rules and no ingress allow rule ([AgentTask child resources](../../runtime/child-resources.md#agenttask-child-resources)).

While the task has a Pod (`Provisioning` with a Pod, or `Running`), the reconciler re-creates a deleted child and leaves the Pod running. This covers the NetworkPolicy, the ServiceAccount, the PVC the Pod mounts, and, for `agentReported` tasks, the completion ConfigMap, Role, and RoleBinding; the Certificate is covered under [AgentTask certificate](#agenttask-certificate). The NetworkPolicy is the running Pod's egress boundary, and the mailbox with its Role is how the task completes, so a deleted one comes back. The reconciler never updates or deletes a child that exists, so an edit to a child is not reverted.

- **Class edits.** A re-created child is built from the class as it now stands, not the class the Pod started under (the class-edit rule is under [AgentTask handling](../change-propagation.md#agenttask-handling-no-degraded-phase)).
- **The PVC.** It comes back only when the running Pod mounts it, so turning on `spec.persistence.enabled` mid-run adds no claim until the next attempt.
- **The ServiceAccount.** When the class sets `security.automountServiceAccountToken` ([AgentClass](../../resources/agentclass.md#automountserviceaccounttoken-is-the-opt-in-for-api-access)), the running Pod's current API token is bound to the deleted ServiceAccount. The re-created ServiceAccount has a new UID, so the API server refuses that token, and the Pod's Kubernetes API calls fail until the kubelet next refreshes its projected token. The kubelet requests the new token by ServiceAccount name, so the refresh needs no retry. A retry's Pod also gets a working token.
- **The CiliumNetworkPolicy.** It is re-created only on `Running` passes, only when it is missing and the class lists `allowedHosts`, and it is never updated or deleted while the task has a Pod. The reconciler reads the kind live and does not watch it ([Child-resource convergence](agent.md#child-resource-convergence)).

A child name held by an object the task does not control, the Certificate included, gives `Ready=False, reason=ChildConflict` while the task has a Pod too. The same holds for `ChildWriteRejected` ([A rejected child write](../operations.md#a-rejected-child-write)). The task keeps running: completion, timeout, and Pod loss are still acted on, and a running task's timeout deadline is still kept. `Ready` returns to `PodRunning` or `PodProvisioning` once the cause is gone. A conflict on one child does not stop the others from being re-created on the same pass. [Child ownership](agent.md#child-ownership) covers the check, the event, and the 30-second re-check.

## What it checks

The first failing check sets `Ready=False` with its reason code, and the later checks do not run. With a Pod, the child check runs after the completion, exit, timeout, and Pod-loss checks, so it never stops the task from finishing or retrying ([Task child-resource convergence](#task-child-resource-convergence)).

| Check | Reason when it fails | Rule |
|---|---|---|
| The task is not in the operator namespace | `SystemNamespaceForbidden` | [28](../../resources/validation/names-and-tasks.md) |
| `agentClassRef` names an AgentClass | `InvalidReference` | [1](../../resources/validation/references-and-access.md) |
| The class admits the task's namespace | `NamespaceNotAllowed` | [47](../../resources/validation/class-policy.md) |
| The class allows the image, and each model provider exists, is allowed, and admits the namespace | `ClassConstraintViolation` | [2](../../resources/validation/class-policy.md), [3 to 5](../../resources/validation/references-and-access.md) |
| Each tool grant resolves, is allowed, and names cataloged tools | `ClassConstraintViolation`, `ToolNotInCatalog` | [35 to 38](../../resources/validation/references-and-access.md) |
| The class allows the task's persistence | `PersistenceNotAllowed` | [24](../../resources/validation/class-policy.md) |
| The pull-Secret Role can be written, and each Secret exists | `ChildConflict`, `ChildWriteRejected`, `ImagePullSecretMissing` | [23](../../resources/validation/references-and-access.md) |
| The env-Secret Role can be written, and each Secret exists with the workload label | `ChildConflict`, `ChildWriteRejected`, `SecretNotOptedIn`, or `InvalidReference` for a `secretKeyRef` with no name | [48](../../resources/validation/references-and-access.md) |
| The task or the class sets an image | `InvalidReference` | |
| The class `allowedCIDRs` entries are well-formed | `InvalidReference` | [19](../../resources/validation/class-policy.md) |
| The Certificate and child names are free or controlled by the task, and the API server accepts each child write | `ChildConflict`, `ChildWriteRejected` | [Child ownership](agent.md#child-ownership) |

The first two checks run until the task settles. The others run only while the task has no Pod and is `Pending` or `Provisioning`, except the last row, which also runs with a Pod. So a retry is validated against the class as it now stands, a task with a Pod finishes under the class it started with ([AgentTask handling](../change-propagation.md#agenttask-handling-no-degraded-phase)), and a finished task needs no `kaalm.io/workload-secret` label on its env Secrets. A failure of rules 47, 2 to 5, 35 to 38, or 24 (`NamespaceNotAllowed`, `ClassConstraintViolation`, `ToolNotInCatalog`, `PersistenceNotAllowed`) settles the task `Failed`, because an AgentTask has no `Degraded` phase to recover from. Any other failed check keeps the phase and is not terminal.

## What it reports

- **`status.phase`** follows the [state machine](../task-lifecycle.md#state-machine).
- **`Ready`** is `True` with `PodRunning` once the Pod is Ready. Otherwise it is `False` with a reason from [What it checks](#what-it-checks), `CertificateNotReady`, `PodProvisioning`, `PodCreateRejected` while the API server refuses the Pod create, `ChildWriteRejected` while it refuses a child write ([Error handling](../operations.md#error-handling) says which errors count), `PodTerminating` while a retry waits for the old Pod to go, or the failure or settling reason.
- **`Completed`** is set on settling: `True` for `Succeeded`, `False` otherwise ([Status](../../resources/agenttask.md#status)).
- **Events** are listed under [Event reasons](../task-lifecycle.md#event-reasons).

## Timing

- **A completion, an `exitCode` Pod exit, a failed `agentReported` Pod, or Pod loss.** Each shows at once, because the reconciler watches the completion ConfigMap and the Pod. An `agentReported` Pod that exits 0 without reporting leaves the task `Running` until its timeout, or indefinitely when it has none ([The class bounds timeout and retention](../../resources/agenttask.md#the-class-bounds-timeout-and-retention)).
- **A running task.** The pass is requeued for the timeout deadline, measured from `status.startTime`.
- **Waiting on the Certificate, a terminating Pod, or a Pod that is not Ready.** The task re-checks every 5 seconds, and at once when the Certificate or the Pod changes. A Pod not Ready five minutes after creation fails the attempt with `ProvisioningDeadlineExceeded` at the next re-check, and the task retries while `backoffLimit` allows.
- **Waiting on a Secret or a conflicting object.** The task re-checks every 30 seconds, because neither raises an event. A running task held this way is still requeued for its timeout deadline when that comes first, so a hold never pushes the timeout back. Before the Pod is Ready, a task with a Pod uses the same 30-second re-check, so the provisioning deadline can be checked up to 30 seconds late. The Pod turning Ready or failing still arrives at once.
- **A rejected Pod create or child write.** The task re-checks every 30 seconds, because a RuntimeClass, quota, or webhook raises no event, so a fixed cause shows within 30 seconds. While the task has no Pod, the attempt fails with `ProvisioningDeadlineExceeded` at the first re-check after five minutes, and the task retries while `backoffLimit` allows ([The clock starts at Ready](../task-lifecycle.md#the-clock-starts-at-ready)). A task with a Pod is held like a conflicting object, as in the previous bullet.
- **A child deleted while the task has a Pod.** It is re-created at once, because the reconciler watches it. The CiliumNetworkPolicy is not watched, so it comes back only at the task's next pass for another reason, such as a Pod change or a class edit, and until then the Pod cannot reach `allowedHosts`.
- **A missing class, an empty image, or a malformed class CIDR.** No timed re-check: these clear when the task or the class changes. `SystemNamespaceForbidden` has no timed re-check and does not clear; create the task in another namespace.
- **A settled task.** The pass is requeued for the remaining TTL ([rule 43](../../resources/validation/class-policy.md)), then the task goes `Terminating` and is deleted. With no TTL, the task stays.

## Design choices

- **`restartPolicy: Never`**, so a crash is a counted retry, not a kubelet restart ([exitCode](../task-lifecycle.md#exitcode)).
- **No probes**, because a task has no Service; the completion timeout ([rule 42](../../resources/validation/class-policy.md)) and the provisioning deadline bound liveness.
