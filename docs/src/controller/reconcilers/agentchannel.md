# AgentChannelReconciler

This page specifies the AgentChannelReconciler: what one pass does, and its credential Roles, health poll, phase reduction, and pruning of async records. What it watches is in [Reconcilers](../reconcilers.md#what-each-reconciler-watches).

The reconciler creates no Pods. It validates the channel, scopes credential access, reduces the gateway's health and the bound Agent's phase into status, and prunes the channel's async records. One pass:

1. **Guard.** A channel in the operator namespace sets `status.phase: Failed` and `Ready=False, reason=SystemNamespaceForbidden`, and the pass ends.
2. **Agent.** Resolve `agentRef` in the channel's namespace; it must name an Agent, not an AgentTask, since tasks have no stable Service. A missing Agent sets `status.phase: Failed` and `Ready=False, reason=AgentNotFound`, and the pass ends.
3. **Validation.** The first failing check sets `Ready=False` with its reason:
   1. The Agent must have `spec.service.enabled: true` (`AgentServiceDisabled`).
   2. The path for the channel's type must be set and begin with `/channels/{namespace}/` (`InvalidPath`, [rule 15](../../resources/validation/channels.md)).
   3. No older channel in the namespace may register the same path (`PathConflict`, rule 15).
   4. Every referenced Secret must be usable. The reconciler ensures the controller-only check Role, reads each referenced Secret, and ensures the gateway's credential Role with the labeled Secrets only ([Per-channel credential Roles](#per-channel-credential-roles)). A reference then fails with: a failed read (`CredentialsMissing`, or `CallbackAuthMissing` for the callback reference); a missing opt-in label (`SecretNotOptedIn`, [rule 45](../../resources/validation/channels.md)); for a bearer `callbackAuth`, a Secret that does not list the `callbackUrl` host (`CallbackHostNotApproved`, rule 46); or a bad key: an inbound or platform key that is missing or empty (`CredentialsMissing`), a `callbackAuth` key that is missing (`CallbackAuthMissing`) or empty (`CallbackAuthInvalid`), or a Discord `publicKey` that does not decode to 32 bytes of hex (`CredentialsInvalid`). The label and host checks come before the key checks, so status says nothing about the keys of a Secret that has not opted in.
   5. A `callbackUrl`, when set, must pass the callback policy of rule 22 (`InvalidCallbackUrl`). A host that does not resolve is not a failure, because a DNS outage must not take a working channel down: the channel stays valid and the reconciler emits a `CallbackHostUnresolved` Warning event naming the host, once per failing host rather than on every pass.

   A failed validation still reduces the phase and requeues in one minute, or in 30 seconds after a `ChildConflict`.
4. **Ready.** Set `Ready=True, reason=AgentReachable`.
5. **Channel health.** Reduce the gateway replicas' health reports into `status.conditions[type=PlatformConnected]`; see [Channel health poll](#channel-health-poll).
6. **Phase.** Reduce the Agent's phase into `status.phase`; see [Channel phase reduction](#channel-phase-reduction).
7. **Prune.** Delete this channel's expired async records; see [Async ConfigMap pruning](#async-configmap-pruning).

A pass that reaches validation requeues in one minute (30 seconds after a `ChildConflict`). Secret changes do not enqueue a channel, so a credential fixed in place, and a label added to or removed from a Secret, shows at the next pass, or at once when the channel is edited. The gateway's side of a channel, from intake to delivery, is under [Request flow](../../gateways/user/overview.md#request-flow).

A path conflict resolves as soon as a competing channel is created, deleted, or moves to another path, not only on the one-minute pass. A deleted channel leaves the conflict check when the object leaves the API server, after the finalizer releases ([Finalizers](../finalizers.md#agentchannel)), so a `Terminating` channel still holds its path until then. When a new or moved channel takes the path (it wins a `creationTimestamp` tie by name under [rule 15](../../resources/validation/channels.md), or the clock went back), the channel that was `Ready=True` turns `Ready=False, reason=PathConflict` on the next pass, without waiting for its requeue. When the winner is deleted or moves, the loser becomes `Ready=True` on the pass that event triggers.

## Per-channel credential Roles

Each channel has a controller-only check Role, which lets the reconciler see the label on every Secret the channel names, and a gateway credential Role, which lists only the Secrets that carry it. [Operator ServiceAccount](../../security/rbac.md#operator-serviceaccount) names both Roles and says why they exist.

The reconciler builds the credential Role from the Secrets it read through the check Role, and updates it when the labeled set changes, so the Role stops granting a Secret that lost its label or that the channel no longer references. A channel that fails a check re-checks every minute. The gateway refuses an unlabeled Secret on every read, whether or not the Role has shrunk yet, so a Secret that loses its label stops serving the channel before the next pass ([Dynamic per-namespace grants](../../security/rbac.md#dynamic-per-namespace-grants-channel-credentials)).

A Role or RoleBinding with one of these names that the channel does not control is never written: the channel reports `Ready=False, reason=ChildConflict` and re-checks every 30 seconds, as described under [Child ownership](agent.md#child-ownership). A failure to write either Role reports `InvalidReference`. The Secret checks are the runtime half of [rule 25](../../resources/validation/channels.md), rules 45 and 46, and the platform key sets of rule 40; a `callbackAuth` block that names no Secret for its type reports `CallbackAuthInvalid`.

## Channel health poll

The reconciler fans `GET /v1/channels/health?namespace={ns}` out to every gateway Pod over mTLS with the controller's client certificate, because each replica reports only the deliveries it handled, and skips unreachable replicas. The result is cached per namespace, so the health data a pass reads can be up to 15 seconds old. The endpoint is specified under [GET /v1/channels/health](../../gateways/api/internal-endpoints.md#get-v1channelshealth), and the four-rule reduction into `PlatformConnected` under [How the controller reduces it](../../gateways/user/platform-adapters.md#how-the-controller-reduces-it).

## Channel phase reduction

`Active` and `Degraded` are a memoryless reduction of the bound Agent's phase, recomputed on every pass; `Failed` means `agentRef` does not resolve, and `Terminating` is set once by the delete path. The phase is one of three separate axes: `Ready` reports validation, `PlatformConnected` reports delivery health, and the phase reports the Agent.

![AgentChannel phase state machine, one trigger per edge. The initial pseudo-state enters Active when the Agent resolves and Failed when the Agent is not found. Active moves to Degraded when the Agent is Failed or Degraded, and Degraded back to Active when the Agent recovers. Active and Degraded move to Failed when the Agent is deleted, and Failed returns to Active when the Agent is recreated. Any phase moves to Terminating on deletion.](../../diagrams/agentchannel-phase-reduction.svg)

| Channel phase | When |
|---|---|
| `Failed` | `agentRef` does not resolve. Recovers when the Agent is created. |
| `Degraded` | the Agent is `Failed` or `Degraded` |
| `Active` | every other Agent phase, including `Pending`, `Provisioning`, `Hibernating`, `Hibernated`, `Resuming`, `Terminating`, and an Agent whose phase is unset. Transient unavailability surfaces through `PlatformConnected`, not the phase. |
| `Terminating` | set by this reconciler's delete path before the [delete handshake](../finalizers.md#agentchannel) |

The gateway gates routing admission on `Ready` and observes Agent availability through delivery outcomes; the phase exists for `kubectl describe`.

## Async ConfigMap pruning

The reconciler deletes the `kaalm-async-*` ConfigMaps in `kaalm-system` that carry this channel's labels (`kaalm.io/channel-namespace`, `kaalm.io/channel-name`) and whose `kaalm.io/expires-at` annotation is in the past. The 1-hour TTL is enforced here because a cross-namespace ownerRef cannot express the linkage; see [Response persistence](../../gateways/api/async-responses.md#response-persistence). Pruning runs on every pass, so a record lingers at most one requeue interval past its expiry. The delete-time finalizer sweeps every record of the channel, expired or not; see [Finalizers](../finalizers.md#agentchannel).
