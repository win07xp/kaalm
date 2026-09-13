# TLS on the cluster listener

The cluster listener (`:8443`) serves TLS to protect every payload that crosses it in transit within the cluster: LLM requests and responses, brokered tool calls, and the internal endpoints' traffic. Without TLS, prompts, completions, and tool-call content traverse the cluster network in plaintext, which is unacceptable when agent containers run untrusted code on shared nodes. `$KAALM_GATEWAY_ENDPOINT` is an `https://` URL: TLS is not optional.

This page covers what an implementer of the listener needs: which certificate the listener presents, where its trust material comes from, how the socket is configured, and how each path enforces client authentication. The trust chain that produces those certificates, the CA rotation semantics, and the CA re-key runbook are described once in [In-cluster TLS](../security/tls.md#in-cluster-tls); this page links there rather than restating them.

## Where the listener's TLS material comes from

**cert-manager and trust-manager are required dependencies.** Kaalm uses cert-manager to manage the Kaalm CA and every leaf certificate (gateway serving cert, controller activator cert, per-agent serving/client certs). The Helm chart ships the cert-manager resources (two `ClusterIssuer`s and the gateway/controller `Certificate` objects) but not the cert-manager controller itself, so clusters must already have cert-manager installed. Teams with an existing cert-manager deployment reuse it; [In-cluster TLS](../security/tls.md#in-cluster-tls) says why the operator manages no CA of its own.

The chain in brief: a self-signed `ClusterIssuer` (`kaalm-selfsigned`) issues the Kaalm root `Certificate` (`kaalm-ca`, `isCA: true`), which backs the `kaalm-ca-issuer` `ClusterIssuer` that signs every Kaalm leaf, including the per-Agent and per-AgentTask certs created in user namespaces. The full chain, including why the CA `Certificate` and its Secret must live in cert-manager's cluster resource namespace and why a `ClusterIssuer` is used rather than a namespaced `Issuer`, is in [In-cluster TLS](../security/tls.md#in-cluster-tls).

Two artifacts matter to the listener itself:

**The serving certificate: `kaalm-gateway-tls`.** Issued from `kaalm-ca-issuer` by a chart-installed `Certificate`.

- SANs: `kaalm-gateway.kaalm-system.svc.cluster.local`, `kaalm-gateway.kaalm-system.svc`, `localhost`.
- Usages: `server auth`, `client auth`. Client auth is included because the gateway also presents this same cert when dialing the controller's activator, activity, and channels-health endpoints.
- The Helm value `gateway.externalHostnames` (see [Helm chart contents](../operations/deployment.md#helm-chart-contents)) extends this SAN list with operator-supplied public hostnames. It is required when the User listener is exposed through a TLS pass-through Ingress.
- Chart rotation defaults: `spec.duration: 2160h` (90d), `spec.renewBefore: 720h` (30d).

**The trust bundle: the `kaalm-ca` ConfigMap.** The gateway never reads the CA Secret directly. Its trust material arrives as a ConfigMap projected by trust-manager, the same bundle that agent Pods mount at `/var/run/kaalm/ca.crt` (`$KAALM_CA_CERT`). The gateway verifies inbound client certificates against this bundle and uses it to verify the certs of in-cluster peers it dials.

### Reload mechanism

When a `Certificate`'s Secret is updated by cert-manager, kubelet updates the projected volume in any Pod that mounts it, and the consumer (gateway, controller, agent) reloads from disk. The gateway watches both its serving-cert Secret (`kaalm-gateway-tls`) and its projected `kaalm-ca` trust bundle for changes. Starter templates (see [Starter templates](../runtime/starter-templates.md)) demonstrate the inotify-based reload pattern that custom images must implement.

Two reload paths are distinct, and both are required:

- A **cert/key change** reloads the serving certificate (and, since the same cert is used outbound, the client certificate) without a process restart.
- A **CA-bundle change** MUST rebuild **both** trust pools the gateway holds: the inbound server's `ClientCAs` pool (which verifies agent and controller client certs) and the outbound HTTP client's `RootCAs` pool (which verifies the peers the gateway dials). Rebuilding only one leaves the other stale, and a CA re-key then breaks that direction once leaves are re-issued under the new key. The re-key runbook's dual-trust window is finite, so a component that misses the CA-bundle update does not recover on its own.

The both-pools rule applies to every Kaalm component that speaks mTLS in both directions, which is all of them: the gateway, the controller, and each agent. The agent-side statement of the same obligation is [The runtime contract](../runtime/contract.md) item 4; see [In-cluster TLS](../security/tls.md#in-cluster-tls) for the re-key runbook that makes it matter.

kubelet rotates projected volumes by swapping the `..data` symlink rather than rewriting the leaf files, so a watcher must be anchored to the mount directory, not to `tls.crt` or `ca.crt` themselves. [Starter templates](../runtime/starter-templates.md) covers this.

CA renewal itself is transparent: the `kaalm-ca` `Certificate` pins `spec.privateKey.rotationPolicy: Never`, so renewal re-uses the key pair and every leaf issued before the renewal still chains. A true CA re-key (compromise recovery) is a manual runbook. Both are documented in [In-cluster TLS](../security/tls.md#in-cluster-tls). Certificates are contained, not revocable: there is no CRL or OCSP and Go's `crypto/tls` performs no revocation checking, so a leaked leaf stays valid until its `notAfter` unless the CA is re-keyed. See [Agent to gateway authentication](../security/rbac.md#agent-to-gateway-authentication).

## Mutual TLS on the listener

The cluster listener requires client certificates from agents in the Kaalm-managed path. Agents present their per-agent TLS certificate (the same cert used for gateway to agent delivery) as the client cert when calling `$KAALM_GATEWAY_ENDPOINT`. The gateway verifies the client cert against `kaalm-ca` and extracts the SAN to identify the agent and namespace. This is the primary identity mechanism for Kaalm-managed Pods; see [Workload identity](llm/workload-identity.md) for the SAN shapes and label-count rules.

Starter templates configure client cert presentation. Custom images must configure their HTTP client to use `$KAALM_TLS_CERT` / `$KAALM_TLS_KEY` as the client certificate.

Gateway-only-tier workloads do not present a client cert. They authenticate with `TokenReview` (see [Mode 2](llm/workload-identity.md#mode-2-serviceaccount-bearer-token)), so client certs are optional on the TLS handshake for that path. That optionality is what forces the per-path design below.

### Controller-only paths on the same socket

`GET /v1/activity` is served on the same gateway TLS listener but requires a client cert whose SAN matches the controller Service DNS (`kaalm-controller.kaalm-system.svc.cluster.local` or `kaalm-controller.kaalm-system.svc`). The controller presents its `kaalm-controller-tls` cert. Agent and AgentTask certs are rejected on this path because their SANs do not match: defense in depth against a compromised agent using a valid CA-signed certificate to query activity data across namespaces. See [Internal endpoint authentication](../security/rbac.md#internal-endpoint-authentication).

`/v1/channels/health` is served on this listener too. Like `/v1/activity`, it requires an mTLS client cert whose SAN matches the controller Service DNS, and the same SAN-authorization rule applies: requests bearing gateway, agent, or AgentTask certs are rejected. It lives on port 8443, **not** the externally-exposed User listener on 8080, so that Ingress fronting 8080 cannot route an untrusted caller to this endpoint. See [TLS and Ingress](user/overview.md#tls-and-ingress) for the listener-split rationale and [GET /v1/channels/health](api/internal-endpoints.md#get-v1channelshealth).

## Per-path client auth enforcement

The cluster listener on `:8443` serves three authentication regimes on a single TLS socket, and a single `tls.Config.ClientAuth` value cannot express path-conditional requirements. The gateway therefore sets `ClientAuth: tls.VerifyClientCertIfGiven` at the handshake, so a caller without a client certificate can still complete it, and enforces the requirement per path in HTTP middleware.

![Activity diagram of per-path client-auth enforcement on :8443. After the TLS handshake with ClientAuth set to VerifyClientCertIfGiven, middleware routes by path family. Dual-mode paths (the LLM proxy paths and /v1/mcp/*) take Mode 1 when a client certificate is present, Mode 2 when only a bearer header is present, and 401 when neither is. The mTLS-only paths (/v1/agent/heartbeat, /v1/task/complete) return 401 without a client certificate and 403 when the certificate's SAN kind does not match the path. The controller-only paths (/v1/activity, /v1/channels/health) return 401 without a client certificate and 403 unless the SAN is the controller's. Every admitted request proceeds to its handler.](../diagrams/per-path-auth-enforcement.svg)

Reading the diagram: the handshake decides nothing. It is deliberately the weakest step, and every real decision is in the middleware below it, which is why the path-to-auth mapping, not the TLS configuration, is the detail that authorization depends on. The [`:8443` listener profile](overview.md#the-8443-listener-profile) on the gateway overview is the table form of the same mapping.

**Dual-mode paths**: the LLM proxy (`/v1/messages`, `/v1/chat/completions`, `/v1/completions`, plus adapter-registered provider-specific paths, see [Request format detection](llm/request-handling.md#request-format-detection)) and the tool plane (`/v1/mcp/*`, see [The tool plane](tool-plane.md)). The middleware reads `r.TLS.PeerCertificates`:

- Non-empty: Mode 1. The namespace comes from the SAN, with the SAN-shape and label-count rules enforced ([Workload identity](llm/workload-identity.md)). A bearer header, if also present, is ignored; mTLS wins.
- Empty with an `Authorization: Bearer <token>` header: Mode 2. The Pod-ownership precheck runs first, to reject Agent and AgentTask Pods, then `TokenReview` validates the token ([Mode 2](llm/workload-identity.md#mode-2-serviceaccount-bearer-token), step 0).
- Empty with no bearer header: `401 Unauthorized`.

**mTLS-only paths** (`/v1/agent/heartbeat`, `/v1/task/complete`): no bearer fallback, per the `:8443` profile and [HTTP API](api/overview.md). An empty `r.TLS.PeerCertificates` returns `401 Unauthorized` regardless of any bearer header, because gateway-only-tier workloads have no Agent or AgentTask identity and nothing to report on these endpoints. The listener admits either SAN form; the handler then splits by kind (heartbeat: Agent only; task-complete: AgentTask only) and returns `403 Forbidden` to the other kind.

**Controller-only paths** (`/v1/activity`, `/v1/channels/health`): a client certificate whose SAN matches the controller Service DNS. Empty `r.TLS.PeerCertificates` returns `401 Unauthorized`; a present but non-matching SAN returns `403 Forbidden`. No bearer fallback.

Path-conditional middleware is the only correct way to express this on Go's `crypto/tls`:

- `RequireAndVerifyClientCert` on the listener would lock out gateway-only-tier callers, because the handshake would fail before the request reached the path router.
- `NoClientCert` would silently downgrade the mTLS tier: a certificate would be presented but never verified.

A routing bug in the middleware would let agent-certificate holders reach controller-only paths, which is why the mapping is the security-critical detail of the listener.

## Agent serving and client TLS

The User Gateway's delivery to agent Services (`POST /v1/message`) is over HTTPS. The AgentReconciler creates a cert-manager `Certificate` per Agent named `{agentName}-tls` in the Agent's namespace, owner-referenced to the Agent so it is garbage-collected on Agent deletion. Its `issuerRef` is `{ name: "kaalm-ca-issuer", kind: "ClusterIssuer" }`. A `ClusterIssuer` is used because `Certificate` resources in user namespaces cannot reference a namespaced `Issuer` in another namespace across the namespace boundary.

The `spec.secretName` output Secret is mounted into the agent Pod at `/var/run/kaalm/tls.crt` / `tls.key`. The certificate SAN list includes:

- `{agentName}.{namespace}.svc.cluster.local` (Service DNS)
- `{agentName}.{namespace}.svc`
- `{agentName}.{namespace}`

The same cert is used as a client cert when the agent calls `$KAALM_GATEWAY_ENDPOINT` (see [Workload identity](llm/workload-identity.md)). Only the first of those SANs is a shape the gateway's identity extractor recognizes; the two short forms match no recognized suffix and are ignored.

Chart rotation defaults for the per-agent cert are `spec.duration: 2160h` (90d) and `spec.renewBefore: 720h` (30d). Rotation is done entirely by cert-manager: the reconciler does not batch re-issues or maintain rotation-state ConfigMaps.

### Agent health probes and TLS

Because the agent serves HTTPS on `$KAALM_HEALTH_PORT` using the same per-agent certificate, the readiness and liveness probes injected by the AgentReconciler must set `httpGet.scheme: HTTPS`. Kubernetes `httpGet` probes do not verify TLS certificates, so no additional CA configuration is required on the probe. See [The runtime contract](../runtime/contract.md).
