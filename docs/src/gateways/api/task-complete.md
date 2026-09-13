# Task completion

`POST /v1/task/complete` is the internal endpoint an AgentTask's agent container calls to report that its work is finished. It applies only to tasks with `completion.condition: agentReported`; tasks in `exitCode` mode signal completion through container exit and are rejected here with `403 TaskNotAgentReported`. Like the other agent-report endpoints it is mTLS-only: there is no ServiceAccount-bearer alternative, and gateway-only-tier workloads cannot reach it (see [Workload identity](../llm/workload-identity.md) and [Agent to gateway authentication](../../security/rbac.md#agent-to-gateway-authentication)).

**Caller.** The task's agent container, presenting the per-task client certificate whose SAN carries the AgentTask kind, name, and namespace.

**Effect.** A `200` means the completion payload is in the task's `{taskName}-completion` ConfigMap. The [AgentTaskReconciler](../../controller/reconcilers.md#agenttaskreconciler) observes the write and moves the task to `Completing`. The gateway never writes `AgentTask.status` itself.

## How completion is recorded

The gateway writes the pre-existing `{taskName}-completion` ConfigMap in the task's namespace, which acts as a mailbox. The ConfigMap is the data channel; admission to write it is a separate check, described under [The identity gate](#the-identity-gate).

- The AgentTaskReconciler creates the ConfigMap at task provisioning with `data: {}` and an ownerRef to the AgentTask, so it is deleted with the task, together with a per-task Role and RoleBinding that grant the gateway `update` and `patch` on that one ConfigMap name. The names, and why the Role carries no `create` verb, are under [The completion mailbox](../../runtime/child-resources.md#the-completion-mailbox).
- The gateway patches the ConfigMap through that Role; see [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions).
- The reconciler watches the ConfigMap, re-validates the artifact names, and transitions the task to `Completing` on the first observed payload. It remains the final authority on AgentTask state; see [AgentTask lifecycle](../../controller/task-lifecycle.md).

![Sequence diagram of the record path: the Task Pod POSTs to the gateway over mTLS, the gateway runs its gates and validation, patches the completion ConfigMap through the scoped Role, and returns 200; the reconciler's ConfigMap watch fires, it re-checks the artifacts, and it sets status.phase to Completing.](../../diagrams/task-completion-record.svg)

## The identity gate

Every call passes the gates below in order, and every rejection fires before the ConfigMap `Patch` is attempted, so only a `503` means the write itself failed. Gates 1 to 3 are the mTLS profile shared by the agent-report paths, specified under [Per-path client auth enforcement](../listener-tls.md#per-path-client-auth-enforcement) and [Source-IP cross-check](../llm/workload-identity.md#source-ip-cross-check-both-modes). Gates 4 to 7 read `spec.completion`, `status.phase`, and `status.currentPodUID` from the gateway's cluster-wide AgentTask watch, so they run synchronously with no apiserver round trip.

![Flowchart of the gates on POST /v1/task/complete in order: client certificate, SAN kind, source IP resolves to a Pod, an AgentTask backs the caller, the condition is agentReported, the phase is not terminal, the Pod UID matches status.currentPodUID, the body and artifact names are valid, the size caps hold, and the Patch succeeds. Each failed check ends in its status code and reason; the Pod UID and Patch checks are marked retryable.](../../diagrams/task-completion-gates.svg)

| Order | Check | Rejection | `retryable` |
|---|---|---|---|
| 1 | A client certificate is presented | `401 unauthorized` | `false` |
| 2 | The SAN kind is AgentTask | `403 access_denied` | `false` |
| 3 | The source IP resolves to a Pod in the SAN namespace, in the informer cache or, on a miss, in a live `List Pods` narrowed to that namespace | `401 unauthorized` | `false` |
| 4 | An AgentTask with the SAN name exists in that namespace | `403 access_denied` | `false` |
| 5 | `completion.condition` is `agentReported` | `403 access_denied`, `TaskNotAgentReported` | `false` |
| 6 | `status.phase` is not terminal (`Succeeded`, `Failed`, `TimedOut`) | `403 access_denied`, `TaskAlreadyCompleted` | `false` |
| 7 | The calling Pod's UID equals `status.currentPodUID` | `403 access_denied`, `StalePodCompletion` | `true` |
| 8 | The body parses and the artifact names pass the per-status rule | `400 invalid_request` | `false` |
| 9 | Every artifact value is within 4 KiB and the combined payload within 32 KiB | `413 request_too_large` | `false` |
| 10 | The ConfigMap `Patch` succeeds | `503 internal_unavailable`, `Retry-After: 1` | `true` |

The live `List Pods` in gate 3 exists for the new-Pod startup window, where the gateway's Pod informer has not observed the calling Pod. Without it that window would end in a terminal `401`; with it, the call reaches gate 7 and at worst receives the retryable `403 StalePodCompletion`. The fallback is scoped to this path alone: heartbeats are periodic and recover on the next tick, and a fleet-wide fallback would turn an informer resync into a live-List stampede.

Gate 7 is the identity gate proper. It closes the stale-write race after a `backoffLimit` retry, where an old Pod's delayed completion would otherwise overwrite the new Pod's data, and it is the same rejection a new Pod can receive in the restamp-lag window described under [Race windows](#race-windows), which is why it is retryable. Gate 6 exists because the reconciler does not re-process the mailbox once the phase is terminal; without the gate the agent's write would be silently dropped.

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

`error.message` names the offending key in either branch: `missing declared artifact: pr-url` (success only) or `undeclared artifact in payload: extra-key` (both branches).

The gateway reads `spec.artifacts` from its AgentTask watch, so the check runs synchronously and the agent learns of a mismatch before exiting rather than later in `AgentTask.status`. The AgentTaskReconciler re-validates the names with the same per-status rule when it reads the ConfigMap, as a second check against RBAC drift on the per-task Role. Under normal operation the gateway-side check makes the reconciler's re-check a no-op.

## Response codes

| Code | `error.type` | `retryable` | Condition |
|---|---|---|---|
| `200 OK` | (none) | n/a | Success; empty body |
| `400 Bad Request` | `invalid_request` | `false` | Malformed body or artifact-name rule violation |
| `401 Unauthorized` | `unauthorized` | `false` | No client certificate, or the source IP resolves to no Pod in the SAN namespace even after the live fallback |
| `403 Forbidden` | `access_denied` | `false`, except `true` for `StalePodCompletion` | One of four reasons, see [403 Forbidden](#403-forbidden) |
| `413 Payload Too Large` | `request_too_large` | `false` | Per-artifact or combined size cap exceeded |
| `503 Service Unavailable` | `internal_unavailable` | `true` | The ConfigMap `Patch` itself failed |

The `error.type` names reuse the vocabulary of the [LLM Gateway](errors.md#llm-gateway-error-responses) and [User Gateway](errors.md#user-gateway-error-responses) error tables. A client certificate whose SAN does not parse returns `403` with `error.type: invalid_cert` from the shared mTLS profile; see [Mode 1: mTLS client certificate](../llm/workload-identity.md#mode-1-mtls-client-certificate).

### 400 Bad Request

Returned when the request body is not valid JSON, when `status` is missing or is neither `"success"` nor `"failure"`, when `artifacts` is present but is not an object of string-to-string entries, or when the artifact names violate the per-`status` rule under [Artifact name validation](#artifact-name-validation). The body is validated before the `Patch`, so a `400` means no state change.

### 401 Unauthorized

Returned when no client certificate is presented, or when the source IP resolves to no Pod in the SAN namespace in the informer cache or in the live `List Pods` fallback. Typical causes of the second case are off-cluster spoofing, or a Pod terminated and removed by kubelet between dial and handle. The envelope is the [LLM Gateway 401 row](errors.md#llm-gateway-error-responses), `error.type: unauthorized`, `retryable: false`: both lookups have been exhausted, so a fresh attempt hits the same condition. Agents under normal operation do not observe this code.

### 403 Forbidden

| Reason | Condition | `error.message` |
|---|---|---|
| `NotAgentTaskPod` | The SAN kind is not AgentTask, or no AgentTask with the SAN name exists in the namespace | `Agent callers are not accepted on this path`, or `no AgentTask backs this caller` |
| `TaskNotAgentReported` | The task has `completion.condition: exitCode` | `TaskNotAgentReported: this task completes via container exit` |
| `StalePodCompletion` | The calling Pod's UID does not match `status.currentPodUID`, or the field is empty | `StalePodCompletion: the calling Pod is not the task's current Pod` |
| `TaskAlreadyCompleted` | `status.phase` is terminal (`Succeeded`, `Failed`, `TimedOut`) | `TaskAlreadyCompleted: the task has reached a terminal phase` |

Three of the four messages carry the reason as a prefix. The two checks behind `NotAgentTaskPod` return plain messages, so a caller can distinguish that reason only by the absence of a prefix.

`exitCode` tasks have no completion mailbox: the ConfigMap and the per-task Role are provisioned in `agentReported` mode only (see [Child resources](../../runtime/child-resources.md)). `StalePodCompletion` and `TaskAlreadyCompleted` are gates 7 and 6 under [The identity gate](#the-identity-gate).

### 413 Payload Too Large

Returned when any single artifact value exceeds 4 KiB (`error.message` names the artifact key) or when the sum of `message` plus all artifact values exceeds 32 KiB. Sizes are measured in UTF-8 bytes of the value strings only; keys are bounded by ConfigMap key naming rules and are not counted. The combined cap exists because the body is buffered in gateway memory and then patched into the per-task ConfigMap, which has the Kubernetes object limit of about 1 MiB. Large artifacts should be stored externally and referenced by URL in the value.

### 503 Service Unavailable

Returned when the `Patch` against the completion ConfigMap fails after every gate has passed: apiserver transiently unavailable, etcd unreachable, a `Patch` conflict, or RBAC drift on the per-task Role. `error.type: internal_unavailable`, `retryable: true`, with `Retry-After: 1` (integer delta-seconds, RFC 7231 § 7.1.3) as a cadence floor, mirroring the `504 controller_unavailable` pattern on the User Gateway. Agents must wait at least 1 second before retrying and may apply their own bounded backoff that waits longer, per [The runtime contract](../../runtime/contract.md), item 6.

## Race windows

Re-completion across a `backoffLimit` retry is the supported multi-call path. The reconciler clears `status.currentPodUID`, resets the mailbox to `data: {}`, creates the replacement Pod, and stamps the new UID once it observes the Pod; the order and the figure are under [Retry mechanics](../../controller/task-lifecycle.md#retry-mechanics). Any in-flight call from the old Pod fails gate 7, and the new Pod's call lands on a fresh mailbox under the new UID.

There is a narrow restamp-lag window, typically under 100ms of informer lag against seconds of agent startup, where the new Pod's first call races the UID stamp and receives `403 StalePodCompletion`. This is the transient, retryable form of that code.

## Retry guidance

- **`retryable: false`** on `400`, `413`, and the `NotAgentTaskPod`, `TaskNotAgentReported`, and `TaskAlreadyCompleted` reasons: a duplicate call from the same Pod hits the same outcome. On `TaskAlreadyCompleted` the task is terminal and further writes are rejected by design; the agent should log and exit.
- **`retryable: true`** on `StalePodCompletion` and `503 internal_unavailable`: the restamp lag is transient, and so are the conditions behind a `503` (an apiserver flap, a leader election, brief etcd unavailability).

Agents should retry both retryable cases with bounded backoff per [The runtime contract](../../runtime/contract.md), item 6: 100ms, 500ms, 2s, 3 attempts at most. For `503`, the `Retry-After: 1` floor also applies.

## Error envelope

Error responses carry the structured `{ "error": { "type", "message", "retryable" } }` envelope, the same envelope as [User Gateway error responses](errors.md#user-gateway-error-responses). `error.type` is `invalid_request` for 400, `unauthorized` for 401, `access_denied` for 403, `request_too_large` for 413, and `internal_unavailable` for 503. `error.message` carries the diagnostic: the offending artifact name or key for 400 and 413, the reason string for 403, and `patching the completion ConfigMap failed` for 503.
