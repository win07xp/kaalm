# Observability

Kaalm emits five kinds of signal:

1. **Prometheus metrics** from the controller and the gateway.
2. **Logs** from the controller, the gateway, and the console.
3. **Kubernetes Events** on the Kaalm resources.
4. **OpenTelemetry traces** from the gateway, off by default.
5. **Go profiles** from the controller and the gateway, off by default.

This page carries the full metric table, the log conventions and the PII rule, the recommended alerts, the dashboards, tracing, and profiling. Each component's own page states when a metric increments and what its label values mean: [Controller observability](../controller/operations.md#observability), [LLM Gateway observability](../gateways/llm/operations.md#observability), [User Gateway observability](../gateways/user/operations.md#observability), and [Audit and metering](../gateways/tool-plane.md#audit-and-metering) for the tool broker.

![Which component emits which signal to where: the controller and gateway serve metrics on plain-HTTP ports to a Prometheus scrape, write logs to their standard streams for the cluster log pipeline, and post Events to the API server; the gateway alone exports spans to an optional OTLP collector.](../diagrams/signal-topology.svg)

## Scope

**What ships.**

- Prometheus metrics on dedicated ports (controller `:8080/metrics`, gateway `:9090/metrics`)
- Logs from all three components with a hard PII-safety rule
- Kubernetes Events on all six Kaalm CRDs
- A small recommended-alerts set tied to architectural failure modes
- Three Grafana dashboards (per-namespace, per-provider, cluster) as importable JSON
- OpenTelemetry tracing across the gateway to agent to provider hops (default off)

**Not part of Kaalm.** Both are on the vision page's [Scope for v1](../concepts/vision-and-scope.md#scope-for-v1) list.

- An audit-log export pipeline beyond standard Kubernetes audit logging
- Cost analytics and chargeback reporting

## Metrics

### Endpoints

| Component | Port | Path | Auth | Notes |
|---|---|---|---|---|
| Controller | `:8080` | `/metrics` | None | Plain HTTP, controller-runtime's metrics port. Every replica serves it |
| Gateway | `:9090` | `/metrics` | None | Plain HTTP, shared by the LLM and User Gateway paths |

Both endpoints are ClusterIP only. The chart ships no `ServiceMonitor` or `PodMonitor`; its NetworkPolicy admits the two ports only from the `networkPolicy.metricsFrom` peers, so point that value at your Prometheus ([The operator's own NetworkPolicy](deployment.md#the-operators-own-networkpolicy), [Recommendations for deployment](../security/model.md#recommendations-for-deployment)). The console serves no metrics.

### Aggregated catalog

Standard controller-runtime reconcile metrics (counts, duration, queue depth, work-queue saturation) are emitted automatically by the controller. See [Observability](../controller/operations.md#observability) for the per-component canonical list.

The table lists the Kaalm-specific metrics of the controller and the gateway; the gateway rows are split into LLM Gateway, User Gateway, and Tool broker. The dashboard test under `test/dashboards` reads this table, so a new row needs a panel ([Dashboards](#dashboards)). One Kaalm metric is outside it on purpose: `kaalm_storage_migrated_objects_total{kind}`, a counter the storage-version migrator increments once per upgrade ([Storage-version migration](api-versioning.md#storage-version-migration)).

| Source | Metric | Type | Labels |
|---|---|---|---|
| Controller | `kaalm_agents` | gauge | `phase`, `namespace` |
| Controller | `kaalm_tasks` | gauge | `phase`, `namespace` |
| Controller | `kaalm_channels` | gauge | `namespace`, `phase`, `ready`, `platform_connected` |
| Controller | `kaalm_provider_budget_canonical_usd` | gauge | `provider`, `namespace`, `period` |
| Controller | `kaalm_hibernations_total` | counter | `namespace` |
| Controller | `kaalm_wakes_total` | counter | `namespace`, `trigger` (`channel` or `annotation`) |
| LLM Gateway | `kaalm_llm_requests_total` | counter | `provider`, `model`, `namespace`, `status` |
| LLM Gateway | `kaalm_llm_request_duration_seconds` | histogram | `provider`, `model` |
| LLM Gateway | `kaalm_llm_tokens_total` | counter | `provider`, `model`, `namespace`, `direction` |
| LLM Gateway | `kaalm_llm_spend_usd_total` | counter | `provider`, `namespace` |
| LLM Gateway | `kaalm_llm_fallback_total` | counter | `from_provider`, `to_provider`, `reason` |
| LLM Gateway | `kaalm_llm_budget_utilization` | gauge | `provider`, `namespace`, `period` |
| LLM Gateway | `kaalm_budget_threshold_events_total` | counter | `provider`, `namespace`, `action` |
| LLM Gateway | `kaalm_llm_budget_boundary_events_total` | counter | `provider`, `namespace`, `event` |
| LLM Gateway | `kaalm_llm_server_tool_use_total` | counter | `provider`, `namespace`, `tool` |
| LLM Gateway | `kaalm_llm_usage_missing_total` | counter | `provider`, `model` |
| Tool broker | `kaalm_tool_calls_total` | counter | `provider`, `namespace`, `tool`, `status` |
| Tool broker | `kaalm_tool_call_duration_seconds` | histogram | `provider`, `tool` |
| User Gateway | `kaalm_channel_messages_total` | counter | `channel_type`, `namespace`, `status` |
| User Gateway | `kaalm_channel_message_duration_seconds` | histogram | `channel_type` |
| User Gateway | `kaalm_channel_wake_total` | counter | `namespace` |
| User Gateway | `kaalm_channel_wake_duration_seconds` | histogram | `namespace`, `result` |
| User Gateway | `kaalm_channel_delivery_attempts_total` | counter | `namespace`, `outcome` |
| User Gateway | `kaalm_channel_callback_total` | counter | `namespace`, `status` |
| User Gateway | `kaalm_channel_callback_duration_seconds` | histogram | `namespace` |
| User Gateway | `kaalm_channel_response_too_large_total` | counter | `namespace`, `mode` |
| User Gateway | `kaalm_channel_async_patch_failed_total` | counter | `namespace` |

### Cardinality

The `namespace` label appears on most metrics and dominates cardinality in clusters with many active tenants. The `model` and `provider` labels are bounded by `ModelProvider.spec.models` and the count of declared providers. The `tool` label is bounded by declared catalogs: on the broker metrics it carries only ids from `ToolProvider.spec.tools` (everything else collapses to `uncataloged`; see [Audit and metering](../gateways/tool-plane.md#audit-and-metering)), and on `kaalm_llm_server_tool_use_total` it carries the provider-side tool vocabulary, a handful of values per provider type. Enum labels (`status`, `result`, `mode`, `phase`, `trigger`, `action`, `direction`, `ready`, `platform_connected`) carry a handful of values each.

**No metric carries per-Agent or per-AgentTask identity as a label.** That resolution belongs in logs, Events, and the console read API backed by the gateway's [per-workload spend ledger](../gateways/llm/budgets-and-rate-limits.md#per-workload-spend), not metrics, to keep cardinality bounded as the cluster scales to thousands of agents.

## Logs

The gateway and the console write structured JSON to stdout at `info`. The controller writes structured JSON to stderr at `info`. `controller.logLevel`, `gateway.logLevel`, and `console.logLevel` set each component's level ([Configuration reference](deployment.md#configuration-reference)); the controller accepts `debug`, `info`, or `error`, and the gateway and console accept `debug`, `info`, `warn`, or `error`. The chart configures no log shipping; ship the streams with your cluster log pipeline.

Per-line fields are per call site, not a fixed schema. The controller's lines carry controller-runtime's `controller`, `namespace`, `name`, and `reconcileID` fields. The gateway's lines carry the identifiers each path has: `requestId`, `messageId`, and `namespace` on the User Gateway, `namespace` and `provider` on the budget and fallback paths, and the audit fields on the tool broker. No line carries a `component` field; the stream tells the components apart.

### PII safety

**Hard rule: in the default build, prompt and response bodies are never logged at any level.** This holds at `info`, at `debug`, and on every code path. Specifically:

- The **LLM proxy** writes no per-request line. Its only log lines are a budget threshold crossing, a streaming relay error, a response that carried no usage ([Streaming responses](../gateways/llm/request-handling.md#streaming-responses)), and a refused provider credential ([Credential handling](../gateways/llm/provider-routing.md#credential-handling)). Each carries names, counts, or a reason, never a body or a key value. Request accounting is metrics ([Aggregated catalog](#aggregated-catalog)).
- The **tool broker** logs one audit record per call: caller identity, ToolProvider, tool, method, outcome, duration, and sizes ([Audit and metering](../gateways/tool-plane.md#audit-and-metering)). It also writes two warnings: a refused or unreadable credential, paced ([Credential injection](../gateways/tool-plane.md#credential-injection)), and a tool server that rejected the gateway credential. Each carries the ToolProvider name and a reason or status, never a body or a credential value. Arguments and results are never logged.
- The **User Gateway** logs a failed delivery attempt and each test-chat delivery, with channel, request id, status, and latency. Webhook payloads and agent replies are never logged.
- **Reconciler logs** cite resource names and condition reasons, never Secret content, channel auth tokens, or provider API keys.

This is a hard rule because logs are typically shipped to lower-trust aggregation pipelines, and prompt content can include credentials, customer data, or platform-team policy decisions surfaced through tool calls. The same rule is stated from the security side at [Audit trail](../security/model.md#audit-trail).

### Debug build for body logging

A separate **debug build**, gated by the Go build tag `kaalm_debug_logs` at compile time, can log prompt and response bodies on the LLM proxy paths, and tool-call request and response bodies on the MCP broker routes, for testing an image against the [runtime contract](../runtime/contract.md) and for integration debugging. Body logging exists only in that build:

- The published images are default builds. A debug build emits a startup banner, so an operator who runs one notices.
- There is **no runtime Helm value, environment variable, feature flag, or administration endpoint** that flips body logging on in a default build. The gate is build-time only.

## Kubernetes Events

Events are the surface for status changes that platform teams discover with `kubectl describe`. [Event emission](../controller/operations.md#event-emission) is the full table of the controller's reasons, and the gateway's four runtime Warnings are stated under [Gateway ServiceAccount permissions](../security/rbac.md#gateway-serviceaccount-permissions). The groups that matter for alerting:

- **Phase transitions** on Agent (`Normal`, `PhaseChanged`). AgentTask emits no phase Event; its settle and retry Events carry the outcome.
- **Hibernation and wake** on Agent (`Normal`, `Hibernated` and `Woken`; `Warning`, `WakeIgnored`). See [Hibernation mechanics](../controller/hibernation-and-wake.md#hibernation-mechanics).
- **Provider health** on ModelProvider and ToolProvider (`Warning`, `ProviderUnhealthy`, on every failing probe pass), the ToolProviderReconciler's `CredentialsInvalid` when its probe first sets `Ready=False` with that reason ([ToolProviderReconciler](../controller/reconcilers/toolprovider.md)), and the hard-enforcement margin on ModelProvider (`Warning`, reason `ObservedTrafficExceededMargin`, matching the `BoundaryMarginRaised` condition's reason).
- **Credential validation** on ModelProvider and ToolProvider (`Warning`, `CredentialsMissing`, `SecretNotOptedIn`, and `EndpointHostNotApproved` ([rules 49 and 50](../resources/validation/providers.md#provider-credentials)), each emitted once when `Ready` first turns `False` with that reason). ModelProvider also emits every other `Ready=False` reason this way, including the probe's `CredentialsInvalid`; the ToolProvider's `CredentialsInvalid` is the event above.
- **Child conflicts** on Agent and AgentTask (`Warning`, `ChildConflict`), emitted when the reason or its message first appears on `Ready` ([Child ownership](../controller/reconcilers/agent.md#child-ownership)).
- **Rejected Pod creates** on Agent and AgentTask (`Warning`, `PodCreateRejected`), emitted once when the reason first appears on `Ready`, not on each re-check ([Event emission](../controller/operations.md#event-emission)). An alert on it catches quota exhaustion and a class that names a missing RuntimeClass.
- **Rejected child writes** on Agent and AgentTask (`Warning`, `ChildWriteRejected`), emitted once when the reason first appears on `Ready` ([Event emission](../controller/operations.md#event-emission)). An alert on it catches quota exhaustion on Services or PVCs and webhook denials of policies.
- **Degraded entry** on Agent (`Warning`, the Degraded reason as the Event reason), emitted once when the Agent enters `Degraded` ([Degraded](../controller/agent-lifecycle.md#degraded)).
- **Task settlement and retry** on AgentTask (`Normal`, `TaskSucceeded`; `Warning`, `TaskFailed` and the timeout reasons; one `Warning` per retry).
- **Provider configuration** on ModelProvider (`Warning`, `FallbackIneligible` at reconcile time and at request time, `DegradeTargetNotCheapest`, `MaxOutputTokensUnset`) and the gateway's `CredentialsInvalid` when a provider refuses the key, during a fallback walk for a ModelProvider or a brokered call for a ToolProvider ([Fallback logic](../gateways/llm/fallback.md), [The tool plane](../gateways/tool-plane.md#failure-modes)).
- **Callback rejection** on AgentChannel (`Warning`, `CallbackRejected` when a platform refuses or exhausts a reply). A `callbackUrl` that fails the pre-dial check lands on the channel's `PlatformConnected` condition with reason `CallbackInvalid`, not in an Event ([Channel health tracking](../gateways/user/platform-adapters.md#channel-health-tracking)).
- **Pod replacement** on Agent (`Normal`, `SpecDrift` and `SpecDriftPending`; `Warning`, `PodDisrupted`). See [Change propagation](../controller/change-propagation.md).

Events persist per the cluster's standard Event retention. For long-term audit, see [Audit trail](../security/model.md#audit-trail).

## Recommended alerts

These alerts cover the failure modes this book names.

| Alert | Severity | Architectural hook |
|---|---|---|
| Controller all replicas unready | Page | Wake-on-demand depends on the controller. A replica is Ready only while its activator listener serves, so this alert also covers a dead or not-yet-started activator ([Readiness](../controller/overview.md#readiness)). See [The activator](../gateways/user/activation-and-activity.md#the-activator) |
| Gateway all replicas unready | Page | LLM and webhook traffic blocked cluster-wide |
| Reconcile error rate elevated | Warn | Reconciler is stuck. Surface before the work queue backs up |
| LLM error rate elevated for a provider | Warn | Provider degraded; consider promoting fallback |
| Sustained fallback rate to a backup provider | Warn | Primary provider effectively down |
| Budget threshold `degrade` or `block` triggered | Warn / Page | Tenant or provider crossed the configured spend ceiling |
| Hibernation and wake churn in a namespace | Warn | An idle timeout set too short; the counters carry `namespace` only, so find the Agent from its Events. See [Agent lifecycle](../controller/agent-lifecycle.md) |
| Per-namespace rate-limit saturation | Warn | Tenant hitting the per-(namespace, model) ceiling. See [Rate limiting](../gateways/llm/budgets-and-rate-limits.md#rate-limiting) |
| Wake duration p95 elevated | Warn | [Activator](../gateways/user/activation-and-activity.md#the-activator) path slow. Watch `kaalm_channel_wake_duration_seconds` |
| Async-callback exhaustion rate elevated | Warn | Receivers' `callbackUrl` repeatedly unreachable; receivers should [poll](../gateways/api/async-responses.md) |
| `kaalm_channel_async_patch_failed_total` nonzero | Warn | An async response payload was dropped; pollers see `202` then `404`. See [Response-patch failure](../gateways/api/async-responses.md#response-patch-failure) |

## Dashboards

Three Grafana dashboards ship as JSON in `config/grafana/`, one per topology level. Each file is self-contained: import it through the Grafana UI or provision it from the file, with no manual edits. The data source is chosen by a `datasource` template variable, the only import form that works unchanged on both paths (file provisioning never substitutes `__inputs`).

| File | Scope | Variables | Panels |
|---|---|---|---|
| `kaalm-namespace.json` | One tenant namespace | `datasource`, `namespace` | Agent, AgentTask, and AgentChannel counts by phase and condition; canonical spend and budget utilization per provider; budget policy actions; LLM request, rate-limited, and token rates; channel message rate and p95 duration; wake count and p95 duration; tool calls; async callbacks, oversized responses, patch failures |
| `kaalm-provider.json` | One ModelProvider | `datasource`, `provider` | Request rate, error ratio, latency p50 and p95 by model, token rates, fallback events by target and reason, budget utilization and canonical spend by namespace, spend rate, budget policy and hard-enforcement boundary events, provider-side tool use |
| `kaalm-cluster.json` | The whole cluster | `datasource`, `job` | Scrape targets up, controller leader, reconcile errors, duration, and queue depth; fleet totals by phase; hibernations and wakes; wake duration; channel messages; LLM and tool-broker traffic and latency; fallbacks; canonical spend by provider; async delivery state |

Conventions the panels follow:

- Every query is over the [Aggregated catalog](#aggregated-catalog), and every catalog metric is on at least one panel; the test under `test/dashboards` checks both directions and that each dashboard file imports with no manual edits. The only non-catalog series are on the cluster dashboard's control-plane row: the scrape `up` series, controller-runtime's reconcile and work-queue families, and its `leader_election_master_status` gauge.
- The phase-count and budget-utilization gauges are computed on every scrape by every replica, so the panels aggregate them with `max`, never `sum`.
- The per-namespace rate-limit panel is the `rate_limited` outcome of `kaalm_llm_requests_total`; there is no separate utilization gauge for the per-(namespace, model) ceiling.
- The cluster dashboard's `job` variable matches scrape jobs whose name contains `kaalm`; a ServiceMonitor on the chart's Services resolves to such names. Every other panel is independent of how the scrape is configured.

`make dashboards-verify` proves the files against a live cluster: it installs a throwaway Prometheus and Grafana, provisions the three files unchanged into Grafana, and checks that every panel query is valid PromQL against the scraped series and that the metric families the e2e suite exercises are present.

## Tracing

OpenTelemetry tracing is off by default. When it is on, it connects one user message to the LLM and tool calls it caused, across the gateway to agent to provider hops.

**Propagation** is W3C `traceparent` and `tracestate`, and nothing else. Per-request identity travels as span attributes, not metric labels, so the [Cardinality](#cardinality) rule holds: `kaalm.message_id`, `kaalm.namespace`, `kaalm.agent`, `kaalm.workload`, `kaalm.provider`, `kaalm.model`, `kaalm.channel_type`, `kaalm.method`, and `kaalm.tool`, each where it applies.

**Span inventory.** Every span is created by the gateway; the agent hop propagates:

| Span | Kind | Where | Notes |
|---|---|---|---|
| `channel.receive` | server | User Gateway | Webhook and test-chat receipt; root unless the caller sent trace context. Covers handling through the sync reply, or through the `202` in async mode; the background delivery stays in the same trace |
| `agent.deliver` | client | User Gateway | The delivery to the agent, retries included; its context travels to the agent on the delivery request |
| `llm.request` | server | LLM proxy | Parented by whatever context the agent propagated. A denial past route authorization (budget, rate limit) closes it with an error status, so a blocked request is visible in its trace |
| `llm.forward` | client | LLM proxy | One per provider attempt, fallback candidates included; the candidate is the `kaalm.provider` attribute |
| `tool.call` | server | Tool broker | The governed MCP call; broker denials and failed relays carry an error status, as the note after the table explains |
| `tool.forward` | client | Tool broker | The upstream half of a forwarded call |

**Tool call status.** The `tool.call` span carries an error status for broker denials and for a failed relay: a response over the size cap, streamed or not, or a response the broker could not read before relaying it. The broker reads non-stream responses and `tools/list` answers (JSON or SSE) in full before it relays anything. If that read fails, times out, or can't be parsed, the span gets the error type `tool_unavailable` or `tool_timeout`. A stream that times out or breaks partway is marked too, with `tool_timeout` or `tool_unavailable`. A call whose caller disconnected mid-stream is marked as well, with the description `client_closed`. The status description is the error type, such as `response_too_large`, the same type the caller and the `status` label of `kaalm_tool_calls_total` see. `client_closed` is the only description the caller never sees ([Audit and metering](../gateways/tool-plane.md#audit-and-metering) says what it means). A stream the broker ends with an error event has already sent its status line, usually `200`, so the span status is the trace's only sign of the failure. A tool server's own 4xx answer, relayed unchanged, leaves the status unset, and the metric labels it `upstream_error`.

![The span tree for one message: channel.receive parents agent.deliver; the agent's own work has no span but propagates the context; llm.request parents llm.forward, and tool.call parents tool.forward.](../diagrams/span-tree.svg)

The agent's own processing appears as the gap between `agent.deliver` and its child spans: the base images carry no OpenTelemetry SDK, and propagation alone connects the trace. The runtime forwards the delivery's trace context on every gateway call ([contract item 8](../runtime/contract.md#8-trace-context-propagation)); a framework running its own SDK reads the same context (`kaalm.trace_context()` in Python, `agentruntime.TraceContext` in Go) and fills the gap with real agent spans.

**Exporter.** OTLP over HTTP, configured by two Helm values ([Deployment](deployment.md)): `gateway.tracing.otlpEndpoint` (default `""`) and `gateway.tracing.sampleRatio` (default `1.0`, parent-based head sampling for traces the gateway starts). With no endpoint, no tracer is installed: no spans, no propagation, and request handling carries no tracing overhead, which is the default install. An `https` endpoint is verified against the gateway's upstream trust pool when one is configured, else the system roots; an `http` endpoint sends in the clear.

The controller and the console emit no spans: the traced path is the message path, and reconcile visibility remains metrics, logs, and Events. Scenario [S20](../appendix/scenarios.md#s20-follow-one-message-across-the-hops) proves the connected trace live.

## Profiling

Both components can serve Go's `net/http/pprof` profiles, off by default. Setting `controller.pprofPort` or `gateway.pprofPort` to a port number starts the listener and adds a named `pprof` container port; it serves CPU, heap, goroutine, mutex, and block profiles under `/debug/pprof/`. Turning it on also enables mutex and block sampling, which cost a little on every contended lock and blocking call.

The listener is a debugging aid, not an operations surface. It is unauthenticated, and the chart never puts it behind a Service, so the way to reach it is a port-forward for the length of a profiling session:

```bash
kubectl -n kaalm-system port-forward deploy/kaalm-gateway 6060:6060
go tool pprof -http=:8000 http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

Leave both values at `0` in production. The [load harness](load-and-scale.md) turns them on for the load cluster so a profile can be taken during any phase.

## See also

- [Controller observability](../controller/operations.md#observability): controller metric semantics
- [Event emission](../controller/operations.md#event-emission): the controller's Event reasons
- [LLM Gateway observability](../gateways/llm/operations.md#observability): LLM Gateway metric semantics
- [User Gateway observability](../gateways/user/operations.md#observability): User Gateway metric semantics
- [Audit trail](../security/model.md#audit-trail): Kubernetes audit logging guidance
