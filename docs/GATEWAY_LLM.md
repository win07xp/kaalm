# Agentry — LLM Gateway Design

This document covers the LLM Gateway: the shared cluster-level component responsible for mediating LLM traffic between agent containers and upstream providers. It is where spend tracking, budget guardrails, rate limiting, fallback, and credential isolation live.

For the User Gateway (channel message delivery, activator, activity tracking), see [GATEWAY_USER.md](./GATEWAY_USER.md). For the HTTP endpoint contracts agents use, see [API_ENDPOINTS.md](./API_ENDPOINTS.md).

## Why a Shared Gateway

Agent containers need to call LLM providers. Doing this naively (agents holding API keys and calling providers directly) gives up all centralized control: no spend visibility, no fallback, no per-namespace accounting, and every agent image must embed credentials. Agentry interposes on LLM traffic to deliver ModelProvider guarantees.

Similarly, agents need to be reachable from user-facing platforms (Discord, WhatsApp, webhooks). Rather than requiring each developer to build their own webhook receiver and protocol adapter, Agentry provides a shared channel ingress point.

### Architecture Option Analysis

Three architectural options were evaluated for the LLM proxy component:

**Option A: Per-Agent Sidecar Proxy**
A small proxy container runs as a sidecar in every Agent Pod.

Pros: Small failure domain; no shared state contention at request time.

Cons: Kubernetes `NetworkPolicy` cannot enforce per-container rules within a Pod — the sidecar and agent container share the same network namespace and the same IP, so NetworkPolicy cannot prevent the agent container from making direct egress calls to LLM providers if the node allows it. Credentials must be copied into user namespaces. Budget state requires eventual-consistent replication across all sidecars.

**Option B: Namespace-Scoped Gateway (not selected)**
One proxy Deployment per namespace.

Cons: More complex operator (gateway lifecycle per namespace), harder to reason about at scale, still requires per-namespace credential propagation.

**Option C: Cluster-Wide Gateway (SELECTED for v1)**
One replicated proxy Deployment in `agentry-system`.

Pros: Credentials never leave `agentry-system`. NetworkPolicy cleanly isolates agent Pods (deny all egress to LLM provider IPs; allow egress to the gateway Service — this is cross-Pod and fully enforceable). Budget state is centralized in one component: cross-replica reconciliation reduces to a single per-provider ConfigMap exchange with a bounded staleness window (see [Budget State Management](#budget-state-management)), rather than the per-sidecar eventual-consistency mesh Option A would require. The gateway also serves as the activator for hibernated agents. SPOF concern is addressed with 2-3 replicas, a PodDisruptionBudget (`minAvailable: 1`), and `maxUnavailable: 1` rolling updates.

**v1 ships with Option C.** The per-Pod sidecar pattern was rejected because the same-Pod network namespace sharing undermines the credential isolation guarantee on standard Kubernetes clusters without a service mesh.

---

## Gateway Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│                    Agentry Gateway (agentry-system)                  │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐     │
│  │                    LLM Gateway Listener                     │     │
│  │                                                             │     │
│  │  Request validator ──▶ Model allow-list check               │     │
│  │                  ──▶ Namespace access check                 │     │
│  │                  ──▶ Budget check + policy enforcement      │     │
│  │                  ──▶ Rate limiter                           │     │
│  │                  ──▶ Upstream router (with fallback)        │     │
│  │                  ──▶ Provider adapter                       │     │
│  │                  ◀── Response relay                         │     │
│  │                  ──▶ Token counter (post-call)              │     │
│  │                  ──▶ Spend state update                     │     │
│  └─────────────────────────────────────────────────────────────┘     │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐     │
│  │                   User Gateway Listener                     │     │
│  │                      (see GATEWAY_USER.md)                  │     │
│  └─────────────────────────────────────────────────────────────┘     │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐     │
│  │              Activator / Activity Store                     │     │
│  │                      (see GATEWAY_USER.md)                  │     │
│  └─────────────────────────────────────────────────────────────┘     │
└──────────────────────────────────────────────────────────────────────┘
         │ (egress)
         ▼
  LLM Provider APIs
  (Anthropic, OpenAI,
   Vertex, etc.)
```

---

## LLM Gateway — Request Flow

1. **Agent sends request**: the agent container makes an HTTPS request to `$AGENTRY_GATEWAY_ENDPOINT` (resolves to the gateway Service in `agentry-system`). The agent uses the upstream provider's native API path (e.g., `/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible) and includes a qualified model name in the request body (see [Model Identification](#model-identification) below). Request bodies above `gateway.maxLLMRequestBodyBytes` (Helm-configurable, default 4 MiB) are rejected with `413 request_too_large` at the listener, before namespace identification — same defense-in-depth pattern as the User Gateway's `gateway.maxMessageBodyBytes` cap on `:8080`. See [API_ENDPOINTS.md § LLM Gateway Error Responses](./API_ENDPOINTS.md#llm-gateway-error-responses) for the wire contract.
2. **Namespace identification**: the gateway authenticates the request — mTLS client-cert SAN (Mode 1) or `TokenReview`-verified bearer token (Mode 2) — to establish the caller's namespace, then cross-checks that the Pod at the request's source IP (from its Pod informer cache) is in that namespace (see [Namespace Identification](#namespace-identification) below).
3. **Provider routing**: the gateway resolves the Pod's ownerRef to the Agent resource, reads `spec.providers` to determine which ModelProviders this agent is allowed to use, and parses the `provider/model` name from the request to identify the target ModelProvider (see [Provider Routing](#provider-routing) below).
4. **Gateway validates**: confirms the requested model is listed in the target ModelProvider's `models` and the namespace is in `allowedNamespaces`.
5. **Budget check**: the gateway reads the current budget state for the agent's namespace. If a `degrade` policy applies, it rewrites the model name in the request. If `block` applies, it returns an error to the agent.
6. **Rate limit check**: per-(namespace, model) token-bucket rate limiter on requests/min and tokens/min.
7. **Route to upstream**: the gateway attaches the provider credential (read directly from Secrets in `agentry-system` — a static API-key header for Anthropic/OpenAI-style providers, an OAuth2 access token for Google Vertex; see [Credential Handling](#credential-handling)), strips the provider prefix from the model name, and forwards the request under a strict **forwarded-header contract**: all inbound authentication material is stripped before the provider credential is injected (`Authorization`, `x-api-key`, `api-key` — injection alone does not displace them, since Anthropic authenticates via `x-api-key` while a gateway-only-tier caller arrives with `Authorization: Bearer <SA-token>`; without the strip, a live audience-bound Kubernetes credential would be forwarded verbatim into third-party provider logs); hop-by-hop headers (`Connection`, `TE`, `Upgrade`, `Proxy-Authorization`) are removed per RFC 7230 §6.1; and `Accept-Encoding` is pinned to `identity` so upstream response bodies arrive uncompressed — Go's transport only auto-decompresses gzip it negotiated itself, so relaying a caller's `Accept-Encoding: gzip` would make every response body opaque to usage extraction and silently zero all spend accounting. If the upstream fails (connection error, 5xx, timeout), the gateway walks the fallback chain — trying the primary provider's `spec.fallback` entries, then each fallback's own fallback, up to `maxFallbackDepth` (default 3). See [Fallback Logic](#fallback-logic) below.
8. **Response returned**: the gateway relays the response to the agent container. For streaming responses (SSE), the gateway transparently relays each chunk as it arrives — see [Streaming Responses](#streaming-responses) below.
9. **Token counting**: the gateway extracts actual token usage from the provider response (`usage.input_tokens` / `usage.output_tokens` for Anthropic; `usage.prompt_tokens` / `usage.completion_tokens` for OpenAI; `usageMetadata.promptTokenCount` / `usageMetadata.candidatesTokenCount` for Google Vertex). For streaming responses, token usage is extracted from the usage-bearing SSE events (for Anthropic, `message_start` carries `input_tokens` and the final `message_delta` carries the cumulative `output_tokens`; for OpenAI-compatible providers, the usage object arrives in the final chunk before `[DONE]` — see [Streaming Responses](#streaming-responses) for the `stream_options.include_usage` injection this depends on; for Vertex, `usageMetadata` arrives on the final streamed chunk). Actual usage from the provider response is always preferred over pre-call estimation.
10. **Spend update**: the gateway updates the in-process spend counter for the namespace.

---

## Streaming Responses

Most LLM usage involves streaming responses (Server-Sent Events / SSE), where the provider sends token-by-token output as a stream of chunks. The gateway supports streaming transparently:

**Relay model**: the gateway acts as a pass-through proxy for SSE streams. When the upstream provider begins sending a streaming response (`Content-Type: text/event-stream`), the gateway relays each SSE chunk to the agent as it arrives. The gateway does not buffer the full response — chunks are forwarded immediately to preserve the low-latency benefit of streaming.

**Token counting**: the gateway inspects each SSE chunk as it relays it, accumulating usage metadata where the provider's stream format carries it. For Anthropic, `input_tokens` arrive on the `message_start` event and the cumulative `output_tokens` on the final `message_delta` event (`message_stop` carries no usage). For OpenAI-compatible providers, a usage object appears in the final chunk preceding the `[DONE]` sentinel — but only when the request sets `stream_options: {"include_usage": true}`, so the gateway **injects that field into OpenAI-format streaming requests when absent** (the addition is backward-compatible: the extra terminal usage chunk has an empty `choices` array, which OpenAI client libraries tolerate, and it is relayed to the agent unchanged). For Google Vertex, the adapter **appends `?alt=sse` to `:streamGenerateContent` requests when absent** — Vertex otherwise returns a JSON-array stream rather than SSE, which would never engage the SSE relay — and usage arrives as `usageMetadata` (`promptTokenCount` / `candidatesTokenCount`) on the final streamed chunk. The gateway extracts this data and updates spend counters after the stream completes — the same as step 9 in the non-streaming flow. A stream that ends without usage metadata (misbehaving upstream) is counted as zero spend and logged at warning level — acceptable under the soft-guardrail budget model, and visible to operators via the log signal.

**Budget checks**: budget checks occur pre-call (step 5) using the last-known spend state, the same as non-streaming requests. No mid-stream budget enforcement is performed — once a stream has started, it runs to completion. This is the correct behavior: aborting a stream mid-response would leave the agent with a partial, unusable response while still incurring provider charges for the full generation.

**Mid-stream failures**: if the upstream provider connection drops or errors mid-stream (after the first chunk has been relayed to the agent), the gateway closes the agent's SSE stream with an error event and does **not** attempt fallback. A partially-consumed stream cannot be retried — the agent has already received partial output, and replaying the request on a fallback provider would produce a different, potentially contradictory continuation. Fallback only applies to **pre-stream failures**: connection errors, timeouts before the first chunk, and error responses returned before streaming begins.

**Provider adapter**: the `ProviderAdapter.ForwardRequest` method handles both streaming and non-streaming modes. The adapter detects streaming from the upstream response headers (`Content-Type: text/event-stream` or `Transfer-Encoding: chunked` with SSE content) and returns a streaming reader that the gateway relays to the agent. Token extraction is adapter-specific — each adapter knows where usage metadata appears in its provider's SSE format.

---

## Namespace Identification

The gateway supports **two authentication modes** that cover the two Helm tiers:

1. **mTLS client certificate** — the primary, zero-config path for Agentry-managed Agent/AgentTask Pods.
2. **`TokenReview`-verified ServiceAccount bearer token** — the path for gateway-only-tier workloads (existing Deployments that were not created by the Agentry controller and therefore do not have a cert-manager-issued client cert).

A request that presents **neither** a client certificate nor a `Authorization: Bearer <token>` header is rejected with `401 Unauthorized`. A request presenting both is processed by the mTLS path; the bearer token is ignored.

A source-IP cross-check runs against both modes — the Pod at the request's source IP must be in the namespace that authentication identified. This is defense in depth; it is not the identity mechanism.

### Mode 1 — mTLS client certificate (Agentry-managed Pods)

The LLM Gateway listener requires a client certificate on connections from Pods created by the AgentReconciler or AgentTaskReconciler. Agents and tasks present the cert at `$AGENTRY_TLS_CERT` (`/var/run/agentry/tls.crt`) with key at `$AGENTRY_TLS_KEY`. The gateway verifies it against the Agentry CA (trust bundle from the trust-manager-projected `agentry-ca` ConfigMap — see [TLS on the LLM Gateway Listener](#tls-on-the-llm-gateway-listener) for where the CA material lives) and extracts identity from the certificate's SAN. Two SAN shapes are recognized:

- `{name}.{namespace}.svc.cluster.local` — issued by the AgentReconciler (matches the Agent's Service DNS). Exactly **5 labels** when split on `.`.
- `{name}.{namespace}.task.agentry.io` — issued by the AgentTaskReconciler. AgentTasks have no Service, so a non-Service shape is used to make the workload type explicit. Exactly **4 labels** when split on `.`.

Namespace extraction is identical for both shapes (second label). The shape discriminates workload type for audit and metrics. This produces a cryptographically attested (namespace, workload name, workload kind) triple on every request.

**Exact label-count enforcement**: the identity extractor scans the certificate's DNS SAN **list** for entries ending in a recognized shape suffix (`.svc.cluster.local` / `.task.agentry.io`). The per-Agent certificate also carries short-form Service SANs (`{name}.{namespace}.svc` and `{name}.{namespace}` — see [Agent Serving & Client TLS](#agent-serving--client-tls)); these match no recognized suffix and are ignored by the extractor. Exactly one SAN must match a recognized shape, and it must have exactly the expected label count — 5 for `.svc.cluster.local`, 4 for `.task.agentry.io`. A recognized-suffix SAN with extra (or fewer) labels, or a certificate with zero or multiple recognized SANs, is rejected as `403 invalid_cert`. This is defense in depth against a dotted-name bypass: if the CRD CEL constraint restricting Agent/AgentTask `metadata.name` to DNS-1123 labels (see the [Agent CRD design notes](./API_RESOURCES.md#agent)) were ever relaxed or bypassed, a name like `admin.svc` in namespace `team-a` would yield the SAN `admin.svc.team-a.svc.cluster.local` (6 labels) and be rejected by the gateway before the namespace extractor ran. Both layers must be breached for the bypass to succeed.

Starter templates (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)) demonstrate client-cert presentation and the cert-file watch-and-reload pattern. Custom images must configure their HTTP client to present the cert when calling `$AGENTRY_GATEWAY_ENDPOINT`. See [Agent Runtime Contract](./RUNTIME_CONTRACT.md).

This mode is **CNI-independent**: identity is cryptographically attested by the certificate, not by any network-layer header that an intermediate hop could modify. An agent cannot claim a different identity without a CA-signed certificate for that identity — and the CA key is not reachable from any agent Pod.

**Agent/AgentTask Pods MUST use mTLS.** Their ServiceAccount tokens are deliberately not accepted by the gateway. If they were, a compromised agent would hold two independent credentials instead of one, and the SA token would outlive the containment properties of the cert (bounded `notAfter`, namespace-pinned SAN). Note the cert itself is contained, not revocable: re-issuance does nothing to an already-leaked leaf — there is no CRL or OCSP, and Go's `crypto/tls` performs no revocation checking — so a known-compromised leaf is invalidated only by the [CA re-key runbook](./SECURITY.md#in-cluster-tls-bidirectional) or by waiting out `notAfter`. See [Agent→Gateway Authentication](./SECURITY.md#agentgateway-authentication) for the full analysis.

Provider routing for Agentry-managed Pods runs the full chain — Agent/AgentClass `allowedProviders`, then ModelProvider `allowedNamespaces`/`models`. See [Provider Routing § mTLS tier](#provider-routing).

### Mode 2 — ServiceAccount bearer token (gateway-only tier)

Existing workloads running in user namespaces (Deployments, StatefulSets, Jobs that the platform team wants to grant LLM-provider access to without adopting the full Agent CRD) use their projected ServiceAccount token. The caller sets:

```
Authorization: Bearer <projected-sa-token>
```

On receipt, the gateway runs a Pod-ownership precheck **before** any token validation, then performs a `TokenReview`:

0. **Pod-ownership precheck.** The gateway resolves the request's source IP to a Pod via its Pod informer cache. If the Pod has an `ownerRef` pointing to an `Agent` or `AgentTask` resource (or carries the Agentry-managed label set), the request is rejected with `401 Unauthorized` regardless of the bearer token presented. Agentry-managed Pods are required to use mTLS — SA-token auth is reserved for the gateway-only tier. This is what keeps the mTLS tier's credential surface to a single artifact — a bounded-lifetime, namespace-pinned client cert: a compromised Agent/AgentTask Pod cannot fall back to its projected ServiceAccount token as a second credential (containment, not revocation — a leaked leaf stays valid until `notAfter` unless the CA is re-keyed; see [SECURITY.md § Agent→Gateway Authentication](./SECURITY.md#agentgateway-authentication)). The precheck runs before the `TokenReview` apiserver call so a hostile Pod cannot exploit `TokenReview` latency or unavailability.
1. POST the token to `authentication.k8s.io/v1/tokenreviews`. Include the expected audience (`agentry-gateway`) so the apiserver rejects tokens minted for a different audience.
2. On `status.authenticated: true`, parse `status.user.username`. It has the form `system:serviceaccount:<namespace>:<sa>`; the middle segment is the authoritative namespace.
3. Cache the validation result keyed by the token's SHA-256 hash. `TokenReviewStatus` carries no expiry field (only `authenticated`, `user`, `audiences`, `error`), so the cache TTL is derived from the token's own `exp` claim — projected SA tokens are JWTs, and the claim is parsed locally *without* signature verification, which is safe because the apiserver has already authenticated the token and the claim only bounds cache lifetime, never grants access — minus a 60s safety margin, capped at 5 minutes. A token that does not parse as a JWT gets the fixed 5-minute TTL. Subsequent requests from the same token hit the cache and skip the apiserver roundtrip. The Pod-ownership precheck is **not** cached — it re-runs on every request, since Pod identity at a given source IP can change.
4. Perform the source-IP cross-check: the Pod at the request's source IP (from the Pod informer) must be in the namespace returned by `TokenReview`. This closes the gap where a stolen token could be used from a different Pod.

The gateway's ServiceAccount needs `create` on `authentication.k8s.io/v1/tokenreviews` (cluster-scoped) — see [SECURITY.md § Gateway ServiceAccount](./SECURITY.md#gateway-serviceaccount-permissions).

Token audiences are set by workloads via a `projected` volume with `audience: agentry-gateway`. Using an explicit audience prevents generic `kubernetes.default.svc` tokens from being accepted — a stolen kubelet token cannot be reused against the gateway.

Provider routing for this tier is governed by `ModelProvider.spec.allowedNamespaces` and `spec.models` only — the gateway has no Agent, AgentTask, or AgentClass to consult. See [Provider Routing § Gateway-only tier](#provider-routing).

### Source-IP cross-check (both modes)

After the authentication step produces a claimed (namespace, …) pair, the gateway looks up the source IP in its Pod informer cache and confirms the Pod's namespace matches. Mismatch → request rejected. This catches:

- Stolen client certificate presented from a different Pod (mode 1).
- Stolen SA token presented from a different Pod (mode 2).

The gateway maintains a Pod informer cache for this lookup and for provider-routing resolution and activity tracking. The Pod informer must be fully synced before the gateway's readiness probe passes — see [Gateway Readiness](#gateway-readiness).

**Pod IP reassignment**: when a Pod is deleted and a new Pod receives the same IP (common in small CIDR ranges), the informer cache may briefly map the old Pod. The gateway MUST process Pod delete events before accepting traffic from recycled IPs. In practice, the watch event for Pod deletion arrives before the new Pod is scheduled, so the window is negligible.

See [Agent→Gateway Authentication](./SECURITY.md#agentgateway-authentication) for the full security analysis of both modes, including threat-model coverage.

---

## TLS on the LLM Gateway Listener

The LLM Gateway listener serves TLS to protect LLM request and response payloads in transit within the cluster. Without TLS, prompts and completions traverse the cluster network in plaintext, which is unacceptable when agent containers run untrusted code on shared nodes. See [In-cluster TLS](./SECURITY.md#in-cluster-tls-bidirectional) for the full security analysis.

**cert-manager is a required dependency.** Agentry uses cert-manager to manage the Agentry CA and all leaf certificates (gateway serving cert, controller activator cert, per-agent serving/client certs). The Helm chart ships the cert-manager resources (two `ClusterIssuer`s and the gateway/controller `Certificate` objects) but not the cert-manager controller itself — clusters must have cert-manager installed. Teams with an existing cert-manager deployment reuse it. This replaces an earlier operator-managed CA approach; see [V1 design note in SECURITY.md](./SECURITY.md#in-cluster-tls-bidirectional).

**Trust chain**:

1. Chart installs a cluster-scoped self-signed `ClusterIssuer` named `agentry-selfsigned`.
2. Chart installs a `Certificate` named `agentry-ca` in cert-manager's **cluster resource namespace** (default `cert-manager`; Helm value `certManager.clusterResourceNamespace` — see [DEPLOYMENT.md](./DEPLOYMENT.md)) whose `issuerRef` points at `agentry-selfsigned` and which has `isCA: true`. This is the Agentry root. Long-lived (default 5y). It lives there — not in `agentry-system` — because of the constraint in step 3.
3. Chart installs a cluster-scoped `ClusterIssuer` named `agentry-ca-issuer` whose `ca.secretName` is `agentry-ca`'s output Secret. cert-manager resolves a `ClusterIssuer`'s `ca.secretName` **only in its cluster resource namespace** (the `--cluster-resource-namespace` flag, default `cert-manager`) — the secret ref has no namespace field — so the CA `Certificate` and its Secret must live there, or the issuer sits `Ready=False, reason=SecretNotFound` and every leaf issuance fails. All Agentry leaf certs — including the per-Agent and per-AgentTask certs created in user namespaces — are issued from this `ClusterIssuer`. A `ClusterIssuer` is used instead of a namespace-scoped `Issuer` because cert-manager's `issuerRef` on a `Certificate` does not resolve across namespaces to a namespaced `Issuer`; a `ClusterIssuer` is the idiomatic way to let `Certificate` resources in user namespaces reference a signing key held outside their own namespace.
4. Chart installs a `Certificate` for the gateway serving cert (`agentry-gateway-tls`) issued from `agentry-ca-issuer`. SAN: `agentry-gateway.agentry-system.svc.cluster.local`, `agentry-gateway.agentry-system.svc`, `localhost`. Usages: `server auth`, `client auth` (the gateway also presents this cert when dialing the controller's activator / activity / channels-health endpoints). The Helm value `gateway.externalHostnames` (see [DEPLOYMENT.md § Helm Chart Contents](./DEPLOYMENT.md#helm-chart-contents)) extends this SAN list with operator-supplied public hostnames; required when the User listener is exposed via TLS pass-through Ingress.
5. Controller deployment ships with a `Certificate` for the activator / activity-API / channels-health serving cert (see [CONTROLLER_RECONCILERS.md](./CONTROLLER_RECONCILERS.md)). Usages: `server auth`, `client auth` (the controller also presents this cert when dialing the gateway's activity endpoint).
6. The `AgentReconciler` creates a `Certificate` per Agent (owner-referenced from the Agent) — see [Agent Serving & Client TLS](#agent-serving--client-tls) below.

**Certificate rotation**: cert-manager rotates each leaf continuously. Chart defaults:

- Gateway cert: `spec.duration: 2160h` (90d), `spec.renewBefore: 720h` (30d).
- Per-agent cert: `spec.duration: 2160h` (90d), `spec.renewBefore: 720h` (30d).
- Agentry CA: `spec.duration: 43800h` (5y), `spec.renewBefore: 8760h` (1y).

When a `Certificate`'s Secret is updated by cert-manager, kubelet updates the projected volume in any Pod that mounts it, and the consumer (gateway, controller, agent) reloads from disk. The gateway watches `agentry-gateway-tls` for changes; starter templates (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)) demonstrate the inotify-based reload pattern that custom images must implement.

**Agent trust bundle**: every agent Pod mounts the Agentry CA at `/var/run/agentry/ca.crt` (the `$AGENTRY_CA_CERT` env var points at this path). This is a projected volume sourced from a ConfigMap projected into the agent's namespace by `trust-manager`. The Helm chart installs a `trust-manager` `Bundle` resource whose source is the `agentry-ca` Secret in the cluster resource namespace (trust-manager reads `Bundle` sources only from its configured trust namespace — `--trust-namespace`, default `cert-manager` — which must be, and by default is, the same namespace that holds the CA Secret) and whose target writes a ConfigMap named `agentry-ca` into every non-system namespace selected by the bundle's `target.namespaceSelector` (default excludes `kube-system`, `kube-public`, `kube-node-lease`; overridable via the Helm value `trustManager.bundleSelector`) — see [DEPLOYMENT.md § Certificate Lifecycle](./DEPLOYMENT.md#certificate-lifecycle). Agent HTTP clients must trust this CA when calling `$AGENTRY_GATEWAY_ENDPOINT`. Starter templates handle this. `trust-manager` is a required dependency alongside cert-manager.

**CA rotation**: the `agentry-ca` `Certificate` pins `spec.privateKey.rotationPolicy: Never` — cert-manager's default, stated explicitly because it is load-bearing. When cert-manager renews the CA certificate within `spec.renewBefore` of expiry it re-uses the existing key pair, so every previously issued leaf still chains to the renewed CA cert: the renewal is transparent, trust-manager re-projects the new CA bytes, and no dual-trust window is needed. cert-manager does **not** proactively re-issue leaves on CA renewal, and trust-manager does **not** maintain an automatic old+new CA overlap — neither is needed under key-reuse renewal. A true CA **re-key** (compromise recovery) is a manual runbook, not an automatic behavior: add the new CA as a second source on the trust-manager `Bundle` alongside the old one, force leaf re-issuance (`cmctl renew` on the leaf `Certificate`s), then drop the old source once no live leaf chains to it. No operator *code* is involved in either path — this was the main motivation for adopting cert-manager.

**Mutual TLS (mTLS)**: the LLM Gateway listener requires client certificates from agents in the Agentry-managed path. Agents present their per-agent TLS certificate (the same cert used for gateway→agent delivery) as the client cert when calling `$AGENTRY_GATEWAY_ENDPOINT`. The gateway verifies the client cert against `agentry-ca` and extracts the SAN to identify the agent and namespace. Starter templates configure client cert presentation automatically. Custom images must configure their HTTP client to use `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY` as the client certificate. This is the primary identity mechanism for Agentry-managed Pods — see [Namespace Identification](#namespace-identification). Gateway-only-tier workloads do not present a client cert; they authenticate via `TokenReview` (see Mode 2 above), and client certs are optional on the TLS handshake for that path.

**Activity API path also requires mTLS.** `GET /v1/activity` is served on the same gateway TLS listener but requires a client cert whose SAN matches the controller Service DNS (`agentry-controller.agentry-system.svc.cluster.local` or `.svc`). The controller presents its `agentry-controller-tls` cert. Agent/AgentTask certs are rejected on this path because their SANs do not match — defense in depth against a compromised agent using a valid CA-signed cert to query activity data across namespaces. See [Internal Endpoint Authentication](./SECURITY.md#internal-endpoint-authentication-activator-activity-channel-health).

**`/v1/channels/health` is served on this listener too.** Like `/v1/activity`, it requires an mTLS client cert whose SAN matches the controller Service DNS, and the same SAN-authorization rule applies (requests from gateway, agent, or AgentTask certs are rejected). It lives on port 8443 — **not** the externally-exposed User listener on 8080 — so that Ingress fronting 8080 cannot route an untrusted caller to this endpoint. See [GATEWAY_USER.md § TLS and Ingress](./GATEWAY_USER.md#tls-and-ingress) for the listener-split rationale and [API_ENDPOINTS.md § GET /v1/channels/health](./API_ENDPOINTS.md#get-v1channelshealth-internal--controller-use-only).

**Agent health probes and TLS**: because the agent serves HTTPS on `$AGENTRY_HEALTH_PORT` (using the same per-agent certificate), the readiness and liveness probes injected by the AgentReconciler must set `httpGet.scheme: HTTPS`. Kubernetes `httpGet` probes do not verify TLS certificates, so no additional CA configuration is required on the probe. See [Agent Runtime Contract](./RUNTIME_CONTRACT.md).

`$AGENTRY_GATEWAY_ENDPOINT` is an `https://` URL — TLS is not optional.

### Per-path client-auth enforcement

The LLM listener on `:8443` serves three authentication regimes on a single TLS socket:

1. **mTLS-required** for Agentry-managed Agent/AgentTask requests (Mode 1 — see [Namespace Identification](#namespace-identification)).
2. **mTLS-optional** for gateway-only-tier `TokenReview` callers (Mode 2) who do not present a client cert.
3. **mTLS-required-with-SAN-authorization** for the controller's `/v1/activity` and `/v1/channels/health` calls.

A single `tls.Config.ClientAuth` value cannot express path-conditional requirements. The gateway therefore configures `ClientAuth: tls.VerifyClientCertIfGiven` at the handshake layer (so callers without a client cert can still complete the TLS handshake) and enforces per-path requirements in HTTP middleware:

- `/v1/messages`, `/v1/chat/completions`, `/v1/completions`, plus adapter-registered provider-specific paths (see [Request Format Detection](#request-format-detection)): if `r.TLS.PeerCertificates` is non-empty, follow the mTLS path (Mode 1 — extract namespace from SAN, enforce the SAN-shape and label-count rules). If empty, follow the bearer-token path (Mode 2 — first run the Pod-ownership precheck described in [Mode 2 § step 0](#mode-2--serviceaccount-bearer-token-gateway-only-tier) to reject Agent/AgentTask Pods, then `TokenReview`-validate the `Authorization: Bearer <token>` header). If both auth materials are absent, return `401 Unauthorized`. If both are present, the mTLS path wins and the bearer header is ignored — see [Namespace Identification](#namespace-identification).
- `/v1/agent/heartbeat`, `/v1/task/complete`: **mTLS-only** — there is no bearer-token fallback on these paths (per [ARCHITECTURE.md § The Agentry Gateway](./ARCHITECTURE.md#the-agentry-gateway) `:8443` auth profile and [API_ENDPOINTS.md](./API_ENDPOINTS.md)). Empty `r.TLS.PeerCertificates` returns `401 Unauthorized` regardless of any bearer header; gateway-only-tier workloads have no Agent/AgentTask identity and nothing meaningful to report on these endpoints. The Agent-vs-AgentTask split is enforced at the handler (heartbeat: Agent only; task-complete: AgentTask only; the other kind gets `403`).
- `/v1/activity`, `/v1/channels/health`: require a client cert whose SAN matches the controller Service DNS. Empty `r.TLS.PeerCertificates` returns `401 Unauthorized`; a present-but-non-matching SAN returns `403 Forbidden`. There is no fallback to bearer-token auth on these paths — they are controller-only.

Path-conditional middleware is the only correct way to express this on Go's `crypto/tls`: setting `RequireAndVerifyClientCert` on the listener would lock out gateway-only-tier callers (the TLS handshake would fail before the request reached the path router), and setting `NoClientCert` would silently downgrade the mTLS tier (cert presented but never verified).

### Agent Serving & Client TLS

The User Gateway's delivery to agent Services (`POST /v1/message`) is over HTTPS. The AgentReconciler creates a cert-manager `Certificate` per Agent named `{agentName}-tls` in the Agent's namespace, owner-referenced to the Agent (so it is garbage-collected on Agent deletion). Its `issuerRef` is `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`. A `ClusterIssuer` is used because `Certificate` resources in user namespaces cannot reference a namespaced `Issuer` in another namespace across the namespace boundary. The `spec.secretName` output Secret is mounted into the agent Pod at `/var/run/agentry/tls.crt` / `tls.key`. The certificate SAN list includes:

- `{agentName}.{namespace}.svc.cluster.local` (Service DNS)
- `{agentName}.{namespace}.svc`
- `{agentName}.{namespace}`

The same cert is used as a client cert when the agent calls `$AGENTRY_GATEWAY_ENDPOINT` (see [Namespace Identification](#namespace-identification)). Rotation is fully owned by cert-manager; the reconciler does not batch re-issues or maintain rotation-state ConfigMaps.

---

## Model Identification

Agents identify both the provider and the model in each LLM request using a **qualified model name** format: `{providerRef}/{modelId}`.

Examples:
- `anthropic-shared/claude-opus-4-6` — Claude Opus via the `anthropic-shared` ModelProvider
- `anthropic-shared/claude-sonnet-4-6` — Claude Sonnet via the same provider
- `openai-fallback/gpt-4o` — GPT-4o via the `openai-fallback` ModelProvider
- `local-vllm/llama-3-70b` — Llama 3 70B via a local vLLM instance registered as a ModelProvider

The gateway splits the model name on the first `/`: the prefix identifies the ModelProvider by `metadata.name`, and the suffix is the raw model ID that must appear in the ModelProvider's `models` list. Before forwarding upstream, the gateway strips the provider prefix and sends only the raw model ID (e.g., the upstream Anthropic API receives `claude-opus-4-6`, not `anthropic-shared/claude-opus-4-6`).

This format uniquely identifies the (provider, model) pair and eliminates ambiguity when multiple ModelProviders offer models with similar names (e.g., a managed Anthropic endpoint and an OpenAI-compatible proxy both serving Claude models). The agent is always responsible for constructing the qualified `provider/model` name in its API calls. Where the qualified name appears follows the upstream format: in the request body's `model` field for Anthropic / OpenAI / OpenAI-compatible formats, and in the URL path's `{model}` segment (URL-encoded) for Google Vertex — see [Request Format Detection](#request-format-detection).

---

## Provider Routing

Provider routing differs between the two authentication tiers because the gateway-only tier has no Agent resource to consult. Both variants run after [Namespace Identification](#namespace-identification) has produced an authenticated namespace.

### mTLS tier (Agentry-managed Pods)

Agents and AgentTasks created by the controller have an Agent (or AgentTask) resource with `spec.providers` and an AgentClass with `allowedProviders`. The gateway walks the full chain:

1. **Source IP -> Pod**: resolved from the Pod informer cache (see [Namespace Identification](#namespace-identification)).
2. **Pod -> Agent**: the Pod's ownerRef identifies the Agent (or AgentTask) resource. The gateway maintains an Agent informer cache for this lookup.
3. **Agent -> allowed providers**: the Agent's `spec.providers` lists the ModelProviders this agent may use. The referenced providers must also appear in the AgentClass's `allowedProviders` — the gateway resolves the class from the workload's `agentClassRef` via its AgentClass informer (see [Gateway Readiness](#gateway-readiness) and [SECURITY.md § Gateway ServiceAccount permissions](./SECURITY.md#gateway-serviceaccount-permissions)).
4. **Model name -> ModelProvider**: the gateway parses the `provider/model` qualified name from the request body (or, for Vertex-format requests, from the URL path's `{model}` segment — see [Model Identification](#model-identification)). The provider prefix must match a `providerRef` in the Agent's `spec.providers`. If it does not, the request is rejected.
5. **ModelProvider -> upstream**: the gateway reads the ModelProvider's `spec.endpoint`, `spec.type`, and credentials to forward the request. The namespace must also be in the ModelProvider's `allowedNamespaces`.

This chain ensures that an agent can only reach ModelProviders explicitly listed in its spec, which in turn must be in the AgentClass's `allowedProviders` and must include the agent's namespace in `allowedNamespaces`. All three access checks (Agent -> ModelProvider -> Namespace) must pass.

### Gateway-only tier (TokenReview)

Existing workloads that authenticate with a projected ServiceAccount bearer token have **no Agent resource**, so steps 2–4 above do not apply. Routing is governed by the ModelProvider's own allowlist plus its model list:

1. **Token -> namespace**: `TokenReview` yields the caller's authenticated namespace (see [Mode 2](#mode-2--serviceaccount-bearer-token-gateway-only-tier)).
2. **Model name -> ModelProvider**: the gateway parses the `provider/model` qualified name from the request body (or the URL path for Vertex-format requests — see [Model Identification](#model-identification)). The provider prefix must resolve to an existing `ModelProvider` by `metadata.name`; if not, the request is rejected with `400 invalid_request`.
3. **Namespace allowlist**: the caller's namespace must match a `ModelProvider.spec.allowedNamespaces` entry (exact name or glob). If not, the request is rejected with `403 access_denied`.
4. **Model allowlist**: the requested model must appear in `ModelProvider.spec.models`. If not, the request is rejected with `400 invalid_request`.
5. **Forward**: the gateway reads `spec.endpoint`, `spec.type`, and credentials and forwards the request.

**AgentClass `allowedProviders` is deliberately not enforced in this tier.** AgentClass is the platform-team policy layer for the full-lifecycle tier; gateway-only workloads are not Agents and are not associated with any AgentClass. Platform teams who need class-scoped provider policy must onboard workloads through the full Agent lifecycle tier. The gateway-only tier trades that policy surface for a zero-CRD on-ramp — see [VISION.md § What Agentry Provides](./VISION.md#what-agentry-provides) and [DEPLOYMENT.md § Tiered On-Ramp](./DEPLOYMENT.md#tiered-on-ramp).

---

## Request Format Detection

The agent sends LLM requests using the upstream provider's native API format. The gateway detects the request format from the **URL path** the agent uses:

- `/v1/messages` -> Anthropic format
- `/v1/chat/completions` -> OpenAI / OpenAI-compatible format (also used by vLLM, Ollama, LiteLLM)
- `/v1/completions` -> OpenAI legacy completions format
- `…/models/{model}:generateContent` and `…/models/{model}:streamGenerateContent` -> Google Vertex (Gemini) format. The Vertex adapter matches on the `:generateContent` / `:streamGenerateContent` method suffix rather than a fixed prefix (Vertex paths embed project and location segments). Vertex is also the one format that names the model in the **URL path** rather than the request body: the `{model}` segment carries the qualified `{providerRef}/{modelId}` name (URL-encoded), and the gateway rewrites it to the raw model ID before forwarding — the same strip-the-prefix step applied to body-carried model names. On `:streamGenerateContent` the adapter also guarantees `?alt=sse` is present — see [Streaming Responses](#streaming-responses).

Each provider adapter registers the path patterns it recognizes; requests to unrecognized paths on the LLM listener are rejected with `400 invalid_request`. The gateway uses the detected format to parse the request (extracting the model name and other fields), then forwards to the upstream provider.

The gateway is **protocol-aware** in that it understands request/response shapes for supported provider types (for token extraction, model name parsing, etc.), but it does **not** translate between formats. Cross-format fallback (e.g., Anthropic-format request falling back to an OpenAI-compatible endpoint) is not supported in v1. Fallback is restricted to providers of the same `spec.type` — see [Fallback Logic](#fallback-logic) below. This keeps the gateway's request path simple and avoids the large, error-prone surface area of bidirectional API translation (streaming, tool use, multimodal content, etc.).

---

## Budget State Management

Budget counters are maintained **in-process in the gateway**. Because the gateway is the single choke point for all LLM traffic, there is no need for a separate aggregator or distributed counter.

Each gateway replica maintains an in-memory spend counter per (provider, namespace, period) tuple. On startup, each replica reads the current period's spent value from the canonical ConfigMap managed by the [ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler). On each LLM call, the counter is updated synchronously.

**Budget counter exchange interface**: each gateway replica periodically (every 10s) writes its partial spend counters to a ConfigMap in `agentry-system` named `agentry-budget-{providerName}`, keyed by the replica's Pod name. Replicas use **server-side apply** with per-replica field managers (field manager name = Pod name), so each replica owns only its own key. This eliminates optimistic concurrency conflicts between replicas writing simultaneously. The ConfigMap data structure is:

```yaml
data:
  # Each key is a gateway Pod name; value is JSON with the budget period and per-namespace spend.
  # The "period" field is required so the reconciler can exclude stale entries from prior periods
  # during rollover — replicas transition to the new period independently on their first request,
  # so mixed-period entries are expected in the rollover window.
  agentry-gateway-0: '{"period": "2026-04", "team-support": "142.50", "team-ml": "87.30"}'
  agentry-gateway-1: '{"period": "2026-04", "team-support": "138.20", "team-ml": "91.10"}'
  _canonical: '{"team-support": "280.70", "team-ml": "178.40"}'
```

The ModelProviderReconciler reads this ConfigMap on each reconcile pass (event-driven plus the controller's 5-minute periodic requeue per [CONTROLLER_RECONCILERS.md § Reconcile Interval and Performance](./CONTROLLER_RECONCILERS.md#reconcile-interval-and-performance)), **filters out any per-replica entries whose `period` does not match the current period**, sums the remaining partials, writes the `_canonical` key with the total, and updates `status.budgetUsage` on the ModelProvider. Gateway replicas read the `_canonical` key on startup to initialize their local counters. This avoids a Prometheus dependency and works with existing ConfigMap RBAC.

**Cross-replica enforcement view**: after startup, each replica **watches the budget ConfigMap** (via the `agentry-system` ConfigMap informer it already holds) and folds the other replicas' current-period partials into its enforcement view on every change. The spend value a replica enforces budget policies against is therefore its own live in-memory counter plus every peer's most recently written partial — at most one 10-second write interval stale. This is load-bearing: if replicas only read `_canonical` at startup, a long-lived replica would never observe peer spend and enforcement drift would grow unbounded within a budget period. The reconciler's `_canonical` write remains the durable roll-up for status reporting and replica (re)starts; per-request enforcement never waits on it.

**Period tag rationale**: at period rollover (midnight UTC), gateway replicas detect the new period on their first incoming request and reset their local counter to zero. Because replicas transition independently, there is a window where some replicas have written new-period partials and others still hold old-period totals. Without the `period` field, the reconciler would sum mixed values and produce an incorrect canonical total. By tagging each entry, the reconciler skips old-period entries until all replicas have transitioned, giving a correct (if slightly underestimated) total during the rollover window — which is acceptable for a soft guardrail.

**Budget state on crash**: if all gateway replicas crash simultaneously, up to 10s of spend data (the partial-write interval) may be lost. This is acceptable given that budgets are soft limits — the bounded loss is small relative to typical budget thresholds. Gateway replicas re-initialize from the `_canonical` ConfigMap value on restart.

This means budget enforcement is **approximate under high concurrency** — replicas can collectively overspend within a partial-write window, since each replica sees peer spend at most ~10s stale (the partial-write interval). The overspend is bounded by: `number_of_replicas x max_calls_per_second_per_replica x cost_per_call x partial_write_interval_seconds`. For typical deployments (2-3 replicas, 1 call/sec peak per replica, $0.01-0.10 per call, 10s partial-write interval), the maximum overspend per window is roughly $0.20-$3.00, which is acceptable for a soft guardrail. Streaming widens this window: streamed usage lands on the counters only after the stream completes (see [Streaming Responses](#streaming-responses)), so an in-flight stream's cost is invisible to every replica — including the one serving it — for the stream's full duration, often 30–120s rather than 10s. The bound therefore carries an additional `+ number_of_replicas × concurrent_streams_per_replica × cost_per_call` term, and streaming-heavy namespaces should size their soft-guardrail slack from that larger figure.

**Agentry's budget feature is spend visibility and soft guardrails, not a hard financial cap.** This is an explicit design decision, not a limitation to be fixed. Teams requiring hard caps should use provider-level account limits in addition to Agentry's per-namespace guardrails.

**Budget period rollover**: budget periods roll over at midnight UTC. Each gateway replica detects the period change on its first request of the new period and resets its local counter, writing a new-period entry (with the updated `period` field) to its ConfigMap key. The reconciler, filtering by the current period, excludes old-period entries during the rollover window — the canonical total may be temporarily underestimated until all replicas have written new-period entries, which is acceptable for soft guardrails. Once the new period is fully established, the controller archives the previous period's totals to ModelProvider status for auditability, deletes all per-replica keys from the budget ConfigMap, and writes a fresh `_canonical: {}`.

**Stale replica cleanup**: when a gateway replica is scaled down or replaced, its entry in the budget ConfigMap persists. The ModelProviderReconciler cross-references ConfigMap keys against the current set of gateway Pod names and deletes stale entries before summing partials. This prevents inflated spend totals from terminated replicas.

---

## Credential Handling

Provider credentials are stored as Secrets in `agentry-system` and referenced by ModelProvider. The gateway reads these Secrets directly at startup and watches them for rotation. Credentials never leave `agentry-system` — there is no per-agent or per-namespace credential copying.

The credential's shape is adapter-specific. For Anthropic, OpenAI, and OpenAI-compatible providers, the referenced Secret holds a static API key that the adapter injects as the provider's auth header. Google Vertex does not accept static API keys: its Secret holds a GCP service-account JSON key, and the Vertex adapter mints OAuth2 access tokens from it (cached in memory and refreshed roughly 5 minutes before the ~1-hour expiry), attaching the current token as the `Authorization: Bearer` header on LLM requests and health probes alike.

When a credential Secret is updated, the gateway's Secret watcher picks up the change and refreshes the in-memory credential without a restart (for Vertex, the next token mint uses the new service-account key).

---

## Provider Adapters

The gateway supports multiple upstream provider types via a pluggable adapter interface:

```go
type ProviderAdapter interface {
    Type() string                               // "anthropic", "openai", etc.
    ExtractUsage(resp Response) (Usage, error)
    ForwardRequest(ctx, req, credentials) (Response, error)
    HealthCheck(ctx, endpoint, credentials) error
}
```

v1 ships adapters for: Anthropic, OpenAI, Google Vertex, OpenAI-compatible (Ollama/vLLM/LiteLLM gateways).

Pre-call token estimation is not used for budget gating (it adds latency and is inaccurate). Budget checks use the last-known spend state. Post-call actual usage is authoritative for accounting.

---

## Fallback Logic

When the primary provider returns a **fallbackable** response, the gateway walks `ModelProvider.spec.fallback` in order. A **budget-blocked primary does not trigger fallback** — the gateway returns `429 budget_exhausted` to the agent immediately (see Request Flow step 5). This keeps budget enforcement predictable: a namespace at its cap does not silently drain budget from a fallback provider.

### Fallback triggers

Not every upstream error is a fallback signal — a malformed prompt sent to provider A will fail identically on provider B, so forwarding it just wastes latency and budget. The gateway classifies upstream outcomes as follows:

| Upstream response | Action | Rationale |
|---|---|---|
| Connection error / DNS failure / TLS handshake failure | Fall back | Upstream is unreachable; a different provider may be reachable. |
| Timeout before any response bytes | Fall back | Treated like a connection error — the request never landed. The per-attempt bound is `gateway.providerFirstByteTimeout` (default `120s`; see [DEPLOYMENT.md](./DEPLOYMENT.md#helm-chart-contents)), applied from connection start through first response byte. |
| `5xx` | Fall back | Upstream-side failure; retrying the same upstream would hit the same failure. |
| `429` (upstream-side rate limit) | Fall back | Distinct from the gateway's own `429 rate_limited`, which is returned to the caller without fallback. Upstream 429 indicates the primary's capacity is exhausted, and a different provider will often succeed. |
| `401` / `403` from upstream | Fall back **and** emit a `Warning` event with `reason=CredentialsInvalid` on the primary ModelProvider. If a subsequent health probe still sees 401/403, the reconciler sets `Ready=False, reason=CredentialsInvalid`. | Upstream refuses the credential. Falling back preserves availability while signalling to the platform team that rotation or re-issuance is needed. |
| `400` / `422` (malformed or unprocessable request) | Return to caller unchanged; **do not fall back** | The request itself is malformed; fallback will fail for the same reason. Consumes one attempt slot. |
| Other `4xx` | Return to caller unchanged; do not fall back | Client-side error surface — the caller should fix the request. |

The distinction matters most at the `4xx` boundary: `429` (transient capacity) and `401/403` (credential problem, often fixable by switching provider) fall back; `400/422` (caller-driven) do not. This avoids turning a bad-prompt bug into a cross-provider retry storm that drains budget across every fallback.

For each candidate provider (primary or a fallback entry) the gateway performs these checks before forwarding. The checks split into two kinds with different effects on the `attemptCount` budget:

**Static eligibility** — derived from configuration alone, unchanged between requests:

1. Verify the candidate provider has the **same `spec.type`** as the primary provider (e.g., both `anthropic`, or both `openai-compatible`). If the types differ, the candidate is skipped. This constraint exists because the gateway does not translate between API formats — see [Request Format Detection](#request-format-detection) above.
2. Verify the namespace is in the candidate provider's `allowedNamespaces`.
3. Verify the requested model exists in the candidate provider's `models`.

A static-eligibility failure is a misconfiguration (usually discoverable at reconcile time). The gateway **skips the candidate without consuming an `attemptCount` slot** and emits a `Warning` event with `reason=FallbackIneligible` on the primary `ModelProvider`, naming the offender and the specific failure (e.g., `"fallback 'openai-backup' skipped: namespace 'team-ml' not in allowedNamespaces"`). Silently burning attempt slots on misconfigured fallbacks hides the problem and makes the misconfiguration indistinguishable from upstream outages in metrics; surfacing it as a status event makes it fixable.

**Runtime gating** — derived from request-time state:

4. Check the candidate provider's budget state for the agent's namespace. If the candidate is budget-blocked, skip it and **do** consume an `attemptCount` slot. This applies only while walking the chain after a non-budget primary failure — a budget-blocked *primary* never reaches this step (see above). Budget state is legitimately runtime, and slot-bounded latency still matters.
5. Forward the request with the candidate provider's credentials.

### Traversal algorithm

`ModelProvider.spec.fallback` is a list, and each entry may carry its own `spec.fallback` list, so the chain is a tree rather than a flat sequence. The gateway walks it **depth-first in declared order**:

```
# Top-level entry called by the request handler:
#   tryWithFallbacks(primary=primary, provider=primary, request, attemptCount=0, visited={})
# `primary` is threaded unchanged through every recursive call so the
# FallbackIneligible event is always emitted on the primary ModelProvider
# (the resource the platform team owns and watches).
# `attemptCount` is returned from every call so increments inside one subtree
# are visible to the next sibling iteration — the depth cap counts attempts
# across the entire tree, not per path. Without this thread-back, sibling
# fallbacks would each restart from the caller's local count and the cap
# could be violated along the breadth dimension.

tryWithFallbacks(primary, provider, request, attemptCount, visited) -> (result, attemptCount):
    if provider.name in visited:               # runtime dedup, defense in depth
        return error("cycle_detected"), attemptCount
    visited.add(provider.name)

    if not staticallyEligible(provider, request):   # type, allowedNamespaces, models (checks 1–3)
        # Static misconfiguration. Do NOT consume an attempt slot.
        # Emit Warning event reason=FallbackIneligible on the PRIMARY
        # (not this provider) so the platform team sees the misconfig on
        # the ModelProvider they own.
        emitFallbackIneligible(primary, provider, reason)
        # Do not walk children of a type-mismatched provider either —
        # validation guarantees same-type chains, so children should be
        # reachable via an eligible ancestor.
        return error("statically_ineligible"), attemptCount

    if attemptCount >= maxFallbackDepth:       # cap on total providers tried
        return error("fallback_depth_exhausted"), attemptCount
    attemptCount += 1

    if budgetBlocked(provider, request.namespace):  # runtime gate (check 4)
        # Consumed a slot; fall through to children.
        pass
    else:
        response = forward(provider, request)
        if response.ok:
            return response, attemptCount
        if not isFallbackable(response):        # see Fallback triggers table above
            return response, attemptCount       # pass 400/422/other 4xx back to caller

    for next in provider.spec.fallback:        # declared order, depth-first
        result, attemptCount = tryWithFallbacks(primary, next, request, attemptCount, visited)
        if result.ok:
            return result, attemptCount

    return error("all_fallbacks_exhausted"), attemptCount
```

`isFallbackable(response)` encapsulates the table above: it returns true for connection/DNS/TLS errors, pre-stream timeouts, any `5xx`, upstream `429`, and upstream `401`/`403` (with the credential-warning side effect); false for `400`, `422`, and other `4xx`. Non-fallbackable responses are passed through to the caller verbatim and do not consume additional chain attempts, because continuing the walk would both waste latency and be wrong — no other provider will succeed with the same bad request.

`FallbackIneligible` is surfaced as a Kubernetes `Warning` event on the primary `ModelProvider`, not returned to the caller as a 5xx — the caller's request continues walking the tree. The event exists so platform teams see the misconfiguration on the `ModelProvider` resource (`kubectl describe modelprovider …`) rather than discovering it only via an elevated fallback failure rate in metrics. The `ModelProviderReconciler` also emits this event at reconcile time when it detects static eligibility violations in the declared chain — see [ModelProviderReconciler step 5](./CONTROLLER_RECONCILERS.md#modelproviderreconciler).

### Depth cap semantics

`maxFallbackDepth` (default `3`, set via Helm `gateway.maxFallbackDepth` → `AGENTRY_MAX_FALLBACK_DEPTH`) bounds the **total number of providers attempted per request, including the primary** — not the nesting depth of the tree. With the default, the gateway tries at most the primary plus two others before giving up, regardless of how the fallback tree is shaped. This is the latency guarantee: each attempt is bounded from connect through first response byte by `gateway.providerFirstByteTimeout` (default `120s`), so no single request waits more than `maxFallbackDepth × providerFirstByteTimeout` before a terminal error. Once a stream has started, the same value applies as an idle-bytes timeout between SSE chunks — an upstream that stalls without closing is terminated with the documented mid-stream error event rather than holding gateway and agent connections open indefinitely.

If the chain is exhausted or the cap is reached without a successful response, the gateway returns a **fallback-exhausted error whose `error.type` reflects the failure classes observed across the walk**: `503 provider_unavailable` when every attempted provider failed at the connect layer (connection error, DNS failure, TLS handshake failure), `504 provider_timeout` when every attempt timed out pre-stream, and `502 provider_error` otherwise — any upstream error response (5xx, upstream 429, 401/403) or a mix of failure classes, including a walk exhausted purely by budget-blocked candidates. All three share the fallback-exhausted `retryable: false` rationale and carry the originally-requested provider in `error.provider` — see [API_ENDPOINTS.md § LLM Gateway Error Responses](./API_ENDPOINTS.md#llm-gateway-error-responses). Circular references are rejected at reconcile time by the [ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler), so cycles should never reach the gateway; the runtime `visited` check is defense in depth.

**Same-type constraint**: this is validated at reconcile time by the ModelProviderReconciler. Each provider in the fallback chain must have the same `spec.type` as the primary provider (e.g., all `anthropic` or all `openai-compatible`). A ModelProvider with `type: anthropic` cannot list a fallback with `type: openai`. Cross-format fallback may be considered for a future version if there is sufficient demand, but the translation surface area (streaming, tool use, system prompts, multimodal content) is large and error-prone.

---

## Rate Limiting

Rate limits are enforced at the gateway using token-bucket limiters keyed on (namespace, model). Limits come from `ModelProvider.spec.rateLimits` and represent **cluster-wide ceilings**. When a limit is hit, the gateway returns HTTP 429 with a `Retry-After` header.

Each gateway replica divides the configured limit by the number of active gateway replicas (discovered from its Pod informer — count Pods matching the gateway label selector). When replicas scale up or down, each replica adjusts its local token bucket capacity on the next refill cycle. This means the configured value directly represents the intended cluster-wide rate limit regardless of replica count.

**Note:** because each replica enforces its share independently, the effective cluster-wide limit is approximate — transient bursts may slightly exceed the configured ceiling. The approximation is bounded by `configured_limit / number_of_replicas` per replica (one replica's full bucket) and is acceptable for v1.

**Worst-case deviation during scaling events**: during scale-up, existing replicas immediately divide by N+1 when the new Pod appears in their informer — before the new replica begins serving traffic — momentarily reducing each existing replica's effective limit. During rolling restarts (`maxUnavailable: 1`), different replicas can transiently hold different bucket sizes, causing the effective cluster-wide ceiling to deviate by up to one replica's share. If tighter enforcement is required, replace per-replica division with a shared ConfigMap-backed token bucket (the `agentry-budget-{providerName}` ConfigMap already provides the coordination primitive).

---

## Gateway Error Responses

When the gateway cannot fulfill an LLM request, it returns a structured error response so agents can handle failures programmatically (e.g., scenario S10 — graceful degradation on budget exhaustion). See [API_ENDPOINTS.md](./API_ENDPOINTS.md#llm-gateway-error-responses) for the full error schema and status code mapping.

---

## Upstream TLS Configuration

The gateway always connects to upstream LLM providers over HTTPS. For enterprise environments that require custom CA bundles or HTTP proxies for outbound traffic, the gateway supports:

- **Custom CA bundle**: a ConfigMap in `agentry-system` (`agentry-upstream-ca`) containing additional CA certificates. The gateway loads these on startup and watches for changes. All upstream HTTPS connections trust both the system CA bundle and the custom bundle.
- **HTTP proxy**: standard `HTTPS_PROXY` / `NO_PROXY` environment variables on the gateway Deployment. The gateway respects these for all upstream provider calls.

These are gateway-level settings (not per-ModelProvider) because they typically reflect cluster-wide network infrastructure.

---

## Gateway Readiness

The gateway Pod's readiness probe (`GET /readyz` on the internal health port — `:8081` by default, Helm value `gateway.healthPort`; TLS with no client auth, serving only `/healthz` and `/readyz`) returns `200` only when **all** of the following are true:

1. The **LLM listener** on `:8443` is bound and accepting TLS connections. The probe performs a local dial to confirm.
2. The **User listener** on `:8080` is bound and accepting TLS connections (both listeners use the `agentry-gateway-tls` certificate). The probe performs a local TLS dial to confirm.
3. **All informer caches** the request path depends on have completed their initial sync (`cache.WaitForCacheSync` returned true for each): `Pod` (source-IP → namespace resolution), `Agent` and `AgentTask` (provider-routing ownerRef resolution, hibernation-state checks), `AgentClass` (the `allowedProviders` gate in the mTLS-tier routing chain), `AgentChannel` (webhook path → target Agent lookup), `ModelProvider` (model validation, `allowedNamespaces`, fallback chain traversal). Until every cache is synced, namespace identification, provider routing, and channel routing would either fail or return spurious `404` / `403` / `invalid_request` responses while caches hydrate.
4. The **gateway serving certificate** (`agentry-gateway-tls`) has been loaded from disk. On startup, the gateway reads the mounted Secret; if the Secret does not yet exist (cert-manager has not issued it), readiness fails. This matters on initial chart install where the Pod may start before cert-manager completes issuance.

Any single failure above returns `503 Service Unavailable` with a body listing which checks failed. Kubernetes retries the probe per the Pod's `readinessProbe.periodSeconds` (default 10s) until the gateway is fully ready, which keeps the gateway Pod out of the Service's endpoints during the startup window. The same checks feed the "Gateway not ready" row in [Failure Modes](#failure-modes).

Because both listeners and every dependent informer must be green for the probe to pass, the Service never receives traffic for a listener that would error at connection time or for a Pod that cannot yet resolve source IPs to namespaces, map ownerRefs to Agents/AgentTasks, look up AgentChannels, or validate requested models.

---

## Observability

The gateway exposes Prometheus metrics on `:9090/metrics`:

- `agentry_llm_requests_total{provider,model,namespace,status}`
- `agentry_llm_request_duration_seconds{provider,model}`
- `agentry_llm_tokens_total{provider,model,namespace,direction}` (direction = input|output)
- `agentry_llm_spend_usd_total{provider,namespace}`
- `agentry_llm_fallback_total{from_provider,to_provider,reason}`
- `agentry_llm_budget_utilization{provider,namespace,period}` (gauge, 0-1)

For User Gateway metrics, see [GATEWAY_USER.md](./GATEWAY_USER.md#observability).

---

## Failure Modes

| Failure | Behavior |
|---|---|
| Gateway replica crashes | Other replicas continue; Kubernetes restarts the crashed replica |
| All gateway replicas down | LLM calls from agents fail; up to 10s of spend data may be lost (see [Budget State Management](#budget-state-management)) |
| Gateway replica not ready (listener dial fails, any of the dependent informers not synced — Pod, Agent, AgentTask, AgentClass, AgentChannel, ModelProvider — or cert not yet issued) | Readiness probe returns 503; replica excluded from Service endpoints until all checks pass. See [Gateway Readiness](#gateway-readiness) |
| Provider API down | Fallback chain walked (same-type providers only, up to `maxFallbackDepth` depth); if all providers in the chain fail, the request fails with a fallback-exhausted error — `502 provider_error`, or `503`/`504` when every attempt was unreachable / timed out (see [Depth cap semantics](#depth-cap-semantics)) |
| Budget exhausted | Request blocked (`429 budget_exhausted` with `Retry-After` header) or degraded per policy; Warning event emitted on ModelProvider |
| `TokenReview` apiserver unreachable (mode 2 only) | Gateway returns `503 Service Unavailable` (`error.type: internal_unavailable`, `retryable: true`, `Retry-After: 1` — see [API_ENDPOINTS.md § LLM Gateway Error Responses](./API_ENDPOINTS.md#llm-gateway-error-responses)) to the caller for requests that miss the token cache; mTLS requests and cached-token requests are unaffected |
| CNI does not support FQDN egress policy but AgentClass sets `allowedHosts` | AgentClassReconciler emits a `Warning` event and ignores `allowedHosts`; `allowedCIDRs` alone governs egress. See [AgentClassReconciler](./CONTROLLER_RECONCILERS.md#agentclassreconciler) |
