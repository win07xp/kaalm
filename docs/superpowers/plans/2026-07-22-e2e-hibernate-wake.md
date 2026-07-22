# e2e: S7 hibernate/wake + S14 wake_timeout (issue #9)

## Goal

Prove the full hibernation lifecycle on a real k3d cluster: a persistent,
hibernation-enabled Agent goes Running -> Idle -> Hibernating -> Hibernated
(Pod gone, PVC kept), an async webhook wakes it (Resuming -> Running), the
reply is retrieved via the async polling endpoint, and the PVC persists across
the cycle. Plus the S14 negative arm: an impossibly short wakeTimeout yields a
`wake_timeout` error payload on the polling endpoint.

## Why the existing 0s fixtures can't be reused (verified in code)

- `agent_controller.go:207-209`: idle/hibernation evaluation runs only when
  `eff.IdleTimeout > 0`. The golden-path `e2e-standard` class + `e2e-agent`
  use `0s` everywhere, which DISABLES hibernation (correct for golden path,
  which must stay Running). S7 needs POSITIVE small timers.
- `agent_desired.go:146-157`: `pick(v, def, max)` treats `max == 0` as
  "uncapped", and lifecycle values default from the class. So the new class
  can set positive defaults and the agent need not restate them.
- Hibernation also requires class `persistence.enabled: true`,
  `persistence.defaultSizeGi`, and `lifecycle.hibernationAllowed: true`
  (envtest `TestAgent_HibernateAndWake` sets exactly these), and the agent
  `persistence.enabled: true` + `lifecycle.hibernationEnabled: true`.
- PVC name is `<agent>-memory`, mounted at `/var/agent/memory`
  (`agent_desired.go:251,391`).

## Wake path (verified end to end, no new wiring needed)

`user.go:136` async branch -> `async.go:167 handleAsyncAccept` returns 202 with
`{requestId, channelPath}` -> `async.go:204 runAsyncPipeline` ->
`user.go:185 wakeAndDeliver`: on `Phase==Hibernated` it calls
`Activator.Wake` (mTLS to the controller activator, client built by default at
`cmd/gateway/main.go:206` from the gateway's own kaalm-gateway-tls + kaalm-ca)
then `waitAgentReachable` (TCP dial to the agent Service bounded by
`wakeTimeout`, default 120s) then delivers. The reply/error is patched to the
poll record (`async.go:233`) and read at `GET /v1/channels/responses/{id}?channelPath=`
(`async.go:376 handlePoll`, channel bearer auth). `wake_timeout` is the
error-type string (`errors.go:46`); async error payload is
`{error:{type,message,retryable:false}, failedAt}` (`async.go:212-219`).

## Known race to budget for (memory: k3s netpol ipset ~20s lag)

A woken agent is a brand-new Pod; kube-router's netpol ipset lags ~20s before
the gateway's ingress allow-rule to the agent Service takes effect. The S7
happy path relies on the default `wakeTimeout` (120s) to absorb this, and the
poll Eventually window must exceed it (use 180s). The S14 arm turns this race
into the assertion: `wakeTimeout: 1s` guarantees the first wake attempt times
out before the fresh Pod is ever reachable -> deterministic `wake_timeout`.

## Fixtures (test/e2e/testdata/hibernation.yaml)

Descriptive filename; scenario-prefixed resource names (two-axis convention
from #25). All in namespace `e2e` unless cluster-scoped.

- AgentClass `s7-hibernating` (cluster-scoped): `runtime.backend: pod`;
  `image.allowedImages: [registry.test/agents/*]`, `pullPolicy: IfNotPresent`;
  `persistence: {enabled: true, defaultSizeGi: 1}`;
  `lifecycle: {hibernationAllowed: true, defaultIdleTimeout: 2s,
  defaultHibernationDelay: 2s, defaultWakeTimeout: 0s}` (0s wake => 120s
  default). Positive `max*` not required (max=0 is uncapped).
- Agent `s7-agent`: `agentClassRef: s7-hibernating`,
  `image: registry.test/agents/starter-go:e2e`,
  `persistence.enabled: true`,
  `lifecycle: {hibernationEnabled: true, activitySource: gatewayTraffic}`
  (idle/hibernation timers default from the class).
- AgentChannel `s7-channel`: `type: webhook`,
  `webhook: {path: /channels/e2e/s7-channel, responseMode: async,
  auth: {type: bearer, secretRef: {name: s7-hook, key: token}}}`.
  No `callbackUrl` -> the pipeline stores to the poll record (sidesteps the
  SSRF guard that #11 addresses; matches the issue's "poll for the reply").
- Secret `s7-hook` (ns e2e): `token: <bearer>`.
- Agent `s14-agent`: identical to `s7-agent` but
  `lifecycle.wakeTimeout: 1s` (impossibly short).
- AgentChannel `s14-channel`: same as `s7-channel` but path
  `/channels/e2e/s14-channel`, `agentRef: s14-agent`, secret `s14-hook`.
- Secret `s14-hook`.

## Spec (test/e2e/hibernate_wake_test.go, //go:build e2e)

`Describe("Hibernate and wake", Ordered)`:

1. BeforeAll: apply `hibernation.yaml`. Wait class Ready, `s7-agent` phase
   Running (180s), capture the `s7-agent-memory` PVC UID.
2. It hibernates on inactivity: Eventually (180s, 5s) `s7-agent` phase ==
   `Hibernated`. Assert: no Pod with `kaalm.io/agent=s7-agent`; the
   `s7-agent-memory` PVC still exists with the SAME UID (memory intact,
   object level -- see Open Decision); Service `s7-agent` still exists;
   `status.hibernatedAt` set.
   (Idle and Hibernating are transient; assert the terminal Hibernated plus
   the Pod-gone/PVC-kept invariants rather than racing the intermediate
   phases, which the deterministic envtest already pins.)
3. It wakes on an async webhook and returns the reply: port-forward gateway
   :8080; POST the async webhook with the s7 bearer -> expect 202, capture
   `requestId`. Eventually (30s) `s7-agent` phase leaves Hibernated
   (Resuming/Running). Poll `GET /v1/channels/responses/{requestId}?channelPath=/channels/e2e/s7-channel`
   with the bearer; Eventually (180s, 5s) it returns 200 with a `response`
   whose content contains the echoed text (starter-go echoes the message).
   Assert the same PVC UID still present after wake (remounted).
4. S14 It delivers a wake_timeout payload when wakeTimeout is exceeded: drive
   `s14-agent` to Hibernated (Eventually 180s). POST the s14 async webhook ->
   202 + requestId. Poll; Eventually (60s) 200 with
   `error.type == "wake_timeout"` and `retryable == false`.

## Makefile / suite

- `AGENT_IMG` already builds starter-go and imports it (`Makefile:203,214,216`);
  no new images. No `trustClusterCAForUpstream` needed (no LLM upstream here).
- AfterSuite (`e2e_suite_test.go:36`): add `test/e2e/testdata/hibernation.yaml`
  to the teardown list (before namespace/secrets), `--wait=false`.
- The suite shares one cluster; DeferCleanup the cluster-scoped `s7-hibernating`
  class, or rely on AfterSuite delete of the file. Namespaced resources die with
  the `e2e` namespace.

## Verification

- `go build ./...`, `go vet ./...`, `make lint` clean (spec is build-tagged
  e2e; it compiles under the e2e tag).
- `helm`/drift unaffected (no chart change). `make cover-check` unaffected
  (e2e specs are not in the coverage union).
- Full k3d `make e2e` green, including the new Ordered spec. Delete any stale
  k3d cluster first. Expect the S7 wake step to take up to ~40-60s (pod
  recreate + ipset lag); the 180s poll window covers it.

## Open decision (resolve before coding)

"Memory intact" fidelity:
- (A, recommended) Object-level: assert the `s7-agent-memory` PVC survives
  Hibernated and is remounted on wake (same UID). No agent/image change;
  deterministic; matches the design guarantee and envtest. Honest claim:
  "the PVC persists across hibernation and is remounted."
- (B) Byte-level: extend starter-go to write a marker to `/var/agent/memory`
  and echo it post-wake. Proves bytes survived but needs an example-agent
  change, image rebuild, and a pre-hibernation message that resets the idle
  clock (extra wait). Higher flake surface.

## Delivery

Branch `feat/e2e-hibernate-wake` off main, PR closing #9, normal review/merge
cycle. Post-merge: sync main, delete branch, update the scenario-coverage doc
row for S7/S14 if #12 hasn't already, update project memory.
