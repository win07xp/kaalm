# Activation and activity tracking

Two mechanisms make hibernation work, and they run in opposite directions.

The **activator** is the wake path: a message arrives for an Agent whose Pod has been deleted, and something has to bring the Pod back before the message can be delivered. The **activity tracking API** is the sleep path: the controller needs to know when an Agent last did anything, and that knowledge lives in the gateway's memory rather than in etcd.

Both are calls between the gateway and the controller, one in each direction, and both are mTLS-authenticated with SAN-based authorization.

## The activator

Wake-on-demand is a hard dependency on the controller. While the activator endpoint is unreachable, a hibernated Agent cannot receive a message and the caller gets `controller_unavailable`; Agents that are already `Running` are unaffected. The chart's two-replica floor on the controller exists to keep this endpoint reachable through drains ([Deployment](../../operations/deployment.md#the-two-deployments)).

The gateway wakes an Agent when the Agent's cached `status.phase` is `Hibernated` at delivery time ([Request flow](overview.md#request-flow), step 7). It watches no Endpoints and infers nothing from connect failures: a connect failure during delivery is an ordinary failed attempt, retried on the delivery schedule. As shipped the trigger is the `Hibernated` phase alone, so a message that arrives while the Agent is `Hibernating`, with its Pod being deleted, is not woken and fails delivery.

### The wake sequence

![Sequence diagram of waking a hibernated Agent. The User Gateway sees the cached Agent phase Hibernated and POSTs /v1/activate/{namespace}/{name} over mTLS to the controller activator on :9443, on any replica. The activator patches kaalm.io/wake=true through the apiserver and answers 202 Accepted. The apiserver fires the Agent watch on the controller leader, which sets Resuming and creates the Pod. The gateway polls a TCP connect to the Agent Service every two seconds up to wakeTimeout, then POSTs /v1/message and receives the response envelope.](../../diagrams/wake-sequence.svg)

1. The gateway calls `POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service at `:9443` over mTLS, with a ten-second client timeout and no retry.
2. The activator handler, served on every controller replica, patches `kaalm.io/wake=true` on the Agent through the apiserver and answers `202 Accepted` before any wake work happens. The `202` confirms the annotation was written, not that the Agent is coming up.
3. The leader's Agent watch fires, and the wake handling in [AgentReconciler step 1](../../controller/reconcilers.md#agentreconciler) moves the Agent to `Resuming`; the next pass creates the Pod. The replica that answered the POST is not necessarily the leader: the annotation is the message, and the apiserver carries it. See [Wake trigger](../../controller/hibernation-and-wake.md#wake-trigger).
4. The gateway polls a TCP connect to the Agent Service every two seconds, bounded by the Agent's `status.effectiveWakeTimeout`: its `wakeTimeout` with the [class default and cap](../../controller/hibernation-and-wake.md#timing-knobs) applied, or 2 minutes when neither the Agent nor the class sets it. Then it delivers the message.

![Flowchart of how a wake fails, in the order the gateway finds out, as two rows. Wake: activator configured, reachable, and answering 202, else controller_unavailable (sync 504 with Retry-After 5); Agent Service reachable within wakeTimeout, else wake_timeout (sync 504). Deliver: sync deadline still open, else sync_deadline_exceeded (504, sync only); delivered within four attempts, else delivery_failed (sync 502); otherwise the reply.](../../diagrams/wake-failures.svg)

| Failure | Trigger | Sync mode | Async mode |
|---|---|---|---|
| `controller_unavailable` | No activator configured, a connect or TLS failure, a SAN rejection, the ten-second timeout, or any response other than `202` (a `404` for a deleted Agent, a `5xx`). The Agent stays `Hibernated`. | `504`, `Retry-After: 5`, `retryable: true` | error payload to `callbackUrl` or the polling endpoint |
| `wake_timeout` | The Service is not reachable within `wakeTimeout`. The controller keeps working; the next message calls the activator again, which is idempotent. | `504` | error payload |

Under default settings a sync caller never sees `wake_timeout`: `gateway.syncDeliveryDeadline` (30s) fires first and the caller gets `504 sync_deadline_exceeded`. [Sync-mode reachability](../api/async-responses.md#sync-mode-reachability) draws the three bounds on one axis, and [Response: sync mode](../api/channel-webhook.md#response-sync-mode) is the status table. The payload forms are under [Error payloads](../api/async-responses.md#error-payloads).

### Sync-mode retry risk

A channel backed by a hibernating Agent should use `responseMode: async`. In sync mode the caller gets `504 sync_deadline_exceeded` at 30 seconds by default, before a cold wake completes, and typically retries. The gateway treats that retry as a new delivery with a fresh gateway-generated `messageId`, so caller retries never redeliver the same id. The gateway's own delivery retries do reuse the `messageId`, so an agent that received an earlier attempt sees it again; agents deduplicate on `messageId` per [The runtime contract](../../runtime/contract.md).

### Activator authentication

Both directions authenticate by mTLS with SAN authorization, and neither carries a shared secret or bearer token on top of the tunnel.

| Direction | Client cert | Verified against | SAN required | Otherwise |
|---|---|---|---|---|
| Gateway to controller, `POST /v1/activate/...` | `kaalm-gateway-tls` | `kaalm-ca` | the gateway Service DNS (`kaalm-gateway.kaalm-system.svc.cluster.local` or `.svc`) | `401` without a client cert, `403` with any other SAN |
| Controller to gateway, `GET /v1/activity` and `GET /v1/channels/health` | `kaalm-controller-tls` | `kaalm-ca` | the controller Service DNS | the same |

Both certs are issued and rotated by cert-manager from `kaalm-ca-issuer`; the trust chain is under [In-cluster TLS](../../security/tls.md#in-cluster-tls) and the SAN rules under [Internal endpoint authentication](../../security/rbac.md#internal-endpoint-authentication).

---

## Activity tracking API

Each gateway replica keeps per-agent activity timestamps in memory, one per signal source: `gatewayTraffic`, stamped by a successful channel delivery and by every LLM request an Agent forwards through the proxy (AgentTask traffic is not tracked, since idle detection does not apply to tasks), and `heartbeat`, stamped by `POST /v1/agent/heartbeat`. The controller reads them to drive the idle and hibernation transitions. Why the store is in memory and how the controller evaluates it are under [Activity detection](../../controller/hibernation-and-wake.md#activity-detection).

Heartbeats are an Agent-only signal: the mTLS middleware rejects an AgentTask caller on that path with `403 access_denied`. See [POST /v1/agent/heartbeat](../api/agent-endpoints.md#post-v1agentheartbeat).

### The endpoint

`GET /v1/activity?namespace={ns}` on the cluster listener returns the replica's `replicaStartedAt` and, for each agent in the namespace it has seen, both source timestamps, `null` for a source it has not observed since it started. Both sources are always returned; the gateway never reads Agent specs, and the controller applies `Agent.spec.lifecycle.activitySource` after merging replicas. The wire contract, request, response, and status codes are under [GET /v1/activity](../api/internal-endpoints.md#get-v1activity).

### Multi-replica fan-out

Each replica records only the traffic it handled, so a query to the gateway Service, which round-robins to one replica, would show agents whose last request landed elsewhere as idle. The controller instead queries every gateway Pod IP directly.

![Sequence diagram of the AgentReconciler's activity read. The reconciler asks its per-namespace cache, valid for 15 seconds; on a hit it receives the cached replicas. On a miss it issues one GET /v1/activity?namespace=X per gateway Pod IP in parallel; Pods A and B return replicaStartedAt and agents, Pod C is unreachable. The reconciler stores the reachable replicas in the cache, takes the newest timestamp per agent per source, then applies activitySource.](../../diagrams/activity-fanout.svg)

1. The reconciler consults a per-namespace cache with a 15-second window. Every Agent reconcile in that namespace within the window is served from it, so the fan-out runs once per namespace per window rather than once per Agent.
2. On a miss it lists gateway Pods from its informer by the gateway label in `kaalm-system`, skips Pods without an IP or with a deletion timestamp, and issues one `GET /v1/activity?namespace={ns}` per Pod IP in parallel, with a five-second client timeout. A replica that is unreachable, times out, or answers anything but `200` is skipped; the reachable replicas are used.
3. The reachable replicas are cached for the window, with the count of targets.
4. For an Agent, the newest timestamp per source across replicas is taken, then `activitySource` selects `gatewayTraffic`, `heartbeat`, or the newer of the two.

### TLS verification on per-Pod-IP dials

The gateway cert's SAN list covers the gateway Service DNS names, not Pod IPs. The controller's transport therefore sets `tls.Config.ServerName` to `kaalm-gateway.kaalm-system.svc.cluster.local` for these dials, so SAN verification succeeds against the Service DNS while the dial target is the Pod IP. The chain is still verified against `kaalm-ca`.

### Query cadence and restart behavior

The fan-out runs on every reconcile of an Agent in `Running` or `Idle` whose effective `idleTimeout` is above zero, when the controller has a TLS identity to dial with. If no replica is reachable, the controller preserves the phase, sets `GatewayReachable=False` on the Agent, and requeues in 30 seconds. No idle or hibernation transition is made without activity data.

Activity data is lost on a gateway restart, and an empty store looks like silence. `replicaStartedAt` is how the controller tells them apart. There is no per-replica comparison: when no reachable replica has a record for the Agent, the controller proceeds only if some reachable replica has been up for at least `idleTimeout`, and then counts silence from the Agent's `status.phaseTransitionTime`; otherwise it defers and requeues in 30 seconds. A synchronized gateway restart therefore defers every idle and hibernation transition for `idleTimeout`. The full decision, and how a wake stamps `status.lastActivityTime` so a woken Agent gets a full `idleTimeout`, are under [When activity data is missing](../../controller/hibernation-and-wake.md#when-activity-data-is-missing).
