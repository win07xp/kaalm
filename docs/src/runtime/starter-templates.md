# Starter Templates

Starter templates are minimal, working implementations of the [Agent Runtime Contract](contract.md), intended to be adapted when an agent outgrows the [reference base images](base-images.md). Since v0.3.0 the base images are the primary on-ramp; the templates are the rung below full custom images, for developers who need to restructure the runtime itself rather than just supply a handler. They are **not** a framework. Developers own their copy.

Two templates ship with v1:

- `examples/starter-go/`: Go implementation using the standard library HTTP server.
- `examples/starter-python/`: Python implementation using `aiohttp`.

Both templates implement the same runtime contract and have feature parity. They target the **full-lifecycle tier** (Kaalm-managed Agent and AgentTask Pods, mTLS auth). Gateway-only-tier workloads are pre-existing images that authenticate with a projected ServiceAccount token instead, so the templates do not apply to them: see [Tiered On-Ramp](../operations/deployment.md#tiered-on-ramp).

## What the templates implement automatically

A custom agent image has to satisfy every bullet in the [Agent Runtime Contract](contract.md). The template handles the repetitive and error-prone parts, so developers can replace the agent logic without rebuilding the contract.

The ten template items below map onto the contract as follows:

| Template item | Runtime-contract item |
|---|---|
| 1. HTTPS serving on `$KAALM_HEALTH_PORT` | [item 1](contract.md) (HTTPS health endpoints) |
| 2. mTLS client-cert presentation | [item 3](contract.md) (gateway communication) |
| 3. CA trust bundle | [item 3](contract.md) (server verification) |
| 4. Cert-file watch and reload | [item 4](contract.md) (reload without dropping connections) |
| 5. `/v1/message` handler skeleton | [item 4](contract.md) plus [item 7](contract.md) (dedup) |
| 6. mTLS verification on `/v1/message` | [item 4](contract.md) (client-cert verification) |
| 7. Readiness + liveness endpoints | [item 1](contract.md) |
| 8. Graceful SIGTERM | [item 2](contract.md) |
| 9. Heartbeat loop | [item 5](contract.md) (optional activity signal, Agent only) |
| 10. Task-completion helper | [item 6](contract.md) (optional completion signal, AgentTask only) |

1. **HTTPS serving on `$KAALM_HEALTH_PORT`**: loads `$KAALM_TLS_CERT` / `$KAALM_TLS_KEY`, configures the HTTP server with the loaded cert, and binds on the port the controller injected.

2. **mTLS client-cert presentation**: pre-configures the outbound HTTP client used to call `$KAALM_GATEWAY_ENDPOINT` with the same cert pair as the client certificate, so LLM requests, heartbeats, and task completion calls satisfy mTLS identity without per-call plumbing.

3. **CA trust bundle**: adds `$KAALM_CA_CERT` to both the inbound TLS config (for the mandatory mTLS client-cert verification, see item 6) and the outbound HTTP client's root CA set (so the gateway's cert is trusted).

4. **Cert-file watch and reload**: see [Why the watch is on the directory, not the file](#why-the-watch-is-on-the-directory-not-the-file) below. This item carries the most subtle failure mode in the template, so it is written out in full.

5. **`/v1/message` handler skeleton**: accepts the normalized message envelope, decodes JSON, and hands the payload to a single user-provided `handleMessage(envelope) -> response` function. Deduplicates on `messageId` using an in-memory LRU of the last 1024 IDs. That is sufficient for non-hibernated agents; Agents with `hibernationEnabled: true` MUST back the buffer with the PVC so a wake-replacement Pod still recognizes previously-delivered IDs (see [The Runtime Contract item 7](contract.md)).

6. **mTLS verification on `/v1/message`**: the inbound TLS server is configured with `VerifyClientCertIfGiven` and `ClientCAs` populated from `$KAALM_CA_CERT`. The handshake must not demand a client certificate, because the kubelet presents none on `/readyz` / `/livez` probes. Enforcement is therefore per-request in the `/v1/message` handler: requests with no peer certificate are rejected with `401`, and cert-bearing requests whose SAN does not match the gateway Service DNS (`kaalm-gateway.kaalm-system.svc.cluster.local` / `.svc`) are rejected with `403`. Required by [The Runtime Contract](contract.md) bullet 4.

7. **Readiness + liveness endpoints**: `/readyz` and `/livez` on the same port, returning `200` once the server has loaded its cert.

8. **Graceful SIGTERM**: drains in-flight requests before exit, honoring `terminationGracePeriodSeconds`.

9. **Heartbeat loop**: a background goroutine/task that POSTs to [`$KAALM_GATEWAY_ENDPOINT/v1/agent/heartbeat`](../gateways/api/agent-endpoints.md#post-v1agentheartbeat) every 30s, so the gateway's [activity tracker](../gateways/user/activation-and-activity.md#activity-tracking-api) has a non-traffic signal. It runs in Agent mode only; the template detects AgentTask mode and does not start it. See [The heartbeat toggle and the hibernation footgun](#the-heartbeat-toggle-and-the-hibernation-footgun) below.

10. **Task-completion helper**: a `completeTask(status, message, artifacts)` function that POSTs [`/v1/task/complete`](../gateways/api/task-complete.md) using the pre-configured mTLS client, retrying `403 access_denied` with `reason=StalePodCompletion` on the bounded backoff schedule from [The Runtime Contract item 6](contract.md) (100ms, 500ms, 2s) and treating `reason=TaskAlreadyCompleted` as terminal (log and exit). Only relevant when the image runs as an AgentTask with `completion.condition: agentReported`; Agent images ignore it.

What the template does **not** do: choose an LLM client library, persist conversation state, implement the agent's actual logic. The `handleMessage` function is the single developer-owned extension point.

### Why the watch is on the directory, not the file

This is template item 4, and it is the part most likely to be broken by a well-intentioned rewrite.

The kubelet rotates projected Secret and ConfigMap volumes by atomically renaming the `..data` symlink under the mount directory. The leaf files (`tls.crt`, `tls.key`, `ca.crt`) themselves are never written in place. An inotify watcher attached to a leaf path will therefore not see `IN_MODIFY` on rotation, and will silently miss every rotation event.

![Inside the mount directory, tls.crt, tls.key and ca.crt are symlinks pointing one hop at a ..data symlink, which in turn points at a timestamped directory holding the real files. On rotation the kubelet writes a second timestamped directory first, then atomically renames ..data onto it, so no leaf file is ever modified and a leaf-path watch never fires.](../diagrams/projected-volume-rotation.svg)

The templates instead watch the **mount directory** (`/var/run/kaalm/`, the parent of `$KAALM_TLS_CERT` / `$KAALM_TLS_KEY` / `$KAALM_CA_CERT`) for `IN_CREATE` and `IN_MOVED_TO` events on the `..data` entry: fsnotify in Go, `watchdog`/`aionotify` in Python, both anchored to the parent directory.

On each `..data` event the template re-reads the relevant leaf files and reloads accordingly:

- A **TLS-cert/key event** reloads both the inbound server cert and the outbound client cert, without a process restart.
- A **CA-bundle event** rebuilds **both** the inbound server's `ClientCAs` pool (Go: serve via `tls.Config.GetConfigForClient` returning a config with the fresh pool; Python: swap the server SSL context) **and** the outbound HTTP client's `RootCAs` pool from the new bundle.

Both reloads are required. cert-manager rotates leaf certs continuously (see [Lifecycle of an agent TLS serving certificate](../security/tls.md#lifecycle-of-an-agent-tls-serving-certificate)), and trust-manager re-projects the CA ConfigMap whenever the CA cert renews or a manual CA re-key adds or removes bundle sources (see [Certificate Lifecycle](../operations/deployment.md#certificate-lifecycle)). Without watching the CA bundle, a CA re-key eventually breaks both directions once gateway leaves are re-issued under the new key: outbound calls stop trusting the gateway's serving cert, and the inbound `ClientCAs` pool rejects the gateway's re-issued client cert on `/v1/message`. The re-key runbook's dual-trust window is finite, so this is a matter of when, not if; [CA Renewal and Re-Key](../security/tls.md#ca-renewal-and-re-key) traces the window and both failure directions against the runbook.

### The heartbeat toggle and the hibernation footgun

This is template item 9.

**The starter template's heartbeat is unconditional.** It fires every 30s for the lifetime of the process, regardless of whether the agent is doing useful work. This is compatible only with the default [`Agent.spec.lifecycle.activitySource: gatewayTraffic`](../resources/agent.md), where the controller ignores heartbeats for idle-detection purposes. The gateway still records them, but they are not consulted when deciding [`Idle` / `Hibernated` transitions](../controller/agent-lifecycle.md).

If a developer sets `activitySource: agentHeartbeat` or `both`, the unconditional 30s heartbeat will keep the agent's last-activity timestamp younger than any reasonable `idleTimeout`, and the agent will **never** transition to `Idle` or `Hibernated`. Either leave `activitySource` at the default, or modify the template's heartbeat loop to gate emission on actual work (for example, emit only while a request is in flight). The `activitySource` field is intended for custom agent images that emit a meaningful liveness signal; pairing it with the starter template's default heartbeat behavior breaks hibernation.

Heartbeats are also an **Agent-only** signal. `/v1/agent/heartbeat` rejects AgentTask callers with `403` at the handler (see [POST /v1/agent/heartbeat](../gateways/api/agent-endpoints.md#post-v1agentheartbeat)). The templates therefore **detect task mode and do not start the heartbeat loop at all**: an AgentTask's client certificate carries the SAN `{name}.{namespace}.task.kaalm.io`, while an Agent's carries `{name}.{namespace}.svc.cluster.local` (see [Workload Identity](../gateways/llm/workload-identity.md)). The template already loads that certificate for mTLS, so the check needs no new configuration. A template image used as an AgentTask emits no heartbeats and never sees the `403`. Task liveness is governed by the task timeout, not idle detection.

`KAALM_TEMPLATE_HEARTBEAT` overrides the detection:

| Value | Behavior |
|---|---|
| `auto` (default) | Emit every 30s in Agent mode. Emit nothing in AgentTask mode. |
| `off` | Never emit, in either mode. Use this when the image gates emission itself, which is the prerequisite for a non-default `activitySource`. |

There is deliberately no value that forces heartbeats on in task mode: the endpoint rejects task callers by design, so the only effect would be a `403` every 30 seconds.

## Layout

```
examples/
  starter-go/          # the template rung: imports the runtime module
    Dockerfile         # compiles the program, layers it onto kaalm-agent-go
    go.mod             # requires github.com/win07xp/kaalm/agentruntime
    main.go            # wiring only: New() + Run(ctx, handler(a))
    handler.go         # the developer-owned handler, replace this
    README.md
  starter-python/      # the FROM rung: the base image owns the runtime
    Dockerfile         # FROM kaalm-agent-python + COPY handler.py + ENV
    handler.py         # handle_message(envelope), replace this
    README.md
```

Since v0.3.0 neither template carries contract code of its own. The Python template is a `FROM ghcr.io/win07xp/kaalm-agent-python` build (source in `images/agent-python/`), and the Go template imports the [`agentruntime` module](base-images.md#the-go-image) (source in `agentruntime/`); the ten items above are supplied by the base image and the module respectively.

Each template's README contains:

- The exact `kubectl apply` manifests to deploy a test Agent using the template image.
- The environment variables the template expects: the runtime-contract set (`$KAALM_*`) plus one template-specific toggle, `KAALM_TEMPLATE_HEARTBEAT` (`auto` default, which emits in Agent mode only; set `off` to suppress it entirely, see item 9).
- A "what to change" checklist pointing at the single handler function.

## Relationship to the reference base images

Kaalm v1 and v0.2.0 shipped starter templates instead of published base images, and the templates were then independent copies of the contract code. v0.3.0 ships the [reference base images](base-images.md) and inverts that relationship: the contract runtime is **single-sourced** in the image source, and the templates are thin consumers of it rather than copies.

- **The Python template** is a `FROM ghcr.io/win07xp/kaalm-agent-python` build: its own code is the `handler.py` and the Dockerfile. It exists as the worked example of the `FROM` rung (extra dependencies, handlers past the ConfigMap size cap).
- **The Go template** imports the published Go runtime module instead of vendoring the plumbing: its own code is `main.go` wiring a handler into the runtime. It exists as the worked example of restructuring the runtime while keeping the contract.

A contract fix therefore lands once, in the runtime source, and reaches mount-and-run users on the next image pull, `FROM` users on their next rebuild, and template users on their next module update. The full on-ramp ladder, and when to step down it, is the ranking table in [Reference Base Images](base-images.md#relationship-to-the-starter-templates).

The runtime contract is the invariant across every rung: one envelope in, one response out, whether the handler is Python's `handle_message(envelope)` or Go's `Handler` function.
