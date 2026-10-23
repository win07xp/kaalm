# Implementation roadmap

The rest of this book is the design. This page is where the implementation stands
against it, and what comes next.

## Where the project stands

**v2.0.0 is the first stable release, and a fresh start.** It installs with a
single `helm install` from the published OCI chart. It has no upgrade path
from 1.x or 0.x: a cluster on an earlier release removes it and installs
2.0.0 ([Moving from 1.x or 0.x](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/upgrading.md#moving-from-1x-or-0x)).
Upgrades within 2.x are two documented steps, the new CRDs and then
`helm upgrade`. The latest release is v2.0.1, a patch release on 2.0.0 with
no API, CRD schema, or chart value changes. Every acceptance scenario
(S1 to S25) has a test that proves it on a real cluster. S21, the in-place
upgrade, runs on every release PR, and the release tags only after it passed
on the PR's head commit.

Every component of the design ships: all six CRDs, the reconciling
controller (lifecycle, hibernation and wake, budgets, health probes,
finalizers), the two-listener gateway (LLM proxy with credential isolation,
budgets, rate limits, and fallback trees that cross API formats; the MCP tool
broker; user gateway with webhook, Discord, and WhatsApp channels), the
optional operator console, the Helm chart with cert-manager-issued TLS, the
runtime contract with published base images and starter templates, and the
three books.

The API is `kaalm.io/v1beta1`, the only version served and the stored one. It
is the compatibility contract: every change is additive within `v1beta1`, and
anything breaking arrives as a new version with conversion both ways
([Deprecation policy](operations/api-versioning.md#deprecation-policy)).

The 0.x releases built the design in stages and graduated the API from
`v1alpha1` to `v1beta1`. The 1.0.0 release hardened it: a security audit of
every listener, restricted Pod defaults for workloads, NetworkPolicy for the
operator's own Pods, hostname egress on Cilium, and a performance harness
with a scale baseline. 2.0.0 removes the code that only carried objects
through those upgrades, which is why it installs fresh.

Quality bar: an 85% CI coverage minimum, envtest suites against a real
apiserver, a k3d end-to-end suite green locally and in GitHub Actions, and a
Cilium job for hostname egress.

## Next

The next milestone is **v2.1.0**. Its main item is the isolation work: the
`agentSandbox` runtime backend
([#167](https://github.com/win07xp/kaalm/issues/167)). It starts when the
upstream conditions the issue lists hold. One of them is a quarter of
stable releases on upstream's v1beta1 line, so the start is no earlier than
late November 2026. The milestone also carries a Claude Code coding agent
as a proof of concept ([#476](https://github.com/win07xp/kaalm/issues/476)),
and an efficiency review of the gateway and controller each holding one watch
per Secret they read ([#485](https://github.com/win07xp/kaalm/issues/485)). The full list is on the
[milestone](https://github.com/win07xp/kaalm/milestone/9).

## Beyond

The unscheduled backlog: the deferrals the design itself names (see
[Scope](concepts/vision-and-scope.md#scope)), roughly in the order
they are likely to matter:

- **The Discord Gateway WebSocket adapter.** Free-text message bots need a
  persistent connection per bot (identify, heartbeat, resume, sharding) and
  one replica to hold it; the Discord adapter covers slash commands over
  HTTP instead. Tracked as
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
- **A `v1` API version.** `v1beta1` is the only API version. A `v1`
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
