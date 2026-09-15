# Operator structure

The Kaalm operator is the control plane: it turns the declarative CRDs in [Resource Overview](../resources/overview.md) into running Pods, Services, Secrets, and status. This chapter describes it from the inside out. The [Reconcilers](reconcilers.md) page walks through the reconcile steps for each of the six CRDs, then [Agent lifecycle](agent-lifecycle.md), [Hibernation and wake](hibernation-and-wake.md#hibernation-mechanics), [Change propagation](change-propagation.md#agentclass-change-handling), [AgentTask lifecycle](task-lifecycle.md), and [Finalizers](finalizers.md) cover the state machines those steps drive. [Errors, events, and testing](operations.md#error-handling) closes the chapter with the operator's failure handling, emitted Events, metrics, and test strategy.

This page covers what the binary is, what it serves, and how often it reconciles.

## The binary

The operator is a single binary built with `controller-runtime` (kubebuilder scaffolding is fine but not required). It hosts:

- Six reconcilers: `AgentClassReconciler`, `ModelProviderReconciler`, `ToolProviderReconciler`, `AgentReconciler`, `AgentTaskReconciler`, `AgentChannelReconciler`.
- An activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) called by the gateway to trigger hibernated agent wake-up. This endpoint is exposed through a ClusterIP Service (`kaalm-controller.kaalm-system.svc.cluster.local`, default port 9443).
- A health/readiness endpoint (`/healthz`, `/readyz`) on the same Service. The listener serves TLS with `tls.Config.ClientAuth = VerifyClientCertIfGiven` so kubelet (which presents no client cert) can complete the handshake; per-path middleware enforces mTLS-with-SAN only on `/v1/activate`, so the probes are unauthenticated at the path level.
- A metrics endpoint (Prometheus format) exposing controller internals (reconcile counts, errors, queue depth). It listens on `:8080/metrics`, the standard controller-runtime port, and is documented in [Observability](operations.md#observability).
- A CRD conversion webhook (`POST /convert`) on a listener of its own (port 9444, the `conversion` port of the same Service), served on every replica. It translates the six CRDs between `v1alpha1` and `v1beta1` for the apiserver and enforces nothing; see [API versioning and deprecation](../operations/api-versioning.md#where-the-conversion-webhook-runs).

## Deployment and leader election

The operator runs as a Deployment in `kaalm-system` with leader election enabled. Two replicas are recommended for availability; only the leader actively reconciles. Non-leader replicas still serve `/metrics`, but emit only the controller-runtime defaults at zero counts (they hold no active reconciler queues). Dashboards should therefore aggregate across replicas or filter on the leader by Pod label, otherwise the idle replica's zeros will look like real data.

![Two controller replicas behind the kaalm-controller Service. Both serve the :9443 activator and probes, the :9444 conversion webhook, and :8080 metrics; only replica A, which holds the leader Lease, runs the six reconcilers, while replica B's reconcilers idle and its metrics read zero. The gateway POSTs /v1/activate to the Service, which round-robins to either replica; the replica that takes the call patches kaalm.io/wake=true on the Agent through the Kubernetes API, the leader's Agent watch fires, and the leader reconciles the Agent from Hibernated to Resuming and recreates the Pod. Both replicas hold or wait on the leader Lease in the API.](../diagrams/controller-replicas.svg)

Reading the diagram: the leader is not the replica that answered the gateway. The wake reaches the leader because it is written into the Agent, and the apiserver, not the replica that took the call, is the message bus.

### Activator handler (served on every replica)

The `POST /v1/activate/{namespace}/{agentName}` handler is served on every controller replica, not only the leader. The handler authenticates the caller (see [Internal endpoint authentication](../security/rbac.md#internal-endpoint-authentication)), loads the target Agent, and patches `kaalm.io/wake=true` onto it through the apiserver. The leader's existing Agent watch fires and runs the manual-wake path in [AgentReconciler](reconcilers.md#agentreconciler) step 9, which transitions the Agent to `Resuming`.

The reason for splitting the work this way: it avoids any leader-aware Service endpoint selection. A non-leader replica that handles the POST still drives the wake correctly, because the wake travels through the apiserver rather than through the replica that received the request. Non-leader replicas do not run reconcilers; they only need apiserver patch access for Agents, which the controller ServiceAccount already has.

## No admission webhooks

Field-level validation uses CEL expressions in CRD schemas (`x-kubernetes-validations`). Cross-resource validation runs at reconcile time and is surfaced as `Ready=False` status conditions with descriptive messages. This eliminates the availability risk of a webhook server on the apiserver request path: a wedged webhook would otherwise block writes to the resources it guards.

The controller does serve one webhook, and the distinction matters: the **CRD conversion webhook** that translates the six kinds between `v1alpha1` and `v1beta1`. It is not an admission webhook. It validates and mutates nothing, it is consulted only when a request's version differs from the version an object is stored at, and after storage migration that means only clients still sending the deprecated `v1alpha1`. Kaalm's own components speak the storage version and never depend on it, so a wedged conversion path cannot block the control plane, the gateway, or the console; it can only inconvenience a legacy client until a replica answers. [API versioning and deprecation](../operations/api-versioning.md#what-depends-on-the-webhook) states the availability argument in full.

## Controller TLS

The activator and health/readiness endpoints on port 9443 serve HTTPS using a cert-manager-issued `Certificate` named `kaalm-controller-tls` in `kaalm-system`. The chart installs this `Certificate` with an `issuerRef` naming `kaalm-ca-issuer` (the same `ClusterIssuer` that signs the gateway cert). Its SAN set covers `kaalm-controller.kaalm-system.svc.cluster.local`, `kaalm-controller.kaalm-system.svc`, and `localhost`. The conversion webhook listener on port 9444 serves the same certificate, server side only: the apiserver verifies it against the Kaalm CA that cert-manager's cainjector keeps in each CRD's `caBundle` ([API versioning and deprecation](../operations/api-versioning.md#where-the-conversion-webhook-runs)).

Usages are `server auth` **and** `client auth`, because the certificate is used in both directions:

- **Server:** the controller serves TLS for inbound activator and probe traffic, and for the apiserver's conversion calls on port 9444.
- **Client:** the controller presents the same cert when dialing the gateway's `/v1/activity` and `/v1/channels/health` endpoints (see [AgentReconciler](reconcilers.md#agentreconciler) step 8 for `/v1/activity` and [AgentChannelReconciler](reconcilers.md#agentchannelreconciler) step 4 for `/v1/channels/health`).

The gateway and controller mutually verify against the Kaalm CA (`kaalm-ca`). The listener uses `tls.Config.ClientAuth = VerifyClientCertIfGiven` so a single port can carry the mTLS-required activator and the cert-less kubelet probes; per-path HTTP middleware enforces mTLS-with-SAN on `/v1/activate` and lets `/healthz` and `/readyz` through unauthenticated. cert-manager rotates this cert continuously, so no operator code is involved in its lifecycle. The full bidirectional trust chain (CA, issuers, and how gateway and controller certs relate) is described in [In-cluster TLS](../security/tls.md#in-cluster-tls); see also [TLS on the cluster listener](../gateways/listener-tls.md).

## Reconcile interval and performance

Every reconciler runs on events first; each also requeues on a cadence of its own, and there is no shared periodic interval:

- Agent: every 15 seconds while `Running` or `Idle` (the activity cache window, so idle and hibernation transitions are evaluated against fresh activity data), every 30 seconds while a pre-Pod gate holds, and every 5 seconds while waiting on the certificate.
- AgentTask: at `startTime + timeout` while `Running`. `status.startTime` is stamped at the Provisioning to Running transition (Pod Ready), so scheduling and image-pull time never count against `spec.completion.timeout`; a task stuck before Running is bounded separately by the fixed 5-minute provisioning deadline (see [AgentTask lifecycle](task-lifecycle.md)).
- ModelProvider and ToolProvider: at the provider's `healthCheck.intervalSeconds` (default 60), which is also when the budget reducer runs.
- AgentChannel: every minute.
- AgentClass: on events only.

controller-runtime's own resync (10 hours by default) is the only other periodic trigger.

The design target is 1000 or more Agents and AgentTasks per cluster; [Load and scale](../operations/load-and-scale.md) records what a run proves. Use indexed caches for all cross-resource lookups. The Agent, AgentChannel, and AgentTask controllers each run up to `controller.maxConcurrentReconciles` reconciles at once (default 4; see [Deployment](../operations/deployment.md)); controller-runtime serializes reconciles of the same object at any setting, so the reconcilers hold no state across objects that concurrency could race.
