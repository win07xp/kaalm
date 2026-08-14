# Implementation Roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v0.4.0 shipped on 2026-08-14**
([release](https://github.com/win07xp/kaalm/releases/tag/v0.4.0)). It installs
with a single `helm install` from the published OCI chart, and every
implemented acceptance scenario (S1 to S18) is proven on a real cluster.

The operator is feature-complete against the v1 design: all six CRDs, the
reconciling controller (lifecycle, hibernation and wake, budgets, health probes,
finalizers), the two-listener gateway (LLM proxy with credential isolation,
budgets, rate limits, and fallback trees; the MCP tool broker; user gateway
with sync and async webhooks), the Helm chart with cert-manager TLS wiring,
the runtime contract with published base images and starter templates, and
this book.

What v0.4.0 added on top of v0.3.0 (the "tool plane" milestone):

- **The tool plane, implemented** as designed in
  [The Tool Plane](gateways/tool-plane.md): the cluster-scoped `ToolProvider`
  CRD and its reconciler with the MCP health probe, the class and workload
  grant chain enforced at reconcile time and call time (validation rules 35
  to 38), the gateway broker on the cluster listener (`/v1/mcp/*` with
  credential injection, stateless session ownership, a filtered `tools/list`,
  and rate limits), the per-call audit record and metric surface, and
  provider-side server-tool counts on the LLM path. Scenario S18 is proven
  on a real cluster. Three breaking changes rode this milestone; the
  [release notes](https://github.com/win07xp/kaalm/releases/tag/v0.4.0)
  state them prominently.
- **Framework agents, documented and equipped**: the guide's Running
  Framework Agents page covers both adoption tiers with exact wire facts,
  three runnable LangGraph examples under `examples/langgraph-*/` span the
  on-ramp rungs, and the Python ABI grew rotation-aware httpx client
  factories (`kaalm.http_client()` / `kaalm.http_async_client()`) so a
  framework presents the pod's mTLS identity with no handler code.

Quality bar at release: 87% project test coverage enforced by an 85% CI gate,
envtest suites against a real apiserver, and a k3d end-to-end suite (42
specs) that is green both locally and in GitHub Actions.

Two honest caveats:

- The API is `v1alpha1` and may change in breaking ways between minor
  releases, as both the v0.2.0 `agentry.io` to `kaalm.io` move and the
  v0.4.0 breaking notes show.
- The `v0.1.0` tag predates the release machinery and installs only from
  source.

## Next

The current milestone is **v0.5.0, "the console and observability"**, tracked
in the [v0.5.0 GitHub milestone](https://github.com/win07xp/kaalm/milestone/4)
(umbrella issue [#48](https://github.com/win07xp/kaalm/issues/48)): an
optional operator console, concrete Grafana dashboards for the shipped metric
catalogs, and OpenTelemetry tracing across the gateway to agent to provider
hops; see [Observability](operations/observability.md) for the surfaces they
build on.

Beyond v0.5.0, the milestone runway to v1.0.0 is laid out in the
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
- **Cross-format provider fallback.** Translation between provider API formats
  (for example Anthropic to OpenAI) so fallback chains can cross `spec.type`.
- **Larger horizons.** Agent-to-agent orchestration, a web UI, multi-cluster
  federation, and agent-aware scheduling (GPU awareness, priority, preemption).

## How this page is maintained

Items move here from the release notes when a version ships, and out of here into
the design book when they get designed. History lives in git and in the
[releases](https://github.com/win07xp/kaalm/releases); this page only ever
describes the present and the future.
