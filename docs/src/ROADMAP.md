# Implementation Roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v0.1.0 shipped on 2026-07-21**
([release](https://github.com/win07xp/kubeclaw/releases/tag/v0.1.0)). The operator
is feature-complete against the v1 design: all five CRDs, the reconciling
controller (lifecycle, hibernation and wake, budgets, health probes, finalizers),
the two-listener gateway (LLM proxy with credential isolation, budgets, rate
limits, and fallback trees; user gateway with sync and async webhooks), the Helm
chart with cert-manager TLS wiring, the runtime contract with Go and Python
starter templates, and this book.

Quality bar at release: 87.6% project test coverage enforced by an 85% CI gate,
envtest suites against a real apiserver, and a k3d end-to-end suite that is green
both locally and in GitHub Actions. The
[scenario-coverage map](appendix/scenario-coverage.md) ties each acceptance
scenario to the tests that verify it.

Two honest caveats, also stated in the release notes:

- The API is `v1alpha1` and may change in breaking ways between minor releases.
- No container image or Helm chart is published yet. Installing v0.1.0 means
  building from source.

## Next: v0.2.0

Two workstreams, both already scoped:

**1. Prove every acceptance scenario on a real cluster.** The
[scenarios](appendix/scenarios.md) (S1 to S15) define "done" for the design, and
v0.1.0 ships with cluster-level e2e proof for S1, S3, S6, S8, S11, and S13. The
rest are verified at the unit or envtest level only (S4 fallback, S7 hibernate
and wake, S10 budget exhaustion, S15 async webhook) or not systematically at all
(S2, S5, S9, S12, S14). v0.2.0 closes that gap: every scenario gets an e2e spec,
and the scenario-coverage map goes all green in its e2e column.

**2. Release machinery, so a tag is installable.** A tag-triggered workflow that
builds and pushes a multi-arch controller and gateway image to a registry,
packages and publishes the Helm chart, versions the image reference into the
chart, and attaches both to the GitHub release. After this, `helm install` works
without cloning the repo.

## Beyond v0.2.0

These are the deferrals the design itself names (see
[Scope for v1](concepts/vision-and-scope.md#scope-for-v1)), roughly in the order
they are likely to matter:

- **API graduation.** Promote `v1alpha1` toward `v1beta1` once real usage has
  shaken out the field shapes; from that point, breaking changes require
  conversion.
- **Platform channel adapters.** Discord and WhatsApp adapters for the user
  gateway (the v1 channel type is the generic webhook only); see
  [Future platform types](resources/agentchannel.md#future-platform-types-v11).
- **Reference base images.** Published container images wrapping the runtime
  contract, replacing copy-the-starter-template as the primary on-ramp; see
  [Starter Templates](runtime/starter-templates.md).
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
[releases](https://github.com/win07xp/kubeclaw/releases); this page only ever
describes the present and the future.
