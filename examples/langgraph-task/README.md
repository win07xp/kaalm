# langgraph-task: a run-to-completion LangGraph worker as an AgentTask

A three-step summarize / critique / refine graph that runs once and reports
its result. This is the custom-image rung: an AgentTask's work is its whole
program (the base images' handler mount is an Agent-mode extension point),
so `main.py` implements only the part of the runtime contract a task needs:
mTLS to the gateway and the completion report.

What it proves:

- **A framework inside task mode.** The graph runs to completion, every
  model call brokered by the gateway with the pod's mTLS identity, and the
  program reports the result through `POST /v1/task/complete` with the
  contract's bounded retry (`409 stale_pod` and `503 internal_unavailable`,
  the latter waiting at least `Retry-After`; `TaskAlreadyCompleted` terminal).
- **Goal in, artifact out.** The input arrives through the task's own
  `spec.env` (Kaalm injects no goal variables), and the declared `summary`
  artifact is reported on success, which the gateway validates against
  `spec.artifacts`.

This example reads the certificate files once, while the Agent examples use
the `kaalm` client factories, which reload the certificate after it rotates.
With the [default rotation settings](../../docs/src/security/tls.md#rotation-defaults),
a task's certificate rotates 60 days after it's issued. A task that finishes
in minutes ends long before then.

## Build

```bash
docker build -t <your-registry>/langgraph-task:0.1.0 .
```

No base image and no build args: `main.py` implements the mTLS client and
the completion report itself.

## Run

Adjust `task.yaml` (class, provider, model, image, and the text to
summarize), then:

```bash
kubectl apply -f task.yaml
kubectl get agenttasks -w
```

The task moves through the Pending, Provisioning, Running, Completing, and
Succeeded phases, and the summary appears in the task's
`status.artifactValues`.

The guide walks through this example in Running framework agents
(`guide/src/developers/framework-agents.md`).
