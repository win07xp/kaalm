# Starter templates

Starter templates are minimal, working agents built on [The runtime contract](contract.md), for adapting when an agent outgrows the [reference base images](base-images.md). The base images are the primary on-ramp; a template is the rung below a fully custom image, for developers who need to restructure the program around the runtime rather than only supply a handler. They are not a framework. Developers own their copy.

Two templates ship:

| Template | Path | What it is |
|---|---|---|
| Go | `examples/starter-go/` | A `main.go` that wires a handler into the `agentruntime` module, plus the handler and its test |
| Python | `examples/starter-python/` | A `handler.py` and a Dockerfile that builds `FROM` the Python base image |

Both target Kaalm-managed Agent and AgentTask Pods with mTLS. Gateway-only-tier workloads are existing images that authenticate with a projected ServiceAccount token, so the templates do not apply to them ([Tiered on-ramp](../operations/deployment.md#tiered-on-ramp)).

## What the templates implement automatically

A custom image has to satisfy every item of the contract. The templates satisfy all of them by consuming the runtime the base images are built from, so the developer replaces the agent logic without rebuilding the contract:

- **HTTPS serving on `$KAALM_HEALTH_PORT`** with the certificate at `$KAALM_TLS_CERT` and `$KAALM_TLS_KEY`, and `/readyz` and `/livez` on the same port (items 1 and 4).
- **Graceful SIGTERM**: in-flight requests drain before exit (item 2).
- **mTLS on every gateway call** through a preconfigured client that presents the same certificate and trusts `$KAALM_CA_CERT` (item 3).
- **Certificate watch and reload** on the mount directory, rebuilding both trust pools on a CA-bundle change; see [Why the watch is on the directory, not the file](#why-the-watch-is-on-the-directory-not-the-file) (item 4).
- **Per-path client-certificate verification on `/v1/message`**: `401` with no peer certificate, `403` unless the SAN is the gateway Service DNS (item 4).
- **The `/v1/message` handler skeleton**, which decodes the envelope, deduplicates on `messageId` over a persisted window of 1024 ids, and calls the one developer-owned function (items 4 and 7).
- **Trace-context propagation** on every gateway call made while handling a message (item 8).
- **The heartbeat loop**, every 30s in Agent mode only; see [The heartbeat toggle and the hibernation footgun](#the-heartbeat-toggle-and-the-hibernation-footgun) (item 5).
- **The task-completion helper** in Go, `CompleteTask`, with the bounded retry on `409 stale_pod` (`StalePodCompletion`) and `TaskAlreadyCompleted` treated as final (item 6). The Python `kaalm` module has no completion member as shipped, so a Python task posts to `/v1/task/complete` through `kaalm.gateway` itself; issue #235 tracks the helper ([Task mode](base-images.md#task-mode)).

What a template does not do: choose an LLM client library, persist conversation state, or implement the agent's logic. The handler function is the single extension point.

### Why the watch is on the directory, not the file

This is the part most likely to be broken by a well-intentioned rewrite.

The kubelet rotates projected Secret and ConfigMap volumes by renaming the `..data` symlink under the mount directory; the layout is drawn under [TLS material layout](contract.md#tls-material-layout). The leaf files `tls.crt`, `tls.key`, and `ca.crt` are symlinks and are never written in place. A watcher attached to a leaf path never sees a modification on rotation, misses every rotation, and keeps serving an expired certificate until the process restarts.

The runtimes watch the mount directory instead, `/var/run/kaalm/`, for the create and rename events on the `..data` entry: fsnotify in Go, `watchdog` in Python, both anchored to the parent directory. On each event the runtime re-reads the leaf files and reloads:

- A certificate or key change reloads the inbound serving certificate and the outbound client certificate.
- A CA-bundle change rebuilds both the inbound `ClientCAs` pool (Go: `tls.Config.GetConfigForClient` returns a config with the fresh pool; Python: the server SSL context is swapped) and the outbound trust pool.

Both reloads are needed. cert-manager rotates leaf certificates on its schedule ([Rotation defaults](../security/tls.md#rotation-defaults)), and trust-manager re-projects the CA ConfigMap whenever the CA renews or a re-key adds or removes bundle sources. Without the CA-bundle reload, a re-key breaks both directions once gateway leaves are re-issued under the new key: outbound calls stop trusting the gateway's serving certificate, and the inbound pool rejects the gateway's client certificate on `/v1/message` ([CA renewal and re-key](../security/tls.md#ca-renewal-and-re-key)).

### The heartbeat toggle and the hibernation footgun

**The heartbeat is unconditional.** In Agent mode it fires every 30s for the lifetime of the process, whether or not the agent is doing useful work. That is compatible only with the default [`Agent.spec.lifecycle.activitySource: gatewayTraffic`](../resources/agent.md), where the controller ignores heartbeats for idle detection. The gateway still records them, but they play no part in the [`Idle` and `Hibernated` transitions](../controller/agent-lifecycle.md).

With `activitySource: agentHeartbeat` or `both`, the unconditional heartbeat keeps the last-activity timestamp younger than any `idleTimeout`, and the Agent never goes `Idle` or `Hibernated`. Either leave `activitySource` at the default, or set the toggle below to `off` and emit heartbeats from the handler only while real work is in flight. The field is intended for images that emit a meaningful liveness signal.

**Heartbeats are Agent-only.** `/v1/agent/heartbeat` rejects an AgentTask certificate with `403` ([POST /v1/agent/heartbeat](../gateways/api/agent-endpoints.md#post-v1agentheartbeat)). The runtime detects task mode from the certificate it already loads for mTLS: an AgentTask's SAN is `{name}.{namespace}.task.kaalm.io`, an Agent's is `{name}.{namespace}.svc.cluster.local` ([Workload identity](../gateways/llm/workload-identity.md)). A template image run as an AgentTask emits no heartbeats and never sees the `403`. Task liveness is governed by the task timeout, not idle detection.

`KAALM_TEMPLATE_HEARTBEAT` overrides the detection:

| Value | Behavior |
|---|---|
| `auto` (default) | Emit every 30s in Agent mode. Emit nothing in AgentTask mode. |
| `off` | Never emit, in either mode. Use this when the image gates emission itself, which is the prerequisite for a non-default `activitySource`. |

There is no value that forces heartbeats on in task mode, because the endpoint rejects task callers and the only effect would be a `403` every 30 seconds.

## Layout

```
examples/
  starter-go/
    Dockerfile         # compiles the program, layers it onto kaalm-agent-go
    go.mod             # no require line: go.work resolves the module in-repo; run go mod tidy in your copy
    main.go            # wiring only: New() + Run(ctx, handler(a))
    handler.go         # the developer-owned handler, replace this
    handler_test.go    # a unit test for the handler, replace it with yours
    README.md
  starter-python/
    Dockerfile         # FROM kaalm-agent-python + COPY handler.py + ENV
    handler.py         # handle_message(envelope), replace this
    README.md
```

The Python template's image is `FROM ghcr.io/win07xp/kaalm-agent-python` (source in `images/agent-python/`), and the Go template imports the [`agentruntime` module](base-images.md#the-go-image) (source in `agentruntime/`). The module is served from the repository's `go.work` inside the repo, so the template's `go.mod` carries no `require` line; a copy outside the repo runs `go mod tidy` once to pin the published version.

Each README contains the `kubectl apply` manifests to deploy a test Agent from the template image, the environment variables the image expects (the `$KAALM_*` set the controller injects, plus `KAALM_TEMPLATE_HEARTBEAT`), and a "what to change" checklist pointing at the handler.

## Relationship to the reference base images

The contract runtime is single-sourced in the base images' source, and the templates are thin consumers of it rather than copies. The Python template is a `FROM` build whose own code is `handler.py` and the Dockerfile: the worked example of the `FROM` rung, for extra dependencies or a handler past the ConfigMap size cap. The Go template imports the published runtime module: the worked example of restructuring the program while keeping the contract.

A contract fix therefore lands once, in the runtime source, and reaches mount-and-run users on the next image pull, `FROM` users on their next rebuild, and template users on their next module update. The full on-ramp ladder, and when to step down it, is the table in [Reference base images](base-images.md#relationship-to-the-starter-templates).

The contract is the invariant across every rung: one envelope in, one response out, whether the handler is Python's `handle_message(envelope)` or Go's `Handler` function.
