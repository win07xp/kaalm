# Multi-tenancy and adoption tiers

Kaalm is built for a shared cluster: many tenants, one platform team, one set of provider credentials. Two questions follow. How deeply does a workload adopt Kaalm? Some want nothing but a metered path to an LLM; others want the full managed lifecycle. And what keeps one tenant from reaching another's traffic, budget, or agents?

This page answers both, in that order, because the adoption tier a workload belongs to changes which isolation layers apply to it.

A note on vocabulary: [Personas](personas.md) describes a two-tier model, the split between Priya's cluster-scoped resources and Dev's namespaced ones. The adoption tiers on this page are a different axis: how much of Kaalm a given workload uses.

## Adoption tiers

Kaalm can be adopted at two depths, and several behaviors in the design branch on which one a workload belongs to.

### Gateway-only tier

Existing workloads call the gateway with projected ServiceAccount tokens and get LLM traffic with spend tracking, budgets, and rate limits, and nothing else. They own no Agent, AgentTask, or AgentChannel. The chart ships a `standard` AgentClass, but no workload in this tier references it.

Because there is no Agent, AgentTask, or AgentClass to consult, provider access is gated by `ModelProvider.allowedNamespaces` alone.

**Egress is the platform team's responsibility.** Kaalm synthesizes no NetworkPolicy for these Pods. Budgets, rate limits, and provider gating are enforced at the gateway, so they hold only if traffic goes through the gateway, and the platform team must write NetworkPolicies in those namespaces that deny egress to provider addresses except through it. Without them a Pod can call the provider directly and every gateway-side control is bypassed.

### Full lifecycle tier

The operator manages Agents, AgentTasks, and AgentChannels, with hibernation, wake-on-demand, and per-Pod mTLS with cert-manager-issued certificates. The egress boundary is not left to the platform team: the controller writes each Pod's NetworkPolicy ([NetworkPolicy as the cross-tenant boundary](#networkpolicy-as-the-cross-tenant-boundary)).

The chart-level setup for both tiers, Helm values, prerequisites, and install order, is [Tiered on-ramp](../operations/deployment.md#tiered-on-ramp).

## Multi-tenancy

Kaalm assumes a single platform team manages the cluster-scoped policy resources (AgentClass, ModelProvider, ToolProvider) while tenants operate at namespace boundaries through Agent, AgentTask, and AgentChannel ([The custom resources](core-concepts.md#the-custom-resources)).

Tenant isolation is layered. No single layer is the boundary; they compose, and each subsection names what it is responsible for.

### Trust tiers

[Trust model](../security/model.md#trust-model) defines four trust tiers, from cluster admin down to the untrusted agent container. The one that matters here is the agent developer, trusted within namespace guardrails: the synthesized NetworkPolicy depends on that assumption, because a developer who can rewrite the policy can undo it. A platform team that treats developers as untrusted restricts `networkpolicies` create and patch in user namespaces with cluster RBAC.

### RBAC layering

The controller ServiceAccount holds the cluster-scoped surface: CRD watches and child-object management. The gateway ServiceAccount holds cluster-wide reads of the Kaalm kinds and Pods, but its credential reach is `kaalm-system` plus the per-channel and per-task Roles with `resourceNames`-bounded access in user namespaces ([RBAC and authentication](../security/rbac.md)).

### Provider access gating

For a full-lifecycle workload, an Agent or AgentTask can use a provider only when all three of these admit it:

| Layer | Set by |
|---|---|
| `Agent.spec.providers` or `AgentTask.spec.providers` | The workload author |
| `AgentClass.allowedProviders` | The platform team, on the class |
| `ModelProvider.allowedNamespaces` | The platform team, on the provider |

Each is enforced twice: at reconcile time as rules 3 to 5, and at request time by the gateway. A gateway-only-tier caller has no Agent, AgentTask, or AgentClass to consult, so `ModelProvider.allowedNamespaces` alone gates it.

Separately, the requested model must exist in `ModelProvider.spec.models`. That check is model resolution, not a tenancy boundary, and it applies to both tiers ([Model identification](../gateways/llm/request-handling.md#model-identification)).

![The gate chain the gateway applies to a request carrying a qualified model name, in the order it runs. Identify the caller from an mTLS certificate SAN (full lifecycle tier) or a TokenReview-verified bearer token (gateway-only tier), then cross-check the source IP against a Pod in that namespace (401 unauthorized). A caller with a workload passes Gate A: the workload and its class exist and the providerRef is in both spec.providers and the class's allowedProviders (403 access_denied); a gateway-only caller skips it. Then the providerRef must name a ModelProvider (400 invalid_request), Gate B checks the namespace against allowedNamespaces (403 access_denied), and Gate C resolves the model (400 invalid_request). Budget, rate limit, and forwarding follow.](../diagrams/provider-access-gates.svg)

Gate A is the only gate that needs a workload and a class to read, so it is the only one a gateway-only caller skips, and it runs before the provider is looked up so that a full-lifecycle caller cannot learn which provider names exist without passing it. Gates A and B are tenancy decisions and answer `403 access_denied`; an unknown provider name and Gate C are resolution failures and answer `400 invalid_request`. The tool plane applies the same chain, gate for gate, with the class's `allowedToolProviders` in place of `allowedProviders` ([Grants](../gateways/tool-plane.md#grants)). What follows the chain, budgets, the rate limiter, and the fallback walk, is on [Request handling](../gateways/llm/request-handling.md#request-flow).

### Per-namespace throughput and spend isolation

Gateway rate-limit buckets are keyed on (namespace, model) against the cluster-wide ceiling in `ModelProvider.spec.rateLimits`, and budget counters track spend per namespace, so one tenant cannot exhaust a shared provider's rate limit or budget on behalf of another. Both controls apply to gateway-only-tier callers too, because both are enforced at the gateway from the namespace identified on every request, whichever auth mode identified it ([Workload identity](../gateways/llm/workload-identity.md)). Both counters live in gateway replicas and are reconciled across them ([Multi-replica state](../gateways/overview.md#multi-replica-state)).

### NetworkPolicy as the cross-tenant boundary

The synthesized per-Pod NetworkPolicy bounds a Kaalm-managed Pod's egress to the gateway, cluster DNS, and the class's `allowedCIDRs`, and on Cilium a second policy adds the class's `allowedHosts`:

- **LLM and tool traffic is gateway-mediated**, with no direct provider egress. This is what makes the spend and rate-limit controls in the previous section unbypassable for these Pods. The [tool plane](../gateways/tool-plane.md) gives MCP traffic the same treatment.
- **`allowedCIDRs` and `allowedHosts` are the only direct egress.** `allowedCIDRs` is governed at the IP level on every CNI. `allowedHosts` is governed by host name, through a CiliumNetworkPolicy, and only on Cilium; elsewhere it is ignored.

The rule set is on [Child resources](../runtime/child-resources.md#what-the-synthesized-networkpolicy-protects). Gateway-only-tier Pods are not Kaalm-managed and inherit no policy ([Adoption tiers](#adoption-tiers)).

### Webhook path namespace-prefixing

An AgentChannel path must begin with `/channels/{namespace}/` for its own namespace, so cross-tenant path collisions are impossible at the routing layer: a tenant cannot register a path that captures another tenant's inbound traffic. The rule is enforced at reconcile time (`Ready=False, reason=InvalidPath`) and checked again by the gateway on every request it resolves ([rule 15](../resources/validation-and-defaulting.md#cross-resource-validation)); CEL cannot express it, because CEL cannot read `metadata.namespace`. `AgentChannel.spec.agentRef` is name-only and binds to an Agent in the channel's own namespace; there is no cross-namespace binding ([AgentChannel](../resources/agentchannel.md)).

### Tenant-level resource limits

Per-Pod caps live in `AgentClass.spec.resources.maxLimits`. That bounds any single Pod, not a tenant's total footprint. Kaalm synthesizes no ResourceQuota or LimitRange; platform teams bound aggregate footprint with the standard namespace-level objects.

## Where to go next

The cross-tenant attack surface and the per-attack mitigations are in [Threat model](../security/threat-model.md).
