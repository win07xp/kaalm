# Giving It a Job

An agent is something you talk to; it sits there waiting. A **task** is
something you fire off: it starts, does one piece of work, reports what
happened, and goes away.

Same image, same class, entirely different lifecycle.

## Run one

Put this in `task.yaml`:

```yaml
apiVersion: kaalm.io/v1alpha1
kind: AgentTask
metadata:
  name: one-off-job
  namespace: default
spec:
  agentClassRef:
    name: tutorial
  image: my-agent:1
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
just an exit code.

The starter image has a shortcut for this tutorial: `KAALM_TASK_AUTOCOMPLETE`
makes it report `success` as soon as it starts, so you get to watch the
lifecycle without writing any logic.

`timeout: 2m` with `onTimeout: Fail` bounds it, and `ttlSecondsAfterFinished`
is how long the remains stick around before cleanup.

```bash
kubectl apply -f task.yaml
```

## Watch it run

```bash
kubectl get agenttasks
```

Run it a few times and you will see it move:

```
t=5s   phase=Provisioning
t=10s  phase=Running
t=15s  phase=Succeeded
```

```
NAME          PHASE       CLASS      AGE
one-off-job   Succeeded   tutorial   10s
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
alongside it, which is how you get a pull request URL back out.

## It cleans up after itself

Wait a minute, then look for the pod that ran the work:

```bash
kubectl get pods -l kaalm.io/task=one-off-job
```

```
No resources found in default namespace.
```

Gone, because `ttlSecondsAfterFinished: 60` said so. The task object itself
remains, so the record of what happened survives, but the container that did
the work is not sitting around consuming memory.

This is the difference between the two shapes. Your agent is a pet: it has a
name, storage, and an address, and it stays. A task is cattle: it appears, does
one job, reports, and is collected. Kaalm runs both because agent work comes in
both shapes.

Next: [Sleep and Wake](sleep-and-wake.md).
