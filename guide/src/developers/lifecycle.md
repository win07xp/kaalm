# Agent Lifecycle Day-to-Day

This page describes the lifecycle as you observe it: what the phases mean in
`kubectl get agents -w`, what hibernation looks like, and what deletion
actually tears down.

## The phase walk

A healthy persistent agent moves through:

```
Pending -> Provisioning -> Running
                             |  (idleTimeout with no activity)
                             v
                           Idle
                             |  (hibernationDelay elapses)
                             v
                        Hibernating -> Hibernated
                             |  (a message arrives, or you wake it)
                             v
                         Resuming -> Running
```

`Degraded` is a side state (provider revoked, budget exhausted: the agent
runs but something it needs is missing), `Failed` is a crash-loop verdict,
and `Terminating` is deletion in progress.

## Hibernation, observed

Requirements: the class must allow it (`hibernationAllowed`), the agent must
enable it, and the agent needs persistence; hibernation deletes the Pod, so
without a PVC there would be nothing left to wake.

What "idle" means is set by `spec.lifecycle.activitySource`: with
`gatewayTraffic`, LLM calls and channel deliveries count as activity; agent
heartbeats are the other signal. After `idleTimeout` without activity the
agent goes `Idle`; after a further `hibernationDelay` the controller deletes
the Pod (keeping the PVC and identity) and parks the agent at `Hibernated`.
A hibernated agent costs storage, not compute.

## Waking

Any message through the agent's channel wakes it: the gateway holds the
message, triggers the wake, waits for the Pod to become Ready (`Resuming`),
then delivers. Conversation memory is intact because it lives on the PVC.

The timing caveat that matters: under default settings, a sync-mode channel
times out (`504 sync_deadline_exceeded` at 30 seconds) before a cold wake
completes (`wakeTimeout` 120 seconds). **Give hibernation-backed channels
`responseMode: async`**; the caller gets its `202` instantly and the reply
arrives via callback or polling once the agent is up. If the wake itself
exceeds `wakeTimeout`, async callers receive a `wake_timeout` error payload
instead of silence.

You can also wake an agent by hand, the same way the gateway does:

```bash
kubectl annotate agent support-assistant agentry.io/wake=true
```

## Promoting a task to a persistent agent

When a finished task's sandbox is worth keeping (a human wants to inspect or
take over), the pattern uses standard Kubernetes primitives, before the
task's `ttlSecondsAfterFinished` cleans up its PVC:

1. Snapshot the task's PVC (`VolumeSnapshot`).
2. Create a PVC from the snapshot.
3. Create a persistent Agent from the same image with
   `spec.persistence.existingClaim` pointing at the new PVC.

## Deletion

`kubectl delete agent` drains in-flight requests, stops the Pod with SIGTERM,
runs the finalizer, and only then releases the resource. The PVC's fate is
the class's `pvcRetention` policy: `Retain` (the sample class's choice) keeps
it for a successor agent or post-mortem; `Delete` removes it with the agent.
This policy is Agentry's own and is independent of the PV reclaim policy
underneath.

---

*How this works: design book pages Controller, Agent Lifecycle (the state
machine and every timer), Gateways, User, Activation and Activity (the wake
sequence, drawn step by step), and Resources, Agent (the lifecycle spec
fields and their class-level bounds).*
