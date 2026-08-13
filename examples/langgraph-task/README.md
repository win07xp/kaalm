# langgraph-task: a run-to-completion LangGraph worker as an AgentTask

A three-step summarize / critique / refine graph that runs once and reports
its result. This is the custom-image rung: an AgentTask's work is its whole
program (the base images' handler mount is an Agent-mode extension point),
so `main.py` owns the slice of the runtime contract a task needs and
nothing more.

What it proves:

- **A framework inside task mode.** The graph runs to completion, every
  model call brokered by the gateway with the pod's mTLS identity, and the
  result lands through `POST /v1/task/complete` with the contract's bounded
  retry (`StalePodCompletion` retried on 100ms / 500ms / 2s,
  `TaskAlreadyCompleted` terminal).
- **Goal in, artifact out.** The input arrives through the task's own
  `spec.env` (Kaalm injects no goal variables), and the declared `summary`
  artifact is reported on success, which the gateway validates against
  `spec.artifacts`.

Why raw certificate files are fine here, when the Agent examples use the
`kaalm` client factories: a task lives minutes, and the rotation edge those
factories close needs a pod that outlives most of a 90-day certificate.

## Build

```bash
docker build -t <your-registry>/langgraph-task:0.1.0 .
```

No base image and no build args: this rung owns everything.

## Run

Adjust `task.yaml` (class, provider, model, image, and the text to
summarize), then:

```bash
kubectl apply -f task.yaml
kubectl get agenttasks -w
```

The task walks Provisioning to Running to Completing to Succeeded; the
summary lands in the task's completion record.

The guide walks this example in Running Framework Agents
(`guide/src/developers/framework-agents.md`).
