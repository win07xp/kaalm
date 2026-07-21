# Scenario Coverage

The [acceptance scenarios](scenarios.md) S1 to S15 are the north-star
definition of "done". This page maps each scenario to the implemented behavior
that exercises it and the automated tests that verify it, so the acceptance
surface is auditable rather than aspirational.

Coverage is one of three kinds:

- **Unit / integration**: a Go test in the repo asserts the behavior directly.
- **Envtest**: a controller-runtime test against a real apiserver asserts the
  reconciler behavior.
- **Live smoke**: verified by hand against a k3d cluster with the real binaries
  during the phase that built it (recorded in the commit message).

| Scenario | Behavior | Coverage |
|---|---|---|
| S1 Install + standard class | AgentClass validation, defaults, conditions | Envtest `TestAgentClass_*`; live smoke (Phase 3) |
| S2 Sandboxed class (RuntimeClass, PVC, no host net) | Class `runtime.runtimeClassName`, persistence, network flow into the Pod spec | Unit `TestDeriveEffectiveSpec_*`, `TestDesiredPod_*`; envtest provisioning |
| S3 Shared provider + per-namespace budget | ModelProvider validation, budget policies (degrade at 80%, block at 100%), `allowedNamespaces` | Unit `TestBudgetLedger_EnforceThresholds`, `TestProxy_BudgetDegradeAndBlock`; envtest `TestModelProvider_BudgetReducer*` |
| S4 Fallback chain for availability | Depth-first fallback walk, same-type constraint, depth cap, exhaustion mapping | Unit `TestFallback_*`, `TestIntegration_FallbackChainWalksToBackup`, `TestIntegration_FallbackExhaustionMaps503` |
| S5 Revoke a team | `allowedNamespaces` removal → gateway 403 + Agent `Degraded` via the ModelProvider watch | Unit `TestProxy_TenancyDenialsInOrder`; envtest `TestAgent_ProviderNamespaceDeniedDegrades` |
| S6 Deploy a persistent agent | Agent → Pod, PVC, Service, Certificate, NetworkPolicy; `Running` with endpoint | Envtest `TestAgent_ProvisionToRunning`; live smoke (Phase 3) |
| S7 Hibernate + wake on message | Idle→Hibernating→Hibernated (Pod deleted, PVC kept); activator wake to Resuming→Running | Envtest `TestAgent_HibernateAndWake`; live activator smoke (Phase 6) |
| S8 Ephemeral coding agent | AgentTask run-to-completion, agentReported completion, artifact collection, timeout | Envtest `TestTask_*`; live exitCode + agentReported smokes (Phase 4, 8) |
| S9 Promote a task to persistent | `Agent.spec.persistence.existingClaim` (rule 27); PVC not owner-referenced | Envtest `TestAgent_ExistingClaimNotFound`; CEL `sizeGi`/`existingClaim` mutex |
| S10 Budget-exhausted graceful fail | Gateway 429 `budget_exhausted`; recoverable `Degraded` condition path | Unit `TestProxy_BudgetDegradeAndBlock`; the block path returns 429 without fallback |
| S11 Clean teardown on delete | Finalizer: SIGTERM the Pod, `pvcRetention` ownerRef rewrite, release | Envtest `TestAgent_Finalizer{Retain,Delete}*`; live delete smoke |
| S12 Personal assistant via webhook | AgentChannel, bearer auth, User Gateway delivery, conversation memory via PVC; starter template | Unit `TestWebhook_SyncRoundTrip`; live template webhook smoke (Phase 8) |
| S13 Generic webhook exposure | AgentChannel webhook path, normalize, deliver, return the reply | Unit `TestWebhook_SyncRoundTrip`, `TestWebhook_ExtractorsAndBadJSON`; live smoke (Phase 7) |
| S14 Webhook for a hibernated agent | Wake-then-deliver; async `wake_timeout` payload on failure | Unit `TestWebhook_Async*`; the wake path in `wakeAndDeliver` |
| S15 Async webhook for a long-running agent | `responseMode: async`, 202 + `requestId`, callback delivery, polling fallback | Unit `TestWebhook_AsyncAcceptAndPoll`, `TestWebhook_AsyncCallbackDelivery`; live async smoke (Phase 7) |

Two scenarios are deliberately not fully automated in v1:

- **S9** ships the enabling primitive (`existingClaim`); the `VolumeSnapshot`
  step is standard Kubernetes, not Agentry machinery, so it is not part of the
  Agentry test surface.
- **S2**'s gVisor sandbox depends on a `RuntimeClass` the local k3d loop does
  not install; the reconciler wiring (passing `runtimeClassName` into the Pod
  spec) is unit-tested, but the sandbox-escape assertion is an operator-side
  security property outside the code's test surface.
