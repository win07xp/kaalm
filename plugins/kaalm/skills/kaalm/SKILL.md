---
name: kaalm
description: Use Kaalm, the Kubernetes operator that runs AI agents (kaalm.io resources). Load when the user wants to install Kaalm, try it, deploy an Agent, run an AgentTask, connect an AgentChannel, give agents tools, set up ModelProviders, AgentClasses, budgets, or team access, upgrade Kaalm, get a Kubernetes cluster for it, or fix a Kaalm resource that is not Ready. Also load for running AI agents on Kubernetes when Kaalm is the tool. For readers new to Kubernetes as well as developers and platform engineers.
---

# Kaalm

This skill matches Kaalm v2.1.0.

This skill holds procedures: what to run, in what order, and what to check. The facts live in Kaalm's books, which each procedure links. Read the linked page when a step fails or the user asks for more than the procedure covers.

**Read a linked page as raw Markdown.** A fetch tool that is handed a GitHub page address often returns a summary without the exact commands. Fetch the raw text with `curl -fsSL URL` (or `gh api`), because a fetch tool can summarize even a raw page. Build the URL by replacing `https://github.com/win07xp/kaalm/blob/` with `https://raw.githubusercontent.com/win07xp/kaalm/`, and replace the ref (`main` or the skill's tag) with `v` and the installed Kaalm version, since the cluster runs that version. Before Kaalm is installed, keep the ref in the link. Take commands and values from that text word for word, with two changes. Book commands do not pass a context, so add `--context CONTEXT` to every `kubectl` command and `--kube-context CONTEXT` to every `helm` command you take from a page. And a `#anchor` means nothing in raw Markdown, so find the section by its heading text.

## Start every task

1. **Load the config.** The user's choices live in `.claude/kaalm.json` in the folder the user works from. Run `python3 SKILL_DIR/scripts/config.py show` from the user's folder, where `SKILL_DIR` is this skill's folder. The script reads and writes `.claude/kaalm.json` relative to where it runs. Without `python3`, read the file by hand, treat a missing file as empty, and keep to the keys in the "Format" section of the `config.py` docstring.
2. **Ask about experience and goal** when either is missing and the request does not answer it. Ask one short question with two parts:
   - Experience: new to Kubernetes, uses Kubernetes, or runs clusters.
   - Goal: try it out, run my own agents, or set up for a team.

   Repeat the choice back. The values to save are `new-to-kubernetes`, `uses-kubernetes`, `runs-clusters`, `try-it`, `run-agents`, and `team-setup`. The user can change either at any time, and a request that shows a different level or goal is a reason to update it.
3. **Save the choices with `config.py set`.** Before the first write, ask whether to create the file, and say that it holds choices and names, never secrets. In a git repository, offer to append `.claude/kaalm.json` to the file that `git rev-parse --git-path info/exclude` prints. If the user declines, keep the answers for this conversation only.
4. **Pick the procedure** from [Where to go](#where-to-go).
5. **Confirm the cluster context** (see [The cluster context](#the-cluster-context)) and **check the Kaalm version** (see [The version check](#the-version-check)) before any step that reads or changes a cluster. If the user has no cluster or no context, do these once the cluster exists: [procedures/try-it-out.md](procedures/try-it-out.md) for a user who wants to try Kaalm, otherwise [procedures/get-a-cluster.md](procedures/get-a-cluster.md), which come back to this step.

## The cluster context

A kube context names a cluster and a user. Every command below passes the saved context, so no command reaches a cluster the user did not confirm. Never run `kubectl config use-context`.

- **No saved context, or `context.name` is gone from `kubectl config get-contexts -o name`:** list the contexts and ask which one to use. For `new-to-kubernetes`, name the cluster in plain words, such as "the local cluster `kaalm-tutorial` we created", and do not show the API server address. For the other levels, show the name and the address. After the user confirms, save both with `config.py set context.name` and `config.py set context.server`.
- **A saved context that still exists:** read its API server address and compare it with `context.server`. Ask again only when they differ. A context name can point at another cluster after a kubeconfig change.

  ```bash
  kubectl config view --minify --context CONTEXT -o jsonpath='{.clusters[0].cluster.server}'
  ```

- **Otherwise:** use the saved context without asking.

Replace `CONTEXT` with the context name.

## The version check

Find the installed Kaalm version:

```bash
helm --kube-context CONTEXT list -A -o json
```

The entry whose `chart` starts with `kaalm-` gives `namespace` and `app_version`. If Helm has no such entry, read the controller image tag:

```bash
kubectl --context CONTEXT get deploy -A -l app.kubernetes.io/name=kaalm,app.kubernetes.io/component=controller \
  -o jsonpath='{range .items[*]}{.metadata.namespace} {.spec.template.spec.containers[0].image}{"\n"}{end}'
```

No output means Kaalm is not installed, and [procedures/install-kaalm.md](procedures/install-kaalm.md) installs the version this skill matches.

When Kaalm is installed, tell the user its version and namespace. A *namespace* is a named folder for objects inside the cluster; Kaalm's own parts live in one. Then save what you found: `config.py set install.namespace` with the namespace, and `config.py set install.chartVersion` with the version (the `chart` value without `kaalm-`, or the image tag). The same consent as the config file applies.

Compare versions without the leading `v`: Helm's `app_version` is `2.1.0`, and the line above says `v2.1.0`. If the installed version differs from the version above, warn the user in plain words: the steps and examples in this skill may not fit their cluster. Name the skill that matches: in Claude Code, `/plugin marketplace add win07xp/kaalm#vX.Y.Z` with the installed version, then install the `kaalm` plugin. For another agent, copy `plugins/kaalm/skills/kaalm` from that tag. Continue only when the user says to.

## How experience changes what you do

Match the saved `experience`. Without a saved value, treat the user as new to Kubernetes.

| | `new-to-kubernetes` | `uses-kubernetes` | `runs-clusters` |
|---|---|---|---|
| Terms | Define each Kubernetes and Kaalm term in one sentence on first use. | Define Kaalm terms only. | Define a term only when asked. |
| Choices | Pick a default, say why in one sentence, and offer to change it. | Lay out the options with their trade-offs and ask. | Lay out the options and ask only about decisions with consequences. |
| Showing a change | Say in plain words what will exist and what it does. Offer the YAML, and show it on request. | Show the YAML and one line on what it does. | Show the full YAML or diff. |
| Pace | One step at a time. Say what to expect and what a good result looks like. | Group steps that belong together. | Run steps in order and report the results. |

Use short sentences and plain words with every user. After a command, say what it showed in a sentence instead of pasting long output.

## Safety rules

These rules hold at every experience level.

- **Dry run, explain, ask, apply.** Every change to a cluster gets a server-side dry run, then one or two sentences on what it does, then the user's OK, then the real command. [procedures/change-safely.md](procedures/change-safely.md) has the commands. The secret command that the user runs has no dry run from you, because only the user can type the value. The checks after it replace the dry run.
- **Scripts that create resources only write files.** Write the manifest or script for the user to review. Apply a manifest only through the steps above.
- **Never read or print a Secret value or an API key.** Do not run `kubectl get secret -o yaml`, `-o json`, or a `jsonpath` on `data`. Do not ask the user to paste a key into the chat. The user runs the command that handles a key, as [procedures/add-a-secret.md](procedures/add-a-secret.md) shows. To see which keys a Secret holds, run `kubectl --context CONTEXT describe secret NAME`: it lists key names and sizes, never values. If a command output holds a key anyway, do not repeat it, and tell the user.
- **Pass the saved context on every command you write:** `kubectl --context CONTEXT` and `helm --kube-context CONTEXT`. This holds in code blocks, in sentences that name a command, and in text someone else will run, such as a request to the platform team. `kubectl config ...` and `--local` commands touch no cluster and need no context.
- **Report only output from commands that ran.** When you cannot run a command, say so and ask the user to run it and share the result. Label any output you show for it as expected output, not a result, and make no decision from it as if it were confirmed.
- **Name the context in every proposed change.** Start the summary with the context, for example "On context `k3d-kaalm-tutorial`, this creates...".
- **Create no cloud accounts and no paid resources.** For a managed cluster or a model provider account, give the steps and let the user do them.

## Where to go

Start with the row that matches the request. If the request names no task, use the user's goal: `try-it` starts at the first row, `run-agents` at the dependency check, then the agent's namespace, then (when the agent needs a model) the key row, then the Agent or task rows, in that order, because a provider's `allowedNamespaces` needs the namespace, and `team-setup` at the team row.

| The user wants to | Read |
|---|---|
| Try Kaalm out, with no cluster yet | [procedures/try-it-out.md](procedures/try-it-out.md) |
| Check what is missing before anything else | [procedures/check-dependencies.md](procedures/check-dependencies.md) |
| Get a Kubernetes cluster | [procedures/get-a-cluster.md](procedures/get-a-cluster.md) |
| Install Kaalm | [procedures/install-kaalm.md](procedures/install-kaalm.md) |
| Give agents a model, with an API key | [procedures/add-a-model-key.md](procedures/add-a-model-key.md) |
| Give an agent or task a secret value, such as an API token | [procedures/add-a-secret.md](procedures/add-a-secret.md) |
| Choose or build the agent's image | [procedures/choose-an-image.md](procedures/choose-an-image.md) |
| Send model calls from an existing Deployment through the gateway | [procedures/connect-an-existing-deployment.md](procedures/connect-an-existing-deployment.md) |
| Run a long-lived agent | [procedures/deploy-an-agent.md](procedures/deploy-an-agent.md) |
| Run a job that finishes | [procedures/run-a-task.md](procedures/run-a-task.md) |
| Send messages to an agent from outside | [procedures/connect-a-channel.md](procedures/connect-a-channel.md) |
| Let agents call tools | [procedures/give-tools.md](procedures/give-tools.md) |
| Offer Kaalm to teams: classes, providers, budgets, access | [procedures/team-setup.md](procedures/team-setup.md) |
| Find out why something is not working | [procedures/diagnose.md](procedures/diagnose.md) |
| Upgrade Kaalm | [procedures/upgrade-kaalm.md](procedures/upgrade-kaalm.md) |

Manifests the procedures use are in `examples/`. Copy one into the user's folder, edit the copy, and leave the original alone.
