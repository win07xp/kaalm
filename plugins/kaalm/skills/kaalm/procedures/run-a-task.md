# Run an AgentTask

Use this for work that starts, finishes, and reports a result. An *AgentTask* runs one Pod to completion. It has no Service, no channel, and no hibernation. Kaalm must be installed, and a class must allow the task's image. The page behind this procedure is [Running tasks](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md).

## Steps

1. **Gather the names:** the namespace, an AgentClass that allows the image (see step 1 of [deploy-an-agent.md](deploy-an-agent.md)), and the image.
2. **Choose the image.** To prove the path first, use the Go base image `ghcr.io/win07xp/kaalm-agent-go:VERSION` with the `KAALM_TASK_AUTOCOMPLETE` environment variable, as [agenttask.yaml](../examples/agenttask.yaml) does. The task reports `success` as soon as it starts and does no work. For real work, the user's image does the work and reports its own result. See [Declaring a task](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#declaring-a-task) and [Writing a task in Python](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#writing-a-task-in-python).
3. **Pick a completion mode** with the user. `agentReported` means the task says when it is done and can return artifacts. `exitCode` means the container's exit status is the result, and zero succeeds. [The two completion modes](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#the-two-completion-modes) explains both.
4. **Write the task.** Copy [agenttask.yaml](../examples/agenttask.yaml). Set the name, class, image tag, and `completion`. Set `ttlSecondsAfterFinished`: the task and its result are deleted that many seconds after it finishes.
5. **Apply it** with [change-safely.md](change-safely.md), adding `-n NAMESPACE`.
6. **Watch it:**

   ```bash
   kubectl --context CONTEXT get agenttasks -n NAMESPACE
   ```

   `Phase` runs `Pending`, `Provisioning`, `Running`, `Completing`, then `Succeeded`, `Failed`, or `TimedOut`.
7. **Read the result** of an `agentReported` task before its TTL ends:

   ```bash
   kubectl --context CONTEXT get configmap TASK_NAME-completion -n NAMESPACE -o jsonpath='{.data}'
   ```

   See [Watching and reading results](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#watching-and-reading-results).
8. **If it does not reach `Succeeded`,** use [diagnose.md](diagnose.md). A crashed Pod is retried only when `completion.backoffLimit` is above zero. See [Cleanup and retries](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#cleanup-and-retries).
