# Welcome

This book takes you from an empty laptop to a running AI agent you can talk
to, in one sitting. You do not need to know Kubernetes; you will meet the
handful of ideas you need exactly when you need them.

By the end you will have an agent running on a cluster on your own machine.
You will talk to it over HTTP, hand it a one-off job, and watch it fall asleep
when nobody needs it and wake up when you say hello again, still remembering
the conversation you had before it slept.

Every command on these pages was run, in order, on a fresh cluster against
Kaalm 0.2.0, and the output you see is the output it printed.

## Two honest notes before you start

**The agent you run first is not very smart.** It echoes what you say back to
you. That is deliberate: it lets you learn everything Kaalm does, including
identity, storage, a front door, and sleeping and waking, without needing an
API key from a model provider or spending a cent. Once all of that works,
[Give It a Real Brain](give-it-a-real-brain.md) shows how to point it at an
actual model. That is the only chapter that needs an account.

**Everything here runs on your laptop and is thrown away at the end.** The
cluster lives inside Docker, and deleting it takes one command, which you will
see early so that experimenting feels safe.

Ready? Start with [What Is Kaalm](what-is-kaalm.md).
