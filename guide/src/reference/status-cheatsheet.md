# Status cheatsheet

Everything Kaalm tells you through `kubectl get` and `describe`, resource
by resource. Conditions listed are the ones the controller sets.

## Agent

`kubectl get agents` columns: `Phase`, `Ready`, `Class`, `Age`.

![Agent state machine. Pending to Provisioning on Certificate created, Provisioning to Running on Pod Ready, Running to Idle when idleTimeout elapses, Idle back to Running on activity observed, Idle to Hibernating when hibernationDelay elapses, Hibernating to Hibernated when the Pod is gone, Hibernated to Resuming on the wake annotation, and Resuming to Running on Pod Ready. Running and Idle return to Provisioning on spec drift or Pod disruption. From any phase: Degraded on a class mismatch, returning to that phase when the mismatch clears; Failed on a crash loop or image pull failure, returning to Provisioning when the Pod recovers or is replaced; Terminating when deleted.](../diagrams/agent-lifecycle.svg)

Phases, in lifecycle order:

| Phase | Meaning |
|---|---|
| `Pending` | Accepted, children not created |
| `Provisioning` | Pod, PVC, Service, Certificate, NetworkPolicy coming up; the Pod waits on its certificate |
| `Running` | Everything up; `Ready: True` |
| `Idle` | No activity for `idleTimeout`; still running |
| `Hibernating` | Pod being torn down, PVC retained |
| `Hibernated` | No Pod; storage and identity parked |
| `Resuming` | Waking: Pod recreating after a wake trigger, until it is Ready |
| `Degraded` | The Agent fails a class gate (image, provider or tool grant revoked, deleted, or narrowed, or a namespace the class no longer admits); the Pod keeps running |
| `Failed` | Crash-looping, or the image cannot be pulled |
| `Terminating` | Deletion in progress, finalizer running |

Conditions: `Ready` (the roll-up), `GatewayReachable` (the controller's view
of the gateway; reasons `GatewayReady` and `GatewayUnavailable`, the latter
deferring idle and hibernation transitions until the gateway answers;
present only while idle detection runs on a `Running` or `Idle` Agent, absent
otherwise),
`IdleDetection` (present only as `False` with `Disabled`, when neither the
Agent nor its class sets an idle timeout, so the Agent never goes `Idle` on
its own; absent otherwise),
`ProvidersReady` (`AllProvidersHealthy` when every referenced provider is
allowed, exists, and is `Ready`; `ClassConstraintViolation` when one is not
allowed or does not exist; `ProviderUnhealthy` when one is not `Ready`),
`Degraded`, which carries one reason, `BudgetExhausted`, present only while a
referenced provider reports the namespace budget-blocked, and `PodUpToDate`
(`True` with `Current` when the Pod matches the derived spec and mounts the
Certificate's TLS Secret ([Change propagation](https://github.com/win07xp/kaalm/blob/main/docs/src/controller/change-propagation.md#a-pod-that-mounts-another-tls-secret-is-replaced)); `False` with
`Replacing` while the Agent holds one of the class's `maxUnavailableOnDrift`
slots, `ReplacementPending` while it waits for one, or `CertificateNotReady`
while the replacement waits for the certificate; in the last two cases the current Pod
keeps running). None of `ProvidersReady`, `Degraded`, or `PodUpToDate`
changes the phase.

`Ready=False` reasons that move the phase to `Degraded` (the message names
the failed gate): `NamespaceNotAllowed` (the class sets `allowedNamespaces`
and none of its patterns matches the Agent's namespace; it is checked first,
so it is the reason shown when several gates fail), `ClassConstraintViolation` (an image, provider, or tool
grant does not pass its class or allowlist gate), `ToolNotInCatalog` (a
granted tool is outside the ToolProvider's declared catalog),
`PersistenceNotAllowed`, `HibernationNotAllowed`,
`HibernationRequiresPersistence`, and `HandlerMountNotAllowed` (the class
does not allow handler mounts).

`Ready=False` reasons that hold the Agent without degrading it:
`InvalidReference` (no image, the class does not exist, or the class has a
malformed `allowedCIDRs` entry),
`ImagePullSecretMissing`, `SecretNotOptedIn` (an env Secret is missing or
lacks the label `kaalm.io/workload-secret: "true"`; a running Pod stays in
place and the phase is kept), `ExistingClaimNotFound` (the adopted PVC is
missing), `HandlerConfigMapNotFound`, `CertificateNotReady` (cert-manager has
not issued the certificate yet; a running Pod keeps serving and keeps its
phase while new or replacement Pods wait; see
[AgentReconciler](https://github.com/win07xp/kaalm/blob/main/docs/src/controller/reconcilers/agent.md#certificate-wait-with-a-running-pod)),
`PodCreateRejected` (the apiserver refused the Pod create; the message says
why),
`ChildConflict` (an object the Agent does not own already has the name of
one of its children, such as its NetworkPolicy; the message names it, and
deleting it lets the Agent continue, unless an AgentTask of the same name owns
it: then delete one of the two workloads, and recreate it under another name
if you still need it, as
[Child resources](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/child-resources.md#an-agent-and-an-agenttask-cannot-share-a-name) explains), and
`SystemNamespaceForbidden` (an Agent in `kaalm-system` is never
provisioned). Reasons on `Ready` that report progress: `PodProvisioning`,
`PodRunning`, `PodDisrupted`, `SpecDrift`, `Hibernated`, and `Woken`; an
AgentTask retry also reports `PodTerminating` while the old Pod drains. A wake
annotation on an Agent that is not `Hibernated` or `Hibernating` is refused
with event reason `WakeIgnored`.

## AgentTask

`kubectl get agenttasks` columns: `Phase`, `Class`, `Age`.

Phases: `Pending`, `Provisioning`, `Running`, `Completing` (result being
recorded), then one of `Succeeded`, `Failed`, `TimedOut`; `Terminating` on
delete. There is no Degraded: a task that cannot run fails, including a
task whose provider or tool grant fails a gate, or whose namespace the
class does not admit, at provisioning (same reasons as the Agent's Degraded,
but terminal here).

Conditions: `Ready` (provisioning gate; `ChildConflict` holds a task
without a Pod, as it holds an Agent (an Agent of the same name can own the
conflicting object, with the same fix as above), and `PodCreateRejected` holds a task
whose Pod create the apiserver refused until the five-minute provisioning
deadline fails the attempt) and `Completed` (terminal verdict,
reason `TaskSucceeded`, `TaskFailed`, `TimeoutExceeded`, or
`TimeoutSucceeded`). A completion call from the wrong Pod is refused with
`409 stale_pod` (retryable by the task), and one against a finished task
with `403 access_denied` and the message prefix `TaskAlreadyCompleted`
(final); neither is a condition.

`Ready=False` with reason `SecretNotOptedIn` also holds a task without a Pod:
an env Secret is missing or lacks the label `kaalm.io/workload-secret: "true"`.
The task is not `Failed`, and the controller checks again every 30 seconds.
The fix is in
[Troubleshooting](troubleshooting.md#workload-is-readyfalse-with-secretnotoptedin).

## AgentChannel

`kubectl get agentchannels` columns: `Agent`, `Phase`, `Connected`, `Age`.

Phases: `Active` and `Degraded` mirror the bound Agent's phase; `Failed`
means the bound Agent does not exist or the channel is in `kaalm-system`;
`Terminating` is the delete path; the phase is unset until the first
reconcile. `Connected` shows the
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
(the `callbackAuth` Secret or key does not exist), `CallbackAuthInvalid`
(the `callbackAuth` key is empty), `SecretNotOptedIn` (a referenced Secret
lacks the label `kaalm.io/channel-credential: "true"`),
`CallbackHostNotApproved` (a Secret used as a `bearer` `callbackAuth` token
does not list the `callbackUrl` host in its `kaalm.io/callback-hosts`
annotation), and `ChildConflict` (a Role or RoleBinding with the name of one
of the channel's credential Roles exists and the channel does not own it; the
channel re-checks every 30 seconds, so removing that object clears the
conflict). The fixes for the two Secret reasons are in
[Troubleshooting](troubleshooting.md#channel-is-readyfalse-with-secretnotoptedin-or-callbackhostnotapproved).

A `callbackUrl` host that does not resolve leaves the channel `Ready=True`,
because the gateway checks the host again before every delivery. The
controller emits a `Warning` event with reason `CallbackHostUnresolved` that
names the host, so `kubectl describe agentchannel` shows the typo.

## ModelProvider

`kubectl get modelproviders` columns: `Type`, `Ready`, `Healthy`, `Age`.

- `Ready`: spec valid and credentials resolve. False reasons:
  `CredentialsMissing`, `SecretNotOptedIn` (the credential Secret lacks the
  label `kaalm.io/provider-credential: "true"`), `EndpointHostNotApproved`
  (the Secret's `kaalm.io/provider-hosts` annotation does not list the
  `spec.endpoint` host), `CredentialsInvalid`, `InvalidDegradeTarget`,
  `FallbackIneligible`, `InvalidModelMap`, `HardBudgetUnpriced` (hard
  enforcement requires a fully priced model catalog),
  `InvalidNamespacePattern` (an `allowedNamespaces` entry is not a valid
  glob), `DeletionBlocked`
  (a delete is held by a referrer; see [Deleting a provider, tool
  provider, or class never finishes](troubleshooting.md#deleting-a-provider-tool-provider-or-class-never-finishes)).
  Each of these reasons fires a `Warning` event the first time `Ready` turns
  `False` with it. The fixes for the two Secret reasons are in
  [Troubleshooting](troubleshooting.md#modelprovider-readyfalse).
- `Healthy`: the periodic upstream probe, run against every provider type
  (`UpstreamReachable` when good, `ProviderUnhealthy` when not, or
  `CredentialsInvalid` when the probe itself is refused). A probe that keeps
  failing is checked less often over time. Ready with Healthy `False`
  (`ProviderUnhealthy`) means valid config, unreachable provider. `Unknown`
  with `NotProbed` means the probe did not run: `Ready` is `False` for a
  Secret or configuration reason, `healthCheck.enabled` is `false`, or a
  delete is held. The message names which. When `Ready` is `False`, fix that first; `Healthy` updates when the
  probe next runs.
- `GatewayReachable`: mirrored onto every provider from the controller's
  view of the gateway Pods, and updated at once when a gateway Pod's readiness
  changes.
- `FallbackIneligible` (advisory, never affects `Ready`): the reconcile-time
  scan found a fallback candidate that a caller's namespace or model can
  never reach. Clears on its own once the configuration is fixed.
- `DegradeTargetNotCheapest` (advisory, never affects `Ready`): a degrade
  policy's `degradeTo` costs more than another priced model in the catalog
  (`CheaperModelAvailable`); `False` with `DegradeTargetCheapest` once none does.
- `MaxOutputTokensUnset` (advisory, never affects `Ready`): a fallback edge
  from `openai` or `openai-compatible` into `anthropic` reaches models with
  no `maxOutputTokens`, and the message lists them; `False` with
  `MaxOutputTokensDeclared` once none remain.
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
  resolves in `kaalm-system`; a provider with no credential and valid
  `allowedNamespaces` entries is Ready. False reasons: `CredentialsMissing`, `SecretNotOptedIn` and
  `EndpointHostNotApproved` (the Secret lacks the label
  `kaalm.io/provider-credential: "true"` or does not list the endpoint host
  in `kaalm.io/provider-hosts`; both apply only when `credentialsRef` is
  set, and the fixes are in
  [Troubleshooting](troubleshooting.md#modelprovider-readyfalse)),
  `InvalidNamespacePattern` (an `allowedNamespaces` entry is not a valid
  glob; it applies with or without `credentialsRef`),
  `CredentialsInvalid` (the server rejected
  the injected credential), `DeletionBlocked` (a delete is held by a
  referrer; see [Deleting a provider, tool provider, or class never
  finishes](troubleshooting.md#deleting-a-provider-tool-provider-or-class-never-finishes)).
  Each of these reasons fires a `Warning` event the first time `Ready` turns
  `False` with it.
- `Healthy`: the periodic probe, which speaks MCP (`server/discover` or
  `initialize`, then `tools/list`; the negotiated revision lands in
  `status.mcpRevision`); `UpstreamReachable` when good, `ProviderUnhealthy`
  when not, or `CredentialsInvalid` when the tool server rejects the probe's
  credential (`Ready` turns `False` with it, and the fix is the credential
  Secret, not the server). As with ModelProvider, Ready with Healthy `False`
  (`ProviderUnhealthy`) means valid config, unreachable server. `Unknown` with
  `NotProbed`, as for ModelProvider. The probe trusts the same CAs as the
  gateway's upstream path; see [Providing LLM access](../platform/llm-access.md#4-trust-a-private-ca).

## AgentClass

`kubectl get agentclasses` columns: `Agents`, `Tasks`, `Replacing`, `Age`;
the counts are live usage, which is also your "is anyone still using this
class" check before deleting one. `Replacing` counts the class's Agents
holding a `maxUnavailableOnDrift` slot; `status.agentsPendingReplacement`
counts the rest waiting for one, but has no column of its own.

Conditions: `Ready` (`AllReferencesResolved`; `InvalidCIDR` when an
`allowedCIDRs` entry does not parse; `InvalidNamespacePattern` when an
`allowedNamespaces` entry is not a valid glob (it ranks after `InvalidCIDR`
and before `InvalidReference`); `InvalidImagePattern` when an `allowedImages`
entry is not a valid glob (it ranks after `InvalidNamespacePattern` and before
`InvalidReference`); `InvalidReference` when a
listed provider or tool provider does not exist or an `allowedHosts` entry is
not a DNS name; `DeletionBlocked` while a delete is held by a referrer, see
[Deleting a provider, tool provider, or class never
finishes](troubleshooting.md#deleting-a-provider-tool-provider-or-class-never-finishes)), `SecurityBaseline` (`RestrictedBaseline`, or
`BelowRestrictedBaseline` naming each `security` field the class relaxes
below the restricted Pod Security Standard), `DeprecatedFields` (advisory, present
only once the class sets a deprecated field: `DeprecatedFieldSet` naming each
one, or `NoDeprecatedFields` after it is removed; it never affects `Ready`), and `FQDNPolicySupported`
(whether the CNI supports the FQDN egress rules the class asks for; reason
`NoHostsRequested` when the class names no `allowedHosts`,
`FQDNPolicySupported` when it does and the CNI can carry them out,
`FQDNPolicyUnsupported` when it cannot and the hosts are ignored). On Cilium,
each agent and task of the class then has a CiliumNetworkPolicy named
`NAME-fqdn` that allows those hosts. A fresh class shows empty `AGENTS` and `TASKS`
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
machine), Resources (each CRD page documents its full status), Controller,
Errors, events, and testing (the Event reasons and when they fire), and
Operations, Observability (the metrics that complement these statuses).*
