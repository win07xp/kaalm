# Operator structure

The Kaalm operator is the control plane: it turns the declarative CRDs in [Resource overview](../resources/overview.md) into running Pods, Services, Secrets, and status. This chapter describes it from the inside out. [Reconcilers](reconcilers.md) walks through what each of the six reconcilers does on a pass, then [Agent lifecycle](agent-lifecycle.md), [Hibernation and wake](hibernation-and-wake.md#hibernation-mechanics), [Change propagation](change-propagation.md#agentclass-change-handling), [AgentTask lifecycle](task-lifecycle.md), and [Finalizers](finalizers.md) cover the state machines those passes drive. [Errors, events, and testing](operations.md#error-handling) closes the chapter with the operator's failure handling, Events, metrics, and test strategy.

This page covers what the binary is, what it serves, what runs on every replica, and how often it reconciles.

## The binary

The operator is one `controller-runtime` binary. It hosts six reconcilers, `AgentClassReconciler`, `ModelProviderReconciler`, `ToolProviderReconciler`, `AgentReconciler`, `AgentTaskReconciler`, and `AgentChannelReconciler`, and these listeners:

| Listener | Port | Serves | Enabled by |
|---|---|---|---|
| Activator | `:9443`, on the `kaalm-controller` Service | `POST /v1/activate/{namespace}/{agentName}` over mTLS; `/healthz` and `/readyz` answer `ok` without a client cert | `--controller-tls-cert`, which the chart always sets |
| Conversion webhook | `:9444`, on the Service | `POST /convert`, translating the six kinds between `v1alpha1` and `v1beta1` for the apiserver ([API versioning and deprecation](../operations/api-versioning.md#where-the-conversion-webhook-runs)) | `--webhook-cert-path`, which the chart always sets |
| Metrics | `:8080/metrics`, on the Service | Prometheus metrics over plain HTTP; the binary keeps metrics off unless `--metrics-bind-address` is passed, and the chart passes it | the chart |
| Probes | `:8081`, Pod-local | `/healthz` and `/readyz` for kubelet, plain HTTP | always |
| pprof | `controller.pprofPort` | `net/http/pprof`, for profiling under load; off by default | the chart value |

The activator listener serves TLS with `tls.Config.ClientAuth = VerifyClientCertIfGiven`, so a caller without a client cert completes the handshake and per-path middleware enforces mTLS with SAN authorization on `/v1/activate` only; the rules are under [Internal endpoint authentication](../security/rbac.md#internal-endpoint-authentication). Kubelet probes the `:8081` listener, not `:9443`. The controller serves no admission webhooks; see [No admission webhooks](#no-admission-webhooks).

## Deployment and leader election

The operator runs as a Deployment in `kaalm-system` with leader election on the Lease `e06da714.io`. The chart enforces two replicas as a floor; only the leader runs the reconcilers and the storage-version migrator, while every replica serves the activator, the conversion webhook, metrics, and the probes. Leader release on shutdown is not enabled, so after the leader exits the survivor acquires the Lease when it expires, 15 seconds by default, and reconciles from there. The replica count, disruption budget, and anti-affinity are under [The two Deployments](../operations/deployment.md#the-two-deployments).

Every replica serves `/metrics` from its own cache. controller-runtime's reconcile and workqueue families read zero on a non-leader, but the three Kaalm phase gauges (`kaalm_agents`, `kaalm_tasks`, `kaalm_channels`) are computed from the cache on every scrape and read the same fleet counts on both replicas, so dashboards aggregate them with `max`, not `sum`; see [Observability](operations.md#observability).

![Two controller replicas behind the kaalm-controller Service. Both serve the :9443 activator, the :9444 conversion webhook, and :8080 metrics; only replica A, the leader, runs the six reconcilers. The gateway POSTs /v1/activate to the Service, which lands on any replica; that replica patches kaalm.io/wake=true on the Agent through the apiserver, and the leader's Agent watch fires. Replica A holds the leader Lease; replica B waits on it.](../diagrams/controller-replicas.svg)

### Activator handler (served on every replica)

The `POST /v1/activate/{namespace}/{agentName}` handler runs on every replica, not only the leader. It authenticates the caller by SAN, loads the Agent, patches `kaalm.io/wake=true` onto it through the apiserver, and answers `202`. The leader's Agent watch fires and the [AgentReconciler](reconcilers.md#agentreconciler) handles the annotation as its first step. The wake reaches the leader because it is written into the Agent: the apiserver, not the replica that took the call, is the message bus, so the Service needs no leader-aware endpoint selection, and a non-leader needs only the patch access the controller ServiceAccount already has. The gateway side of the call is under [The activator](../gateways/user/activation-and-activity.md#the-activator).

## No admission webhooks

Field-level validation uses CEL expressions in the CRD schemas (`x-kubernetes-validations`). Cross-resource validation runs at reconcile time and is surfaced as `Ready=False` conditions with descriptive messages. This removes the availability risk of a webhook server on the apiserver's write path: a wedged admission webhook would block writes to the resources it guards.

The one webhook the controller serves is the CRD conversion webhook, and it is not an admission webhook. It validates and mutates nothing, and it is consulted only when a request's version differs from the stored version, which after storage migration means only clients still sending `v1alpha1`. Kaalm's own components speak the storage version, so a wedged conversion path cannot block the control plane, the gateway, or the console. The availability argument in full is under [What depends on the webhook](../operations/api-versioning.md#what-depends-on-the-webhook).

## Controller TLS

The activator listener and the conversion webhook serve the cert-manager `Certificate` `kaalm-controller-tls`, issued by `kaalm-ca-issuer` with `server auth` and `client auth` usages: the same cert is presented as a client cert when the controller dials the gateway's `/v1/activity` and `/v1/channels/health` endpoints. The SAN set, the issuer chain, and rotation are under [In-cluster TLS](../security/tls.md#in-cluster-tls).

## Reconcile interval and performance

Every reconciler runs on events first; each also requeues on a cadence of its own, and there is no shared periodic interval. Every lifecycle the reconcilers drive is indexed on [Lifecycles at a glance](../appendix/lifecycles.md).

| Reconciler | Requeues |
|---|---|
| Agent | every 15 seconds while `Running` or `Idle` (the activity cache window); every 30 seconds while a Ready gate or a gateway outage holds; every 5 seconds while waiting on the Certificate |
| AgentTask | every 5 seconds while waiting on the Certificate or for the Pod to become Ready; every 30 seconds while a pull Secret is missing; at `startTime + timeout` while `Running`; at TTL expiry once terminal |
| ModelProvider | at `healthCheck.intervalSeconds` (default 60) when the probe is enabled, else every minute for a provider with a budget period; the budget ConfigMap watch fires the fold between passes |
| ToolProvider | at `healthCheck.intervalSeconds` (default 60) when the probe is enabled |
| AgentChannel | every minute |
| AgentClass | on events only |

controller-runtime's own resync (10 hours by default) is the only other periodic trigger. The Agent, AgentChannel, and AgentTask reconcilers each run up to `controller.maxConcurrentReconciles` reconciles at once (default 4; [Deployment](../operations/deployment.md)); controller-runtime serializes reconciles of the same object at any setting, and the reconcilers hold no state across objects that concurrency could race. Every cross-resource lookup goes through an indexed cache; the indexes and watches are tabled under [What each reconciler watches](reconcilers.md#what-each-reconciler-watches). The design target is 1,000 or more Agents and AgentTasks per cluster; [Load and scale](../operations/load-and-scale.md) records what a run proves.
