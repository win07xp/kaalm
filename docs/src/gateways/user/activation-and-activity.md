# Activation and Activity Tracking

Two mechanisms keep hibernation honest, and they run in opposite directions.

The **activator** is the wake path: a message arrives for an Agent that has been scaled to zero, and something has to bring the Pod back before the message can be delivered. The **activity tracking API** is the sleep path: the controller needs to know when an Agent last did anything, and that knowledge lives in the gateway's memory rather than in etcd.

Both cross the gateway/controller boundary, and both are mTLS-authenticated with SAN-based authorization.

## The Activator

When an Agent is in the `Hibernated` phase, its Service has no endpoints. The gateway detects this **lazily, on delivery**, with a bounded connect timeout (`gateway.agentDeliveryConnectTimeout`, default 1s). Any connect-phase failure inside that window is the hibernation signal:

- A **TCP RST** from iptables-mode kube-proxy against a ClusterIP Service with empty endpoints.
- A **connect timeout** on service data paths that drop packets to empty-endpoint ClusterIPs (IPVS, Cilium kube-proxy replacement, eBPF).

There is no separate Endpoint or EndpointSlice watch on the gateway. Connect failure is the whole detection mechanism.

Transient network failures take the same path, so the design has to tolerate a wake request for an Agent that was never actually hibernated. It does: on a still-`Running` Agent, the manual-wake handler in [`AgentReconciler` step 9](../../controller/reconcilers.md#agentreconciler) removes the annotation immediately and emits a `WakeIgnored` warning event without changing phase. That handler is purpose-built so stale annotations cannot trigger spurious wakes (see [Wake trigger](../../controller/hibernation-and-wake.md#wake-trigger)).

On clusters whose service data path drops rather than RSTs to empty-endpoint ClusterIPs, lazy detection adds up to one connect-timeout to the user-visible wake latency. This is a **per-wake cost, not per-message**: once the Pod is Ready, subsequent dials succeed immediately.

### The wake sequence

![Sequence diagram of waking a hibernated Agent. The User Gateway dials POST /v1/message at the Agent Service and the connect fails; a note records that connect-phase failure is the whole detection mechanism, arriving either as a TCP RST from iptables-mode kube-proxy or as a connect timeout within agentDeliveryConnectTimeout on data paths that drop to empty-endpoint ClusterIPs, with no Endpoint or EndpointSlice watch involved. When the controller is reachable, the gateway POSTs /v1/activate/{ns}/{name} over mTLS to the controller activator on :9443, which lands on any replica; that replica patches agentry.io/wake=true via the apiserver and returns 202 Accepted immediately. A highlighted note marks that the 202 precedes the wake and only confirms the annotation was written. The apiserver then fires the Agent watch on the leader only, whose AgentReconciler step 9 transitions Hibernated to Resuming and recreates the Pod; a second highlighted note marks that the replica which took the call is not the one that does the work, because the apiserver is the message bus. The gateway meanwhile polls the Agent Service for readiness rather than learning it from the activator response, bounded by wakeTimeout at 120s, then delivers. Two failure arms: wakeTimeout exceeded yields wake_timeout (504 in sync mode, an error payload to callbackUrl or the polling endpoint in async mode), and an unreachable controller yields controller_unavailable (504 with Retry-After: 5 in sync mode), leaving the Agent Hibernated with the wake never attempted.](../../diagrams/wake-sequence.svg)

Reading the diagram: the two facts that surprise people are both visible in the ordering. The `202` is returned before any wake work happens, and the replica that answers the POST is not the replica that drives the reconcile. Both fall out of the same design choice: the activator writes an annotation and lets the leader's existing watch do the rest.

The gateway serves as the activator:

1. A channel message arrives at the User Gateway targeting a hibernated Agent (via AgentChannel).
2. The gateway calls the controller's activator endpoint (`POST /v1/activate/{namespace}/{agentName}` on the controller's ClusterIP Service, port `:9443`) to signal a wake request. **This call is mTLS over HTTPS.** The controller serves its activator endpoint with a cert-manager-issued `Certificate` (`agentry-controller-tls`) signed by the same `ClusterIssuer` (`agentry-ca-issuer`) that signs the gateway cert, so the gateway verifies the controller's cert against the Agentry CA and the controller verifies the gateway's client cert against the same CA. See [Activator Authentication](#activator-authentication) below.
3. The activator handler (served on **every controller replica**) patches `agentry.io/wake=true` on the target Agent via the apiserver. The leader's existing Agent watch observes the annotation and runs the manual-wake path in `AgentReconciler` step 9, which transitions the Agent from `Hibernated` to `Resuming` and recreates the Pod. The handler does not need to be on the leader: any replica that receives the POST can patch the annotation, and the leader picks it up through the watch. See [Agent State Machine](../../controller/agent-lifecycle.md) for the full lifecycle and [Operator Structure](../../controller/overview.md) for the handler wiring.
4. The gateway waits for the Pod to become Ready (bounded by `spec.lifecycle.wakeTimeout`, which defaults from [AgentClass](../../resources/agentclass.md)), then delivers the message. If the timeout is exceeded:
   - **Sync mode**: the gateway returns HTTP 504 to the webhook caller.
   - **Async mode**: the gateway delivers a `wake_timeout` error payload to `callbackUrl` (if configured, with retries) or stores it at the polling endpoint under the original `requestId`. The error expires after 1 hour, same as successful responses. See [Async Webhook Response](../api/async-responses.md) for the error payload schema.
5. **Controller unreachable**: if the gateway cannot reach the controller's activator endpoint at all (connection refused, TLS handshake fails, client-cert authorization rejection, or 5xx after one internal retry), the wake cannot be attempted. The Agent remains `Hibernated`. In sync mode, the gateway returns `504 Gateway Timeout` with an error body carrying `error.type: controller_unavailable` and `retryable: true`. In async mode, the gateway delivers a `controller_unavailable` error payload to `callbackUrl` (with retries) or stores it at the polling endpoint. See [Async Webhook Response](../api/async-responses.md). Already-`Running` agents are unaffected: this failure mode only impacts wake-on-demand.

The placement of step 3 is the part worth remembering. The wake handler is an HTTP endpoint on the controller, not on the gateway, and it is live on every replica rather than only the leader. It does no lifecycle work itself; it writes one annotation and lets the normal watch-driven reconcile do the rest.

### Sync-mode retry risk

In sync mode, a wake can take longer than the webhook caller's HTTP timeout. Callers commonly time out at 30-60s, which is shorter than the default `wakeTimeout` of 2 minutes. The caller receives a 504 and will typically retry the webhook call.

The gateway treats that retry as a **new delivery** with a fresh gateway-generated `messageId`. Because `messageId` is gateway-side and not derived from the caller's payload, caller retries do not produce same-`messageId` redelivery.

The same-`messageId` case is driven separately, by the gateway's own agent-delivery retry pipeline (up to 3 retries; see [Async Webhook Response](../api/async-responses.md) for the schedule and `delivery_failed` semantics). If an earlier attempt actually reached the agent but the gateway's read of the response failed, the next retry redelivers the same `messageId`. Agents with `hibernationEnabled: true` must deduplicate on `messageId` to handle that case. See the [Agent Runtime Contract](../../runtime/contract.md).

### Activator Authentication

The activator endpoint authenticates callers via **mTLS**. There is no shared-secret layer on top of TLS. The controller's activator listener requires a client certificate on every connection:

- The gateway presents its `agentry-gateway-tls` cert as the client cert when calling `POST /v1/activate/{namespace}/{agentName}`.
- The controller verifies the client cert against the Agentry CA (`agentry-ca`) and authorizes the request only if the cert's SAN matches the gateway Service DNS (`agentry-gateway.agentry-system.svc.cluster.local` or `.svc`). Any other SAN, even one signed by `agentry-ca`, is rejected with `403 Forbidden`.

Both certs are issued and rotated continuously by cert-manager from `agentry-ca-issuer`, so there is no separate Secret to manage or rotate; the full trust chain is described in [In-cluster TLS](../../security/tls.md#in-cluster-tls). See [Internal Endpoint Authentication](../../security/rbac.md#internal-endpoint-authentication) for the matching SAN authorization rules on the reverse direction (controller → gateway activity API).

---

## Activity Tracking API

The gateway maintains per-agent activity timestamps **in-memory**, updated on every LLM request, channel message delivery, and agent heartbeat. This avoids per-request etcd writes: v1 targets 1000 Agents/AgentTasks per cluster, and the in-memory store is deliberately designed to scale an order of magnitude higher without a design change as future versions grow the target. The controller uses this data to evaluate idle and hibernation transitions. See [Activity Detection](../../controller/hibernation-and-wake.md#activity-detection).

Note that heartbeats are an **Agent-only** signal: `POST /v1/agent/heartbeat` rejects AgentTask callers with `403` at the handler, since idle detection does not apply to one-shot tasks. See [POST /v1/agent/heartbeat](../api/agent-endpoints.md#post-v1agentheartbeat).

### The endpoint

The gateway exposes an internal endpoint for the controller to query activity state. The endpoint serves **HTTPS** using the gateway's `agentry-gateway-tls` Certificate and **requires an mTLS client cert on this path**. The controller presents its `agentry-controller-tls` cert; the gateway verifies against `agentry-ca` and authorizes only if the client cert's SAN matches the controller Service DNS (`agentry-controller.agentry-system.svc.cluster.local` or `.svc`). There is no separate shared-secret or bearer-token layer on top of the mTLS tunnel. See [Internal Endpoint Authentication](../../security/rbac.md#internal-endpoint-authentication).

**`GET /v1/activity?namespace={ns}`**

Returns a JSON object containing the gateway's startup timestamp and a map of agent names to their last-activity timestamps, broken out by signal source, for the given namespace:

```json
{
  "replicaStartedAt": "2026-04-05T06:00:00Z",
  "agents": {
    "support-assistant": {
      "gatewayTraffic": "2026-04-05T11:58:22Z",
      "heartbeat": "2026-04-05T11:57:10Z"
    },
    "code-helper": {
      "gatewayTraffic": "2026-04-05T11:45:10Z",
      "heartbeat": null
    }
  }
}
```

The gateway tracks both signal sources (gateway-observed LLM and channel traffic, and agent heartbeats) separately per agent and always returns both. The controller applies the `activitySource` filter (from `Agent.spec.lifecycle.activitySource`) after merging results across replicas, selecting `gatewayTraffic`, `heartbeat`, or the max of both depending on the setting. The gateway does not need to read Agent specs to perform this filtering; the controller owns the policy.

A `null` value for a source means the gateway has no record of that signal type for the agent since its last restart.

The `replicaStartedAt` field indicates when the gateway started. The controller uses this to detect gateway restarts: if the gateway started more recently than an agent's `status.phaseTransitionTime` (a dedicated Agent status field set by the AgentReconciler on every phase change, see the [Agent CRD design notes](../../resources/agent.md)), missing activity data is treated as "unknown" rather than "no activity".

### Multi-replica fan-out

Each gateway replica maintains its own in-memory activity store, updated only by the traffic that replica handles. Querying the gateway ClusterIP Service (which round-robins to one replica) would therefore return only that replica's view, and agents whose last request landed on a different replica would appear idle.

The controller instead queries **all gateway Pod IPs directly, in parallel**: it enumerates gateway Pods via its Pod informer (matching the gateway label selector in `agentry-system`) and issues one `GET /v1/activity?namespace={ns}` request per Pod IP. It takes the **most recent timestamp per agent per source** across all responses. Replicas that are unreachable (connection refused, timeout) are skipped; data from the remaining replicas is used.

![Sequence diagram of the AgentReconciler's activity fan-out. The reconciler first asks its reconciler-local 15-second per-namespace cache for namespace X. On a cache hit inside the window it gets the per-agent timestamps for both sources with zero HTTP, which is how every other agent reconcile in that namespace is served. On a cache miss or expired window, the reconciler fans out in parallel, issuing one GET /v1/activity?namespace=X per gateway Pod IP: Pod A and Pod B return their replicaStartedAt plus per-agent gatewayTraffic and heartbeat timestamps, while Pod C is unreachable and is skipped, with the remaining replicas' data still used. A note records that the calls are dialed by Pod IP rather than the Service, because the Service round-robins and would return a single replica's partial view, and that because Pod IPs are absent from the gateway cert's SAN the transport pins tls.Config.ServerName to the gateway Service DNS so SAN verification still passes against agentry-ca. The reconciler then merges the most recent timestamp per agent per source, stores the result for 15 seconds, and only then applies the activitySource filter. A legend notes that the cache turns O(agents x replicas) HTTP calls per window into O(namespaces x replicas).](../../diagrams/activity-fanout.svg)

Reading the diagram: two separate reductions are stacked here. The cache collapses the call count from per-agent to per-namespace, and the merge collapses the per-replica partial views into one. The `activitySource` filter deliberately sits last, after the merge, because the gateway returns both sources unconditionally and the controller owns the policy.

Restart detection is likewise per-replica: the `replicaStartedAt` field in each response is evaluated on its own, so if one replica has restarted more recently than an agent's `status.phaseTransitionTime`, only that replica's data is treated as unknown. See [AgentReconciler](../../controller/reconcilers.md#agentreconciler) for the reconciler implementation detail, including the per-namespace 15-second cache that keeps this fan-out from running once per agent reconcile.

### TLS verification on per-Pod-IP dials

Dialing Pod IPs breaks ordinary SAN verification, and the fix is worth understanding before you read the controller code.

The gateway cert's SAN list covers the gateway's Service DNS (`agentry-gateway.agentry-system.svc.cluster.local`, `.svc`, `localhost`), not Pod IPs, which would be impractical to enroll. The controller's HTTP transport therefore sets `tls.Config.ServerName = "agentry-gateway.agentry-system.svc.cluster.local"` for these per-Pod-IP dials, so SAN verification succeeds against the Service DNS while the dial target remains the Pod IP.

Cert authenticity is unchanged: verification still chains to `agentry-ca`, and the SAN match is performed against the explicit `ServerName`. Without this override, every fan-out dial would fail TLS verification because the Pod IP does not appear in the cert's SAN.

### Query cadence and restart behavior

The controller queries all replica Pod IPs on each reconcile for agents in `Running` or `Idle` phase to evaluate idle and hibernation transitions. If all replicas are unreachable, the controller preserves the agent's current phase: no idle transitions are made without activity data.

Activity data is ephemeral. It is lost on gateway restart. The gateway includes its `replicaStartedAt` timestamp in the `/v1/activity` response so the controller can detect this condition. After a gateway restart:

- The controller defers idle and hibernation transitions for agents whose last phase transition predates the gateway's `replicaStartedAt`, treating missing data as "unknown" until the gateway has been running for at least `idleTimeout`.
- Agents that are actively sending traffic re-establish their activity timestamps immediately.
- Agents that are truly idle will transition to `Idle` after `idleTimeout` elapses from the gateway's startup, which is the correct behavior.
