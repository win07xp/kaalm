# Implementation Roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v0.6.0 shipped on 2026-08-24**
([release](https://github.com/win07xp/kaalm/releases/tag/v0.6.0)). It installs
with a single `helm install` from the published OCI chart, upgrades in place
from the previous release with two documented commands, and every acceptance
scenario (S1 to S21) is proven on a real cluster.

The operator is feature-complete against the v1 design: all six CRDs, the
reconciling controller (lifecycle, hibernation and wake, budgets, health probes,
finalizers), the two-listener gateway (LLM proxy with credential isolation,
budgets, rate limits, and fallback trees; the MCP tool broker; user gateway
with sync and async webhooks), the optional operator console, the Helm chart
with cert-manager TLS wiring, the runtime contract with published base images
and starter templates, and this book.

What v0.6.0 added on top of v0.5.0 (the "API graduation" milestone):

- **The graduated API.** `v1beta1` is the hub and storage version for all six
  CRDs, with a schema deliberately identical to `v1alpha1` (the field-change
  audit deferred every lossy candidate to v1). `v1alpha1` stays served,
  deprecated with a per-kind warning, and converted in both directions by a
  conversion webhook every controller replica serves; round-trip fuzz tests
  enforce wire-form identity for every kind in both directions, so a field
  added to one version and not the other fails the suite. The design is
  [API Versioning and Deprecation](operations/api-versioning.md), which also
  carries the written deprecation policy: `v1alpha1` is served at least
  through v1.0.0, and its removal is announced a release ahead.
- **Storage-version migration.** On its first start after an upgrade, the
  leader rewrites every stored object at `v1beta1` and trims each CRD's
  `storedVersions`, so the graduation finishes without manual steps; the
  counter `kaalm_storage_migrated_objects_total` and one log line per kind
  record it.
- **The upgrade e2e (S21).** A suite that installs the previous *released*
  chart, loads it with `v1alpha1` workloads, runs the two documented upgrade
  steps to the current build, and proves nothing was recreated and nothing
  lost: same Pod, state intact on the volume, a hibernated agent that wakes
  on its next message, finished task status preserved, `storedVersions`
  migrated. It runs on release tags and on PRs that touch the API surface,
  and it is the release-readiness gate before every tag. The user-facing
  procedure is the guide's
  [Upgrading Kaalm](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/upgrading.md) page.
- **MCP 2026-07-28, dual-era.** The tool plane speaks both the stateless
  2026-07-28 revision and the 2025 handshake era for the length of the
  upstream deprecation window: the ToolProvider probe negotiates per server
  (`server/discover`, with `initialize` fallback) and records
  `status.mcpRevision`; the broker enforces per request (mirrored-header
  validation with the revision's own `HeaderMismatch` rejection, the
  `cacheScope: private` rewrite on grant-filtered lists, session ownership
  scoped to the legacy era). The design is the tool-plane chapter's
  [Protocol Revisions](gateways/tool-plane.md#protocol-revisions) section.

Quality bar at release: 87% project test coverage enforced by an 85% CI gate,
envtest suites against a real apiserver, a k3d end-to-end suite (64 specs)
that is green both locally and in GitHub Actions, and the 6-spec upgrade
suite that gates every release tag.

Two honest caveats:

- `v1alpha1` is deprecated. Everything that says it keeps working, with a
  warning per request, at least through v1.0.0; the
  [deprecation policy](operations/api-versioning.md#deprecation-policy) is
  the contract, and moving a manifest is one `apiVersion` line because the
  schema is identical.
- The `v0.1.0` tag predates the release machinery and installs only from
  source.

## Next

The current milestone is **v0.7.0, "Reach"** (tracking issue
[#50](https://github.com/win07xp/kaalm/issues/50)): Discord and WhatsApp
channel adapters for the user gateway, and cross-format provider fallback so
fallback chains can cross `spec.type`. Behind it stands **v1.0.0, "The
complete release"** ([#51](https://github.com/win07xp/kaalm/issues/51)): the
security pass, the scale proof, the Agent Sandbox decision, and the docs
audit.

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
