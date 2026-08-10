# Building Your Own Agent Image

An agent image is any container that honors the runtime contract. Most
agents never need one: the ladder starts at a published base image with a
mounted handler ([Deploying from a Base Image](deploying-from-a-base-image.md)),
climbs to a `FROM` build on that base when you need extra dependencies, and
only ends here, at owning the whole image, when you need another runtime or
full control. The templates under `examples/starter-go` and
`examples/starter-python` implement all of the contract; this page is the
contract from the implementer's seat, so you can grow out of a template or
start clean.

## What the operator hands your container

Environment:

| Variable | Meaning |
|---|---|
| `KAALM_HEALTH_PORT` | The port to serve on (default 8080) |
| `KAALM_GATEWAY_ENDPOINT` | Base HTTPS URL of the gateway's cluster listener; all outbound calls go here |
| `KAALM_TLS_CERT` / `KAALM_TLS_KEY` | Your per-agent certificate and key, mounted at `/var/run/kaalm/` |
| `KAALM_CA_CERT` | The cluster CA bundle, same mount |
| `KAALM_HANDLER_PATH` | Only when `Agent.spec.handler` is set: the directory the handler ConfigMap is mounted at (`/opt/kaalm/handler`). Its absence is how a base image knows to serve its built-in default; a `FROM` build that bakes a handler sets it itself |

The certificate is your identity: the gateway authenticates you by its SAN,
and it doubles as your serving certificate. Your `spec.env` entries are
appended after these, so do not shadow the `KAALM_` names.

## The contract as a checklist

Numbered as in the design book (runtime contract items 1 to 7):

1. **Health endpoints (required).** Serve `GET /readyz` and `GET /livez`
   over TLS on `$KAALM_HEALTH_PORT`, returning 200 when healthy. The
   controller's injected probes target exactly these paths.
2. **Graceful SIGTERM (required).** Finish in-flight work and exit within
   the grace period.
3. **Gateway communication (required in practice).** Talk to
   `$KAALM_GATEWAY_ENDPOINT` for LLM calls, presenting your client
   certificate and verifying the gateway against `$KAALM_CA_CERT`.
4. **Message endpoint (channel-backed Agents only).** Serve
   `POST /v1/message` on the health port: message envelope in, response
   envelope out. Verify the caller's client certificate; only the gateway
   should be able to deliver. Reload both your serving certificate and the
   CA bundle from disk when they rotate.
5. **Heartbeats (persistent Agents only, optional).**
   `POST /v1/agent/heartbeat` to the gateway signals activity for idle
   detection; alternatively let the gateway infer activity from your
   traffic. Task images must NOT heartbeat; the gateway rejects it.
6. **Completion (AgentTasks only).** Report the verdict with
   `POST /v1/task/complete`, including any declared artifacts. Retry a
   `403` with `reason=StalePodCompletion` a few times with backoff (the
   identity stamp can lag Pod creation by a moment); treat
   `reason=TaskAlreadyCompleted` as final and exit.
7. **Message deduplication (required if you implement /v1/message).**
   Deliveries carry a gateway-generated `messageId`; process each id once.

## One image, both modes

The same image can serve as a persistent Agent and as an AgentTask. The
starter templates detect task mode from their own certificate's SAN and
switch behavior: no heartbeat loop, report completion instead. The
`KAALM_TEMPLATE_HEARTBEAT` variable (`auto`, the default, or `off`) exists
only as an override for the heartbeat loop; there is no force-on.

## Growing out of a template versus starting clean

Before either: if a base image plus a mounted or `FROM`-baked handler covers
your case, stay there and skip this whole page. Start from a template if
your language is Go or Python and you need the whole program: the TLS wiring,
rotation reload, envelope parsing, dedup, and completion retry logic are the
fiddly parts, and they are exactly what the templates already do. Start
clean only when you need another runtime, and port the template's structure
rather than its lines: serve, verify, reload, dedup.

Replace the template's handler (`handler.go` / `handler.py`) with your agent
logic; everything else is contract plumbing you should rarely touch.

## Testing an image before pointing an Agent at it

The honest answer: the fastest full-fidelity loop is the e2e
cluster, because the contract is mostly about TLS identity, and that needs
the real certificate machinery:

```bash
make k3d-up e2e-images e2e-deploy
# then apply an Agent pointing at your image, imported via:
# docker build -t registry.test/agents/mine:dev . && k3d image import ...
```

For pure handler logic, both templates keep it behind a plain function you
can unit test without any of the above.

---

*How this works: design book pages Runtime, Runtime Contract (the normative
version of this checklist, including the dedup rationale), Runtime,
Reference Base Images (the bottom rungs of the ladder), Runtime, Starter
Templates (what each template implements), and Gateways, API, Task Complete
(the identity gate you are retrying against).*
