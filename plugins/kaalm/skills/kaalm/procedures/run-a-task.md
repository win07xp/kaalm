# Run an AgentTask

Use this for work that starts, finishes, and reports a result. An *AgentTask* runs one Pod to completion. It has no Service, no channel, and no hibernation. Kaalm must be installed, and a class must allow the task's image. The page behind this procedure is [Running tasks](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/running-tasks.md).

## Steps

1. **Gather the names:** the namespace, an AgentClass that allows the image (see step 1 of [deploy-an-agent.md](deploy-an-agent.md)), and the image.
2. **Choose the image** with [choose-an-image.md](choose-an-image.md), then come back. To prove the path first, the demo image reports `success` at start and does no work; for real work, the user's image does the work and reports its own result ([Declaring a task](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/running-tasks.md#declaring-a-task)).
3. **Pick a completion mode** with the user. `agentReported` means the task says when it is done and can return artifacts. `exitCode` means the container's exit status is the result, and zero succeeds. [The two completion modes](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/running-tasks.md#the-two-completion-modes) explains both. The mode decides how the user reads the result (step 7) and how long the Pod stays. In `agentReported` mode the Pod is not stopped when the task reports: a container that keeps running stays up until the TTL removes the task. The Python base image keeps serving after the report, so on it, prefer `exitCode` when the task needs no artifacts and no result ConfigMap: the runtime then exits when `run_task` returns. With `agentReported` on the Python base image, set a short `ttlSecondsAfterFinished` and read the result first.
4. **Write the task.** Copy [agenttask.yaml](../examples/agenttask.yaml). Set the name, class, image, and `completion`. Set `ttlSecondsAfterFinished`: the task, its Pod, and its result are deleted that many seconds after it finishes. If the task reads a secret value or calls a host outside the cluster, do [A secret the task reads](#a-secret-the-task-reads) and [Reaching outside the cluster](#reaching-outside-the-cluster) first.
5. **Apply it** with [change-safely.md](change-safely.md), adding `-n NAMESPACE`.
6. **Watch it:**

   ```bash
   kubectl --context CONTEXT get agenttasks -n NAMESPACE
   ```

   `Phase` runs `Pending`, `Provisioning`, `Running`, `Completing`, then `Succeeded`, `Failed`, or `TimedOut`.
7. **Read the result** before the TTL ends. An `agentReported` task leaves a result in a ConfigMap:

   ```bash
   kubectl --context CONTEXT get configmap TASK_NAME-completion -n NAMESPACE -o jsonpath='{.data}'
   ```

   An `exitCode` task leaves no result ConfigMap, because the exit status is the verdict. Read its output from the Pod's log:

   ```bash
   kubectl --context CONTEXT logs -l kaalm.io/task=TASK_NAME -n NAMESPACE --all-containers
   ```

   See [Watching and reading results](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/running-tasks.md#watching-and-reading-results).
8. **If it does not reach `Succeeded`,** use [diagnose.md](diagnose.md). A crashed Pod is retried only when `completion.backoffLimit` is above zero. See [Cleanup and retries](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/running-tasks.md#cleanup-and-retries).

## A task that calls a model

A task reaches a model only through a ModelProvider it asks for. Three things must line up, as they do for an agent ([deploy-an-agent.md](deploy-an-agent.md) step 1):

- The task lists the provider in `spec.providers`: `providers: [{providerRef: {name: PROVIDER_NAME}}]`.
- The task's class lists the provider in `allowedProviders`.
- The provider's `allowedNamespaces` admits the task's namespace.

Without all three, the gateway answers the task's model calls with `403 access_denied` ([LLM call returns 403 access_denied](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/reference/troubleshooting.md#llm-call-returns-403-access_denied)).

### Porting a script that calls a model directly

A script that calls a provider with its own API key changes in four ways:

1. Remove the key from the code, the environment, and the image. The gateway holds it ([add-a-model-key.md](add-a-model-key.md)).
2. Send the call to the gateway, not to the provider. The Python base image gives the code a client that already carries the task's identity and trusts the cluster CA: `kaalm.http_client()` returns a standard `httpx.Client`, and `kaalm.gateway` is a preconfigured session ([The Python image](https://github.com/win07xp/kaalm/blob/v2.1.0/docs/src/runtime/base-images.md#the-python-image)). An image built another way follows [Running framework agents](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/framework-agents.md).
3. Name the model with the provider prefix, `PROVIDER_NAME/MODEL_ID`. The gateway reads the provider from the prefix.
4. Run blocking calls, such as `requests` or a synchronous client, with `asyncio.to_thread` inside `run_task`, so they do not block the event loop the runtime serves on:

```python
import asyncio
import os

import kaalm


def handle_message(envelope):
    return {"content": ""}  # required, though a task receives no messages


def ask(prompt):
    client = kaalm.http_client()
    reply = client.post(
        os.environ["KAALM_GATEWAY_ENDPOINT"] + "/v1/chat/completions",
        json={"model": "PROVIDER_NAME/MODEL_ID", "messages": [{"role": "user", "content": prompt}]},
    )
    return reply.json()


async def run_task():
    answer = await asyncio.to_thread(ask, "Summarize the report.")
    await kaalm.complete_task("success", "done")
```

The path `/v1/chat/completions` is for an `openai` or `openai-compatible` provider; an `anthropic` provider takes `/v1/messages`.

## A secret the task reads

A task or agent that reads a value that is not a model key, such as an API token for a service, gets it through `spec.env`:

```yaml
env:
  - name: REPORT_API_TOKEN
    valueFrom:
      secretKeyRef:
        name: report-api-token
        key: token
```

The Secret must be in the task's namespace and carry the label `kaalm.io/workload-secret: "true"`, or the task reports `Ready=False` with `SecretNotOptedIn`. Create the Secret before the task, with the command in [add-a-secret.md](add-a-secret.md) ("Value a workload reads" column), so you never see the value. Model keys do not use this path: the gateway holds them. [Label the Secrets a workload reads](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/managing-access.md#label-the-secrets-a-workload-reads) has the rule.

## Reaching outside the cluster

A task or agent reaches only the gateway and cluster DNS unless its class opens more. Model and tool calls go through the gateway and need nothing more. For a service outside the cluster, the class's `network.egress` lists the destinations:

- `allowedCIDRs` allows address ranges, on any CNI that enforces NetworkPolicy.
- `allowedHosts` allows DNS names, on Cilium only. On another CNI it has no effect.

See what the class allows with `kubectl --context CONTEXT get agentclass CLASS_NAME -o jsonpath='{.spec.network.egress}'`. Changing it is a change to the class: decide first whether the class is shared, with [Change a class only if no one else uses it](change-safely.md#change-a-class-only-if-no-one-else-uses-it), and use [change-safely.md](change-safely.md), including its rule for when the user may not change classes. A running task keeps the egress it started with; only tasks whose Pod is created afterward see the edit. The field rules are in [Network egress: allowedCIDRs and allowedHosts](https://github.com/win07xp/kaalm/blob/v2.1.0/docs/src/resources/agentclass.md#network-egress-allowedcidrs-and-allowedhosts), and [Changing a class later](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/agent-classes.md#changing-a-class-later) covers when edits reach workloads.

## Run it on a schedule

An AgentTask has no schedule. A Kubernetes *CronJob* starts a small `kubectl` Pod on a schedule, and that Pod creates a new AgentTask each time.

1. Copy [agenttask-recurring.yaml](../examples/agenttask-recurring.yaml). It holds the task manifest in a ConfigMap, a ServiceAccount that may only create AgentTasks, and the CronJob.
2. Set `namespace` in every object, the `schedule`, the task (class, image, completion), and `KUBECTL_VERSION` to `v` and the cluster's server version, which `kubectl --context CONTEXT version` prints. Apply the file without `-n`, because the file names the namespace.
3. The task uses `generateName` instead of `name`, so each run gets a new name. Keep the prefix short, lowercase, and free of dots: the generated name must be a valid name of at most 63 characters, or the API server rejects the task.
4. Set `ttlSecondsAfterFinished` in the task, so finished tasks delete themselves and do not pile up.
5. Apply it with [change-safely.md](change-safely.md). Check it with `kubectl --context CONTEXT get cronjob -n NAMESPACE`. To test without waiting, create a Job from it: `kubectl --context CONTEXT create job --from=cronjob/CRONJOB_NAME test-run -n NAMESPACE`, dry run first.
