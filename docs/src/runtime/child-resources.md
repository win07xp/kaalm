# Child Resources

When you create an Agent or an AgentTask, the controller does not just start a container. It provisions a small set of Kubernetes resources around that container: storage, identity, TLS material, and a network boundary. This page is the inventory. For each resource it states who owns it, what condition must hold for it to exist, and what happens to it when the workload hibernates or is deleted.

The two workload kinds get different sets, and the difference follows from their shape. An Agent is long-lived and can receive inbound messages, so it gets a Service, a server-auth certificate, and an ingress allow rule. An AgentTask is ephemeral and has no listener, so it gets none of those.

## Agent Child Resources

For each Agent, the controller provisions the resources below. **The Pod is the only state-coupled resource:** it is created on the transition into `Running` and deleted on the transition into `Hibernated`. Every other resource is provisioned on first reconcile and persists across hibernation. Retention details follow the list.

- **One Pod** containing the user's agent container, present only while the Agent is `Running`. The Pod runs under the [RuntimeClass](../security/model.md#runtimeclass) specified by its AgentClass, if one is set (e.g. gVisor or Kata); when unset it runs under the cluster's default container runtime.
- **One PVC** if the [Agent spec requests persistence](../resources/agent.md#spec), mounted into the agent container at a configured path. It is provisioned by the controller, or is a pre-existing claim referenced via `persistence.existingClaim` (no provisioning, no ownerRef; see [Agent](../resources/agent.md)).
- **One Service** (ClusterIP) if [`spec.service.enabled`](../resources/agent.md#spec) (default `true`), exposing the agent's HTTPS endpoint for intra-cluster traffic. The gateway uses this Service to deliver channel messages via [`POST /v1/message`](../gateways/api/agent-endpoints.md#post-v1message) over TLS; direct external exposure remains the developer's responsibility. Agents with the Service disabled are outbound-only: they have no inbound delivery path and cannot be referenced by an AgentChannel (validated by AgentChannelReconciler with `Ready=False, reason=AgentServiceDisabled`).
- **One [cert-manager `Certificate`](../security/tls.md#lifecycle-of-an-agent-tls-serving-certificate)** (and the Secret it writes) holding a per-agent TLS cert (`server auth, client auth`) signed by the Agentry CA `ClusterIssuer` and rotated continuously by cert-manager. The same cert serves the agent's HTTPS listener and is presented client-side on every agent to gateway call. The Agentry CA bundle is projected into Pods via trust-manager.
- **One ServiceAccount**, per-Agent, with no RoleBindings by default. The agent has no Kubernetes API access unless the platform team or developer explicitly grants it. See [Agent Pod ServiceAccount](../security/rbac.md#agent-pod-serviceaccount).
- **One NetworkPolicy** synthesized from the AgentClass network policy and the gateway's egress allow rule. See the [full rule set](../controller/reconcilers.md#agentreconciler) (AgentReconciler step 6), and the discussion below.

### Why the synthesized NetworkPolicy is load-bearing

The per-Agent NetworkPolicy is the primitive cited in the [gateway architecture analysis](../gateways/llm/overview.md#architecture-option-analysis) for keeping LLM credentials inside `agentry-system`. **NetworkPolicy enforcement by the cluster CNI is a required prerequisite of Agentry's trust model.**

Its two halves carry different weight:

- **The ingress rule is layered**, not solitary. It is combined with the [agent-side mTLS check on `POST /v1/message`](../security/tls.md#in-cluster-tls) (specified in [The Runtime Contract](contract.md), bullet 4), so a misconfigured per-Agent NetworkPolicy does not open delivery to arbitrary in-cluster callers.
- **The egress rule is not layered.** It is the only Agentry-managed control preventing agents from calling provider IPs directly.

Three caveats bound the guarantee. This synthesis applies only to Agentry-managed Pods (Agents and AgentTasks); the gateway-only tier's egress responsibility is stated under [Adoption Tiers](../concepts/tenancy-and-tiers.md#adoption-tiers). Because NetworkPolicy is additive, the guarantee assumes the developer trust tier defined in [Trust Model](../security/model.md#trust-model). And CNI enforcement remains a hard prerequisite: clusters running default kindnet or default flannel do not enforce NetworkPolicy and are not supported deployment targets. See also [Recommendation #4](../security/model.md#recommendations-for-deployment).

### Ownership and deletion

All of the resources above live in the same namespace as the Agent CR and carry an ownerRef back to it. Full Agent deletion cascade-GCs them. The one exception is a PVC referenced via `persistence.existingClaim`: the controller does not own it, so it survives.

**There is no per-Agent configuration ConfigMap.** Non-sensitive config (gateway endpoint, ports) is delivered as env vars injected at Pod creation, and config changes are Pod-replacing spec drift by design. The same model applies to AgentTask.

### What survives hibernation

On `Hibernated`, only the Pod is deleted. The PVC, per-Agent `Certificate` (and its Secret), Service (with no endpoints), ServiceAccount, and NetworkPolicy are all retained, so that wake-on-demand can recreate the Pod against unchanged identity and storage. See [Hibernation mechanics](../controller/hibernation-and-wake.md#hibernation-mechanics).

### AgentClass changes

AgentClass changes propagate to existing Agents along one of three paths, depending on whether the change constrains the derived Pod spec, excludes the Agent's stored spec, or only affects per-request routing; the mechanics of each path, including which child resources are re-derived and which are preserved, are in [AgentClass change handling](../controller/change-propagation.md#agentclass-change-handling).

### No sidecar

There is no sidecar container. The **Agentry Gateway** in `agentry-system` handles all LLM traffic and inbound channel messages as a shared cluster-level service.

## AgentTask Child Resources

For each AgentTask, the controller provisions a parallel set of resources tailored to its ephemeral, no-inbound nature. See [AgentTaskReconciler](../controller/reconcilers.md#agenttaskreconciler) for the authoritative step list.

- **One Pod** containing the user's task container, under the AgentClass [RuntimeClass](../security/model.md#runtimeclass).
- **One PVC** if the task spec requests persistence.
- **One [cert-manager `Certificate`](../security/tls.md#lifecycle-of-an-agenttask-tls-client-certificate)** (and its Secret) holding a per-task TLS cert with `usages: client auth` only. The task uses it to authenticate outbound calls (LLM proxy, `/v1/task/complete`). There is no server-auth EKU because the task does not expose an HTTPS listener.
- **One ServiceAccount**, per-task, with no RoleBindings by default, matching the opt-in posture of Agent Pods. See [Agent Pod ServiceAccount](../security/rbac.md#agent-pod-serviceaccount).
- **One NetworkPolicy** synthesized from the AgentClass and the gateway's egress allow rule. AgentTask Pods have no listener and no Service, so the synthesized policy carries the standard egress allow set with **no ingress allow rules**: default-deny ingress is the posture, made explicit in the synthesized YAML (see [AgentTaskReconciler](../controller/reconcilers.md#agenttaskreconciler)). In contrast, Agent Pods receive `/v1/message` from the gateway and thus carry an explicit gateway to agent ingress allow rule on the agent's HTTPS listener port.

### Additional resources for `agentReported` completion

When [`completion.condition: agentReported`](../controller/task-lifecycle.md), the controller additionally provisions:

- **A pre-created per-task completion ConfigMap** (initial `data: {}`) where the gateway writes the completion payload.
- **A per-task `Role` and `RoleBinding`** granting the gateway ServiceAccount name-scoped `update`/`patch` on that ConfigMap.

This ConfigMap is a completion channel, not a config-delivery mechanism; it is unrelated to the config ConfigMap that Agentry deliberately does not create.

Alongside these, the AgentTaskReconciler stamps `AgentTask.status.currentPodUID = Pod.UID` on every Pod creation (initial provision and `backoffLimit` retries) and clears it during the retry-reset window. The gateway reads this field from its existing cluster-wide AgentTask watch (the same cache used for the `exitCode` short-circuit and artifact-name validation) and rejects mismatched callers at `/v1/task/complete` with `403 access_denied` `reason=StalePodCompletion`. The reset/restamp ordering is documented in [Retry mechanics](../controller/task-lifecycle.md).

### What AgentTasks do not get

There is no Service (tasks do not receive channel messages and have no stable endpoint) and no generic configuration ConfigMap (task config is delivered via env vars and Pod spec). All resources are owner-referenced to the AgentTask for cascade GC.
