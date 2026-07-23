# What Is Kaalm

An AI agent, in this book's sense, is a small program with a model behind it.
You send it a message, it thinks, it answers. You can also hand it a job and
walk away while it works.

Writing that program is the easy part. A few dozen lines will call a model and
print a reply. The hard part starts when you want to actually run it, and keep
running it, for real people.

## Why running one is more than running a script

Once an agent is something other people talk to, a pile of unglamorous
questions arrives at once:

- **Who is it?** It calls a model provider on your account. That means it holds
  a credential, and you would rather that credential not be baked into the
  program, copied into a config file, or visible to every other agent you run.
- **What does it remember?** A chat that forgets everything the moment the
  process restarts is not much use. Something has to persist, and survive the
  program being moved, restarted, or replaced.
- **What stops it spending your money?** A model call costs. An agent in a loop
  costs a lot. Someone needs to be able to say "this team gets this much".
- **How do people reach it?** It needs a front door: a URL, something checking
  that callers are allowed in, and an answer coming back.
- **What does it cost while nobody is using it?** An agent that is idle at 3am
  is still a running program consuming memory, unless something notices and
  stops it.

Every one of those is a solved problem in general. Solving them again, per
agent, by hand, is the tax you pay for running agents seriously.

## What Kaalm does about it

Kaalm makes an agent a thing you *declare* rather than a thing you *operate*.
You write down what you want, including which image to run, how much storage it
gets, which model providers it may use, and when it should be allowed to sleep,
and Kaalm keeps reality matching that description.

In return it handles the list above: it issues each agent its own identity, it
gives it a volume that outlives the program, it puts a gateway in front of the
model providers so the credential never reaches your code, it enforces spending
limits, it exposes a front door with authentication, and it puts idle agents to
sleep and wakes them when a message arrives.

None of that requires you to learn Kubernetes first. It does run *on*
Kubernetes, and you will pick up the vocabulary as you go, one word at a time,
at the moment it first matters.

## What you will have by the end

A cluster on your laptop running Kaalm, and on it:

- an agent, with its own storage and its own identity,
- a URL you can `curl` to talk to it, protected by a token,
- a completed one-off job that cleaned up after itself,
- and that same agent asleep, then awake again, still remembering you.

Then you throw the whole thing away with one command.

Next: [Setting Up Your Laptop](setting-up.md).
