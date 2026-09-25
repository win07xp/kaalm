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
  image: ghcr.io/win07xp/kaalm-agent-go:0.7.0
  env:
    - name: KAALM_TASK_AUTOCOMPLETE
      value: success
  completion:
    condition: agentReported
    timeout: 10m
  ttlSecondsAfterFinished: 300
```

`KAALM_TASK_AUTOCOMPLETE` is a test hook in the Go base image and the
Go starter template: the container reports the given status at startup
instead of doing work. The Python image ignores it. Your task image reports completion itself, from
its own code. The e2e suite's fixture, `test/e2e/testdata/agenttask.yaml`,
has the same fields.

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

`completion.timeout` bounds the run either way. A task that runs out of time
settles `TimedOut` under the default `onTimeout: Fail`, or `Succeeded` under
`onTimeout: Succeed`; neither is retried.

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
while the gateway's Pod index catches up; the base images retry that on their
own, and a task image of your own must too (runtime contract item 6).

## Cleanup and retries

- `ttlSecondsAfterFinished` deletes the AgentTask itself once the TTL has
  passed since completion, and with it the Pod, the mailbox, and the task's
  PVC, the same way a Job's TTL works. Read the result before then, or leave
  the field unset to keep the record. The Pod is not stopped at completion:
  a container that keeps running after it reports stays up until the TTL.
- A crashed task Pod is retried only when `completion.backoffLimit` is above
  zero; the default is no retries. Class-gate failures (image not allowed,
  provider denied) settle `Failed` without retries, and
  `kubectl describe agenttask` names the reason.

---

*How this works: design book pages Resources, AgentTask (spec and phases),
Runtime, Child resources (the mailbox and per-task Role), and Controller,
Reconcilers (the retry and TTL logic).*
