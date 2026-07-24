# Talking to Your Agent

Your agent is running, but nothing can reach it yet. On purpose: Kaalm wraps
every agent in a network policy that refuses traffic by default, so an agent is
unreachable until you deliberately open a door.

The door is an **AgentChannel**.

## Create the channel

A channel needs a token, because the door has a lock. Kubernetes stores
credentials in a **Secret**, an object meant for exactly this, kept separate
from the manifests that refer to it.

Put both in `channel.yaml`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: webhook-token
  namespace: default
type: Opaque
stringData:
  token: tutorial-secret-token
---
apiVersion: kaalm.io/v1alpha1
kind: AgentChannel
metadata:
  name: helper-webhook
  namespace: default
spec:
  agentRef:
    name: helper
  type: webhook
  webhook:
    path: /channels/default/helper-webhook
    responseMode: sync
    content:
      fromBody: text
    auth:
      type: bearer
      secretRef:
        name: webhook-token
        key: token
```

`path` is the URL the gateway will answer on. `responseMode: sync` means the
caller waits and gets the agent's answer as the HTTP response.
`content.fromBody: text` tells the gateway where in your JSON to find the
message, so posting `{"text": "hello"}` sends `hello` to the agent rather than
the whole blob.

```bash
kubectl apply -f channel.yaml
kubectl get agentchannels
```

```
NAME             AGENT    PHASE    CONNECTED   AGE
helper-webhook   helper   Active   10s
```

`Active` means the gateway has accepted the path and the token, and is now
listening.

## Reach the gateway

The gateway is inside the cluster. Forward a local port to it, and leave this
running in its own terminal:

```bash
kubectl -n kaalm-system port-forward svc/kaalm-gateway 18080:8080
```

## Say hello

In a second terminal:

```bash
curl -sk -X POST https://127.0.0.1:18080/channels/default/helper-webhook \
  -H "Authorization: Bearer tutorial-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"text":"hello there"}'
```

```json
{"content":"starter-go received: hello there","attachments":null,"metadata":{"sessionId":"","userId":""}}
```

That round trip went further than it looks. Your request hit the gateway, which
checked the token, found the agent the channel points at, opened a mutually
authenticated TLS connection to it (both sides proving identity with
certificates Kaalm issued), delivered the message, and handed you the reply.

`-k` on the curl skips certificate verification, only because you are talking
to a port-forward on localhost rather than the gateway's real name. Inside the
cluster nothing skips verification.

## It remembers you

Send a second message:

```bash
curl -sk -X POST https://127.0.0.1:18080/channels/default/helper-webhook \
  -H "Authorization: Bearer tutorial-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"text":"are you still there"}'
```

```json
{"content":"starter-go received: are you still there (message 2 from you)","attachments":null,"metadata":{"sessionId":"","userId":""}}
```

**"message 2 from you".** The agent is counting, and it is keeping that count
on the volume Kaalm gave it, not in memory. That distinction looks academic
right now. It is the whole point of the chapter after next, when the program
holding that count gets shut down entirely and a new one takes its place.

Next: [Giving It a Job](giving-it-a-job.md).
