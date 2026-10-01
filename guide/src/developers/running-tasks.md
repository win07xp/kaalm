# Running tasks

An AgentTask is a run-to-completion agent: point it at work, let it finish,
read the result, and let the TTL clean it up. No Service, no channel, no
hibernation; only a Pod with an identity and gateway access.

## Declaring a task

A task you can run under the sample class with no code of your own, on the
published Go base image:

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
starter templates. In a task, any non-empty value is sent as the completion
status, `success` or `failure`, shortly after the container starts. The hook
reports alongside whatever else the task does and does not replace it. Your
task image reports completion itself, from its own code. The e2e suite's
fixture, `test/e2e/testdata/agenttask.yaml`, has the same fields. The
[Reference base images](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/base-images.md#the-kaalm_task_autocomplete-hook)
page has the hook's retry rules.

## Writing a task in Python

A Python task is an image built `FROM` the Python base image, with a
`handler.py` that defines `run_task`. Set `KAALM_HANDLER_PATH` in the
Dockerfile, because the controller sets it only for Agents with
`spec.handler`, and `spec.handler` stays Agent-only:

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
    await kaalm.complete_task("success", "done", {"report-url": report_url})
```

The runtime starts `run_task` once, after its HTTPS server is listening, and
only when the container runs as an AgentTask. `handle_message` is still
required.

- `kaalm.complete_task(status, message="", artifacts=None)` reports the
  result. `status` is `"success"` or `"failure"`, and `artifacts` is a dict
  of strings. It retries the `409 stale_pod` answer for you.
- If `run_task` returns without reporting, the runtime reports `success`.
  If it raises, the runtime reports `failure` with the exception text.
- If the task is already finished, `kaalm.complete_task` raises
  `kaalm.TaskAlreadyCompleted`. Do not retry; let the error end the run or
  catch it and return.
- A `run_task` that is not `async def`, or that takes required arguments,
  stops the container at startup with an error in its log.

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
  declared in this mode; there is nobody to report them.

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
mailbox in your namespace, named `TASK_NAME-completion`. The gateway writes
it through a per-task Role, and an identity check on the reporting call
accepts the report only from the task's current Pod.

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
while the gateway's Pod index catches up; `CompleteTask` in Go and
`kaalm.complete_task` in Python retry that on their own, and a task image of
your own must too (runtime contract item 6).

## Cleanup and retries

- `ttlSecondsAfterFinished` deletes the AgentTask itself once the TTL has
  passed since completion, and with it the Pod, the mailbox, and the task's
  PVC, the same way a Job's TTL works. Read the result before then. If you
  leave the field unset, the class's default TTL applies, and the class can
  cap the value you set. The `standard` class sets neither, so a task with no
  TTL keeps its record. To keep a finished task longer, raise its TTL; the
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
