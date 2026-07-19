# Threat Model

This page enumerates the threats Agentry defends against, the threats it deliberately does **not** defend against, and the reasoning behind each boundary. It is organised into nine themes. Every threat has exactly one row; where a mitigation needs more than a sentence, the row states the decision and a note below the table carries the full argument.

Read [Trust Model](model.md#trust-model) first: several rows below resolve to "out of scope" purely because of where the trust boundary sits. A threat being out of scope is a design decision, not an oversight.

## Credential Containment and Egress

The central invariant is that LLM provider credentials never leave `agentry-system`. Agent Pods reach providers only by proxying through the gateway, and a synthesized NetworkPolicy is what makes that the only reachable path.

| Threat | Mitigation |
|---|---|
| Malicious agent container (Agentry-managed Pod) tries to call LLM providers directly | Credentials never leave `agentry-system`; the AgentReconciler synthesizes a per-Agent NetworkPolicy whose default-deny egress (standard k8s, no service mesh required) blocks direct egress from the agent Pod to provider IPs. Same for AgentTask Pods. |
| Gateway-only-tier workload calls LLM providers directly with its own keys | **Not mitigated by Agentry.** See note below. |
| Developer authors a permissive NetworkPolicy in their own namespace that broadens Agent Pod egress | **Out of scope by trust model.** See note below. |
| Developer bypasses gateway by embedding credentials in image | No mitigation at the platform level: process/review concern; mitigate via image scanning and registry controls. |

### Notes

**Gateway-only-tier workload calls providers directly.** Gateway-only-tier workloads are existing Deployments that the platform team has granted gateway access to via `TokenReview`; they have no Agent CR and therefore no Agentry-synthesized NetworkPolicy. Routing through the gateway is voluntary in this tier: a workload that holds its own provider credentials can bypass the gateway entirely. Platform teams adopting the gateway-only tier must apply their own default-deny egress NetworkPolicy (or use a service mesh) on those namespaces if they want to enforce gateway routing. The full Agent lifecycle tier remains the only path with automatic NetworkPolicy enforcement.

**Developer authors a permissive NetworkPolicy.** [§ Trust Model](model.md#trust-model) places the developer in the trusted tier; opting out of guardrails is the developer's choice, not a defended-against threat. NetworkPolicy is additive, so an additional permissive policy unions with Agentry's synthesized one: Agentry cannot prevent this through synthesis alone. Platform teams who treat developers as untrusted should restrict `networkpolicies` create/patch in user namespaces via cluster RBAC. The agent-container threat (the actual untrusted actor) cannot author NetworkPolicies: its ServiceAccount has no such permissions by default. See [§ Network Policy](model.md#network-policy) and [§ Protecting agent containers from LLM provider access](credentials.md#protecting-agent-containers-from-llm-provider-access).

## Workload Isolation

Agent containers execute LLM-generated code, so they are treated as the untrusted actor inside an otherwise trusted namespace.

| Threat | Mitigation |
|---|---|
| Developer deploys agent with resource bomb | AgentClass.maxLimits enforced; image allowlist prevents arbitrary images |
| Agent container executes LLM-generated code that attempts container escape | RuntimeClass (gVisor/Kata) provides kernel-level isolation; Pod Security Standards prevent privilege escalation |

See [§ RuntimeClass](model.md#runtimeclass) for the enforcement details.

## Credential Storage and Rotation

| Threat | Mitigation |
|---|---|
| Platform credentials leak via etcd backup | Standard k8s concern; encrypt etcd at rest |
| Stale credentials after rotation cause silent failures | Gateway watches Secrets for changes; ModelProviderReconciler verifies credential validity on each health check |
| Channel credential leaked from agent namespace | Channel credentials are stored in the agent's namespace; blast radius is limited to that namespace's channels. The platform team (not the developer) is responsible for rotation. |

Lifecycle detail lives in [§ Lifecycle of an LLM API key](credentials.md#lifecycle-of-an-llm-api-key).

## Component Compromise

These rows ask what an attacker gains by taking over Agentry's own control-plane components. The honest answer for credential reads is: everything in `agentry-system`. The scoping that exists is a least-privilege default and an integrity control against drift, not a hard boundary.

| Threat | Mitigation |
|---|---|
| Compromised operator reads credential Secrets | No cluster-wide Secret access; standing read scoped to `agentry-system`, which does contain every provider key. See note below. |
| Compromised gateway reads all LLM credentials | Gateway Secret access scoped to `agentry-system`; gateway image should be signed and verified; restrict who can update gateway Deployment |
| Compromised gateway writes malicious ConfigMaps to user namespaces | Gateway has **no `create` verb** on user-namespace ConfigMaps. See note below. |

### Notes

**Compromised operator reads credential Secrets.** The operator has no cluster-wide Secret access: its standing Secret read is scoped to `agentry-system`, which **does** contain every LLM provider key, so a compromised operator reads them all, the same blast radius as a compromised gateway (mitigate identically: image signing, restricted Deployment update rights, audit logging on `agentry-system` Secret access). In user namespaces the only reach is via dynamic per-AgentChannel Roles, each `resourceNames`-scoped to that channel's auth Secret(s); a compromised operator cannot *directly* enumerate or read arbitrary user-namespace Secrets.

Note the `escalate`/`bind` grants on `roles`/`rolebindings` (see [Operator ServiceAccount](rbac.md#operator-serviceaccount)) mean a fully compromised operator could widen those Roles and bind them to principals of its choosing. The per-channel scoping is an integrity control against drift and a least-privilege default, not a hard boundary against operator compromise.

**Compromised gateway writes malicious ConfigMaps.** The `{taskName}-completion` ConfigMap is pre-created by the AgentTaskReconciler with the AgentTask as `ownerRef`; the gateway's per-task Role grants only `update, patch` on that exact name (`resourceNames`-scoped, with `get` omitted since the write is a blind merge patch). The gateway has **no `create` verb** on user-namespace ConfigMaps, so a compromised gateway cannot introduce new ConfigMaps in user namespaces: it can mutate only the per-task and per-channel resources it has explicit name-scoped access to. See [§ Gateway ServiceAccount permissions](rbac.md#gateway-serviceaccount-permissions).

## Tenant Isolation and Budgets

Budgets are guardrails, not hard caps. That is a deliberate choice, and two rows here restate the consequence so it is not mistaken for a bug.

| Threat | Mitigation |
|---|---|
| One tenant exhausts another tenant's budget via shared provider | Per-namespace spend accounting; `allowedNamespaces` restricts access; budgets are soft limits with bounded overspend |
| Agent makes requests to unauthorized provider | Gateway validates model against ModelProvider.models and namespace against allowedNamespaces |
| Budget guardrails exceeded under high concurrency | Budgets are documented as soft limits with bounded overspend; hard caps at the provider account level recommended for strict requirements |
| Gateway-only tenant uses a provider their AgentClass would have denied in the mTLS tier | **Expected behavior, not a vulnerability.** See note below. |

### Notes

**Gateway-only tenant uses a provider AgentClass would have denied.** The gateway-only tier is deliberately not gated by AgentClass: those workloads have no Agent resource and therefore no `allowedProviders` to consult. Access control reduces to `ModelProvider.spec.allowedNamespaces` plus `spec.models`. Platform teams who need class-scoped provider policy must onboard workloads through the full Agent lifecycle tier. See [Provider Routing § Gateway-only tier](../gateways/llm/provider-routing.md).

## Channels and Webhooks

Inbound webhooks come from third parties that follow their own signing conventions, and outbound callbacks leave the gateway, which has stronger egress than any user namespace. Both directions get explicit treatment.

| Threat | Mitigation |
|---|---|
| Malicious message from channel platform | Webhook adapter authenticates inbound events (bearer token, HMAC signature) before processing |
| Captured inbound webhook is replayed against the gateway | Not preventable at the gateway: inbound HMAC is body-only with no timestamp. Cost bounded by budgets; side-effect dedup is the agent's job. See note below. |
| Caller with channel A's credentials fetches channel B's async response by supplying channel B's `requestId` to the poll endpoint | Poll endpoint asserts the stored response's channel labels match the authenticated AgentChannel; mismatch returns `404 Not Found`. See note below. |
| Developer uses `AgentChannel.spec.webhook.callbackUrl` as SSRF against internal cluster services (e.g., `kubernetes-dashboard.kube-system`, cloud metadata at 169.254.169.254) | `https://` plus internal-IP-range denial enforced at admission, re-checked and IP-pinned on every delivery. See note below. |
| Third party forges a callback POST to a developer's `callbackUrl` | The gateway signs every callback POST using `AgentChannel.spec.webhook.callbackAuth`, which CRD CEL makes mandatory whenever `callbackUrl` is set. See note below. |

### Notes

**Captured inbound webhook is replayed.** Inbound HMAC is body-only with no timestamp (see [Inbound webhook auth](../gateways/api/channel-webhook.md)): the deliberate cost of supporting arbitrary third-party senders that follow their own signing conventions (GitHub-style body-only HMAC, etc.). The gateway therefore cannot reject replays of a captured `(body, HMAC)` pair until the secret is rotated.

Cost replay is bounded by per-namespace LLM budgets (see [Multi-tenancy](../concepts/tenancy-and-tiers.md#multi-tenancy)). Side-effect replay is the agent's responsibility: agents performing non-idempotent inbound actions must dedup on a caller-supplied idempotency key or content hash. The gateway-side `messageId` dedup ([The Runtime Contract item 7](../runtime/contract.md)) addresses gateway-retry duplicates only, not external replay.

**Cross-channel async response fetch.** The poll endpoint authenticates the caller against the AgentChannel named by the `channelPath` query parameter, then asserts that the stored response's `agentry.io/channel-namespace` / `agentry.io/channel-name` labels match that same AgentChannel before returning the payload. `requestId` values are UUIDs but not secrets; the label check prevents cross-channel data leakage. Mismatches return `404 Not Found`, indistinguishable on the wire from "unknown `requestId`", to avoid confirming the existence of a cross-channel response (the gateway logs the mismatch with `reason=ChannelMismatch` for operator debugging). See [Async Webhook Response poll semantics](../gateways/api/async-responses.md).

**SSRF via `callbackUrl`.** The gateway has stronger egress than any user namespace, so an unrestricted `callbackUrl` would let the developer turn the gateway into a confused deputy. The AgentChannelReconciler enforces at admission/reconcile time that `callbackUrl` uses `https://` and that its host does not resolve to loopback, link-local, RFC1918, unique-local IPv6, or cloud-metadata IPs (see [Cross-Resource Validation rule 22](../resources/validation-and-defaulting.md#cross-resource-validation)).

On every delivery attempt the gateway re-resolves the host, re-applies the check, and **dials the exact IP that passed**: a custom dialer resolves once, range-checks the result, and connects to that pinned IP:port while preserving the Host header and SNI (see [Request Flow step 8](../gateways/user/overview.md#request-flow)). Handing the hostname back to the HTTP transport would let it re-resolve independently, re-opening the DNS-rebinding window the check exists to close. Platform teams may replace the deny-internal default with an explicit allowlist via the Helm value `gateway.callbackUrl.allowlist`.

**Forged callback POST.** The gateway signs every callback POST (success and error payloads alike) using `AgentChannel.spec.webhook.callbackAuth`: bearer (`Authorization: Bearer …`) or HMAC over the canonical string `"{requestId}\n{timestamp}\n{sha256(body)}"` with the timestamp in `X-Agentry-Timestamp`. `callbackAuth` is required by [cross-resource validation rule 25](../resources/validation-and-defaulting.md#cross-resource-validation) whenever `callbackUrl` is set: CRD CEL rejects AgentChannels that try to configure an unsigned callback, so unsigned callbacks cannot be deployed by accident. Receivers verify the signature using the same Secret material; replay is bounded by a 300s timestamp skew window (mirroring the polling-endpoint contract). See [Callback authentication](../gateways/api/async-responses.md).

## Identity and Namespace Spoofing

Every authorization decision the gateway makes keys off a namespace, so forging a namespace is the highest-value attack. Both auth modes attest identity cryptographically and then cross-check it topologically against the source Pod; both must agree. The full request-time flow, the SAN shapes, and the label counts live in [Namespace Identification](../gateways/llm/workload-identity.md).

| Threat | Mitigation |
|---|---|
| Agent spoofs namespace to bypass budget/access controls (mTLS tier) | Namespace comes from the `agentry-ca`-signed cert SAN; the CA key is unreachable from agent Pods. Two extra defenses close the dotted-name label-shift bypass. See note below. |
| Agent spoofs namespace (gateway-only-tier, token auth) | Namespace is extracted from the token's `status.user.username` returned by `TokenReview`, which the apiserver signs. An agent cannot forge a token for a different namespace: the token's signature is checked by the apiserver, not the gateway. Source-IP to Pod cross-check validates the Pod's actual namespace matches. Both cryptographic (apiserver signature) and topological (source IP) attestation must agree. |
| Gateway-only-tier tenant uses a ServiceAccount token from another namespace to read another tenant's budget | Impossible: `TokenReview`'s returned `status.user.username` names the token's actual namespace of origin. The gateway uses *that* namespace for all authorization decisions, not any namespace the caller claims in the request body. A valid token from namespace A can only be used to act as namespace A. |
| Stolen `kubernetes.default.svc`-audience token reused against the gateway | Gateway's `TokenReview` request specifies audience `agentry-gateway`. Tokens minted for a different audience fail validation. Workloads must explicitly project a gateway-audience token. |
| Agentry-managed Agent/AgentTask Pod tries to downgrade auth by using its ServiceAccount token instead of mTLS | Rejected by the gateway's **Pod-ownership precheck** on the bearer-token path, before any `TokenReview` call. See note below. |
| Agent created in `agentry-system` acquires a certificate whose SAN collides with an internal Service identity (e.g., an Agent named `agentry-gateway` would be issued SAN `agentry-gateway.agentry-system.svc.cluster.local`, exactly what activator / activity / channel-health / agent-side `/v1/message` authorization trusts) | The reconcilers refuse to provision Agent, AgentTask, or AgentChannel resources in `agentry-system` (`Ready=False, reason=SystemNamespaceForbidden`, [rule 28](../resources/validation-and-defaulting.md#cross-resource-validation)). See note below. |

### Notes

**Agent spoofs namespace (mTLS tier).** Namespace is extracted from the cert SAN, signed by `agentry-ca`. Agents use `{name}.{namespace}.svc.cluster.local`; AgentTasks use `{name}.{namespace}.task.agentry.io`. An agent cannot forge a cert for a different namespace (the CA key is not reachable from any agent Pod).

Two additional defenses specifically prevent a dotted-name label-shift bypass, where a name containing dots would shift the SAN's labels and make the parser read the wrong position as the namespace:

1. Agent and AgentTask `metadata.name` are restricted to DNS-1123 **label** form (no dots) by CRD CEL. See [Cross-Resource Validation rule 21](../resources/validation-and-defaulting.md#cross-resource-validation).
2. The gateway's SAN parser requires the exact label count for each shape (5 for Service-DNS, 4 for `.task.agentry.io`) and rejects any cert whose SAN has extra labels. See [Namespace Identification § Mode 1](../gateways/llm/workload-identity.md#mode-1-mtls-client-certificate).

Source-IP to Pod cross-check validates the cert identity against the actual source Pod; all checks must agree.

**Auth downgrade to ServiceAccount token.** Before any `TokenReview` call, the gateway resolves the request's source IP to a Pod via its informer cache and returns `401 Unauthorized` if that Pod has an `ownerRef` to an `Agent` or `AgentTask` resource or carries the Agentry-managed label set. The check runs on every request (it is not cached) and runs before `TokenReview`, so it is unaffected by token-cache hits or apiserver latency. The gateway also attempts mTLS first when a client cert is presented; if both auth materials are present the bearer header is ignored.

mTLS remains the only accepted auth mode for that tier, keeping its credential surface to one bounded-lifetime, namespace-pinned artifact. Rotating a leaf cert contains exposure but does not revoke the old one, which is why a single credential surface matters here (see [§ In-cluster TLS](tls.md#in-cluster-tls) for the containment-versus-revocation distinction and the CA re-key runbook). See [Namespace Identification, Mode 2](../gateways/llm/workload-identity.md#mode-2-serviceaccount-bearer-token).

**SAN collision in `agentry-system`.** The per-Agent SAN shape is distinguishable from the internal-endpoint SANs only by its namespace label, so the guard makes the collision unreachable even for admins; locking down `agentry-system` ([Recommendation 1](model.md#recommendations-for-deployment)) is the outer layer.

## Internal Endpoint Abuse

Certificates signed by `agentry-ca` are not interchangeable. Authorization on internal endpoints is by SAN, not by mere possession of a CA-signed cert, which is what keeps a compromised agent from reaching them with its own valid cert.

| Threat | Mitigation |
|---|---|
| Unauthorized agent wake-up via activator endpoint | The activator endpoint requires an mTLS client cert. The controller authorizes only clients whose cert SAN matches the gateway Service DNS: any other SAN, even one signed by `agentry-ca`, is rejected with `403 Forbidden`. Because the per-Agent cert SAN shape (`{name}.{namespace}.svc.cluster.local`) cannot match the gateway SAN, a compromised agent cannot use its own cert to trigger wake-ups. See [§ Internal Endpoint Authentication](rbac.md#internal-endpoint-authentication). |
| Compromised in-cluster Pod with network reach to an agent's Service forges channel messages | The agent's `POST /v1/message` listener requires a client cert with the gateway SAN. NetworkPolicy is the first layer; the agent-side per-path mTLS check is the second. See note below. |

### Notes

**Forged channel messages from a compromised Pod.** The agent's `POST /v1/message` listener requires a client certificate whose SAN matches the gateway Service DNS (`agentry-gateway.agentry-system.svc.cluster.local` / `.svc`). A compromised non-gateway Pod cannot present such a cert (the `agentry-ca` private key is not reachable from any non-gateway Pod), so even if it bypasses or piggybacks on a misconfigured per-Agent NetworkPolicy, the request is rejected at the handler: the agent's listener accepts the handshake without a client cert (`VerifyClientCertIfGiven`, so kubelet probes on the shared port keep working) and the `/v1/message` handler then returns `401` for a missing cert or `403` for a non-gateway SAN. NetworkPolicy is the first layer; the agent-side per-path mTLS check is the second. See [The Runtime Contract](../runtime/contract.md) bullet 4 and [§ In-cluster TLS](tls.md#in-cluster-tls).

## Transport and PKI Dependencies

cert-manager and trust-manager are cluster-critical dependencies. Both fail fast at install; both degrade gracefully at runtime, in the sense that running workloads keep working while new provisioning stalls.

| Threat | Mitigation |
|---|---|
| In-cluster traffic sniffed on shared nodes | All agent/gateway and gateway/controller traffic is TLS-encrypted. Certificates are cert-manager-managed and rooted at `agentry-ca`. See [§ In-cluster TLS](tls.md#in-cluster-tls). |
| cert-manager not installed or unhealthy | Chart install fails fast if `agentry-ca-issuer` cannot be created. Runtime degradation delays new provisioning and blocks rotation; running agents continue. See note below. |
| trust-manager not installed or unhealthy | Chart install fails fast if the `Bundle` resource cannot be created. Runtime degradation blocks CA distribution to new namespaces. See note below. |

### Notes

**cert-manager unhealthy.** Chart install fails fast if `agentry-ca-issuer` cannot be created; a mismatched `certManager.clusterResourceNamespace` (the ClusterIssuer resolves the CA Secret only there) surfaces as the issuer stuck `Ready=False, reason=SecretNotFound`. Runtime degradation of cert-manager delays new Agent/AgentTask provisioning (the `Certificate` Secret is not populated) and blocks cert rotation, but running agents continue until their current certs approach expiry. Operators should monitor cert-manager health as a cluster-critical dependency.

**trust-manager unhealthy.** Chart install fails fast if the `Bundle` resource cannot be created. Runtime degradation prevents the Agentry CA ConfigMap from appearing in new namespaces, so Pods scheduled into those namespaces fail to mount `/var/run/agentry/ca.crt` and cannot verify the gateway's TLS cert. Existing namespaces with the ConfigMap already projected are unaffected until the next CA rotation. Monitor trust-manager alongside cert-manager.

---

Continue to [Recommendations for Deployment](model.md#recommendations-for-deployment) for the operational posture that backs several of the rows above.
