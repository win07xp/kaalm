# starter-python

The worked example of the **`FROM` rung** of the Kaalm on-ramp: a
[reference base image](../../docs/src/runtime/base-images.md) implements the
entire [runtime contract](../../docs/src/runtime/contract.md), and this directory is
everything you own on top of it, a `handler.py` and a four-line Dockerfile.

The contract code lives only in the base image (`images/agent-python/` in
this repository), so this template doesn't carry a copy. Use this rung when
you have outgrown mount-and-run (`Agent.spec.handler`): you need pip
dependencies the base image does not bundle, or your handler exceeds the
ConfigMap size cap.

## What the base image implements for you

Everything the contract requires: HTTPS serving with `/livez` and `/readyz`,
per-path mTLS on `POST /v1/message`, cert and CA rotation reload, persistent
`messageId` dedup that survives hibernation, the Agent-mode heartbeat loop
with task-mode detection (`KAALM_TEMPLATE_HEARTBEAT`: `auto` default, `off` to
suppress), graceful SIGTERM, and the task-completion helper
(`kaalm.complete_task`). See
[Reference Base Images](../../docs/src/runtime/base-images.md).

## What you change

One required function: `handle_message(envelope)` in `handler.py`, sync or
async. An image that runs as an AgentTask may also define an optional
`async def run_task()` in the same file. Capabilities come from `import kaalm`:

- `await kaalm.gateway.post("/v1/chat/completions", json={...})` calls an LLM
  through the gateway with the Pod's mTLS identity (use a qualified model
  name like `anthropic-shared/claude-opus-4-6`).
- `kaalm.memory.get/put/delete` is persistent state: PVC-backed when
  `spec.persistence` is enabled (so it survives hibernation), in-memory
  otherwise.
- `await kaalm.complete_task(status, message="", artifacts=None)` reports an
  AgentTask's result, and `kaalm.TaskAlreadyCompleted` is the exception it
  raises when the task is already finished or has already reported.

Extra dependencies go into the Dockerfile as a
`RUN pip install --no-cache-dir <packages>` line before the `COPY`.

## Build and deploy

```bash
docker build -t registry.example/agents/starter-python:v1 .
# push, or import into your local cluster
kubectl apply -f - <<'EOF'
apiVersion: kaalm.io/v1beta1
kind: AgentClass
metadata: { name: starter-py }
spec:
  image:
    allowedImages: ["registry.example/agents/*"]
---
apiVersion: kaalm.io/v1beta1
kind: Agent
metadata:
  name: starter-python
  namespace: default
spec:
  agentClassRef: { name: starter-py }
  image: "registry.example/agents/starter-python:v1"
EOF
```

The `FROM` tag pins a published release; on an unreleased tree, build the base
image locally first (`make python-image PYTHON_AGENT_IMG=kaalm-agent-python:dev`)
and pass `--build-arg BASE=kaalm-agent-python:dev`.

A baked handler needs no `spec.handler` and no `allowHandlerMounts`
grant: those govern ConfigMap-mounted code. A `FROM` image goes through
ordinary image review and the `allowedImages` gate, like any custom image.

## As an AgentTask

The same image runs as an AgentTask. The runtime detects task mode from the
certificate SAN, does not start the heartbeat loop, and starts `run_task` once
if `handler.py` defines it. When `run_task` returns, the runtime reports
`success` with an empty message and no artifacts; when it raises, the runtime
reports `failure` with the exception text. A task that declares
`spec.artifacts` must call `kaalm.complete_task` from `run_task` with them,
because the gateway rejects an automatic `success` that omits one;
`kaalm.complete_task` also reports a message. It retries transport errors,
`409 stale_pod`, and `503 internal_unavailable` for you. See [Task mode](../../docs/src/runtime/base-images.md#task-mode) for
the full rules.

For smoke and e2e runs, set `KAALM_TASK_AUTOCOMPLETE=success` (in the
AgentTask `spec.env`) to have the task report that status on startup. Leave it
unset in real tasks, which report completion from their own work.
