# Agent lifecycle day to day

This page describes the lifecycle as you observe it: what the phases mean in
`kubectl get agents -w`, what hibernation looks like, and what deletion
actually tears down.

## The phase walk

A healthy persistent agent moves through `Pending`, `Provisioning`, and
`Running`. It goes `Idle` after `idleTimeout` with no activity, then through
`Hibernating` to `Hibernated` after `hibernationDelay` elapses. It comes back
through `Resuming` and `Provisioning` to `Running` when a message arrives or
you wake it. The full status vocabulary is on the
[Status cheatsheet](../reference/status-cheatsheet.md).

![Agent state machine. Pending to Provisioning on Certificate created, Provisioning to Running on Pod Ready, Running to Idle when idleTimeout elapses, Idle back to Running on activity observed, Idle to Hibernating when hibernationDelay elapses, Hibernating to Hibernated when the Pod is gone, Hibernated to Resuming on the wake annotation, and Resuming to Provisioning when the Pod is created. Running and Idle return to Provisioning on spec drift or Pod disruption. From any phase: Degraded on a class mismatch, returning to that phase when the mismatch clears; Failed on a crash loop or image pull failure, returning to Provisioning when the Pod recovers or is replaced; Terminating when deleted.](../diagrams/agent-lifecycle.svg)

`Degraded` means the Agent's spec does not match its class, or a provider
or tool grant it names has been revoked; the Pod keeps running and the phase
is restored when the mismatch clears. Budget exhaustion is not a phase: it
sets a `Degraded` condition and leaves the phase alone. `Failed` is a crash
loop or an image that cannot be pulled, and `Terminating` is deletion in
progress.

## Hibernation, observed

Requirements: the class must allow it (`hibernationAllowed`), the agent must
enable it, and the agent needs persistence; hibernation deletes the Pod, so
without a PVC there would be nothing left to wake. What counts as activity is
set by `spec.lifecycle.activitySource`. After `idleTimeout` without activity
the agent goes `Idle`; after a further `hibernationDelay` the controller
deletes the Pod, keeps the PVC and identity, and parks the agent at
`Hibernated`. A hibernated agent costs storage, not compute. The design book's
Hibernation and wake page states both timers and their class ceilings.

## Waking

Any message through the agent's channel wakes it: the gateway holds the
message, triggers the wake, polls the agent's Service until it accepts a
connection (`Resuming`, then `Provisioning`, then `Running`), then delivers. Conversation memory is intact because it lives on the PVC.

The timing caveat that matters: under default settings, a sync-mode channel
times out (`504 sync_deadline_exceeded` at 30 seconds) before a cold wake
completes (`wakeTimeout` 120 seconds). Give hibernation-backed channels
`responseMode: async`: the caller gets its `202` at once and the reply
arrives by callback or polling after the agent is up. If the wake itself
exceeds `wakeTimeout`, async callers receive a `wake_timeout` error payload
instead of silence.

If the Agent is `Hibernated`, you can wake it by hand, the same way the
gateway does:

```bash
kubectl annotate agent support-assistant kaalm.io/wake=true --overwrite
```

In any other phase the annotation is removed and a `WakeIgnored` event
records it.

## Promoting a task to a persistent agent

When a finished task's sandbox should be kept (a human wants to inspect or
take over), the pattern uses standard Kubernetes primitives, before the
task's `ttlSecondsAfterFinished` cleans up its PVC:

1. Snapshot the task's PVC (`VolumeSnapshot`).
2. Create a PVC from the snapshot.
3. Create a persistent Agent from the same image with
   `spec.persistence.existingClaim` pointing at the new PVC.

An adopted claim is never owned by the Agent, so it survives the Agent's
deletion under either `pvcRetention` setting.

## Deletion

On `kubectl delete agent`, the finalizer marks the Agent `Terminating`,
deletes the Pod, and waits for it to go; the kubelet sends SIGTERM and the
class's grace period applies, and finishing in-flight work in that window is
the image's job. Only then is the resource released. The PVC's fate is the
class's `pvcRetention` policy: `Delete`, the default, removes it with the
agent; `Retain` (the sample class's choice) keeps it for a successor agent or
post-mortem. The chart's `standard` class enables no persistence at all. This
policy is Kaalm's own and is independent of the PV reclaim policy underneath.

---

*How this works: design book pages Controller, Agent lifecycle (the state
machine and every timer), Gateways, User, Activation and activity tracking (the wake
sequence, drawn step by step), and Resources, Agent (the lifecycle spec
fields and their class-level bounds).*
