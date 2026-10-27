# Run an AgentTask

Use this for work that starts, finishes, and reports a result. An *AgentTask* runs one Pod to completion. It has no Service, no channel, and no hibernation. Kaalm must be installed, and a class must allow the task's image. The page behind this procedure is [Running tasks](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md).

## Steps

1. **Gather the names:** the namespace, an AgentClass that allows the image (see step 1 of [deploy-an-agent.md](deploy-an-agent.md)), and the image.
2. **Choose the image** with [choose-an-image.md](choose-an-image.md), then come back. To prove the path first, the demo image reports `success` at start and does no work; for real work, the user's image does the work and reports its own result ([Declaring a task](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#declaring-a-task)).
3. **Pick a completion mode** with the user. `agentReported` means the task says when it is done and can return artifacts. `exitCode` means the container's exit status is the result, and zero succeeds. [The two completion modes](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#the-two-completion-modes) explains both. The mode decides how the user reads the result (step 7) and how long the Pod stays. In `agentReported` mode the Pod is not stopped when the task reports, so an image that keeps running after it reports stays up until the TTL; make the image exit after it reports.
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

   See [Watching and reading results](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#watching-and-reading-results).
8. **If it does not reach `Succeeded`,** use [diagnose.md](diagnose.md). A crashed Pod is retried only when `completion.backoffLimit` is above zero. See [Cleanup and retries](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#cleanup-and-retries).

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

The Secret must be in the task's namespace and carry the label `kaalm.io/workload-secret: "true"`, or the task reports `Ready=False` with `SecretNotOptedIn`. Create the Secret before the task, with the command in [add-a-secret.md](add-a-secret.md) ("Value a workload reads" column), so you never see the value. Model keys do not use this path: the gateway holds them. [Label the Secrets a workload reads](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/managing-access.md#label-the-secrets-a-workload-reads) has the rule.

## Reaching outside the cluster

A task or agent reaches only the gateway and cluster DNS unless its class opens more. Model and tool calls go through the gateway and need nothing more. For a service outside the cluster, the class's `network.egress` lists the destinations:

- `allowedCIDRs` allows address ranges, on any CNI that enforces NetworkPolicy.
- `allowedHosts` allows DNS names, on Cilium only. On another CNI it has no effect.

See what the class allows with `kubectl --context CONTEXT get agentclass CLASS_NAME -o jsonpath='{.spec.network.egress}'`. Changing it is a change to the class: use [change-safely.md](change-safely.md), including its rule for when the user may not change classes. A running task keeps the egress it started with; only tasks whose Pod is created afterward see the edit. The field rules are in [Network egress: allowedCIDRs and allowedHosts](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/agentclass.md#network-egress-allowedcidrs-and-allowedhosts), and [Changing a class later](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/agent-classes.md#changing-a-class-later) covers when edits reach workloads.

## Run it on a schedule

An AgentTask has no schedule. A Kubernetes *CronJob* starts a small `kubectl` Pod on a schedule, and that Pod creates a new AgentTask each time.

1. Copy [agenttask-recurring.yaml](../examples/agenttask-recurring.yaml). It holds the task manifest in a ConfigMap, a ServiceAccount that may only create AgentTasks, and the CronJob.
2. Set `namespace` in every object, the `schedule`, the task (class, image, completion), and `KUBECTL_VERSION` to `v` and the cluster's server version, which `kubectl --context CONTEXT version` prints. Apply the file without `-n`, because the file names the namespace.
3. The task uses `generateName` instead of `name`, so each run gets a new name. Keep the prefix short, lowercase, and free of dots: the generated name must be a valid name of at most 63 characters, or the API server rejects the task.
4. Set `ttlSecondsAfterFinished` in the task, so finished tasks delete themselves and do not pile up.
5. Apply it with [change-safely.md](change-safely.md). Check it with `kubectl --context CONTEXT get cronjob -n NAMESPACE`. To test without waiting, create a Job from it: `kubectl --context CONTEXT create job --from=cronjob/CRONJOB_NAME test-run -n NAMESPACE`, dry run first.
