# Task completion

`POST /v1/task/complete` is the internal endpoint an AgentTask's agent container calls to report that its work is finished. It applies only to tasks with `completion.condition: agentReported`; tasks in `exitCode` mode signal completion through container exit and are rejected here with `403 TaskNotAgentReported`. Like the heartbeat endpoint it is mTLS-only: there is no ServiceAccount-bearer alternative, and gateway-only-tier workloads cannot reach it (see [Workload identity](../llm/workload-identity.md) and [Agent to gateway authentication](../../security/rbac.md#agent-to-gateway-authentication)).

**Caller.** The task's agent container, presenting the per-task client certificate whose SAN carries the AgentTask kind, name, and namespace.

**Effect.** A `200` means the completion payload is in the task's `{taskName}-completion` ConfigMap. The [AgentTaskReconciler](../../controller/reconcilers/agenttask.md) observes the write and moves the task to `Completing`. The gateway never writes `AgentTask.status` itself.

## How completion is recorded

The gateway writes the pre-existing `{taskName}-completion` ConfigMap in the task's namespace, which acts as a mailbox. The ConfigMap is the data channel; admission to write it is a separate check, described under [Checks before the write](#checks-before-the-write).

- The AgentTaskReconciler creates the ConfigMap at task provisioning with `data: {}` and an ownerRef to the AgentTask, so it is deleted with the task. A per-task Role and RoleBinding let the gateway `update` and `patch` that one ConfigMap and nothing else. The names, and why the Role carries no `create` verb, are under [The completion mailbox](../../runtime/child-resources.md#the-completion-mailbox); see also [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions).
- The reconciler watches the ConfigMap and remains the final authority on AgentTask state; see [AgentTask lifecycle](../../controller/task-lifecycle.md).

![Sequence diagram of the record path: the Task Pod POSTs to the gateway over mTLS, the gateway runs its checks and validation, patches the completion ConfigMap through the scoped Role, and returns 200; the reconciler's ConfigMap watch fires, it re-checks the artifacts, and it sets status.phase to Completing.](../../diagrams/task-completion-record.svg)

## Checks before the write

Every call passes the checks below in order, and every rejection fires before the ConfigMap `Patch` is attempted, so only a `503` means the write itself failed. Checks 1 to 3 are the mTLS profile shared by the agent-report paths, specified under [Per-path client auth enforcement](../listener-tls.md#per-path-client-auth-enforcement) and [Source-IP cross-check](../llm/workload-identity.md#source-ip-cross-check-both-modes). Checks 4 to 7 read `spec.completion`, `status.phase`, and `status.currentPodUID` from the gateway's cluster-wide AgentTask watch, so a status change reaches them after an informer lag (see [Race windows](#race-windows)).

![Flowchart of the checks on POST /v1/task/complete in order: client certificate, SAN kind, source IP resolves to a Pod, an AgentTask backs the caller, the condition is agentReported, the phase is not terminal, the Pod UID matches status.currentPodUID, the body and artifact names are valid, the size caps hold, and the Patch succeeds. Each failed check ends in its status code and reason; the Pod UID and Patch checks are marked retryable.](../../diagrams/task-completion-checks.svg)

| Order | Check | Rejection | `retryable` |
|---|---|---|---|
| 1 | A client certificate is presented | `401 unauthorized` | `false` |
| 2 | The SAN kind is AgentTask | `403 access_denied` | `false` |
| 3 | The source IP resolves to a Pod in the SAN namespace, in the informer cache or, on a miss, in a live `List Pods` narrowed to that namespace | `401 unauthorized` | `false` |
| 4 | An AgentTask with the SAN name exists in that namespace | `403 access_denied` | `false` |
| 5 | `completion.condition` is `agentReported` | `403 access_denied`, `TaskNotAgentReported` | `false` |
| 6 | `status.phase` is not terminal (`Succeeded`, `Failed`, `TimedOut`) | `403 access_denied`, `TaskAlreadyCompleted` | `false` |
| 7 | The calling Pod's UID equals `status.currentPodUID` | `409 stale_pod`, `StalePodCompletion` | `true` |
| 8 | The body parses and the artifact names pass the per-status rule | `400 invalid_request` | `false` |
| 9 | Every artifact value is within 4 KiB and the combined payload within 32 KiB | `413 request_too_large` | `false` |
| 10 | The ConfigMap `Patch` succeeds | `503 internal_unavailable`, `Retry-After: 1` | `true` |

The live `List Pods` in check 3 exists for the new-Pod startup window, where the gateway's Pod informer has not observed the calling Pod. Without it that window would end in a terminal `401`; with it, the call reaches check 7 and at worst receives the retryable `409 stale_pod`. The fallback applies to this path alone: other paths recover without it (see [Source-IP cross-check](../llm/workload-identity.md#source-ip-cross-check-both-modes)), and a fallback on every path would turn an informer resync into a burst of live `List` calls against the apiserver.

Check 7 is the Pod UID check: the calling Pod's UID must equal `status.currentPodUID`, and otherwise the call gets `409 stale_pod`. It closes the stale-write race after a `backoffLimit` retry, where an old Pod's delayed completion would otherwise overwrite the new Pod's data. A new Pod can receive the same rejection before the gateway sees its UID, as described under [Race windows](#race-windows), which is why it is retryable. Check 6 exists because the reconciler does not re-process the mailbox once the phase is terminal; without it the agent's write would be silently dropped.

## Request body

```json
{
  "status": "success",
  "message": "PR opened successfully",
  "artifacts": {
    "pr-url": "https://github.com/acme/widgets/pull/587",
    "summary": "Fixed null pointer in WidgetService.get(). Added regression test."
  }
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `status` | string | yes | `"success"` or `"failure"` |
| `message` | string | no | Human-readable completion message |
| `artifacts` | map[string]string | no | Key-value pairs matching the names declared in `spec.artifacts`; validated per `status`, see below |

### Artifact name validation

The gateway validates artifact names against the task's `spec.artifacts` and returns `400 invalid_request` on mismatch. The rule splits by `status`:

- **`status: "success"`**: every declared name must be present, and no undeclared names may appear.
- **`status: "failure"`**: only the no-undeclared-names rule is enforced. A failing task may report a subset of declared artifacts, or none, and still have its failure recorded, so an agent that crashes before producing its full deliverable set can still report failure.

`error.message` names the offending key in either branch.

The gateway reads `spec.artifacts` from its AgentTask watch, so the check runs synchronously and the agent learns of a mismatch before exiting rather than later in `AgentTask.status`. The AgentTaskReconciler re-validates the names with the same per-status rule when it reads the ConfigMap, as a second check against RBAC drift on the per-task Role.

## Response codes

| Code | `error.type` | `retryable` | Condition |
|---|---|---|---|
| `200 OK` | (none) | n/a | Success; empty body |
| `400 Bad Request` | `invalid_request` | `false` | Malformed body or artifact-name rule violation |
| `401 Unauthorized` | `unauthorized` | `false` | No client certificate, or the source IP resolves to no Pod in the SAN namespace even after the live fallback |
| `403 Forbidden` | `access_denied` | `false` | One of three reasons, see [403 Forbidden](#403-forbidden) |
| `405 Method Not Allowed` | `invalid_request` | `false` | The method is not `POST`. The response carries `Allow: POST`. |
| `409 Conflict` | `stale_pod` | `true` | The calling Pod is not the task's current Pod, see [409 Conflict](#409-conflict) |
| `413 Payload Too Large` | `request_too_large` | `false` | Per-artifact or combined size cap exceeded |
| `503 Service Unavailable` | `internal_unavailable` | `true` | The ConfigMap `Patch` itself failed |

The `error.type` names reuse the vocabulary of the [LLM Gateway](errors.md#llm-gateway-error-responses) and [User Gateway](errors.md#user-gateway-error-responses) error tables. A client certificate whose SAN does not parse returns `403` with `error.type: invalid_cert` from the shared mTLS profile; see [Mode 1: mTLS client certificate](../llm/workload-identity.md#mode-1-mtls-client-certificate).

### 400 Bad Request

Returned when the request body is not valid JSON, when `status` is missing or is neither `"success"` nor `"failure"`, when `artifacts` is present but is not an object of string-to-string entries, or when the artifact names violate the per-`status` rule under [Artifact name validation](#artifact-name-validation). The body is validated before the `Patch`, so a `400` means no state change.

### 401 Unauthorized

Returned when no client certificate is presented, or when the source IP resolves to no Pod in the SAN namespace in the informer cache or in the live `List Pods` fallback. The envelope is the [LLM Gateway 401 row](errors.md#llm-gateway-error-responses), `error.type: unauthorized`, `retryable: false`: both lookups have been exhausted, so a fresh attempt hits the same condition. Agents under normal operation do not observe this code.

### 403 Forbidden

| Reason | Condition |
|---|---|
| `NotAgentTaskPod` | The SAN kind is not AgentTask, or no AgentTask with the SAN name exists in the namespace |
| `TaskNotAgentReported` | The task has `completion.condition: exitCode` |
| `TaskAlreadyCompleted` | `status.phase` is terminal (`Succeeded`, `Failed`, `TimedOut`) |

Every `error.message` starts with its reason code followed by `: `, so a caller can tell the three reasons apart by that prefix.

`exitCode` tasks have no completion mailbox: the ConfigMap and the per-task Role are provisioned in `agentReported` mode only (see [Child resources](../../runtime/child-resources.md)).

### 409 Conflict

Returned when the calling Pod's UID does not match `status.currentPodUID`, or the field is empty: the Pod UID check, check 7 under [Checks before the write](#checks-before-the-write). `error.type` is `stale_pod`, `retryable` is `true`, and `error.message` starts with `StalePodCompletion: `. The call conflicts with the task's current state rather than being refused for good: once the gateway sees the new UID, the same Pod's retry can succeed, which is why the code is `409` and not `403`. See [Race windows](#race-windows).

### 413 Payload Too Large

Returned when any single artifact value exceeds 4 KiB or when the sum of `message` plus all artifact values exceeds 32 KiB. Sizes are measured in UTF-8 bytes of the value strings only; keys are bounded by ConfigMap key naming rules and are not counted. The combined cap exists because the body is buffered in gateway memory and then patched into the per-task ConfigMap, which has the Kubernetes object limit of about 1 MiB. Large artifacts should be stored externally and referenced by URL in the value.

### 503 Service Unavailable

Returned when the `Patch` against the completion ConfigMap fails after every check has passed: apiserver transiently unavailable, etcd unreachable, a `Patch` conflict, or RBAC drift on the per-task Role. `error.type: internal_unavailable`, `retryable: true`, with `Retry-After: 1` (integer delta-seconds, RFC 7231 § 7.1.3) as a cadence floor. Agents must wait at least 1 second before retrying; see [Retry guidance](#retry-guidance).

## Race windows

Re-completion across a `backoffLimit` retry is the supported multi-call path. The reconciler clears `status.currentPodUID`, resets the mailbox to `data: {}`, and sets `status.currentPodUID` to the replacement Pod's UID once that Pod exists; the order and the figure are under [Retry mechanics](../../controller/task-lifecycle.md#retry-mechanics). Any in-flight call from the old Pod fails the Pod UID check, and the new Pod's call lands on a fresh mailbox under the new UID.

The gateway sees the new UID after an informer lag, typically under 100ms, while agent startup takes seconds. A first call from the new Pod inside that lag receives `409 stale_pod`. This is the transient, retryable form of that code.

## Retry guidance

- **`retryable: false`** on `400`, `413`, and the `NotAgentTaskPod`, `TaskNotAgentReported`, and `TaskAlreadyCompleted` reasons: a duplicate call from the same Pod hits the same outcome. On `TaskAlreadyCompleted` the task is terminal and further writes are rejected by design; the agent should log and exit.
- **`retryable: true`** on `409 stale_pod` and `503 internal_unavailable`: the lag before the gateway sees the new UID is transient, and so are the conditions behind a `503` (an apiserver flap, a leader election, brief etcd unavailability).

Retry both `409 stale_pod` and `503 internal_unavailable` as [The runtime contract](../../runtime/contract.md), item 6, describes.

## Error envelope

Error responses carry the structured `{ "error": { "type", "message", "retryable" } }` envelope, the same envelope as [User Gateway error responses](errors.md#user-gateway-error-responses), with the `error.type` for each code listed under [Response codes](#response-codes). `error.message` names the offending artifact key when an artifact breaks the name rule or the 4 KiB cap, and starts with the reason code for 403 and 409.
