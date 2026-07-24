# Roadmap

Kaalm makes AI agents a first-class Kubernetes workload: you declare an agent,
and the operator runs it, gives it an identity and storage, puts a gateway in
front of your model providers, and hibernates it when nobody is talking to it.

**Today: v0.2.0.** It installs with one Helm command, and all fifteen
acceptance scenarios are proven on a real cluster.

## What is next

Nothing is currently in progress. These are the deferrals, roughly in the
order they are likely to matter:

- API graduation, `v1alpha1` toward `v1beta1`
- Discord and WhatsApp channel adapters
- Reference base images, so agents need not copy a starter template
- Agent Sandbox runtime backend for code-executing agents
- Observability: dashboard JSON and tracing
- Hard budget enforcement, replacing the v1 soft limit
- Cross-format provider fallback, for example Anthropic to OpenAI

## Where the detail lives

This page is the short version, kept to headlines on purpose.

- **[docs/src/ROADMAP.md](docs/src/ROADMAP.md)** is the canonical roadmap: why
  each item matters and what shipped when. **Edit that one**, and update this
  page only if a headline changes.
- [Scope for v1](docs/src/concepts/vision-and-scope.md) is what the project
  deliberately does not do.

## Proposing something

Open an [issue](https://github.com/win07xp/kaalm/issues). There is no separate
process and no template to fill in:

- **Bug?** What you ran, what happened, what you expected.
- **Feature?** What you are trying to do, not only what you want built. The
  problem is more useful than the proposed solution.

Work is tracked entirely in issues and
[releases](https://github.com/win07xp/kaalm/releases). There is no CHANGELOG
file by design; history lives in git.

## Everything else

- **New to Kaalm?** Start with the tutorial in `learn/`, which goes from an
  empty laptop to a running agent in one sitting.
- **Using it?** The task-oriented guide is in `guide/`.
- **Changing it?** The design book in `docs/` is the specification.
- **Cutting a release?** [RELEASING.md](RELEASING.md). It is one tag push.
