# AgentChannelReconciler

## What it's for

The AgentChannelReconciler decides whether an [AgentChannel](../../resources/agentchannel.md) can serve traffic; the gateway routes only `Ready=True` channels. It also gives the gateway read access to the channel's labeled credential Secrets, reports delivery health and the bound Agent's phase, and prunes the channel's async records. It creates no Pods. On delete it runs the [handshake](../finalizers.md#agentchannel) with the gateway: announce `Terminating`, wait for the disconnect, sweep every async record, release the finalizer. The gateway's side is under [Request flow](../../gateways/user/overview.md#request-flow).

## What it owns and watches

The reconciler creates two Roles, with their RoleBindings, in the channel's namespace, each with a controller ownerRef to the channel.

The figure shows what the reconciler creates for a channel and what it reads and polls.

![An AgentChannel owns a check Role and a credential Role, bound to the ServiceAccounts shown, that grant access to its Secrets. The channel reads its Agent and the channels on its path, has its health polled from the gateway Pods, and has its expired async ConfigMaps deleted by label.](../../diagrams/agentchannel-objects.svg)

Bold lines are ownership, blue lines are Role grants, grey dashed lines are reads, and the red line matches by labels. What re-runs a channel is listed under [What each reconciler watches](../reconcilers.md#what-each-reconciler-watches).

### Per-channel credential Roles

The check Role lets the reconciler read the label on every Secret the channel names. The credential Role lists only the labeled Secrets, and the gateway reads through it. [Operator ServiceAccount](../../security/rbac.md#operator-serviceaccount) names both Roles and their bindings. The reconciler rebuilds the credential Role from what it read, so the Role stops granting a Secret that lost its label or that the channel no longer references, from the next pass that reaches the Role check.

A Role or RoleBinding of one of these names that the channel does not control is never written; see [Child ownership](agent.md#child-ownership).

### Channel health poll

The reconciler fans `GET /v1/channels/health?namespace={ns}` out to every gateway Pod because each replica reports only the deliveries it handled, and skips unreachable replicas. The endpoint is specified under [GET /v1/channels/health](../../gateways/api/internal-endpoints.md#get-v1channelshealth), and the four-rule reduction into `PlatformConnected` under [How the controller reduces it](../../gateways/user/platform-adapters.md#how-the-controller-reduces-it).

### Async ConfigMap pruning

The reconciler deletes the `kaalm-async-*` ConfigMaps in `kaalm-system` that carry this channel's labels (`kaalm.io/channel-namespace`, `kaalm.io/channel-name`) and whose `kaalm.io/expires-at` annotation is in the past. The 1-hour TTL is enforced here because a cross-namespace ownerRef cannot express the linkage; see [Response persistence](../../gateways/api/async-responses.md#response-persistence).

## What it checks

The first failing check in the following table sets `Ready=False` with its reason code, and the later checks do not run.

| Check | Reason when it fails | Rule |
|---|---|---|
| The channel is not in the operator namespace | `SystemNamespaceForbidden` | [28](../../resources/validation/names-and-tasks.md) |
| `agentRef` names an Agent in the channel's namespace | `AgentNotFound` | [13](../../resources/validation/references-and-access.md) |
| The Agent does not set `spec.service.enabled: false` | `AgentServiceDisabled` | [14](../../resources/validation/channels.md) |
| The path for the channel's type begins with `/channels/{namespace}/`, and no older channel in the namespace registers it | `InvalidPath`, `PathConflict` | [15](../../resources/validation/channels.md) |
| The reconciler can write the channel's Roles and RoleBindings | `ChildConflict`, or `InvalidReference` on a failed write | |
| Each referenced Secret can be read, carries the opt-in label, lists the `callbackUrl` host (bearer `callbackAuth`), and has its keys | `CredentialsMissing`, `SecretNotOptedIn`, `CallbackHostNotApproved`, `CallbackAuthMissing`, `CallbackAuthInvalid`, `CredentialsInvalid` | [25](../../resources/validation/channels.md), [40](../../resources/validation/channels.md), [45](../../resources/validation/channels.md), [46](../../resources/validation/channels.md) |
| A `callbackUrl`, when set, passes the callback policy | `InvalidCallbackUrl` | [22](../../resources/validation/channels.md) |

Rules 16 and 39 are apply-time CRD CEL checks, so they are not here. For each Secret, the label and host checks come before the key checks, so status says nothing about the keys of a Secret that has not opted in.

A channel that fails an earlier check neither creates nor changes its Roles.

## What it reports

- **`status.phase`** is `Active`, `Degraded`, `Failed`, or `Terminating`, and is updated even when validation fails; see [Channel phase reduction](#channel-phase-reduction).
- **`Ready`** is `True` with `reason: AgentReachable` when every check passes, and `False` with the reason from [What it checks](#what-it-checks) otherwise.
- **`PlatformConnected`** is `True` with `WebhookReady`, `False` with the newest failure's reason, or `Unknown` with `NoRecentTraffic`, as listed under [The PlatformConnected tri-state](../../resources/agentchannel.md#the-platformconnected-tri-state). It keeps its value when no gateway replica answers.
- **Events.** Every `Ready=False` reason raises a Warning event with the same reason, once when it first appears on `Ready`. A `callbackUrl` host that does not resolve raises a `CallbackHostUnresolved` Warning event and the channel stays `Ready=True` ([rule 22](../../resources/validation/channels.md)).

### Channel phase reduction

The phase is a memoryless reduction of the bound Agent's phase, recomputed on every pass, and separate from `Ready` and `PlatformConnected`. The gateway routes on `Ready`, so the phase exists for `kubectl describe`. The figure shows each Agent change that moves it.

![AgentChannel phase state machine, one trigger per edge. The initial pseudo-state enters Active when the Agent resolves and Failed when the Agent is not found. Active moves to Degraded when the Agent is Failed or Degraded, and Degraded back to Active when the Agent recovers. Active and Degraded move to Failed when the Agent is deleted, and Failed returns to Active when the Agent is recreated. Any phase moves to Terminating on deletion.](../../diagrams/agentchannel-phase-reduction.svg)

| Channel phase | When |
|---|---|
| `Failed` | `agentRef` does not resolve, or the channel is in the operator namespace. Recovers when the Agent is created. |
| `Degraded` | the Agent is `Failed` or `Degraded` |
| `Active` | every other Agent phase, including `Pending`, `Hibernated`, and unset. Transient unavailability surfaces through `PlatformConnected`, not the phase. |
| `Terminating` | set by the delete path before the [delete handshake](../finalizers.md#agentchannel) |

## Timing

- **A change to a Secret the channel references.** Creating or deleting the Secret, adding or removing the `kaalm.io/channel-credential` label, editing the `kaalm.io/callback-hosts` annotation, or changing a key re-runs the channel at once, so a fix needs no channel edit and no wait. The operator already watches each Secret it has read for the channel. A channel that fails an earlier check (Agent, service, path, or Role write) has read no Secret yet, so it picks up Secret changes on the pass after that check passes.
- **A failing channel.** One that fails a check after its Agent is found re-checks every minute. This is the fallback for a Secret change the watch has not reported, such as one made while the Secret's watch is still starting. A `ChildConflict` channel re-checks every 30 seconds, because removing the conflicting object raises no event. `AgentNotFound` runs again when the Agent is created, and `SystemNamespaceForbidden` has no timed re-check.
- **A change to the bound Agent.** It shows at once.
- **A path conflict.** It resolves as soon as a competing channel is created, deleted, or moves to another path. A `Ready=True` channel turns `PathConflict` at once when a new or moved channel wins the path (it wins a `creationTimestamp` tie by name under [rule 15](../../resources/validation/channels.md), or the clock went back), and the next turns `Ready` when the winner leaves. A deleted channel holds its path until the object leaves the API server.
- **Health and pruning.** Both run only on a valid pass, so `PlatformConnected` is re-evaluated at least once a minute on a valid channel, from gateway data up to 15 seconds old. On a valid channel an expired async record lingers at most one minute; a channel that stays invalid keeps its expired records until it is valid again or deleted.

## Design choices

- **Two Roles per channel.** RBAC cannot grant a read by label, and neither the controller nor the gateway has a blanket Secret read ([Operator ServiceAccount](../../security/rbac.md#operator-serviceaccount)).
- **The gateway re-checks the label on every read**, so a Secret that loses its label stops serving before the Role shrinks ([Dynamic per-namespace grants](../../security/rbac.md#dynamic-per-namespace-grants-channel-credentials)).
- **`agentRef` names an Agent only.** A task has no stable Service.
