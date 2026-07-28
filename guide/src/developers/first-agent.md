# Your First Agent

This page takes you from nothing to a running, persistent agent. It assumes
your platform team has given you three names: an AgentClass (here `standard`),
a ModelProvider (here `anthropic-shared`), and confirmation that your
namespace is on the provider's allowlist.

## 1. Pick an image

The fastest start is a published reference base image: it implements the
whole runtime contract (a message endpoint, TLS with the cluster CA,
heartbeats) and answers with a built-in echo handler until you supply code,
so you can prove the plumbing before writing agent logic. See
[Deploying from a Base Image](deploying-from-a-base-image.md) for supplying
your own handler; building and pushing your own image
([Building Your Own Agent Image](building-your-own-image.md)) is the path
for when you outgrow it. Whatever you pick must match the class's
`allowedImages`.

## 2. Declare the Agent

From `config/samples/kaalm_v1alpha1_agent.yaml`:

```yaml
apiVersion: kaalm.io/v1alpha1
kind: Agent
metadata:
  name: support-assistant
  namespace: default
spec:
  agentClassRef:
    name: standard
  image: ghcr.io/win07xp/kaalm-agent-python:0.3.0
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

```bash
kubectl apply -f config/samples/kaalm_v1alpha1_agent.yaml
```

## 3. Watch it come up

```bash
kubectl get agents -w
```

The `Phase` column walks through `Provisioning` (Pod, PVC, Service,
Certificate, and NetworkPolicy being created; the Pod starts only after its
certificate is issued) to `Running`. `Ready: True` means the whole set is up.

Behind the scenes your agent received everything it needs as environment and
mounts: the gateway endpoint (`KAALM_GATEWAY_ENDPOINT`), its client
certificate, and the cluster CA bundle. The base images and starter
templates consume all of this automatically.

## 4. Prove it can reach an LLM

The agent calls models through the gateway using a qualified name, for
example `anthropic-shared/claude-opus-4-6` in the standard `model` field of an
Anthropic or OpenAI-format request. The gateway authenticates the agent by its
client certificate, checks the provider gates, injects the API key, and
proxies the call. From your seat: if the agent answers a message, the
delivery path works, and the base images' `kaalm.gateway` client is already
pointed at the LLM path.

To send it that message from outside the cluster, continue to
[Connecting a Channel](connecting-a-channel.md).

## If it never reaches Running

- `kubectl describe agent support-assistant` shows the failing condition and
  reason (image not allowed by the class, provider not allowed, missing
  certificate).
- The image must match the class's `allowedImages` globs exactly.
- Check the provider allows your namespace: this surfaces as a degraded
  condition on the Agent, not a Pod failure.

---

*How this works: design book pages Runtime, Child Resources (everything
provisioned per agent), Runtime, Runtime Contract (what your image must
implement), and Gateways, LLM, Workload Identity (how the certificate
becomes an identity).*
