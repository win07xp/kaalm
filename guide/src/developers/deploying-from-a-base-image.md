# Deploying from a Base Image

Kaalm publishes two reference base images that implement the whole runtime
contract, so deploying an agent needs no Dockerfile and no registry of your
own:

- `ghcr.io/win07xp/kaalm-agent-python` runs handler source you supply as a
  ConfigMap, and serves a built-in echo handler until you supply one.
- `ghcr.io/win07xp/kaalm-agent-go` runs the same built-in default in a
  compiled binary; custom Go handlers are built `FROM` it (see
  [Building Your Own Agent Image](building-your-own-image.md)).

Both are published per release with the same version tags as the operator.
Pick the tag matching your installed Kaalm minor version; within a minor
series, newer patch tags are drop-in.

## Prerequisites

Handler mounts are a per-class grant. The AgentClass you deploy under must
set `image.allowHandlerMounts: true`, and its `allowedImages` must include
the base image. Both are the platform team's call; see
[Offering Agent Classes](../platform/agent-classes.md) for their side of it.

## 1. Run the default handler

Point an Agent at the image with no handler at all:

```yaml
apiVersion: kaalm.io/v1alpha1
kind: Agent
metadata:
  name: greeter
  namespace: default
spec:
  agentClassRef:
    name: starter
  image: ghcr.io/win07xp/kaalm-agent-python:0.3.0
```

It reaches `Running` and answers every message with `echo: ` plus the
message text: deterministic, no LLM calls, no provider needed. That makes it
the fastest way to prove a class, a channel, and the delivery path before
any of your code enters the picture.

## 2. Supply your handler

Write `handler.py` defining `handle_message(envelope)`, sync or async. The
runtime binds two capabilities before your handler is imported, reached with
`import kaalm`: `kaalm.gateway` (a preconfigured mTLS client for LLM calls
through the gateway) and `kaalm.memory` (persistent key-value state, backed
by the agent's volume when persistence is enabled).

```python
import kaalm


def handle_message(envelope):
    seen = kaalm.memory.get("seen", 0) + 1
    kaalm.memory.put("seen", seen)
    return {"content": f"hello {envelope['userId'] or 'there'}, message {seen}"}
```

Ship it as a ConfigMap and reference it from the Agent:

```bash
kubectl create configmap greeter-handler-v1 --from-file=handler.py
```

```yaml
spec:
  handler:
    configMapRef:
      name: greeter-handler-v1
```

The controller mounts the ConfigMap read-only at `/opt/kaalm/handler` and
injects `KAALM_HANDLER_PATH`. Every key in the ConfigMap becomes a file, and
sibling keys are importable as modules, so a handler can span a few files.
A handler that is configured but fails to load crashes the container rather
than silently serving the default; `kubectl logs` on the agent pod names the
exact failure.

## 3. Roll a change, roll it back

Edits to a mounted ConfigMap do not restart the agent, and content is not
tracked. Version the name instead:

```bash
kubectl create configmap greeter-handler-v2 --from-file=handler.py
kubectl patch agent greeter --type=merge \
  -p '{"spec":{"handler":{"configMapRef":{"name":"greeter-handler-v2"}}}}'
```

Repointing the reference replaces the Pod, which is an ordinary agent
restart: persistent state survives on the volume, in-flight delivery retries
cover the gap. Rolling back is repointing to `-v1`. In production, add
`immutable: true` to handler ConfigMaps: it makes the repoint the only way
to change behavior, which is the property that makes rollbacks trustworthy.

## When you have outgrown the mount

The moment your handler needs a dependency the base image does not bundle,
move to the `FROM` pattern: same base image, same handler file, plus a
one-line `pip install`. The class gate no longer applies (a `FROM` build
passes ordinary image review via `allowedImages`), and the trade-offs are on
[Building Your Own Agent Image](building-your-own-image.md). If the
dependency you need is an agent framework, that is its own page:
[Running Framework Agents](framework-agents.md).

## If the handler never runs

- Agent `Degraded` with reason `HandlerMountNotAllowed`: the class does not
  grant `allowHandlerMounts`. Ask your platform team, or deploy under a class
  that does.
- Agent not Ready with reason `HandlerConfigMapNotFound`: the named ConfigMap
  does not exist in the Agent's namespace. Creating it recovers the Agent
  with no further action.
- Pod in `CrashLoopBackOff`: the handler was found but failed to load
  (syntax error, missing `handle_message`, import failure). The pod log has
  the specific reason; fix the source, ship it as a new ConfigMap name, and
  repoint.

---

*How this works: design book pages Runtime, Reference Base Images (the
image contract, the handler resolution rules, and the `FROM` pattern), and
Resources, Validation and Defaulting (rules 30 and 31, the class gate and
the missing-ConfigMap condition).*
