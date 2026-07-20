# starter-go

A minimal, working implementation of the [Agentry Runtime Contract](../../docs/src/runtime/contract.md).
Copy this directory, replace one function, and you have a compliant agent image.
This is not a framework and not a base image: after `cp` you own the code.

## What it implements for you

Every item of the runtime contract, so you don't rebuild the error-prone parts:

- HTTPS serving on `$AGENTRY_HEALTH_PORT` with `/livez` and `/readyz` (item 1).
- Graceful SIGTERM draining (item 2).
- mTLS client-cert presentation and CA trust on all gateway calls (item 3).
- `POST /v1/message` with per-path mTLS verification: `401` without a client
  cert, `403` unless the SAN is the gateway Service DNS (item 4).
- Cert/CA reload on rotation, watching the mount **directory** for the `..data`
  swap rather than the leaf files (item 4). This is the subtlest part: a
  leaf-path watch silently misses every rotation.
- `messageId` deduplication over the last 1024 IDs, returning the cached reply
  for gateway-retry duplicates (item 7).
- A heartbeat loop that runs in Agent mode only, detecting AgentTask mode from
  the client cert SAN (item 5).
- A `completeTask` helper with the bounded `StalePodCompletion` retry (item 6).

## What you change

Exactly one function, `handleMessage` in `handler.go`. Everything else is
contract boilerplate. To call an LLM, POST to `a.gatewayURL` using the
pre-configured mTLS client `a.gatewayCli`; the gateway proxies to your
ModelProviders.

## Environment

The controller injects the `$AGENTRY_*` runtime-contract variables. One
template-specific toggle:

| Variable | Default | Meaning |
|---|---|---|
| `AGENTRY_TEMPLATE_HEARTBEAT` | `auto` | `auto` emits every 30s in Agent mode only; `off` never emits. |

**Hibernation footgun:** the heartbeat is unconditional, so it is only safe
with the default `activitySource: gatewayTraffic`. Setting `agentHeartbeat` or
`both` while this loop runs keeps the agent permanently non-idle and it will
never hibernate. Either leave `activitySource` at the default, or set
`AGENTRY_TEMPLATE_HEARTBEAT=off` and gate emission on real work yourself.

## Deploy a test Agent

```bash
docker build -t registry.example/agents/starter-go:v1 .
# push, or import into your local cluster

kubectl apply -f - <<'EOF'
apiVersion: agentry.io/v1alpha1
kind: AgentClass
metadata:
  name: starter
spec:
  image:
    allowedImages: ["registry.example/agents/*"]
---
apiVersion: agentry.io/v1alpha1
kind: Agent
metadata:
  name: starter-go
  namespace: default
spec:
  agentClassRef: { name: starter }
  image: "registry.example/agents/starter-go:v1"
EOF
```

Add an [AgentChannel](../../docs/src/resources/agentchannel.md) pointing at the
Agent to route webhook traffic to `/v1/message`.

## As an AgentTask

The same image runs as an AgentTask. Call `a.completeTask(ctx, "success",
"done", map[string]string{"result": "..."})` from your task logic. The template
detects task mode from the cert SAN and does not start the heartbeat loop.

For smoke and e2e runs, set `AGENTRY_TASK_AUTOCOMPLETE=success` (via the
AgentTask `spec.env`) to have the task report that status on startup through
`completeTask`. Leave it unset in real tasks, which report completion from
their own work.
