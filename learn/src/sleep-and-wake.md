# Sleep and wake

This is the chapter where Kaalm stops looking like a fancy way to run a
container.

Your agent is a running program. It holds memory whether or not anybody is
talking to it. Multiply that by a few hundred agents, most of them idle most of
the time, and you are paying continuously for capacity nobody is using.

Kaalm's answer: when an agent goes quiet, shut it down. Keep its identity, keep
its storage, keep its address. Bring it back when a message arrives.

## What survived

You have not messaged the agent since "Make it yours". With the class's
30-second timers, that quiet was long enough for it to fall asleep. The
program is gone:

```bash
kubectl get pods -l kaalm.io/agent=helper
```

```
No resources found in default namespace.
```

Nothing is running. No memory, no CPU. But the agent still exists:

```bash
kubectl get agents
```

```
NAME     PHASE        READY   CLASS      AGE
helper   Hibernated   False   tutorial   2m31s
```

And so does its storage:

```bash
kubectl get pvc
```

```
NAME            STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   VOLUMEATTRIBUTESCLASS   AGE
helper-memory   Bound    pvc-cb87ed98-0038-42db-b92f-f2b6d5ccdec0   1Gi        RWO            local-path     <unset>                 2m30s
```

That is hibernation in one screen: the expensive part (a running program) is
gone, and the parts that make it *this* agent rather than a fresh one (its
name, its identity, its disk) are all still here.

## Wake it up

Say hello to a sleeping agent. Same command as before, nothing special about
it:

```bash
curl -sk -X POST https://127.0.0.1:18080/channels/default/helper-webhook \
  -H "Authorization: Bearer tutorial-secret-token" \
  -H "Content-Type: application/json" \
  -d '{"text":"are you awake"}'
```

```json
{"content": "helper here: are you awake (message 3 from you)"}
```

Read that reply again. **Message 3.**

The program that counted messages one and two was destroyed. This is a
different program, in a different container, started seconds ago because your
message arrived. It picked up the count from the volume Kaalm reattached, and
carried on.

On the walk that produced this book, the reply took about 3 seconds to come
back. Your caller waited; nothing was lost; the agent answered. While it
waited, the agent's phase was `Resuming` with `READY` False, and it turned
`Running` when the new container passed its readiness check.

> **One thing the walk hid.** A `sync` channel gives the caller 30 seconds,
> and a cold wake on a real cluster can take longer than that: pulling the
> image, attaching the disk, issuing nothing new but waiting on more. On this
> laptop the wake won that race with time to spare. For a channel in front of
> an agent that hibernates, use `responseMode: async`: the caller gets an
> immediate acknowledgement and the reply arrives when the agent is up. The
> guide's [Troubleshooting](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md) page
> has the symptom and the fix.

```bash
kubectl get agents
```

```
NAME     PHASE     READY   CLASS      AGE
helper   Running   True    tutorial   2m41s
```

Awake again.

## Watch it fall asleep again

The timers are 30 seconds each, set back in the class. On the walk, the agent
went idle about 44 seconds after the reply and was asleep about 75 seconds
after it. Send nothing more and watch:

```bash
kubectl get agents -w
```

`-w` keeps the command running and prints a new line each time the agent
changes. It moves from `Running` to `Idle` once the idle timeout passes,
holds there for the 30-second hibernation delay, passes through `Hibernating`
in a second or two, and settles at `Hibernated`. Press Ctrl-C when it settles:

```
NAME     PHASE     READY   CLASS      AGE
helper   Running   True    tutorial   2m41s
helper   Idle      True    tutorial   3m25s
helper   Hibernating   True    tutorial   3m55s
helper   Hibernating   True    tutorial   3m55s
helper   Hibernated    False   tutorial   3m56s
```

The columns shift on the later rows because `-w` prints each change as it
arrives instead of re-aligning the table. It prints a line for every change
to the agent, so a phase can appear twice.

> **What counts as activity?** Kaalm watches the traffic an agent sends and
> receives through the gateway: the messages delivered to it and the model
> calls it makes. Every message you sent reset the idle clock, and the agent
> stayed awake while you were talking to it. Stop sending messages and the
> clock runs out: on the walk, with the class's 30-second timers, the agent
> was asleep about 75 seconds after the last reply. The message that wakes
> it counts too, so a woken agent always gets a fresh 30 seconds before it can
> go idle again.

## Why this matters

Consider a hundred agents, each used for a few minutes a day. Without
hibernation you pay for a hundred running programs around the clock. With it
you pay for the handful that happen to be in a conversation right now, and the
rest cost you nothing but disk.

The hard part is what you watched Kaalm handle: an agent that is shut down
and restarted must not lose the thread. That is why storage is declared in the
agent's own spec, and why the count kept going. Anything your agent
keeps on that volume survives; anything it holds only in memory does not.

Next: [Give it a real brain](give-it-a-real-brain.md).
