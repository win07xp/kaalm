# The runtime contract

Kaalm is BYO-image: any container can run as an Agent or AgentTask, provided it implements a small contract. This page specifies that contract. It is the minimum a container image must satisfy to participate in the lifecycle: HTTPS health endpoints on the injected health port, graceful SIGTERM handling, authenticated TLS calls to the injected gateway endpoint, an optional `POST /v1/message` handler when an AgentChannel is in use, and `messageId` deduplication on that handler.

The contract has eight numbered items. Other pages cite them by number, so the numbering is stable. For the surrounding system, see [System architecture](../concepts/system-architecture.md). Working implementations of every item ship as Go and Python [starter templates](starter-templates.md), summarized at the end of this page.

## 1. HTTPS health endpoints

The container serves two health endpoints on a known port (`$KAALM_HEALTH_PORT`, default 8080):

- `GET /readyz` (readiness), returning 200 when healthy.
- `GET /livez` (liveness), returning 200 when healthy.

These are the paths the controller-injected probes target (see [AgentReconciler step 7](../controller/reconcilers.md#agentreconciler)). The agent serves TLS on this port using the cert-manager-issued per-Agent certificate (`$KAALM_TLS_CERT` / `$KAALM_TLS_KEY`). Liveness and readiness probes must therefore be configured with `httpGet.scheme: HTTPS`. Kubernetes does not verify TLS certificates for httpGet probes, so the certificate works without any additional CA configuration on the probe.

## 2. Graceful SIGTERM handling

On receiving SIGTERM, the agent should finish in-flight work and exit within the configured `terminationGracePeriodSeconds`.

## 3. Gateway communication

The controller injects `$KAALM_GATEWAY_ENDPOINT`: an HTTPS URL pointing to the gateway's cluster listener (port 8443). This is the base URL for all agent to gateway calls:

- LLM requests.
- Heartbeats: [`POST /v1/agent/heartbeat`](../gateways/api/agent-endpoints.md#post-v1agentheartbeat), Agents only, see item 5.
- Task completion: [`POST /v1/task/complete`](../gateways/api/task-complete.md), AgentTasks only, see item 6.

The variable is always injected, whether or not `spec.providers` is set. Provider-less workloads can therefore still reach the gateway: Agents for heartbeats, AgentTasks for task completion.

![Sequence diagram of the agent as a client. An Agent container sends LLM requests and POST /v1/agent/heartbeat to the gateway's cluster listener; an AgentTask container sends LLM requests and POST /v1/task/complete. Every call is mTLS with the workload's own certificate, and the two report paths accept only their own workload kind.](../diagrams/agent-outbound-calls.svg)

Every call in the figure is mTLS with the workload's own certificate, and the gateway cross-checks the source IP against the Pod in both client-authentication modes below ([Workload identity](../gateways/llm/workload-identity.md)). The two report paths have no bearer-token mode: the listener admits either SAN form, and the handler splits by workload kind, so an AgentTask certificate on `/v1/agent/heartbeat`, or an Agent certificate on `/v1/task/complete`, gets `403 access_denied`.

### TLS requirements

Two TLS requirements apply to all calls to `$KAALM_GATEWAY_ENDPOINT`.

**Server verification.** The agent must trust the Kaalm CA certificate at `$KAALM_CA_CERT` (`/var/run/kaalm/ca.crt`) to verify the gateway's TLS certificate. The CA is managed by cert-manager (see [Certificate lifecycle](../operations/deployment.md#certificate-lifecycle)).

**Client authentication.** One of two modes, depending on how the workload was provisioned. [Workload identity](../gateways/llm/workload-identity.md) is the canonical reference for both modes, including the SAN shapes and how the gateway extracts the namespace.

- **mTLS** (Kaalm-managed Pods). The AgentReconciler (for Agents) and the AgentTaskReconciler (for AgentTasks) create a cert-manager `Certificate` for the Pod; the cert and key are mounted at `$KAALM_TLS_CERT` / `$KAALM_TLS_KEY`. The agent must present this client certificate on every request to the gateway. The gateway extracts the agent's namespace from the certificate SAN: `{name}.{namespace}.svc.cluster.local` for Agents, `{name}.{namespace}.task.kaalm.io` for AgentTasks. The task SAN shape avoids implying a Service the task does not have. This is the only mode accepted for Pods managed by Kaalm.
- **ServiceAccount bearer token** (gateway-only tier). Workloads not managed by an Agent resource present a projected ServiceAccount token in the `Authorization: Bearer <jwt>` header. The gateway validates it with the Kubernetes `TokenReview` API and extracts the namespace from the validated `status.user.username`. No client cert is required.

The starter templates implement the mTLS mode. The bearer-token mode is for existing gateway-only-tier images, configured per [Tiered on-ramp](../operations/deployment.md#tiered-on-ramp): an audience-bound projected token plus a manual `kaalm-ca` ConfigMap mount. The controller injects nothing into gateway-only Pods. Custom images must configure their HTTP client for the appropriate mode.

### TLS material layout

The TLS material is delivered as a single projected volume at `/var/run/kaalm/`. It combines the cert-manager Secret (`tls.crt`, `tls.key`) and the trust-manager CA ConfigMap (`ca.crt`). The single mount directory has one kubelet-managed `..data` symlink covering all three files. That single directory is what makes the one-directory rotation watch in the starter templates sufficient (see [Starter templates](starter-templates.md) item 4).

![Inside /var/run/kaalm/, tls.crt, tls.key, and ca.crt are symlinks pointing at ..data/<key>, and ..data is a symlink to a timestamped directory holding the real files. A second timestamped directory, drawn in green, is where a rotation lands: the kubelet writes it first, then renames ..data onto it.](../diagrams/projected-volume-rotation.svg)

On rotation the kubelet writes the new files into a fresh timestamped directory and then atomically renames `..data` onto it. No leaf file is ever written in place, which is what [Why the watch is on the directory, not the file](starter-templates.md#why-the-watch-is-on-the-directory-not-the-file) follows from.

## 4. Message endpoint

This item is optional: agents without an AgentChannel do not need to implement it.

If the agent uses an AgentChannel, it exposes `POST /v1/message` on `$KAALM_HEALTH_PORT` over TLS. The endpoint accepts the standard Kaalm message envelope and returns a response envelope. The agent serves TLS using the cert-manager-issued certificate at `$KAALM_TLS_CERT` (`/var/run/kaalm/tls.crt`) and key at `$KAALM_TLS_KEY` (`/var/run/kaalm/tls.key`).

### Certificate reload on rotation

Agents must watch the cert, key, and CA-bundle files for changes. The kubelet automatically updates projected volume contents when the backing Secret or ConfigMap is rotated (see [Lifecycle of an agent TLS serving certificate](../security/tls.md#lifecycle-of-an-agent-tls-serving-certificate)). On a change, the agent reloads its TLS configuration for new connections without dropping existing ones. The obligation differs by file:

- A cert/key change reloads the serving certificate, and the outbound client certificate.
- A CA-bundle change MUST rebuild both trust pools: the inbound server's `ClientCAs` pool used for the client-cert verification below, and the outbound client's trust pool.

The both-pools rule matters during a CA re-key. A stale `ClientCAs` pool rejects the gateway's re-issued client certificate and breaks message delivery, exactly as a stale outbound pool breaks gateway calls. The re-key runbook's dual-trust window, during which old and new CA are projected together, is finite; [CA renewal and re-key](../security/tls.md#ca-renewal-and-re-key) walks the window and this failure through the runbook step by step. An agent that misses the CA-bundle reload eventually breaks in both directions once gateway leaves are re-issued under the new key.

Standard approaches: in Go, a `tls.Config.GetCertificate` callback plus `GetConfigForClient` returning a config with the fresh `ClientCAs` pool (both are consulted on each new TLS handshake); in Python, an `SSLContext` swap on an `inotify` event. The starter templates implement this reload pattern (see [Starter templates](starter-templates.md)).

### Client-certificate verification on /v1/message

Servers handling `POST /v1/message` MUST verify the gateway's client certificate. Enforcement is per path, not at the handshake. The listener shares `$KAALM_HEALTH_PORT` with `/readyz` and `/livez`, and the kubelet presents no client certificate on probes, so the TLS layer must request but not require one. In Go: `tls.Config.ClientAuth = tls.VerifyClientCertIfGiven` with `ClientCAs` populated from `$KAALM_CA_CERT`; equivalent in other runtimes.

Enforcement happens at the handler:

- `POST /v1/message` rejects requests with no peer certificate with `401 Unauthorized`.
- `POST /v1/message` rejects cert-bearing requests whose SAN does not match the gateway Service DNS (`kaalm-gateway.kaalm-system.svc.cluster.local` or `kaalm-gateway.kaalm-system.svc`) with `403 Forbidden`.
- `/readyz` and `/livez` pass unauthenticated.

![Sequence diagram of the agent as a server. The kubelet GETs /readyz and /livez over HTTPS with no client certificate and gets 200 when healthy. The gateway POSTs /v1/message over mTLS with its client certificate, and the agent answers 401 Unauthorized when no peer certificate was presented, 403 Forbidden when the SAN is not the gateway Service DNS, and 200 with a response envelope when it matches.](../diagrams/agent-inbound-calls.svg)

The two callers share one port with no single handshake policy, which is why the 401 and 403 are decided at the handler. `RequireAndVerifyClientCert` would fail every probe at the handshake, before the router ran, and leave the agent permanently unready. The same certificate serves this listener and is presented as the client certificate on every call in item 3.

This is the same per-path pattern the controller's `:9443` and the gateway's `:8443` listeners use. [NetworkPolicy](../security/model.md#network-policy) enforcement remains a prerequisite; mTLS is the second layer above it. See [In-cluster TLS](../security/tls.md#in-cluster-tls).

## 5. Activity signal (Agent only)

This item is optional. For idle detection, the agent may emit activity heartbeats by calling `POST /v1/agent/heartbeat` on the gateway. The gateway tracks these timestamps in-memory, with no etcd writes. Alternatively, the gateway infers activity from observed LLM and channel traffic.

Heartbeats are meaningful only for Agents: idle detection and hibernation do not apply to one-shot tasks. The endpoint enforces this. AgentTask callers are rejected with `403` at the handler (see [the :8443 listener profile](../gateways/overview.md#the-8443-listener-profile)). Task images must not run a heartbeat loop. The controller reads the timestamps through the [activity tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api) rather than from Pod annotations, so no request causes an etcd write.

## 6. Completion signal (AgentTask only)

This item is optional. The agent reports completion to the gateway with `POST /v1/task/complete` and a status payload that may include artifact key-value pairs.

Agents SHOULD retry `/v1/task/complete` on `403 access_denied` with `reason=StalePodCompletion` using bounded backoff (suggested: 100ms, 500ms, 2s; 3 attempts max). This handles the brief reconciler-observation lag between Pod creation and `AgentTask.status.currentPodUID` being stamped. See [POST /v1/task/complete](../gateways/api/task-complete.md) for the identity-gate rationale and [Retry mechanics](../controller/task-lifecycle.md) for the clear/reset/create/restamp ordering.

`403 access_denied` with `reason=TaskAlreadyCompleted` is not retryable. The AgentTask has reached a terminal phase (`Succeeded` / `Failed` / `TimedOut`) and further completion writes are by-design rejected. The agent should log and exit.

## 7. Message deduplication

This item is required for all agents implementing `POST /v1/message`. Each message delivered through `POST /v1/message` carries a unique `messageId` generated by the gateway.

### Where duplicates come from

Caller retries are not the source of same-`messageId` redelivery. In sync mode, if a wake takes longer than the webhook caller's HTTP timeout, the caller receives 504 and commonly retries. The gateway delivers that retry as a new message with a fresh `messageId` (see [The activator](../gateways/user/activation-and-activity.md#the-activator)).

The same-`messageId` case arises from the gateway's own agent-delivery retry pipeline: up to 3 retries with 1s, 5s, 25s backoff, 4 attempts total. See [async responses](../gateways/api/async-responses.md) for the schedule arithmetic and `delivery_failed` semantics. If an earlier attempt actually reached the agent and started side-effecting work, but the gateway's read of the response failed, the next retry redelivers the same `messageId`.

### Dedup obligation and scope

The retry pipeline applies to every agent, hibernated or not, so all agents MUST implement `messageId`-based deduplication. Buffer received IDs, scoped to the session or a rolling time window, and return a cached response for duplicates without reprocessing.

- An in-memory LRU is sufficient for non-hibernated agents.
- Agents with `hibernationEnabled: true` MUST additionally persist the dedup buffer across pod restarts, so a wake-on-demand replacement Pod still recognizes a `messageId` delivered before the restart. A PVC is always available for this, since `hibernationEnabled: true` requires `spec.persistence.enabled: true` ([rule 29](../resources/validation-and-defaulting.md#cross-resource-validation), `HibernationRequiresPersistence`).

The starter templates implement this as an in-memory LRU over the last 1024 `messageId`s; hibernation-enabled adopters layer PVC-backed persistence on top.

`messageId` dedup covers gateway-retry duplicates only. External replay of the inbound webhook (see [Threat model](../security/threat-model.md)) generates a fresh `messageId` per delivery and is not covered. Agents performing non-idempotent inbound actions must additionally dedup on a caller-supplied idempotency key or content hash.

## 8. Trace-context propagation

This item is required for all agents implementing `POST /v1/message`, and it is a header copy, not an SDK obligation. The gateway's delivery request may carry the W3C `traceparent` and `tracestate` headers. While handling that message, the agent must attach the same header values to every call it makes to the gateway (LLM requests, tool calls, and any other gateway endpoint). A delivery without the headers obligates nothing, and the agent must never invent trace context of its own.

Both reference runtimes implement the item invisibly, so handlers change nowhere: the Go module carries the values on the handler's `ctx` (readable through `agentruntime.TraceContext`) and injects them in its gateway client, and the Python base image captures them in a context variable around the handler call and injects them through `kaalm.gateway` and the `kaalm.http_client()` / `kaalm.http_async_client()` factories, exposing them as `kaalm.trace_context()` for frameworks that run their own OpenTelemetry SDK. See [Tracing](../operations/observability.md#tracing) for the spans this propagation connects.

## Starter templates

Kaalm ships starter templates (one Go, one Python) under `examples/starter-go/` and `examples/starter-python/`. Each template delivers the full runtime contract end-to-end: HTTPS serving on `$KAALM_HEALTH_PORT`, mTLS client certificate presentation on gateway calls, cert-file watch and reload, a `/v1/message` handler skeleton, `messageId`-based deduplication, and a task-completion helper with the bounded `StalePodCompletion` retry from item 6. They deliver it by consuming the single-sourced runtime (the Go module, the Python base image) rather than by carrying their own copies.

The templates target Kaalm-managed (mTLS-tier) workloads; gateway-only-tier workloads are pre-existing images configured per [Tiered on-ramp](../operations/deployment.md#tiered-on-ramp). See [Starter templates](starter-templates.md).

The published [reference base images](base-images.md) are the canonical implementations of this contract and the primary on-ramp: they embed the contract runtime, and a developer supplies only a handler (mounted from a ConfigMap through [`Agent.spec.handler`](../resources/agent.md), or layered on with `FROM`). The templates consume the same single-sourced runtime code, so the contract behavior is identical across base images, templates, and anything built from either.
