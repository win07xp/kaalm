# Listener TLS

The LLM Gateway listener serves TLS to protect LLM request and response payloads in transit within the cluster. Without TLS, prompts and completions traverse the cluster network in plaintext, which is unacceptable when agent containers run untrusted code on shared nodes. `$AGENTRY_GATEWAY_ENDPOINT` is an `https://` URL: TLS is not optional.

This page covers what an implementer of the listener needs: which certificate the listener presents, where its trust material comes from, how the socket is configured, and how each path enforces client authentication. The trust chain that produces those certificates, the CA rotation semantics, and the CA re-key runbook are described once in [In-cluster TLS](../../security/tls.md#in-cluster-tls); this page links there rather than restating them.

## Where the Listener's TLS Material Comes From

**cert-manager and trust-manager are required dependencies.** Agentry uses cert-manager to manage the Agentry CA and every leaf certificate (gateway serving cert, controller activator cert, per-agent serving/client certs). The Helm chart ships the cert-manager resources (two `ClusterIssuer`s and the gateway/controller `Certificate` objects) but not the cert-manager controller itself, so clusters must already have cert-manager installed. Teams with an existing cert-manager deployment reuse it. This replaces an earlier operator-managed CA approach; see the [V1 design note on in-cluster TLS](../../security/tls.md#in-cluster-tls).

The chain in brief: a self-signed `ClusterIssuer` (`agentry-selfsigned`) issues the Agentry root `Certificate` (`agentry-ca`, `isCA: true`), which backs the `agentry-ca-issuer` `ClusterIssuer` that signs every Agentry leaf, including the per-Agent and per-AgentTask certs created in user namespaces. The full chain, including why the CA `Certificate` and its Secret must live in cert-manager's cluster resource namespace and why a `ClusterIssuer` is used rather than a namespaced `Issuer`, is in [In-cluster TLS](../../security/tls.md#in-cluster-tls).

Two artifacts matter to the listener itself:

**The serving certificate: `agentry-gateway-tls`.** Issued from `agentry-ca-issuer` by a chart-installed `Certificate`.

- SANs: `agentry-gateway.agentry-system.svc.cluster.local`, `agentry-gateway.agentry-system.svc`, `localhost`.
- Usages: `server auth`, `client auth`. Client auth is included because the gateway also presents this same cert when dialing the controller's activator, activity, and channels-health endpoints.
- The Helm value `gateway.externalHostnames` (see [Helm Chart Contents](../../operations/deployment.md#helm-chart-contents)) extends this SAN list with operator-supplied public hostnames. It is required when the User listener is exposed via TLS pass-through Ingress.
- Chart rotation defaults: `spec.duration: 2160h` (90d), `spec.renewBefore: 720h` (30d).

**The trust bundle: the `agentry-ca` ConfigMap.** The gateway never reads the CA Secret directly. Its trust material arrives as a ConfigMap projected by trust-manager, the same bundle that agent Pods mount at `/var/run/agentry/ca.crt` (`$AGENTRY_CA_CERT`). The gateway verifies inbound client certificates against this bundle and uses it to verify the certs of in-cluster peers it dials.

### Reload Mechanism

When a `Certificate`'s Secret is updated by cert-manager, kubelet updates the projected volume in any Pod that mounts it, and the consumer (gateway, controller, agent) reloads from disk. The gateway watches both its serving-cert Secret (`agentry-gateway-tls`) and its projected `agentry-ca` trust bundle for changes. Starter templates (see [Starter Templates](../../runtime/starter-templates.md)) demonstrate the inotify-based reload pattern that custom images must implement.

Two reload paths are distinct, and both are required:

- A **cert/key change** reloads the serving certificate (and, since the same cert is used outbound, the client certificate) without a process restart.
- A **CA-bundle change** MUST rebuild **both** trust pools the gateway holds: the inbound server's `ClientCAs` pool (used to verify agent and controller client certs) and the outbound HTTP client's `RootCAs` pool (used to verify peers the gateway dials). Rebuilding only one leaves the other stale, and a CA re-key then breaks that direction once leaves are re-issued under the new key. The re-key runbook's dual-trust window is finite, so a component that misses the CA-bundle update does not recover on its own.

The both-pools rule applies to every Agentry component that speaks mTLS in both directions, which is all of them: the gateway, the controller, and each agent. The agent-side statement of the same obligation is [The Runtime Contract](../../runtime/contract.md) item 4; see [In-cluster TLS](../../security/tls.md#in-cluster-tls) for the re-key runbook that makes it matter.

Note that kubelet rotates projected volumes by swapping the `..data` symlink rather than rewriting the leaf files, so a watcher must be anchored to the mount directory, not to `tls.crt` / `ca.crt` themselves. [Starter Templates](../../runtime/starter-templates.md) covers this.

CA renewal itself is transparent: the `agentry-ca` `Certificate` pins `spec.privateKey.rotationPolicy: Never`, so renewal re-uses the key pair and every previously issued leaf still chains. A true CA re-key (compromise recovery) is a manual runbook. Both are documented in [In-cluster TLS](../../security/tls.md#in-cluster-tls). Note that certificates are contained, not revocable: there is no CRL or OCSP and Go's `crypto/tls` performs no revocation checking, so a leaked leaf stays valid until its `notAfter` unless the CA is re-keyed. See [Agent→Gateway Authentication](../../security/rbac.md#agent-to-gateway-authentication).

## Mutual TLS on the Listener

The LLM Gateway listener requires client certificates from agents in the Agentry-managed path. Agents present their per-agent TLS certificate (the same cert used for gateway→agent delivery) as the client cert when calling `$AGENTRY_GATEWAY_ENDPOINT`. The gateway verifies the client cert against `agentry-ca` and extracts the SAN to identify the agent and namespace. This is the primary identity mechanism for Agentry-managed Pods; see [Namespace Identification](workload-identity.md) for the SAN shapes and label-count rules.

Starter templates configure client cert presentation automatically. Custom images must configure their HTTP client to use `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY` as the client certificate.

Gateway-only-tier workloads do not present a client cert. They authenticate via `TokenReview` (see [Mode 2](workload-identity.md#mode-2-serviceaccount-bearer-token)), so client certs are optional on the TLS handshake for that path. That optionality is what forces the per-path design below.

### Controller-Only Paths on the Same Socket

`GET /v1/activity` is served on the same gateway TLS listener but requires a client cert whose SAN matches the controller Service DNS (`agentry-controller.agentry-system.svc.cluster.local` or `agentry-controller.agentry-system.svc`). The controller presents its `agentry-controller-tls` cert. Agent and AgentTask certs are rejected on this path because their SANs do not match: defense in depth against a compromised agent using a valid CA-signed certificate to query activity data across namespaces. See [Internal Endpoint Authentication](../../security/rbac.md#internal-endpoint-authentication).

`/v1/channels/health` is served on this listener too. Like `/v1/activity`, it requires an mTLS client cert whose SAN matches the controller Service DNS, and the same SAN-authorization rule applies: requests bearing gateway, agent, or AgentTask certs are rejected. It lives on port 8443, **not** the externally-exposed User listener on 8080, so that Ingress fronting 8080 cannot route an untrusted caller to this endpoint. See [TLS and Ingress](../user/overview.md#tls-and-ingress) for the listener-split rationale and [GET /v1/channels/health](../api/internal-endpoints.md#get-v1channelshealth).

## Per-Path Client Auth Enforcement

The LLM listener on `:8443` serves three authentication regimes on a single TLS socket:

1. **mTLS-required** for Agentry-managed Agent/AgentTask requests (Mode 1, see [Namespace Identification](workload-identity.md)).
2. **mTLS-optional** for gateway-only-tier `TokenReview` callers (Mode 2) who do not present a client cert.
3. **mTLS-required-with-SAN-authorization** for the controller's `/v1/activity` and `/v1/channels/health` calls.

A single `tls.Config.ClientAuth` value cannot express path-conditional requirements. The gateway therefore configures `ClientAuth: tls.VerifyClientCertIfGiven` at the handshake layer, so callers without a client cert can still complete the TLS handshake, and enforces per-path requirements in HTTP middleware.

**LLM proxy paths** (`/v1/messages`, `/v1/chat/completions`, `/v1/completions`, plus adapter-registered provider-specific paths, see [Request Format Detection](request-handling.md#request-format-detection)):

- If `r.TLS.PeerCertificates` is non-empty, follow the mTLS path (Mode 1: extract the namespace from the SAN, enforce the SAN-shape and label-count rules).
- If empty, follow the bearer-token path (Mode 2: first run the Pod-ownership precheck described in [Mode 2 § step 0](workload-identity.md#mode-2-serviceaccount-bearer-token) to reject Agent/AgentTask Pods, then `TokenReview`-validate the `Authorization: Bearer <token>` header).
- If both auth materials are absent, return `401 Unauthorized`.
- If both are present, the mTLS path wins and the bearer header is ignored. See [Namespace Identification](workload-identity.md).

**Agent-report paths** (`/v1/agent/heartbeat`, `/v1/task/complete`): **mTLS-only**. There is no bearer-token fallback on these paths, per the `:8443` auth profile in [The Agentry Gateway](../overview.md) and [HTTP API](../api/overview.md). Empty `r.TLS.PeerCertificates` returns `401 Unauthorized` regardless of any bearer header, because gateway-only-tier workloads have no Agent/AgentTask identity and nothing meaningful to report on these endpoints. The Agent-vs-AgentTask split is enforced at the handler (heartbeat: Agent only; task-complete: AgentTask only; the other kind gets `403`).

**Controller-only paths** (`/v1/activity`, `/v1/channels/health`): require a client cert whose SAN matches the controller Service DNS. Empty `r.TLS.PeerCertificates` returns `401 Unauthorized`; a present-but-non-matching SAN returns `403 Forbidden`. There is no fallback to bearer-token auth on these paths.

Path-conditional middleware is the only correct way to express this on Go's `crypto/tls`:

- Setting `RequireAndVerifyClientCert` on the listener would lock out gateway-only-tier callers, because the TLS handshake would fail before the request reached the path router.
- Setting `NoClientCert` would silently downgrade the mTLS tier: a cert would be presented but never verified.

## Agent Serving & Client TLS

The User Gateway's delivery to agent Services (`POST /v1/message`) is over HTTPS. The AgentReconciler creates a cert-manager `Certificate` per Agent named `{agentName}-tls` in the Agent's namespace, owner-referenced to the Agent so it is garbage-collected on Agent deletion. Its `issuerRef` is `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`. A `ClusterIssuer` is used because `Certificate` resources in user namespaces cannot reference a namespaced `Issuer` in another namespace across the namespace boundary.

The `spec.secretName` output Secret is mounted into the agent Pod at `/var/run/agentry/tls.crt` / `tls.key`. The certificate SAN list includes:

- `{agentName}.{namespace}.svc.cluster.local` (Service DNS)
- `{agentName}.{namespace}.svc`
- `{agentName}.{namespace}`

The same cert is used as a client cert when the agent calls `$AGENTRY_GATEWAY_ENDPOINT` (see [Namespace Identification](workload-identity.md)). Only the first of those SANs is a shape the gateway's identity extractor recognizes; the two short forms match no recognized suffix and are ignored.

Chart rotation defaults for the per-agent cert are `spec.duration: 2160h` (90d) and `spec.renewBefore: 720h` (30d). Rotation is fully owned by cert-manager: the reconciler does not batch re-issues or maintain rotation-state ConfigMaps.

### Agent Health Probes and TLS

Because the agent serves HTTPS on `$AGENTRY_HEALTH_PORT` using the same per-agent certificate, the readiness and liveness probes injected by the AgentReconciler must set `httpGet.scheme: HTTPS`. Kubernetes `httpGet` probes do not verify TLS certificates, so no additional CA configuration is required on the probe. See [Agent Runtime Contract](../../runtime/contract.md).
