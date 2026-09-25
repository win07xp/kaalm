# Status cheatsheet

Everything Kaalm tells you through `kubectl get` and `describe`, resource
by resource. Conditions listed are the ones the controller actually sets.

## Agent

`kubectl get agents` columns: `Phase`, `Ready`, `Class`, `Age`.

![Agent state machine. Pending to Provisioning on Certificate created, Provisioning to Running on Pod Ready, Running to Idle when idleTimeout elapses, Idle back to Running on activity observed, Idle to Hibernating when hibernationDelay elapses, Hibernating to Hibernated when the Pod is gone, Hibernated to Resuming on the wake annotation, and Resuming to Provisioning when the Pod is created. Running and Idle return to Provisioning on spec drift or Pod disruption. From any phase: Degraded on a class mismatch, returning to that phase when the mismatch clears; Failed on a crash loop or image pull failure, returning to Provisioning when the Pod recovers or is replaced; Terminating when deleted.](../diagrams/agent-lifecycle.svg)

Phases, in lifecycle order:

| Phase | Meaning |
|---|---|
| `Pending` | Accepted, children not created |
| `Provisioning` | Pod, PVC, Service, Certificate, NetworkPolicy coming up; the Pod waits on its certificate |
| `Running` | Everything up; `Ready: True` |
| `Idle` | No activity for `idleTimeout`; still running |
| `Hibernating` | Pod being torn down, PVC retained |
| `Hibernated` | No Pod; storage and identity parked |
| `Resuming` | Waking: Pod recreating after a wake trigger |
| `Degraded` | The spec fails a class gate (image, provider or tool grant revoked, deleted, or narrowed); the Pod keeps running |
| `Failed` | Crash-looping, or the image cannot be pulled |
| `Terminating` | Deletion in progress, finalizer running |

Conditions: `Ready` (the roll-up), `GatewayReachable` (the controller's view
of the gateway; reasons `GatewayReady` and `GatewayUnavailable`),
`ProvidersReady` (`AllProvidersHealthy` when every referenced provider is
allowed, exists, and is `Ready`; `ClassConstraintViolation` when one is not
allowed or does not exist; `ProviderUnhealthy` when one is not `Ready`), and
`Degraded`, which carries one reason, `BudgetExhausted`, present only while a
referenced provider reports the namespace budget-blocked. Neither
`ProvidersReady` nor `Degraded` changes the phase.

`Ready=False` reasons that move the phase to `Degraded` (the message names
the failed gate): `ClassConstraintViolation` (an image, provider, or tool
grant does not pass its class or allowlist gate), `ToolNotInCatalog` (a
granted tool is outside the ToolProvider's declared catalog),
`PersistenceNotAllowed`, `HibernationNotAllowed`,
`HibernationRequiresPersistence`, and `HandlerMountNotAllowed` (the class
does not allow handler mounts).

`Ready=False` reasons that hold the Agent without degrading it:
`InvalidReference` (no image, the class does not exist, or the class has a
malformed `allowedCIDRs` entry),
`ImagePullSecretMissing`, `ExistingClaimNotFound` (the adopted PVC is
missing), `HandlerConfigMapNotFound`, `CertificateNotReady`, and
`SystemNamespaceForbidden` (an Agent in `kaalm-system` is never
provisioned). Reasons on `Ready` that report progress: `PodProvisioning`,
`PodRunning`, `PodDisrupted`, `SpecDrift`, `Hibernated`, and `Woken`; an
AgentTask retry also reports `PodTerminating` while the old Pod drains. A wake
annotation on an Agent that is not `Hibernated` is refused with event reason
`WakeIgnored`.

## AgentTask

`kubectl get agenttasks` columns: `Phase`, `Class`, `Age`.

Phases: `Pending`, `Provisioning`, `Running`, `Completing` (result being
recorded), then one of `Succeeded`, `Failed`, `TimedOut`; `Terminating` on
delete. There is no Degraded: a task that cannot run fails, including a
task whose provider or tool grant fails a gate at provisioning (same
reasons as the Agent's Degraded, but terminal here).

Conditions: `Ready` (provisioning gate) and `Completed` (terminal verdict,
reason `TaskSucceeded`, `TaskFailed`, `TimeoutExceeded`, or
`TimeoutSucceeded`). A completion call from the wrong Pod is refused with
`403 access_denied` and the message prefix `StalePodCompletion` (retryable
by the task) or `TaskAlreadyCompleted` (final); neither is a condition.

## AgentChannel

`kubectl get agentchannels` columns: `Agent`, `Phase`, `Connected`, `Age`.

Phases: `Active` and `Degraded` mirror the bound Agent's phase; `Failed`
means the bound Agent does not exist; `Terminating` is the delete path; the
phase is unset until the first reconcile. `Connected` shows the
`PlatformConnected` condition, the gateway's view of recent deliveries:
`True` with `WebhookReady`, `Unknown` with `NoRecentTraffic`, or `False`
with the reason of the most recent failure: `WebhookAuthFailed` (signature or
token), `AgentNotReady`, `DispatchFailed`, `CallbackInvalid` (a `callbackUrl`
that fails the pre-dial check), or `CallbackRejected` (a callback receiver or
a platform refused the reply).

`Ready=True` carries reason `AgentReachable`. `Ready=False` reasons:
`AgentNotFound` (the bound Agent does not exist), `InvalidReference`,
`AgentServiceDisabled` (the bound Agent has `service.enabled: false`),
`InvalidPath`, `PathConflict`, `InvalidCallbackUrl`,
`SystemNamespaceForbidden`, `CredentialsMissing` (the auth or platform
credential Secret is absent or lacks a required key), `CredentialsInvalid`
(the Discord public key is not a valid Ed25519 key), `CallbackAuthMissing`
(the `callbackAuth` Secret or key does not exist), and `CallbackAuthInvalid`
(the `callbackAuth` key is empty).

## ModelProvider

`kubectl get modelproviders` columns: `Type`, `Ready`, `Healthy`, `Age`.

- `Ready`: spec valid and credentials resolve. False reasons:
  `CredentialsMissing`, `CredentialsInvalid`, `InvalidDegradeTarget`,
  `FallbackIneligible`, `InvalidModelMap`, `HardBudgetUnpriced` (hard
  enforcement requires a fully priced model catalog).
- `Healthy`: the periodic upstream probe (`UpstreamReachable` when good,
  `ProviderUnhealthy` when not, `ProbeSkipped` for a type with no probe).
  Ready without Healthy means valid config, unreachable provider.
- `GatewayReachable`: mirrored onto every provider from the controller's
  view of the gateway Pods.
- `DegradeTargetNotCheapest` (advisory, never affects `Ready`): a degrade
  policy's `degradeTo` is not the cheapest model in the catalog
  (`CheaperModelAvailable`); `False` with `DegradeTargetCheapest` once it is.
- `BoundaryMarginRaised` (hard enforcement only): observed traffic forced the
  gateway to admit more conservatively than the configured
  `boundaryMarginPercent`; a signal to raise the value, not an outage.

Budget state lives in status:

```bash
kubectl get modelprovider PROVIDER_NAME -o jsonpath='{.status.budgetUsage}' | jq
```

Each entry: namespace, period, `spentUSD`, `percentUsed`, and `state`
(`Normal` / `Throttled` / `Blocked`).

## ToolProvider

`kubectl get toolproviders` columns: `Type`, `Ready`, `Healthy`, `Age`.

- `Ready`: the spec is valid and, when `credentialsRef` is set, the Secret
  resolves in `kaalm-system`; a provider with no credential is Ready. False
  reasons: `CredentialsMissing`, `CredentialsInvalid` (the server rejected
  the injected credential).
- `Healthy`: the periodic probe, which speaks MCP (`server/discover` or
  `initialize`, then `tools/list`; the negotiated revision lands in
  `status.mcpRevision`); `UpstreamReachable` when good, `ProviderUnhealthy`
  when not. As with ModelProvider, Ready without Healthy means valid config,
  unreachable server. The probe trusts the system CA roots plus whatever
  `controller.trustClusterCAForProbes` and `controller.probeCA.configMap`
  add ([Providing LLM access](../platform/llm-access.md#4-trust-a-private-ca)).

## AgentClass

`kubectl get agentclasses` columns: `Agents`, `Tasks`, `Age`; the counts are
live usage, which is also your "is anyone still using this class" check
before deleting one.

Conditions: `Ready` (`AllReferencesResolved`; `InvalidCIDR` when an
`allowedCIDRs` entry does not parse; otherwise `InvalidReference` when a
listed provider or tool provider does not exist or an `allowedHosts` entry is
not a DNS name) and `FQDNPolicySupported`
(whether the CNI supports the FQDN egress rules the class asks for; reason
`NoHostsRequested` when the class names no `allowedHosts`,
`FQDNPolicySupported` when it does and the CNI can carry them out,
`FQDNPolicyUnsupported` when it cannot). A fresh class shows empty `AGENTS` and `TASKS`
columns until something uses it.

## One-liners

```bash
# Watch an agent come up or wake
kubectl get agents -w

# Every Kaalm object in a namespace
kubectl get agents,agenttasks,agentchannels -n NAMESPACE

# The cluster-scoped set
kubectl get agentclasses,modelproviders,toolproviders

# Any agent failing a class or provider gate, cluster-wide (a budget block keeps its phase)
kubectl get agents -A | grep Degraded

# Why exactly is this resource not Ready
kubectl describe agent AGENT_NAME | sed -n '/Conditions:/,/Events:/p'
```

---

*How this works: design book pages Controller, Agent lifecycle (the phase
machine), Resources (each CRD page documents its full status), and
Operations, Observability (the metrics that complement these statuses).*
