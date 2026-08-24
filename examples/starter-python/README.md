# starter-python

The worked example of the **`FROM` rung** of the Kaalm on-ramp: a
[reference base image](../../docs/src/runtime/base-images.md) owns the entire
[runtime contract](../../docs/src/runtime/contract.md), and this directory is
everything you own on top of it, a `handler.py` and a three-line Dockerfile.

Since v0.3.0 the contract code is single-sourced in the base image
(`images/agent-python/` in this repo); this template no longer carries a copy.
Reach for this rung when you have outgrown mount-and-run
(`Agent.spec.handler`): you need pip dependencies the base image does not
bundle, or your handler passed the ConfigMap size cap.

## What the base image implements for you

Everything the contract requires: HTTPS serving with `/livez` and `/readyz`,
per-path mTLS on `POST /v1/message`, cert and CA rotation reload, persistent
`messageId` dedup that survives hibernation, the Agent-mode heartbeat loop
with task-mode detection (`KAALM_TEMPLATE_HEARTBEAT`: `auto` default, `off` to
suppress), graceful SIGTERM, and the task-completion helper. See
[Reference Base Images](../../docs/src/runtime/base-images.md).

## What you change

Exactly one function: `handle_message(envelope)` in `handler.py`, sync or
async. Capabilities come from `import kaalm`:

- `await kaalm.gateway.post("/v1/chat/completions", json={...})` calls an LLM
  through the gateway with the Pod's mTLS identity (use a qualified model
  name like `anthropic-shared/claude-opus-4-6`).
- `kaalm.memory.get/put/delete` is persistent state: PVC-backed when
  `spec.persistence` is enabled (so it survives hibernation), in-memory
  otherwise.

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

The `FROM` tag pins a published release; on an unreleased tree build the base
image locally first (`make python-image PYTHON_AGENT_IMG=kaalm-agent-python:dev`)
and pass `--build-arg BASE=kaalm-agent-python:dev`.

Note the baked handler needs no `spec.handler` and no `allowHandlerMounts`
grant: those govern ConfigMap-mounted code. A `FROM` image goes through
ordinary image review and the `allowedImages` gate, like any custom image.
