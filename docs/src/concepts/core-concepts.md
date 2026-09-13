# Core concepts

This page introduces every term the rest of the book leans on. Later chapters define nothing new; they only go deeper on what is named here.

## The custom resources

Kaalm workloads come in two shapes. An **Agent** is a long-lived workload that the controller can hibernate and wake on demand. An Agent may have inbound channels attached through AgentChannels. An **AgentTask** is an ephemeral one-shot job: no inbound endpoint, no hibernation, torn down on completion. Both run as Pods under the policy template defined by an AgentClass.

| Kind | Scope | Tier | Purpose |
|---|---|---|---|
| AgentClass | Cluster | Full lifecycle only (chart default in both) | Policy template: runtime, isolation, allowed providers, network egress |
| ModelProvider | Cluster | Both | LLM provider config: credentials Secret, allowed namespaces, fallback chain |
| ToolProvider | Cluster | Both | External tool server config: endpoint, credentials Secret, tool catalog, allowed namespaces |
| Agent | Namespace | Full lifecycle only | Long-lived agent workload |
| AgentTask | Namespace | Full lifecycle only | Ephemeral task workload |
| AgentChannel | Namespace | Full lifecycle only | Inbound channel (webhook, Discord, or WhatsApp) bound to an Agent |

In plain language:

- **AgentClass** is a platform-team-owned policy resource, analogous to StorageClass: it decides how a category of agents is allowed to run, not what any one agent does.
- **ModelProvider** is a platform-team-owned wrapper around one LLM provider: it holds the API key Secret so individual teams never do, and says which namespaces may use the provider.
- **ToolProvider** is the same wrapper around one external tool server, so agents call tools through the gateway the way they call models: the credential stays with the platform team, and the resource says which namespaces may use the server.
- **Agent** is the developer-facing resource for one long-running agent: its image, its persistence needs, which AgentClass governs it, and which ModelProviders it may call.
- **AgentTask** is the developer-facing, Job-like resource for a goal-driven agent that runs once, reports a defined completion condition, hands back artifacts, and goes away.
- **AgentChannel** connects a running Agent to a user-facing communication channel, so people outside the cluster can message it.

The resources form one reference graph, split along the scope boundary in the table above: the cluster-scoped policy resources the platform team creates, and the namespaced workload resources developers point at them. The four figures below draw the same six boxes each time and add one kind of reference at a time. Arrows point from a field to what it names.

![Structural references between the six Kaalm CRDs. Cluster scope holds AgentClass, ModelProvider, and ToolProvider; namespace scope holds Agent, AgentTask, and AgentChannel. Agent and AgentTask each name an AgentClass through spec.agentClassRef, and AgentChannel names an Agent through spec.agentRef, never an AgentTask.](../diagrams/crd-references-structure.svg)

**What a workload is.** Every Agent and AgentTask names exactly one AgentClass, and the class is what the platform team's policy attaches to. An AgentChannel names one Agent by name in its own namespace; a task has no channel, because it is not a resident to talk to.

![The model-provider gates. AgentClass names ModelProviders through spec.allowedProviders, and Agent and AgentTask each name a ModelProvider through spec.providers[].providerRef, so two separate edges converge on ModelProvider.](../diagrams/crd-references-model-gates.svg)

**Who may call a model.** The two red edges into ModelProvider are **separate gates, not one list**. An Agent may call a provider only when its AgentClass lists that provider in `allowedProviders` (the platform team's decision) **and** the Agent itself names it in `providers[].providerRef` (the developer's decision) **and** the ModelProvider admits the Agent's namespace in `allowedNamespaces`. That third gate is not a reference, so no figure can draw it; it is [rule 4](../resources/validation-and-defaulting.md#cross-resource-validation), beside rules 3 and 5 for the two edges shown.

![The tool-provider gates, the same shape: AgentClass names ToolProviders through spec.allowedToolProviders, and Agent and AgentTask each name a ToolProvider through spec.tools[].providerRef.](../diagrams/crd-references-tool-gates.svg)

**Who may call a tool.** The identical shape governs tools: `allowedToolProviders` on the class, `tools[].providerRef` on the workload, and `allowedNamespaces` on the ToolProvider, as rules 35 to 37.

![References inside the cluster scope. ModelProvider points at other ModelProviders through spec.fallback, and ModelProvider and ToolProvider each point at a Secret in kaalm-system through spec.credentialsRef. The Secret is not a Kaalm CRD.](../diagrams/crd-references-providers.svg)

**What a provider needs.** A provider names its credential Secret in `kaalm-system`, never in a workload namespace, which is how the credential stays out of every agent's reach; a ToolProvider whose server needs no credential omits the reference. The grey self-edge is ModelProvider's alone: `spec.fallback` points at ModelProviders, so a provider chain is a tree that the gateway walks depth-first, and rule 11 rejects cycles in it. All of these are properties of the graph, so the per-resource specs cannot show them.

## The two gateways

Both gateways are call surfaces of a single replicated Deployment in `kaalm-system`. The Deployment's two TLS listeners split by exposure: the **cluster listener** (`:8443`) serves in-cluster callers (the LLM Gateway's proxy paths, the [tool plane](../gateways/tool-plane.md)'s broker, and the internal mTLS endpoints), the user listener (`:8080`) serves the Ingress-fronted channel surface, and a separate internal health port serves kubelet probes.

The **LLM Gateway** is the model-proxy subsystem, served on the cluster listener. It mediates all agent-to-provider traffic in the outbound direction, agent to LLM provider, and provides spend visibility, budget guardrails (soft by default, hard opt-in), rate limiting, fallback routing, and credential isolation. Using it is optional per agent.

The **User Gateway** listens on `:8080` (TLS) and handles the inbound direction plus its return path: it receives webhook messages from outside, normalizes them into a standard envelope, delivers them to the agent's HTTP endpoint, and hosts the async response polling endpoint. It supports three channel types, all inbound HTTP: the generic webhook, Discord, and WhatsApp.

Which endpoints live on which listener, and what authentication each path requires, is consolidated in [the gateway overview](../gateways/overview.md).

## Adoption tiers

Kaalm can be adopted at two depths, and behaviors throughout this book branch on the tier a workload belongs to.

In the **gateway-only tier**, existing workloads keep their existing Deployments and point their LLM traffic at the gateway, authenticating with projected ServiceAccount tokens, to gain spend tracking. They have no Agent, AgentTask, or AgentChannel resources of their own (the chart still ships a default `standard` AgentClass, but no workload references it). Provider access is gated by `ModelProvider.allowedNamespaces` alone, since there is no Agent, AgentTask, or AgentClass to consult. Egress is the platform team's responsibility: Kaalm does not synthesize a NetworkPolicy for these Pods, so budget enforcement, rate limits, and provider-access gating only hold if the platform team applies their own policies in those namespaces denying egress to provider IPs except through the gateway.

In the **full lifecycle tier**, Agents, AgentTasks, and AgentChannels are managed by the operator, with hibernation, wake-on-demand, and per-Pod mTLS with cert-manager-issued certificates. The chart-level framing (Helm values, prerequisites, install order) is in [tiers](../operations/deployment.md#tiered-on-ramp).

## Workload identity in brief

This section is a summary. The full mechanics, including every enforcement rule, live in [workload identity](../gateways/llm/workload-identity.md).

The gateway authenticates calling workloads in one of two modes, mapped to the two tiers. **Mode 1 is an mTLS client certificate**, the primary, zero-config path for Kaalm-managed Agent and AgentTask Pods. The gateway verifies the certificate against the Kaalm CA and reads identity from its SAN, which comes in two shapes: `{name}.{namespace}.svc.cluster.local` for Agents (exactly 5 DNS labels) and `{name}.{namespace}.task.kaalm.io` for AgentTasks (exactly 5 labels). The suffix is what distinguishes an Agent from an AgentTask on the wire; the exact label count is enforced separately, as defense in depth against dotted names.

**Mode 2 is a ServiceAccount bearer token**, the path for gateway-only-tier workloads. The caller presents a projected ServiceAccount token, the gateway validates it with the Kubernetes `TokenReview` API, and the validated username yields the caller's namespace. Kaalm-managed Pods cannot use this mode; for them, mTLS is the only accepted credential.

In both modes a source-IP cross-check confirms, as defense in depth, that the Pod at the request's source IP is in the namespace authentication identified.

## Lifecycle phases

An **Agent** in persistent mode moves through these phases:

- **Pending**: initial phase, references being validated.
- **Provisioning**: the controller is creating the Pod, PVC, and Service.
- **Running**: the Pod is Ready and serving.
- **Idle**: no activity observed for the idle timeout.
- **Hibernating**: the Pod is being scaled to zero.
- **Hibernated**: no Pod exists; the PVC is retained.
- **Resuming**: the Pod is being scaled back up after a wake trigger.
- **Degraded**: the Agent's spec is irreconcilable with its AgentClass or ModelProvider.
- **Failed**: an unrecoverable error occurred.
- **Terminating**: deletion was requested; finalizers are running.

On entering `Degraded`, the controller records the prior phase in `status.preDegradedPhase` and restores it once the mismatch resolves.

An **AgentTask** moves through these phases:

- **Pending**: initial phase, references being validated.
- **Provisioning**: the controller is creating the Pod and PVC.
- **Running**: the Pod is Ready and the task is executing.
- **Completing**: a brief state collecting artifacts and scheduling teardown.
- **Succeeded**: the task reported success.
- **Failed**: the task failed (possibly retryable).
- **TimedOut**: the wall-clock timeout hit before completion.
- **Terminating**: the task was deleted or its TTL expired.

The full state machines, transition triggers, and edge cases are in [Agent lifecycle](../controller/agent-lifecycle.md) and [AgentTask lifecycle](../controller/task-lifecycle.md).

## Response modes

Each AgentChannel selects a `responseMode`: **sync** (the default), where the webhook caller holds the connection open and receives the agent's reply in the HTTP response, or **async**, where the gateway immediately returns `202 Accepted` with a `requestId` and runs delivery and response handling in the background. Async replies reach the caller by callback or by polling with that `requestId`; the schemas are in [async responses](../gateways/api/async-responses.md).
