# Sleep and Wake

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

The agent moves from `Running` through `Idle` and `Hibernating` to
`Hibernated`. The two middle states can pass in a second or two, so do not
worry if you blink and miss them; `Hibernated` is the one that sticks. Press
Ctrl-C when it settles:

```
NAME     PHASE        READY   CLASS      AGE
helper   Hibernated   False   tutorial   82s
```

> **What counts as activity?** Kaalm watches the model calls an agent makes
> through the gateway, because that is the traffic that costs money. This
> starter agent never calls a model, so it is idle by that measure from the
> moment it starts. That is why it hibernates on the timer even though you were
> chatting with it. An agent that actually calls a model stays awake while it
> is working.

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
helper   Hibernated   False   tutorial   82s
```

And so does its storage:

```bash
kubectl get pvc
```

```
NAME            STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   AGE
helper-memory   Bound    pvc-6a91ee09-50a1-461b-a2e4-da964ead5f77   1Gi        RWO            local-path     82s
```

That is the trick in one screen: the expensive part (a running program) is
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
{"content":"starter-go received: are you awake (message 3 from you)","attachments":null,"metadata":{"sessionId":"","userId":""}}
```

Read that reply again. **Message 3.**

The program that counted messages one and two was destroyed. This is a
different program, in a different container, started seconds ago because your
message arrived. It picked up the count from the volume Kaalm reattached, and
carried on.

On the walk that produced this book, the whole thing took two seconds. Your
caller waited; nothing was lost; the agent simply answered.

```bash
kubectl get agents
```

```
NAME     PHASE     READY   CLASS      AGE
helper   Running   True    tutorial   114s
```

Awake again, ready to go quiet and repeat the cycle.

## Why this matters

Consider a hundred agents, each used for a few minutes a day. Without
hibernation you pay for a hundred running programs around the clock. With it
you pay for the handful that happen to be in a conversation right now, and the
rest cost you nothing but disk.

The catch is the one you just watched Kaalm handle: an agent that is shut down
and restarted must not lose the thread. That is why storage is a first-class
part of the declaration, and why the count kept going. Anything your agent
keeps on that volume survives; anything it holds only in memory does not.

Next: [Give It a Real Brain](give-it-a-real-brain.md).
