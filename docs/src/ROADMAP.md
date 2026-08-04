# Implementation Roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v0.3.0 shipped on 2026-08-04**
([release](https://github.com/win07xp/kaalm/releases/tag/v0.3.0)). It installs
with a single `helm install` from the published OCI chart, and every
implemented acceptance scenario (S1 to S17) is proven on a real cluster.

The operator is feature-complete against the v1 design: all five CRDs, the
reconciling controller (lifecycle, hibernation and wake, budgets, health probes,
finalizers), the two-listener gateway (LLM proxy with credential isolation,
budgets, rate limits, and fallback trees; user gateway with sync and async
webhooks), the Helm chart with cert-manager TLS wiring, the runtime contract
with Go and Python starter templates, and this book.

What v0.3.0 added on top of v0.2.0 (the "on-ramp" milestone):

- **Reference base images.** `kaalm-agent-go` and `kaalm-agent-python` are
  published alongside the release, embedding the runtime contract; see
  [Reference Base Images](runtime/base-images.md). With the `spec.handler`
  ConfigMap mount (gated by `AgentClass.spec.image.allowHandlerMounts`,
  validation rules 30 and 31, scenario S16), a first agent is a handler file
  mounted into a published image: no image build, no registry. The beginners'
  tutorial (`learn/`) is rewritten around this path and walked against the
  published 0.3.0 artifacts.
- **Opt-in hard budget enforcement**, designed and shipped in
  [Hard Enforcement](gateways/llm/budgets-and-rate-limits.md#hard-enforcement):
  per-provider `budget.enforcement: hard` turns the block threshold into a
  guarantee, via boundary-region serialized admission, an observed-traffic
  safety margin (surfaced as the `BoundaryMarginRaised` condition), and a
  fail-closed posture when budget state goes stale (validation rules 32 to 34,
  scenario S17). Soft limits stay the default.
- **The tool plane, designed** in [The Tool Plane](gateways/tool-plane.md):
  gateway-brokered MCP access through a cluster-scoped `ToolProvider`, the
  class and workload grant chain (validation rules 35 to 38), credential
  injection with stateless session ownership, per-call audit, and acceptance
  scenario S18. Design only in v0.3.0; the implementation is the whole of the
  v0.4.0 milestone.

Quality bar at release: 87% project test coverage enforced by an 85% CI gate,
envtest suites against a real apiserver, and a k3d end-to-end suite that is
green both locally and in GitHub Actions.

Two honest caveats:

- The API is `v1alpha1` and may change in breaking ways between minor releases,
  as the `agentry.io` to `kaalm.io` move in v0.2.0 showed.
- The `v0.1.0` tag predates the release machinery and installs only from
  source.

## Next

The current milestone is **v0.4.0, "the tool plane"**, tracked in the
[v0.4.0 GitHub milestone](https://github.com/win07xp/kaalm/milestone/3)
(umbrella issue [#47](https://github.com/win07xp/kaalm/issues/47)):

- **Implement the tool plane** as designed in
  [The Tool Plane](gateways/tool-plane.md): the `ToolProvider` CRD and its
  reconciler with the MCP health probe, the gateway broker on the mTLS
  listener with credential injection and stateless session ownership, grant
  enforcement on both `tools/call` and `tools/list` (validation rules 35 to
  38), per-call audit and rate limits, provider-side tool count extraction on
  the LLM path, removal of the inert `Agent.spec.mcpServers` field, and the
  S18 e2e proof.
- **Framework agents, documented**
  ([#68](https://github.com/win07xp/kaalm/issues/68)): running existing
  LangGraph and LangChain agents on Kaalm at both adoption tiers, with worked
  examples spanning different kinds of agent work.

Beyond v0.4.0, the milestone runway to v1.0.0 is laid out in the
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
- **Cross-format provider fallback.** Translation between provider API formats
  (for example Anthropic to OpenAI) so fallback chains can cross `spec.type`.
- **Larger horizons.** Agent-to-agent orchestration, a web UI, multi-cluster
  federation, and agent-aware scheduling (GPU awareness, priority, preemption).

## How this page is maintained

Items move here from the release notes when a version ships, and out of here into
the design book when they get designed. History lives in git and in the
[releases](https://github.com/win07xp/kaalm/releases); this page only ever
describes the present and the future.
