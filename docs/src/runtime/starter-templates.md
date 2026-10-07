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

- **HTTPS serving on `$KAALM_HEALTH_PORT`**, with `/readyz` and `/livez` on the same port (items 1 and 4).
- **Graceful SIGTERM**: in-flight requests drain before exit (item 2).
- **mTLS on every gateway call** through a preconfigured client that presents the workload certificate and trusts `$KAALM_CA_CERT` (item 3).
- **Certificate watch and reload** on the mount directory; see [Why the watch is on the directory, not the file](#why-the-watch-is-on-the-directory-not-the-file) (item 4).
- **Per-path client-certificate verification on `/v1/message`** (item 4).
- **The `/v1/message` handler skeleton**, which decodes the envelope, deduplicates on `messageId`, and calls the single function the developer writes (items 4 and 7).
- **Trace-context propagation** on every gateway call made while handling a message (item 8).
- **The heartbeat loop**, every 30s in Agent mode only; see [The heartbeat toggle and hibernation](#the-heartbeat-toggle-and-hibernation) (item 5).
- **The task-completion helper**, `CompleteTask` in Go and `kaalm.complete_task` in Python, with the bounded retry of item 6. The Python runtime also runs an optional `run_task` entry point and reports its outcome; see [Task mode](base-images.md#task-mode).

What a template does not do: choose an LLM client library, persist conversation state, or implement the agent's logic. The handler function is the single extension point, and the Python template adds an optional `async def run_task()` task entry point.

### Why the watch is on the directory, not the file

The kubelet rotates projected Secret and ConfigMap volumes by renaming the `..data` symlink under the mount directory; the layout is drawn under [TLS material layout](contract.md#tls-material-layout). The leaf files `tls.crt`, `tls.key`, and `ca.crt` are symlinks and are never written in place. A watcher attached to a leaf path never sees a modification on rotation, misses every rotation, and keeps serving an expired certificate until the process restarts.

The runtimes watch the mount directory instead, `/var/run/kaalm/`, for the create and rename events on the `..data` entry. On each event the runtime re-reads the leaf files and reloads the serving and client certificates and both trust pools, as [Certificate reload on rotation](contract.md#certificate-reload-on-rotation) requires.

### The heartbeat toggle and hibernation

**The heartbeat is unconditional.** In Agent mode it fires every 30s for the lifetime of the process, whether or not the agent is doing useful work. That is compatible only with the default [`Agent.spec.lifecycle.activitySource: gatewayTraffic`](../resources/agent.md), where heartbeats play no part in the [`Idle` and `Hibernated` transitions](../controller/agent-lifecycle.md).

With `activitySource: agentHeartbeat` or `both`, the unconditional heartbeat keeps the last-activity timestamp younger than any `idleTimeout`, and the Agent never goes `Idle` or `Hibernated`. Either leave `activitySource` at the default, or set the toggle below to `off` and emit heartbeats from the handler only while real work is in flight.

**Heartbeats are Agent-only.** `/v1/agent/heartbeat` rejects an AgentTask certificate with `403` ([POST /v1/agent/heartbeat](../gateways/api/agent-endpoints.md#post-v1agentheartbeat)). The runtime detects task mode from the SAN of the certificate it already loads for mTLS ([Workload identity](../gateways/llm/workload-identity.md)), so a template image run as an AgentTask emits no heartbeats and never sees the `403`. Task liveness is governed by the task timeout, not idle detection.

`KAALM_TEMPLATE_HEARTBEAT` overrides the detection:

| Value | Behavior |
|---|---|
| `auto` (default) | Emit every 30s in Agent mode. Emit nothing in AgentTask mode. |
| `off` | Never emit, in either mode. Use this when the image decides for itself when to emit, which is the prerequisite for a non-default `activitySource`. |

No value forces heartbeats on in task mode, because the endpoint rejects task callers.

## Layout

Each template directory holds a `Dockerfile` and a `README.md` beside the files listed above.

Inside the repository the Go template resolves the [`agentruntime` module](base-images.md#the-go-image) (source in `agentruntime/`) through the repository's `go.work`, so its `go.mod` carries no `require` line; a copy outside the repository runs `go mod tidy` once to pin the published version. The Python base image's source is in `images/agent-python/`.

Each README has a `kubectl apply` manifest to deploy a test Agent from the template image and a "What you change" section pointing at the handler.

## Relationship to the reference base images

The templates are thin consumers of the base images' runtime, not copies. The Python template is a `FROM` build whose own code is `handler.py` and the Dockerfile: the worked example of the `FROM` rung, for extra dependencies or a handler past the ConfigMap size cap. The Go template imports the published runtime module: the worked example of restructuring the program while keeping the contract. The full on-ramp ladder, and when to step down it, is the table in [Reference base images](base-images.md#relationship-to-the-starter-templates).
