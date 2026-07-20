# starter-python

A minimal, working implementation of the [Agentry Runtime Contract](../../docs/src/runtime/contract.md),
built on `aiohttp`. Feature-parity with [starter-go](../starter-go). Copy this
directory, replace one function, and you have a compliant agent image.

## What it implements for you

- HTTPS serving on `$AGENTRY_HEALTH_PORT` with `/livez` and `/readyz` (item 1).
- Graceful SIGTERM draining (item 2).
- mTLS client-cert presentation and CA trust on all gateway calls (item 3).
- `POST /v1/message` with per-path mTLS: `401` without a client cert, `403`
  unless the SAN is the gateway Service DNS (item 4).
- Cert/CA reload on rotation via a `watchdog` observer on the mount
  **directory** (the `..data` swap), swapping both SSL contexts (item 4).
- `messageId` deduplication over the last 1024 IDs (item 7).
- A heartbeat loop in Agent mode only, detected from the client cert SAN
  (item 5), toggled by `AGENTRY_TEMPLATE_HEARTBEAT` (`auto` default / `off`).
- A `complete_task` coroutine with the bounded `StalePodCompletion` retry
  (item 6).

## What you change

Exactly one coroutine, `handle_message` in `agent/handler.py`. To call an LLM,
POST to `agent.gateway_url` with an `aiohttp` session using
`agent.reloader.client_context`; the gateway proxies to your ModelProviders.

## Environment

The controller injects the `$AGENTRY_*` runtime-contract variables.
`AGENTRY_TEMPLATE_HEARTBEAT` (`auto` default, `off` to suppress) is the one
template toggle. The same hibernation footgun applies as in
[starter-go](../starter-go): the heartbeat is unconditional, so keep
`activitySource` at the default `gatewayTraffic` unless you set the toggle to
`off` and gate emission on real work.

## Deploy

```bash
docker build -t registry.example/agents/starter-python:v1 .
# push, or import into your local cluster
kubectl apply -f - <<'EOF'
apiVersion: agentry.io/v1alpha1
kind: AgentClass
metadata: { name: starter-py }
spec:
  image:
    allowedImages: ["registry.example/agents/*"]
---
apiVersion: agentry.io/v1alpha1
kind: Agent
metadata:
  name: starter-python
  namespace: default
spec:
  agentClassRef: { name: starter-py }
  image: "registry.example/agents/starter-python:v1"
EOF
```
