# Your first agent

This page takes you from nothing to a running, persistent agent. It assumes
your platform team has given you three names: an AgentClass (here `standard`)
that lists your ModelProvider in its `allowedProviders`, the ModelProvider
(here `anthropic-shared`), and confirmation that your namespace is on the
provider's allowlist (here `team-demo`, which the sample provider's `team-*`
glob admits). The `standard` class the chart ships allows storage and
hibernation, but it lists no ModelProvider until the platform team names one
([Offering agent classes](../platform/agent-classes.md)).

## 1. Pick an image

Start from a published reference base image. It implements the runtime
contract and answers with a built-in echo handler until you supply code, so
you can prove the delivery path before writing agent logic. See
[Deploying from a base image](deploying-from-a-base-image.md) for supplying
your own handler; building and pushing your own image
([Building your own agent image](building-your-own-image.md)) is the path
for when you outgrow it. If the class sets `allowedImages`, your image must
match one of its globs.

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
  image: ghcr.io/win07xp/kaalm-agent-python:1.0.0
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

The `Phase` column walks from `Pending` through `Provisioning` to
`Running`. `Ready: True` means the whole set of child resources is up. On a
small cluster the walk takes about fifteen seconds:

```text
NAME                PHASE          READY   CLASS      AGE
support-assistant   Provisioning   False   standard   8s
support-assistant   Running        True    standard   16s
```

The children are all in your namespace:

```bash
kubectl get pod,pvc,svc,serviceaccount,certificate,networkpolicy -n team-demo
```

The provider's own health does not gate this: an agent reaches `Running`
while its ModelProvider is `Ready=False`, and finds out when it calls the
model.

The base images and starter templates pick up the gateway endpoint
(`KAALM_GATEWAY_ENDPOINT`), the agent's client certificate, and the cluster CA
bundle on their own. An image you build yourself reads them as
[The runtime contract](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/contract.md)
describes.

## 4. Prove it can reach an LLM

The agent calls models through the gateway using a qualified name, for
example `anthropic-shared/claude-opus-4-6` in the standard `model` field of an
Anthropic or OpenAI-format request. The gateway holds the API key, so the
agent never sees it ([Request handling](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/request-handling.md)
covers the checks). If the agent answers a message, the delivery path works,
and the base images' `kaalm.gateway` client is already pointed at the LLM
path.

To send it that message from outside the cluster, continue to
[Connecting a channel](connecting-a-channel.md).

## If it never reaches Running

- `kubectl describe agent support-assistant` shows the failing condition and
  reason.
- `Degraded` with `ClassConstraintViolation`: the class does not list
  `anthropic-shared` in `allowedProviders` (the case for a `standard` class
  with no provider named on it), the image matches none of the class's
  `allowedImages` globs, or the provider's `allowedNamespaces` does not admit
  your namespace. Ask your platform team. An agent that is already running
  keeps its Pod. When the cause is a provider, its LLM calls return `403`.

---

*How this works: design book pages Runtime, Child resources (everything
provisioned per agent), Runtime, The runtime contract (what your image must
implement), and Gateways, LLM, Workload identity (how the certificate
becomes an identity).*
