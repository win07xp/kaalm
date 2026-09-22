# Workload identity

Every decision the LLM Gateway makes after authentication depends on which namespace, and which workload, sent the request. [Budget accounting](budgets-and-rate-limits.md#budget-state-management) charges token spend to a namespace, and [provider routing](provider-routing.md) enforces namespace-scoped allowlists. A caller that could lie about its namespace could spend another team's budget or reach a provider it was never granted. The gateway therefore derives a trustworthy identity for every request before doing anything else.

## Two modes, two tiers

The gateway supports two authentication modes, one per Helm tier:

1. **mTLS client certificate**: the zero-configuration path for Pods the Kaalm controller created (Agent and AgentTask workloads).
2. **`TokenReview`-verified ServiceAccount bearer token**: the path for gateway-only-tier workloads, existing Deployments the controller did not create and that therefore have no cert-manager-issued client certificate.

A request that presents neither a client certificate nor an `Authorization: Bearer <token>` header is rejected with `401 Unauthorized`. A request presenting both is processed by the mTLS path, and the bearer token is ignored. At the handshake the listener runs `ClientAuth: tls.VerifyClientCertIfGiven`, and per-path HTTP middleware decides which mode each path accepts; see [Per-path client-auth enforcement](../listener-tls.md#per-path-client-auth-enforcement). Both modes end in the same [source-IP cross-check](#source-ip-cross-check-both-modes).

![Activity diagram of namespace identification on the cluster listener. A request with a client certificate takes Mode 1: the certificate is verified at the handshake, the SAN must be one recognized shape with five labels or the request is rejected with 403 invalid_cert, and the namespace and workload come from the SAN. A request without a certificate takes Mode 2: a missing bearer token is rejected with 401, a Kaalm-managed source Pod is rejected with 401, a token not in the cache goes to TokenReview with audience kaalm-gateway, an unreachable apiserver is 503 internal_unavailable, a rejected token is 401, and an accepted token is cached with a TTL from its exp claim before the namespace is read from the username. Both branches end in the cross-check that the Pod at the source IP is in that namespace, rejecting with 401 on mismatch, and then continue to provider routing.](../../diagrams/workload-auth-modes.svg)

| | Mode 1: mTLS client certificate | Mode 2: ServiceAccount bearer token |
|---|---|---|
| Who | Pods created by the AgentReconciler or AgentTaskReconciler | Workloads the platform team grants access to without the Agent CRD |
| Credential | The cert-manager-issued client certificate at `$KAALM_TLS_CERT` | A projected ServiceAccount token with audience `kaalm-gateway` |
| Verified by | The TLS handshake against `kaalm-ca`, then the SAN parser | A `TokenReview` against the apiserver, cached per token |
| Identity yielded | Namespace, workload name, and workload kind, from the SAN | Namespace only, from `status.user.username` |
| Rejections | A handshake failure, with no HTTP response; `403 invalid_cert` for a SAN that fails the shape or label-count rule | `401` for a missing token, a Kaalm-managed source Pod, or a rejected token; `503 internal_unavailable` when the apiserver is unreachable |
| Provider routing consults | The workload's `spec.providers`, its AgentClass's `allowedProviders`, then the ModelProvider's `allowedNamespaces` and `models` | The ModelProvider's `allowedNamespaces` and `models` only |

Both modes use the gateway's TLS listener, so every caller must also verify the gateway's serving certificate against the Kaalm CA. Kaalm-managed Pods find the bundle at `$KAALM_CA_CERT` (`/var/run/kaalm/ca.crt`); gateway-only workloads mount the `kaalm-ca` ConfigMap themselves (see [Caller-side setup](#caller-side-setup)). See [The runtime contract](../../runtime/contract.md).

## Mode 1: mTLS client certificate

Agents and tasks present the certificate at `$KAALM_TLS_CERT` (`/var/run/kaalm/tls.crt`) with the key at `$KAALM_TLS_KEY` (`/var/run/kaalm/tls.key`). The gateway verifies it against the Kaalm CA, using the trust bundle from the trust-manager-projected `kaalm-ca` ConfigMap (see [TLS on the cluster listener](../listener-tls.md)), and reads the identity from the certificate's DNS SAN. A certificate that fails CA verification is rejected at the TLS handshake itself: handshake failures produce no HTTP response and never appear in the [LLM Gateway error table](../api/errors.md#llm-gateway-error-responses). Two SAN shapes are recognized:

| SAN shape | Issued by | Labels | Workload kind |
|---|---|---|---|
| `{name}.{namespace}.svc.cluster.local` | The AgentReconciler; matches the Agent's Service DNS | 5 | Agent |
| `{name}.{namespace}.task.kaalm.io` | The AgentTaskReconciler; AgentTasks have no Service, so the shape names the kind instead of implying a Service the task does not have | 5 | AgentTask |

The namespace is the second label in both shapes, and the shape gives the workload kind. Every request therefore carries a cryptographically attested (namespace, workload name, workload kind) triple. The workload half underpins the `spec.providers` check, per-task ConfigMap targeting, and the Agent-versus-AgentTask handler split under [Per-path requirements](#per-path-requirements).

### Exact label-count enforcement

The SAN parser scans the certificate's DNS SAN list for entries ending in a recognized suffix, `.svc.cluster.local` or `.task.kaalm.io`. The per-Agent certificate also carries short-form Service SANs (`{name}.{namespace}.svc` and `{name}.{namespace}`); these match no recognized suffix and are ignored. Exactly one SAN must match a recognized shape, and it must have exactly five labels: `{name}.{namespace}` plus the three suffix labels. A recognized-suffix SAN with any other label count, or a certificate with zero or several recognized SANs, is rejected with `403 invalid_cert`.

This is defense in depth against a dotted-name bypass. Rule 21, a CRD CEL constraint enforced at apply time, restricts Agent and AgentTask `metadata.name` to DNS-1123 labels; see the [Agent CRD design notes](../../resources/agent.md). If that constraint were relaxed or bypassed, a name like `admin.svc` in namespace `team-a` would yield the SAN `admin.svc.team-a.svc.cluster.local`, six labels, and the parser would reject it before the namespace was read. Both layers must be breached for the bypass to succeed.

### Properties and obligations

Custom images must configure their HTTP client to present the certificate when calling `$KAALM_GATEWAY_ENDPOINT`, and to reload it when the projected files rotate; see [The runtime contract](../../runtime/contract.md). The starter templates ([Starter templates](../../runtime/starter-templates.md)) do both.

The mode is CNI-independent: identity is attested by the certificate, not by any network-layer header an intermediate hop could modify. An agent cannot claim a different identity without a CA-signed certificate for that identity, and the CA key is not reachable from any agent Pod.

Agent and AgentTask Pods must use mTLS: the gateway does not accept their ServiceAccount tokens, so a compromised Pod holds one credential, not two, and that credential is contained by its bounded `notAfter` and namespace-pinned SAN. The certificate is contained, not revocable. There is no CRL or OCSP, and Go's `crypto/tls` performs no revocation checking, so a known-compromised leaf is invalidated only by the [CA re-key runbook](../../security/tls.md#in-cluster-tls) or by waiting out `notAfter` (90 days by default). The full analysis is under [Agent to gateway authentication](../../security/rbac.md#agent-to-gateway-authentication).

## Mode 2: ServiceAccount bearer token

Existing workloads in user namespaces (Deployments, StatefulSets, Jobs) that the platform team wants to grant LLM-provider access to without adopting the Agent CRD use their projected ServiceAccount token:

```
Authorization: Bearer <projected-sa-token>
```

On receipt, the gateway runs these steps in order:

0. **Pod-ownership precheck.** The gateway resolves the request's source IP to a Pod in its informer cache. A Pod with an `ownerRef` to an `Agent` or `AgentTask`, or carrying the `kaalm.io/workload` label, is rejected with `401 Unauthorized` regardless of the token presented. This is what makes the mTLS tier exclusive: a compromised Agent or AgentTask Pod cannot fall back to its projected ServiceAccount token as a second credential. The precheck runs before the `TokenReview` call, so a hostile Pod cannot exploit `TokenReview` latency or unavailability, and it is never cached, because the Pod at a source IP can change and a token-cache hit must not skip it.
1. **`TokenReview`.** On a token-cache miss, the gateway POSTs the token to `authentication.k8s.io/v1/tokenreviews` with the expected audience, `kaalm-gateway`, so the apiserver rejects tokens minted for another audience. If the apiserver is unreachable, the request fails with `503 Service Unavailable`, `error.type: internal_unavailable`, `retryable: true`, and `Retry-After: 1`; mTLS requests and cached-token requests are unaffected. See [Failure modes](operations.md#failure-modes).
2. **Namespace.** On `status.authenticated: true`, the namespace is the middle segment of `status.user.username`, which has the form `system:serviceaccount:<namespace>:<sa>`. A rejected token fails the request with `401 unauthorized` (see the [LLM Gateway 401 row](../api/errors.md#llm-gateway-error-responses)).
3. **Cache.** The result is cached keyed by the token's SHA-256 hash. `TokenReviewStatus` carries no expiry field, so the TTL is derived from the token's own `exp` claim, minus a 60s safety margin, capped at 5 minutes; a token that does not parse as a JWT gets the fixed 5-minute TTL. The claim is read without signature verification, which is safe because the apiserver has already authenticated the token and the claim only bounds cache lifetime. Later requests with the same token skip the apiserver round trip.
4. **Cross-check.** The Pod at the source IP must be in the namespace `TokenReview` returned; see [Source-IP cross-check](#source-ip-cross-check-both-modes).

The gateway's ServiceAccount needs `create` on `authentication.k8s.io/v1/tokenreviews`, cluster-scoped; see [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions).

This mode yields a namespace only, not a workload identity, which is why provider routing for this tier consults `ModelProvider.spec.allowedNamespaces` and `spec.models` alone: the gateway has no Agent, AgentTask, or AgentClass to read. See [Provider routing and adapters](provider-routing.md).

### Caller-side setup

The workload sets the token audience with a `projected` volume carrying `audience: kaalm-gateway`. The explicit audience keeps generic `kubernetes.default.svc` tokens, such as a stolen kubelet token, from being accepted; a workload that skips the projection gets `401` on every call because its default token names the wrong audience. The workload must also mount the `kaalm-ca` ConfigMap so its calls to the gateway pass TLS verification. The controller injects nothing into gateway-only Pods, so both steps are manual. Full setup instructions are under [Tiered on-ramp](../../operations/deployment.md#tiered-on-ramp).

## Source-IP cross-check (both modes)

The cross-check runs after authentication, never instead of it. Once authentication has produced a namespace, the gateway looks up the source IP in its Pod informer cache and confirms the Pod is in that namespace; a mismatch is rejected with `401 unauthorized`. This catches a stolen client certificate or a stolen ServiceAccount token presented from a different Pod: the cryptographic attestation and the topological attestation must agree. See the [threat model](../../security/threat-model.md).

The Pod informer serves this lookup, provider-routing resolution, and activity tracking, and it must be fully synced before the gateway's readiness probe passes; see [Gateway readiness](operations.md#gateway-readiness).

**Pod IP reassignment.** When a Pod is deleted and a new Pod receives the same IP, common in small CIDR ranges, the informer cache may briefly map the old Pod. The gateway processes Pod delete events before accepting traffic from recycled IPs. In practice the deletion watch event arrives before the new Pod is scheduled, so the window is negligible.

**Informer-lag fallback on `/v1/task/complete` only.** Before declaring a cross-check failure on [`POST /v1/task/complete`](../api/task-complete.md), the gateway performs a live `List Pods` against the apiserver in the SAN's namespace, filtered by source IP. This keeps the new-Pod informer-lag race from surfacing as a terminal `401` instead of the retryable `409 stale_pod`. The fallback costs one `List` per cross-check miss, bounded by the AgentTask Pods in the namespace and rare in practice, and needs no new permissions: the gateway's existing cluster-wide Pod read covers it (see [Gateway overview](../overview.md)). Other endpoints rely on the informer cache only: heartbeats are periodic and recover on the next tick, and LLM-proxy callers retry their request normally.

## Per-path requirements

Not every path accepts both modes:

| Path | Accepted identity |
|---|---|
| LLM proxy paths (`/v1/messages`, `/v1/chat/completions`, `/v1/completions`) | mTLS (either SAN shape) or SA bearer token |
| Tool plane (`/v1/mcp/*`) | mTLS (either SAN shape) or SA bearer token; the same dual-mode profile, with per-plane authorization at the [broker](../tool-plane.md#the-broker) |
| `POST /v1/task/complete` | mTLS only; AgentTask at the handler, Agent callers rejected with 403 |
| `POST /v1/agent/heartbeat` | mTLS only; Agent at the handler, AgentTask callers rejected with 403 |
| `GET /v1/activity`, `GET /v1/channels/health` | mTLS with the controller SAN; Agent and AgentTask certificates rejected with 403 |

The agent-only endpoints have no SA-bearer alternative. The full mapping, and the middleware that implements it, is under [Per-path client-auth enforcement](../listener-tls.md#per-path-client-auth-enforcement). The security analysis of both modes, including threat-model coverage, is under [Agent to gateway authentication](../../security/rbac.md#agent-to-gateway-authentication).
