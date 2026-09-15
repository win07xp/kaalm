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
starter templates: the container reports the given status at startup
instead of doing work. Your task image reports completion itself, from
its own code. The e2e suite's fixture, `test/e2e/testdata/agenttask.yaml`,
is the same shape.

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

`completion.timeout` bounds the run either way, and `onTimeout` decides
whether a timeout is a failure.

## Watching and reading results

```bash
kubectl get agenttasks -n team-demo -w
```

`Phase` runs `Provisioning`, `Running`, then `Succeeded` or `Failed`:

```text
NAME             PHASE       CLASS      AGE
nightly-report   Running     standard   5s
nightly-report   Succeeded   standard   10s
```

For an `agentReported` task, the result lands in a per-task ConfigMap
mailbox in your namespace (name: `<task-name>-completion`); the task Pod
itself is the only writer the mailbox trusts, enforced by a per-task Role
and an identity check on the reporting call.

```bash
kubectl get configmap nightly-report-completion -n team-demo -o jsonpath='{.data}'
```

```json
{"message": "auto-complete on startup", "status": "success"}
```

Artifacts appear as `artifact.<name>` keys next to the status. A declared
artifact is required: a completion call that omits one is rejected with
`400 invalid_request` naming the missing artifact, and the task stays
`Running` until it reports again or times out. The first completion call a
Pod makes can also be refused with `401` while the gateway's Pod index
catches up; the base images retry that on their own, and a task image of your
own should too (runtime contract item 6).

## Cleanup and retries

- `ttlSecondsAfterFinished` deletes the AgentTask itself once the TTL has
  passed since completion, and with it the Pod, the mailbox, and the task's
  PVC, the same way a Job's TTL works. Read the result before then, or leave
  the field unset to keep the record. The Pod is not stopped at completion:
  a container that keeps running after it reports stays up until the TTL.
- A crashed task Pod is retried; an interrupted retry resumes. Terminal
  failures (image not allowed, provider denied) fail fast without retries,
  and `kubectl describe agenttask` names the reason.

---

*How this works: design book pages Resources, AgentTask (spec and phases),
Runtime, Child resources (the mailbox and per-task Role), and Controller,
Reconcilers (the retry and TTL logic).*
