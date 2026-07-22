# Scenario Coverage

The [acceptance scenarios](scenarios.md) S1 to S15 are the north-star
definition of "done". This page maps each scenario to the implemented behavior
that exercises it and the automated tests that verify it, so the acceptance
surface is auditable rather than aspirational.

Coverage is one of four kinds:

- **Unit / integration**: a Go test in the repo asserts the behavior directly.
- **Envtest**: a controller-runtime test against a real apiserver asserts the
  reconciler behavior.
- **End-to-end (e2e)**: a Ginkgo spec (`test/e2e/`, build tag `e2e`) runs
  against a real k3d cluster with the chart installed and the real binaries,
  and asserts the behavior end to end. This is the strongest kind: it exercises
  the kubelet, real garbage collection, cert-manager, and kube-router that the
  lighter kinds cannot. Run with `make e2e`; CI runs it on every PR.
- **Live smoke**: verified by hand against a k3d cluster during the phase that
  built it (recorded in the commit message). The v0.2.0 e2e specs below now
  automate what these smokes checked once.

The **e2e** column is the v0.2.0 acceptance surface: every scenario points at
the spec that proves it on a cluster. It is green for every scenario except
S11, whose spec is tracked in
[#30](https://github.com/win07xp/kaalm/issues/30); the milestone closes when
that lands.

| Scenario | e2e spec (`test/e2e/`) | Also covered by |
|---|---|---|
| S1 Install + standard class | `Deployment` (five CRDs) + `Golden path` (AgentClass to Ready) | Envtest `TestAgentClass_*` |
| S2 Sandboxed class (RuntimeClass, allowlist) | `Sandboxed class (S2)` (runtimeClassName passthrough; image-allowlist rejection) | Unit `TestDeriveEffectiveSpec_*`, `TestDesiredPod_*` |
| S3 Shared provider + per-namespace budget | `Golden path` (ModelProvider to Ready) + `Fallback and budget` (S10 block) | Unit `TestBudgetLedger_EnforceThresholds`, `TestProxy_BudgetDegradeAndBlock` |
| S4 Fallback chain for availability | `Fallback and budget` (S4: walks to the fallback provider) | Unit `TestFallback_*`, `TestIntegration_FallbackChainWalksToBackup` |
| S5 Revoke a team | `Access revocation (S5)` (403 + Agent `Degraded`, Pod kept) | Unit `TestProxy_TenancyDenialsInOrder`; envtest `TestAgent_ProviderNamespaceDeniedDegrades` |
| S6 Deploy a persistent agent | `Golden path` (Agent Pod to Running with child resources) | Envtest `TestAgent_ProvisionToRunning` |
| S7 Hibernate + wake on message | `Hibernate and wake` (hibernates; wakes on async webhook, memory kept) | Envtest `TestAgent_HibernateAndWake` |
| S8 Ephemeral coding agent | `Task lifecycle` (agentReported to Succeeded; mailbox; TTL GC) | Envtest `TestTask_*` |
| S9 Promote a task to persistent | `Promotion via existingClaim` (adopt a pre-populated PVC; read state back) | Envtest `TestAgent_ExistingClaimNotFound`; CEL `sizeGi`/`existingClaim` mutex |
| S10 Budget-exhausted graceful fail | `Fallback and budget` (S10: 429 `budget_exhausted`, Agent `Degraded`) | Unit `TestProxy_BudgetDegradeAndBlock` |
| S11 Clean teardown on delete | pending, tracked in [#30](https://github.com/win07xp/kaalm/issues/30) | Envtest `TestAgent_FinalizerRetainStripsPVCOwnerRef`, `TestAgent_FinalizerDeleteKeepsPVCOwnerRef` |
| S12 Personal assistant via webhook | `Session identity and async callback` (S12: stable/distinct sessionId) + `Golden path` (sync delivery) | Unit `TestWebhook_SyncRoundTrip` |
| S13 Generic webhook exposure | `Golden path` (delivers a sync webhook and returns the reply) | Unit `TestWebhook_ExtractorsAndBadJSON` |
| S14 Webhook for a hibernated agent | `Hibernate and wake` (S14: `wake_timeout` payload on the polling endpoint) | Unit `TestWebhook_Async*` |
| S15 Async webhook for a long-running agent | `Session identity and async callback` (S15: 202, signed callback to the receiver, polling fallback) | Unit `TestWebhook_AsyncAcceptAndPoll`, `TestWebhook_AsyncCallbackDelivery` |

The LLM-proxy scenarios (S4, S10, S15) are exercised against an in-cluster mock
upstream deployed by the `Mock LLM provider` spec; the S2 and S6 NetworkPolicy
behavior is additionally exercised by the `Golden path` cross-namespace deny
probe.

Two scenarios keep a scope note: their e2e spec proves the Kaalm-owned
primitive, but part of the scenario is outside the code's test surface.

- **S2**'s gVisor sandbox depends on a `RuntimeClass` the local k3d loop does
  not install; the e2e proves the reconciler wiring (the class
  `runtimeClassName` reaches the Pod spec, and the image allowlist rejects a
  disallowed image), but the sandbox-escape assertion is an operator-side
  security property outside the code's test surface.
- **S9**'s `VolumeSnapshot` step is standard Kubernetes, not Kaalm machinery,
  and k3d's local-path provisioner has no snapshot support; the e2e proves the
  enabling primitive (`Agent.spec.persistence.existingClaim` adoption and
  read-back), with the snapshot documented rather than CI-proven.
