# Make It Yours

The echo came from the image's default handler. In this chapter you replace it
with code you wrote, and the part worth noticing is everything you will not
do: no Dockerfile, no build, no registry, no new image. The code travels to
the cluster the same way your manifests did, with `kubectl`.

## Write the handler

Put this in a file called `handler.py`:

```python
import kaalm


def handle_message(envelope):
    count = kaalm.memory.get("seen", 0) + 1
    kaalm.memory.put("seen", count)
    reply = "helper here: " + envelope["content"]
    if count > 1:
        reply += f" (message {count} from you)"
    return {"content": reply}
```

Nine lines, and two of them are the interesting ones. `kaalm.memory` is the
runtime's persistent store, and it writes to the volume Kaalm attached back in
[Running an Agent](running-an-agent.md), not to the program's memory. Your
handler receives each message as `envelope` (the text your channel extracts
arrives as `envelope["content"]`) and returns its reply. Everything else that
an agent must get right, the TLS serving, the certificate rotation, the
deduplication of redelivered messages, stays the image's job; the
[runtime contract](https://github.com/win07xp/kaalm) chapter of the design
book is the full list.

## Ship it as configuration

```bash
kubectl create configmap handler-v1 --from-file=handler.py
```

```
configmap/handler-v1 created
```

> **A ConfigMap** is the Kubernetes object for plain configuration: small
> files or key-value pairs that pods can mount and read. Secrets carry
> credentials; ConfigMaps carry everything else that is configuration rather
> than code baked into an image. Kaalm's handler mount leans on exactly that:
> your nine lines of Python are configuration from the cluster's point of
> view.

The name matters more than it looks: `handler-v1`, not `handler`. Editing a
ConfigMap that a running agent mounts does not restart the agent, so the way
to ship a change is to create `handler-v2` and repoint, which also makes
rolling back a one-line change. You will do exactly that in
[Give It a Real Brain](give-it-a-real-brain.md).

## Attach it

```bash
kubectl patch agent helper --type=merge \
  -p '{"spec":{"handler":{"configMapRef":{"name":"handler-v1"}}}}'
```

This is the moment the class permission from
[Running an Agent](running-an-agent.md) earns its keep:
`spec.handler` only works because the `tutorial` class said
`allowHandlerMounts: true`. The class's image list is a review boundary, and
mounted code slips past it by design, so a platform team grants that per
class rather than getting it everywhere by default.

Now look at what the patch landed on:

```bash
kubectl get agents
```

```
NAME     PHASE        READY   CLASS      AGE
helper   Hibernated   False   tutorial   98s
```

Asleep. While you were writing the handler, the 30 second timers from the
class ran out. It does not matter at all: the change is recorded in the
agent's spec, there is no running pod to swap, and the next message will
bring the agent back already wearing your code. (If yours is still
`Running`, Kaalm replaces the pod right away instead; either way the next
message reaches your handler.)

## Prove it is your code

```bash
curl -sk -X POST https://127.0.0.1:18080/channels/default/helper-webhook \
  -H "Authorization: Bearer tutorial-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"text":"hello again"}'
```

```json
{"content": "helper here: hello again"}
```

`helper here:` is your handler talking. Send another:

```bash
curl -sk -X POST https://127.0.0.1:18080/channels/default/helper-webhook \
  -H "Authorization: Bearer tutorial-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"text":"remember me"}'
```

```json
{"content": "helper here: remember me (message 2 from you)"}
```

**"message 2 from you".** Your handler is counting, and because
`kaalm.memory` writes to the agent's volume, it is counting on disk, not in
RAM. That distinction looks academic right now. It is the whole point of the
chapter after next, when the program holding that count gets shut down
entirely and a new one takes its place.

Next: [Giving It a Job](giving-it-a-job.md).
