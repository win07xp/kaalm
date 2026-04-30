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

Pros: Credentials never leave `agentry-system`. NetworkPolicy cleanly isolates agent Pods (deny all egress to LLM provider IPs; allow egress to the gateway Service — this is cross-Pod and fully enforceable). Budget state is centralized in one process with no replication lag. The gateway also serves as the activator for hibernated agents. SPOF concern is addressed with 2-3 replicas, a PodDisruptionBudget (`minAvailable: 1`), and `maxUnavailable: 1` rolling updates.

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

1. **Agent sends request**: the agent container makes an HTTPS request to `$AGENTRY_GATEWAY_ENDPOINT` (resolves to the gateway Service in `agentry-system`). The agent uses the upstream provider's native API path (e.g., `/v1/messages` for Anthropic, `/v1/chat/completions` for OpenAI-compatible) and includes a qualified model name in the request body (see [Model Identification](#model-identification) below).
2. **Namespace identification**: the gateway resolves the source IP to a Pod via its Pod informer cache, then reads the Pod's namespace (see [Namespace Identification](#namespace-identification) below).
3. **Provider routing**: the gateway resolves the Pod's ownerRef to the Agent resource, reads `spec.providers` to determine which ModelProviders this agent is allowed to use, and parses the `provider/model` name from the request to identify the target ModelProvider (see [Provider Routing](#provider-routing) below).
4. **Gateway validates**: confirms the requested model is listed in the target ModelProvider's `models` and the namespace is in `allowedNamespaces`.
5. **Budget check**: the gateway reads the current budget state for the agent's namespace. If a `degrade` policy applies, it rewrites the model name in the request. If `block` applies, it returns an error to the agent.
6. **Rate limit check**: per-namespace token-bucket rate limiter on requests/min and tokens/min.
7. **Route to upstream**: the gateway adds the provider API key (read directly from Secrets in `agentry-system`), strips the provider prefix from the model name, and forwards the request. If the upstream fails (connection error, 5xx, timeout), the gateway walks the fallback chain — trying the primary provider's `spec.fallback` entries, then each fallback's own fallback, up to `maxFallbackDepth` (default 3). See [Fallback Logic](#fallback-logic) below.
8. **Response returned**: the gateway relays the response to the agent container. For streaming responses (SSE), the gateway transparently relays each chunk as it arrives — see [Streaming Responses](#streaming-responses) below.
9. **Token counting**: the gateway extracts actual token usage from the provider response (`usage.input_tokens` / `usage.output_tokens` for Anthropic; `usage.prompt_tokens` / `usage.completion_tokens` for OpenAI, etc.). For streaming responses, token usage is extracted from the final SSE chunk (e.g., the `message_stop` event for Anthropic, the usage object in the final chunk before `[DONE]` for OpenAI). Actual usage from the provider response is always preferred over pre-call estimation.
10. **Spend update**: the gateway updates the in-process spend counter for the namespace.

---

## Streaming Responses

Most LLM usage involves streaming responses (Server-Sent Events / SSE), where the provider sends token-by-token output as a stream of chunks. The gateway supports streaming transparently:

**Relay model**: the gateway acts as a pass-through proxy for SSE streams. When the upstream provider begins sending a streaming response (`Content-Type: text/event-stream`), the gateway relays each SSE chunk to the agent as it arrives. The gateway does not buffer the full response — chunks are forwarded immediately to preserve the low-latency benefit of streaming.

**Token counting**: the gateway inspects each SSE chunk as it relays it, looking for the final chunk that contains usage metadata. For Anthropic, this is the `message_stop` event (which includes `usage.input_tokens` and `usage.output_tokens`). For OpenAI-compatible providers, usage data appears in the final chunk preceding the `[DONE]` sentinel. The gateway extracts this data and updates spend counters after the stream completes — the same as step 9 in the non-streaming flow.

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

The LLM Gateway listener requires a client certificate on connections from Pods created by the AgentReconciler or AgentTaskReconciler. Agents and tasks present the cert at `$AGENTRY_TLS_CERT` (`/var/run/agentry/tls.crt`) with key at `$AGENTRY_TLS_KEY`. The gateway verifies it against the Agentry CA (trust bundle from the `agentry-ca` Secret in `agentry-system`) and extracts identity from the certificate's SAN. Two SAN shapes are recognized:

- `{name}.{namespace}.svc.cluster.local` — issued by the AgentReconciler (matches the Agent's Service DNS). Exactly **5 labels** when split on `.`.
- `{name}.{namespace}.task.agentry.io` — issued by the AgentTaskReconciler. AgentTasks have no Service, so a non-Service shape is used to make the workload type explicit. Exactly **4 labels** when split on `.`.

Namespace extraction is identical for both shapes (second label). The shape discriminates workload type for audit and metrics. This produces a cryptographically attested (namespace, workload name, workload kind) triple on every request.

**Exact label-count enforcement**: the gateway requires the DNS SAN to have exactly the expected label count for its shape — 5 for `.svc.cluster.local`, 4 for `.task.agentry.io`. Any SAN with extra (or fewer) labels is rejected as `403 invalid_cert`. This is defense in depth against a dotted-name bypass: if the CRD CEL constraint restricting Agent/AgentTask `metadata.name` to DNS-1123 labels (see [API_RESOURCES.md § Agent design notes](./API_RESOURCES.md#design-notes)) were ever relaxed or bypassed, a name like `admin.svc` in namespace `team-a` would yield the SAN `admin.svc.team-a.svc.cluster.local` (6 labels) and be rejected by the gateway before the namespace extractor ran. Both layers must be breached for the bypass to succeed.

Starter templates (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)) demonstrate client-cert presentation and the cert-file watch-and-reload pattern. Custom images must configure their HTTP client to present the cert when calling `$AGENTRY_GATEWAY_ENDPOINT`. See [Agent Runtime Contract](./RUNTIME_CONTRACT.md).

This mode is **CNI-independent**: identity is cryptographically attested by the certificate, not by any network-layer header that an intermediate hop could modify. An agent cannot claim a different identity without a CA-signed certificate for that identity — and the CA key is not reachable from any agent Pod.

**Agent/AgentTask Pods MUST use mTLS.** Their ServiceAccount tokens are deliberately not accepted by the gateway. If they were, a compromised agent could bypass certificate rotation by authenticating with its SA token instead, and an agent whose cert was revoked could continue calling the gateway indefinitely. See [Agent→Gateway Authentication](./SECURITY.md#agent-gateway-authentication) for the full analysis.

Provider routing for Agentry-managed Pods runs the full chain — Agent/AgentClass `allowedProviders`, then ModelProvider `allowedNamespaces`/`models`. See [Provider Routing § mTLS tier](#provider-routing).

### Mode 2 — ServiceAccount bearer token (gateway-only tier)

Existing workloads running in user namespaces (Deployments, StatefulSets, Jobs that the platform team wants to grant LLM-provider access to without adopting the full Agent CRD) use their projected ServiceAccount token. The caller sets:

```
Authorization: Bearer <projected-sa-token>
```

On receipt, the gateway runs a Pod-ownership precheck **before** any token validation, then performs a `TokenReview`:

0. **Pod-ownership precheck.** The gateway resolves the request's source IP to a Pod via its Pod informer cache. If the Pod has an `ownerRef` pointing to an `Agent` or `AgentTask` resource (or carries the Agentry-managed label set), the request is rejected with `401 Unauthorized` regardless of the bearer token presented. Agentry-managed Pods are required to use mTLS — SA-token auth is reserved for the gateway-only tier. This is what makes cert rotation the single revocation mechanism for the mTLS tier: a compromised Agent/AgentTask Pod cannot fall back to its projected ServiceAccount token to bypass cert revocation. The precheck runs before the `TokenReview` apiserver call so a hostile Pod cannot exploit `TokenReview` latency or unavailability.
1. POST the token to `authentication.k8s.io/v1/tokenreviews`. Include the expected audience (`agentry-gateway`) so the apiserver rejects tokens minted for a different audience.
2. On `status.authenticated: true`, parse `status.user.username`. It has the form `system:serviceaccount:<namespace>:<sa>`; the middle segment is the authoritative namespace.
3. Cache the validation result keyed by the token's SHA-256 hash for the token's remaining lifetime (`status.expirationTimestamp` minus a 60s safety margin). Subsequent requests from the same token hit the cache and skip the apiserver roundtrip. The Pod-ownership precheck is **not** cached — it re-runs on every request, since Pod identity at a given source IP can change.
4. Perform the source-IP cross-check: the Pod at the request's source IP (from the Pod informer) must be in the namespace returned by `TokenReview`. This closes the gap where a stolen token could be used from a different Pod.

The gateway's ServiceAccount needs `create` on `authentication.k8s.io/v1/tokenreviews` (cluster-scoped) — see [SECURITY.md § Gateway ServiceAccount](./SECURITY.md#gateway-serviceaccount-permissions).

Token audiences are set by workloads via a `projected` volume with `audience: agentry-gateway`. Using an explicit audience prevents generic `kubernetes.default.svc` tokens from being accepted — a stolen kubelet token cannot be reused against the gateway.

Provider routing for this tier is governed by `ModelProvider.spec.allowedNamespaces` and `spec.models` only — the gateway has no Agent or AgentClass to consult. See [Provider Routing § Gateway-only tier](#provider-routing).

### Source-IP cross-check (both modes)

After the authentication step produces a claimed (namespace, …) pair, the gateway looks up the source IP in its Pod informer cache and confirms the Pod's namespace matches. Mismatch → request rejected. This catches:

- Stolen client certificate presented from a different Pod (mode 1).
- Stolen SA token presented from a different Pod (mode 2).

The gateway maintains a Pod informer cache for this lookup and for provider-routing resolution and activity tracking. The Pod informer must be fully synced before the gateway's readiness probe passes — see [Gateway Readiness](#gateway-readiness).

**Pod IP reassignment**: when a Pod is deleted and a new Pod receives the same IP (common in small CIDR ranges), the informer cache may briefly map the old Pod. The gateway MUST process Pod delete events before accepting traffic from recycled IPs. In practice, the watch event for Pod deletion arrives before the new Pod is scheduled, so the window is negligible.

See [Agent→Gateway Authentication](./SECURITY.md#agent-gateway-authentication) for the full security analysis of both modes, including threat-model coverage.

---

## TLS on the LLM Gateway Listener

The LLM Gateway listener serves TLS to protect LLM request and response payloads in transit within the cluster. Without TLS, prompts and completions traverse the cluster network in plaintext, which is unacceptable when agent containers run untrusted code on shared nodes. See [In-cluster TLS](./SECURITY.md#in-cluster-tls-bidirectional) for the full security analysis.

**cert-manager is a required dependency.** Agentry uses cert-manager to manage the Agentry CA and all leaf certificates (gateway serving cert, controller activator cert, per-agent serving/client certs). The Helm chart ships the cert-manager resources (two `ClusterIssuer`s and the gateway/controller `Certificate` objects) but not the cert-manager controller itself — clusters must have cert-manager installed. Teams with an existing cert-manager deployment reuse it. This replaces an earlier operator-managed CA approach; see [V1 design note in SECURITY.md](./SECURITY.md#in-cluster-tls-bidirectional).

**Trust chain**:

1. Chart installs a cluster-scoped self-signed `ClusterIssuer` named `agentry-selfsigned`.
2. Chart installs a `Certificate` in `agentry-system` named `agentry-ca` whose `issuerRef` points at `agentry-selfsigned` and which has `isCA: true`. This is the Agentry root. Long-lived (default 5y).
3. Chart installs a cluster-scoped `ClusterIssuer` named `agentry-ca-issuer` whose `ca.secretName` is `agentry-ca`'s output Secret (read from `agentry-system`). All Agentry leaf certs — including the per-Agent and per-AgentTask certs created in user namespaces — are issued from this `ClusterIssuer`. A `ClusterIssuer` is used instead of a namespace-scoped `Issuer` because cert-manager's `issuerRef` on a `Certificate` does not resolve across namespaces to a namespaced `Issuer`; a `ClusterIssuer` is the idiomatic way to let `Certificate` resources in user namespaces reference a signing key that lives in `agentry-system`.
4. Chart installs a `Certificate` for the gateway serving cert (`agentry-gateway-tls`) issued from `agentry-ca-issuer`. SAN: `agentry-gateway.agentry-system.svc.cluster.local`, `agentry-gateway.agentry-system.svc`, `localhost`. Usages: `server auth`, `client auth` (the gateway also presents this cert when dialing the controller's activator / activity / channels-health endpoints). The Helm value `gateway.externalHostnames` (see [DEPLOYMENT.md § Helm Chart Contents](./DEPLOYMENT.md#helm-chart-contents)) extends this SAN list with operator-supplied public hostnames; required when the User listener is exposed via TLS pass-through Ingress.
5. Controller deployment ships with a `Certificate` for the activator / activity-API / channels-health serving cert (see [CONTROLLER_RECONCILERS.md](./CONTROLLER_RECONCILERS.md)). Usages: `server auth`, `client auth` (the controller also presents this cert when dialing the gateway's activity endpoint).
6. The `AgentReconciler` creates a `Certificate` per Agent (owner-referenced from the Agent) — see [Agent Serving & Client TLS](#agent-serving--client-tls) below.

**Certificate rotation**: cert-manager rotates each leaf continuously. Chart defaults:

- Gateway cert: `spec.duration: 2160h` (90d), `spec.renewBefore: 720h` (30d).
- Per-agent cert: `spec.duration: 2160h` (90d), `spec.renewBefore: 720h` (30d).
- Agentry CA: `spec.duration: 43800h` (5y), `spec.renewBefore: 8760h` (1y).

When a `Certificate`'s Secret is updated by cert-manager, kubelet updates the projected volume in any Pod that mounts it, and the consumer (gateway, controller, agent) reloads from disk. The gateway watches `agentry-gateway-tls` for changes; starter templates (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)) demonstrate the inotify-based reload pattern that custom images must implement.

**Agent trust bundle**: every agent Pod mounts the Agentry CA at `/var/run/agentry/ca.crt` (the `$AGENTRY_CA_CERT` env var points at this path). This is a projected volume sourced from a ConfigMap projected into the agent's namespace by `trust-manager`. The Helm chart installs a `trust-manager` `Bundle` resource whose source is the `agentry-ca` Secret in `agentry-system` and whose target writes a ConfigMap named `agentry-ca` into every non-system namespace selected by the bundle's `target.namespaceSelector` (default excludes `kube-system`, `kube-public`, `kube-node-lease`; overridable via the Helm value `trustManager.bundleSelector`) — see [DEPLOYMENT.md § Certificate Lifecycle](./DEPLOYMENT.md#certificate-lifecycle). Agent HTTP clients must trust this CA when calling `$AGENTRY_GATEWAY_ENDPOINT`. Starter templates handle this. `trust-manager` is a required dependency alongside cert-manager.

**CA rotation**: cert-manager re-issues the root `agentry-ca` `Certificate` within `spec.renewBefore` of expiry. During the overlap window, the bundle Secret contains both the old and new CA certificates, so no leaf cert is ever trusted by only one side. Once cert-manager has rotated all leaves (gateway cert, controller cert, every per-agent cert) to be signed by the new root, the old CA falls out of the bundle automatically. No operator code is required for CA rotation — this was the main motivation for adopting cert-manager.

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

- `/v1/messages`, `/v1/chat/completions`, `/v1/completions`, `/v1/agent/heartbeat`, `/v1/task/complete`: if `r.TLS.PeerCertificates` is non-empty, follow the mTLS path (Mode 1 — extract namespace from SAN, enforce the SAN-shape and label-count rules). If empty, follow the bearer-token path (Mode 2 — first run the Pod-ownership precheck described in [Mode 2 § step 0](#mode-2--serviceaccount-bearer-token-gateway-only-tier) to reject Agent/AgentTask Pods, then `TokenReview`-validate the `Authorization: Bearer <token>` header). If both auth materials are absent, return `401 Unauthorized`. If both are present, the mTLS path wins and the bearer header is ignored — see [Namespace Identification](#namespace-identification).
- `/v1/activity`, `/v1/channels/health`: require a client cert whose SAN matches the controller Service DNS. Empty `r.TLS.PeerCertificates` returns `401 Unauthorized`; a present-but-non-matching SAN returns `403 Forbidden`. There is no fallback to bearer-token auth on these paths — they are controller-only.

Path-conditional middleware is the only correct way to express this on Go's `crypto/tls`: setting `RequireAndVerifyClientCert` on the listener would lock out gateway-only-tier callers (the TLS handshake would fail before the request reached the path router), and setting `NoClientCert` would silently downgrade the mTLS tier (cert presented but never verified).

### Agent Serving & Client TLS

The User Gateway's delivery to agent Services (`POST /v1/message`) is over HTTPS. The AgentReconciler creates a cert-manager `Certificate` per Agent named `{agentName}-tls` in the Agent's namespace, owner-referenced to the Agent (so it is garbage-collected on Agent deletion). Its `issuerRef` is `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`. A `ClusterIssuer` is used because `Certificate` resources in user namespaces cannot reference a namespaced `Issuer` in `agentry-system` across the namespace boundary. The `spec.secretName` output Secret is mounted into the agent Pod at `/var/run/agentry/tls.crt` / `tls.key`. The certificate SAN list includes:

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

This format uniquely identifies the (provider, model) pair and eliminates ambiguity when multiple ModelProviders offer models with similar names (e.g., a managed Anthropic endpoint and an OpenAI-compatible proxy both serving Claude models). The agent is always responsible for constructing the qualified `provider/model` name in its API calls.

---

## Provider Routing

Provider routing differs between the two authentication tiers because the gateway-only tier has no Agent resource to consult. Both variants run after [Namespace Identification](#namespace-identification) has produced an authenticated namespace.

### mTLS tier (Agentry-managed Pods)

Agents and AgentTasks created by the controller have an Agent (or AgentTask) resource with `spec.providers` and an AgentClass with `allowedProviders`. The gateway walks the full chain:

1. **Source IP -> Pod**: resolved from the Pod informer cache (see [Namespace Identification](#namespace-identification)).
2. **Pod -> Agent**: the Pod's ownerRef identifies the Agent (or AgentTask) resource. The gateway maintains an Agent informer cache for this lookup.
3. **Agent -> allowed providers**: the Agent's `spec.providers` lists the ModelProviders this agent may use. The referenced providers must also appear in the AgentClass's `allowedProviders`.
4. **Model name -> ModelProvider**: the gateway parses the `provider/model` qualified name from the request body. The provider prefix must match a `providerRef` in the Agent's `spec.providers`. If it does not, the request is rejected.
5. **ModelProvider -> upstream**: the gateway reads the ModelProvider's `spec.endpoint`, `spec.type`, and credentials to forward the request. The namespace must also be in the ModelProvider's `allowedNamespaces`.

This chain ensures that an agent can only reach ModelProviders explicitly listed in its spec, which in turn must be in the AgentClass's `allowedProviders` and must include the agent's namespace in `allowedNamespaces`. All three access checks (Agent -> ModelProvider -> Namespace) must pass.

### Gateway-only tier (TokenReview)

Existing workloads that authenticate with a projected ServiceAccount bearer token have **no Agent resource**, so steps 2–4 above do not apply. Routing is governed by the ModelProvider's own allowlist plus its model list:

1. **Token -> namespace**: `TokenReview` yields the caller's authenticated namespace (see [Mode 2](#mode-2--serviceaccount-bearer-token-gateway-only-tier)).
2. **Model name -> ModelProvider**: the gateway parses the `provider/model` qualified name from the request body. The provider prefix must resolve to an existing `ModelProvider` by `metadata.name`; if not, the request is rejected with `400 invalid_request`.
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

The gateway uses the detected format to parse the request body (extracting the model name and other fields), then forwards to the upstream provider.

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

The ModelProviderReconciler reads this ConfigMap every 30s, **filters out any per-replica entries whose `period` does not match the current period**, sums the remaining partials, writes the `_canonical` key with the total, and updates `status.budgetUsage` on the ModelProvider. Gateway replicas read the `_canonical` key on startup to initialize their local counters. This avoids a Prometheus dependency and works with existing ConfigMap RBAC.

**Period tag rationale**: at period rollover (midnight UTC), gateway replicas detect the new period on their first incoming request and reset their local counter to zero. Because replicas transition independently, there is a window where some replicas have written new-period partials and others still hold old-period totals. Without the `period` field, the reconciler would sum mixed values and produce an incorrect canonical total. By tagging each entry, the reconciler skips old-period entries until all replicas have transitioned, giving a correct (if slightly underestimated) total during the rollover window — which is acceptable for a soft guardrail.

**Budget state on crash**: if all gateway replicas crash simultaneously, up to 10s of spend data (the partial-write interval) may be lost. This is acceptable given that budgets are soft limits — the bounded loss is small relative to typical budget thresholds. Gateway replicas re-initialize from the `_canonical` ConfigMap value on restart.

This means budget enforcement is **approximate under high concurrency** — replicas can collectively overspend before the next reconcile cycle. The overspend is bounded by: `number_of_replicas x max_calls_per_second_per_replica x cost_per_call x reconcile_interval_seconds`. For typical deployments (2-3 replicas, 1 call/sec peak per replica, $0.01-0.10 per call, 30s interval), the maximum overspend per cycle is roughly $0.60-$9.00, which is acceptable for a soft guardrail.

**Agentry's budget feature is spend visibility and soft guardrails, not a hard financial cap.** This is an explicit design decision, not a limitation to be fixed. Teams requiring hard caps should use provider-level account limits in addition to Agentry's per-namespace guardrails.

**Budget period rollover**: budget periods roll over at midnight UTC. Each gateway replica detects the period change on its first request of the new period and resets its local counter, writing a new-period entry (with the updated `period` field) to its ConfigMap key. The reconciler, filtering by the current period, excludes old-period entries during the rollover window — the canonical total may be temporarily underestimated until all replicas have written new-period entries, which is acceptable for soft guardrails. Once the new period is fully established, the controller archives the previous period's totals to ModelProvider status for auditability, deletes all per-replica keys from the budget ConfigMap, and writes a fresh `_canonical: {}`.

**Stale replica cleanup**: when a gateway replica is scaled down or replaced, its entry in the budget ConfigMap persists. The ModelProviderReconciler cross-references ConfigMap keys against the current set of gateway Pod names and deletes stale entries before summing partials. This prevents inflated spend totals from terminated replicas.

---

## Credential Handling

Provider credentials are stored as Secrets in `agentry-system` and referenced by ModelProvider. The gateway reads these Secrets directly at startup and watches them for rotation. Credentials never leave `agentry-system` — there is no per-agent or per-namespace credential copying.

When a credential Secret is updated, the gateway's Secret watcher picks up the change and refreshes the in-memory credential without a restart.

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
| Timeout before any response bytes | Fall back | Treated like a connection error — the request never landed. |
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

`maxFallbackDepth` (default `3`, set via Helm `gateway.maxFallbackDepth` → `AGENTRY_MAX_FALLBACK_DEPTH`) bounds the **total number of providers attempted per request, including the primary** — not the nesting depth of the tree. With the default, the gateway tries at most the primary plus two others before giving up, regardless of how the fallback tree is shaped. This is the latency guarantee: no single request can wait on more than `maxFallbackDepth` provider round-trips.

If the chain is exhausted or the cap is reached without a successful response, the gateway returns `502 provider_error`. Circular references are rejected at reconcile time by the [ModelProviderReconciler](./CONTROLLER_RECONCILERS.md#modelproviderreconciler), so cycles should never reach the gateway; the runtime `visited` check is defense in depth.

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

The gateway Pod's readiness probe (`GET /readyz` on the internal health port) returns `200` only when **all** of the following are true:

1. The **LLM listener** on `:8443` is bound and accepting TLS connections. The probe performs a local dial to confirm.
2. The **User listener** on `:8080` is bound and accepting TLS connections (both listeners use the `agentry-gateway-tls` certificate). The probe performs a local TLS dial to confirm.
3. **All informer caches** the request path depends on have completed their initial sync (`cache.WaitForCacheSync` returned true for each): `Pod` (source-IP → namespace resolution), `Agent` and `AgentTask` (provider-routing ownerRef resolution, hibernation-state checks), `AgentChannel` (webhook path → target Agent lookup), `ModelProvider` (model validation, `allowedNamespaces`, fallback chain traversal). Until every cache is synced, namespace identification, provider routing, and channel routing would either fail or return spurious `404` / `403` / `invalid_request` responses while caches hydrate.
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
| Gateway replica not ready (listener dial fails, any of the dependent informers not synced — Pod, Agent, AgentTask, AgentChannel, ModelProvider — or cert not yet issued) | Readiness probe returns 503; replica excluded from Service endpoints until all checks pass. See [Gateway Readiness](#gateway-readiness) |
| Provider API down | Fallback chain walked (same-type providers only, up to `maxFallbackDepth` depth); if all providers in the chain fail, request fails with `502 provider_error` |
| Budget exhausted | Request blocked (`429 budget_exhausted` with `Retry-After` header) or degraded per policy; Warning event emitted on ModelProvider |
| `TokenReview` apiserver unreachable (mode 2 only) | Gateway returns `503 Service Unavailable` to the caller for requests that miss the token cache; mTLS requests and cached-token requests are unaffected |
| CNI does not support FQDN egress policy but AgentClass sets `allowedHosts` | AgentClassReconciler emits a `Warning` event and ignores `allowedHosts`; `allowedCIDRs` alone governs egress. See [AgentClassReconciler](./CONTROLLER_RECONCILERS.md#agentclassreconciler) |
