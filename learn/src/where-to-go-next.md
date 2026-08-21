# Where to Go Next

## Tear it down

```bash
k3d cluster delete kaalm-tutorial
```

```
INFO[0001] Successfully deleted cluster kaalm-tutorial!
```

That is everything: the cluster, Kaalm, your agent, its storage, the lot.
Nothing survives on your laptop except the files you wrote.

And those are the part that matters. Three manifests and one handler
(`agent.yaml`, `channel.yaml`, `task.yaml`, `handler.py`) describe an agent,
a front door, a job, and the agent's actual behavior in well under a hundred
lines, and they work the same against a real cluster as against the one you
just deleted. That is the point of declaring things rather than operating
them.

## What you actually learned

Not Kubernetes, particularly. You learned four `kubectl` verbs, that spec is
the wish and status is the reality, and that an operator is a program that
closes the gap between them. Everything else was Kaalm's vocabulary: classes as
rules, agents as long-lived things with storage and an address, tasks as
one-shot work, channels as doors.

And you watched the one behavior that is genuinely hard to build yourself: an
agent shutting down when nobody needs it, and coming back with its memory
intact when somebody does.

## Three doors onward

**Do real work.** The [User Guide](https://github.com/win07xp/kaalm) is
task-shaped: installing for real, offering classes to teams, wiring up
providers, setting budgets, connecting channels, and a troubleshooting chapter
organized by the symptom you are actually seeing.

**Understand the machine.** The
[design book](https://github.com/win07xp/kaalm) is the specification: how the
controller reconciles, how the gateway routes and bills, how the TLS identity
fabric is built, and the twenty acceptance scenarios the whole system is
tested against. Start with its architecture chapter.

**Write a bigger agent.** You already write handlers; that is the whole
programming model. When one outgrows a ConfigMap, usually the day it needs a
dependency the base image does not bundle, the same handler moves into a
`FROM ghcr.io/win07xp/kaalm-agent-python` image, and beyond that lie the
starter templates and fully custom images. Whatever the packaging, the image
keeps handling the parts that are easy to get wrong: TLS with certificate
rotation, verifying the gateway, deduplicating redelivered messages, and
persisting state so hibernation does not lose the thread. The runtime
contract chapter in the design book is the authority on what an agent must
do.

## What this tutorial skipped

Deliberately, so that one sitting stayed one sitting:

- **Budgets and cost control.** You saw where prices are declared; you did not
  set a limit or watch an agent degrade when it hit one.
- **Multiple teams.** Everything here lived in one namespace as one user. The
  guide's [Managing Team Access](https://github.com/win07xp/kaalm) covers
  handing classes and providers to teams without handing over credentials.
- **The TLS fabric.** Certificates were issued, rotated, and verified for you
  throughout, and the book never mentioned it beyond installing cert-manager.
- **Production shape.** One laptop node, no ingress, no monitoring, timers set
  to seconds instead of half an hour. The guide's Observing the Platform part
  is where the fleet gets a screen (the optional console), Grafana
  dashboards, and traces.

Each of those is a chapter in one of the other two books. You now have the
vocabulary to read either of them.
