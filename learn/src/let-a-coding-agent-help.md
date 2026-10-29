# Let a coding agent help

The earlier chapters had you type every command. A coding agent such as Claude
Code can do the same work while you ask for it in plain words, if you give it
the Kaalm *skill*: a folder of instructions that teaches the agent how to use
Kaalm.

This chapter is the other one that cannot be walked end to end. What the agent
says depends on the agent and on your cluster, so there is no captured output
here, and the only commands are the two that install the skill. Treat it as a
signpost to a tool rather than a walkthrough.

## What it does for you

With the skill, you can take the same journey this book took by asking for each
stop:

- a cluster to run on, which is a set of machines that run your programs, here
  one on your laptop
- Kaalm installed on that cluster
- an agent running
- a message sent to the agent
- a task, a one-off job, run to completion

At the start, the agent asks how much Kubernetes you know and what you want to
do. If you say you are new to Kubernetes, it explains each Kubernetes and Kaalm
term in a sentence the first time it uses it, picks a sensible default and says
why, and goes one step at a time. If you already use Kubernetes, it drops the
explanations and shows you the details instead.

## How it keeps you safe

Three rules hold at every level:

- **It asks before it changes anything.** A *manifest* is a file that describes
  an object you want in the cluster. Before the agent applies one, it runs a
  *server-side dry run*: the cluster checks the change and answers as if it had
  applied it, without storing anything. The agent then tells you in a sentence
  or two what the change will do and waits for your yes.
- **It never reads or prints a Secret value or an API key.** When a step needs
  a key, the agent gives you a command to run yourself. You type the key at a
  hidden prompt, where nothing shows as you type, and the command stores it
  straight into a Secret. In Claude Code, you type `!` before the command to run
  it in your own session. The key never reaches the agent, and the agent never
  asks you to paste one into the chat.
- **It only works on the cluster you confirmed.** Your machine can know several
  clusters, each reached through a *context*, a saved name for a cluster and
  the account you use on it. The agent asks which context to use, repeats it
  back, and passes it on every command. It never switches your active context.

## What it remembers

The agent saves your answers so it does not ask twice: your experience, your
goal, and the cluster you confirmed. They go in `./.claude/kaalm.json`, in the
folder where you run the agent. It asks before it creates the file. The file
holds choices and names, never a key or any other secret.

## Install it

The book is pinned to Kaalm 2.1.0, so install the skill from the 2.1.0 tag. In
Claude Code, run:

```text
/plugin marketplace add win07xp/kaalm#v2.1.0
/plugin install kaalm@kaalm
```

Other agents copy the skill folder,
[`plugins/kaalm/skills/kaalm`](https://github.com/win07xp/kaalm/blob/v2.1.0/plugins/kaalm/skills/kaalm/SKILL.md),
from that tag into their own skills folder. If the Kaalm on your cluster is
another version, the agent warns you before it goes on.

## What to ask for

Start the agent in a folder you work from, then ask in your own words. For
example:

- "I'm new to Kubernetes and want to try Kaalm."
- "What is missing on my laptop before I can run Kaalm?"
- "Add my Anthropic API key so my agent can use a model."
- "Run a task on my cluster that prints hello."
- "Why is my agent not Ready?"

## Where the details live

This chapter is a signpost, not a manual. The full versions:

- The skill's entry point,
  [SKILL.md](https://github.com/win07xp/kaalm/blob/v2.1.0/plugins/kaalm/skills/kaalm/SKILL.md),
  lists every task it can do and the safety rules above.
- [Using the agent skill](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/agent-skill.md)
  in the user guide, for installing at your Kaalm version, upgrading, and the
  config file.
- The [Introduction](https://github.com/win07xp/kaalm/blob/main/guide/src/introduction.md)
  to the user guide, for the same work done by hand.

Next: [Where to go next](where-to-go-next.md).
