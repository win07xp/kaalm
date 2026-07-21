# Running Tasks

An AgentTask is a run-to-completion agent: point it at work, let it finish,
read the result, and let the TTL clean it up. No Service, no channel, no
hibernation; just a Pod with an identity and gateway access.

## Declaring a task

The e2e suite's own fixture, from `test/e2e/testdata/agenttask.yaml`:

```yaml
apiVersion: kaalm.io/v1alpha1
kind: AgentTask
metadata:
  name: e2e-task
  namespace: e2e
spec:
  agentClassRef:
    name: e2e-standard
  image: registry.test/agents/starter-go:e2e
  env:
    - name: KAALM_TASK_AUTOCOMPLETE
      value: success
  completion:
    condition: agentReported
    timeout: 2m
    onTimeout: Fail
  ttlSecondsAfterFinished: 30
```

(The `KAALM_TASK_AUTOCOMPLETE` variable is a starter-template test hook;
your task image reports completion itself.)

## The two completion modes

- **`agentReported`**: the task calls the gateway's `POST /v1/task/complete`
  when done, carrying a status and any declared artifacts. This is the mode
  for agents that produce a result (the sample task in
  `config/samples/kaalm_v1alpha1_agenttask.yaml` declares a `report-url`
  artifact). The starter templates implement the call for you.
- **`exitCode`**: the container's exit status is the verdict; zero succeeds.
  Use this for agents that behave like batch jobs. Artifacts cannot be
  declared in this mode; there is nobody to report them.

`completion.timeout` bounds the run either way, and `onTimeout` decides
whether a timeout is a failure.

## Watching and reading results

```bash
kubectl get agenttasks -w
```

`Phase` runs `Provisioning`, `Running`, then `Succeeded` or `Failed`. For an
`agentReported` task, the result lands in a per-task ConfigMap mailbox in
your namespace (name: `<task-name>-completion`); the task Pod itself is the
only writer the mailbox trusts, enforced by a per-task Role and an identity
check on the reporting call.

```bash
kubectl get configmap e2e-task-completion -o yaml
```

Artifacts appear as `artifact.<name>` keys next to the status.

## Cleanup and retries

- `ttlSecondsAfterFinished` garbage-collects the finished Pod and its
  children; the AgentTask resource itself stays, holding the final status.
- A crashed task Pod is retried; an interrupted retry resumes. Terminal
  failures (image not allowed, provider denied) fail fast without retries,
  and `kubectl describe agenttask` names the reason.

---

*How this works: design book pages Resources, AgentTask (spec and phases),
Runtime, Child Resources (the mailbox and per-task Role), and Controller,
Reconcilers (the retry and TTL machinery).*
