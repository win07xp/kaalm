# Reference base images

Reference base images are published container images that embed the full [runtime contract](contract.md), so that deploying an agent does not start with writing contract code. Two ship, built and versioned by the same release workflow as the controller and gateway:

| Image | Role |
|---|---|
| `ghcr.io/win07xp/kaalm-agent-python` | The zero-build on-ramp. It loads a handler from a mounted ConfigMap at startup, so a first agent needs `kubectl` and nothing else. |
| `ghcr.io/win07xp/kaalm-agent-go` | The reference runtime. It runs the built-in default handler out of the box and is the parent image for compiled handlers built with the shared runtime module. |

Without a base image, an adopter copies a few hundred lines of contract code to wrap a handler of a few dozen, and every copy carries its own rotation watch, heartbeat semantics, and dedup buffer, so a contract fix reaches a fleet only when every copy is updated. A base image inverts that: Kaalm maintains the runtime and patches it with an image bump, and the developer writes exactly the handler.

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

Replace `VERSION` with the installed chart version; the images share the chart's tags. The Agent spec has no general-purpose volume mount, and this field does not add one. It is a single-purpose reference: the AgentReconciler mounts the named ConfigMap read-only at `/opt/kaalm/handler`, every key a file, and injects `$KAALM_HANDLER_PATH` pointing at that directory. The mount path is outside `/var/run/kaalm/`, which belongs to the projected TLS volume and its rotation watch ([Why the watch is on the directory, not the file](starter-templates.md#why-the-watch-is-on-the-directory-not-the-file)).

Two validation rules govern the field:

- **The class must allow it (rule 30).** `spec.handler` may be set only when the AgentClass has `spec.image.allowHandlerMounts: true`. The default is `false`: `allowedImages` (rule 2) is an image review boundary, and a mounted handler injects code into an image that review approved, so the capability is a per-class grant. A violation is `phase=Degraded, reason=HandlerMountNotAllowed`, recoverable when the specs align, as for rules 24, 26, and 29 ([Cross-resource validation](../resources/validation-and-defaulting.md#cross-resource-validation); [Workload isolation](../security/threat-model.md#workload-isolation) states what the grant means in RBAC terms).
- **The ConfigMap must exist (rule 31).** A reference to a ConfigMap absent from the Agent's namespace sets `Ready=False, reason=HandlerConfigMapNotFound` and no Pod is created, as for rules 23 and 27: a clear condition beats a Pod wedged in `ContainerCreating`. The reconciler adds no ownerRef to the ConfigMap; it is developer-owned and survives Agent deletion, like an `existingClaim` PVC.

The ConfigMap size cap, 1 MiB, is the intended boundary of the on-ramp. A handler that does not fit has outgrown mount-and-run and moves to the `FROM` rung below or to a [starter template](starter-templates.md).

### Handler update semantics

Handler source is read once, at container start. The reconciler does not track ConfigMap content, consistent with the rule that configuration changes are Pod-replacing spec drift ([Ownership and deletion](child-resources.md#ownership-and-deletion)) and that the controller creates no per-Agent config ConfigMap.

- **Editing the ConfigMap in place changes nothing at once.** The running Pod keeps its loaded handler. The new content lands on the next Pod creation: a manual `kubectl delete pod`, a wake from hibernation, an involuntary-disruption recreate, or class-drift replacement. A hibernated Agent therefore wakes with the newest content.
- **Repointing the reference is the clean redeploy.** Changing `spec.handler.configMapRef.name` to a new ConfigMap is ordinary spec drift: the derived Pod spec hash changes and the Pod is replaced at once ([AgentReconciler](../controller/reconcilers.md#agentreconciler)). Versioned names (`greeter-handler-v2`) give an auditable rollout and an instant rollback by repointing.
- **The controller does not roll Agents on content changes.** A content-hash annotation would restart agents on every ConfigMap write, require the reconciler to watch ConfigMap content across all namespaces, and let an in-place edit destroy in-flight conversation state. Explicit replacement keeps an edit inert until the operator chooses otherwise.
- **Make handler ConfigMaps `immutable: true`.** Immutability turns a handler into a versioned artifact. In-place edits become impossible, so nothing can land silently on a wake; the only way to change behavior is the repoint, so every deployment is explicit and every rollback is a repoint back; and the kubelet stops watching immutable ConfigMaps. The e2e suite uses this form, and the learn book teaches the versioned-name discipline.

## The Python image

`kaalm-agent-python` embeds the contract runtime, built on `aiohttp`, and resolves its handler at startup:

| `$KAALM_HANDLER_PATH` | Behavior |
|---|---|
| Set, which the controller does if and only if `spec.handler` is present | The runtime prepends the directory to `sys.path` and imports `handler.py` from it. Sibling keys in the ConfigMap are importable as modules, so a handler may span a few files. |
| Unset | The runtime serves the built-in [default handler](#the-default-handler). |
| Set but unloadable | A missing `handler.py`, an import error, a missing `handle_message`, or a `handle_message` with the wrong signature logs the exact failure and exits nonzero. The container enters `CrashLoopBackOff`, the loud outcome a configured-but-broken handler must have. There is no fallback to the default handler, because an agent that echoes when it was configured to do real work is a debugging trap. |

`handle_message(envelope)` is the handler ABI: a function, sync or async, taking exactly one required positional argument. The loader rejects any other arity.

The runtime exposes one importable module, `kaalm`, as the handler's interface to the runtime it sits on. Its surface is exactly five members:

| Member | What it is |
|---|---|
| `kaalm.gateway` | A preconfigured HTTP client session for `$KAALM_GATEWAY_ENDPOINT`, carrying the Pod's mTLS identity and CA trust and kept current by the rotation watch. A handler makes an LLM call by posting a qualified model request through it and never touches certificate files. |
| `kaalm.memory` | The runtime's persistent store, namespaced under a `user/` key prefix so handler state cannot collide with the dedup window ([Memory and dedup persistence](#memory-and-dedup-persistence)). |
| `kaalm.http_client()`, `kaalm.http_async_client()` | Factories returning standard `httpx.Client` and `httpx.AsyncClient` objects that carry the same identity and trust and follow rotation. They exist for code the runtime does not control: framework SDKs accept a stock httpx client through their `http_client=` and `http_async_client=` arguments. |
| `kaalm.trace_context()` | The W3C trace context of the message being handled, as a `{"traceparent": ..., "tracestate": ...}` dict, empty outside message handling, for frameworks that run their own OpenTelemetry SDK ([contract item 8](contract.md#8-trace-context-propagation)). The Go module's `agentruntime.TraceContext(ctx)` is the same surface for Go handlers. |

The httpx factories matter because a client hand-built from the certificate files snapshots its SSL context at construction. An agent that neither hibernates nor restarts through most of a leaf certificate's duration would keep presenting the stale certificate past its expiry while its probes stay green. The factories' transports rebuild on the rotation watch. Extra keyword arguments pass through to the httpx constructor; `transport`, `verify`, and `cert` are set by the factory and rejected as arguments; and proxy environment variables are ignored unless re-enabled, because a proxy would route around the identity-bearing transport.

The module has no task-completion member. A Python handler that runs as an AgentTask posts to `/v1/task/complete` through `kaalm.gateway` itself and implements the retry of [contract item 6](contract.md#6-completion-signal-agenttask-only); issue #235 tracks a helper with parity to the Go module's `CompleteTask`.

This surface is append-only within a minor release series: a handler written against one `X.Y` tag runs unchanged on every `X.Y.z`. Members are only ever added, so a handler written against an older minor series usually runs on a newer one, but that is not the tested contract.

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

A `FROM` build keeps central patching (rebuild against the bumped tag) and sheds the ConfigMap size cap; it costs a build pipeline. A baked handler needs no `allowHandlerMounts` grant: rules 30 and 31 govern ConfigMap-mounted code, while a `FROM` image passes through ordinary image review and the `allowedImages` gate. The worked form of this rung is the LangGraph example pair under `examples/langgraph-chat/` and `examples/langgraph-tools/`, walked in the user guide.

## The Go image

Go compiles, so there is no source mount to offer. `kaalm-agent-go` is two things:

- **A runnable reference agent.** It ships the default handler, so it is the zero-build way to see a full-lifecycle Agent work end to end: apply an Agent with this image and no `spec.handler`, and it comes up, hibernates, wakes, and echoes. The e2e suite's baseline and the learn book's first agent both use it.
- **The parent image for compiled handlers.** The contract runtime is published as a Go module, `github.com/win07xp/kaalm/agentruntime` (standard library plus fsnotify, no controller-runtime dependency tree; each release pushes the matching `agentruntime/vX.Y.Z` module tag). A custom Go agent is a `main.go` that calls the runtime with its handler, compiled and layered onto the image.

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

Replace `VERSION` with the installed chart version. Neither of the two mechanisms for a mounted Go handler fits. Go plugins (`plugin.Open` of a mounted `.so`) require the plugin and host to be built by byte-identical toolchains and dependency graphs, a version-matching contract Kaalm cannot impose on users. Mounted pre-compiled binaries collide with the ConfigMap cap: real Go binaries run to megabytes against a 1 MiB limit. Either would offer the appearance of zero-build Go with its failure modes attached.

## Memory and dedup persistence

Both runtimes keep one state file, `state.json`, in the memory directory: `$KAALM_MEMORY_DIR` when set, `/var/agent/memory` otherwise. The file holds two disjoint areas, the handler's `Memory` keys under the `user/` prefix and the runtime's dedup window of the last 1024 `messageId`s with their cached responses ([contract item 7](contract.md#7-message-deduplication)). When the directory is not writable, the runtime logs it and continues in memory only.

The PVC backs the file only when it is mounted at that directory. The controller mounts the Agent PVC at `spec.persistence.mountPath`, default `/var/agent/memory`, and does not inject `$KAALM_MEMORY_DIR`, so an Agent with a custom `mountPath` keeps its state on the container filesystem and loses it on every restart; issue #229 tracks it. An AgentTask's workspace PVC mounts at `/var/task/workspace` by default, which neither runtime reads, so a task's memory is in-memory unless `spec.persistence.mountPath` names the memory directory.

## The default handler

Both images ship the same built-in handler, active only when no handler is configured. It replies to every envelope with the received text prefixed by `echo: `, makes no LLM calls (so it works on a provider-less Agent), and is deterministic. Its two jobs are to make a minimal Agent observable end to end within minutes, and to give automated tests a fixed baseline that a user-supplied handler is distinguishable from.

## Task mode

Both runtimes detect task mode from the certificate SAN: run as an AgentTask, they start no heartbeat and present the task SAN, exactly as the starter templates do. The Go module also honors `KAALM_TASK_AUTOCOMPLETE`: when set on a task, the runtime reports the given status at startup instead of doing work, which is how the e2e suite and the learn book exercise the task lifecycle without a real handler. The Python image has no equivalent.

`spec.handler` exists only on the Agent schema. An AgentTask's work is its whole program, driven by goal environment variables and ending in a completion report, not a resident message loop, so a message-handler mount is the wrong extension point for it. The task on-ramp is the starter templates and custom images.

## Versioning and support

The release workflow publishes both images with the same semver tags as the controller and gateway, multi-arch. A pre-release tag publishes the same way but does not move `latest` ([RELEASING.md](https://github.com/win07xp/kaalm/blob/main/RELEASING.md)). The compatibility contract has two layers:

- **Downward, to the cluster:** a base image's contract behavior tracks [The runtime contract](contract.md) as specified at its release. Contract items are numbered and stable, so compatibility statements cite item numbers.
- **Upward, to the handler:** the handler ABI (`handle_message(envelope)` plus the `kaalm` module surface, and the Go module's exported API) is append-only within a minor series. Breaking either is a minor-version event called out in release notes, never a patch.

Running a base image from one minor series against a control plane from another is expected to work in the gateway-compatible direction but is not a tested configuration; matching the image tag to the installed chart version is the supported configuration.

## Relationship to the starter templates

The contract runtime is single-sourced. The image source is the one copy, and the starter templates are thin consumers of it: the Python template is a `FROM kaalm-agent-python` build with its `handler.py`, and the Go template imports the published runtime module. A contract fix lands once and reaches mount-and-run users on the next image pull, `FROM` users on their next rebuild, and template users on their next module update.

The on-ramp, in order of increasing ownership:

| Rung | You own | Central patching | Fits when |
|---|---|---|---|
| Mount-and-run (`spec.handler`) | A ConfigMap of handler source | Image bump, no action | First agents, small handlers, no build pipeline |
| `FROM` a base image | A Dockerfile and your handler | Rebuild against the new tag | Extra dependencies, handlers past 1 MiB |
| Starter template | The whole program, importing the runtime module | Dependency update | Restructuring the program itself |
| Custom image | Everything, including the contract | None | Existing agents, other languages |

[Starter templates](starter-templates.md) documents the third rung; [The runtime contract](contract.md) is the invariant behind all four.

## Acceptance scenario

[S16](../appendix/scenarios.md#s16-deploy-a-first-agent-without-building-an-image) exercises the on-ramp end to end: a class grants `allowHandlerMounts`, a handler travels as a ConfigMap, an Agent runs it from the published Python image with no build step, and repointing the reference rolls the handler. The e2e spec that proves it on a cluster is recorded in the [scenario coverage map](../appendix/scenario-coverage.md).
