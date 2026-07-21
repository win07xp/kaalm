# Operator Structure

The Kaalm operator is the control plane: it turns the declarative CRDs in [Resource Overview](../resources/overview.md) into running Pods, Services, Secrets, and status. This chapter describes it from the inside out. The [Reconcilers](reconcilers.md) page walks through the reconcile steps for each of the five CRDs, then [Agent Lifecycle](agent-lifecycle.md), [Hibernation and Wake](hibernation-and-wake.md#hibernation-mechanics), [Change Propagation](change-propagation.md#agentclass-change-handling), [AgentTask Lifecycle](task-lifecycle.md), and [Finalizers](finalizers.md) cover the state machines those steps drive. [Errors, Events, and Testing](operations.md#error-handling) closes the chapter with the operator's failure handling, emitted Events, metrics, and test strategy.

This page covers what the binary is, what it serves, and how often it reconciles.

## The Binary

The operator is a single binary built with `controller-runtime` (kubebuilder scaffolding is fine but not required). It hosts:

- Five reconcilers: `AgentClassReconciler`, `ModelProviderReconciler`, `AgentReconciler`, `AgentTaskReconciler`, `AgentChannelReconciler`.
- An activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) called by the gateway to trigger hibernated agent wake-up. This endpoint is exposed via a ClusterIP Service (`kaalm-controller.kaalm-system.svc.cluster.local`, default port 9443).
- A health/readiness endpoint (`/healthz`, `/readyz`) on the same Service. The listener serves TLS with `tls.Config.ClientAuth = VerifyClientCertIfGiven` so kubelet (which presents no client cert) can complete the handshake; per-path middleware enforces mTLS-with-SAN only on `/v1/activate`, so the probes are unauthenticated at the path level.
- A metrics endpoint (Prometheus format) exposing controller internals (reconcile counts, errors, queue depth). It listens on `:8080/metrics`, the standard controller-runtime port, and is documented in [Observability](operations.md#observability).

## Deployment and Leader Election

The operator runs as a Deployment in `kaalm-system` with leader election enabled. Two replicas are recommended for availability; only the leader actively reconciles. Non-leader replicas still serve `/metrics`, but emit only the controller-runtime defaults at zero counts (they hold no active reconciler queues). Dashboards should therefore aggregate across replicas or filter on the leader by Pod label, otherwise the idle replica's zeros will look like real data.

### Activator Handler (Served on Every Replica)

The `POST /v1/activate/{namespace}/{agentName}` handler is served on every controller replica, not only the leader. The handler authenticates the caller (see [Internal Endpoint Authentication](../security/rbac.md#internal-endpoint-authentication)), loads the target Agent, and patches `kaalm.io/wake=true` onto it via the apiserver. The leader's existing Agent watch fires and runs the manual-wake path in [AgentReconciler](reconcilers.md#agentreconciler) step 9, which transitions the Agent to `Resuming`.

The reason for splitting the work this way: it avoids any leader-aware Service endpoint plumbing. A non-leader replica that handles the POST still drives the wake correctly, because the wake travels through the apiserver rather than through the replica that received the request. Non-leader replicas do not run reconcilers; they only need apiserver patch access for Agents, which the controller ServiceAccount already has.

## No Admission Webhooks

Field-level validation uses CEL expressions in CRD schemas (`x-kubernetes-validations`). Cross-resource validation runs at reconcile time and is surfaced as `Ready=False` status conditions with descriptive messages. This eliminates the availability risk of a webhook server on the apiserver request path: a wedged webhook would otherwise block writes to the resources it guards.

## Controller TLS

The activator and health/readiness endpoints on port 9443 serve HTTPS using a cert-manager-issued `Certificate` named `kaalm-controller-tls` in `kaalm-system`. The chart installs this `Certificate` with `issuerRef` → `kaalm-ca-issuer` (the same `ClusterIssuer` that signs the gateway cert). Its SAN set covers `kaalm-controller.kaalm-system.svc.cluster.local`, `kaalm-controller.kaalm-system.svc`, and `localhost`.

Usages are `server auth` **and** `client auth`, because the certificate is used in both directions:

- **Server:** the controller serves TLS for inbound activator and probe traffic.
- **Client:** the controller presents the same cert when dialing the gateway's `/v1/activity` and `/v1/channels/health` endpoints (see [AgentReconciler](reconcilers.md#agentreconciler) step 8 for `/v1/activity` and [AgentChannelReconciler](reconcilers.md#agentchannelreconciler) step 4 for `/v1/channels/health`).

The gateway and controller mutually verify against the Kaalm CA (`kaalm-ca`). The listener uses `tls.Config.ClientAuth = VerifyClientCertIfGiven` so a single port can carry the mTLS-required activator and the cert-less kubelet probes; per-path HTTP middleware enforces mTLS-with-SAN on `/v1/activate` and lets `/healthz` and `/readyz` through unauthenticated. cert-manager rotates this cert continuously, so no operator code is involved in its lifecycle. The full bidirectional trust chain (CA, issuers, and how gateway and controller certs relate) is described in [In-Cluster TLS](../security/tls.md#in-cluster-tls); see also [TLS on the LLM Gateway Listener](../gateways/llm/listener-tls.md).

## Reconcile Interval and Performance

- Default reconcile requeue: 5 minutes (for periodic health/budget re-evaluation when no events trigger).
- Event-driven reconciles: immediate.
- AgentTask timeout checking: requeue at `startTime + timeout + small jitter` when in Running state. `status.startTime` is stamped at the Provisioning→Running transition (Pod Ready), so scheduling and image-pull time never count against `spec.completion.timeout`; a task stuck before Running is bounded separately by the fixed 5-minute provisioning deadline (see [AgentTask](task-lifecycle.md)).
- Idle detection: requeue at `lastActivityTime + idleTimeout` when in Running state.

The operator should handle 1000+ Agents and AgentTasks per cluster without issue. Use indexed caches for all cross-resource lookups.
