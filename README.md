# Kaalm

Kaalm is a Kubernetes operator that makes AI agents a first-class workload
type. You declare an agent; Kaalm runs it, gives it its own identity and
storage, routes its model calls through a gateway that holds the credentials,
and puts it to sleep when nobody is talking to it.

**Status: v1.0.0.** Installs with one Helm command, upgrades in place with
two, and your first agent needs no image build: mount a handler file into a
published base image. The API is `kaalm.io/v1beta1`, and from v1.0.0 it only
changes in ways that keep existing manifests working; `v1alpha1` manifests
still apply with a deprecation warning. The gateway holds every credential
and brokers, meters, and audits both LLM and MCP tool calls. Agents run with
secure defaults: the restricted Pod Security Standard, no Kubernetes API
token, and a NetworkPolicy that lets them reach the gateway, DNS, and only
the destinations their class allows. An optional console puts the fleet, its
spend, and a test-chat panel on one screen; Grafana dashboards and
OpenTelemetry tracing cover the rest. All twenty-four acceptance scenarios
are proven on a real cluster, the listeners have been through an
authorization and SSRF audit, and a load test ran 400 agents on one machine.

## What it looks like

```yaml
apiVersion: kaalm.io/v1beta1
kind: Agent
metadata:
  name: helper
spec:
  agentClassRef:
    name: tutorial
  image: my-agent:1
  persistence:
    enabled: true
  lifecycle:
    hibernationEnabled: true
```

That is a running container with its own TLS identity, a volume that outlives
it, a service, a network policy restricting who may reach it, and hibernation
when it goes idle. Your code never sees a provider API key: it calls the
gateway, and the gateway injects the credential, checks the agent is allowed
that provider, and records what it cost. Budgets are soft limits by default,
with opt-in hard enforcement that turns the block threshold into a guarantee.

## Install

Kaalm expects three things already in the cluster, and the chart installs
none of them: cert-manager, trust-manager, and a CNI that enforces
NetworkPolicy. With those in place:

```bash
helm install kaalm oci://ghcr.io/win07xp/charts/kaalm \
  --version 1.0.0 \
  --namespace kaalm-system --create-namespace \
  --set certManager.clusterResourceNamespace=cert-manager \
  --wait
```

The [installation guide](guide/src/getting-started/installation.md) covers
the prerequisites and what the chart creates.

To try it on a throwaway cluster on your laptop, follow the tutorial below
instead; it sets up everything from scratch.

## Documentation

Three books, each with a different job. Build them with `make books`.

| Book | Read it if you are |
|---|---|
| [`learn/`](learn/src/welcome.md) | New to Kaalm. Empty laptop to a running agent, one sitting, no API key needed. |
| [`guide/`](guide/src/introduction.md) | Using it. Installing for real, offering classes to teams, providers, budgets, the console and dashboards, troubleshooting. |
| [`docs/`](docs/src/introduction.md) | Changing it. The specification: architecture, the six resources, the runtime contract. |

## Contributing

Open an [issue](https://github.com/win07xp/kaalm/issues). There is no template
to fill in:

- **Bug?** What you ran, what happened, what you expected.
- **Feature?** What you are trying to do, not only what you want built. The
  problem is more useful than the proposed solution.

Working on the code:

```bash
make build   # binaries
make test    # unit and envtest suites
make e2e     # full suite on a throwaway k3d cluster
```

- **Roadmap:** [docs/src/ROADMAP.md](docs/src/ROADMAP.md), which is where the
  project's direction is recorded.
- **Releasing:** [RELEASING.md](RELEASING.md). It is one tag push.

## License

Apache 2.0, as stated in the header of every source file.
