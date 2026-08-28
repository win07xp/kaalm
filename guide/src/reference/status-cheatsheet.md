# Status Cheatsheet

Everything Kaalm tells you through `kubectl get` and `describe`, resource
by resource. Conditions listed are the ones the controller actually sets.

## Agent

`kubectl get agents` columns: `Phase`, `Ready`, `Class`, `Age`.

Phases, in lifecycle order:

| Phase | Meaning |
|---|---|
| `Pending` | Accepted, children not yet created |
| `Provisioning` | Pod, PVC, Service, Certificate, NetworkPolicy coming up; the Pod waits on its certificate |
| `Running` | Everything up; `Ready: True` |
| `Idle` | No activity for `idleTimeout`; still running |
| `Hibernating` | Pod being torn down, PVC retained |
| `Hibernated` | No Pod; storage and identity parked |
| `Resuming` | Waking: Pod recreating after a wake trigger |
| `Degraded` | Running but missing something (a provider or tool grant revoked, deleted, or narrowed) |
| `Failed` | Crash-looping or unprovisionable |
| `Terminating` | Deletion in progress, finalizer running |

Conditions: `Ready` (the roll-up), `GatewayReachable` (the controller's view
of the gateway), and `Degraded`. Degraded reasons: `BudgetExhausted`
(present only while a referenced provider reports the namespace
budget-blocked; phase is preserved), `ClassConstraintViolation` (an image,
model provider, or tool grant no longer passes its class or allowlist
gates; the message names the failed gate), and `ToolNotInCatalog` (a
granted tool is outside the ToolProvider's declared catalog). A wake can
also be refused with event reason `WakeIgnored` (for
example, hibernation not in effect). Handler-mount problems surface as
Ready-condition reasons: `HandlerMountNotAllowed` (the class does not allow
mounts) and `HandlerConfigMapNotFound`.

## AgentTask

`kubectl get agenttasks` columns: `Phase`, `Class`, `Age`.

Phases: `Pending`, `Provisioning`, `Running`, `Completing` (result being
recorded), then one of `Succeeded`, `Failed`, `TimedOut`; `Terminating` on
delete. There is no Degraded: a task that cannot run fails, including a
task whose provider or tool grant fails a gate at provisioning (same
reasons as the Agent's Degraded, but terminal here).

Conditions: `Ready` (provisioning gate) and `Completed` (terminal verdict,
reason `TaskSucceeded` or `TaskFailed`). Completion-identity rejections
surface as `StalePodCompletion` (retryable by the task) and
`TaskAlreadyCompleted` (final).

## AgentChannel

`kubectl get agentchannels` columns: `Agent`, `Phase`, `Connected`, `Age`.

Phases: `Active`, `Degraded`, `Failed`, `Terminating`; the phase is unset
until the finalizer is installed. `Connected` shows the
`PlatformConnected` condition: the gateway's view of whether deliveries
reach the agent (reasons like `AgentReachable`, `WebhookReady`,
`NoRecentTraffic`, `AgentNotFound`).

Spec problems show as Ready-condition reasons: `InvalidPath`,
`PathConflict`, `InvalidCallbackURL`, `CallbackAuthMissing`,
`SystemNamespaceForbidden`, and for a platform channel (`type: discord`)
`CredentialsMissing` (a required key is absent from the credential Secret)
or `CredentialsInvalid` (the Discord public key is not a valid Ed25519 key).
`PlatformConnected=False` reasons name what failed most recently:
`WebhookAuthFailed` (signature or token), `AgentNotReady`, `DispatchFailed`,
`CallbackInvalid`, `CallbackRejected` (a callback receiver or a platform
refused the reply).

## ModelProvider

`kubectl get modelproviders` columns: `Type`, `Ready`, `Healthy`, `Age`.

- `Ready`: spec valid and credentials resolve. False reasons:
  `CredentialsMissing`, `CredentialsInvalid`, `InvalidDegradeTarget`,
  `FallbackIneligible`, `HardBudgetUnpriced` (hard enforcement requires a
  fully priced model catalog).
- `Healthy`: the periodic upstream probe (`UpstreamReachable` when good).
  Ready without Healthy means valid config, unreachable provider.
- `BoundaryMarginRaised` (hard enforcement only): observed traffic forced the
  gateway to admit more conservatively than the configured
  `boundaryMarginPercent`; a signal to raise the knob, not an outage.

Budget state lives in status:

```bash
kubectl get modelprovider <name> -o jsonpath='{.status.budgetUsage}' | jq
```

Each entry: namespace, period, `spentUSD`, `percentUsed`, and `state`
(`Normal` / `Throttled` / `Blocked`).

## ToolProvider

`kubectl get toolproviders` columns: `Type`, `Ready`, `Healthy`, `Age`.

- `Ready`: the credential Secret resolves in `kaalm-system`. False reasons:
  `CredentialsMissing`, `CredentialsInvalid` (the server rejected the
  injected credential).
- `Healthy`: the periodic probe, which speaks MCP (`initialize` then
  `tools/list`); `UpstreamReachable` when good, `ProviderUnhealthy` when
  not. As with ModelProvider, Ready without Healthy means valid config,
  unreachable server. The probe trusts system CA roots only; disable it for
  a server on a private CA.

## AgentClass

`kubectl get agentclasses` columns: `Agents`, `Tasks`, `Age`; the counts are
live usage, which is also your "is anyone still using this class" check
before deleting one.

Conditions: `Ready` and `FQDNPolicySupported` (whether the CNI supports the
FQDN egress rules the class asks for; reason `FQDNPolicyUnsupported` when
not).

## One-liners worth keeping

```bash
# Watch an agent come up or wake
kubectl get agents -w

# Everything Kaalm owns in a namespace
kubectl get agents,agenttasks,agentchannels -n <ns>

# The cluster-scoped set
kubectl get agentclasses,modelproviders,toolproviders

# Any agent locked out of its provider, cluster-wide
kubectl get agents -A | grep Degraded

# Why exactly is this resource not Ready
kubectl describe agent <name> | sed -n '/Conditions:/,/Events:/p'
```

---

*How this works: design book pages Controller, Agent Lifecycle (the phase
machine), Resources (each CRD page documents its full status shape), and
Operations, Observability (the metrics that complement these statuses).*
