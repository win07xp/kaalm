# Reference Base Images

Reference base images are published container images that embed the full [runtime contract](contract.md), so that deploying an agent no longer starts with owning contract code. Two ship since v0.3.0, built and versioned by the same release workflow as the controller and gateway:

- `ghcr.io/win07xp/kaalm-agent-python`: the zero-build on-ramp. It loads a developer-supplied handler from a mounted ConfigMap at startup, so a first agent needs `kubectl` and nothing else.
- `ghcr.io/win07xp/kaalm-agent-go`: the reference runtime. It runs the built-in default handler out of the box and serves as the canonical parent for compiled handlers built with the shared runtime module.

Before base images, the on-ramp was the [starter templates](starter-templates.md): copy roughly three hundred lines of contract plumbing to wrap a handler of a few dozen. Every copy owned its own TLS rotation watch, heartbeat semantics, and dedup buffer, and a contract fix could only reach a deployed fleet by every owner re-copying it. Base images invert the ownership: Kaalm owns the contract runtime and patches it with an image bump; the developer owns exactly the handler.

The runtime contract itself is unchanged. A base image is one implementation of it, the same one the starter templates now consume (see [Relationship to the starter templates](#relationship-to-the-starter-templates)), and custom images that implement the contract themselves remain fully supported. `AgentClass.spec.image.allowedImages` governs base images like any other image.

## The handler mount

The zero-build path rests on one narrow API addition. An Agent may reference a ConfigMap holding its handler source:

```yaml
spec:
  image: "ghcr.io/win07xp/kaalm-agent-python:0.4.0"
  handler:
    configMapRef:
      name: greeter-handler
```

The Agent spec deliberately has no general-purpose volume mount, and this field does not add one. It is a single-purpose reference: the AgentReconciler mounts the named ConfigMap read-only at `/opt/kaalm/handler` (every key becomes a file) and injects `$KAALM_HANDLER_PATH` pointing at that directory. The mount path is deliberately outside `/var/run/kaalm/`, which belongs to the projected TLS volume and its rotation watch (see [Starter Templates](starter-templates.md#why-the-watch-is-on-the-directory-not-the-file)); nesting an unrelated volume inside a watched projected directory would be fragile for no benefit.

Two validation rules govern the field, both following established patterns:

- **The class must allow it (rule 30).** `spec.handler` may only be set when the referenced AgentClass has `spec.image.allowHandlerMounts: true`. The default is `false`: `allowedImages` (rule 2) is an image review boundary, and a mounted handler injects new code into an image that review already approved, so the capability is a deliberate per-class grant, not ambient. Violations follow the class-mismatch handling of rules 24, 26, and 29: `phase=Degraded, reason=HandlerMountNotAllowed`, recoverable when the specs align. See [Cross-Resource Validation](../resources/validation-and-defaulting.md#cross-resource-validation), and the [threat model](../security/threat-model.md#workload-isolation), which states in RBAC terms what granting the gate means.
- **The ConfigMap must exist (rule 31).** A `spec.handler.configMapRef` naming a ConfigMap absent from the Agent's namespace sets `Ready=False, reason=HandlerConfigMapNotFound` and the Pod is not created, mirroring rules 23 and 27: a clear condition beats a Pod wedged in `ContainerCreating` on a missing volume source. The reconciler adds no ownerRef to the ConfigMap; it is developer-owned and survives Agent deletion, like an `existingClaim` PVC.

The ConfigMap size cap (1MiB) is not a limitation to engineer around; it is the intended boundary of the on-ramp. A handler that no longer fits in a ConfigMap has outgrown mount-and-run and should graduate to the `FROM` pattern below or to a [starter template](starter-templates.md).

### Handler update semantics

Handler source is read once, at container start. The reconciler does not track ConfigMap content, consistent with the existing model that config changes are Pod-replacing spec drift ([Child Resources](child-resources.md#ownership-and-deletion)) and that the controller creates no per-Agent config ConfigMap. Three consequences, stated plainly:

- **Editing the ConfigMap in place changes nothing immediately.** The running Pod keeps its loaded handler. The new content lands on the next Pod creation, whichever comes first: a manual `kubectl delete pod`, a wake from hibernation, an involuntary-disruption recreate, or class-drift replacement. A hibernating agent therefore wakes with the newest handler content, which is worth knowing before editing the ConfigMap under a hibernated agent.
- **Repointing the reference is the clean redeploy.** Changing `spec.handler.configMapRef.name` to a new ConfigMap is ordinary spec drift: the derived Pod spec hash changes and the Pod is replaced immediately ([AgentReconciler](../controller/reconcilers.md#agentreconciler)). Versioned ConfigMap names (`greeter-handler-v2`) give an explicit, auditable rollout with instant rollback by repointing.
- **Automatic rolls on content change were considered and rejected.** A content-hash annotation (the Deployment checksum idiom) would restart agents on every ConfigMap write, require the reconciler to watch ConfigMap content across all namespaces, and make an in-place edit silently destroy in-flight conversation state. Explicit replacement keeps the blast radius of an edit at zero until the operator chooses otherwise.
- **Make handler ConfigMaps `immutable: true`.** Immutability turns a handler into what it should be: a versioned artifact. In-place edits become impossible, which deletes the entire first bullet's trap (nothing can silently land on a wake); the only way to change behavior is the repoint, so every deployment is explicit and every rollback is a repoint back; and the kubelet stops watching immutable ConfigMaps, which is free scalability. The e2e suite models exactly this form; the learn book teaches the versioned-name discipline and leaves immutability as this bullet's production advice.

## The Python image

`kaalm-agent-python` embeds the contract runtime (HTTPS serving, mTLS, rotation watch, dedup, heartbeat with task-mode detection, completion helper) and resolves its handler at startup:

- **`$KAALM_HANDLER_PATH` set** (the controller injects it if and only if `spec.handler` is present): the runtime prepends the directory to `sys.path` and imports `handler.py` from it, which must define `handle_message(envelope)`, the same signature the starter template uses. Sibling keys in the ConfigMap are importable as modules, so a handler may be split across a few files.
- **`$KAALM_HANDLER_PATH` unset**: the runtime serves the built-in [default handler](#the-default-handler).
- **Set but unloadable**: a missing `handler.py`, an import error, or a missing `handle_message` function logs the exact failure and exits nonzero. The container enters `CrashLoopBackOff`, which is the loud, visible outcome a configured-but-broken handler must have. There is deliberately no silent fallback to the default handler: an agent that answers with echo when it was configured to do real work is a debugging trap.

The runtime exposes one importable module, `kaalm`, as the handler's window into the machinery it sits on. Its surface is exactly four members:

- `kaalm.gateway`: a preconfigured HTTP client session for `$KAALM_GATEWAY_ENDPOINT`, carrying the Pod's mTLS identity and CA trust, kept current by the same rotation watch the runtime already runs. A handler makes an LLM call by POSTing a qualified model request through it; it never touches certificate files.
- `kaalm.memory`: the runtime's persistent store, namespaced under a `user/` key prefix so handler state can never collide with the contract-mandated dedup buffer (contract item 7). Backed by the PVC when `spec.persistence.enabled: true`, in-memory otherwise, with the same degradation semantics as the starter's store.
- `kaalm.http_client()` and `kaalm.http_async_client()` (since v0.4.0): factories returning standard `httpx.Client` and `httpx.AsyncClient` objects that carry the same mTLS identity and CA trust and follow certificate rotation internally. They exist for the code the runtime does not own: framework SDKs accept a stock httpx client (the names mirror the `http_client=` / `http_async_client=` keyword arguments the LangChain, OpenAI, and Anthropic SDKs take), but a client hand-built from the certificate files snapshots its SSL context at construction, so an agent that neither hibernates nor restarts through most of the leaf certificate's 90-day duration would keep presenting the stale certificate past its expiry while its health probes stay green. The factories' transports rebuild on the runtime's rotation watch, which closes that edge. Extra keyword arguments pass through to the httpx constructor; `transport`, `verify`, and `cert` are owned by the factory and rejected, and proxy environment variables are ignored unless explicitly re-enabled, because a proxy mount would route around the identity-bearing transport.

This surface is append-only within a minor release series: a handler written against `kaalm-agent-python:0.3` runs unchanged on every `0.3.x`. The v0.4.0 factories are a pure append, so a `0.3` handler happens to run unchanged on `0.4` too.

The image installs no dependencies at runtime. The class security defaults mount the root filesystem read-only and the synthesized NetworkPolicy has no PyPI egress, and both are features. What the image bundles (the standard library plus its own HTTP stack) is the handler's dependency budget; needing more is the signal to graduate to `FROM`:

```dockerfile
FROM ghcr.io/win07xp/kaalm-agent-python:0.4.0
# The image runs as nonroot; installing needs root, serving does not.
USER 0
RUN pip install --no-cache-dir beautifulsoup4 lxml
USER 65532:65532
COPY handler.py /opt/kaalm/handler/handler.py
ENV KAALM_HANDLER_PATH=/opt/kaalm/handler
```

The `ENV` line is load-bearing: the controller injects `$KAALM_HANDLER_PATH` only for ConfigMap-mounted handlers, so a `FROM` build declares it itself. The variable stays the single signal in every configuration, and if an Agent sets `spec.handler` on a `FROM`-built image anyway, the mount shadows the baked directory and the mounted handler wins.

A `FROM` build keeps the central-patching property (rebuild against the bumped tag) and sheds the ConfigMap size cap; it costs a build pipeline. This is the intended middle rung between mount-and-run and a fully custom image. Note that a baked handler needs no `allowHandlerMounts` grant: rules 30 and 31 govern ConfigMap-mounted code, while a `FROM` image passes through ordinary image review and the `allowedImages` gate like any custom image. The worked form of this rung is the LangGraph example pair under `examples/langgraph-chat/` and `examples/langgraph-tools/`, walked by the user guide's Running Framework Agents page.

## The Go image

Go compiles, so there is no source mount to offer, and Kaalm does not pretend otherwise. `kaalm-agent-go` is two honest things:

- **A runnable reference agent.** It ships the default handler, so it is the zero-build way to see a full-lifecycle Agent work end to end: apply an Agent with this image and no `spec.handler` at all, and it comes up, hibernates, wakes, and echoes. The e2e suite's baseline leans on this, and the learn book's first agent follows the same path from v0.3.0.
- **The canonical parent for compiled handlers.** The contract runtime is published as a lightweight Go module, `github.com/win07xp/kaalm/agentruntime` (standard library plus fsnotify, no controller-runtime dependency tree; each release pushes the matching `agentruntime/vX.Y.Z` module tag). The image is the build target for binaries built against it: a custom Go agent is a `main.go` that calls the runtime with its handler, compiled and layered onto the base image.

The module's surface mirrors the Python `kaalm` module in concept, idiomatic Go in shape. `New()` reads the standard Kaalm environment; `Run(ctx, handler)` serves the contract until canceled, with `nil` selecting the default handler; and a handler reaches capabilities by closing over the `Agent`, whose `Gateway` (rotation-aware mTLS client) and `Memory` (persistent store behind the same `user/` key wall) correspond one to one with `kaalm.gateway` and `kaalm.memory`. Like that surface, the module's exported API is append-only within a minor series.

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

FROM ghcr.io/win07xp/kaalm-agent-go:0.4.0
COPY --from=build /agent /kaalm-agent
```

Two mechanisms were considered for mounted Go handlers and rejected. Go plugins (`plugin.Open` of a mounted `.so`) require the plugin and host to be built by byte-identical toolchains and dependency graphs, a version-matching contract Kaalm cannot impose on users. Mounted pre-compiled binaries collide with the ConfigMap cap: real Go binaries run megabytes against a 1MiB limit. Both would have manufactured the appearance of zero-build Go while shipping its failure modes.

## The default handler

Both images ship the same built-in handler, active only when no `spec.handler` is configured. It replies to every message envelope with the received text prefixed by `echo: `, makes no LLM calls (so it works on a provider-less Agent), and is deterministic. Its two jobs are to make a minimal Agent observable end to end within minutes, and to give automated tests a fixed baseline that a user-supplied handler is distinguishable from.

## Task mode

Base images inherit the runtime's task-mode detection: run as an AgentTask, they present the task SAN, start no heartbeat, and expose the completion helper, exactly as the starter templates do. But `spec.handler` exists only on the Agent schema in v0.3.0. An AgentTask's work is its whole program, driven by goal env vars and ending in a completion report, not a resident message loop, so a message-handler mount is the wrong extension point for it. A task-body analog (a mounted script the runtime executes to completion) is a plausible future addition and is deliberately not designed here. The task on-ramp remains the starter templates and custom images.

## Versioning and support

Base images ride the release train: the same tag push that publishes the controller and gateway images publishes `kaalm-agent-go` and `kaalm-agent-python` with the same semver tags, multi-arch, with pre-release tags behaving identically ([RELEASING.md](https://github.com/win07xp/kaalm/blob/main/RELEASING.md)). The compatibility contract has two layers:

- **Downward, to the cluster:** a base image's contract behavior tracks [The Runtime Contract](contract.md) as specified at its release. Contract items are numbered and stable, so compatibility statements cite item numbers.
- **Upward, to the handler:** the handler ABI (`handle_message(envelope)` plus the `kaalm` module surface, and the Go runtime module's exported API) is append-only within a minor series. Breaking either is a minor-version event called out in release notes, never a patch.

Running a base image from one minor series against a Kaalm control plane from another is expected to work in the gateway-compatible direction but is not a tested configuration; matching the image tag to the installed chart version is the supported posture.

## Relationship to the starter templates

The contract runtime is single-sourced. The image source owns it, and the starter templates are thin consumers of the same code rather than independent copies: the Python template becomes a `FROM kaalm-agent-python` build with its `handler.py`, and the Go template imports the published runtime module instead of vendoring six hundred lines of plumbing. A contract fix lands once, in the runtime source, and reaches mount-and-run users on the next image pull, `FROM` users on their next rebuild, and template users on their next module update.

That replaces the copy-the-code relationship the templates had in v1 and v0.2.0, and it re-ranks the on-ramp. In order of increasing ownership:

| Rung | You own | Central patching | Fits when |
|---|---|---|---|
| Mount-and-run (`spec.handler`) | a ConfigMap of handler source | image bump, no action | first agents, small handlers, no build pipeline |
| `FROM` a base image | a Dockerfile and your handler | rebuild against new tag | extra dependencies, handlers past 1MiB |
| Starter template | the whole program (importing the runtime module) | dependency update | restructuring the runtime itself |
| Custom image | everything including the contract | none | pre-existing agents, other languages |

[Starter Templates](starter-templates.md) documents the third rung; [The Runtime Contract](contract.md) is the invariant behind all four.

## Acceptance scenario

[S16](../appendix/scenarios.md#s16-deploy-a-first-agent-without-building-an-image) exercises the on-ramp end to end: a class grants `allowHandlerMounts`, a handler travels as a ConfigMap, an Agent runs it from the published Python image with no build step, and repointing the reference rolls the handler. The e2e spec that proves it on a cluster is recorded in the [scenario coverage map](../appendix/scenario-coverage.md).
