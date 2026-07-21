# Task Completion

`POST /v1/task/complete` is the internal endpoint an AgentTask's agent container calls to report that its work is finished. It applies only to tasks with `completion.condition: agentReported`; tasks in `exitCode` mode signal completion through container exit and are rejected here (see [403 cases](#403-forbidden-four-reasons) below). Like the other agent-only internal endpoints, it is mTLS-only: there is no ServiceAccount-bearer alternative, and gateway-only-tier workloads cannot reach it (see [Namespace Identification](../llm/workload-identity.md) and [Agent to Gateway Authentication](../../security/rbac.md#agent-to-gateway-authentication)).

![Sequence diagram of the task-completion protocol. At provisioning the AgentTaskReconciler creates the empty {taskName}-completion ConfigMap and a per-task Role scoped by resourceNames to that ConfigMap name with mutate verbs but no create verb, then stamps status.currentPodUID once the Pod is observed. At completion the Task Pod POSTs /v1/task/complete over mTLS; the gateway resolves the source IP to a Pod UID via its informer, falling back to a live List Pods on a cache miss, then rejects with 401 unauthorized or one of the four 403 access_denied reasons (NotAgentTaskPod, TaskNotAgentReported, StalePodCompletion which alone is retryable, and TaskAlreadyCompleted) before validating artifact names and the 4 KiB and 32 KiB size caps, patching the ConfigMap through the scoped Role, and returning 200. The reconciler's ConfigMap watch then fires, re-checks artifacts defensively, and moves the task to Completing.](../../diagrams/task-completion-protocol.svg)

Reading the diagram: everything above the `Completion` divider happens once, at task provisioning. Every rejection arm fires before the ConfigMap `Patch` is attempted, so only a `503` implies the data-channel write itself failed.

## How completion is recorded

The gateway does not update `AgentTask.status` directly. It updates the pre-existing `{taskName}-completion` ConfigMap in the task's namespace, which acts as a mailbox:

- The ConfigMap is created by the [AgentTaskReconciler](../../controller/reconcilers.md#agenttaskreconciler) at task provisioning time and is owned by the AgentTask, so it is cascade-deleted with the task.
- The gateway writes it using `update, patch`-only RBAC scoped to that exact ConfigMap name (see [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions)).
- The AgentTaskReconciler watches the ConfigMap for changes and transitions the task to `Completing` on the first observed payload. The reconciler remains the final authority on `AgentTask` state. See the [AgentTask State Machine](../../controller/task-lifecycle.md) for transitions and retry mechanics.

The ConfigMap is the **data** channel. Admission to write it is gated by a separate **identity check**, described next.

## The identity gate

Every call is checked against `AgentTask.status.currentPodUID` before any write happens:

1. The gateway resolves the calling Pod's UID via the source-IP → Pod cross-check ([The Kaalm Gateway](../overview.md)).
2. On an informer-cache miss (typically a new-Pod startup window where the gateway's Pod informer has not yet observed the calling Pod), the gateway falls back to a live `List Pods` against the API server in the cert-SAN-derived namespace, filtered by source IP, before considering the cross-check failed. This fallback collapses the new-Pod informer-lag window into the retryable `403 StalePodCompletion` path described below, rather than leaking it as a terminal `401`.
3. The resulting UID is compared against `AgentTask.status.currentPodUID`, resolved from the gateway's existing AgentTask watch (the same cache used for the `exitCode` short-circuit and the artifact-name validation below). Any call whose UID does not match is rejected with `403 access_denied`, `reason=StalePodCompletion`.
4. The gateway also rejects calls when `AgentTask.status.phase` is already terminal (`Succeeded` / `Failed` / `TimedOut`) with `403 access_denied`, `reason=TaskAlreadyCompleted`. Without this gate, the data the agent is trying to report would be silently dropped after the reconciler has finalized state.

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

The gateway validates artifact names against the task's `spec.artifacts` and returns `400 invalid_request` on mismatch. The exact rule splits by `status`:

- **`status: "success"`**: every declared name must be present, and no undeclared names may appear.
- **`status: "failure"`**: only the no-undeclared-names rule is enforced. A failing task may report a subset of declared artifacts (or none) and still have its failure recorded. This carve-out exists so an agent that crashes before producing its full deliverable set can still report failure, rather than being rejected at the gateway for the missing artifacts it never got to produce.

`error.message` names the offending key in either branch, for example `"missing declared artifact: pr-url"` (success-only) or `"undeclared artifact in payload: extra-key"` (both branches).

The gateway resolves `spec.artifacts` from its existing cluster-wide `AgentTask` watch (the same cache it uses for the `exitCode` short-circuit), so the name check runs synchronously: the agent learns of a mismatch before exiting, rather than discovering it later via `AgentTask.status`.

The AgentTaskReconciler defensively re-validates artifact names against `spec.artifacts` using the same per-status rule when reading the ConfigMap (strict on `status: "success"`, no-undeclared-only on `status: "failure"`). This is a cheap loop, belt-and-suspenders against any future RBAC drift on the per-task `update, patch` Role. The reconciler remains the final authority on `AgentTask` state, but under normal operation the gateway-side check makes the reconciler's re-check a no-op.

## Response codes

| Code | `error.type` | `retryable` | Condition |
|---|---|---|---|
| `200 OK` | (none) | n/a | Success; empty body |
| `400 Bad Request` | `invalid_request` | `false` | Malformed body or artifact-name rule violation |
| `401 Unauthorized` | `unauthorized` | `false` | Source-IP → Pod cross-check failed, even after the live API-server fallback |
| `403 Forbidden` | `access_denied` | `false`, except `true` for `StalePodCompletion` | One of four reason codes, see below |
| `413 Payload Too Large` | `request_too_large` | `false` | Per-artifact or combined size cap exceeded |
| `503 Service Unavailable` | `internal_unavailable` | `true` | The ConfigMap `Patch` itself failed |

The `error.type` names reuse the vocabulary of the [LLM Gateway](errors.md#llm-gateway-error-responses) and [User Gateway](errors.md#user-gateway-error-responses) error tables.

### 400 Bad Request

Returned when:

- the request body is not valid JSON;
- the required `status` field is missing or has a value other than `"success"` or `"failure"`;
- `artifacts` is present but is not an object of string-to-string entries; or
- the artifact names in `artifacts` violate the per-`status` validation rule described under [Artifact name validation](#artifact-name-validation).

The gateway validates the body before patching the ConfigMap, so a `400` means no state change.

### 401 Unauthorized

Returned when the source-IP → Pod cross-check fails *after* the live API-server fallback described under [The identity gate](#the-identity-gate): the source IP does not resolve to any Pod in the cert-SAN-derived namespace via the informer cache **and** a live `List Pods` query. Typical causes are off-cluster spoofing, or a Pod terminated and removed by kubelet between dial and handle. Same envelope shape as the [LLM Gateway 401 row](errors.md#llm-gateway-error-responses), `error.type: unauthorized`, `retryable: false`: the cross-check has already exhausted both the informer cache and a live API query, so the failure is structural and a fresh attempt hits the same condition. Agents under normal operation should not observe this; it is documented for completeness.

### 403 Forbidden (four reasons)

| Reason | Condition |
|---|---|
| (a) `NotAgentTaskPod` | The calling Pod is not associated with an AgentTask |
| (b) `TaskNotAgentReported` | The calling AgentTask has `completion.condition: exitCode` |
| (c) `StalePodCompletion` | The calling Pod's UID does not match `AgentTask.status.currentPodUID` |
| (d) `TaskAlreadyCompleted` | `AgentTask.status.phase` is terminal (`Succeeded` / `Failed` / `TimedOut`) |

- **(b)**: `exitCode` tasks signal completion via container exit and have no gateway-side completion mailbox. The per-task completion ConfigMap and the per-task `update, patch` Role are only provisioned in `agentReported` mode (see [Per-Agent and Per-Task Child Resources](../../runtime/child-resources.md) and [AgentTask completion detection](../../controller/task-lifecycle.md)).
- **(c)**: this is the identity gate. It closes the in-flight stale-write race after a `backoffLimit` retry, where an old Pod's delayed completion would otherwise overwrite the new Pod's data. `error.message` names the expected and actual UIDs, for debuggability.
- **(d)**: the reconciler will not re-process the payload once the phase is terminal; absent this gate the agent's write would be silently dropped. `error.message` names the observed terminal phase.

The gateway resolves the calling task via the same `AgentTask` watch and short-circuits cases (b) through (d) with the relevant `reason` before attempting any patch.

### 413 Payload Too Large

Returned when:

- any single artifact value exceeds **4 KiB** (`error.message` names the offending artifact key); or
- the sum of `message` plus all artifact value bytes exceeds **32 KiB** (`error.message` names the combined-budget overflow).

Sizes are measured in UTF-8 bytes against the `message` and `artifacts` **value** strings only; artifact key bytes are not counted (keys are bounded by Kubernetes ConfigMap key naming rules). The combined cap exists because the body is buffered in gateway memory before validation and then patched into the per-task ConfigMap, which has the standard ~1 MiB Kubernetes object limit. Large artifacts should be stored externally and referenced by URL in the value.

### 503 Service Unavailable

Returned when the gateway's `Patch` against the per-task completion ConfigMap fails after admission and body validation have passed: apiserver transiently unavailable, etcd unreachable, `Patch` conflict, or RBAC drift on the per-task `update, patch` Role. This is distinct from `401` (source-IP → Pod cross-check) and `403` (identity / mode / phase gates), both of which fire before any apiserver write is attempted; `503` is reached only when the data-channel write itself fails.

`error.type: internal_unavailable`, `retryable: true`. The gateway sets `Retry-After: 1` (integer delta-seconds, RFC 7231 § 7.1.3) as a machine-readable cadence floor, mirroring the `504 controller_unavailable` pattern on the User Gateway. The 1s value is a floor: agents MUST wait at least 1 second before retrying, but MAY apply their own bounded-backoff schedule that waits longer per [The Runtime Contract item 6](../../runtime/contract.md). The 1s floor is conservative for apiserver-flap recovery.

## Race windows

Re-completion across a `backoffLimit` retry is the supported multi-call path. The reconciler:

1. clears `status.currentPodUID = ""`,
2. resets the ConfigMap to `data: {}`,
3. triggers the replacement Pod, and
4. stamps `status.currentPodUID = newPod.UID` once the new Pod is observed via the informer.

As a result, any in-flight `/v1/task/complete` from the terminated old Pod fails the identity gate, and the new Pod's `/v1/task/complete` lands on a fresh mailbox under the new UID.

There is a narrow **restamp-lag window** (typically <100ms of informer lag, versus seconds of agent startup) where the new Pod's first call may race the reconciler's UID stamp and receive `403 StalePodCompletion`. This is the transient, retryable flavor of that code. See [AgentTask State Machine](../../controller/task-lifecycle.md) for the clear/reset/create/restamp ordering.

## Retry guidance

- **`retryable: false`** on `400`, `413`, and 403 cases (a) `NotAgentTaskPod`, (b) `TaskNotAgentReported`, and (d) `TaskAlreadyCompleted`: these are structural conditions where a duplicate call from the same Pod hits the same outcome. In particular, on `TaskAlreadyCompleted` the task has reached a terminal phase and further completion writes are by-design rejected; the agent should log and exit.
- **`retryable: true`** on 403 case (c) `StalePodCompletion` and on `503 internal_unavailable`. The reconciler-observation lag between Pod creation and `status.currentPodUID` being stamped is genuinely transient (typically <100ms), and the conditions behind a `503` are operationally transient too: an apiserver flap, a leader election, or brief etcd unavailability.

Agents SHOULD retry both retryable cases with bounded backoff per [The Runtime Contract item 6](../../runtime/contract.md): the suggested schedule is **100ms, 500ms, 2s** (3 attempts max). For `503`, the `Retry-After: 1` floor described above additionally applies.

## Error envelope

Error responses (`400`, `403`, `413`, `503`) carry the structured `{ "error": { "type", "message", "retryable" } }` envelope, same shape as the [User Gateway Error Responses](errors.md#user-gateway-error-responses).

`error.type` is `invalid_request` for 400, `access_denied` for 403, `request_too_large` for 413, and `internal_unavailable` for 503. `error.message` carries the human-readable diagnostic: the offending artifact name for the name-match `400`, the offending artifact key for the 4 KiB cap, the combined-budget overflow for the 32 KiB cap, the specific 400/403 reason, or the underlying apiserver failure class for `503`.
