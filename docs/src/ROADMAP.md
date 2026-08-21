# Implementation Roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v0.5.0 shipped on 2026-08-21**
([release](https://github.com/win07xp/kaalm/releases/tag/v0.5.0)). It installs
with a single `helm install` from the published OCI chart, and every
implemented acceptance scenario (S1 to S20) is proven on a real cluster.

The operator is feature-complete against the v1 design: all six CRDs, the
reconciling controller (lifecycle, hibernation and wake, budgets, health probes,
finalizers), the two-listener gateway (LLM proxy with credential isolation,
budgets, rate limits, and fallback trees; the MCP tool broker; user gateway
with sync and async webhooks), the optional operator console, the Helm chart
with cert-manager TLS wiring, the runtime contract with published base images
and starter templates, and this book.

What v0.5.0 added on top of v0.4.0 (the "console and observability"
milestone):

- **The operator console**, designed in
  [Console Overview](console/overview.md) and shipped as an optional third
  component (`console.enabled`, default off): a read-only web view of the
  fleet (phase and hibernation state, spend against budget per namespace and
  per workload, task history, channel health) with one governed write-shaped
  action, test-chat, which rides the gateway like any channel message.
  Authentication is the cluster's own (`TokenReview` and
  `SubjectAccessReview`), the JSON read API under `/api/v1` is additive
  within the minor series, and scenario S19 is proven on a real cluster.
- **Per-workload spend**: the gateway's ledger attributes every metered call
  to `agent/{name}`, `task/{name}`, or an unattributed bucket, publishes the
  breakdown through the same replicated ConfigMap exchange as budgets, and
  serves it on `GET /v1/spend` for the console; the metric catalog still
  carries no per-agent identity, by design.
- **Grafana dashboards**: three importable JSON files (per namespace, per
  provider, cluster) in `config/grafana/`, pinned to the metric catalog in
  both directions by a test and verified against a live cluster by
  `make dashboards-verify`. Getting there implemented four catalog gauges
  documented since v0.1 without an implementation (`kaalm_agents`,
  `kaalm_tasks`, `kaalm_channels`, `kaalm_llm_budget_utilization`), wired
  the `kaalm_channel_*` metrics, and made
  `kaalm_llm_request_duration_seconds` observe forwarded requests; a test
  now pins the catalog table to the registries, so a documented metric can
  no longer go unimplemented.
- **OpenTelemetry tracing** (default off): six gateway spans across the
  message, LLM, and tool paths, exported over OTLP/HTTP when
  `gateway.tracing.otlpEndpoint` is set. The agent hop is propagation only
  (runtime contract item 8), implemented invisibly by both reference
  runtimes and exposed as `kaalm.trace_context()` and
  `agentruntime.TraceContext` for frameworks that run their own SDK.
  Scenario S20 proves one webhook message as one connected trace.
- **Controller-side CA trust for health probes**
  (`controller.trustClusterCAForProbes`, `controller.probeCA`), the
  rotation-aware probe-side mirror of the gateway's upstream trust, so an
  in-cluster provider under a private CA is both forwarded to and probed
  `Healthy`; the e2e suite now runs its mock provider and tool server with
  probes on.
- **The guide's Observing the Platform part**: Using the Console,
  Installing the Grafana Dashboards, and Enabling Tracing, every command
  traced to the e2e suite or the verification stack.

Quality bar at release: 86% project test coverage enforced by an 85% CI gate,
envtest suites against a real apiserver, and a k3d end-to-end suite (59
specs) that is green both locally and in GitHub Actions.

Two honest caveats:

- The API is `v1alpha1` and may change in breaking ways between minor
  releases, as both the v0.2.0 `agentry.io` to `kaalm.io` move and the
  v0.4.0 breaking notes show. Graduation is the next milestone.
- The `v0.1.0` tag predates the release machinery and installs only from
  source.

## Next

The current milestone is **v0.6.0, "API graduation"**, tracked in the
[v0.6.0 GitHub milestone](https://github.com/win07xp/kaalm/milestone/5)
(tracking issue [#49](https://github.com/win07xp/kaalm/issues/49)):
`v1beta1` served and stored for every CRD with conversion from `v1alpha1`,
round-trip fuzz tests on the conversion, an upgrade e2e (install the previous
release, upgrade in place, and prove that existing agents and tasks survive
with state intact), and a written deprecation policy page in this book. From
that point, a breaking change requires conversion rather than a
rename-and-reinstall. The MCP 2026-07-28 revision
([#82](https://github.com/win07xp/kaalm/issues/82): stateless protocol,
routable headers) rides the same milestone.

Beyond v0.6.0, the milestone runway to v1.0.0 is laid out in the
[GitHub milestones](https://github.com/win07xp/kaalm/milestones); items below
remain the unscheduled backlog.

## Beyond

These are the deferrals the design itself names (see
[Scope for v1](concepts/vision-and-scope.md#scope-for-v1)), roughly in the order
they are likely to matter:

- **Platform channel adapters.** Discord and WhatsApp adapters for the user
  gateway (the v1 channel type is the generic webhook only); see
  [Future platform types](resources/agentchannel.md#future-platform-types-v11).
- **Agent Sandbox integration.** The `agentSandbox` runtime backend for
  code-executing agents.
- **Cross-format provider fallback.** Translation between provider API formats
  (for example Anthropic to OpenAI) so fallback chains can cross `spec.type`.
- **Larger horizons.** Agent-to-agent orchestration, multi-cluster
  federation, and agent-aware scheduling (GPU awareness, priority, preemption).

## How this page is maintained

Items move here from the release notes when a version ships, and out of here into
the design book when they get designed. History lives in git and in the
[releases](https://github.com/win07xp/kaalm/releases); this page only ever
describes the present and the future.
