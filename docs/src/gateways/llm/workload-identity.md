# Workload Identity

Every decision the LLM Gateway makes downstream of authentication hangs on one question: which namespace, and which workload, sent this request? [Budget accounting](budgets-and-rate-limits.md#budget-state-management) charges token spend to a namespace, and [provider routing](provider-routing.md) enforces namespace-scoped allowlists. If a caller could lie about its namespace, it could spend another team's budget or reach a provider it was never granted. The gateway therefore derives a trustworthy identity for every request before doing anything else.

## Two Modes, Two Tiers

The gateway supports two authentication modes that cover the two Helm tiers:

1. **mTLS client certificate**: the primary, zero-config path for Agentry-managed Agent/AgentTask Pods.
2. **`TokenReview`-verified ServiceAccount bearer token**: the path for gateway-only-tier workloads. These are existing Deployments that were not created by the Agentry controller and therefore have no cert-manager-issued client cert.

A request that presents neither a client certificate nor a `Authorization: Bearer <token>` header is rejected with `401 Unauthorized`. A request presenting both is processed by the mTLS path; the bearer token is ignored. At the handshake layer the listener runs `ClientAuth: tls.VerifyClientCertIfGiven`, and per-path HTTP middleware enforces which mode each path accepts. See [Per-path client-auth enforcement](listener-tls.md#per-path-client-auth-enforcement).

A [source-IP cross-check](#source-ip-cross-check-both-modes) runs against both modes: the Pod at the request's source IP must be in the namespace that authentication identified. This is defense in depth; it is not the identity mechanism.

Both modes ride on the gateway's TLS listener, so every caller must also verify the gateway's serving certificate against the Agentry CA. Agentry-managed Pods find the bundle at `$AGENTRY_CA_CERT` (`/var/run/agentry/ca.crt`); gateway-only workloads mount the `agentry-ca` ConfigMap manually (see [Caller-side setup](#caller-side-setup)). See the [Agent Runtime Contract](../../runtime/contract.md).

## Mode 1: mTLS Client Certificate

The LLM Gateway listener requires a client certificate on connections from Pods created by the AgentReconciler or AgentTaskReconciler. Agents and tasks present the cert at `$AGENTRY_TLS_CERT` (`/var/run/agentry/tls.crt`) with key at `$AGENTRY_TLS_KEY` (`/var/run/agentry/tls.key`). The gateway verifies it against the Agentry CA, using the trust bundle from the trust-manager-projected `agentry-ca` ConfigMap (see [TLS on the LLM Gateway Listener](listener-tls.md) for where the CA material lives), and extracts identity from the certificate's SAN. A certificate that fails CA verification is rejected at the TLS handshake itself; handshake failures produce no HTTP response and never appear in the [LLM Gateway error table](../api/errors.md#llm-gateway-error-responses). Two SAN shapes are recognized:

- `{name}.{namespace}.svc.cluster.local`: issued by the AgentReconciler and matches the Agent's Service DNS. Exactly **5 labels** when split on `.`.
- `{name}.{namespace}.task.agentry.io`: issued by the AgentTaskReconciler. AgentTasks have no Service, so a non-Service shape is used to make the workload type explicit rather than implying a Service the task does not have. Exactly **4 labels** when split on `.`.

Namespace extraction is identical for both shapes: the second label. The shape discriminates workload type for audit and metrics. This produces a cryptographically attested (namespace, workload name, workload kind) triple on every request. The workload half of that triple underpins the Agent/AgentClass `spec.providers` check, per-task ConfigMap targeting, and the Agent-vs-AgentTask handler split described under [per-path requirements](#per-path-requirements).

### Exact label-count enforcement

The identity extractor scans the certificate's DNS SAN **list** for entries ending in a recognized shape suffix (`.svc.cluster.local` / `.task.agentry.io`). The per-Agent certificate also carries short-form Service SANs (`{name}.{namespace}.svc` and `{name}.{namespace}`); these match no recognized suffix and are ignored by the extractor. Exactly one SAN must match a recognized shape, and it must have exactly the expected label count: 5 for `.svc.cluster.local`, 4 for `.task.agentry.io`. A recognized-suffix SAN with extra (or fewer) labels, or a certificate with zero or multiple recognized SANs, is rejected as `403 invalid_cert`.

This is defense in depth against a dotted-name bypass. Rule 21 (a CRD CEL constraint, enforced at apply time) restricts Agent/AgentTask `metadata.name` to DNS-1123 labels; see the [Agent CRD design notes](../../resources/agent.md). If that constraint were ever relaxed or bypassed, a name like `admin.svc` in namespace `team-a` would yield the SAN `admin.svc.team-a.svc.cluster.local` (6 labels) and be rejected by the gateway before the namespace extractor ran. Both layers must be breached for the bypass to succeed.

### Properties and obligations

Starter templates (see [Starter Templates](../../runtime/starter-templates.md)) demonstrate client-cert presentation and the cert-file watch-and-reload pattern. Custom images must configure their HTTP client to present the cert when calling `$AGENTRY_GATEWAY_ENDPOINT`. See the [Agent Runtime Contract](../../runtime/contract.md).

This mode is **CNI-independent**: identity is cryptographically attested by the certificate, not by any network-layer header that an intermediate hop could modify. An agent cannot claim a different identity without a CA-signed certificate for that identity, and the CA key is not reachable from any agent Pod.

**Agent/AgentTask Pods MUST use mTLS.** Their ServiceAccount tokens are deliberately not accepted by the gateway. If they were, a compromised agent would hold two independent credentials instead of one, and the SA token would outlive the containment properties of the cert (bounded `notAfter`, namespace-pinned SAN). Rejecting the token keeps that tier's credential surface to a single artifact. Note the cert itself is contained, not revocable: re-issuance does nothing to an already-leaked leaf. There is no CRL or OCSP, and Go's `crypto/tls` performs no revocation checking, so a known-compromised leaf is invalidated only by the [CA re-key runbook](../../security/tls.md#in-cluster-tls) or by waiting out `notAfter` (90d by default). Clusters that need a tighter compromise bound should shorten the per-Agent `Certificate` `duration`. See [Agent to Gateway Authentication](../../security/rbac.md#agent-to-gateway-authentication) for the full analysis.

Provider routing for Agentry-managed Pods runs the full chain: Agent/AgentClass `allowedProviders`, then ModelProvider `allowedNamespaces`/`models`. See [Provider Routing](provider-routing.md).

## Mode 2: ServiceAccount Bearer Token

Existing workloads running in user namespaces (Deployments, StatefulSets, Jobs that the platform team wants to grant LLM-provider access to without adopting the full Agent CRD) use their projected ServiceAccount token. The caller sets:

```
Authorization: Bearer <projected-sa-token>
```

On receipt, the gateway runs a Pod-ownership precheck **before** any token validation, then performs a `TokenReview`:

0. **Pod-ownership precheck.** The gateway resolves the request's source IP to a Pod via its Pod informer cache. If the Pod has an `ownerRef` pointing to an `Agent` or `AgentTask` resource (or carries the Agentry-managed label set), the request is rejected with `401 Unauthorized` regardless of the bearer token presented. Agentry-managed Pods are required to use mTLS; SA-token auth is reserved for the gateway-only tier, and this precheck is what enforces that exclusivity at the gateway. A compromised Agent/AgentTask Pod cannot fall back to its projected ServiceAccount token as a second credential. The precheck runs before the `TokenReview` apiserver call, so a hostile Pod cannot exploit `TokenReview` latency or unavailability, and it is unaffected by token-cache hits.
1. POST the token to `authentication.k8s.io/v1/tokenreviews`. Include the expected audience (`agentry-gateway`) so the apiserver rejects tokens minted for a different audience. If the `TokenReview` apiserver is unreachable, the gateway returns `503 Service Unavailable` with `error.type: internal_unavailable`, `retryable: true`, and `Retry-After: 1` for bearer-token requests that miss the token cache; mTLS requests and cached-token requests are unaffected. See [Failure Modes](operations.md#failure-modes).
2. On `status.authenticated: true`, parse `status.user.username`. It has the form `system:serviceaccount:<namespace>:<sa>`; the middle segment is the authoritative namespace. A rejected token (`status.authenticated: false`) fails the request with `401 unauthorized` (see the [LLM Gateway 401 row](../api/errors.md#llm-gateway-error-responses)).
3. Cache the validation result keyed by the token's SHA-256 hash. `TokenReviewStatus` carries no expiry field (only `authenticated`, `user`, `audiences`, `error`; `expirationTimestamp` belongs to TokenRequest, the minting API). The cache TTL is therefore derived from the token's own `exp` claim, minus a 60s safety margin, capped at 5 minutes. Projected SA tokens are JWTs, and the claim is parsed locally *without* signature verification. That is safe because the apiserver has already authenticated the token, and the claim only bounds cache lifetime, never grants access. A token that does not parse as a JWT gets the fixed 5-minute TTL. Subsequent requests from the same token hit the cache and skip the apiserver roundtrip. The Pod-ownership precheck is **not** cached; it re-runs on every request, since Pod identity at a given source IP can change.
4. Perform the source-IP cross-check: the Pod at the request's source IP (from the Pod informer) must be in the namespace returned by `TokenReview`. This closes the gap where a stolen token could be used from a different Pod.

The gateway's ServiceAccount needs `create` on `authentication.k8s.io/v1/tokenreviews` (cluster-scoped); see [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions).

This mode yields a namespace only, not a workload identity. That is the structural reason provider routing for this tier is governed by `ModelProvider.spec.allowedNamespaces` and `spec.models` alone: the gateway has no Agent, AgentTask, or AgentClass to consult. See [Provider Routing](provider-routing.md).

### Caller-side setup

Token audiences are set by workloads via a `projected` volume with `audience: agentry-gateway`. Using an explicit audience prevents generic `kubernetes.default.svc` tokens from being accepted; a stolen kubelet token cannot be reused against the gateway. A workload that skips the audience-bound projection gets `401` on every call, because its default token names the wrong audience. The workload must also mount the `agentry-ca` ConfigMap so its calls to the gateway pass TLS verification. The controller injects nothing into gateway-only Pods, so both steps are manual. Full setup instructions live in [Tiered On-Ramp](../../operations/deployment.md#tiered-on-ramp).

## Source-IP Cross-Check (Both Modes)

The cross-check runs after authentication, never instead of it. Once the authentication step produces a claimed (namespace, ...) pair, the gateway looks up the source IP in its Pod informer cache and confirms the Pod's namespace matches. Mismatch: the request is rejected with `401 unauthorized`. This catches:

- Stolen client certificate presented from a different Pod (mode 1).
- Stolen SA token presented from a different Pod (mode 2).

In both cases the cryptographic attestation and the topological attestation must agree; see the [threat model](../../security/threat-model.md).

The gateway maintains a Pod informer cache for this lookup and for provider-routing resolution and activity tracking. The Pod informer must be fully synced before the gateway's readiness probe passes; see [Gateway Readiness](operations.md#gateway-readiness).

**Pod IP reassignment**: when a Pod is deleted and a new Pod receives the same IP (common in small CIDR ranges), the informer cache may briefly map the old Pod. The gateway MUST process Pod delete events before accepting traffic from recycled IPs. In practice, the watch event for Pod deletion arrives before the new Pod is scheduled, so the window is negligible.

**Informer-lag fallback on `/v1/task/complete` only**: before declaring cross-check failure on [`POST /v1/task/complete`](../api/task-complete.md), the gateway performs a live API-server `List Pods` in the cert-SAN-derived namespace, filtered by source IP. This keeps the new-Pod informer-lag race from surfacing as a terminal `401` instead of the retryable `403 StalePodCompletion`. The fallback costs one `List` per cross-check miss, bounded by the AgentTask Pods in the namespace and rare in practice, and needs no new permissions: the gateway's existing cluster-wide Pod read covers it (see [The Agentry Gateway](../overview.md)). Other endpoints rely on the informer cache only: heartbeats are periodic and recover on the next tick, and LLM-proxy callers retry their request normally.

## Per-Path Requirements

Not every path accepts both modes:

| Path | Accepted identity |
|---|---|
| LLM proxy paths (`/v1/messages`, `/v1/chat/completions`, `/v1/completions`, provider-specific paths) | mTLS (either SAN shape) or SA bearer token |
| `POST /v1/task/complete` | mTLS only; AgentTask at the handler, Agent callers rejected with 403 |
| `POST /v1/agent/heartbeat` | mTLS only; Agent at the handler, AgentTask callers rejected with 403 |
| `GET /v1/activity`, `GET /v1/channels/health` | mTLS with the controller SAN; Agent/AgentTask certs rejected with 403 |

The agent-only endpoints have no SA-bearer alternative. The full mapping, and the middleware that implements it, is specified in [Per-path client-auth enforcement](listener-tls.md#per-path-client-auth-enforcement).

See [Agent to Gateway Authentication](../../security/rbac.md#agent-to-gateway-authentication) for the full security analysis of both modes, including threat-model coverage.
