# Running tasks

An AgentTask is a run-to-completion agent: point it at work, let it finish,
read the result, and let the TTL clean it up. No Service, no channel, no
hibernation; only a Pod with an identity and gateway access.

## Declaring a task

A task you can run under the sample class with no code of your own, on the
published Go base image (adapted from `test/e2e/testdata/agenttask.yaml`):

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentTask
metadata:
  name: nightly-report
  namespace: team-demo
spec:
  agentClassRef:
    name: standard
  image: ghcr.io/win07xp/kaalm-agent-go:1.0.0
  env:
    - name: KAALM_TASK_AUTOCOMPLETE
      value: success
  completion:
    condition: agentReported
    timeout: 10m
  ttlSecondsAfterFinished: 300
```

`KAALM_TASK_AUTOCOMPLETE` is a test hook in both base images and in the
starter templates. In a task, it reports its value as the completion status
shortly after the container starts, beside whatever else the task does. The
gateway accepts only `success` or `failure`. Your task image reports
completion itself, from its own code. The
[Reference base images](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/base-images.md#the-kaalm_task_autocomplete-hook)
page has the hook's retry rules.

## Writing a task in Python

A Python task is an image built `FROM` the Python base image, with a
`handler.py` that defines `run_task`. Set `KAALM_HANDLER_PATH` in the
Dockerfile: `spec.handler` is Agent-only, so the controller never sets it for
a task:

```dockerfile
FROM ghcr.io/win07xp/kaalm-agent-python:1.0.0
COPY handler.py /opt/kaalm/handler/handler.py
ENV KAALM_HANDLER_PATH=/opt/kaalm/handler
```

```python
import kaalm


def handle_message(envelope):
    # Required, but a task receives no messages.
    return {"content": ""}


async def run_task():
    report_url = await do_the_work()
    try:
        await kaalm.complete_task("success", "done", {"report-url": report_url})
    except kaalm.TaskAlreadyCompleted:
        pass  # something else already settled the task
```

The runtime runs `run_task` once, and only when the container runs as an
AgentTask. `handle_message` is still required.

- `kaalm.complete_task(status, message="", artifacts=None)` reports the
  result. `status` is `"success"` or `"failure"`, and `artifacts` is a dict
  of strings. It retries transport errors, the `409 stale_pod` answer, and the
  `503 internal_unavailable` answer for you. Any other refusal raises
  `RuntimeError`.
- If `run_task` returns without reporting, the runtime reports `success`
  with an empty message and no artifacts. A task that declares
  `spec.artifacts` must call `kaalm.complete_task` itself with them;
  otherwise the gateway rejects the automatic `success` with `400`, and the
  runtime does not retry it.
- If `run_task` raises, the runtime reports `failure` with the exception
  text. That includes an exception from `kaalm.complete_task` inside
  `run_task`.
- Once the gateway accepts a report, no other report is sent: a later
  `kaalm.complete_task` call raises `kaalm.TaskAlreadyCompleted` without
  sending, and so does a call after the gateway answers that the task is
  already finished. Do not retry; catch the error and return.
- In an `exitCode` task, the container's exit is the verdict. The gateway
  refuses every report with `403 TaskNotAgentReported`, and the runtime exits
  for you: 0 when `run_task` returned, 1 when it raised. Return to succeed,
  raise to fail, and do not call `kaalm.complete_task`. Its refusal raises
  `RuntimeError`, not `kaalm.TaskAlreadyCompleted`, so the `except` clause in
  the example does not catch it, and an error that escapes `run_task` exits 1
  even when the work succeeded. The example is for `agentReported` tasks.
- A `run_task` that is not `async def`, or that takes required arguments,
  stops the container at startup, and the Pod log names the error.

The [Reference base images](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/base-images.md#the-python-run_task-entry-point)
page has the full reporting and retry rules. In Go, your code provides `main()`
and calls `CompleteTask` itself.

## The two completion modes

- **`agentReported`**: the task calls the gateway's `POST /v1/task/complete`
  when done, carrying a status and any declared artifacts. This is the mode
  for agents that produce a result (the sample task in
  `config/samples/kaalm_v1beta1_agenttask.yaml` declares a `report-url`
  artifact). The base images and starter templates implement the call for
  you.
- **`exitCode`**: the container's exit status is the verdict; zero succeeds.
  Use this for agents that behave like batch jobs. Artifacts cannot be
  declared in this mode; there is nobody to report them. A Python
  task on the base image gets its exit code from the runtime; see
  [Writing a task in Python](#writing-a-task-in-python).

`completion.timeout` bounds the run either way. If you leave it unset, the
class's default timeout applies; under the chart's `standard` class, that's
one hour. The class can also cap the timeout you set. A task that runs out of
time settles `TimedOut` under the default `onTimeout: Fail`, or `Succeeded`
under `onTimeout: Succeed`; neither is retried.

## Watching and reading results

```bash
kubectl get agenttasks -n team-demo -w
```

`Phase` runs `Pending`, `Provisioning`, `Running`, `Completing`, then one of
`Succeeded`, `Failed`, or `TimedOut`:

```text
NAME             PHASE       CLASS      AGE
nightly-report   Running     standard   5s
nightly-report   Succeeded   standard   10s
```

For an `agentReported` task, the result lands in a per-task ConfigMap
mailbox in your namespace, named `TASK_NAME-completion`.

![AgentTask state machine, one trigger per edge. Pending to Provisioning on Certificate created, Provisioning to Running on Pod Ready, Running to Completing on completion, exit, or timeout, and Completing to Succeeded on success, to Failed on failure, or to TimedOut on a timeout with onTimeout Fail. Pending to Failed when the spec is irreconcilable, Provisioning to Failed when the Pod fails to start or the spec is irreconcilable, Running to Failed when the Pod is lost with an empty mailbox, and Failed back to Provisioning while retries are left. Any phase moves to Terminating when deleted or when the TTL expires.](../diagrams/task-lifecycle.svg)

```bash
kubectl get configmap nightly-report-completion -n team-demo -o jsonpath='{.data}'
```

```json
{"message": "auto-complete on startup", "status": "success"}
```

Artifacts appear as `artifact.ARTIFACT_NAME` keys next to the status. On a
`success` report every declared artifact is required: a completion call that
omits one is rejected with `400 invalid_request` naming the missing artifact,
and the task stays `Running` until it reports again or times out. A `failure`
report may carry any subset. The first completion call a Pod makes can also be
refused with `409 Conflict` and `error.type: stale_pod`
before the gateway has seen the new Pod; `CompleteTask` in Go and
`kaalm.complete_task` in Python retry that on their own, and a task image of
your own must too (runtime contract item 6).

## Cleanup and retries

- `ttlSecondsAfterFinished` deletes the AgentTask itself once the TTL has
  passed since completion, and with it the Pod, the mailbox, and the task's
  PVC. Read the result before then. If you leave the field unset, the
  class's default TTL applies, and the class can cap the value you set. The
  `standard` class sets no default TTL and no cap, so a task with no TTL
  keeps its record. To keep a finished task longer, raise its TTL; the
  change applies at once, up to the class cap in force when the task's Pod
  was created. The Pod is not stopped at completion: a container that keeps
  running after it reports stays up until the TTL.
- A crashed task Pod is retried only when `completion.backoffLimit` is above
  zero; the default is no retries. Class-gate failures (image not allowed,
  provider denied) settle `Failed` without retries, and
  `kubectl describe agenttask` names the reason.

---

*How this works: design book pages Resources, AgentTask (spec and phases),
Runtime, Child resources (the mailbox and per-task Role), and Controller,
Reconcilers (the retry and TTL logic).*
