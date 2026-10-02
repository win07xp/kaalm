# Reference base images

Reference base images are published container images that embed the full [runtime contract](contract.md), so that deploying an agent does not start with writing contract code. Two ship, built and versioned by the same release workflow as the controller and gateway:

| Image | Role |
|---|---|
| `ghcr.io/win07xp/kaalm-agent-python` | The zero-build on-ramp. It loads a handler from a mounted ConfigMap at startup, so a first agent needs `kubectl` and nothing else. |
| `ghcr.io/win07xp/kaalm-agent-go` | The reference runtime. It runs the built-in default handler out of the box and is the parent image for compiled handlers built with the shared runtime module. |

Without a base image, every adopter copies the contract code, so a contract fix reaches a fleet only when every copy is updated. With one, Kaalm maintains the runtime and patches it with an image bump, and the developer writes only the handler.

The contract is unchanged by this. A base image is one implementation of it, the same one the [starter templates](starter-templates.md) consume, and a custom image that implements the contract itself is fully supported. `AgentClass.spec.image.allowedImages` governs a base image like any other.

## The handler mount

The zero-build path rests on one field. An Agent may reference a ConfigMap holding its handler source:

```yaml
spec:
  image: "ghcr.io/win07xp/kaalm-agent-python:VERSION"
  handler:
    configMapRef:
      name: greeter-handler
```

Replace `VERSION` with the installed chart version; the images share the chart's tags. The field is a single-purpose reference, not a general-purpose volume mount: the AgentReconciler mounts the named ConfigMap read-only at `/opt/kaalm/handler`, every key a file, and injects `$KAALM_HANDLER_PATH` pointing at that directory. The mount path is outside `/var/run/kaalm/`, which belongs to the projected TLS volume and its rotation watch ([Why the watch is on the directory, not the file](starter-templates.md#why-the-watch-is-on-the-directory-not-the-file)).

Two validation rules govern the field:

- **The class must allow it (rule 30).** `spec.handler` may be set only when the AgentClass has `spec.image.allowHandlerMounts: true`. The default is `false`: `allowedImages` (rule 2) is an image review boundary, and a mounted handler injects code into an image that review approved, so the capability is a per-class grant. A violation is `phase=Degraded, reason=HandlerMountNotAllowed`, recoverable when the specs align, as for rules 24, 26, and 29 ([Cross-resource validation](../resources/validation-and-defaulting.md#cross-resource-validation); [Workload isolation](../security/threat-model.md#workload-isolation) states what the grant means in RBAC terms).
- **The ConfigMap must exist (rule 31).** A reference to a ConfigMap absent from the Agent's namespace sets `Ready=False, reason=HandlerConfigMapNotFound` and no Pod is created, as for rules 23 and 27: a clear condition beats a Pod wedged in `ContainerCreating`. The reconciler adds no ownerRef to the ConfigMap; it is developer-owned and survives Agent deletion, like an `existingClaim` PVC.

The ConfigMap size cap, 1 MiB, is the intended boundary of the on-ramp. A handler that does not fit has outgrown mount-and-run and moves to the `FROM` rung below or to a [starter template](starter-templates.md).

### Handler update semantics

Handler source is read once, at container start. The reconciler does not track ConfigMap content, consistent with the rule that configuration changes are Pod-replacing spec drift ([Spec change handling](../controller/change-propagation.md#spec-change-handling)).

- **Editing the ConfigMap in place changes nothing at once.** The running Pod keeps its loaded handler. The new content lands on the next Pod creation: a manual `kubectl delete pod`, a wake from hibernation, an involuntary-disruption recreate, or class-drift replacement. A hibernated Agent therefore wakes with the newest content.
- **Repointing the reference is the clean redeploy.** Changing `spec.handler.configMapRef.name` to a new ConfigMap is ordinary spec drift: the derived Pod spec hash changes and the Pod is replaced at once ([AgentReconciler](../controller/reconcilers.md#agentreconciler)). Versioned names (`greeter-handler-v2`) give an auditable rollout and an instant rollback by repointing.
- **The controller does not roll Agents on content changes.** Rolling on content changes would restart agents on every ConfigMap write and let an in-place edit destroy in-flight conversation state. Explicit replacement keeps an edit inert until the operator chooses otherwise.
- **Make handler ConfigMaps `immutable: true`.** Immutability turns a handler into a versioned artifact. In-place edits become impossible, so nothing can land silently on a wake; the only way to change behavior is the repoint, so every deployment is explicit and every rollback is a repoint back.

## The Python image

`kaalm-agent-python` embeds the contract runtime, built on `aiohttp`, and resolves its handler at startup:

| `$KAALM_HANDLER_PATH` | Behavior |
|---|---|
| Set, which the controller does if and only if `spec.handler` is present | The runtime prepends the directory to `sys.path` and imports `handler.py` from it. Sibling keys in the ConfigMap are importable as modules, so a handler may span a few files. |
| Unset | The runtime serves the built-in [default handler](#the-default-handler). |
| Set but unloadable | A missing `handler.py`, an import error, a missing `handle_message`, or a `handle_message` with the wrong signature logs the exact failure and exits nonzero. The container enters `CrashLoopBackOff`, the loud outcome a configured-but-broken handler must have. There is no fallback to the default handler, because an agent that echoes when it was configured to do real work is a debugging trap. |

`handle_message(envelope)` is the handler ABI: a function, sync or async, taking exactly one required positional argument. The loader rejects any other arity. The module may also define an optional `async def run_task()`, the AgentTask entry point described under [Task mode](#task-mode); `handle_message` stays required either way.

The runtime exposes one importable module, `kaalm`, as the handler's interface to the runtime it sits on. Its surface is exactly seven members:

| Member | What it is |
|---|---|
| `kaalm.gateway` | A preconfigured HTTP client session for `$KAALM_GATEWAY_ENDPOINT`, carrying the Pod's mTLS identity and CA trust and kept current by the rotation watch. A handler makes an LLM call by posting a qualified model request through it and never touches certificate files. |
| `kaalm.memory` | The runtime's persistent store, namespaced under a `user/` key prefix so handler state cannot collide with the dedup window ([Memory and dedup persistence](#memory-and-dedup-persistence)). |
| `kaalm.http_client()`, `kaalm.http_async_client()` | Factories returning standard `httpx.Client` and `httpx.AsyncClient` objects that carry the same identity and trust and follow rotation. They exist for code the runtime does not control: framework SDKs accept a stock httpx client through their `http_client=` and `http_async_client=` arguments. |
| `kaalm.trace_context()` | The W3C trace context of the message being handled, as a `{"traceparent": ..., "tracestate": ...}` dict, empty outside message handling, for frameworks that run their own OpenTelemetry SDK ([contract item 8](contract.md#8-trace-context-propagation)). The Go module's `agentruntime.TraceContext(ctx)` is the same surface for Go handlers. |
| `await kaalm.complete_task(status, message="", artifacts=None)` | The runtime's own coroutine for reporting AgentTask completion ([contract item 6](contract.md#6-completion-signal-agenttask-only)). `status` is `"success"` or `"failure"`, and `artifacts` is a dict of `str` to `str`. It retries transport errors (raised before the gateway answers) and the `409 stale_pod` rejection within four attempts in all: immediately, then after 100ms, 500ms, and 2s. When the attempts run out, it raises `RuntimeError`. Any other non-`200` answer raises `RuntimeError` at once, with no retry. After a report has been accepted, it raises `kaalm.TaskAlreadyCompleted` without sending. The module does not block a call outside task mode; the gateway rejects it. The Go module's `CompleteTask` is the same surface. |
| `kaalm.TaskAlreadyCompleted` | An `Exception` subclass that `kaalm.complete_task` raises when the gateway answers `403 TaskAlreadyCompleted`, which means the task is already terminal, and when it is called after a report was accepted. The error is final: do not retry. It mirrors Go's `agentruntime.ErrTaskAlreadyCompleted`. It is a plain class, usable without a running runtime, unlike the other members. |

The httpx factories matter because a client hand-built from the certificate files snapshots its SSL context at construction, and would keep presenting a stale certificate past its expiry while the probes stay green. The factories' transports rebuild on the rotation watch. Extra keyword arguments pass through to the httpx constructor; `transport`, `verify`, and `cert` are set by the factory and rejected as arguments; and proxy environment variables are ignored unless re-enabled, because a proxy would route around the identity-bearing transport.

This surface is append-only within a minor release series: a handler written against one `X.Y` tag runs unchanged on every `X.Y.z`.

The image installs no dependencies at runtime. The synthesized NetworkPolicy has no PyPI egress, and a class that sets `readOnlyRootFilesystem` under `spec.security` blocks writes as well. What the image bundles, the standard library plus its own HTTP stack, is the handler's dependency budget, and needing more is the signal to move to `FROM`:

```dockerfile
FROM ghcr.io/win07xp/kaalm-agent-python:VERSION
# The image runs as nonroot; installing needs root, serving does not.
USER 0
RUN pip install --no-cache-dir beautifulsoup4 lxml
USER 65532:65532
COPY handler.py /opt/kaalm/handler/handler.py
ENV KAALM_HANDLER_PATH=/opt/kaalm/handler
```

Replace `VERSION` with the installed chart version. The `ENV` line is required: the controller injects `$KAALM_HANDLER_PATH` only for ConfigMap-mounted handlers, so a `FROM` build declares it itself. If an Agent sets `spec.handler` on a `FROM`-built image anyway, the mount shadows the baked directory and the mounted handler wins.

A `FROM` build keeps central patching (rebuild against the bumped tag) and sheds the ConfigMap size cap; it costs a build pipeline. A baked handler needs no `allowHandlerMounts` grant: rules 30 and 31 govern ConfigMap-mounted code, while a `FROM` image passes through ordinary image review and the `allowedImages` gate. The LangGraph examples under `examples/langgraph-chat/` and `examples/langgraph-tools/` are the worked form of this rung.

## The Go image

Go compiles, so there is no source mount to offer. `kaalm-agent-go` is two things:

- **A runnable reference agent.** It ships the default handler, so it is the zero-build way to see a full-lifecycle Agent work end to end: apply an Agent with this image and no `spec.handler`, and it comes up, hibernates, wakes, and echoes.
- **The parent image for compiled handlers.** The contract runtime is published as a Go module, `github.com/win07xp/kaalm/agentruntime` (each release pushes the matching `agentruntime/vX.Y.Z` module tag). A custom Go agent is a `main.go` that calls the runtime with its handler, compiled and layered onto the image.

The module's surface mirrors the Python `kaalm` module in concept and is idiomatic Go in form. `New()` reads the standard Kaalm environment; `Run(ctx, handler)` serves the contract until canceled, with `nil` selecting the default handler; and a handler reaches capabilities by closing over the `Agent`, whose `Gateway` (rotation-aware mTLS client), `Memory` (persistent store behind the same `user/` key wall), and `CompleteTask` (the completion helper of [contract item 6](contract.md#6-completion-signal-agenttask-only)) correspond to the Python surface. The exported API is append-only within a minor series.

```go
a, err := agentruntime.New()
// ...
err = a.Run(ctx, func(ctx context.Context, env agentruntime.Envelope) (agentruntime.Response, error) {
	resp, err := a.Gateway.Post(ctx, "/v1/chat/completions", llmRequest(env))
	// ...
})
```

Layering onto the image is one `COPY`: the entrypoint runs `/kaalm-agent`, and a build that replaces that file replaces the agent.

```dockerfile
FROM golang:1.24 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /agent .

FROM ghcr.io/win07xp/kaalm-agent-go:VERSION
COPY --from=build /agent /kaalm-agent
```

Replace `VERSION` with the installed chart version. Neither mechanism for a mounted Go handler fits. Go plugins (`plugin.Open` of a mounted `.so`) require the plugin and host to be built by byte-identical toolchains and dependency graphs, a version-matching contract Kaalm cannot impose on users. Mounted pre-compiled binaries collide with the ConfigMap cap: real Go binaries run to megabytes against a 1 MiB limit.

## Memory and dedup persistence

Both runtimes keep one state file, `state.json`, in the memory directory: `$KAALM_MEMORY_DIR` when set, `/var/agent/memory` otherwise. The file holds two disjoint areas, the handler's `Memory` keys under the `user/` prefix and the runtime's dedup window of the last 1024 `messageId`s with their cached responses ([contract item 7](contract.md#7-message-deduplication)). When the directory is not writable, the runtime logs it and continues in memory only.

The PVC backs the file only when it is mounted at that directory. The controller mounts the Agent PVC at `spec.persistence.mountPath`, default `/var/agent/memory`, and injects `$KAALM_MEMORY_DIR` with that same path, so whichever `mountPath` an Agent names is the directory the runtime writes to. An AgentTask Pod gets no `$KAALM_MEMORY_DIR`, and its workspace PVC mounts at `/var/task/workspace` by default, which neither runtime reads, so a task's memory is in-memory unless `spec.persistence.mountPath` names the memory directory.

## The default handler

Both images ship the same built-in handler, active only when no handler is configured. It replies to every envelope with the received text prefixed by `echo: `, makes no LLM calls (so it works on a provider-less Agent), and is deterministic. It makes a minimal Agent observable end to end, and gives automated tests a fixed baseline that a user-supplied handler is distinguishable from.

## Task mode

Both runtimes detect task mode from the certificate SAN, which ends in `.task.kaalm.io` for an AgentTask: run as an AgentTask, they start no heartbeat and present the task SAN, exactly as the starter templates do.

### The KAALM_TASK_AUTOCOMPLETE hook

Both runtimes honor `KAALM_TASK_AUTOCOMPLETE`, a startup test hook that exercises the task lifecycle without a real handler. In task mode, any non-empty value is sent as the completion status (the gateway accepts only `success` or `failure`) with the message `auto-complete on startup`, in up to six attempts 5 seconds apart. Both runtimes stop at once on `403 TaskAlreadyCompleted` and on `403 TaskNotAgentReported`, the answer an `exitCode` task gets to every report. The Python runtime also stops on any other `4xx` except `401` and `409 stale_pod`, which it retries, and exits in an `exitCode` task (see its [retry and exit rules](#the-python-run_task-entry-point)). The Go runtime does not exit, because your code provides `main()`. Outside task mode the variable is ignored. The hook runs beside the task's own work and does not replace it.

### The Python run_task entry point

In Python, the runtime provides `main()`, so a task's entry point is `run_task`: an optional `async def run_task()` that takes no arguments and sits next to `handle_message` in the handler module. In Go, your code provides `main()` and calls `CompleteTask` itself. The Python rules:

- **Start.** In task mode, the runtime starts `run_task` once as a background task after the HTTPS server is listening, and keeps serving while it runs. Outside task mode, the runtime logs that `run_task` is defined and does not run it.
- **Reporting.** If `run_task` returns and the task has not reported, the runtime reports `success` with an empty message and no artifacts. A task that declares `spec.artifacts` must call `kaalm.complete_task` itself with them: otherwise the gateway rejects the automatic `success` with `400` (missing declared artifact), which the runtime logs once and does not retry. If `run_task` raises, the runtime reports `failure` with the exception text, or the exception type name when the text is empty, cut to at most 4 KiB (4096 bytes of UTF-8). An exception from `kaalm.complete_task` inside `run_task` is such a raise.
- **One report.** Once any report is accepted (`200`), no further report is sent: the hook and the report after `run_task` skip, and `kaalm.complete_task` raises `kaalm.TaskAlreadyCompleted` without sending. The same holds after a `403 TaskAlreadyCompleted`, which the runtime's own report only logs.
- **Retries.** The runtime's own reports (the hook's and the post-`run_task` report) stop at once on any `4xx` answer except `401` and `409 stale_pod`: the runtime logs the error once and sends nothing more. They retry only answers that a later attempt may change: transport errors, a `5xx`, a `401`, and a `409 stale_pod` that outlasts the schedule in `kaalm.complete_task`'s own retries. The `401` comes from the gateway's source-IP check, which answers it at Pod start until the kubelet posts the Pod's IP. The limit is six attempts 5 seconds apart. A report from `kaalm.complete_task` follows the schedule in the member table.
- **Exit codes for `exitCode` tasks.** The gateway answers every completion report of an `exitCode` task with `403 TaskNotAgentReported`, because the container's exit is the task's verdict. When the runtime's own report gets that answer, it stops retrying and exits. After `run_task`, the exit code is 0 when `run_task` returned and 1 when it raised. With the hook and no `run_task`, it is 0 when the hook status is `success` and 1 for any other value. A `kaalm.complete_task` call inside `run_task` gets the same `403` and raises `RuntimeError`, not `kaalm.TaskAlreadyCompleted`. If that error escapes `run_task`, it is a raise: the runtime sends no report and exits 1, even when the work succeeded. So in an `exitCode` task, return from `run_task` to succeed and raise to fail, and do not call `kaalm.complete_task`. For an `agentReported` task nothing changes: the Pod keeps serving after the report and the controller cleans up.
- **Both set.** When the hook and `run_task` are both set, both run. The first report the gateway accepts is the task's outcome, and the other is never sent (a `kaalm.complete_task` call in `run_task` after that raises `kaalm.TaskAlreadyCompleted`). In an `exitCode` task, the hook's `403` does not end the process: the outcome of `run_task` gives the exit code, and no second report is sent.
- **SIGTERM.** A running `run_task` is cancelled and nothing is reported.
- **Load errors.** A `run_task` that is not `async def` (including a non-callable value) or that has required parameters fails the handler load. The container logs the error, exits 1, and enters `CrashLoopBackOff`, like the other unloadable-handler cases above.

`spec.handler` exists only on the Agent schema. An AgentTask's work is its whole program, driven by goal environment variables and ending in a completion report, not a resident message loop, so a message-handler mount is the wrong extension point for it. The Python task on-ramp is a `FROM`-built image whose `handler.py` defines `run_task`; the image sets `KAALM_HANDLER_PATH` itself, as the [`FROM` example](#the-python-image) shows and as `examples/starter-python/Dockerfile` does. The Go on-ramp is the starter template. Custom images are the other option.

## Versioning and support

The release workflow publishes both images with the same semver tags as the controller and gateway, multi-arch. A pre-release tag publishes the same way but does not move `latest` ([RELEASING.md](https://github.com/win07xp/kaalm/blob/main/RELEASING.md)). The compatibility contract has two layers:

- **Downward, to the cluster:** a base image's contract behavior tracks [The runtime contract](contract.md) as specified at its release. Contract items are numbered and stable, so compatibility statements cite item numbers.
- **Upward, to the handler:** the handler ABI (`handle_message(envelope)` plus the `kaalm` module surface, and the Go module's exported API) is append-only within a minor series. Breaking either is a minor-version event called out in release notes, never a patch.

Running a base image from one minor series against a control plane from another is expected to work in the gateway-compatible direction but is not a tested configuration; matching the image tag to the installed chart version is the supported configuration.

## Relationship to the starter templates

The contract runtime is single-sourced. The image source is the only copy, and the starter templates carry no contract code of their own: the Python template is a `FROM kaalm-agent-python` build with its `handler.py`, and the Go template imports the published runtime module. A contract fix lands once and reaches mount-and-run users on the next image pull, `FROM` users on their next rebuild, and template users on their next module update.

The on-ramp, in order of increasing ownership:

| Rung | You own | Central patching | Fits when |
|---|---|---|---|
| Mount-and-run (`spec.handler`) | A ConfigMap of handler source | Image bump, no action | First agents, small handlers, no build pipeline |
| `FROM` a base image | A Dockerfile and your handler | Rebuild against the new tag | Extra dependencies, handlers past 1 MiB |
| Starter template | The whole program, importing the runtime module | Dependency update | Restructuring the program itself |
| Custom image | Everything, including the contract | None | Existing agents, other languages |

[Starter templates](starter-templates.md) documents the third rung; [The runtime contract](contract.md) is the invariant behind all four.

## Acceptance scenario

[S16](../appendix/scenarios.md#s16-deploy-a-first-agent-without-building-an-image) exercises the on-ramp end to end: a class grants `allowHandlerMounts`, a handler travels as a ConfigMap, an Agent runs it from the published Python image with no build step, and repointing the reference rolls the handler. The e2e spec that proves it is recorded in the [scenario coverage map](../appendix/scenario-coverage.md).
