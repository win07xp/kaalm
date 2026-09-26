# Connecting a channel

An AgentChannel gives your running Agent an inbound address: an authenticated
webhook path on the user gateway. Callers POST a message; your agent's reply
comes back synchronously or through a callback, your choice per channel.
This page covers the generic webhook. For a Discord slash command, see
[Connecting Discord](connecting-discord.md); for a WhatsApp business number,
[Connecting WhatsApp](connecting-whatsapp.md).

## A synchronous channel

The e2e suite's own fixture, from `test/e2e/testdata/agentchannel.yaml`:

```yaml
apiVersion: kaalm.io/v1beta1
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

- It must start with `/channels/{namespace}/`, your own namespace; the rest
  is yours to choose, and no two channels in the namespace may share a path.
- The `/v1/` prefix is reserved for the gateway's API and rejected.
- The bearer Secret (`e2e-hook`) lives in your namespace, next to the
  channel; the reconciler grants the gateway a scoped read.

Apply it and check the channel reaches `Active`:

```bash
kubectl get agentchannels -n e2e
```

## Calling it

The webhook listens on the user gateway, port 8080 (TLS), Service
`kaalm-gateway` in `kaalm-system`. The listener's certificate comes from the
Kaalm CA, which trust-manager projects into every namespace as the `kaalm-ca`
ConfigMap; from inside the cluster, read `ca.crt` from that ConfigMap. From
in-cluster or through your ingress:

```bash
curl -sS --cacert ca.crt \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"content": "hello"}' \
  https://GATEWAY_HOST:8080/channels/e2e/e2e-channel
```

Replace `GATEWAY_HOST` with `kaalm-gateway.kaalm-system.svc` from inside the
cluster, or your ingress hostname from outside.

In `sync` mode the agent's reply is the HTTP response body. The caller
identity can be threaded through per request (the sample channel in
`config/samples/kaalm_v1beta1_agentchannel.yaml` maps it from an
`X-User-Id` header), which is what gives each user a stable conversation
session with the agent.

## Asynchronous channels

For long-running exchanges, set `responseMode: async`. The gateway then
answers `202` with a `requestId` and a `channelPath` at once. To have the
reply pushed, add a `callbackUrl` and, with it, `callbackAuth` (the sample
channel shows an HMAC-signed callback); without one, poll
`GET /v1/channels/responses/{requestId}?channelPath={channelPath}` with the
`channelPath` value from the `202` body. Responses are held for a bounded
time; the design book's Async webhook responses page states the TTL and the
retry schedule.

![Sequence diagram of delivery and response after intake. The webhook caller POSTs to the User Gateway on :8080, which runs the intake checks. In async mode the gateway creates the placeholder ConfigMap and answers 202 with requestId and channelPath. If the Agent is Hibernated or Hibernating the gateway POSTs /v1/activate/{namespace}/{name} to the controller activator on :9443 and polls the Agent Service for reachability up to wakeTimeout. The gateway POSTs /v1/message to the Agent Service with up to four attempts and receives the response envelope. In sync mode it answers 200 within syncDeliveryDeadline; in async mode with a callbackUrl it sends a signed POST with up to four attempts; otherwise it patches the ConfigMap with the payload.](../diagrams/user-webhook-flow.svg)

## Exposing it outside the cluster

The gateway Service is ClusterIP; fronting it with a TLS pass-through
Ingress is your cluster's business. If you do, add the external hostname to
the chart's `gateway.externalHostnames` so it lands in the gateway
certificate's SANs.

---

*How this works: design book pages Resources, AgentChannel (auth types and
the delete handshake), Gateways, User Gateway (the delivery pipeline and what happens
when the agent is hibernated), and Gateways, API, Async webhook responses (the
retry arithmetic and TTLs).*
