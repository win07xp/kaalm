# Connecting a Channel

An AgentChannel gives your running Agent an inbound address: an authenticated
webhook path on the user gateway. Callers POST a message; your agent's reply
comes back synchronously or through a callback, your choice per channel.

## A synchronous channel

The e2e suite's own fixture, from `test/e2e/testdata/agentchannel.yaml`:

```yaml
apiVersion: agentry.io/v1alpha1
kind: AgentChannel
metadata:
  name: e2e-channel
  namespace: e2e
spec:
  agentRef:
    name: e2e-agent
  type: webhook
  webhook:
    path: /channels/e2e/e2e-channel
    responseMode: sync
    auth:
      type: bearer
      secretRef:
        name: e2e-hook
        key: token
```

Three rules about `path`:

- It must have the shape `/channels/{namespace}/{name}` matching the
  channel's own namespace and name.
- The `/v1/` prefix is reserved for the gateway's API and rejected.
- The bearer Secret (`e2e-hook`) lives in your namespace, next to the
  channel; the reconciler grants the gateway a scoped read.

Apply it and check the channel reaches `Active`:

```bash
kubectl get agentchannels
```

## Calling it

The webhook listens on the user gateway, port 8080 (TLS), Service
`agentry-gateway` in `agentry-system`. In-cluster or through your ingress:

```bash
curl -sS --cacert ca.crt \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"content": "hello"}' \
  https://<gateway-host>:8080/channels/e2e/e2e-channel
```

In `sync` mode the agent's reply is the HTTP response body. The caller
identity can be threaded through per request (the sample channel in
`config/samples/agentry_v1alpha1_agentchannel.yaml` maps it from an
`X-User-Id` header), which is what gives each user a stable conversation
session with the agent.

## Asynchronous channels

For long-running exchanges, set `responseMode: async` plus a `callbackUrl`
and `callbackAuth` (the sample channel shows an HMAC-signed callback). The
gateway then answers `202` with a `requestId` immediately, and either
delivers the reply to your callback URL or lets you poll
`GET /v1/channels/responses/{requestId}`. Async responses are held for a
bounded TTL and a bounded per-channel pending count; a caller that never
collects does not grow state forever.

## Exposing it outside the cluster

The gateway Service is ClusterIP; fronting it with a TLS pass-through
Ingress is your cluster's business. If you do, add the external hostname to
the chart's `gateway.externalHostnames` so it lands in the gateway
certificate's SANs.

---

*How this works: design book pages Resources, AgentChannel (auth types and
the delete handshake), Gateways, User (the delivery pipeline and what happens
when the agent is hibernated), and Gateways, API, Async Responses (the
retry arithmetic and TTLs).*
