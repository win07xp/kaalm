# Kaalm

Kaalm is a Kubernetes operator that makes AI agents a first-class workload
type. You declare an agent; Kaalm runs it, gives it its own identity and
storage, routes its model calls through a gateway that holds the credentials,
and puts it to sleep when nobody is talking to it.

- **No image build for your first agent.** Mount a handler file into a
  published base image, or bring your own image.
- **Credentials stay out of agent code.** Model and MCP tool calls go through
  a gateway that holds the keys, checks what each agent may use, and records
  what it cost against a per-namespace budget.
- **Agents sleep when idle.** A quiet agent is shut down and its storage kept;
  the next message wakes it with its memory intact.
- **Channels built in.** Reach an agent through a webhook, Discord, or
  WhatsApp.
- **Safe defaults.** Agents run as non-root with no Kubernetes API access and
  a network policy that allows only the gateway, DNS, and the destinations
  their class permits.
- **A stable API.** `kaalm.io/v1beta1` changes only in ways that keep your
  manifests working.

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
when it goes idle.

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
