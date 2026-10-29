# Using the agent skill

The `kaalm` skill is a folder of instructions that a coding agent follows to do
Kaalm tasks for you. You ask in plain words, and the agent runs the commands,
shows you each change before it makes it, and checks the result. This page
covers how to install the skill, what to ask it, and the rules it follows.

The skill is a Claude Code plugin. Other agents that load skill folders can use
it by copying the folder.

## Install the skill at your Kaalm version

Each Kaalm release tag holds the skill that matches that release. Install the
tag that matches the Kaalm on your cluster. In Claude Code, run:

```text
/plugin marketplace add win07xp/kaalm#vX.Y.Z
/plugin install kaalm@kaalm
```

Replace `X.Y.Z` with your Kaalm version. For another agent, copy
`plugins/kaalm/skills/kaalm` from that tag into the agent's skills folder.

## Move the skill when Kaalm moves

The skill matches one Kaalm version, and its upgrade procedure moves your
cluster to that version. So:

- **To upgrade Kaalm with the agent**, install the skill at the tag you are
  upgrading to, then ask the agent to upgrade Kaalm. It follows
  [Upgrading Kaalm](upgrading.md). For a 1.x or 0.x install, it goes one step
  at a time and asks before each, because that move removes every Kaalm
  resource.
- **After you upgrade Kaalm by hand**, move the skill to the new tag. In Claude
  Code, remove the `kaalm` marketplace, add it again at the new tag, and
  install the plugin:

  ```text
  /plugin marketplace remove kaalm
  /plugin marketplace add win07xp/kaalm#vX.Y.Z
  /plugin install kaalm@kaalm
  ```

  Replace `X.Y.Z` with the new version. If the agent still reports the old
  version, restart the session or reload plugins. For another agent, replace
  the copied folder with the one from the new tag.

To confirm the skill and the cluster agree, ask the agent to check the Kaalm
version.

## The version check

Before any step that reads or changes a cluster, the agent looks up the
installed Kaalm version in the Helm release, or in the controller's image tag
when there is no release. It tells you the version and the namespace.

If the installed version differs from the one the skill matches, the agent warns
you that its steps and examples may not fit your cluster, names the tag to
install, and goes on only when you say so. If Kaalm is not installed, the agent
offers to install the version the skill matches.

## How the agent adapts to you

When you start, the agent asks two things, unless your request already answers
them: how much Kubernetes you know, and what you want to do. You can change
either answer at any time.

The experience level changes how the agent talks. Without an answer, the agent
treats you as new to Kubernetes.

| Level | Terms | Choices | Changes | Pace |
|---|---|---|---|---|
| `new-to-kubernetes` | Defines each Kubernetes and Kaalm term on first use. | Picks a default, says why, and offers to change it. | Says in plain words what will exist. Shows the YAML on request. | One step at a time, with what a good result looks like. |
| `uses-kubernetes` | Defines Kaalm terms only. | Lays out the options with trade-offs and asks. | Shows the YAML and a line on what it does. | Groups steps that belong together. |
| `runs-clusters` | Defines a term only when asked. | Asks only about decisions with consequences. | Shows the full YAML or diff. | Runs steps in order and reports the results. |

The goal sets where the agent starts when your request names no task:

- `try-it`: tries Kaalm out. You need no cluster yet.
- `run-agents`: checks what is missing, sets up the agent's namespace, adds a
  model key if the agent needs one, then runs the Agent or task.
- `team-setup`: starts at the team setup below. The agent lays out the choices
  at each step, even if you are new to Kubernetes.

## What you can ask for

The agent can check what is missing on your machine and help you get a cluster.
Beyond that, the tasks split by who does them.

For developers:

- Run an [agent](../developers/first-agent.md) or a
  [task](../developers/running-tasks.md).
- Choose or build the agent's image: [Deploying from a base
  image](../developers/deploying-from-a-base-image.md) and [Building your own
  agent image](../developers/building-your-own-image.md).
- Give an agent or task a secret value, such as an API token.
- [Connect a channel](../developers/connecting-a-channel.md).
- [Give agents tools](../developers/calling-tools.md).
- Put an existing Deployment's model calls behind the gateway, as in [Running
  framework agents](../developers/framework-agents.md).
- Find out why something is not Ready, as in
  [Troubleshooting](../reference/troubleshooting.md).

For platform teams:

- [Install](installation.md) and [upgrade](upgrading.md) Kaalm.
- Offer [agent classes](../platform/agent-classes.md).
- Add [providers and their keys](../platform/llm-access.md).
- Set [budgets, limits, and fallback](../platform/budgets-limits-fallback.md).
- Give teams [access](../platform/managing-access.md), including Kubernetes
  roles.

For a team setup, the agent first asks for the team namespaces, the providers
and who may use each, the monthly spend limit per team, and who needs which
Kubernetes access. At the end, it checks that the classes and providers are
`Ready` and deploys a test Agent in a team namespace.

## Safety rules the agent follows

These rules hold at every experience level.

- **Dry run, explain, ask, apply.** For each change, the agent runs a
  *server-side dry run*, in which the cluster checks the change and answers as
  if it had applied it, without storing anything. It then explains the change
  in a sentence or two, starting with the context, waits for your OK, and runs
  the real command. A change that is not a Kubernetes API call, such as
  creating a local cluster or building an image, has no dry run, so the agent
  explains it and asks first.
- **No Secret value or key is read or printed.** The agent never runs
  `kubectl get secret -o yaml` or reads a Secret's `data`, and it never asks you
  to paste a key into the chat. To see which keys a Secret holds, it uses
  `kubectl describe secret`, which lists key names and sizes, not values. When
  a step needs a key, the agent gives you a command to run yourself. You type
  the value at a hidden prompt, and the command creates the Secret with the
  labels Kaalm expects. In Claude Code, type `!` before the command to run it
  in your own session.
- **The saved context goes on every command.** The agent asks which context to
  use, saves it, and passes it as `kubectl --context` and `helm --kube-context`
  on every command. It asks again if the saved context's API server address
  changed. It never runs `kubectl config use-context`, so your active context
  never changes.
- **A class that other teams use is not changed.** Before it changes an
  AgentClass, the agent checks which namespaces and workloads use it. If anyone
  but you does, it proposes a separate class for your namespace. If your
  account may not create or change the object, it writes the manifest and a
  short request for your platform team instead.
- **A blocked command is handed to you.** If your agent's tool or permission
  settings block a command after its dry run passed, the agent looks for no
  other way. It gives you the exact command to run in your own terminal.
- **No cloud accounts and no paid resources.** For a managed cluster or a model
  provider account, the agent gives you the steps and you do them.

## The config file

The agent saves your answers in `./.claude/kaalm.json`, in the folder where you
run it, so it does not ask twice. The file holds:

- Your experience level and goal.
- The context you confirmed, with its API server address.
- Your default namespace and the ModelProvider you normally use.
- The installed Kaalm version and the namespace Kaalm runs in.

The agent asks before it creates the file, and tells you that it holds choices
and names, never secrets. The skill's script accepts only these fields, so
nothing else is written. In a git repository, the agent offers to add the file
to the repository's local exclude list, which, unlike `.gitignore`, is not
committed. If you decline the file, the agent keeps your answers for that
conversation only. Delete the file to make the agent ask again.

## More

- [SKILL.md](https://github.com/win07xp/kaalm/blob/main/plugins/kaalm/skills/kaalm/SKILL.md)
  is the skill's entry point. It links one procedure for each task.
- [Let a coding agent help](https://github.com/win07xp/kaalm/blob/main/learn/src/let-a-coding-agent-help.md)
  is the walkthrough for readers new to Kubernetes.
