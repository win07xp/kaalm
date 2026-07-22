# Implementation Roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v0.1.0 shipped on 2026-07-21**
([release](https://github.com/win07xp/kaalm/releases/tag/v0.1.0)). The operator
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

Two honest caveats:

- The API is `v1alpha1` and may change in breaking ways between minor releases.
- Each tagged release now publishes multi-arch images and an OCI Helm chart, so
  installing is a single `helm install` (see the User Guide). The `v0.1.0` tag
  predates that machinery and installs only from source.

## Next: v0.2.0

**Release machinery is done:** a tag now builds and pushes the multi-arch
images and the OCI Helm chart and cuts the GitHub release, so `helm install`
works without cloning the repo. The remaining v0.2.0 workstream is test
coverage:

**Prove every acceptance scenario on a real cluster.** The
[scenarios](appendix/scenarios.md) (S1 to S15) define "done" for the design, and
v0.1.0 ships with cluster-level e2e proof for S1, S3, S6, S8, S11, and S13. The
rest are verified at the unit or envtest level only (S4 fallback, S7 hibernate
and wake, S10 budget exhaustion, S15 async webhook) or not systematically at all
(S2, S5, S9, S12, S14). v0.2.0 closes that gap: every scenario gets an e2e spec,
and the scenario-coverage map goes all green in its e2e column.

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
- **The beginners' tutorial.** A third book (`learn/`, already scaffolded
  with its chapter plan) taking a reader from an empty laptop to a running
  agent; deliberately unwritten until installs are one command.
- **Larger horizons.** Agent-to-agent orchestration, a web UI, multi-cluster
  federation, and agent-aware scheduling (GPU awareness, priority, preemption).

## How this page is maintained

Items move here from the release notes when a version ships, and out of here into
the design book when they get designed. History lives in git and in the
[releases](https://github.com/win07xp/kaalm/releases); this page only ever
describes the present and the future.
