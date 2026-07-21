# AgentTask

AgentTask is a namespace-scoped resource representing an ephemeral, goal-driven agent workload. It is analogous to a Kubernetes Job: it runs once, pursues a completion condition, produces artifacts, and terminates.

Where an [Agent](agent.md) is a long-running service, an AgentTask has a beginning and an end. The spec therefore centers on two questions a Job never has to answer for an AI workload: how does the system know the task is done (`spec.completion`), and how does the task hand back its results (`spec.artifacts`)?

## Spec

```yaml
apiVersion: kaalm.io/v1alpha1
kind: AgentTask
metadata:
  name: fix-issue-342
  namespace: team-support
spec:
  agentClassRef:
    name: sandboxed

  image: "registry.internal.corp/agents/coder:v1.0.0"
  env:
    - name: TASK_GOAL
      value: "Fix GitHub issue #342 in repo acme/widgets and open a PR"
    - name: GITHUB_TOKEN
      valueFrom:
        secretKeyRef: { name: github-bot-token, key: token }

  providers:
    - providerRef: { name: anthropic-shared }

  resources:
    requests: { cpu: "1", memory: "2Gi" }
    limits:   { cpu: "2", memory: "4Gi" }

  # Scratch persistence for the task. Lifecycle is tied to the AgentTask.
  # Setting enabled=true requires the referenced AgentClass to also have
  # persistence.enabled=true; see rule 24 in Cross-Resource Validation.
  persistence:
    enabled: true
    sizeGi: 10
    mountPath: "/workspace"

  # Completion semantics
  completion:
    # How the task signals completion.
    # "agentReported": agent POSTs to gateway /v1/task/complete
    # "exitCode": task is complete when the container exits 0
    # "webhook": external service calls a controller webhook (v1.1+, not in v1)
    condition: agentReported
    timeout: "1h"
    # What to do if timeout is hit before completion. "Fail" (default) settles
    # the task in phase=TimedOut, a failure-class terminal phase kept distinct
    # from Failed so timeouts are attributable and exempt from backoffLimit
    # retries. "Succeed" settles it in phase=Succeeded, keeping any partial
    # agent-reported payload best-effort. See the AgentTask lifecycle.
    onTimeout: Fail    # "Fail" | "Succeed" (rarely used) | "Retry" (v1.1+)
    # Retry on failure. v1 supports simple count-based retries, no backoff tuning.
    backoffLimit: 0

  # Artifacts to collect on completion. The agent includes values for these
  # names in the POST /v1/task/complete body.
  # Only valid with condition: agentReported. CRD schema enforces:
  # x-kubernetes-validations:
  #   - rule: "!has(self.artifacts) || size(self.artifacts) == 0 || !has(self.completion) || !has(self.completion.condition) || self.completion.condition != 'exitCode'"
  #     message: "artifacts cannot be collected with exitCode completion; use agentReported"
  # The has() guards are load-bearing: in CRD CEL, reading an absent optional
  # field is an evaluation error that FAILS validation. completion.condition
  # is defaulted at reconcile time (see Defaulting), so the stored spec may
  # omit it; an unguarded self.completion.condition would reject every
  # manifest that declares artifacts without an explicit completion block.
  artifacts:
    - name: pr-url
    - name: summary

  # Retention: how long to keep the AgentTask resource after completion.
  ttlSecondsAfterFinished: 3600
```

A few points worth calling out before the design notes:

- **Persistence is opt-in and gated by the class.** The scratch volume's lifecycle is tied to the AgentTask, and `persistence.enabled: true` is only allowed when the referenced AgentClass also has `persistence.enabled: true` (rule 24 in [Cross-Resource Validation](validation-and-defaulting.md#cross-resource-validation)).
- **The artifacts CEL rule looks over-defensive on purpose.** In CRD CEL, reading an absent optional field is an evaluation error, and an evaluation error fails validation. Because `completion.condition` is defaulted at reconcile time (see [Defaulting](validation-and-defaulting.md#defaulting)) rather than at admission, the stored spec may legitimately omit it. Without the `has()` guards, every manifest that declared artifacts but no explicit `completion` block would be rejected, even though its effective condition is the artifact-compatible default.
- **`timeout` measures execution, not scheduling.** It counts from `status.startTime`, which is stamped when the Pod becomes Ready (see [Status](#status) below).

## Status

```yaml
status:
  observedGeneration: 1
  phase: Succeeded   # Pending | Provisioning | Running | Completing | Succeeded | Failed | TimedOut | Terminating
  conditions:
    - type: Completed
      status: "True"
      reason: AgentReported
      message: "Agent reported completion at 2026-04-05T11:30:42Z"
  # Stamped when the task transitions Provisioning -> Running (Pod Ready), in
  # the same status write as the phase change. spec.completion.timeout measures
  # from startTime, so scheduling and image-pull time never count against it;
  # Provisioning is bounded separately (see the AgentTask lifecycle).
  startTime: "2026-04-05T11:05:12Z"
  completionTime: "2026-04-05T11:30:42Z"
  podName: "fix-issue-342-xk9p2"
  currentPodUID: "9d3e2c1b-4a5f-6d7e-8c9b-1a2f3e4d5c6b"
  # Incremented at the start of each backoffLimit retry cycle; compared
  # against spec.completion.backoffLimit to decide whether Failed is terminal.
  # See Retry mechanics.
  retries: 0
  # Artifact values captured inline. Oversize artifacts are rejected by the
  # gateway with HTTP 413; agents must externalize large outputs and pass a
  # reference URL inline (see design notes below).
  artifactValues:
    pr-url: "https://github.com/acme/widgets/pull/587"
    summary: "Fixed null pointer in WidgetService.get(). Added regression test."
  agentReportedStatus: "success"   # "success" | "failure"
  agentReportedMessage: "PR opened successfully"
```

Three status fields deserve emphasis:

- **`startTime`** is stamped when the task transitions Provisioning to Running (Pod Ready), in the same status write as the phase change. Because `spec.completion.timeout` measures from `startTime`, scheduling and image-pull time never count against the task's time budget; the Provisioning phase is bounded separately (see [AgentTask](../controller/task-lifecycle.md)).
- **`retries`** is incremented at the start of each `backoffLimit` retry cycle and compared against `spec.completion.backoffLimit` to decide whether `Failed` is terminal. See [Retry mechanics](../controller/task-lifecycle.md).
- **`currentPodUID`** identifies which Pod is currently allowed to report completion. Its role as an identity gate is explained under [the completion protocol](#the-completion-protocol-data-channel-and-identity-gate) below.

## Design Notes

### Task names must be DNS-1123 labels

`metadata.name` must be a DNS-1123 label, the same constraint and rationale as the [Agent CRD](agent.md), including the 63-character bound (the task name becomes a single DNS label in the SAN). It is enforced via the same **root-scoped** CRD CEL pattern: `x-kubernetes-validations: [{rule: "self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63", message: "AgentTask name must be a DNS-1123 label (no dots, max 63 characters)"}]` at the root of the AgentTask schema.

The constraint is load-bearing for workload identity: the gateway extracts the namespace from the `{name}.{namespace}.task.kaalm.io` SAN shape by splitting on `.` and reading label index 1. A dotted task name would shift the namespace label and defeat identification. See [Namespace Identification § Mode 1](../gateways/llm/workload-identity.md).

### Completion modes

**`completion.condition: agentReported` is the v1 default.** The agent container calls the gateway's completion endpoint with a status payload that may include artifact key-value pairs. This is more flexible than exit codes alone because the agent can report structured metadata and artifacts in a single call. See [POST /v1/task/complete](../gateways/api/task-complete.md) for the endpoint spec.

**`exitCode` does not support artifact collection.** Artifacts are collected via the `POST /v1/task/complete` payload, which is only used by `agentReported` mode. Declaring `spec.artifacts` with `completion.condition: exitCode` is rejected by CRD schema validation. Tasks using `exitCode` that need to produce output should write results to an external system (e.g., a Git repository, object storage) and rely on the container logs for status.

**`onTimeout: Retry` and `completion.condition: webhook` are intentionally deferred.** v1 is simple: one attempt, report or exit, collect artifacts, done. The CRD schema enforces this: `spec.completion.condition` accepts only `agentReported` and `exitCode` in v1 via `x-kubernetes-validations: [{rule: "self in ['agentReported', 'exitCode']", message: "webhook completion condition is not supported in v1"}]`, so invalid values are rejected at apply time.

### Artifact collection

Artifacts are declared by name in the spec; the agent includes artifact values (keyed by name) in the `POST /v1/task/complete` body. The gateway validates the payload's artifact names against `spec.artifacts` and returns `400 invalid_request` synchronously on mismatch. The rule splits by `status`:

- `status: "success"` requires every declared name present and no undeclared names.
- `status: "failure"` enforces only the no-undeclared-names half, so a failing task can report a subset of declared artifacts (or none).

See [POST /v1/task/complete](../gateways/api/task-complete.md) for the wire contract; the AgentTaskReconciler re-validates defensively when reading the ConfigMap.

This payload-based design eliminates race conditions, removes the need for `pods/exec` RBAC, gives the agent a synchronous error it can log and exit non-zero on, and keeps the artifact contract simple.

Artifact size limits apply: **4 KiB per artifact, 32 KiB total**. Oversize payloads are rejected at the gateway with HTTP 413; agents must externalize large outputs (object storage, Git, etc.) and place a reference URL in the artifact value. There is no auto-spill into ConfigMaps: the inline payload is the only delivery path.

Related: **`status.agentReportedStatus`** mirrors the `status` field from the agent's [`POST /v1/task/complete`](../gateways/api/task-complete.md) payload, `"success"` or `"failure"`. The gateway rejects other values with `400 invalid_request` synchronously, so `agentReportedStatus` always settles to one of those two when populated.

### The completion protocol: data channel and identity gate

The gateway and the reconciler coordinate completion through two mechanisms:

- The per-task `{taskName}-completion` ConfigMap is the **data** channel. The gateway patches it with the completion payload; the reconciler watches it for changes.
- `status.currentPodUID` is the **identity gate**. The AgentTaskReconciler stamps it with the current Pod's UID on every Pod creation (initial provisioning and `backoffLimit` retries) and clears it (`""`) during the retry-reset window. The gateway resolves the calling Pod's UID at `/v1/task/complete` admission and rejects mismatched callers with `403 access_denied` `reason=StalePodCompletion`.

Combined with a terminal-phase rejection (`reason=TaskAlreadyCompleted` when `status.phase ∈ {Succeeded, Failed, TimedOut}`), the identity gate prevents two failure modes the data-channel reset alone cannot close: stale writes from a terminated Pod (in-flight after a retry), and silent drops from a delayed second call against a completed task. See [/v1/task/complete](../gateways/api/task-complete.md) 403 cases (c) and (d) for the wire-level contract, and [Retry mechanics](../controller/task-lifecycle.md) for the clear/reset/create/restamp ordering.

### Retention and concurrency

**`ttlSecondsAfterFinished`** mirrors Job semantics. The controller garbage-collects the resource (and its Pod, PVC) after the TTL.

**Concurrency**: unlike Job, AgentTask is always parallelism=1 in v1. Parallel fan-out tasks would be a separate future resource (`AgentTaskSet`) rather than a field on AgentTask.

### Runtime-contract guarantees (same as Agent)

The AgentTaskReconciler injects the full `$KAALM_*` environment-variable set on the task Pod (`$KAALM_HEALTH_PORT`, `$KAALM_GATEWAY_ENDPOINT`, `$KAALM_CA_CERT`, `$KAALM_TLS_CERT`, `$KAALM_TLS_KEY`) and creates a per-task cert-manager `Certificate` (`{taskName}-tls`) with `usages: [client auth]`. The output Secret mounts at `/var/run/kaalm/`, so the task image presents a valid mTLS client cert on every call to `$KAALM_GATEWAY_ENDPOINT`: LLM requests and `POST /v1/task/complete`. Tasks send no heartbeats; `/v1/agent/heartbeat` is Agent-only and rejects per-task certs with 403.

The Certificate's SAN is `{taskName}.{namespace}.task.kaalm.io`, a non-Service shape, since tasks have no Service. See [AgentTaskReconciler](../controller/reconcilers.md#agenttaskreconciler) and [Namespace Identification](../gateways/llm/workload-identity.md) for the full flow.
