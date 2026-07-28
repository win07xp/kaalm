# starter-go

A complete Kaalm agent that imports the contract runtime from the
[`agentruntime` module](../../agentruntime) instead of vendoring it. Copy this
directory, replace the handler, and you own exactly your agent's logic: the
[runtime contract](../../docs/src/runtime/contract.md) stays Kaalm's code and
reaches you as a module update, not a re-copy.

This is the third rung of the
[on-ramp ladder](../../docs/src/runtime/base-images.md#relationship-to-the-starter-templates).
If you have not outgrown the first two (mount a handler ConfigMap into
`kaalm-agent-python`, or `FROM` a base image), start there instead.

## What the runtime module implements for you

Every item of the runtime contract, so you don't rebuild the error-prone
parts: HTTPS serving with `/livez` and `/readyz`, graceful draining, mTLS on
all gateway calls, per-path client-cert verification on `POST /v1/message`
(401 without a cert, 403 unless the SAN is the gateway Service DNS),
certificate rotation via the `..data` directory watch, `messageId`
deduplication persisted across hibernation, the Agent-mode heartbeat loop
with task-mode detection from the cert SAN, and the `CompleteTask` helper
with the bounded `StalePodCompletion` retry.

## What you change

`handler.go`. The handler closes over the `*agentruntime.Agent`, which is how
it reaches the runtime's capabilities:

- `a.Memory`: persistent key-value state (PVC-backed when the Agent enables
  persistence), for anything that must survive hibernation.
- `a.Gateway`: the preconfigured mTLS client. To call an LLM, POST a
  qualified model request to `/v1/chat/completions` through it; the gateway
  proxies to your ModelProviders.

`main.go` is wiring and should not need changes.

## Building a copy of this template

```bash
cp -r examples/starter-go my-agent && cd my-agent
go mod tidy    # pins the published agentruntime version, writes go.sum
docker build -t registry.example/agents/my-agent:v1 .
```

The [Dockerfile](Dockerfile) compiles your program and layers the binary onto
the `kaalm-agent-go` base image, replacing its default-handler binary at
`/kaalm-agent` (the image's entrypoint). Inside the Kaalm repo itself the
build works differently (the unreleased runtime is compiled from the tree via
`test/e2e/starter-go/Dockerfile`); your copy never needs that.

## Environment

The controller injects the `$KAALM_*` runtime-contract variables. One
runtime toggle:

| Variable | Default | Meaning |
|---|---|---|
| `KAALM_TEMPLATE_HEARTBEAT` | `auto` | `auto` emits every 30s in Agent mode only; `off` never emits. |

**Hibernation footgun:** the heartbeat is unconditional, so it is only safe
with the default `activitySource: gatewayTraffic`. Setting `agentHeartbeat` or
`both` while this loop runs keeps the agent permanently non-idle and it will
never hibernate. Either leave `activitySource` at the default, or set
`KAALM_TEMPLATE_HEARTBEAT=off` and gate emission on real work yourself.

## Deploy a test Agent

```bash
kubectl apply -f - <<'EOF'
apiVersion: kaalm.io/v1alpha1
kind: AgentClass
metadata:
  name: starter
spec:
  image:
    allowedImages: ["registry.example/agents/*"]
---
apiVersion: kaalm.io/v1alpha1
kind: Agent
metadata:
  name: my-agent
  namespace: default
spec:
  agentClassRef: { name: starter }
  image: "registry.example/agents/my-agent:v1"
EOF
```

Add an [AgentChannel](../../docs/src/resources/agentchannel.md) pointing at the
Agent to route webhook traffic to `/v1/message`.

## As an AgentTask

The same image runs as an AgentTask. Call `a.CompleteTask(ctx, "success",
"done", map[string]string{"result": "..."})` from your task logic. The runtime
detects task mode from the cert SAN and does not start the heartbeat loop.

For smoke and e2e runs, set `KAALM_TASK_AUTOCOMPLETE=success` (via the
AgentTask `spec.env`) to have the task report that status on startup through
`CompleteTask`. Leave it unset in real tasks, which report completion from
their own work.
