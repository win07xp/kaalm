# Giving it a job

An agent is something you talk to; it sits there waiting. A **task** is
something you fire off: it starts, does one piece of work, reports what
happened, and goes away.

Same class, entirely different lifecycle. The image changes too, to the Go
sibling of the Python image your agent runs: a task brings its whole program rather
than a mounted handler, and the Go reference image has a tutorial shortcut
built in that you will use in a moment.

## Run one

This is the e2e suite's `test/e2e/testdata/agenttask.yaml` with this book's
names, the published Go image, and a 60-second TTL. Put it in `task.yaml`:

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentTask
metadata:
  name: one-off-job
  namespace: default
spec:
  agentClassRef:
    name: tutorial
  image: ghcr.io/win07xp/kaalm-agent-go:0.7.0
  env:
    - name: KAALM_TASK_AUTOCOMPLETE
      value: success
  completion:
    condition: agentReported
    timeout: 2m
    onTimeout: Fail
  ttlSecondsAfterFinished: 60
```

`completion.condition: agentReported` is the interesting line. The task is not
finished when the program exits; it is finished when the program *says* it is,
by calling the gateway to report a result. That distinction matters for real
work: a coding agent that opens a pull request wants to hand back the URL, not
an exit code alone.

The Go reference image has a shortcut for this tutorial:
`KAALM_TASK_AUTOCOMPLETE` makes it report `success` as soon as it starts, so
you get to watch the lifecycle without writing any logic. A real task image
carries code that does the work and reports its own result.

`timeout: 2m` with `onTimeout: Fail` bounds it, and `ttlSecondsAfterFinished`
is how long the finished task and its pod are kept before Kaalm deletes them.

```bash
kubectl apply -f task.yaml
```

## Watch it run

```bash
kubectl get agenttasks
```

The task passes through `Pending`, `Provisioning`, and `Running`, reports,
and settles. A few seconds after you applied it:

```
NAME          PHASE       CLASS      AGE
one-off-job   Succeeded   tutorial   23s
```

## Read what it reported

```bash
kubectl get agenttask one-off-job \
  -o jsonpath='{.status.agentReportedStatus} / {.status.agentReportedMessage}{"\n"}'
```

```
success / auto-complete on startup
```

That came from the program itself, through the gateway, into the task's status.
A real task reports its own result the same way, and can attach named outputs
alongside it, which is how you get a pull request URL back out; the design
book's [Task completion](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/api/task-complete.md) has the call.

## It cleans up after itself

Wait a minute, then look for the pod that ran the work, and for the task:

```bash
kubectl get pods -l kaalm.io/task=one-off-job
kubectl get agenttasks
```

```
No resources found in default namespace.
No resources found in default namespace.
```

Both gone, because `ttlSecondsAfterFinished: 60` said so. The field works
the way it does on a Kubernetes Job: after a finished task has outlived its
TTL, the whole record is collected, the task, its pod, everything it made.
Sixty seconds is tutorial-sized; a real pipeline keeps the record for an
hour or a day, and a task with no TTL at all stays until someone deletes
it. Read the result before the TTL you chose, or set none.

This is the difference between the two kinds of work. Your agent has a name,
storage, and an address, and it stays. A task appears, does one job, reports,
and is collected on the schedule you set. Kaalm runs both because agent work
comes in both kinds.

Next: [Sleep and wake](sleep-and-wake.md).
