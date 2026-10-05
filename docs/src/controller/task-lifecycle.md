# AgentTask lifecycle

An [AgentTask](../resources/agenttask.md) is a run-to-completion workload: the operator provisions a Pod, the Pod does its work, and the operator records the result; the Pod is removed with the task when its TTL expires. Unlike an Agent, which is long-lived and hibernates between requests, an AgentTask always settles in a terminal phase.

This page covers the AgentTask state machine, how the operator decides a task is done, how artifacts get from the container into `status`, and what happens on a retry. The checks the reconciler runs before it creates a Pod, and their order, are on the [AgentTaskReconciler](reconcilers/agenttask.md#what-it-checks) page; the wire contract of the completion call is [POST /v1/task/complete](../gateways/api/task-complete.md).

## State machine

The other lifecycles in the system are indexed on [Lifecycles at a glance](../appendix/lifecycles.md).

![AgentTask state machine, one trigger per edge. Pending to Provisioning on Certificate created, Provisioning to Running on Pod Ready, Running to Completing on completion, exit, or timeout, and Completing to Succeeded on success, to Failed on failure, or to TimedOut on a timeout with onTimeout Fail. Pending to Failed when the spec is irreconcilable, Provisioning to Failed when the Pod fails to start or the spec is irreconcilable, Running to Failed when the Pod is lost with an empty mailbox, and Failed back to Provisioning while retries are left. Any phase moves to Terminating when deleted or when the TTL expires.](../diagrams/task-lifecycle.svg)

## Transition triggers

| From | To | Trigger |
|---|---|---|
| `Pending` | `Provisioning` | The pre-Pod checks pass and the Certificate exists; the task holds in `Provisioning` until the Certificate is Ready. A task whose env Secret is missing or lacks the workload label stays in its phase with `Ready=False, reason=SecretNotOptedIn` (rule 48). |
| `Pending`, `Provisioning` | `Failed` (terminal) | A class-versus-spec violation under rules 2 to 5, 24, 35 to 38, or 47: `ClassConstraintViolation`, `PersistenceNotAllowed`, `ToolNotInCatalog`, or `NamespaceNotAllowed` (the class does not admit the task's namespace; this check runs first, so it is the reported reason when several fail). Checked whenever no Pod exists, so a retry is validated against the class as it now stands. Not retried. |
| `Provisioning` | `Running` | The Pod reports Ready. The same status write sets `status.startTime`, and the timeout clock starts. |
| `Provisioning` | `Failed` | The container image name is invalid or can never be pulled (`InvalidImageName`, `ErrImageNeverPull`); the Pod reaches a terminal phase before Ready (`PodStartFailed`); or the attempt has no Ready Pod five minutes after the Pod was created, or after the API server first rejected the Pod create, whatever the cause (`ProvisioningDeadlineExceeded`). While a Pod create is rejected, the task waits in `Provisioning` with `Ready=False, reason=PodCreateRejected` ([Error handling](operations.md#error-handling) says which errors count). Retryable. An `exitCode` task whose Pod exits 0 before Ready goes to `Completing` instead. |
| `Running` | `Completing` | The completion mailbox holds a payload (`agentReported`), the container exits (`exitCode`), or the timeout elapses. |
| `Running` | `Failed` | The Pod is lost mid-run and the mailbox is empty (`PodDisrupted`). Retryable. See [Completion beats disruption](#completion-beats-disruption). |
| `Completing` | `Succeeded` | The payload reports success and its artifact names pass the re-check, or the container exited 0. |
| `Completing` | `Failed` | The payload reports failure, its artifact names fail the re-check, the container exited non-zero, or the Pod is gone with no payload. Retryable. |
| `Completing` | `TimedOut` | The timeout has elapsed and the mailbox is empty, with `onTimeout: Fail` (the default). With `onTimeout: Succeed` the task settles `Succeeded`. Not retried. |
| `Failed` | `Provisioning` | `status.retries` is below `backoffLimit`. See [Retry mechanics](#retry-mechanics). A `Failed` task with no `completionTime` resumes here too (a retry that a controller restart interrupted). |
| any terminal | `Terminating` | The effective `ttlSecondsAfterFinished` has elapsed since `completionTime`, or deletion was requested. A task with no TTL of its own and no class default is kept. |

A terminal `Failed` carries `completionTime`; a transient `Failed` written mid-retry does not, which is how the two are told apart.

### The clock starts at Ready

The task's effective timeout comes from its own `completion.timeout` and its class bounds ([The class bounds timeout and retention](../resources/agenttask.md#the-class-bounds-timeout-and-retention)). It measures from `status.startTime`, which the reconciler sets when the Pod reports Ready, so scheduling and image-pull time never count against the task's budget. A retry clears `startTime`, so each attempt gets the full timeout. A task stuck before Ready, including one whose Pod create the API server rejects, is bounded by the fixed five-minute provisioning deadline instead.

### Completion beats disruption

The Pod can vanish mid-run: evicted, deleted out of band, or its node lost. Before classifying the loss, the reconciler reads the completion mailbox. A payload already accepted through the identity gate (the `currentPodUID` check at `/v1/task/complete`; see [agentReported](#agentreported)) wins and drives `Running` to `Completing`; only an empty mailbox makes the loss a retryable `Failed`. Without this order, an eviction landing right after a successful completion call would wipe a valid result in the retry sequence and re-run a finished task. In `exitCode` mode there is no mailbox: an evicted Pod has no exit code, which counts as failure.

### Completing re-reads the evidence

On entering `Completing`, the reconciler re-reads the mailbox, the Pod, and the clock, in that precedence: a payload settles the task by its status, then a terminal container by its exit code, then an elapsed timeout by `onTimeout`, and a Pod gone with none of these is a retryable disruption. A timeout-triggered transition therefore still settles by payload when the completion landed in the meantime, and `onTimeout` decides only when the mailbox is empty.

`TimedOut` is distinct from `Failed` so that timeouts are attributable and exempt from retries: a timeout means the budget was too small, and retrying with the same timeout would time out again. Raise `spec.completion.timeout`, within the class `maxTaskTimeout`, and re-apply the task instead.

## Completion detection

How the operator learns a task is done depends on `spec.completion.condition`.

### agentReported

The agent container calls [POST /v1/task/complete](../gateways/api/task-complete.md). The gateway writes the payload (status, message, and artifact key-values) into the pre-existing `{taskName}-completion` ConfigMap in the task's namespace, and the reconciler, watching that ConfigMap, moves the task to `Completing` once a payload is present. A ConfigMap rather than a Pod annotation keeps the completion data across Pod crashes and evictions between the agent's call and the reconciler's next pass, which is what makes the precedence rule above possible.

The reconciler sets `status.currentPodUID` from the Create response in the same pass that creates the Pod, so the identity gate at `/v1/task/complete` is open before the container starts. The gate's two rejections, `409 stale_pod` and `403 TaskAlreadyCompleted`, are specified on the wire page. `exitCode` tasks carry no `currentPodUID`.

### exitCode

The reconciler watches the Pod phase: `Succeeded` settles `Succeeded`, `Failed` settles `Failed` with the container's exit message. This depends on task Pods being created with `restartPolicy: Never`, which the reconciler pins unconditionally: with `Always` or `OnFailure` the kubelet restarts the exited container in place and the Pod phase never reaches a terminal value, and an in-place restart would also bypass `status.retries` and blur the one-run-per-`currentPodUID` gate.

## Artifact collection

In `agentReported` mode the artifact values travel in the completion payload. The reconciler reads them from the ConfigMap and writes them to `status.artifactValues`, along with `status.agentReportedStatus` and `status.agentReportedMessage`; no exec into the container is needed. The gateway enforces artifact-name conformance against `spec.artifacts` and the size caps before it writes the ConfigMap; the reconciler re-checks the names when it reads them, as a second check against RBAC drift on the per-task Role, and a re-check failure is a retryable `Failed`. The caps, the `413` response, and the externalize-and-reference guidance are on [POST /v1/task/complete](../gateways/api/task-complete.md).

## Retry mechanics

When the task fails and `status.retries` is below `backoffLimit`, one status write increments `status.retries`, clears `status.currentPodUID`, `status.startTime`, and the previous attempt's artifact and report fields, and sets a transient `Failed` phase. The reconciler then deletes the old Pod, resets the `{taskName}-completion` ConfigMap to `data: {}` (an update rather than a delete, so the ownerRef and the gateway's name-scoped Role stay valid), and sets `Provisioning`. The next pass creates the new Pod, and the status write that records it sets `currentPodUID` from the Create response and records the class's current task bounds in `status.classBounds`. The PVC is retained, so the retry runs with the same scratch storage.

![Sequence diagram of an AgentTask retry. The reconciler increments retries and clears currentPodUID in one status write, which closes the gate. Inside the closed gate it deletes the old Pod, whose late completion call receives 409 stale_pod from the gateway, resets the completion ConfigMap to an empty data map, creates the new Pod, and sets currentPodUID to the new UID. The new Pod's completion call is then accepted and the gateway writes the result.](../diagrams/task-retry-race.svg)

Clearing the UID before resetting the mailbox closes the stale-write window: a late completion from the old Pod fails the gate with `409 stale_pod` instead of refilling the emptied mailbox. The gateway rejects every completion from the clear until the new UID is set, so a new Pod whose first completion call comes before that status write also receives `409 stale_pod`; agents retry on it per [The runtime contract](../runtime/contract.md), item 6. While the old Pod is still terminating, the pass after the retry holds with `Ready=False, reason=PodTerminating` and creates the replacement only once no task Pod remains.

A retry re-runs the pre-Pod class check against the class as it now stands. A violation there settles the task as terminal `Failed` at once, whatever `backoffLimit` remains, and the increment already spent is not refunded. To retry a task against a class you have since aligned, delete and recreate the task: a `kubectl apply` of the same spec does not reset `status.retries`, since status is controller-owned and apply patches only `spec`.

A retry also re-runs the env Secret gate ([rule 48](../resources/validation/references-and-access.md)). That gate is not terminal: a task whose env Secret is missing or has lost the label waits in `Provisioning` with `Ready=False, reason=SecretNotOptedIn`, creates no Pod, and counts no further retry. Labeling the Secret lets the next pass, within 30 seconds, create the Pod. A task that has a Pod or has finished is not checked.

## Event reasons

| Reason | Type | When |
|---|---|---|
| `TaskSucceeded` | Normal | the task settles `Succeeded` |
| `TaskFailed` | Warning | the task settles `Failed` from a reported failure, a non-zero exit, or an artifact re-check |
| `TimeoutExceeded`, `TimeoutSucceeded` | Warning, Normal | the task settles `TimedOut`, or `Succeeded` under `onTimeout: Succeed` |
| `PodStartFailed`, `ProvisioningDeadlineExceeded`, `InvalidImageName`, `ErrImageNeverPull` | Warning | a provisioning failure settles or retries |
| `PodDisrupted` | Warning | the Pod was lost mid-run or before completion settled |
| `ClassConstraintViolation`, `PersistenceNotAllowed`, `ToolNotInCatalog`, `NamespaceNotAllowed` | Warning | the pre-Pod class check settles the task `Failed` |
| `PodCreateRejected` | Warning | the API server rejects the Pod create and `Ready` first takes this reason, not on each re-check and not when only the message changes |
| `ChildConflict` | Warning | a child object that the task would own already exists and is not owned by it, when the reason or its message first appears on `Ready` |
| `ResourceClaimsIgnored` | Warning | the task's `spec.resources` sets `claims` ([rule 53](../resources/validation/class-policy.md)); not on every pass, and never for a settled task. [Event emission](operations.md#event-emission) gives the timing |
| `SystemNamespaceForbidden`, `InvalidReference`, `ImagePullSecretMissing`, `SecretNotOptedIn` | Warning | a pre-Pod reconcile-time gate sets `Ready=False` with the reason, when the reason first appears on `Ready`, not on each pass that finds the problem again |

A retry emits a `Warning` event with the failure's reason. A settled task emits its event once.
