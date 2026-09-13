# Sleep and wake

This is the chapter where Kaalm stops looking like a fancy way to run a
container.

Your agent is a running program. It holds memory whether or not anybody is
talking to it. Multiply that by a few hundred agents, most of them idle most of
the time, and you are paying continuously for capacity nobody is using.

Kaalm's answer: when an agent goes quiet, shut it down. Keep its identity, keep
its storage, keep its address. Bring it back when a message arrives.

## Watch it fall asleep

You set the timers to 30 seconds each back in the class, so this takes about a
minute. Stop sending messages and watch:

```bash
kubectl get agents -w
```

`-w` keeps the command running and prints a new line each time the agent
changes. It moves from `Running` to `Idle` after 30 seconds of quiet, holds
there for the 30-second hibernation delay, passes through `Hibernating` in a
second or two, and settles at `Hibernated`. Press Ctrl-C when it settles:

```
NAME     PHASE        READY   CLASS      AGE
helper   Hibernated   False   tutorial   3m18s
```

> **What counts as activity?** Kaalm watches the traffic an agent sends and
> receives through the gateway: the messages delivered to it and the model
> calls it makes. Every message you sent reset the idle clock, and the agent
> stayed awake while you were talking to it. Stop sending messages and the
> clock runs out: with the class's 30-second timers, the agent is asleep about
> a minute after your last message. The message that wakes it counts too, so
> a woken agent always gets a fresh 30 seconds before it can go idle again.

## What survived

The program is gone:

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
helper   Hibernated   False   tutorial   3m18s
```

And so does its storage:

```bash
kubectl get pvc
```

```
NAME            STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   VOLUMEATTRIBUTESCLASS   AGE
helper-memory   Bound    pvc-5babc3ae-fbec-4fa9-856d-1e6ee45113e5   1Gi        RWO            local-path     <unset>                 3m17s
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

On the walk that produced this book, the whole thing took two seconds. Your
caller waited; nothing was lost; the agent answered.

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
helper   Running   True    tutorial   3m26s
```

Awake again, ready to go quiet and repeat the cycle.

## Why this matters

Consider a hundred agents, each used for a few minutes a day. Without
hibernation you pay for a hundred running programs around the clock. With it
you pay for the handful that happen to be in a conversation right now, and the
rest cost you nothing but disk.

The hard part is what you watched Kaalm handle: an agent that is shut down
and restarted must not lose the thread. That is why storage is a first-class
part of the declaration, and why the count kept going. Anything your agent
keeps on that volume survives; anything it holds only in memory does not.

Next: [Give it a real brain](give-it-a-real-brain.md).
