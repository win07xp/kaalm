# TLS on the cluster listener

The cluster listener (`:8443`) serves TLS so that every payload crossing it, LLM requests and responses, brokered tool calls, and internal endpoint traffic, is protected in transit. Agent containers run untrusted code on shared nodes, so plaintext on the cluster network is not acceptable: `$KAALM_GATEWAY_ENDPOINT` is an `https://` URL.

The trust chain that produces the certificates, the rotation defaults, and the CA re-key runbook are on [In-cluster TLS](../security/tls.md#in-cluster-tls). This page covers the listener's certificate, its trust material, and how each path enforces client authentication.

## Where the listener's TLS material comes from

cert-manager and trust-manager are required dependencies ([Trust chain](../security/tls.md#trust-chain)). Two artifacts matter to the listener:

**The serving certificate, `kaalm-gateway-tls`.** Issued from `kaalm-ca-issuer` by a chart-installed `Certificate`.

| Property | Value |
|---|---|
| SANs | `kaalm-gateway.kaalm-system.svc.cluster.local`, `kaalm-gateway.kaalm-system.svc`, `localhost`, plus every name in `gateway.externalHostnames` ([Chart values that bind the PKI](../operations/deployment.md#chart-values-that-bind-the-pki)) |
| Usages | `server auth` and `client auth`. The gateway presents the same certificate as a client when it dials the controller's activator and when it delivers `POST /v1/message` to an agent. |

`gateway.externalHostnames` is required when the user listener is exposed through a TLS pass-through Ingress, because the Ingress forwards the handshake and external clients verify `kaalm-gateway-tls` against the hostname they dial ([TLS and Ingress](user/overview.md#tls-and-ingress)). Rotation follows the chart defaults in [Rotation defaults](../security/tls.md#rotation-defaults).

**The trust bundle, the `kaalm-ca-system` ConfigMap.** The gateway never reads the CA Secret. Its trust material is the `kaalm-ca-system` ConfigMap, which a trust-manager Bundle projects into `kaalm-system` ([Trust bundle projection](../security/tls.md#trust-bundle-projection)). The gateway verifies inbound client certificates and the peers it dials against it.

### Reload mechanism

The gateway re-reads the serving certificate and the CA bundle on the next handshake after either file changes. kubelet updates the projected volume when cert-manager renews the Secret, so a renewed certificate is served with no restart.

Two reload paths are distinct, and both are needed:

- A **certificate change** reloads the serving certificate and, because the same certificate is used outbound, the client certificate.
- A **CA-bundle change** rebuilds both trust pools the gateway holds: the inbound `ClientCAs` pool that verifies agent, controller, and console certificates, and the outbound `RootCAs` pool that verifies the peers the gateway dials. Rebuilding only one leaves the other stale, and a CA re-key then breaks that direction once leaves are re-issued under the new key.

The both-pools rule applies to every Kaalm component that speaks mTLS in both directions. The agent-side obligation is [Certificate reload on rotation](../runtime/contract.md#certificate-reload-on-rotation) on the runtime contract, and the reason an agent must watch the mount directory rather than the files is on [Starter templates](../runtime/starter-templates.md#why-the-watch-is-on-the-directory-not-the-file).

The listener does no CRL or OCSP check, so a leaked leaf stays valid until its `notAfter` unless the CA is re-keyed ([Containment, not revocation](../security/tls.md#containment-not-revocation)). A CA re-key is a manual runbook ([CA renewal and re-key](../security/tls.md#ca-renewal-and-re-key)).

## Mutual TLS on the listener

Kaalm-managed Pods present their per-workload certificate as the client certificate when calling `$KAALM_GATEWAY_ENDPOINT`. The gateway verifies it against the Kaalm CA and reads the SAN to identify the workload and its namespace ([Mode 1](llm/workload-identity.md#mode-1-mtls-client-certificate)). A custom image configures its HTTP client as the [TLS requirements](../runtime/contract.md#tls-requirements) describe.

Gateway-only-tier workloads present no client certificate. They authenticate with a bearer token ([Mode 2](llm/workload-identity.md#mode-2-serviceaccount-bearer-token)), so the handshake must complete without one. That optionality is what forces the per-path design below.

### Controller-only paths on the same socket

`GET /v1/activity` and `GET /v1/channels/health` are served on the same socket but require a client certificate whose SAN is the controller Service DNS (`kaalm-controller.kaalm-system.svc.cluster.local` or `kaalm-controller.kaalm-system.svc`). The controller presents `kaalm-controller-tls`. Agent and AgentTask certificates are valid CA-signed certificates whose SANs do not match, so they are rejected: a compromised agent cannot use its own certificate to read activity or channel health across namespaces. `POST /v1/test-chat` and `GET /v1/spend` apply the same rule with the console Service DNS and `kaalm-console-tls` ([Internal endpoint authentication](../security/rbac.md#internal-endpoint-authentication)).

## Per-path client auth enforcement

One `tls.Config.ClientAuth` value cannot express a requirement that differs by path. The listener therefore sets `ClientAuth: tls.VerifyClientCertIfGiven`: a client certificate is verified against the Kaalm CA when one is offered, and the handshake completes without one. The handshake decides nothing on its own: every decision is made in HTTP middleware, per path. A routing bug in the path-to-regime mapping would let agent-certificate holders reach controller-only paths, so that mapping, not the TLS configuration, is what authorization depends on. [The :8443 listener profile](overview.md#the-8443-listener-profile) is the table form of it. The four regimes:

**Dual mode** (`/v1/messages`, `/v1/chat/completions`, `/v1/completions`, `/v1/mcp/*`). With a client certificate, the SAN must parse as an Agent or AgentTask identity, and the source IP must resolve to a Pod in that namespace; a bearer header is ignored. Without one, the bearer token must be present, must not come from a Kaalm-managed Pod, must pass `TokenReview`, and the source IP must resolve to a Pod in the token's namespace ([Source-IP cross-check](llm/workload-identity.md#source-ip-cross-check-both-modes)).

![Activity diagram of the dual-mode regime. After the handshake, a request with a client certificate answers 403 invalid_cert when the SAN does not parse and 401 when the source IP is outside the SAN namespace, otherwise it is Mode 1. A request without one answers 401 when there is no bearer header, 401 when the source Pod is Kaalm-managed, 503 internal_unavailable when TokenReview is unreachable, 401 when the token is rejected, and 401 when the source IP is outside the token namespace, otherwise it is Mode 2. Both modes reach the handler.](../diagrams/per-path-auth-dual-mode.svg)

**Agent report** (`/v1/agent/heartbeat`, `/v1/task/complete`). A client certificate is required; there is no bearer fallback, because gateway-only-tier workloads have no Agent or AgentTask identity and nothing to report. The SAN must parse, its kind must match the path (Agent for the heartbeat, AgentTask for task completion), and the source IP must resolve to a Pod in the SAN namespace. Task completion uses the live API-server fallback for the cross-check; the heartbeat uses the informer cache alone ([`POST /v1/agent/heartbeat`](api/agent-endpoints.md#post-v1agentheartbeat)).

![Activity diagram of the agent-report regime. After the handshake, no client certificate answers 401, a SAN that does not parse answers 403 invalid_cert, a SAN kind that does not match the path answers 403 access_denied, and a source IP outside the SAN namespace answers 401. Otherwise the request reaches the handler.](../diagrams/per-path-auth-agent-report.svg)

**Controller only and console only** (`/v1/activity`, `/v1/channels/health`; `/v1/test-chat`, `/v1/spend`). A client certificate is required, its SAN must be the Service DNS of the component the path serves ([Controller-only paths on the same socket](#controller-only-paths-on-the-same-socket)), and the source IP must resolve to a Pod in the operator namespace. The SAN names a `kaalm-system` Service, not a tenant namespace, so the cross-check runs against `kaalm-system`: a stolen controller or console certificate presented from a tenant Pod is rejected.

![Activity diagram of the controller-only and console-only regimes. After the handshake, no client certificate answers 401, and a SAN that is not the path's component identity answers 403 access_denied, and a source IP outside the operator namespace answers 401. Otherwise the request reaches the handler.](../diagrams/per-path-auth-internal.svg)

**Any other path** answers `400 invalid_request` before any credential is examined, and the answer never echoes the path.

The handlers, not the regimes, check the HTTP method. `/v1/agent/heartbeat`, `/v1/task/complete`, and `/v1/test-chat` answer `405 invalid_request` to any method other than `POST`, and `/v1/activity`, `/v1/channels/health`, and `/v1/spend` to any method other than `GET`, each with an `Allow` header. The two `403` types are defined on [The :8443 listener profile](overview.md#the-8443-listener-profile).

Per-path middleware is the only correct way to express this on Go's `crypto/tls`. `RequireAndVerifyClientCert` on the listener would lock out gateway-only-tier callers, because the handshake would fail before the request reached the path router. `NoClientCert` would silently downgrade the mTLS tier: a certificate would be presented but never verified.

## Agent-side TLS

The per-Agent and per-AgentTask certificates, their SANs, and their mounts are the other end of the mTLS pair. Their lifecycle is on [In-cluster TLS](../security/tls.md#lifecycle-of-an-agent-tls-serving-certificate), and the obligations on the container are items 1, 3, and 4 of [The runtime contract](../runtime/contract.md).
