# Multi-Tenancy and Adoption Tiers

Agentry is built for a shared cluster: many tenants, one platform team, one set of provider credentials. Two questions follow from that. First, **how deeply does a workload adopt Agentry?** Some workloads want nothing but a metered path to an LLM; others want the full managed lifecycle. Second, **what keeps one tenant from reaching another's traffic, budget, or agents?**

This page answers both, in that order. The adoption tier a workload belongs to changes which isolation layers apply to it, so the tiers come first.

A note on vocabulary: the personas page described a "two-tier model" meaning the split between Priya's cluster-scoped resources and Dev's namespaced ones. That is a different axis from the **adoption tiers** below, which describe how much of Agentry a given workload uses. Both splits are real; they are just not the same split.

## Adoption Tiers

Agentry can be adopted at two depths. This is not a cosmetic distinction: several behaviors throughout the design branch on which tier a workload belongs to, and the sections below say so explicitly where they do.

### Gateway-only tier

Existing workloads call the gateway via projected ServiceAccount tokens, getting LLM traffic and spend tracking, and nothing else. They own no Agent, AgentTask, or AgentChannel resources. (The chart still ships a default `standard` AgentClass, but in this tier no workload references it.)

Because there is no Agent, AgentTask, or AgentClass to consult, provider access is gated by `ModelProvider.allowedNamespaces` alone.

**Egress is the platform team's responsibility.** Agentry does not synthesize a NetworkPolicy for these Pods. Budget enforcement, rate limits, and provider-access gating are all enforced *at the gateway*, so they only hold if traffic actually goes through the gateway. That means the platform team must apply their own NetworkPolicies in those namespaces, denying egress to provider IPs except via the gateway. Without those policies a Pod can simply call the provider directly and every gateway-side control is bypassed. See [Deployment Model](system-architecture.md#deployment-model).

### Full lifecycle tier

Agents, AgentTasks, and AgentChannels managed by the operator, with hibernation, wake-on-demand, and per-Pod mTLS via cert-manager-issued certificates.

Here the egress boundary is not left to the platform team: because these Pods are Agentry-managed, the controller synthesizes their NetworkPolicies itself (see [NetworkPolicy as the cross-tenant boundary](#networkpolicy-as-the-cross-tenant-boundary) below).

The chart-level framing (Helm values, prerequisites, install order) is in [Deployment Model](system-architecture.md#deployment-model).

## Multi-tenancy

v1 assumes a single platform team owns the cluster-scoped policy resources (AgentClass, ModelProvider) while individual tenants operate at namespace boundaries via Agent, AgentTask, and AgentChannel. See [Custom Resources](core-concepts.md#the-five-custom-resources) for the scope split.

Tenant isolation is layered. No single one of the layers below is the boundary; they compose, and each subsection names what it is responsible for.

### Trust tiers

[Trust Model](../security/model.md#trust-model) defines four tiers:

1. **Cluster admin.**
2. **Platform engineer**, trusted with provider credentials.
3. **Agent developer**, trusted within namespace guardrails. This assumption is load-bearing: it is exactly what the synthesized NetworkPolicy depends on, since a developer who can rewrite the policy can undo it.
4. **The agent container itself**, untrusted.

Platform teams that need to treat developers as untrusted should restrict `networkpolicies` create/patch in user namespaces via cluster RBAC.

### RBAC layering

The operator ServiceAccount holds the cluster-scoped surface (CRD watches, child-resource management). The gateway ServiceAccount is scoped to `agentry-system`, plus dynamic per-channel and per-task `Role`s with `resourceNames`-bounded access in user namespaces. See [RBAC Model](../security/rbac.md).

### Provider access gating

For full-lifecycle workloads, an Agent or AgentTask can use a provider only when **all three** of the following admit it:

| Layer | Set by |
|---|---|
| `Agent.spec.providers` / `AgentTask.spec.providers` | The workload author: the per-workload declared usage list, gateway-enforced |
| `AgentClass.allowedProviders` | The platform team, on the class |
| `ModelProvider.allowedNamespaces` | The platform team, on the provider |

All three layers must pass. Gateway-only-tier callers have no Agent, AgentTask, or AgentClass to consult, so they are gated by `ModelProvider.allowedNamespaces` alone.

Separately, the requested model must exist in `ModelProvider.spec.models`. That check is a model-resolution prerequisite enforced at the gateway, not a tenancy boundary, and it applies to callers in both tiers: see [The Agentry Gateway](../gateways/overview.md).

See [AgentClass](../resources/agentclass.md) and [Cross-Resource Validation](../resources/validation-and-defaulting.md#cross-resource-validation) (rules 4-5).

### Per-namespace throughput and spend isolation

Gateway rate-limit token buckets are keyed on (namespace, model) against the cluster-wide ceiling in `ModelProvider.spec.rateLimits`, and budget counters track spend per namespace. So one tenant cannot exhaust a shared provider's rate-limit or spend ceiling on behalf of another.

Both controls apply to gateway-only-tier callers as well, because both are enforced at the gateway from the namespace identified on every request; the gateway derives that namespace from the caller's workload identity, whichever auth mode it used (see [Namespace identification](../gateways/llm/workload-identity.md)).

See [The Agentry Gateway](../gateways/overview.md) for the per-request enforcement points, and [Multi-replica state](../gateways/overview.md#multi-replica-state) for the cross-replica reconciliation strategies (both counters live in gateway replicas, so they need reconciling).

### NetworkPolicy as the cross-tenant boundary

Synthesized per-Pod NetworkPolicies bound Agentry-managed Pods' egress to the AgentClass-permitted set:

- **LLM provider traffic is gateway-mediated**, with no direct provider egress. This is what makes the spend and rate-limit controls above unbypassable for these Pods.
- **Plus any AgentClass-allowed direct egress** (`allowedCIDRs`, `allowedHosts`, e.g. MCP servers).

See [Per-Agent and Per-Task Child Resources](../runtime/child-resources.md).

Gateway-only-tier Pods are not Agentry-managed and inherit no synthesized policy: see the caveat under [Adoption Tiers](#adoption-tiers).

### Webhook path namespace-prefixing

AgentChannel webhook paths must be namespace-prefixed (`/channels/{namespace}/...`), so cross-tenant path collisions are impossible at the routing layer: a tenant cannot register a path that captures another tenant's inbound traffic.

The rule is enforced twice: at reconcile time (`Ready=False, reason=InvalidPath`) and re-checked by the gateway at path registration ([rule 15](../resources/validation-and-defaulting.md#cross-resource-validation)). It is not enforced in CRD CEL, because CEL cannot read `metadata.namespace`, which is exactly what the rule needs to compare against.

`AgentChannel.spec.agentRef` is name-only and binds to an Agent in the AgentChannel's own namespace. Cross-namespace channel-to-Agent binding is not supported. See [AgentChannel](../resources/agentchannel.md).

### Tenant-level resource limits

Per-Pod resource caps live in `AgentClass.spec.maxLimits`. That bounds any single Pod, not a tenant's total footprint. Agentry does not synthesize ResourceQuota or LimitRange: platform teams should apply standard namespace-level ResourceQuota and LimitRange to bound aggregate tenant footprint.

## Where to go next

The cross-tenant attack surface and the per-attack mitigation walkthrough are in [Threat Model](../security/threat-model.md).
