# Implementation Roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v0.2.0 shipped on 2026-07-22**
([release](https://github.com/win07xp/kaalm/releases/tag/v0.2.0)). It installs
with a single `helm install` from the published OCI chart, and every acceptance
scenario is proven on a real cluster.

The operator is feature-complete against the v1 design: all five CRDs, the
reconciling controller (lifecycle, hibernation and wake, budgets, health probes,
finalizers), the two-listener gateway (LLM proxy with credential isolation,
budgets, rate limits, and fallback trees; user gateway with sync and async
webhooks), the Helm chart with cert-manager TLS wiring, the runtime contract
with Go and Python starter templates, and this book.

What v0.2.0 added on top of v0.1.0:

- **The project is now Kaalm.** The API group moved from `agentry.io` to
  `kaalm.io` and the chart from `agentry` to `kaalm`. This is a breaking
  rename with no automatic migration; see the release notes.
- **One-command install.** A tag now builds and pushes multi-arch images and
  the OCI Helm chart and cuts the GitHub release, so installing no longer
  means cloning the repo.
- **Every scenario proven on a cluster.** S1 to S15 each have an automated k3d
  e2e spec, so the
  [scenario-coverage map](appendix/scenario-coverage.md) is all green in its
  e2e column. S2 and S9 keep a scope note: their spec proves the Kaalm-owned
  primitive, while the gVisor sandbox escape and the `VolumeSnapshot` step sit
  outside the code's test surface.

Building that suite paid for itself by finding defects the unit and envtest
layers could not reach: a hibernation-enabled agent that had never made an LLM
call oscillated between `Running` and `Idle` and never hibernated; async
callbacks to an in-cluster receiver were blocked at three independent layers;
the documented `callbackUrl` allowlist was never wired to anything; and the
upstream and callback CA bundles were read once at startup instead of on
rotation. All are fixed.

Quality bar at release: 87.5% project test coverage enforced by an 85% CI gate,
envtest suites against a real apiserver, and a k3d end-to-end suite that is
green both locally and in GitHub Actions.

Two honest caveats:

- The API is `v1alpha1` and may change in breaking ways between minor releases,
  as the `agentry.io` to `kaalm.io` move in this release shows.
- The `v0.1.0` tag predates the release machinery and installs only from
  source.

## Next

The v0.2.0 workstreams are done, and the beginners' tutorial (`learn/`) that
was waiting on one-command installs is now written and walked against 0.2.0.

The current milestone is **v0.3.0, "the on-ramp"**, tracked in the
[v0.3.0 GitHub milestone](https://github.com/win07xp/kaalm/milestone/2)
(umbrella issue [#46](https://github.com/win07xp/kaalm/issues/46)):

- **Reference base images**, now designed in
  [Reference Base Images](runtime/base-images.md): published `kaalm-agent-go`
  and `kaalm-agent-python` images embedding the runtime contract, a
  `spec.handler` ConfigMap mount gated by
  `AgentClass.spec.image.allowHandlerMounts` (validation rules 30 and 31), and
  acceptance scenario S16. The images, the mount, and the S16 e2e proof are
  implemented; what remains is the v0.3.0 release that publishes the images.
- **Hard budget enforcement** (opt-in; soft limits stay the default), design
  pending.
- **The tool plane design chapter** (gateway-brokered MCP tool access), design
  pending; implementation is the v0.4.0 milestone.

Beyond v0.3.0, the milestone runway to v1.0.0 is laid out in the
[GitHub milestones](https://github.com/win07xp/kaalm/milestones); items below
remain the unscheduled backlog.

## Beyond

These are the deferrals the design itself names (see
[Scope for v1](concepts/vision-and-scope.md#scope-for-v1)), roughly in the order
they are likely to matter:

- **API graduation.** Promote `v1alpha1` toward `v1beta1` once real usage has
  shaken out the field shapes; from that point, breaking changes require
  conversion.
- **Platform channel adapters.** Discord and WhatsApp adapters for the user
  gateway (the v1 channel type is the generic webhook only); see
  [Future platform types](resources/agentchannel.md#future-platform-types-v11).
- **Agent Sandbox integration.** The `agentSandbox` runtime backend for
  code-executing agents.
- **Observability deepening.** Concrete Grafana dashboard JSON for the shipped
  metric catalogs, and OpenTelemetry tracing across the gateway to agent to
  provider hops; see [Observability](operations/observability.md).
- **Hard budget enforcement.** Synchronous per-request aggregation, replacing
  the v1 soft-limit design where overshoot within a sync window is accepted.
- **Cross-format provider fallback.** Translation between provider API formats
  (for example Anthropic to OpenAI) so fallback chains can cross `spec.type`.
- **Larger horizons.** Agent-to-agent orchestration, a web UI, multi-cluster
  federation, and agent-aware scheduling (GPU awareness, priority, preemption).

## How this page is maintained

Items move here from the release notes when a version ships, and out of here into
the design book when they get designed. History lives in git and in the
[releases](https://github.com/win07xp/kaalm/releases); this page only ever
describes the present and the future.
