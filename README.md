# Kaalm

Kaalm is a Kubernetes operator that makes AI agents a first-class workload
type. You declare an agent; Kaalm runs it, gives it its own identity and
storage, routes its model calls through a gateway that holds the credentials,
and puts it to sleep when nobody is talking to it.

**Status: v0.2.0.** Installs with one Helm command. All fifteen acceptance
scenarios are proven on a real cluster on every pull request.

## What it looks like

```yaml
apiVersion: kaalm.io/v1alpha1
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
that provider, and records what it cost.

## Install

Kaalm expects cert-manager and trust-manager already in the cluster; the chart
installs neither. With those in place:

```bash
helm install kaalm oci://ghcr.io/win07xp/charts/kaalm \
  --version 0.2.0 \
  --namespace kaalm-system --create-namespace \
  --set certManager.clusterResourceNamespace=cert-manager
```

To try it on a throwaway cluster on your laptop, follow the tutorial below
instead; it sets up everything from scratch.

## Documentation

Three books, each with a different job. Build them with `make books`.

| Book | Read it if you are |
|---|---|
| [`learn/`](learn/src/welcome.md) | New to Kaalm. Empty laptop to a running agent, one sitting, no API key needed. |
| [`guide/`](guide/src/introduction.md) | Using it. Installing for real, offering classes to teams, providers, budgets, troubleshooting. |
| [`docs/`](docs/src/introduction.md) | Changing it. The specification: architecture, the five resources, the runtime contract. |

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
