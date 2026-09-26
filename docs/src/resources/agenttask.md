# AgentTask

AgentTask is a namespace-scoped resource representing an ephemeral, goal-driven agent workload. It is analogous to a Kubernetes Job: it runs once, pursues a completion condition, produces artifacts, and terminates.

Where an [Agent](agent.md) is a long-running service, an AgentTask has a beginning and an end. The spec therefore answers two questions a Job never has to for an AI workload: how the system knows the task is done (`spec.completion`), and how the task hands back its results (`spec.artifacts`).

## Spec

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentTask
metadata:
  name: fix-issue-342
  namespace: team-support
spec:
  # Required. Rule 1.
  agentClassRef:
    name: sandboxed

  # Must match the class allowlist (rule 2). Defaults from the class.
  image: "registry.internal.corp/agents/coder:v1.0.0"
  # Merged with the injected KAALM_* set. There is no command or args
  # override on a task; the image's entrypoint is the task.
  env:
    - name: TASK_GOAL
      value: "Fix GitHub issue #342 in repo acme/widgets and open a PR"
    - name: GITHUB_TOKEN
      valueFrom:
        secretKeyRef: { name: github-bot-token, key: token }

  # Optional. Rules 3 to 5.
  providers:
    - providerRef: { name: anthropic-shared }

  # Optional. Rules 35 to 38; a violation settles the task as Failed, since
  # tasks have no Degraded phase.
  tools:
    - providerRef: { name: search-tools }
      tools: ["web_search"]

  # Clamped to the class maxLimits (rule 6).
  resources:
    requests: { cpu: "1", memory: "2Gi" }
    limits:   { cpu: "2", memory: "4Gi" }

  # Workspace PVC for the task's lifetime. Requires persistence.enabled on
  # the class (rule 24). No existingClaim on a task.
  persistence:
    enabled: true
    sizeGi: 10
    # Default /var/task/workspace.
    mountPath: "/workspace"

  completion:
    # "agentReported" (schema default): the container POSTs
    # /v1/task/complete. "exitCode": the task is complete when the
    # container exits.
    condition: agentReported
    # Bounds running time, measured from status.startTime. Unset means no
    # bound: the task runs until it reports or exits.
    timeout: "1h"
    # "Fail" (schema default) settles a timeout as TimedOut, exempt from
    # backoffLimit. "Succeed" settles it as Succeeded with any partial
    # agent-reported payload kept.
    onTimeout: Fail
    # Pod recreations before Failed is terminal. Count-based only.
    backoffLimit: 0

  # Names the container includes in its POST /v1/task/complete body. Only
  # valid with condition agentReported (rule 17, apply time).
  artifacts:
    - name: pr-url
    - name: summary

  # Seconds to keep the resource after it settles. Unset means it is never
  # cleaned up.
  ttlSecondsAfterFinished: 3600
```

`kubectl get at` prints the phase and the class.

## Status

```yaml
status:
  observedGeneration: 1
  phase: Succeeded
  conditions:
    - type: Completed
      status: "True"
      reason: TaskSucceeded
      message: "PR opened successfully"
  startTime: "2026-04-05T11:05:12Z"
  completionTime: "2026-04-05T11:30:42Z"
  podName: "fix-issue-342-xk9p2"
  currentPodUID: "9d3e2c1b-4a5f-6d7e-8c9b-1a2f3e4d5c6b"
  retries: 0
  artifactValues:
    pr-url: "https://github.com/acme/widgets/pull/587"
    summary: "Fixed null pointer in WidgetService.get(). Added regression test."
  agentReportedStatus: "success"
  agentReportedMessage: "PR opened successfully"
```

| Field | Meaning |
|---|---|
| `phase` | One of `Pending`, `Provisioning`, `Running`, `Completing`, `Succeeded`, `Failed`, `TimedOut`, `Terminating`. The transitions are on [Task lifecycle](../controller/task-lifecycle.md). |
| `Completed` | `True` with `reason: TaskSucceeded` or `TaskFailed` once the task settles; the message is the agent's reported message, the container's exit summary, or the validation failure. |
| `startTime` | Stamped on the transition to `Running` (Pod Ready), in the same status write. `spec.completion.timeout` measures from it, so scheduling and image-pull time never count; `Provisioning` is bounded separately. |
| `completionTime` | Stamped when the task settles. |
| `podName` | The current Pod. |
| `currentPodUID` | For an `agentReported` task, the UID of the Pod allowed to report completion, stamped on every Pod creation and cleared during a retry reset. Never set for an `exitCode` task. |
| `retries` | Incremented at the start of each `backoffLimit` retry cycle and compared with the limit to decide whether `Failed` is terminal ([Retry mechanics](../controller/task-lifecycle.md#retry-mechanics)). |
| `artifactValues` | The values the container reported, keyed by declared name. |
| `agentReportedStatus`, `agentReportedMessage` | The `status` (`success` or `failure`) and `message` from the completion report. |

## Design notes

### Task names must be DNS-1123 labels

`metadata.name` carries the same root-scoped rule as the Agent schema (rule 21), for the same reason: the task name becomes one DNS label in the `{name}.{namespace}.task.kaalm.io` SAN, and the gateway reads the namespace by position ([Name validation](agent.md#name-validation-dns-1123-label-enforced-at-the-schema-root)).

### Completion modes

**`agentReported` is the default.** The container calls [`POST /v1/task/complete`](../gateways/api/task-complete.md) with a status, a message, and artifact values in one call, which is more than an exit code can carry.

**`exitCode` collects no artifacts.** Artifacts travel in the completion payload, which only `agentReported` mode sends. Declaring `spec.artifacts` with `condition: exitCode` is rejected at apply time (rule 17). An `exitCode` task that produces output writes it to an external system and relies on container logs for status.

**There is no webhook condition and no retry on timeout.** The schema enums bound `condition` to `agentReported` and `exitCode` and `onTimeout` to `Fail` and `Succeed`.

### Timeout and retention are unbounded by default

`completion.timeout` has no default. Unset, the task runs until it reports or exits; an `agentReported` task whose container never reports holds its Pod, PVC, and certificate indefinitely. `ttlSecondsAfterFinished` has no default either: unset, a settled task and its children are never removed. As shipped, no AgentClass field bounds or defaults either value, unlike the idle and hibernation timings for Agents; issue #239 tracks it. Set both on every task.

### Artifact collection

Artifacts are declared by name; the container reports values keyed by name. The gateway validates the names against `spec.artifacts` and the per-artifact and total size caps before writing the completion mailbox, and answers synchronously so the container can log and exit non-zero ([Task completion](../gateways/api/task-complete.md)). The reconciler re-validates when it reads the mailbox. Large outputs are externalized (object storage, Git) and referenced by URL in the value; there is no spill into ConfigMaps.

This payload-based design has no race, needs no `pods/exec` RBAC, and keeps the artifact contract small.

### The completion protocol: data channel and identity gate

The gateway and the reconciler coordinate completion through two mechanisms:

- The per-task `{taskName}-completion` ConfigMap is the data channel. The gateway writes the completion payload; the reconciler watches it ([The completion mailbox](../runtime/child-resources.md#the-completion-mailbox)).
- `status.currentPodUID` is the identity gate. The reconciler stamps it on every Pod creation of an `agentReported` task and clears it during the retry-reset window; the gateway rejects a report from any other Pod with `409 stale_pod` and a `StalePodCompletion` message, and a report against a settled task with `403 access_denied` and `TaskAlreadyCompleted`.

The wire-level contract is on [Task completion](../gateways/api/task-complete.md), and the clear, reset, create, and restamp order on [Retry mechanics](../controller/task-lifecycle.md#retry-mechanics).

### Concurrency

Unlike a Job, an AgentTask is always one Pod. There is no parallelism field, and fan-out is not a property of the resource.

### Runtime-contract guarantees

The task Pod gets the same injected `$KAALM_*` set as an Agent, no probes, and a per-task certificate with `client auth` only, since a task has no listener. The obligations on the image are [The runtime contract](../runtime/contract.md), items 3 and 6; the child objects are on [AgentTask child resources](../runtime/child-resources.md#agenttask-child-resources).
