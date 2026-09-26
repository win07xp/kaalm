# Threat model

This page lists the threats Kaalm defends against, the threats it accepts, and the threats it leaves to the platform team. Each threat is one table row with the mitigation and a decision: **mitigated**, **accepted** (a bound Kaalm chooses to live with), **out of scope** (outside the trust boundary), or **as shipped** (the design says one thing and the code does another; an issue tracks it). A row whose argument needs more than a sentence links to a note after its table.

Read [Trust model](model.md#trust-model) first: several rows are out of scope only because of where the trust boundary sits.

## Credential containment and egress

Provider credentials stay in `kaalm-system`, and agent Pods reach providers only through the gateway.

| Threat | Mitigation | Decision |
|---|---|---|
| A Kaalm-managed agent container calls an LLM provider directly | The synthesized NetworkPolicy denies all egress except the gateway and DNS ([Network policy](model.md#network-policy)), on any CNI that enforces NetworkPolicy. A class `allowedCIDRs` entry reopens exactly that CIDR. | Mitigated |
| A gateway-only-tier workload calls providers with its own keys | No Agent resource, so no synthesized policy. See [Gateway-only-tier workload calls providers directly](#gateway-only-tier-workload-calls-providers-directly). | Out of scope |
| A developer writes a permissive NetworkPolicy that widens agent egress | NetworkPolicy is additive. See [Developer authors a permissive NetworkPolicy](#developer-authors-a-permissive-networkpolicy). | Out of scope |
| A developer embeds provider credentials in the image | Image scanning and registry controls, outside Kaalm. | Out of scope |
| A provider key or tool credential leaves `kaalm-system` on the controller's health probe | The probe sends the credential to the configured provider or tool endpoint, the same host the gateway sends it to ([Liveness probe](../controller/reconcilers.md#liveness-probe)). | Accepted |

#### Gateway-only-tier workload calls providers directly

Gateway-only-tier workloads are existing Deployments the platform team has granted gateway access through `TokenReview`. They have no Agent resource and so no synthesized NetworkPolicy, and routing through the gateway is voluntary: a workload holding its own credentials can bypass the gateway. If you adopt this tier and need gateway routing enforced, apply a default-deny egress NetworkPolicy on those namespaces yourself. The Agent lifecycle tier is the only path with automatic enforcement.

#### Developer authors a permissive NetworkPolicy

The trust model places the developer in a trusted tier, so opting out of a guardrail is the developer's choice. NetworkPolicy is additive: a permissive policy unions with Kaalm's, and synthesis alone cannot prevent that. A platform team that treats developers as untrusted restricts `networkpolicies` create and patch in user namespaces with cluster RBAC. The agent container, the untrusted actor, cannot author a NetworkPolicy: its ServiceAccount has no RBAC ([Agent Pod ServiceAccount](rbac.md#agent-pod-serviceaccount)).

## Workload isolation

Agent containers execute LLM-generated code and are the untrusted actor inside an otherwise trusted namespace.

| Threat | Mitigation | Decision |
|---|---|---|
| A developer deploys an agent that exhausts node resources | `spec.resources.maxLimits` on the class clamps limits and requests; the image allowlist keeps arbitrary images out ([Resource isolation](model.md#resource-isolation)). A class that sets no `maxLimits` and no defaults leaves the Pod unlimited. | Mitigated when the class sets `maxLimits` |
| Generated code attempts a container escape | The class RuntimeClass (gVisor, Kata) isolates the kernel; the class security contexts, when declared, prevent privilege escalation ([RuntimeClass](model.md#runtimeclass)). As shipped the controller applies no security defaults of its own. | Mitigated when the class declares them |
| A namespace member with ConfigMap write injects code into a reviewed base image through a handler mount | Off by default: [rule 30](../resources/validation-and-defaulting.md#cross-resource-validation) requires `allowHandlerMounts: true` on the class. See [What granting allowHandlerMounts means](#what-granting-allowhandlermounts-means). | Mitigated |
| Generated code in one agent reaches another agent in the same namespace | Ingress is denied by default. With `allowSameNamespaceIngress: true`, the namespace's other agent Pods reach the agent on its health port, where the [mTLS check on `POST /v1/message`](../runtime/contract.md) is a second layer. | Mitigated by default; layered when opted in |

#### What granting allowHandlerMounts means

Granting the gate makes ConfigMap write access in a namespace equivalent to code execution as that namespace's handler-mounting Agents, with their ServiceAccount, certificate identity, and gateway access, effective at the next Pod recreation. Grant it on dedicated classes whose namespaces treat every ConfigMap author as a code author. Injected code runs under the same containment as reviewed code: the class security context, a RoleBinding-less ServiceAccount, the synthesized NetworkPolicy, and the optional RuntimeClass. A cross-namespace reference is unrepresentable because `configMapRef` is a local reference ([AgentClass](../resources/agentclass.md#spec), rules 30 and 31).

## Credential storage and rotation

| Threat | Mitigation | Decision |
|---|---|---|
| Platform credentials leak through an etcd backup | Encrypt etcd at rest, outside Kaalm. | Out of scope |
| Stale credentials after rotation fail silently | The gateway follows every Secret it uses through a watch; the ModelProviderReconciler re-reads the key and probes the provider on every pass ([Lifecycle of an LLM API key](credentials.md#lifecycle-of-an-llm-api-key)). | Mitigated |
| A channel credential leaks from an agent namespace | The Secret lives in that namespace, so the exposure is that namespace's channels. The platform team rotates it. | Accepted |

## Component compromise

These rows ask what an attacker gains by taking over a Kaalm component. For Secrets the answer is everything the component can read: `kaalm-system` plus the channel Secrets each AgentChannel names for the gateway, and the whole cluster for the operator.

| Threat | Mitigation | Decision |
|---|---|---|
| A compromised operator reads credential Secrets | The standing read is `kaalm-system`. In user namespaces the operator reads only named Secrets under Roles it mints, and holds none in memory. See [Compromised operator reads credential Secrets](#compromised-operator-reads-credential-secrets). | Accepted |
| A compromised gateway reads credential Secrets | The standing read is `kaalm-system`; in user namespaces it reads only the Secrets each AgentChannel names ([Summary of the gateway's reach](rbac.md#summary-of-the-gateways-reach)). Sign and verify the gateway image and restrict who can update its Deployment. | Accepted |
| A compromised gateway writes ConfigMaps into user namespaces | No `create` on user-namespace ConfigMaps; `update, patch` only on each task's pre-created completion ConfigMap ([Dynamic per-namespace grants: task completion ConfigMaps](rbac.md#dynamic-per-namespace-grants-task-completion-configmaps)). | Mitigated |
| A hostile or oversized body reaches the gateway's parsers | Every listener bounds its reads before parsing and fails closed on a body it cannot decode. See [Hostile bodies and the parsers](#hostile-bodies-and-the-parsers). | Mitigated |
| A compromised console reads credentials or cluster state | The console ServiceAccount reads every Kaalm resource and the Namespace list cluster-wide, creates `TokenReview` and `SubjectAccessReview`, and holds no Secret access. See [What a compromised console gains](#what-a-compromised-console-gains). | Accepted |

#### Compromised operator reads credential Secrets

The operator's standing Secret read is `kaalm-system` ([Operator ServiceAccount](rbac.md#operator-serviceaccount)), so a compromised operator reads every provider key and tool credential there, and its process holds them in the manager's Secret informer. In user namespaces it holds no standing read and keeps no Secret in memory: it reads a channel's credentials and a class's pull Secrets by name, from the API server, under Roles scoped to those names. The residual is the `escalate` and `bind` verbs those Roles require. A compromised operator can mint itself a Role over any Secret, which it could also reach by creating a Pod that mounts the Secret, since it creates Pods in every namespace. Either route is an API write that the audit log records, not a silent read. The operator never writes or copies a Secret. Mitigate as for the gateway: image signing, restricted Deployment update rights, and audit logging on Secret access and on Role creation by the operator's ServiceAccount.

#### Hostile bodies and the parsers

Every listener bounds its reads before parsing: the LLM proxy answers `413` above its cap, and the message, MCP, and platform paths carry their own caps ([Helm chart contents](../operations/deployment.md#helm-chart-contents) lists the values). Bodies decode with the standard library into typed structures, and an unparseable body fails closed with `400` before any provider or agent is contacted. The cross-format translator adds no parsing trust: it rewrites between two structures the proxy has already decoded, and a request it cannot express in the target format makes that candidate ineligible rather than producing a partial translation. Prompt bodies are never logged in the default build ([PII safety](../operations/observability.md#pii-safety)).

#### What a compromised console gains

The gateway authorizes the console SAN on `POST /v1/test-chat` and `GET /v1/spend`, so a compromised console can wake agents, deliver messages to them and spend their namespaces' budgets, and read spend figures. Test-chat deliveries carry a `/console/{namespace}/{agentName}` channel origin, so they are distinguishable in the gateway's delivery log. Through its own ServiceAccount the console reads the spec and status of every Kaalm resource in every namespace. The component is stateless, and the mitigation matches the other `kaalm-system` components: restrict and audit the namespace, and leave the console disabled where it is not used.

## Tenant isolation and budgets

Budgets are guardrails by default, not hard caps; a provider that needs a cap opts in to [hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement), which states its guarantee and its bounds.

| Threat | Mitigation | Decision |
|---|---|---|
| One tenant exhausts another tenant's budget through a shared provider | Per-namespace spend accounting; `allowedNamespaces` restricts access; soft limits have bounded overspend, hard limits cap it. | Mitigated within the stated bounds |
| An agent requests a provider or model it is not allowed | The gateway checks `allowedNamespaces`, the model catalog, and the class `allowedProviders`; every fallback candidate is re-checked against its own namespaces and catalog ([rules 12 and 41](../resources/validation-and-defaulting.md#cross-resource-validation)). | Mitigated |
| Budget guardrails are exceeded under concurrency | Soft mode documents its bounded overspend; hard enforcement bounds the crossing to its in-flight guarantee; provider account limits remain defense in depth. | Accepted |
| A gateway-only tenant uses a provider its AgentClass would have denied | No Agent resource, so no class to consult. See [Gateway-only tenant uses a provider AgentClass would have denied](#gateway-only-tenant-uses-a-provider-agentclass-would-have-denied). | Out of scope |
| A denied caller probes which providers and models exist | Authorization ordering protects the model catalog; provider existence is distinguishable. See [What a denied caller learns](#what-a-denied-caller-learns). | Accepted |
| Fallback routes a request to a provider outside the class `allowedProviders` | A fallback edge is the platform team's routing decision. See [Fallback edges and allowedProviders](#fallback-edges-and-allowedproviders). | Accepted |
| Upstream error text leaks platform detail to a caller | Only non-fallbackable 4xx answers relay verbatim; everything else reduces to a classified envelope. See [What flows back from a failed provider call](#what-flows-back-from-a-failed-provider-call). | Mitigated |
| A Pod with network reach scrapes the gateway metrics port | The port is unauthenticated and labels requests, tokens, and spend by provider, model, and namespace. The chart's `kaalm-system` NetworkPolicy admits the metrics ports only from the `networkPolicy.metricsFrom` peers, and every other port only from its callers ([The operator's own NetworkPolicy](../operations/deployment.md#the-operators-own-networkpolicy)). Inert on a CNI without enforcement, as every NetworkPolicy is. | Mitigated |

#### Gateway-only tenant uses a provider AgentClass would have denied

Gateway-only workloads have no Agent resource and so no `allowedProviders` to consult; access control reduces to `ModelProvider.spec.allowedNamespaces` plus `spec.models`. A platform team that needs class-scoped provider policy onboards the workload through the Agent lifecycle tier ([Gateway-only tier](../gateways/llm/provider-routing.md#gateway-only-tier-tokenreview)).

#### What a denied caller learns

`allowedNamespaces` is checked before model existence, so a namespace that may not use a provider cannot learn which models it hosts. Provider existence is the accepted remainder: an unknown provider answers `400 invalid_request` and a denied one `403 access_denied`, so a caller can tell whether a provider name exists. Provider names are cluster-scoped identifiers on the same footing as namespace names. The LLM proxy and the MCP broker share this ordering.

#### Fallback edges and allowedProviders

A fallback candidate is re-checked against its own `allowedNamespaces` and catalog, but not against the workload's class `allowedProviders`. The workload gates govern what a caller may request; a fallback edge is declared by the platform team on the provider they own ([Fallback logic](../gateways/llm/fallback.md)). A platform team that does not want traffic reaching a provider through fallback does not declare the edge.

#### What flows back from a failed provider call

Only a non-fallbackable provider answer (`400`, `422`, and the other 4xx statuses except `401`, `403`, and `429`) relays to the caller verbatim, translated when the serving candidate was cross-format. Statuses that could echo credential material (`401`, `403`) and the retryable classes (`429`, 5xx, transport errors) never relay: the walk continues past them, and exhaustion answers with a classified envelope. Transport error text reduces to a failure class before it can carry an upstream address.

## Tool plane

Tool credentials follow the same containment as provider keys, and the broker gates every call before it reads one ([The broker](../gateways/tool-plane.md#the-broker)).

| Threat | Mitigation | Decision |
|---|---|---|
| An agent calls a ToolProvider it was not granted | The broker checks the provider's `allowedNamespaces`, the class catalog, and the agent's `spec.tools` grant before any upstream call ([Grants](../gateways/tool-plane.md#grants)). | Mitigated |
| A tool credential reaches an agent container | The credential is injected on the broker's upstream leg only, and a denied call ends before the credential is read ([Lifecycle of a tool server credential](credentials.md#lifecycle-of-a-tool-server-credential)). | Mitigated |
| A tool call or response exhausts the gateway or reaches an internal host | Body caps on both directions and the SSRF checks on the upstream endpoint ([Limits and SSRF protection](../gateways/tool-plane.md#limits-and-ssrf-protection)). | Mitigated |
| One workload uses another's MCP session | Session ids are bound to the workload identity that opened them ([Session ownership](../gateways/tool-plane.md#session-ownership-legacy-revisions)). | Mitigated |

## Channels and webhooks

Inbound webhooks come from third parties with their own signing conventions, and outbound callbacks leave the gateway, which has wider egress than any user namespace.

| Threat | Mitigation | Decision |
|---|---|---|
| A forged message arrives from a channel platform | The webhook adapter verifies the bearer token or HMAC before processing ([Inbound webhook auth](../gateways/api/channel-webhook.md)). | Mitigated |
| A captured inbound webhook is replayed | Inbound HMAC is body-only with no timestamp. See [Captured inbound webhook is replayed](#captured-inbound-webhook-is-replayed). | Accepted |
| A forged Discord interaction or WhatsApp event | The adapters verify the platform's own signature first ([Inbound](../gateways/user/platform-adapters.md#inbound)). Discord's signed timestamp bounds replay to 300 seconds; WhatsApp's signature is body-only, as for the generic webhook. | Mitigated; replay accepted on WhatsApp |
| A tenant points a platform reply at a host of their choosing, carrying the channel's token | Not expressible: platform base URLs are Helm values, not channel fields. See [Why the platform reply path has no deny ranges](#why-the-platform-reply-path-has-no-deny-ranges). | Mitigated |
| A caller with channel A's credentials fetches channel B's async response | The poll endpoint asserts the stored response's channel labels match the authenticated channel and answers `404` otherwise ([Channel-match assertion](../gateways/api/async-responses.md#channel-match-assertion)). See [Cross-channel async response fetch](#cross-channel-async-response-fetch). | Mitigated |
| A developer uses `callbackUrl` for SSRF against internal services or the cloud metadata IP | `https://` and the deny ranges of [rule 22](../resources/validation-and-defaulting.md#cross-resource-validation) at admission, re-checked and IP-pinned on every delivery. See [SSRF through callbackUrl](#ssrf-through-callbackurl). | Mitigated |
| A third party forges a callback POST to a developer's `callbackUrl` | Every callback POST is signed with `callbackAuth`, which [rule 25](../resources/validation-and-defaulting.md#cross-resource-validation) requires whenever `callbackUrl` is set ([Callback authentication](../gateways/api/async-responses.md)). | Mitigated |

#### Captured inbound webhook is replayed

Inbound HMAC is body-only with no timestamp, the cost of accepting third-party senders that follow their own conventions, such as GitHub-style body-only HMAC. The gateway cannot reject a replayed `(body, HMAC)` pair until the secret is rotated. Cost replay is bounded by per-namespace budgets ([Multi-tenancy](../concepts/tenancy-and-tiers.md#multi-tenancy)). Side-effect replay is the agent's responsibility: an agent performing non-idempotent inbound actions must deduplicate on a caller-supplied idempotency key or a content hash. The gateway's `messageId` deduplication ([The runtime contract](../runtime/contract.md), item 7) covers gateway retries only, not external replay.

#### Why the platform reply path has no deny ranges

The SSRF defenses on `callbackUrl` exist because the URL is developer-supplied. A platform base URL (`gateway.platforms.<type>.apiBaseUrl`) is operator-supplied at install time, in the same trust tier as a ModelProvider endpoint, and defending the gateway against its own operator is out of scope. The value accepts `http://` for the same reason: the e2e mock platforms stand in at install time.

#### Cross-channel async response fetch

The poll endpoint authenticates the caller against the AgentChannel named by `channelPath`, then requires the stored response's `kaalm.io/channel-namespace` and `kaalm.io/channel-name` labels to match that channel. `requestId` values are UUIDs but not secrets; the label check is the isolation. A mismatch answers `404`, indistinguishable from an unknown `requestId`, so a caller cannot confirm that a cross-channel response exists; the gateway logs it with `reason=ChannelMismatch`.

#### SSRF through callbackUrl

The gateway has wider egress than any user namespace, so an unrestricted `callbackUrl` would make it a confused deputy. Rule 22 rejects a URL at admission unless it is `https://` and its host resolves outside the deny ranges. On every delivery the gateway re-resolves the host, re-applies the check, and dials the exact IP that passed while preserving the Host header and SNI ([Request flow](../gateways/user/overview.md#request-flow), step 8); handing the hostname back to the transport would reopen the DNS-rebinding window. A pooled connection is reused only after the check passes again for that attempt. The Helm value `gateway.callbackUrl.allowlist` replaces the deny-internal default with an explicit allowlist, and loopback, link-local, and unspecified addresses stay denied under any allowlist.

## Identity and namespace spoofing

Every authorization decision keys off a namespace, so forging one is the highest-value attack. Both modes attest identity cryptographically and cross-check it against the source Pod ([Workload identity](../gateways/llm/workload-identity.md)).

| Threat | Mitigation | Decision |
|---|---|---|
| An agent spoofs its namespace in the mTLS tier | The namespace comes from the SAN of a `kaalm-ca`-signed certificate; the CA key is unreachable from any workload Pod. See [Agent spoofs namespace (mTLS tier)](#agent-spoofs-namespace-mtls-tier). | Mitigated |
| A gateway-only workload spoofs its namespace | The namespace comes from the `TokenReview` username, which the API server signs; the source-IP cross-check must agree. | Mitigated |
| A gateway-only tenant presents a token from another namespace | The token names its own namespace, and the gateway uses that for every decision. A token from namespace A acts only as namespace A. | Mitigated |
| A stolen `kubernetes.default.svc`-audience token is reused against the gateway | The `TokenReview` names audience `kaalm-gateway`, so a token for another audience fails. | Mitigated |
| A Kaalm-managed Pod downgrades to its ServiceAccount token | The bearer path's Pod-ownership precheck answers `401` before any `TokenReview`. See [Auth downgrade to ServiceAccount token](#auth-downgrade-to-serviceaccount-token). | Mitigated |
| An Agent created in `kaalm-system` gets a SAN that collides with an internal Service identity | The reconcilers refuse Agent, AgentTask, and AgentChannel resources in `kaalm-system` (`Ready=False, reason=SystemNamespaceForbidden`, [rule 28](../resources/validation-and-defaulting.md#cross-resource-validation)). See [SAN collision in kaalm-system](#san-collision-in-kaalm-system). | Mitigated |

#### Agent spoofs namespace (mTLS tier)

The namespace is read from the SAN, whose forms and label counts [Mode 1](../gateways/llm/workload-identity.md#mode-1-mtls-client-certificate) specifies. Two defenses close the dotted-name label shift, where a name containing dots would move the namespace to a different label position: Agent and AgentTask names are restricted to DNS-1123 label form ([rule 21](../resources/validation-and-defaulting.md#cross-resource-validation)), and the parser requires the exact label count for each form. The source-IP cross-check then requires the certificate's namespace to match the source Pod's.

#### Auth downgrade to ServiceAccount token

Before any `TokenReview`, the gateway resolves the request's source IP to a Pod and answers `401` if that Pod has an ownerRef to an Agent or AgentTask or carries the Kaalm-managed label. The check is not cached and runs before the token cache, so it is unaffected by cache hits or API server latency. When both a client certificate and a bearer header are present, the certificate wins and the header is ignored. By default the Pod also sets `automountServiceAccountToken: false`, so it holds no token to present; a class that opts in to API access mounts one, and the precheck still rejects it here ([Agent Pod ServiceAccount](rbac.md#agent-pod-serviceaccount)). Keeping the tier's credential surface to one artifact matters because leaf rotation contains a leak but does not revoke it ([Containment, not revocation](tls.md#containment-not-revocation)).

#### SAN collision in kaalm-system

An Agent named `kaalm-gateway` in `kaalm-system` would be issued the SAN `kaalm-gateway.kaalm-system.svc.cluster.local`, the identity the activator, the internal endpoints, and the agent-side `/v1/message` check trust. The per-Agent SAN form differs from the internal-endpoint SANs only by its namespace label, so the guard makes the collision unreachable even for administrators; locking down `kaalm-system` ([Recommendations for deployment](model.md#recommendations-for-deployment)) is the outer layer.

## Internal endpoint abuse

Certificates signed by `kaalm-ca` are not interchangeable: internal endpoints authorize by SAN ([Internal endpoint authentication](rbac.md#internal-endpoint-authentication)).

| Threat | Mitigation | Decision |
|---|---|---|
| An unauthorized caller wakes agents through the activator | `/v1/activate` requires a client certificate with the gateway Service SAN; any other SAN, even one signed by `kaalm-ca`, answers `403`. | Mitigated |
| A compromised Pod with network reach to an agent's Service forges channel messages | The agent's `POST /v1/message` requires a client certificate with the gateway SAN. See [Forged channel messages from a compromised Pod](#forged-channel-messages-from-a-compromised-pod). | Mitigated |
| An in-cluster caller POSTs `ConversionReview` payloads to the cert-less conversion listener | Conversion is a pure function with nothing to extract, and bodies are capped. See [The conversion listener's exposure](#the-conversion-listeners-exposure). | Accepted |

#### Forged channel messages from a compromised Pod

The agent's `POST /v1/message` handler requires a client certificate whose SAN is the gateway Service DNS, `kaalm-gateway.{operatorNamespace}.svc.cluster.local` or `.svc`. A non-gateway Pod cannot present one, because the CA key is unreachable from it, so even a Pod that bypasses a misconfigured per-Agent NetworkPolicy is rejected at the handler: `401` without a certificate, `403` with the wrong SAN. The listener accepts the handshake without a client certificate so that kubelet probes on the shared port keep working, and enforces per path ([The runtime contract](../runtime/contract.md), item 4).

#### The conversion listener's exposure

The conversion listener on `:9444` requires no client authentication because its caller, the API server, presents no certificate. An unauthorized caller gains nothing: the handler decodes a `ConversionReview`, converts between `kaalm.io` versions, and echoes the result, reading no cluster state and holding no credentials. The remaining lever is resource exhaustion, and bodies are capped far above any real review. The chart ships no NetworkPolicy for `kaalm-system`, so restricting who can reach `:9444` and the two metrics ports is a policy you write ([Recommendations for deployment](model.md#recommendations-for-deployment)).

## The console

The [console](../console/overview.md) is optional and off by default. Its callers are people with browser sessions.

| Threat | Mitigation | Decision |
|---|---|---|
| A console session cookie is stolen or replayed | The cookie is `Secure`, `HttpOnly`, `SameSite=Strict`; sessions live in console memory and expire with the pasted token or after 24 hours; a restart invalidates every session ([Console](../console/overview.md)). | Accepted within those bounds |
| A caller views namespaces their token does not grant | The console fixes the identity at login with `TokenReview` and gates every namespace read with a `SubjectAccessReview` ([S19](../appendix/scenarios.md#s19-see-the-fleet-without-kubectl)). | Mitigated |
| Cross-site request forgery against test-chat | The `SameSite=Strict` cookie is the control. See [Why SameSite is the CSRF control](#why-samesite-is-the-csrf-control). | Mitigated |

#### Why SameSite is the CSRF control

Browsers do not attach a `SameSite=Strict` cookie to any cross-site request, so a hostile page cannot use an operator's session to reach test-chat. API callers authenticate per request with a bearer header, which a cross-site page cannot set. The login POST carries no session yet, and a forged login would bind the attacker's own token. The console therefore carries no separate CSRF token.

## Transport and PKI dependencies

cert-manager and trust-manager are cluster-critical dependencies. Both fail fast at install, and at runtime running workloads keep working while new provisioning stalls ([Dependency failure modes](tls.md#dependency-failure-modes)).

| Threat | Mitigation | Decision |
|---|---|---|
| In-cluster traffic is sniffed on shared nodes | Every hop that carries agent, user, or tool traffic is TLS rooted at `kaalm-ca` ([In-cluster TLS](tls.md#in-cluster-tls)). The two metrics ports are plain HTTP. | Mitigated |
| cert-manager is unhealthy | Install fails if `kaalm-ca-issuer` cannot be created; at runtime new provisioning holds and rotation stops. | Accepted |
| trust-manager is unhealthy | Install fails if the `Bundle` cannot be created; at runtime new namespaces get no CA ConfigMap. | Accepted |
| A leaked leaf certificate is used after rotation | Rotation contains, it does not revoke; the leaf stays valid to its `notAfter` ([Containment, not revocation](tls.md#containment-not-revocation)). The CA re-key runbook is the only invalidation. | Accepted |
