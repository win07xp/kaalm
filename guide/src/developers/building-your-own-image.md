# Building your own agent image

An agent image is any container that honors the runtime contract. Most
agents never need one: the ladder starts at a published base image with a
mounted handler ([Deploying from a base image](deploying-from-a-base-image.md)),
climbs to a `FROM` build on that base when you need extra dependencies, and
only ends here, at building the whole image, when you need another runtime or
full control. The templates under `examples/starter-go` and
`examples/starter-python` implement all of the contract; this page is the
contract from the implementer's side, so you can grow out of a template or
start clean.

## What the operator hands your container

Environment:

| Variable | Meaning |
|---|---|
| `KAALM_HEALTH_PORT` | The port to serve on (default 8080) |
| `KAALM_GATEWAY_ENDPOINT` | Base HTTPS URL of the gateway's cluster listener; all outbound calls go here |
| `KAALM_OPERATOR_NAMESPACE` | The namespace the gateway runs in. Build the gateway Service DNS names from it, `kaalm-gateway.{namespace}.svc.cluster.local` and `kaalm-gateway.{namespace}.svc`, to check who is delivering to you |
| `KAALM_TLS_CERT` / `KAALM_TLS_KEY` | Your per-agent certificate and key, mounted at `/var/run/kaalm/` |
| `KAALM_CA_CERT` | The cluster CA bundle, same mount |
| `KAALM_HANDLER_PATH` | Only when `Agent.spec.handler` is set: the directory the handler ConfigMap is mounted at (`/opt/kaalm/handler`). Its absence is how a base image knows to serve its built-in default; a `FROM` build that bakes a handler sets it itself |
| `KAALM_MEMORY_DIR` | Only when `Agent.spec.persistence.enabled` is set: the directory the PVC is mounted at (`spec.persistence.mountPath`, default `/var/agent/memory`). Write anything that must outlive the Pod here |

The certificate is your identity: the gateway authenticates you by its SAN,
and it doubles as your serving certificate. Your `spec.env` entries are
appended after these, so do not shadow the `KAALM_` names.

A `spec.env` entry that reads a Secret through `valueFrom.secretKeyRef` works
only when the Secret exists in the workload's namespace and carries the label
`kaalm.io/workload-secret: "true"`. Until it does, the workload is
`Ready=False` with reason `SecretNotOptedIn` and no new Pod starts. Ask whoever
manages Secrets in your namespace to set the label; see
[Workload is `Ready=False` with `SecretNotOptedIn`](../reference/troubleshooting.md#workload-is-readyfalse-with-secretnotoptedin).

## The contract as a checklist

Numbered as in the design book (runtime contract items 1 to 8):

1. **Health endpoints (required).** Serve `GET /readyz` and `GET /livez`
   over TLS on `$KAALM_HEALTH_PORT`. `/readyz` returns 200 when the
   container can accept a message and 503 otherwise; `/livez` returns 200
   when the process is healthy.
2. **Graceful SIGTERM (required).** Finish in-flight work and exit within
   the grace period.
3. **Gateway communication (required in practice).** Talk to
   `$KAALM_GATEWAY_ENDPOINT` for LLM calls, presenting your client
   certificate and verifying the gateway against `$KAALM_CA_CERT`.
4. **Message endpoint (channel-backed Agents only).** Serve
   `POST /v1/message` on the health port: message envelope in, response
   envelope out. Verify the caller's client certificate on this path only,
   because the kubelet's probes of `/readyz` and `/livez` present none: answer
   `401` with no certificate and `403` when its SAN is not a gateway Service
   DNS name. Reload both your serving certificate and the CA bundle from disk
   when they rotate.
5. **Heartbeats (persistent Agents only, optional).**
   `POST /v1/agent/heartbeat` to the gateway signals activity for idle
   detection; alternatively let the gateway infer activity from your
   traffic. Task images must NOT heartbeat; the gateway rejects it.
6. **Completion (AgentTasks only).** Report the verdict with
   `POST /v1/task/complete`, including any declared artifacts. Retry a
   `409` with `error.type: stale_pod` with backoff, because a report can
   arrive before the controller records the new Pod (the reference runtimes
   try four times: at once, then after 100ms, 500ms, and 2s). Retry a `503`
   with `error.type: internal_unavailable` too, waiting at least its
   `Retry-After` before the next attempt (the
   [runtime contract](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/contract.md#6-completion-signal-agenttask-only)
   has the schedule). Treat `reason=TaskAlreadyCompleted` as final and exit.
7. **Message deduplication (required if you implement /v1/message).**
   Deliveries carry a gateway-generated `messageId`; process each id once.
   With `hibernationEnabled: true` the buffer of seen ids must survive Pod
   restarts.
8. **Trace-context propagation (required if you implement /v1/message).**
   Copy the delivery's `traceparent` and `tracestate` headers onto every
   gateway call you make while handling that message; never invent them.

## One image, both modes

The same image can serve as a persistent Agent and as an AgentTask. The
starter templates detect task mode from their own certificate's SAN and
switch behavior: no heartbeat loop, report completion instead. Set
`KAALM_TEMPLATE_HEARTBEAT=off` to stop the heartbeat in both modes; the
[Starter templates](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/starter-templates.md#the-heartbeat-toggle-and-hibernation)
says when you need that.

## Growing out of a template versus starting clean

The templates carry no contract code of their own. `examples/starter-python` is a `FROM` build on
the Python base image plus a `handler.py`, and `examples/starter-go` is a
`main.go` and `handler.go` that import the `agentruntime` module, which
implements the whole checklist. Start from the Go template if Go is your
language and you need the whole program. Start clean only when you need
another runtime, and implement the checklist above.

## Testing an image before pointing an Agent at it

The fastest full-fidelity loop is the e2e cluster, because the contract is
mostly about TLS identity and that needs real certificate issuance:

```bash
make k3d-up e2e-images e2e-deploy
docker build -t registry.test/agents/mine:dev .
k3d image import registry.test/agents/mine:dev -c kaalm-dev
```

Then apply an Agent pointing at `registry.test/agents/mine:dev`.

For pure handler logic, both templates keep it behind a plain function you
can unit test without any of the above.

---

*How this works: design book pages Runtime, The runtime contract (the normative
version of this checklist, including the dedup rationale), Runtime,
Reference base images (the bottom rungs of the ladder), Runtime, Starter
templates (what each template implements), and Gateways, API, Task completion
(the Pod UID check you are retrying against).*
