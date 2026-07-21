# LLM Gateway

The LLM Gateway is the shared cluster-level component responsible for mediating LLM traffic between agent containers and upstream providers. It is where spend tracking, budget guardrails, rate limiting, fallback, and credential isolation live.

The pages in this chapter follow a single request through the gateway. [Request Handling](request-handling.md#request-flow) walks the end-to-end request flow, streaming, and model identification; [Workload Identity](workload-identity.md) covers how the gateway establishes which namespace a caller belongs to; and [Listener TLS](listener-tls.md) covers the certificates and per-path client-auth rules on the listener itself. [Provider Routing and Adapters](provider-routing.md) covers picking a ModelProvider and translating to its wire format, [Budgets and Rate Limits](budgets-and-rate-limits.md#budget-state-management) covers spend accounting and throttling, and [Fallback Logic](fallback.md) covers what happens when the chosen provider fails. [LLM Gateway Operations](operations.md#gateway-readiness) collects readiness, observability, and failure modes.

For the User Gateway (channel message delivery, activator, activity tracking), see [User Gateway](../user/overview.md). For the HTTP endpoint contracts agents use, see [HTTP API](../api/overview.md).

## Why a Shared Gateway

Agent containers need to call LLM providers. Doing this naively (agents holding API keys and calling providers directly) gives up all centralized control: no spend visibility, no fallback, no per-namespace accounting, and every agent image must embed credentials. Kaalm interposes on LLM traffic to deliver ModelProvider guarantees.

Similarly, agents need to be reachable from user-facing platforms (Discord, WhatsApp, webhooks). Rather than requiring each developer to build their own webhook receiver and protocol adapter, Kaalm provides a shared channel ingress point.

### Architecture Option Analysis

Three architectural options were evaluated for the LLM proxy component.

**Option A: Per-Agent Sidecar Proxy**

A small proxy container runs as a sidecar in every Agent Pod.

Pros: Small failure domain; no shared state contention at request time.

Cons: Kubernetes `NetworkPolicy` cannot enforce per-container rules within a Pod. The sidecar and agent container share the same network namespace and the same IP, so NetworkPolicy cannot prevent the agent container from making direct egress calls to LLM providers if the node allows it. Credentials must be copied into user namespaces. Budget state requires eventual-consistent replication across all sidecars.

**Option B: Namespace-Scoped Gateway (not selected)**

One proxy Deployment per namespace.

Cons: More complex operator (gateway lifecycle per namespace), harder to reason about at scale, still requires per-namespace credential propagation.

**Option C: Cluster-Wide Gateway (SELECTED for v1)**

One replicated proxy Deployment in `kaalm-system`.

Pros: Credentials never leave `kaalm-system`. NetworkPolicy cleanly isolates agent Pods (deny all egress to LLM provider IPs; allow egress to the gateway Service, which is cross-Pod and fully enforceable). Budget state is centralized in one component: cross-replica reconciliation reduces to a single per-provider ConfigMap exchange with a bounded staleness window (see [Budget State Management](budgets-and-rate-limits.md#budget-state-management)), rather than the per-sidecar eventual-consistency mesh Option A would require. The gateway also serves as the activator for hibernated agents. SPOF concern is addressed with 2-3 replicas, a PodDisruptionBudget (`minAvailable: 1`), and `maxUnavailable: 1` rolling updates.

**v1 ships with Option C.** The per-Pod sidecar pattern was rejected because the same-Pod network namespace sharing undermines the credential isolation guarantee on standard Kubernetes clusters without a service mesh.

## Gateway Architecture

The gateway process hosts both listeners and the activator. The LLM Gateway listener runs the request pipeline shown below; the User Gateway listener and the activator are covered in [User Gateway](../user/overview.md).

![The Kaalm Gateway process in kaalm-system, containing three subsystems. The LLM Gateway Listener runs a request pipeline that descends from request validator to model allow-list check, namespace access check, budget check and policy enforcement, rate limiter, upstream router with fallback, and provider adapter, which egresses to the LLM Provider APIs. The provider response returns into a second column: response relay, then token counter (post-call), then spend state update. The User Gateway Listener and the Activator / Activity Store share the same process and are covered in the User Gateway chapter.](../../diagrams/llm-gateway-pipeline.svg)

Reading the diagram: the left column is the outbound request path and the right column is the return path. The provider adapter is the egress point, and everything after the response relay is post-call accounting, which is why token counting and the spend update sit outside the request's own latency path.
