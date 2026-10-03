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

Two rules about `path`:

- It must start with `/channels/{namespace}/`, your own namespace; the rest
  is yours to choose, and no two channels in the namespace may share a path.
- The `/v1/` prefix is reserved for the gateway's API and rejected.

The bearer Secret (`e2e-hook`) lives in your namespace, next to the channel.
It must carry the label `kaalm.io/channel-credential: "true"`, which opts it
in to channel use. The e2e suite's `test/e2e/testdata/secrets.yaml`
creates `e2e-hook` with it. Your team's credential manager creates and labels
the Secret; if the platform team set `rbac.personas.developerSecrets`, you do
it yourself:

```bash
kubectl label secret SECRET_NAME -n NAMESPACE kaalm.io/channel-credential=true
```

Replace `SECRET_NAME` with the Secret's name and `NAMESPACE` with your
namespace. A channel whose Secret has no label reports `Ready=False` with
reason `SecretNotOptedIn`; see
[Troubleshooting](../reference/troubleshooting.md#channel-is-readyfalse-with-secretnotoptedin-or-callbackhostnotapproved).

Apply it and check the channel reaches `Active`:

```bash
kubectl get agentchannels -n e2e
```

## Calling it

The webhook listens on the user gateway, port 8080 (TLS), Service
`kaalm-gateway` in `kaalm-system`. The listener's certificate comes from the
Kaalm CA. To verify it, read `ca.crt` from the `kaalm-ca` ConfigMap that
trust-manager projects into every namespace. From in-cluster or through your
ingress:

```bash
curl -sS --cacert ca.crt \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"content": "hello"}' \
  https://GATEWAY_HOST:8080/channels/e2e/e2e-channel
```

Replace `GATEWAY_HOST` with `kaalm-gateway.kaalm-system.svc` from inside the
cluster, or your ingress hostname from outside.

This channel sets no `spec.webhook.content` extractor, so the agent receives
the whole request body as text: `{"content": "hello"}`, not `hello`. To pass
only the field, set `spec.webhook.content.fromBody: content`; see
[AgentChannel](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/agentchannel.md#extracting-userid-and-content).

In `sync` mode the agent's reply is the HTTP response body. To give each
caller a stable conversation session with the agent, set `spec.session.enabled:
true` and map the caller identity with `userId` (the sample channel in
`config/samples/kaalm_v1beta1_agentchannel.yaml` maps it from an `X-User-Id`
header).

## Asynchronous channels

For long-running exchanges, set `responseMode: async`. The gateway then
answers `202` with a `requestId` and a `channelPath` at once. To have the
reply pushed, add a `callbackUrl` and, with it, `callbackAuth` (the sample
channel shows an HMAC-signed callback); without one, poll
`GET /v1/channels/responses/{requestId}?channelPath={channelPath}` with the
`channelPath` value from the `202` body. Responses are held for a bounded
time; the TTL and the retry schedule are on
[Async webhook responses](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/api/async-responses.md).

Every Secret a channel names needs the label, including the `callbackAuth`
Secret; the sample's `webhook-secret` and `callback-secret` both do. A
`callbackAuth` of type `bearer` also needs the host of the `callbackUrl` in the
Secret's `kaalm.io/callback-hosts` annotation, a comma-separated list of bare
hostnames, or the channel reports `CallbackHostNotApproved`. An HMAC callback
needs no annotation.

```bash
kubectl annotate secret SECRET_NAME -n NAMESPACE kaalm.io/callback-hosts=HOST
```

Replace `HOST` with the hostname of your `callbackUrl`, such as
`receiver.example.com`.

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
