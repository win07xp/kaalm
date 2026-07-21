# Building Your Own Agent Image

An agent image is any container that honors the runtime contract. The
templates under `examples/starter-go` and `examples/starter-python` implement
all of it; this page is the contract from the implementer's seat, so you can
grow out of a template or start clean.

## What the operator hands your container

Environment:

| Variable | Meaning |
|---|---|
| `AGENTRY_HEALTH_PORT` | The port to serve on (default 8080) |
| `AGENTRY_GATEWAY_ENDPOINT` | Base HTTPS URL of the gateway's LLM listener; all outbound calls go here |
| `AGENTRY_TLS_CERT` / `AGENTRY_TLS_KEY` | Your per-agent certificate and key, mounted at `/var/run/agentry/` |
| `AGENTRY_CA_CERT` | The cluster CA bundle, same mount |

The certificate is your identity: the gateway authenticates you by its SAN,
and it doubles as your serving certificate. Your `spec.env` entries are
appended after these, so do not shadow the `AGENTRY_` names.

## The contract as a checklist

Numbered as in the design book (runtime contract items 1 to 7):

1. **Health endpoints (required).** Serve `GET /readyz` and `GET /livez`
   over TLS on `$AGENTRY_HEALTH_PORT`, returning 200 when healthy. The
   controller's injected probes target exactly these paths.
2. **Graceful SIGTERM (required).** Finish in-flight work and exit within
   the grace period.
3. **Gateway communication (required in practice).** Talk to
   `$AGENTRY_GATEWAY_ENDPOINT` for LLM calls, presenting your client
   certificate and verifying the gateway against `$AGENTRY_CA_CERT`.
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
`AGENTRY_TEMPLATE_HEARTBEAT` variable (`auto`, the default, or `off`) exists
only as an override for the heartbeat loop; there is no force-on.

## Growing out of a template versus starting clean

Start from a template if your language is Go or Python: the TLS wiring,
rotation reload, envelope parsing, dedup, and completion retry logic are the
fiddly parts, and they are exactly what the templates already do. Start
clean only when you need another runtime, and port the template's structure
rather than its lines: serve, verify, reload, dedup.

Replace the template's handler (`handler.go` / `handler.py`) with your agent
logic; everything else is contract plumbing you should rarely touch.

## Testing an image before pointing an Agent at it

The honest answer for v0.1.0: the fastest full-fidelity loop is the e2e
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
version of this checklist, including the dedup rationale), Runtime, Starter
Templates (what each template implements and the planned v1.1 base images),
and Gateways, API, Task Complete (the identity gate you are retrying
against).*
