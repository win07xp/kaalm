# Your first agent

This page takes you from nothing to a running, persistent agent. It assumes
your platform team has given you three names: an AgentClass (here `standard`),
a ModelProvider (here `anthropic-shared`), and confirmation that your
namespace is on the provider's allowlist (here `team-demo`, which the sample
provider's `team-*` glob admits).

## 1. Pick an image

The fastest start is a published reference base image: it implements the
whole runtime contract (a message endpoint, TLS with the cluster CA,
heartbeats) and answers with a built-in echo handler until you supply code,
so you can prove the delivery path before writing agent logic. See
[Deploying from a base image](deploying-from-a-base-image.md) for supplying
your own handler; building and pushing your own image
([Building your own agent image](building-your-own-image.md)) is the path
for when you outgrow it. Whatever you pick must match the class's
`allowedImages`.

## 2. Declare the Agent

Adapted from `config/samples/kaalm_v1beta1_agent.yaml`, with the sample's
placeholder image replaced by the published Python base image and the
namespace set to yours:

```yaml
apiVersion: kaalm.io/v1beta1
kind: Agent
metadata:
  name: support-assistant
  namespace: team-demo
spec:
  agentClassRef:
    name: standard
  image: ghcr.io/win07xp/kaalm-agent-python:0.7.0
  providers:
    - providerRef:
        name: anthropic-shared
  persistence:
    enabled: true
    sizeGi: 10
  lifecycle:
    hibernationEnabled: true
    activitySource: gatewayTraffic
```

Reading it top to bottom: run this image under the `standard` class's rules,
let it call the `anthropic-shared` provider, give it a 10 Gi volume that
survives restarts, and hibernate it when gateway traffic goes quiet.

Save it as `agent.yaml` and apply it:

```bash
kubectl apply -f agent.yaml
```

## 3. Watch it come up

```bash
kubectl get agents -n team-demo -w
```

The `Phase` column walks through `Provisioning` (Pod, PVC, Service,
Certificate, and NetworkPolicy being created; the Pod starts only after its
certificate is issued) to `Running`. `Ready: True` means the whole set is up.
On a small cluster the walk takes about fifteen seconds:

```text
NAME                PHASE          READY   CLASS      AGE
support-assistant   Provisioning   False   standard   8s
support-assistant   Running        True    standard   16s
```

The children are all in your namespace:

```bash
kubectl get pod,pvc,svc,certificate,networkpolicy -n team-demo
```

The provider's own health does not gate this: an agent reaches `Running`
while its ModelProvider is `Ready=False`, and finds out when it calls the
model.

Behind the scenes your agent received everything it needs as environment and
mounts: the gateway endpoint (`KAALM_GATEWAY_ENDPOINT`), its client
certificate, and the cluster CA bundle. The base images and starter
templates consume all of this automatically.

## 4. Prove it can reach an LLM

The agent calls models through the gateway using a qualified name, for
example `anthropic-shared/claude-opus-4-6` in the standard `model` field of an
Anthropic or OpenAI-format request. The gateway authenticates the agent by its
client certificate, checks the provider gates, injects the API key, and
proxies the call. From your side: if the agent answers a message, the
delivery path works, and the base images' `kaalm.gateway` client is already
pointed at the LLM path.

To send it that message from outside the cluster, continue to
[Connecting a channel](connecting-a-channel.md).

## If it never reaches Running

- `kubectl describe agent support-assistant` shows the failing condition and
  reason (image not allowed by the class, provider not allowed, missing
  certificate).
- The image must match the class's `allowedImages` globs exactly.
- Check the provider allows your namespace: this surfaces as a degraded
  condition on the Agent, not a Pod failure.

---

*How this works: design book pages Runtime, Child resources (everything
provisioned per agent), Runtime, The runtime contract (what your image must
implement), and Gateways, LLM, Workload identity (how the certificate
becomes an identity).*
