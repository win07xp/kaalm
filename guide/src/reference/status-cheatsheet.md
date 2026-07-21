# Status Cheatsheet

Everything Agentry tells you through `kubectl get` and `describe`, resource
by resource. Conditions listed are the ones the controller actually sets in
v0.1.0.

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
| `Degraded` | Running but missing something (provider revoked or deleted) |
| `Failed` | Crash-looping or unprovisionable |
| `Terminating` | Deletion in progress, finalizer running |

Conditions: `Ready` (the roll-up) and `GatewayReachable` (the controller's
view of the gateway). A wake can also be refused with event reason
`WakeIgnored` (for example, hibernation not in effect).

## AgentTask

`kubectl get agenttasks` columns: `Phase`, `Class`, `Age`.

Phases: `Pending`, `Provisioning`, `Running`, `Completing` (result being
recorded), then one of `Succeeded`, `Failed`, `TimedOut`; `Terminating` on
delete. There is no Degraded: a task that cannot run fails.

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
`SystemNamespaceForbidden`.

## ModelProvider

`kubectl get modelproviders` columns: `Type`, `Ready`, `Healthy`, `Age`.

- `Ready`: spec valid and credentials resolve. False reasons:
  `CredentialsMissing`, `CredentialsInvalid`, `InvalidDegradeTarget`,
  `FallbackIneligible`.
- `Healthy`: the periodic upstream probe (`UpstreamReachable` when good).
  Ready without Healthy means valid config, unreachable provider.

Budget state lives in status:

```bash
kubectl get modelprovider <name> -o jsonpath='{.status.budgetUsage}' | jq
```

Each entry: namespace, period, `spentUSD`, `percentUsed`, and `state`
(`Normal` / `Throttled` / `Blocked`).

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

# Everything Agentry owns in a namespace
kubectl get agents,agenttasks,agentchannels -n <ns>

# The cluster-scoped pair
kubectl get agentclasses,modelproviders

# Any agent locked out of its provider, cluster-wide
kubectl get agents -A | grep Degraded

# Why exactly is this resource not Ready
kubectl describe agent <name> | sed -n '/Conditions:/,/Events:/p'
```

---

*How this works: design book pages Controller, Agent Lifecycle (the phase
machine), Resources (each CRD page documents its full status shape), and
Operations, Observability (the metrics that complement these statuses).*
