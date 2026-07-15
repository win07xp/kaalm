# The Runtime Contract

Agentry is BYO-image: any container can run as an Agent or AgentTask, provided it implements a small contract. This page specifies that contract. It is the minimum a container image must satisfy to participate in the lifecycle: HTTPS health endpoints on the injected health port, graceful SIGTERM handling, authenticated TLS calls to the injected gateway endpoint, an optional `POST /v1/message` handler when an AgentChannel is in use, and `messageId` deduplication on that handler.

The contract has seven numbered items. Other pages cite them by number, so the numbering is stable. For the surrounding system, see [System Architecture](../concepts/system-architecture.md). Working implementations of every item ship as Go and Python [starter templates](starter-templates.md), summarized at the end of this page.

## 1. HTTPS health endpoints

The container serves two health endpoints on a known port (`$AGENTRY_HEALTH_PORT`, default 8080):

- `GET /readyz` (readiness), returning 200 when healthy.
- `GET /livez` (liveness), returning 200 when healthy.

These are the paths the controller-injected probes target (see [AgentReconciler step 7](../controller/reconcilers.md#agentreconciler)). The agent serves TLS on this port using the cert-manager-issued per-Agent certificate (`$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY`). Liveness and readiness probes must therefore be configured with `httpGet.scheme: HTTPS`. Kubernetes does not verify TLS certificates for httpGet probes, so the certificate works without any additional CA configuration on the probe.

## 2. Graceful SIGTERM handling

On receiving SIGTERM, the agent should finish in-flight work and exit within the configured `terminationGracePeriodSeconds`.

## 3. Gateway communication

The controller injects `$AGENTRY_GATEWAY_ENDPOINT`: an HTTPS URL pointing to the gateway's LLM listener (port 8443). This is the base URL for all agent to gateway calls:

- LLM requests.
- Heartbeats: [`POST /v1/agent/heartbeat`](../gateways/api/agent-endpoints.md#post-v1agentheartbeat), Agents only, see item 5.
- Task completion: [`POST /v1/task/complete`](../gateways/api/task-complete.md), AgentTasks only, see item 6.

The variable is always injected, whether or not `spec.providers` is set. Provider-less workloads can therefore still reach the gateway: Agents for heartbeats, AgentTasks for task completion.

### TLS requirements

Two TLS requirements apply to all calls to `$AGENTRY_GATEWAY_ENDPOINT`.

**Server verification.** The agent must trust the Agentry CA certificate at `$AGENTRY_CA_CERT` (`/var/run/agentry/ca.crt`) to verify the gateway's TLS certificate. The CA is managed by cert-manager (see [Certificate Lifecycle](../operations/deployment.md#certificate-lifecycle)).

**Client authentication.** One of two modes, depending on how the workload was provisioned. [Workload identity](../gateways/llm/workload-identity.md) is the canonical reference for both modes, including the SAN shapes and how the gateway extracts the namespace.

- **mTLS** (Agentry-managed Pods). The AgentReconciler (for Agents) and the AgentTaskReconciler (for AgentTasks) create a cert-manager `Certificate` for the Pod; the cert and key are mounted at `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY`. The agent must present this client certificate on every request to the gateway. The gateway extracts the agent's namespace from the certificate SAN: `{name}.{namespace}.svc.cluster.local` for Agents, `{name}.{namespace}.task.agentry.io` for AgentTasks. The task SAN shape avoids implying a Service the task does not have. This is the only mode accepted for Pods managed by Agentry.
- **ServiceAccount bearer token** (gateway-only tier). Workloads not managed by an Agent resource present a projected ServiceAccount token in the `Authorization: Bearer <jwt>` header. The gateway validates it with the Kubernetes `TokenReview` API and extracts the namespace from the validated `status.user.username`. No client cert is required.

The starter templates implement the mTLS mode. The bearer-token mode is for existing gateway-only-tier images, configured per [Tiered On-Ramp](../operations/deployment.md#tiered-on-ramp): an audience-bound projected token plus a manual `agentry-ca` ConfigMap mount. The controller injects nothing into gateway-only Pods. Custom images must configure their HTTP client for the appropriate mode.

### TLS material layout

The TLS material is delivered as a single projected volume at `/var/run/agentry/`. It combines the cert-manager Secret (`tls.crt`, `tls.key`) and the trust-manager CA ConfigMap (`ca.crt`). The single mount directory has one kubelet-managed `..data` symlink covering all three files. That single directory is what makes the one-directory rotation watch in the starter templates sufficient (see [Starter Templates](starter-templates.md) item 4).

## 4. Message endpoint

This item is optional: agents without an AgentChannel do not need to implement it.

If the agent uses an AgentChannel, it exposes `POST /v1/message` on `$AGENTRY_HEALTH_PORT` over TLS. The endpoint accepts the standard Agentry message envelope and returns a response envelope. The agent serves TLS using the cert-manager-issued certificate at `$AGENTRY_TLS_CERT` (`/var/run/agentry/tls.crt`) and key at `$AGENTRY_TLS_KEY` (`/var/run/agentry/tls.key`).

### Certificate reload on rotation

Agents must watch the cert, key, and CA-bundle files for changes. The kubelet automatically updates projected volume contents when the backing Secret or ConfigMap is rotated (see [Lifecycle of an agent TLS serving certificate](../security/tls.md#lifecycle-of-an-agent-tls-serving-certificate)). On a change, the agent reloads its TLS configuration for new connections without dropping existing ones. The obligation differs by file:

- A cert/key change reloads the serving certificate, and the outbound client certificate.
- A CA-bundle change MUST rebuild both trust pools: the inbound server's `ClientCAs` pool used for the client-cert verification below, and the outbound client's trust pool.

The both-pools rule matters during a CA re-key. A stale `ClientCAs` pool rejects the gateway's re-issued client certificate and breaks message delivery, exactly as a stale outbound pool breaks gateway calls. The re-key runbook's dual-trust window, during which old and new CA are projected together, is finite (see [In-cluster TLS](../security/tls.md#in-cluster-tls)). An agent that misses the CA-bundle reload eventually breaks in both directions once gateway leaves are re-issued under the new key.

Standard approaches: in Go, a `tls.Config.GetCertificate` callback plus `GetConfigForClient` returning a config with the fresh `ClientCAs` pool (both are consulted on each new TLS handshake); in Python, an `SSLContext` swap on an `inotify` event. The starter templates implement this reload pattern (see [Starter Templates](starter-templates.md)).

### Client-certificate verification on /v1/message

Servers handling `POST /v1/message` MUST verify the gateway's client certificate. Enforcement is per path, not at the handshake. The listener shares `$AGENTRY_HEALTH_PORT` with `/readyz` and `/livez`, and the kubelet presents no client certificate on probes, so the TLS layer must request but not require one. In Go: `tls.Config.ClientAuth = tls.VerifyClientCertIfGiven` with `ClientCAs` populated from `$AGENTRY_CA_CERT`; equivalent in other runtimes.

Enforcement happens at the handler:

- `POST /v1/message` rejects requests with no peer certificate with `401 Unauthorized`.
- `POST /v1/message` rejects cert-bearing requests whose SAN does not match the gateway Service DNS (`agentry-gateway.agentry-system.svc.cluster.local` or `agentry-gateway.agentry-system.svc`) with `403 Forbidden`.
- `/readyz` and `/livez` pass unauthenticated.

This is the same per-path pattern the controller's `:9443` and the gateway's `:8443` listeners use. [NetworkPolicy](../security/model.md#network-policy) enforcement remains a prerequisite; mTLS is the second layer above it. See [In-cluster TLS](../security/tls.md#in-cluster-tls).

## 5. Activity signal (Agent only)

This item is optional. For idle detection, the agent may emit activity heartbeats by calling `POST /v1/agent/heartbeat` on the gateway. The gateway tracks these timestamps in-memory, with no etcd writes. Alternatively, the gateway infers activity from observed LLM and channel traffic.

Heartbeats are meaningful only for Agents: idle detection and hibernation do not apply to one-shot tasks. The endpoint enforces this. AgentTask callers are rejected with `403` at the handler (see the `:8443` auth profile in [The Agentry Gateway](../gateways/overview.md)). Task images must not run a heartbeat loop.

## 6. Completion signal (AgentTask only)

This item is optional. The agent reports completion to the gateway via `POST /v1/task/complete` with a status payload that may include artifact key-value pairs.

Agents SHOULD retry `/v1/task/complete` on `403 access_denied` with `reason=StalePodCompletion` using bounded backoff (suggested: 100ms, 500ms, 2s; 3 attempts max). This handles the brief reconciler-observation lag between Pod creation and `AgentTask.status.currentPodUID` being stamped. See [POST /v1/task/complete](../gateways/api/task-complete.md) for the identity-gate rationale and [Retry mechanics](../controller/task-lifecycle.md) for the clear/reset/create/restamp ordering.

`403 access_denied` with `reason=TaskAlreadyCompleted` is not retryable. The AgentTask has reached a terminal phase (`Succeeded` / `Failed` / `TimedOut`) and further completion writes are by-design rejected. The agent should log and exit.

## 7. Message deduplication

This item is required for all agents implementing `POST /v1/message`. Each message delivered via `POST /v1/message` carries a unique `messageId` generated by the gateway.

### Where duplicates come from

Caller retries are not the source of same-`messageId` redelivery. In sync mode, if a wake takes longer than the webhook caller's HTTP timeout, the caller receives 504 and commonly retries. The gateway delivers that retry as a new message with a fresh `messageId` (see [Activator](../gateways/user/activation-and-activity.md#the-activator)).

The same-`messageId` case arises from the gateway's own agent-delivery retry pipeline: up to 3 retries with 1s, 5s, 25s backoff, 4 attempts total. See [async responses](../gateways/api/async-responses.md) for the schedule arithmetic and `delivery_failed` semantics. If an earlier attempt actually reached the agent and started side-effecting work, but the gateway's read of the response failed, the next retry redelivers the same `messageId`.

### Dedup obligation and scope

The retry pipeline applies to every agent, hibernated or not, so all agents MUST implement `messageId`-based deduplication. Buffer received IDs, scoped to the session or a rolling time window, and return a cached response for duplicates without reprocessing.

- An in-memory LRU is sufficient for non-hibernated agents.
- Agents with `hibernationEnabled: true` MUST additionally persist the dedup buffer across pod restarts, so a wake-on-demand replacement Pod still recognizes a previously-delivered `messageId`. A PVC is always available for this, since `hibernationEnabled: true` requires `spec.persistence.enabled: true` ([rule 29](../resources/validation-and-defaulting.md#cross-resource-validation), `HibernationRequiresPersistence`).

The starter templates implement this as an in-memory LRU over the last 1024 `messageId`s; hibernation-enabled adopters layer PVC-backed persistence on top.

`messageId` dedup covers gateway-retry duplicates only. External replay of the inbound webhook (see [Threat Model](../security/threat-model.md)) generates a fresh `messageId` per delivery and is not covered. Agents performing non-idempotent inbound actions must additionally dedup on a caller-supplied idempotency key or content hash.

## Communication summary

All agent to gateway and gateway to agent communication is over TLS. Agent to gateway traffic (LLM requests, heartbeats, task completion) is authenticated via mTLS for Agentry-managed Pods, or via a `TokenReview`-validated ServiceAccount bearer token for gateway-only-tier workloads, with a source-IP to Pod cross-check in both modes (see [Namespace Identification](../gateways/llm/workload-identity.md)). Gateway to agent traffic (channel message delivery) is also mTLS: the gateway verifies the agent's cert-manager-issued certificate against the Agentry CA (`agentry-ca`), and the agent enforces the SAN-match check on the gateway's client cert as required by item 4.

Activity timestamps are maintained in-memory in the gateway. The controller queries them via the [activity tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api) rather than reading Pod annotations, avoiding per-request etcd writes at scale.

## Starter templates

Agentry ships starter templates (one Go, one Python) under `examples/starter-go/` and `examples/starter-python/` as part of v1. Each template implements the full runtime contract end-to-end: HTTPS serving on `$AGENTRY_HEALTH_PORT`, mTLS client certificate presentation on gateway calls, cert-file watch and reload, a `/v1/message` handler skeleton, `messageId`-based deduplication, and a task-completion helper with the bounded `StalePodCompletion` retry from item 6.

The templates target Agentry-managed (mTLS-tier) workloads; gateway-only-tier workloads are pre-existing images configured per [Tiered On-Ramp](../operations/deployment.md#tiered-on-ramp). Adopters copy the template and replace the agent logic; the boilerplate stays. See [Starter Templates](starter-templates.md). Full-featured reference base images (published container images that embed and wrap the contract) are planned for a future release.
