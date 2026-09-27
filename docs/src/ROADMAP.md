# Implementation roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v1.0.0 shipped on 2026-09-27**
([release](https://github.com/win07xp/kaalm/releases/tag/v1.0.0)). It installs
with a single `helm install` from the published OCI chart, upgrades in place
from the previous release with two documented commands, and every acceptance
scenario (S1 to S24) is proven on a real cluster.

Every component of the v1 design ships: all six CRDs, the reconciling
controller (lifecycle, hibernation and wake, budgets, health probes,
finalizers), the two-listener gateway (LLM proxy with credential isolation,
budgets, rate limits, and fallback trees that cross API formats; the MCP tool
broker; user gateway with webhook, Discord, and WhatsApp channels), the
optional operator console, the Helm chart with cert-manager TLS wiring, the
runtime contract with published base images and starter templates, and the
three books.

The API is `kaalm.io/v1beta1`, and v1.0.0 makes it the compatibility
contract: every change after this release is additive within `v1beta1`, and
anything breaking arrives as a new version with conversion both ways
([Deprecation policy](operations/api-versioning.md#deprecation-policy)).
`v1alpha1` stays served and converted, with a warning per request, and is
removed no earlier than the first minor release after v1.0.0, announced a
release ahead.

What v1.0.0 added on top of v0.7.0 (the "complete release" milestone,
[#51](https://github.com/win07xp/kaalm/issues/51)) is hardening rather than
new surface:

- **A security pass.** The threat model was re-derived over everything added
  since v0.2.0 ([#138](https://github.com/win07xp/kaalm/issues/138)), and
  every listener was audited for authorization and SSRF
  ([#139](https://github.com/win07xp/kaalm/issues/139)): 33 checks, 10
  findings, all fixed or corrected. Each verdict names the files it
  examined, and `make audit-drift` reports which verdicts a later change
  needs re-walked; the whole audit was re-walked against the release commit.
  The design is the [threat model](security/threat-model.md).
- **Secure defaults for workload Pods.** Every agent and task Pod starts from
  the restricted Pod Security Standard (non-root, no privilege escalation,
  a read-only root filesystem with a writable `/tmp`, all capabilities
  dropped, the `RuntimeDefault` seccomp profile) and does not mount a
  ServiceAccount token. A class can relax either, and the class's
  `SecurityBaseline` condition names every field it relaxes.
- **NetworkPolicy for the operator's own Pods.** The chart ships a
  default-deny ingress policy for the controller, gateway, and console that
  admits each port's real callers, with the metrics ports open to your
  monitoring namespace only
  ([The operator's own NetworkPolicy](operations/deployment.md#the-operators-own-networkpolicy)).
- **Hostname egress on Cilium.** `allowedHosts` on an AgentClass becomes a
  `CiliumNetworkPolicy` with `toFQDNs` rules for each workload when the CNI
  is Cilium ([FQDN egress policy](runtime/child-resources.md#fqdn-egress-policy)),
  proven in CI on a Cilium cluster.
- **A scale proof and a performance pass.** A repeatable load harness
  ([#140](https://github.com/win07xp/kaalm/issues/140)) and a profiling pass
  under its baseline ([#174](https://github.com/win07xp/kaalm/issues/174)):
  400 agents on one machine, hibernation churn, and concurrent tasks, with
  the numbers in [Load and scale](operations/load-and-scale.md).
- **The Agent Sandbox decision**
  ([#141](https://github.com/win07xp/kaalm/issues/141)): v1 runs agents as
  plain Pods and documents the RuntimeClass alternative for code-executing
  agents; the `agentSandbox` backend is planned for v1.2.0.
- **A docs audit and its follow-ups.** Every page of the three books was
  checked against the code and rewritten to one style
  ([#142](https://github.com/win07xp/kaalm/issues/142)), with
  `make docs-check` holding links, structure, and wording. The gaps it found
  between the books and the code were filed as issues; the 27 that belonged
  in this release are fixed, among them fields the schema accepted and the
  code never applied, status the design specified and no reconciler wrote,
  and gateway answers that differed from the wire contract.

Quality bar at release: an 85% CI coverage gate, envtest suites against a
real apiserver, a k3d end-to-end suite (76 specs) green locally and in GitHub
Actions, a Cilium job for hostname egress, and an upgrade suite run from both
the previous release and the last pre-graduation release.

The `v0.1.0` tag predates the release workflow that publishes the chart and
images, and installs only from source.

## Next

The next milestone is **v1.1.0**, a fix release. It closes the gaps the
docs audit filed between the books and the code; until each is fixed, its
page describes the shipped behavior and cites the issue. It also verifies
and documents the microVM path, Kata first
([#168](https://github.com/win07xp/kaalm/issues/168)), adds the tool-plane
and streaming phases of the load harness
([#175](https://github.com/win07xp/kaalm/issues/175)), and carries the
tooling and test issues. The full list is on the
[milestone](https://github.com/win07xp/kaalm/milestone/8).

The milestone after it is **v1.2.0**, the isolation milestone: the
`agentSandbox` runtime backend
([#167](https://github.com/win07xp/kaalm/issues/167)). It starts when the
upstream conditions the issue lists hold. One of them is a quarter of
stable releases on upstream's v1beta1 line, so the start is no earlier than
late November 2026. Hostname egress on Calico Enterprise
([#273](https://github.com/win07xp/kaalm/issues/273)) is on the same
[milestone](https://github.com/win07xp/kaalm/milestone/9).

## Beyond

The unscheduled backlog: the deferrals the design itself names (see
[Scope for v1](concepts/vision-and-scope.md#scope-for-v1)), roughly in the order
they are likely to matter:

- **The Discord Gateway WebSocket adapter.** Free-text message bots need a
  persistent connection per bot (identify, heartbeat, resume, sharding) and
  one replica to own it; the v0.7.0 Discord adapter covers slash commands
  over HTTP instead. Tracked as
  [#124](https://github.com/win07xp/kaalm/issues/124).
- **Serving the `google-vertex` provider type.** The enum is reserved and
  the adapter's outbound pieces exist, but no inbound path is routed and
  the OAuth2 token minting the platform requires is not implemented
  ([#147](https://github.com/win07xp/kaalm/issues/147) records the
  alignment). Google now brands the platform the Gemini Enterprise Agent
  Platform; the wire API is unchanged.
- **Tool plane extensions.** Pricing tool calls (a `costPerCallUSD` facet
  for provider-side tools on the ModelProvider, a per-call cost on the
  ToolProvider catalog), the MCP surfaces the broker denies (`resources/*`,
  `prompts/*`, `sampling/*`, and the server-initiated notification stream,
  tracked as [#80](https://github.com/win07xp/kaalm/issues/80)), stripping
  or rejecting an undeclared server tool in a request body, and in-process
  tool observability at the LLM gateway. Each is designed far enough to
  name in [The tool plane](gateways/tool-plane.md) and not built.
- **A `v1` API version.** v1.0.0 keeps `v1beta1` as its API. A `v1`
  arrives when real usage asks for a change `v1beta1` cannot absorb
  additively, under the
  [Deprecation policy](operations/api-versioning.md#deprecation-policy).
- **Larger horizons.** Agent-to-agent orchestration, multi-cluster
  federation, agent-aware scheduling (GPU awareness, priority, preemption),
  audit-log export, and cost analytics and chargeback reporting.

## How this page is maintained

Items move here from the release notes when a version ships, and out of here into
the design book when they get designed. History lives in git and in the
[releases](https://github.com/win07xp/kaalm/releases); this page describes
the present, the current milestone, and what is deferred.
