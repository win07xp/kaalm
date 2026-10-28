# Set up Kaalm for a team

Use this for a user whose goal is `team-setup`: a platform engineer who offers Kaalm to teams. The teams get agents; the platform team decides which images run, which models are used, what they may spend, and who may use what. The guide pages for this work start at [Offering agent classes](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/agent-classes.md).

Lay out the choices at each step and let the user decide, even when the user is new to Kubernetes.

## Steps

1. **Install.** Run [check-dependencies.md](check-dependencies.md) and, if needed, [install-kaalm.md](install-kaalm.md). Production use means a managed or shared cluster: [get-a-cluster.md](get-a-cluster.md).
2. **Plan with the user.** Ask for the team namespaces, the model providers and who may use each, the monthly spend limit per team, and who needs which Kubernetes access. Create each team namespace through [change-safely.md](change-safely.md).
3. **Add each provider.** For each model provider, use [add-a-model-key.md](add-a-model-key.md). Set `allowedNamespaces` to the team namespaces. Prefer exact names, and use a glob such as `team-*` only when the naming convention makes it safe.
4. **Offer classes.** Copy [agentclass-team.yaml](../examples/agentclass-team.yaml). Set `allowedNamespaces`, `allowedImages`, and `allowedProviders`. The chart's `standard` class allows any image and names no provider; replace it with your own class, or name providers on it with a chart value, as [A standard class](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/agent-classes.md#a-standard-class) shows. Two more classes are worth offering:
   - A class for handler mounts: [A starter class for handler mounts](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/agent-classes.md#a-starter-class-for-handler-mounts).
   - A class for agents that run untrusted code: [A sandboxed class for code-executing agents](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/agent-classes.md#a-sandboxed-class-for-code-executing-agents).
5. **Set budgets and limits.** The ModelProvider's `budget`, `rateLimits`, and `fallback` fields do this. Read [Budget policies](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/budgets-limits-fallback.md#budget-policies-warn-degrade-block) for what `warn`, `degrade`, and `block` do to the team, and [Turning on hard enforcement](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/budgets-limits-fallback.md#turning-on-hard-enforcement) when spend must not exceed the limit. Then [Rate limits](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/budgets-limits-fallback.md#rate-limits) and [Fallback chains](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/budgets-limits-fallback.md#fallback-chains). Apply changes with [change-safely.md](change-safely.md).
6. **Set access.** Every model call passes three checks: the workload's own list, the class, and the provider's namespace list ([The three access checks](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/managing-access.md#the-three-access-checks)). Then:
   - Grant a team: [Granting a team access](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/managing-access.md#granting-a-team-access).
   - Give people Kubernetes roles with the chart: [Kubernetes roles for a team](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/managing-access.md#kubernetes-roles-for-a-team). It is a Helm upgrade, so keep the user's other values ([upgrade-kaalm.md](upgrade-kaalm.md) step 3 shows how to read them).
   - Tool servers: [give-tools.md](give-tools.md).
7. **Verify.** Check the cluster-scoped objects, then deploy a test Agent in one team namespace with [deploy-an-agent.md](deploy-an-agent.md):

   ```bash
   kubectl --context CONTEXT get agentclasses,modelproviders,toolproviders
   ```

   Classes and providers should be `Ready`. A test Agent that reaches `Running` proves the three checks pass for that namespace.

## Later changes

- Remove a team's access: [Revoking a team](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/managing-access.md#revoking-a-team). Running Pods keep running; only model access goes.
- See who may use what: [Auditing with kubectl alone](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/managing-access.md#auditing-with-kubectl-alone).
- Change a class that has running agents: [Changing a class later](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/agent-classes.md#changing-a-class-later). Some edits replace Pods.
- Tune the controller and gateway: [Tuning the controller, gateway, and console](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/tuning.md).
