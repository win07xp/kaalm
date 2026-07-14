# Agentry — Observability

Agentry's v1 observability surface has three pillars: **Prometheus metrics** scraped from the controller and the gateway, **structured JSON logs** from both, and **Kubernetes Events** on the Agentry CRDs and their child resources. This document indexes the per-component metric catalogs, specifies log conventions and PII safety, and lists architecturally-significant alerts and dashboards. Detailed Grafana dashboards, distributed tracing, and an audit-export pipeline are out of scope for v1 — see [VISION.md § Scope for v1](./VISION.md#scope-for-v1).

## Scope

**In v1.**
- Prometheus metrics on dedicated ports (controller `:8080/metrics`, gateway `:9090/metrics`)
- Structured JSON logs from controller and gateway with a hard PII-safety rule
- Kubernetes Events on all five Agentry CRDs
- A small recommended-alerts set tied to architectural failure modes
- Dashboard topology sketches (per-namespace, per-provider, cluster)

**Deferred to v1.1+** (per [VISION.md § Scope for v1](./VISION.md#scope-for-v1)).
- Distributed tracing (OpenTelemetry instrumentation)
- Prebuilt Grafana dashboards (v1 ships catalogs only; concrete JSON ships in v1.1)
- Audit-log export pipeline beyond standard Kubernetes audit logging
- Cost analytics / chargeback reporting

## Metrics

### Endpoints

| Component | Port | Path | Auth | Notes |
|---|---|---|---|---|
| Controller | `:8080` | `/metrics` | None | Standard controller-runtime metrics port |
| Gateway | `:9090` | `/metrics` | None | Shared by LLM Gateway and User Gateway (single Deployment) |

Both endpoints are unauthenticated by design — the standard Prometheus scrape pattern. They are reachable inside the cluster only; platform teams running an in-cluster Prometheus should restrict scrape RBAC and apply a NetworkPolicy admitting only the Prometheus ServiceAccount. The chart documents the scrape ports but does not ship `ServiceMonitor` / `PodMonitor` manifests — scrape integration is left to the platform team. See [DEPLOYMENT.md](./DEPLOYMENT.md).

### Aggregated catalog

Standard controller-runtime reconcile metrics (counts, duration, queue depth, work-queue saturation) are emitted automatically by the controller — see [CONTROLLER_RECONCILERS.md § Observability](./CONTROLLER_RECONCILERS.md#observability) for the per-component canonical list. The Agentry-specific metrics across all three components:

| Source | Metric | Type | Labels |
|---|---|---|---|
| Controller | `agentry_agents` | gauge | `phase`, `namespace` |
| Controller | `agentry_tasks` | gauge | `phase`, `namespace` |
| Controller | `agentry_channels` | gauge | `namespace`, `phase`, `ready`, `platform_connected` |
| Controller | `agentry_provider_budget_canonical_usd` | gauge | `provider`, `namespace`, `period` |
| Controller | `agentry_hibernations_total` | counter | `namespace` |
| Controller | `agentry_wakes_total` | counter | `namespace`, `trigger` |
| Controller | `agentry_budget_threshold_events_total` | counter | `provider`, `namespace`, `action` |
| LLM Gateway | `agentry_llm_requests_total` | counter | `provider`, `model`, `namespace`, `status` |
| LLM Gateway | `agentry_llm_request_duration_seconds` | histogram | `provider`, `model` |
| LLM Gateway | `agentry_llm_tokens_total` | counter | `provider`, `model`, `namespace`, `direction` |
| LLM Gateway | `agentry_llm_spend_usd_total` | counter | `provider`, `namespace` |
| LLM Gateway | `agentry_llm_fallback_total` | counter | `from_provider`, `to_provider`, `reason` |
| LLM Gateway | `agentry_llm_budget_utilization` | gauge | `provider`, `namespace`, `period` |
| User Gateway | `agentry_channel_messages_total` | counter | `channel_type`, `namespace`, `status` |
| User Gateway | `agentry_channel_message_duration_seconds` | histogram | `channel_type` |
| User Gateway | `agentry_channel_wake_total` | counter | `namespace` |
| User Gateway | `agentry_channel_wake_duration_seconds` | histogram | `namespace`, `result` |
| User Gateway | `agentry_channel_callback_total` | counter | `namespace`, `status` |
| User Gateway | `agentry_channel_callback_duration_seconds` | histogram | `namespace` |
| User Gateway | `agentry_channel_response_too_large_total` | counter | `namespace`, `mode` |
| User Gateway | `agentry_channel_async_patch_failed_total` | counter | `namespace` |

For full semantics — when each metric increments, what each label value means, retention and reset behavior — see the per-component canonical sections:

- [CONTROLLER_RECONCILERS.md § Observability](./CONTROLLER_RECONCILERS.md#observability)
- [GATEWAY_LLM.md § Observability](./GATEWAY_LLM.md#observability)
- [GATEWAY_USER.md § Observability](./GATEWAY_USER.md#observability)

### Cardinality

The `namespace` label appears on most metrics and dominates cardinality in clusters with many active tenants. The `model` and `provider` labels are bounded by `ModelProvider.spec.models` and the count of declared providers; enum labels (`status`, `result`, `mode`, `phase`, `trigger`, `action`, `direction`, `ready`, `platform_connected`) carry a handful of values each. **No metric carries per-Agent or per-AgentTask identity as a label** — that resolution belongs in logs and Events, not metrics, to keep cardinality bounded as the cluster scales to thousands of agents.

## Logs

Both the controller and the gateway emit **structured JSON logs to stdout** (klog / logr). The chart does not configure log shipping; platform teams ship via a standard cluster log pipeline (Fluent Bit, Vector, Loki, etc.).

- **Default level:** `info`. The runtime level can be raised to `debug` via a Helm value for development clusters; even at `debug`, prompt and response bodies are not logged in the default build (see PII safety below).
- **Per-line fields:** timestamp, level, component (`controller` | `gateway`), reconciler/handler name, namespace, resource name, and a request-correlation field on gateway request paths.

### PII safety

**Hard rule: in the default build, prompt and response bodies are never logged at any level.** This holds at `info`, at `debug`, and on every code path. Specifically:

- The **LLM Gateway** logs request metadata only — namespace, workload identity, model, status, latency, and prompt/response token counts. Prompt content and provider responses are never serialized to logs.
- The **User Gateway** logs webhook envelope metadata — channel, request id, status, latency. Channel message bodies (inbound webhook payloads) and agent reply bodies are never logged.
- **Reconciler logs** cite resource names and condition reasons, never Secret content, channel auth tokens, or provider API keys.

This is a hard rule because logs are typically shipped to lower-trust aggregation pipelines and prompt content can include credentials, customer data, or platform-team policy decisions surfaced through tool calls. The same posture is stated from the security side at [SECURITY.md § Audit trail](./SECURITY.md#audit-trail).

### Debug-build escape hatch

A separate **debug build** — gated by the Go build tag `agentry_debug_logs` at compile time — can log prompt and response bodies for contract bring-up and integration debugging. The escape hatch exists at the build layer only:

- The official Helm chart only ships default builds. Debug-build images carry the `-debug` tag suffix and emit a startup banner so an operator who accidentally pulls one notices.
- There is **no runtime Helm value, environment variable, feature flag, or admin endpoint** that flips body logging on in a default build. The gate is build-time only.

This keeps the production wire format provably PII-clean while leaving developers a way to inspect bodies during local work against the [runtime contract](./RUNTIME_CONTRACT.md).

## Kubernetes Events

Events are the primary surface for status changes that platform teams discover via `kubectl describe`. The canonical Events list lives at [CONTROLLER_RECONCILERS.md § Event Emission](./CONTROLLER_RECONCILERS.md#event-emission); reconciler-specific reasons (`FQDNPolicyUnsupported`, `WakeIgnored`, `FallbackIneligible`, `DegradeTargetNotCheapest`) are documented at the relevant reconciler step. (`InvalidDegradeTarget` is a `Ready=False` condition reason, not an Event — see [CONTROLLER_RECONCILERS.md § ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler) step 6.) The gateway also emits a runtime `FallbackIneligible` Warning at request time — see [GATEWAY_LLM.md § Fallback Logic](./GATEWAY_LLM.md#fallback-logic).

Architecturally-significant Event groups, with the reasons attached:

- **Phase transitions** on Agent and AgentTask (`Normal`, `PhaseChanged`).
- **Hibernation / wake** on Agent (`Normal`, `Hibernated` / `Woken`; `Warning`, `WakeIgnored`) — see [CONTROLLER_LIFECYCLE.md § Hibernation mechanics](./CONTROLLER_LIFECYCLE.md#hibernation-mechanics) and [Wake trigger](./CONTROLLER_LIFECYCLE.md#wake-trigger).
- **Provider health and budget** on ModelProvider (`Warning`, `ProviderUnhealthy` / `BudgetExhausted`).
- **Validation failures** on any CRD (`Warning`, `InvalidReference` plus reconciler-specific reasons).
- **Fallback misconfiguration** on ModelProvider (`Warning`, `FallbackIneligible` from both reconcile-time and runtime paths; `DegradeTargetNotCheapest` advisory).
- **Callback failures** on AgentChannel (`Warning`, `CallbackInvalid` when the `callbackUrl` fails the pre-dial deny-range / allowlist re-check; `CallbackRejected` when the receiver terminally rejects the POST) — the per-occurrence signal paired with the persistent `PlatformConnected=False` condition; see [API_ENDPOINTS.md § Async Webhook Response](./API_ENDPOINTS.md#async-webhook-response-gateway-managed).
- **AgentClass propagation cascade** on each affected Agent during recreate-and-clamp or `Degraded` transitions — see [ARCHITECTURE.md § Per-Agent and Per-Task Child Resources](./ARCHITECTURE.md#per-agent-and-per-task-child-resources).

Events persist per the cluster's standard Event retention. For long-term audit, see [SECURITY.md § Audit trail](./SECURITY.md#audit-trail).

## Recommended alerts

A v1 alert set tied to architectural failure modes already named in the doc set. Concrete PromQL and threshold tuning is implementation work and is not specified here.

| Alert | Severity | Architectural hook |
|---|---|---|
| Controller all replicas unready | Page | Wake-on-demand is a hard control-plane dependency — see [ARCHITECTURE.md § The Agentry Gateway](./ARCHITECTURE.md#the-agentry-gateway) |
| Gateway all replicas unready | Page | LLM and webhook traffic blocked cluster-wide |
| Reconcile error rate elevated | Warn | Reconciler is stuck — surface before the work queue backs up |
| LLM error rate elevated for a provider | Warn | Provider degraded; consider promoting fallback |
| Sustained fallback rate to a backup provider | Warn | Primary provider effectively down |
| Budget threshold `degrade` or `block` triggered | Warn / Page | Tenant or provider crossed the configured spend ceiling |
| Hibernation / wake churn for a single Agent | Warn | Likely idle-timeout misconfig — see [Agent (persistent mode)](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode) |
| Per-namespace rate-limit saturation | Warn | Tenant hitting the per-(namespace, model) ceiling — see [Rate Limiting](./GATEWAY_LLM.md#rate-limiting) |
| Wake duration p95 elevated | Warn | [Activator](./GATEWAY_USER.md#activator) path slow — `agentry_channel_wake_duration_seconds` |
| Async-callback exhaustion rate elevated | Warn | Receivers' `callbackUrl` repeatedly unreachable; receivers should [poll](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) |
| `agentry_channel_async_patch_failed_total` nonzero | Warn | The v1 async silent-loss limitation fired — a response was dropped after `Patch` retry exhaustion; pollers see `202` → `404` with no stored envelope. See [API_ENDPOINTS.md § Response-Patch failure semantics](./API_ENDPOINTS.md#async-webhook-response-gateway-managed) |

## Recommended dashboards

Three top-level panels cover v1 visibility. Concrete Grafana JSON ships in v1.1.

1. **Per-namespace.** Agent and AgentTask phase counts, spend (`agentry_provider_budget_canonical_usd`), rate-limit utilization, channel message rate, channel condition rollup (`agentry_channels` by `ready` / `platform_connected`).
2. **Per-provider.** Request rate, error rate, latency p50/p95, fallback events, budget utilization.
3. **Cluster.** Controller and gateway replica readiness, reconcile error rate, hibernation and wake counts, wake-duration distribution, async-callback delivery state.

## Tracing

Distributed tracing across gateway → agent → provider hops is out of scope for v1 (per [VISION.md § Scope for v1](./VISION.md#scope-for-v1)). When tracing lands in v1.1, OpenTelemetry instrumentation will key spans on the gateway's existing request-correlation field. v1 takes no position on the eventual span-propagation header — that decision sits with the v1.1 work.

## See also

- [CONTROLLER_RECONCILERS.md § Observability](./CONTROLLER_RECONCILERS.md#observability) — controller metric catalog and emit-points
- [CONTROLLER_RECONCILERS.md § Event Emission](./CONTROLLER_RECONCILERS.md#event-emission) — controller Events list
- [GATEWAY_LLM.md § Observability](./GATEWAY_LLM.md#observability) — LLM Gateway metric catalog
- [GATEWAY_USER.md § Observability](./GATEWAY_USER.md#observability) — User Gateway metric catalog
- [SECURITY.md § Audit trail](./SECURITY.md#audit-trail) — Kubernetes audit logging guidance
