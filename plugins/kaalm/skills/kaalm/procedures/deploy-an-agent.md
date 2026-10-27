# Deploy an Agent

Use this to run a long-lived agent: a Pod with its own identity, optional storage, and hibernation when idle. Kaalm must be installed ([install-kaalm.md](install-kaalm.md)). The page behind this procedure is [Your first agent](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/first-agent.md).

## Steps

1. **Gather three names, in this order.** The provider's `allowedNamespaces` needs the namespace, and the class check needs the provider.
   - The namespace for the agent. Use the saved `namespace`, or ask. `default` works for a first agent. A new namespace is a change: make it with `kubectl --context CONTEXT create namespace NAME` through [change-safely.md](change-safely.md).
   - The AgentClass: `kubectl --context CONTEXT get agentclasses`. An *AgentClass* is the set of rules an agent runs under. The chart installs `standard`, which allows any image but names no ModelProvider. For a model, the class must list the provider. To see which providers and namespaces a class allows (an empty namespace list admits every namespace):

     ```bash
     kubectl --context CONTEXT get agentclass CLASS_NAME -o jsonpath='{.spec.allowedProviders[*].name}{"\n"}{.spec.allowedNamespaces}{"\n"}'
     ```

     If no class fits, copy [agentclass.yaml](../examples/agentclass.yaml) (one person) or use [team-setup.md](team-setup.md). Check that the class name is free first, as [Names that already exist](change-safely.md#names-that-already-exist) says.
   - The ModelProvider, if the agent should call a model: `kubectl --context CONTEXT get modelproviders`. Its `allowedNamespaces` must admit the namespace. With none yet, use [add-a-model-key.md](add-a-model-key.md) now that the namespace exists, then come back here. Or deploy without one.
2. **Pick the image** with [choose-an-image.md](choose-an-image.md), then come back. For a first agent it picks the published Python base image, which echoes each message and proves the agent runs and can be reached.
3. **Write the Agent.** Copy [agent.yaml](../examples/agent.yaml). Set the name, `agentClassRef`, the image, and `providers`. Delete the `providers` block when there is no ModelProvider.
4. **Apply it** with [change-safely.md](change-safely.md), adding `-n NAMESPACE`.
5. **Watch it come up.** Run this every few seconds instead of `-w`, which never returns:

   ```bash
   kubectl --context CONTEXT get agents -n NAMESPACE
   ```

   `Phase` goes from `Pending` to `Provisioning` to `Running`, with `READY` `True`. It takes about 15 seconds on a small cluster, longer while the image downloads for the first time. See [Watch it come up](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/first-agent.md#3-watch-it-come-up).

   When the Agent lists providers, `Running` and `READY` `True` do not prove the model works: an Agent reaches `Running` while its ModelProvider is not `Ready`. Also read the `ProvidersReady` condition:

   ```bash
   kubectl --context CONTEXT get agent AGENT_NAME -n NAMESPACE \
     -o jsonpath='{.status.conditions[?(@.type=="ProvidersReady")].status} {.status.conditions[?(@.type=="ProvidersReady")].reason}{"\n"}'
   ```

   `True AllProvidersHealthy` is success. Anything else, such as `False ProviderUnhealthy` or `False ClassConstraintViolation`, goes to [diagnose.md](diagnose.md). Do not report success on `READY` alone.
6. **If it stays in `Pending` or `Provisioning`, shows `Degraded`, or `ProvidersReady` is not `True`,** use [diagnose.md](diagnose.md). The usual cause of `Degraded` with `ClassConstraintViolation` is that the class does not list the provider, or the provider does not admit the namespace. See [If it never reaches Running](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/first-agent.md#if-it-never-reaches-running).
7. **Save the namespace:** `python3 SKILL_DIR/scripts/config.py set namespace NAMESPACE`.

## What next

- Send it a message: [connect-a-channel.md](connect-a-channel.md). An agent has no address outside the cluster until it has a channel.
- Give it a real model: the echo handler makes no model call. Code that calls one needs a ModelProvider, the class and agent listing it, and a handler that sends requests through the gateway ([How the agent calls a model](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/first-agent.md#4-how-the-agent-calls-a-model)).
- Tools: [give-tools.md](give-tools.md).
- Reaching a service outside the cluster, or reading a non-model secret: [Reaching outside the cluster](run-a-task.md#reaching-outside-the-cluster) and [A secret the task reads](run-a-task.md#a-secret-the-task-reads) apply to agents too.
- Day-to-day behavior (idle, hibernation, wake): [Agent lifecycle day to day](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/lifecycle.md).
