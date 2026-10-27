# Try Kaalm out

Use this for a user whose goal is `try-it`. It follows the [learn book](https://github.com/win07xp/kaalm/blob/main/learn/src/welcome.md), which walks from an empty laptop to a running agent on a throwaway cluster. The first agent echoes messages and needs no API key and no account.

Go one chapter at a time. After each, say what now exists and ask whether to continue. Skip the book's explanations for terms the user's experience level already covers, and keep them for `new-to-kubernetes`.

| Step | Do | Learn book chapter |
|---|---|---|
| 1 | Check the tools, then create a local k3d cluster with [get-a-cluster.md](get-a-cluster.md). | [Setting up your laptop](https://github.com/win07xp/kaalm/blob/main/learn/src/setting-up.md), [A cluster in one command](https://github.com/win07xp/kaalm/blob/main/learn/src/a-cluster-in-one-command.md#the-cluster) |
| 2 | Install cert-manager and trust-manager with [check-dependencies.md](check-dependencies.md) step 3. | [The prerequisites](https://github.com/win07xp/kaalm/blob/main/learn/src/a-cluster-in-one-command.md#the-prerequisites) |
| 3 | Install Kaalm with [install-kaalm.md](install-kaalm.md). | [Kaalm](https://github.com/win07xp/kaalm/blob/main/learn/src/a-cluster-in-one-command.md#kaalm), [What got installed](https://github.com/win07xp/kaalm/blob/main/learn/src/a-cluster-in-one-command.md#what-got-installed) |
| 4 | Look at what Kaalm added. Run the read-only `kubectl get` and `describe` commands from the chapter. | [Looking around](https://github.com/win07xp/kaalm/blob/main/learn/src/looking-around.md) |
| 5 | Run the first agent: a class named `tutorial` and an agent named `helper`, through [change-safely.md](change-safely.md). | [Running an agent](https://github.com/win07xp/kaalm/blob/main/learn/src/running-an-agent.md) |
| 6 | Send it a message with [connect-a-channel.md](connect-a-channel.md). The reply is `echo: ...`. | [Talking to your agent](https://github.com/win07xp/kaalm/blob/main/learn/src/talking-to-your-agent.md) |

Write the manifests in step 5 from the chapter, with the image tag set to the installed Kaalm version. The chapter's own tag is the version it was walked against. The book's commands do not pass a context: add `--context CONTEXT` to each `kubectl` and `--kube-context CONTEXT` to each `helm`. The book's `k3d cluster create` command also gets `--kubeconfig-switch-context=false`, as [get-a-cluster.md](get-a-cluster.md) explains.

When step 6 works, the user has a running agent. Offer what the book does next, one at a time:

- [Make it yours](https://github.com/win07xp/kaalm/blob/main/learn/src/make-it-yours.md): give the agent code as configuration.
- [Giving it a job](https://github.com/win07xp/kaalm/blob/main/learn/src/giving-it-a-job.md): run a task, with [run-a-task.md](run-a-task.md).
- [Sleep and wake](https://github.com/win07xp/kaalm/blob/main/learn/src/sleep-and-wake.md): watch the agent hibernate and wake.
- [Give it a real brain](https://github.com/win07xp/kaalm/blob/main/learn/src/give-it-a-real-brain.md): connect a language model. It needs an account and a key, and it is the only step that costs money. Use [add-a-model-key.md](add-a-model-key.md) for the key, so you never see it.

## Clean up

Offer to delete the cluster with the command in [Tear it down](https://github.com/win07xp/kaalm/blob/main/learn/src/where-to-go-next.md#tear-it-down). Deleting removes the cluster, Kaalm, the agent, and its storage. Run it only with the user's OK.
