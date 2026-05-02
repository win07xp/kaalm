# Agentry — Security Model

This document defines Agentry's security posture: the RBAC model, credential scoping, isolation guarantees, and the trust boundaries between platform engineers, developers, agent containers, and the cluster itself. It is written to be the answer sheet when a security team asks "what can go wrong here?"

## Trust Model

Agentry assumes four trust tiers:

1. **Cluster administrator** — trusted to install Agentry, manage CRDs, and deploy the operator.
2. **Platform engineer** — trusted to create AgentClasses and ModelProviders, and to manage LLM credentials. This role should be distinct from agent developers.
3. **Agent developer** — trusted to deploy workloads in their namespace within the guardrails set by the platform team. Not trusted with LLM credentials or cross-namespace access.
4. **Agent container** — **not trusted**. Even developer-authored agents may execute LLM-generated code. Agent containers should be treated as potentially adversarial.

Agentry's security design flows from the assumption that agent containers are untrusted.

---

## RBAC Model

### Operator ServiceAccount

The operator runs under a ServiceAccount (`agentry-system/agentry-controller`) with a ClusterRole granting:

- Full access (`get, list, watch, create, update, patch, delete`) to all Agentry CRDs.
- `get, list, watch, create, update, patch, delete` on `Pods`, `PersistentVolumeClaims`, `Services`, `ConfigMaps`, `NetworkPolicies`, `ServiceAccounts` cluster-wide. `NetworkPolicy` and `ServiceAccount` are owned per-Agent and per-AgentTask — the reconcilers synthesize one of each during provisioning and owner-reference them to the parent resource for cascade GC, so the operator must be able to create and delete them in user namespaces. `list` and `watch` are required because controller-runtime drives owned-resource reconciliation through informers; they are also required cluster-wide on `Pods` because the AgentReconciler enumerates gateway Pods in `agentry-system` for the activity fan-out (see [CONTROLLER_RECONCILERS.md § AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) step 8 and [GATEWAY_USER.md § Multi-replica fan-out](./GATEWAY_USER.md#activity-tracking-api)).
- `get, list, watch` on `RuntimeClass`, `StorageClass`, `Namespaces` (for validation).
- `create, patch` on `Events` (for event emission).
- `get, list, watch, create, update, patch, delete` on `Leases` in `agentry-system` (controller-runtime's leader-election lock requires the full set; restricted to the operator's own namespace).
- `get, list, watch` on `Secrets` in `agentry-system` only (not cluster-wide) — the operator validates that ModelProvider credential Secrets exist. In addition, the AgentChannelReconciler creates **dynamic, per-AgentChannel Roles in user namespaces** that grant the operator ServiceAccount `get, list, watch` scoped via `resourceNames` to the single Secret referenced by that AgentChannel's active webhook auth config (`spec.webhook.auth.secretRef` for bearer; `spec.webhook.auth.hmac.secretRef` for HMAC). These Roles are owned by the AgentChannel and torn down on deletion the same way the gateway-side per-channel Roles are. This scoped read path is what the reconciler uses to verify that the configured `data` key is present in the Secret (see [AgentChannelReconciler step 3](./CONTROLLER_RECONCILERS.md#agentchannelreconciler)). The operator has no broader Secret read access in user namespaces.
- `get, list, watch, create, update, patch, delete` on `cert-manager.io/v1/Certificate` in user namespaces (the per-Agent and per-AgentTask certificates live alongside the workload, not in `agentry-system`). `list` and `watch` are required so the reconciler can observe `Certificate.status.conditions[type=Ready]` before creating the dependent Pod.

**Unlike a sidecar model, the operator does not need cluster-wide Secret read/write access.** Credentials are held by the gateway ServiceAccount in `agentry-system` and never copied to user namespaces. This significantly reduces the operator's blast radius.

### Gateway ServiceAccount permissions

The Agentry Gateway runs under a separate ServiceAccount (`agentry-system/agentry-gateway`) with:

- `get, watch` on `Secrets` in `agentry-system` (to read LLM provider credentials).
- `get, watch` on `ConfigMaps` in `agentry-system` (to receive budget configuration from the operator).
- **`create` on `tokenreviews.authentication.k8s.io`** (cluster-scoped, no resource name needed — `TokenReview` is a virtual resource). This permits the gateway to validate projected ServiceAccount bearer tokens presented by gateway-only-tier workloads. Without this, the gateway cannot accept non-mTLS authentication. See [LLM Gateway § Namespace Identification mode 2](./GATEWAY_LLM.md#mode-2--serviceaccount-bearer-token-gateway-only-tier).
- `get, list, watch` on `Agent` resources cluster-wide (for provider routing: the gateway resolves the calling Pod's `ownerRef` to its `Agent` on every LLM request and reads `spec.providers` to validate the qualified `provider/model` name — see [LLM Gateway § Provider Routing](./GATEWAY_LLM.md#provider-routing)). Hibernation is detected by Service-endpoint absence in the User Gateway path, not by reading `Agent.status` — see [GATEWAY_USER.md § Activator](./GATEWAY_USER.md#activator).
- `get, list, watch` on `AgentTask` resources cluster-wide (for task completion handling: the gateway resolves the calling Pod's ownerRef to identify the associated AgentTask, validates that declared artifact names in the completion payload match `spec.artifacts`, and reads the AgentTask's UID to set the ownerRef on the completion ConfigMap it creates).
- `get, list, watch` on `AgentChannel` resources cluster-wide (to look up which Agent a channel message targets and to manage platform connections).
- `patch` on `AgentChannel` resources cluster-wide (to write the `agentry.io/channel-disconnected` annotation during the finalizer handoff — see [Finalizers](./CONTROLLER_LIFECYCLE.md#finalizers)).
- `get, list, watch` on `ModelProvider` resources cluster-wide (for model validation, `allowedNamespaces` checks, budget configuration, and fallback chain resolution).
- `get, watch` on specific Secrets in user namespaces referenced by AgentChannel webhook auth config — both **inbound** auth (`spec.webhook.auth.secretRef` for bearer, `spec.webhook.auth.hmac.secretRef` for HMAC, used to verify inbound webhook signatures from channel platforms) and **outbound** callback auth (`spec.webhook.callbackAuth.secretRef` / `.hmac.secretRef`, used to sign outbound callback POSTs to `callbackUrl`; required by [API_RESOURCES.md cross-resource validation rule 25](./API_RESOURCES.md#cross-resource-validation) whenever `callbackUrl` is set). This is implemented via **dynamic per-namespace Roles**: when the AgentChannelReconciler creates or updates an AgentChannel, it ensures a Role and RoleBinding exist in the agent's namespace granting the gateway ServiceAccount `get, watch` `resourceNames`-scoped to the Secret(s) referenced by the active inbound and outbound auth types. When `auth` and `callbackAuth` reference the same Secret, the Role lists it once; when they differ, the Role lists both. The reconciler cleans up these Roles when the AgentChannel is deleted. The gateway does not have blanket Secret access across user namespaces.
- `get, list, watch` on `Pods` cluster-wide (to maintain the Pod informer cache used for source IP → namespace resolution on LLM requests).
- Per-task dynamic `Role` and `RoleBinding` for ConfigMap write access: when the AgentTaskReconciler provisions a task, it pre-creates an empty `{taskName}-completion` ConfigMap (owned by the AgentTask) and creates a `Role` in the task's namespace granting the gateway ServiceAccount `update, patch` (not `create`) on that exact ConfigMap name, along with a `RoleBinding` to `agentry-system/agentry-gateway`. Both Role and RoleBinding are owned by the AgentTask (via ownerRef) and cascade-deleted on task cleanup. The verb set deliberately omits `create` because Kubernetes RBAC's `resourceNames` does not constrain `create` requests — granting `create` on a named ConfigMap would silently widen the gateway's access to all ConfigMaps in the namespace. Pre-creating the resource and granting only `update, patch` makes the name-scoping enforceable. This follows the same scoped-access pattern used for channel credentials (see AgentChannel webhook auth config). The gateway does **not** have blanket ConfigMap write access across user namespaces.
- `get` on `Services` in user namespaces (to resolve Agent endpoints for message delivery).

The gateway's LLM credential access is scoped to `agentry-system`. Channel credential access extends to user namespaces via dynamic per-namespace Roles created by the AgentChannelReconciler — the gateway only reads the specific Secrets referenced by the AgentChannel's active webhook auth config, not arbitrary Secrets. These Roles are cleaned up when the AgentChannel is deleted. The `patch` permission on AgentChannel is narrowly used: the gateway writes only the `agentry.io/channel-disconnected` annotation as part of the finalizer handshake when a channel is deleted. ConfigMap write access in user namespaces follows the same scoped-Role pattern: the AgentTaskReconciler pre-creates the `{taskName}-completion` ConfigMap and grants the gateway `update, patch` (not `create`) on only that named ConfigMap in the task's namespace; both Role and RoleBinding are owned by the AgentTask and cascade-deleted. `create` is intentionally excluded because RBAC `resourceNames` does not constrain `create` and would otherwise broaden the scope to all ConfigMaps in the namespace. The gateway has no blanket ConfigMap access across user namespaces. Async webhook response ConfigMaps (`agentry-async-{requestId}`) are stored in `agentry-system` (where the gateway already has full ConfigMap access), not in agent namespaces — this is why no additional per-channel Role is needed for async response writes. The AgentChannelReconciler prunes these ConfigMaps in `agentry-system` using label selectors (`agentry.io/channel-namespace`, `agentry.io/channel-name`) rather than ownerRefs (cross-namespace ownerRefs do not trigger Kubernetes GC). Activity tracking does not require any etcd writes — the gateway maintains activity timestamps in-memory and serves them to the controller via the [activity tracking API](./GATEWAY_USER.md#activity-tracking-api).

### Platform Engineer role

A `ClusterRole` named `agentry-platform-admin` with:
- Full access to `AgentClass` and `ModelProvider`.
- `get, list, watch` on `Agent`, `AgentTask` cluster-wide (for observability).
- `create, get, update, delete` on Secrets in the operator's namespace (to manage LLM credentials).
- `create, get, update, delete` on Secrets in agent namespaces (to provision channel credentials referenced by AgentChannel webhook auth config).

Assigned via `ClusterRoleBinding` to users/groups who should manage platform-level configuration.

### Agent Developer role

A `Role` (namespaced) named `agentry-developer` with:
- Full access to `Agent`, `AgentTask`, and `AgentChannel` in their namespace.
- `get, list` on `AgentClass` and `ModelProvider` cluster-wide (read-only; developers need to know what's available to reference).
- `get` on Pods, PVCs, Services, ConfigMaps in their namespace.
- `get` on Events in their namespace.
- `create` on `pods/exec` for debugging (optional, platform team decides).

No access to Secrets, no ability to create/modify AgentClass or ModelProvider, no access to other namespaces.

### Agent Pod ServiceAccount

Each Agent Pod runs with a ServiceAccount **distinct** from both the operator and the developer. The AgentReconciler creates this ServiceAccount at provisioning time as an owned child resource (`ServiceAccount` named `agent-{agentName}`, in the Agent's namespace, owner-referenced to the Agent so cascade deletion cleans it up). No RoleBindings are attached by default, so the agent has no access to the Kubernetes API. The AgentTaskReconciler creates an analogous `task-{taskName}` ServiceAccount for AgentTask Pods with the same defaults.

If an agent needs cluster API access (e.g., a Kubernetes-administering agent), the developer or platform team must explicitly create a Role and RoleBinding against the per-agent ServiceAccount. This is opt-in, not default. See [AgentReconciler](./CONTROLLER_RECONCILERS.md#agentreconciler) step 6 and [AgentTaskReconciler](./CONTROLLER_RECONCILERS.md#agenttaskreconciler) step 4 for the convergence step.

### Agent→Gateway Authentication

The gateway supports **two authentication modes** for inbound requests, mapped to the two Helm tiers.

**Mode 1 — mTLS client certificate (Agentry-managed Pods).** This is the only authentication path available to Pods created by the AgentReconciler (Agent / AgentTask workloads). Agents present the cert-manager-issued certificate at `$AGENTRY_TLS_CERT` as a client cert on the LLM Gateway's TLS listener. The gateway verifies against `agentry-ca` and extracts (namespace, agent name) from the SAN (`{name}.{namespace}.svc.cluster.local`). Identity is cryptographically attested; the CA private key is not reachable from any agent Pod.

**Agent/AgentTask Pods must use mTLS — their ServiceAccount tokens are not accepted by the gateway.** This is a deliberate asymmetry: accepting SA tokens from Agentry-managed Pods would create two attack paths (cert path plus token path), and a compromised agent could use the token path to bypass cert revocation after rotation. Rejecting SA tokens from the cert-path tier forces cert rotation to remain the single revocation mechanism for that tier.

**Mode 2 — projected ServiceAccount bearer token via `TokenReview` (gateway-only tier).** For existing workloads in user namespaces that the platform team has granted LLM-provider access to without adopting the Agent CRD. **Before validating any token, the gateway runs a Pod-ownership precheck**: it resolves the request's source IP to a Pod via its informer cache and rejects the request with `401 Unauthorized` if the Pod has an `ownerRef` to an `Agent` or `AgentTask` resource. The mTLS tier is exclusive — Agentry-managed Pods cannot fall back to bearer-token auth — and this precheck is what enforces it at the gateway. Once the precheck passes, the caller mounts a projected ServiceAccount token with audience `agentry-gateway` and sends it as `Authorization: Bearer <token>`. The gateway POSTs the token to `authentication.k8s.io/v1/tokenreviews`, requires `status.authenticated: true`, and extracts the namespace from `status.user.username` (`system:serviceaccount:<ns>:<sa>`). Validation results are cached by the token's SHA-256 hash until `status.expirationTimestamp` minus a 60s safety margin; the Pod-ownership precheck is not cached and re-runs on every request.

Audience binding is critical: the gateway requests the `agentry-gateway` audience in its `TokenReview`, so a generic `kubernetes.default.svc`-audience token (such as a stolen kubelet token) cannot authenticate. Callers must configure their projected-volume `audience: agentry-gateway`.

**Secondary cross-check (both modes):** the gateway resolves the source IP to a Pod via its Pod informer and confirms the Pod is in the namespace identity resolution produced. This closes a stolen-credential scenario where a cert or token from one Pod is presented from a different Pod. Cross-check failure → request rejected.

See [GATEWAY_LLM.md § Namespace Identification](./GATEWAY_LLM.md#namespace-identification) for the full request-time flow.

**Client cert presentation:** starter templates (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)) configure mTLS client-cert presentation and the cert-file watch-and-reload pattern. Custom Agent/AgentTask images must present `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY` on every agent→gateway call (LLM requests, heartbeats, task completion). Activity timestamps and heartbeats are tracked in-memory in the gateway — no etcd writes are involved in agent→gateway communication.

### Internal Endpoint Authentication (Activator, Activity, Channel Health)

The controller's internal endpoint (`POST /v1/activate/{namespace}/{agentName}`) and the gateway's internal endpoints (`GET /v1/activity`, `GET /v1/channels/health`) authenticate callers **via mTLS with SAN-based authorization** — there is no separate shared-secret layer on top of TLS.

- **Activator** (gateway → controller): the controller's TLS listener on port 9443 is shared with kubelet's `/healthz` and `/readyz` probes, so it is configured with `ClientAuth: tls.VerifyClientCertIfGiven` — cert-less probes can complete the handshake, while per-path HTTP middleware enforces mTLS-with-SAN on `/v1/activate`. The gateway presents `agentry-gateway-tls` as its client cert; the controller verifies it against `agentry-ca` and authorizes the request only if the cert's SAN matches the gateway Service DNS (`agentry-gateway.agentry-system.svc.cluster.local` or `.svc`). Any other SAN is rejected as `403 Forbidden`. A request missing the cert entirely is rejected as `401 Unauthorized` at the handler.
- **Activity API and Channel Health** (controller → gateway): the gateway's TLS listener on port 8443 requires a client certificate on `/v1/activity` and `/v1/channels/health`. The controller presents `agentry-controller-tls`; the gateway verifies against `agentry-ca` and authorizes only if the SAN matches the controller Service DNS (`agentry-controller.agentry-system.svc.cluster.local` or `.svc`).

Both peer certificates are issued by cert-manager from `agentry-ca-issuer`, live only in `agentry-system`, and rotate continuously via cert-manager — there is no Secret-based shared key to rotate separately. The gateway cert declares `usages: [server auth, client auth]` (the gateway also dials the controller); the controller cert declares the same pair (the controller also dials the gateway activity API).

Authorization is by SAN, not by mere possession of a cert signed by `agentry-ca`. Per-Agent certs are signed by the same CA but have different SANs (`{name}.{namespace}.svc.cluster.local`), so a compromised agent cannot present its own cert to reach either internal endpoint — the authorization layer rejects the SAN before the handler runs.

Both `/v1/activity` and `/v1/channels/health` are served on the gateway's `:8443` listener with TLS configured as `ClientAuth: tls.VerifyClientCertIfGiven` (so token-auth callers on adjacent paths can complete the TLS handshake without a client cert — see [GATEWAY_LLM.md § Per-path client-auth enforcement](./GATEWAY_LLM.md#per-path-client-auth-enforcement)). The path handlers explicitly require a client cert with the controller SAN before invoking any business logic — a missing cert returns `401`, a non-matching SAN returns `403`. The TLS-handshake check alone is not sufficient, since the listener accepts cert-less connections for the gateway-only tier.

---

## Credential Handling

### Lifecycle of an LLM API key

1. **Stored**: in a Secret in `agentry-system` (e.g., `agentry-system/anthropic-api-key`), created and managed by platform engineers.
2. **Referenced**: by ModelProvider.spec.credentialsRef. Only the gateway ServiceAccount has read access to this Secret.
3. **Loaded**: the gateway reads the Secret at startup and on rotation events (Kubernetes watch). Credentials are held in the gateway process memory only.
4. **Used**: the gateway injects the API key into upstream requests on behalf of agent containers. Agent containers never have access to the credential — they do not have the Secret mounted and cannot reach `agentry-system` Secrets via the Kubernetes API.
5. **Rotated**: when the source Secret is updated, the gateway's Secret watch picks up the change and refreshes in-memory credentials without a restart.
6. **Never copied**: there are no per-agent or per-namespace copies of LLM credentials. The source Secret in `agentry-system` is the single authoritative location.

### Lifecycle of a channel credential (AgentChannel)

1. **Stored**: in a Secret in the agent's namespace (e.g., `team-support/discord-bot-credentials`), created by the platform team or a provisioning service.
2. **Referenced**: by the AgentChannel's webhook auth config — `spec.webhook.auth.secretRef` (inbound, bearer), `spec.webhook.auth.hmac.secretRef` (inbound, HMAC), and/or `spec.webhook.callbackAuth.secretRef` / `.hmac.secretRef` (outbound callback signing — required when `spec.webhook.callbackUrl` is set; see [rule 25](./API_RESOURCES.md#cross-resource-validation)). Future platform types (v1.1) will use a top-level `credentialsRef`.
3. **Loaded**: the gateway watches `AgentChannel` resources directly. When it sees a new or updated AgentChannel, it reads the referenced Secret(s) from the agent's namespace using its scoped RBAC and holds them in-process — inbound `auth` material for the webhook adapter's verifier, outbound `callbackAuth` material for the adapter's `SendReply` signer. The operator ServiceAccount also has a parallel scoped read path on the same Secret(s) via a dynamic per-channel Role (see [Operator ServiceAccount](#operator-serviceaccount) above), used solely by the AgentChannelReconciler to validate that the configured `data` key exists; the operator does not retain credential material in memory.
4. **Rotated**: same watch-based mechanism as LLM credentials — the gateway watches the referenced Secret for changes and refreshes in-memory credentials without a restart.

Channel credentials are namespace-scoped for organizational isolation — each namespace contains only the credentials for its own agents' channels. They are created by the platform team or a provisioning service; developers do not need Secret access in their namespace.

### Lifecycle of an agent TLS serving certificate

1. **Created**: by the AgentReconciler when provisioning the agent's Pod. The reconciler creates a cert-manager `Certificate` resource named `{agentName}-tls` in the Agent's namespace, owner-referenced to the Agent. Its `issuerRef` is `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`; SAN list covers `{name}.{namespace}.svc.cluster.local`, `{name}.{namespace}.svc`, `{name}.{namespace}`; usages are `server auth` and `client auth` (the same cert serves both directions).
2. **Stored**: cert-manager writes the output Secret (name = `Certificate.spec.secretName`, e.g., `team-support/support-assistant-tls`) in the Agent's namespace.
3. **Mounted**: into the agent Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The agent serves HTTPS using this certificate and presents it as a client cert on agent→gateway calls.
4. **Verified**: the gateway verifies the agent's certificate against the Agentry CA (`agentry-ca`) on every message delivery request and on every inbound mTLS call.
5. **Rotated**: cert-manager continuously re-issues the cert within `spec.renewBefore` of expiry (chart defaults: 90d duration, 30d renewBefore). kubelet updates the projected volume in the running Pod when the Secret changes; the agent reloads via a cert-file watch (starter templates demonstrate the pattern — see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)). Starter templates also watch `$AGENTRY_CA_CERT` and rebuild the outbound HTTP client's `RootCAs` pool when trust-manager re-projects the CA ConfigMap during CA rotation, so CA rotation is transparent to long-lived agent processes.
6. **Deleted**: the `Certificate` resource is owner-referenced to the Agent, so deleting the Agent cascade-deletes the `Certificate`; cert-manager in turn cleans up the output Secret.

### Lifecycle of an AgentTask TLS client certificate

1. **Created**: by the AgentTaskReconciler when provisioning the task Pod. The reconciler creates a cert-manager `Certificate` resource named `{taskName}-tls` in the AgentTask's namespace, owner-referenced to the AgentTask. `issuerRef` is `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`; the SAN is a single entry `{taskName}.{namespace}.task.agentry.io` (non-Service shape, since tasks have no Service); usages is `client auth` only.
2. **Stored**: cert-manager writes the output Secret (`{taskName}-tls`) in the AgentTask's namespace.
3. **Mounted**: into the task Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The task presents this cert on every call to `$AGENTRY_GATEWAY_ENDPOINT` (LLM requests, heartbeats, task completion). Tasks are not delivery targets for channel messages, so the cert does not need to serve TLS.
4. **Verified**: the gateway verifies the task's cert against the Agentry CA on every inbound mTLS call and extracts the namespace from the SAN.
5. **Rotated**: same mechanism as Agent certs — cert-manager re-issues within `spec.renewBefore`, kubelet propagates the update to the projected volume, the task's HTTP client reloads via cert-file watch.
6. **Deleted**: the `Certificate` is owner-referenced to the AgentTask, so task cleanup cascade-deletes it and cert-manager removes the output Secret.

### In-cluster TLS (Bidirectional)

All traffic between agent containers and the gateway is encrypted with TLS in both directions. **Agentry uses cert-manager (with the `trust-manager` sub-controller) as its sole CA and leaf-cert management stack.** The Helm chart installs the cert-manager/trust-manager resources (not the controllers themselves — both must already be present in the cluster):

- A cluster-scoped self-signed `ClusterIssuer` (`agentry-selfsigned`).
- A `Certificate` for the Agentry root (`agentry-ca` in `agentry-system`, `isCA: true`, chart default 5y lifetime).
- A cluster-scoped `ClusterIssuer` (`agentry-ca-issuer`) sourcing from the `agentry-ca` Secret in `agentry-system`. Because per-Agent and per-AgentTask `Certificate` resources live in user namespaces but are signed by the CA that lives in `agentry-system`, a `ClusterIssuer` is used — cert-manager does not resolve a namespaced `Issuer` across a namespace boundary.
- A `Certificate` for the gateway (`agentry-gateway-tls`) and one for the controller (`agentry-controller-tls`), both issued from `agentry-ca-issuer`. The gateway cert is used by both listeners — LLM (8443) and User (8080) — and there is no plaintext gateway listener. Both the gateway and controller `Certificate` resources declare `usages: [server auth, client auth]`, because each also presents its cert as a **client cert** when dialing the other's authenticated endpoints (see [Internal Endpoint Authentication](#internal-endpoint-authentication-activator-activity-channel-health)).
- A `trust-manager` `Bundle` that projects `agentry-ca` into a ConfigMap (`agentry-ca`) in every non-system namespace selected by `target.namespaceSelector.matchExpressions: [{ key: kubernetes.io/metadata.name, operator: NotIn, values: [kube-system, kube-public, kube-node-lease] }]`. The default selector is broad on purpose — CA bundle material is non-secret, and broad projection avoids the operator needing `patch` on Namespaces to label-target only Agent-hosting namespaces. Platform teams that want a tighter projection can override via the Helm value `trustManager.bundleSelector` (passed verbatim into the `Bundle`'s `target.namespaceSelector`). Agent and AgentTask Pods mount the resulting ConfigMap at `/var/run/agentry/ca.crt`.

cert-manager and trust-manager are **required dependencies**. Teams with existing deployments reuse them; the chart ships the Agentry-specific `Certificate`/`Issuer`/`Bundle` resources but leaves the controllers themselves out of scope. This decision supersedes an earlier self-managed-CA design; the earlier design was rejected because the operator code needed to manage CA generation, bundle rotation, staged leaf re-issuance, and cross-namespace cert distribution was large, had no analogue to borrow from, and duplicated functionality that cert-manager/trust-manager already provide correctly.

**Agent → Gateway (LLM traffic):** the LLM Gateway listener serves TLS using the `agentry-gateway-tls` Secret. The Agentry CA (`agentry-ca`) is distributed into every agent Pod's namespace as a trust bundle (via the `trust-manager` `Bundle` resource above, which projects the CA into a ConfigMap per namespace) and mounted into the agent Pod at `/var/run/agentry/ca.crt` (`$AGENTRY_CA_CERT`). Agents use this CA to verify the gateway's cert on `$AGENTRY_GATEWAY_ENDPOINT`. See [TLS on the LLM Gateway Listener](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener) for the full trust chain.

**Gateway → Agent (channel message delivery):** delivery on `POST /v1/message` is **bidirectional mTLS**. The gateway verifies the agent's cert-manager-issued `{agentName}-tls` against `agentry-ca`, and the agent verifies the gateway's `agentry-gateway-tls` against the same CA, requiring a SAN match on the gateway Service DNS — see [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) bullet 4 for the agent-side enforcement. This protects user messages (which may contain PII or sensitive data) from network-level sniffing on shared nodes and removes the need to treat NetworkPolicy as the sole access control on the message path.

**Controller endpoints (activator, activity, health):** the controller's HTTPS endpoints on port 9443 use `agentry-controller-tls` (issued from the same `agentry-ca-issuer` `ClusterIssuer`). The gateway trusts `agentry-ca` to verify. See [CONTROLLER_RECONCILERS.md § Operator Structure](./CONTROLLER_RECONCILERS.md#operator-structure).

**CA rotation:** cert-manager re-issues the `agentry-ca` `Certificate` within `spec.renewBefore` of expiry. During the overlap window both the old and new CA certificates are present in the trust-bundle Secret; kubelet propagates the update into projected volumes; every leaf cert remains valid under at least one of the two CAs until cert-manager has rotated all leaves to the new root. No operator code implements CA rotation — this was the main motivation for adopting cert-manager. The earlier bundle-based 4-step rotation sequence has been removed; cert-manager provides the equivalent behavior natively.

### Protecting agent containers from LLM provider access

Because the gateway is a separate Pod in `agentry-system`, NetworkPolicy can cleanly enforce agent isolation without any per-container workarounds:

```yaml
# NetworkPolicy for agent Pods
policyTypes:
  - Ingress
  - Egress
ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: agentry-system
      podSelector:
        matchLabels:
          app.kubernetes.io/name: agentry-gateway
    ports:
      - port: 8080      # Agent HTTPS health/message port ($AGENTRY_HEALTH_PORT) — gateway→agent channel message delivery
        protocol: TCP
egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: agentry-system
      podSelector:
        matchLabels:
          app.kubernetes.io/name: agentry-gateway
    ports:
      - port: 8443      # All agent→gateway TLS traffic (LLM calls, heartbeats, task completion)
        protocol: TCP
  - to:                    # DNS — scoped to kube-dns in kube-system
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: kube-system
      podSelector:
        matchLabels:
          k8s-app: kube-dns
    ports:
      - port: 53
        protocol: UDP
      - port: 53
        protocol: TCP
```

Agent containers that attempt to call LLM providers directly are blocked at the NetworkPolicy level. No service mesh or L7-capable CNI is required for this guarantee — standard Kubernetes NetworkPolicy is sufficient because the enforcement is cross-Pod. Both gateway listeners serve TLS using the same `agentry-gateway-tls` certificate: the LLM Gateway on port 8443 and the User Gateway on port 8080. External webhook traffic arrives via Ingress configured for backend re-encrypt or TLS pass-through, so there is no plaintext hop anywhere in the data path.

The DNS egress rule above is scoped to `kubernetes.io/metadata.name: kube-system` + `k8s-app: kube-dns`, which matches the upstream kube-dns/CoreDNS labelling used by kubeadm, EKS, GKE, AKS, and the standard CoreDNS chart. Clusters whose DNS Pod uses a different namespace or label set (custom CoreDNS chart, NodeLocal DNSCache only) must override the selector — the reconciler exposes this as the Helm value [`controller.networkPolicy.dnsSelector`](./DEPLOYMENT.md#helm-chart-contents) (an object with `namespaceLabels` and `podLabels` keys) on the synthesized per-agent NetworkPolicy. An untrusted agent must not be able to reach arbitrary Pods on port 53; the previous `namespaceSelector: {}` rule allowed exactly that and is no longer acceptable.

---

## Isolation

### RuntimeClass

AgentClass specifies a `runtimeClassName` that maps to a Kubernetes `RuntimeClass`. Platform teams use this to require stronger isolation for risky agents:

- `runc` (default): standard container isolation. Appropriate for trusted agents calling APIs only.
- `gvisor` / `runsc`: userspace kernel, syscall filtering. Appropriate for agents executing untrusted code.
- `kata`: VM-level isolation via lightweight hypervisor. Appropriate for strong multi-tenancy.
- `firecracker` (via Kata or Agent Sandbox): microVM isolation. Highest isolation, comparable cost to Kata.

Platform teams create separate AgentClasses for each isolation tier (e.g., `standard` uses runc, `sandboxed` requires gVisor) and developers choose based on their needs.

### Pod Security Standards

Every Agentry-created Pod complies with the `restricted` Pod Security Standard by default:
- `runAsNonRoot: true`
- `allowPrivilegeEscalation: false`
- `readOnlyRootFilesystem: true` (the PVC provides writable storage)
- All Linux capabilities dropped
- `seccompProfile: RuntimeDefault`

AgentClass can override these defaults, but the operator emits warnings for any deviation and the AgentClass reconciler sets a condition for any deviation from the restricted baseline.

### Network Policy

AgentClass includes network policy fields that the controller translates into `NetworkPolicy` resources:

- **Egress**: by default, deny all egress except to the Agentry gateway in `agentry-system` and DNS. Because the gateway is a separate Pod (not a sidecar), this is enforceable with standard Kubernetes NetworkPolicy without requiring a service mesh. Platform team adds explicit allowlist entries for MCP servers, external APIs, etc. via two fields:
  - **`spec.network.egress.allowedCIDRs`** — array of CIDR blocks. Maps directly to `NetworkPolicy.egress.to.ipBlock.cidr` and works on every CNI that implements Kubernetes NetworkPolicy. This is the portable primitive and should be preferred.
  - **`spec.network.egress.allowedHosts`** — array of DNS names. Only enforceable on CNIs that support FQDN egress policies (Cilium via `CiliumNetworkPolicy.toFQDNs`, Calico Enterprise). Standard `NetworkPolicy` has no equivalent. On unsupported CNIs the AgentClassReconciler emits a `Warning` event and ignores the field; `allowedCIDRs` alone governs egress. See [AgentClassReconciler](./CONTROLLER_RECONCILERS.md#agentclassreconciler).
- **Ingress**: by default, deny all ingress except from the Agentry gateway (which delivers channel messages via `POST /v1/message`). The Service makes the agent reachable within the cluster by the gateway; no other inbound traffic is allowed by default. NetworkPolicy and the agent-side mTLS check on `POST /v1/message` (see [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) bullet 4) are layered controls — a misconfigured per-Agent NP no longer opens delivery to arbitrary in-cluster callers.
- **Inter-agent**: disabled by default. To allow same-namespace agent-to-agent traffic, platform teams set `spec.network.allowSameNamespaceIngress: true` on the AgentClass. The controller translates this into a NetworkPolicy `ingress.from.podSelector` rule scoped to Pods in the same namespace bearing the Agentry agent label. This is opt-in — the default deny-all-ingress posture reflects the assumption that agent containers are untrusted.

**The "standard Kubernetes NetworkPolicy is sufficient" claim is scoped to agent → gateway enforcement and to IP-/CIDR-level egress governance.** Hostname-based egress (`allowedHosts`) is *not* enforceable on standard NetworkPolicy; clusters that require FQDN-level egress must use Cilium or Calico Enterprise.

### Resource Isolation

Every Agent/AgentTask has resource limits enforced via Pod `resources.limits`. AgentClass.maxLimits sets the cap. This prevents a runaway agent from exhausting node resources.

---

## Data Flow and Audit

### What flows where

- **Agent → LLM Gateway**: prompts and completions. In-cluster HTTPS (TLS terminated at the gateway; agent trusts the Agentry CA via the projected trust bundle). See § In-cluster TLS above.
- **LLM Gateway → LLM Provider**: prompts and completions over egress. Always HTTPS. Custom CA bundles supported for enterprise environments — see [Upstream TLS Configuration](./GATEWAY_LLM.md#upstream-tls-configuration).
- **Channel Platform → User Gateway**: inbound webhook messages. HTTPS inbound to the gateway's public endpoint (via Ingress).
- **User Gateway → Agent**: normalized message envelope via `POST /v1/message` to the agent's ClusterIP Service over **bidirectional mTLS** (gateway verifies agent's cert-manager-issued TLS certificate against `agentry-ca`; agent verifies the gateway's client cert with SAN-match against `agentry-ca` per [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) bullet 4). See § In-cluster TLS above.
- **Controller → Gateway**: activity timestamp queries via `GET /v1/activity` (mTLS with SAN-based authorization, internal ClusterIP Service). See § Internal Endpoint Authentication above.
- **Gateway → API Server**: task completion data written to per-task ConfigMaps in user namespaces.
- **Controller ↔ API server**: CRD updates, Pod creation, events. Standard kubelet/apiserver channels.

### Audit trail

The operator emits Kubernetes Events for:
- Every phase transition on Agent/AgentTask.
- Every provider access decision (grant/deny).
- Every budget threshold crossing.
- Every credential rotation.

Events persist in etcd per the cluster's Event retention. For long-term audit, platform teams should ship events to an external audit log (standard k8s audit logging, Falco, etc.).

Agentry does **not** log prompts or completions. LLM payloads are sensitive (may contain PII, proprietary data) and Agentry takes no responsibility for their persistence. If prompt auditing is required, it should be implemented as a separate concern (e.g., an auditing provider adapter that duplicates traffic to a log sink).

---

## Threat Model

| Threat | Mitigation |
|---|---|
| Malicious agent container (Agentry-managed Pod) tries to call LLM providers directly | Credentials never leave `agentry-system`; the AgentReconciler synthesizes a per-Agent NetworkPolicy whose default-deny egress (standard k8s, no service mesh required) blocks direct egress from the agent Pod to provider IPs. Same for AgentTask Pods. |
| Gateway-only-tier workload calls LLM providers directly with its own keys | **Not mitigated by Agentry.** Gateway-only-tier workloads are existing Deployments that the platform team has granted gateway access to via `TokenReview`; they have no Agent CR and therefore no Agentry-synthesized NetworkPolicy. Routing through the gateway is voluntary in this tier — a workload that holds its own provider credentials can bypass the gateway entirely. Platform teams adopting the gateway-only tier must apply their own default-deny egress NetworkPolicy (or use a service mesh) on those namespaces if they want to enforce gateway routing. The full Agent lifecycle tier remains the only path with automatic NetworkPolicy enforcement. |
| Developer authors a permissive NetworkPolicy in their own namespace that broadens Agent Pod egress | **Out of scope by trust model.** § Trust Model places the developer in the trusted tier; opting out of guardrails is the developer's choice, not a defended-against threat. NetworkPolicy is additive, so an additional permissive policy unions with Agentry's synthesized one — Agentry cannot prevent this through synthesis alone. Platform teams who treat developers as untrusted should restrict `networkpolicies` create/patch in user namespaces via cluster RBAC. The agent-container threat (the actual untrusted actor) cannot author NetworkPolicies — its ServiceAccount has no such permissions by default. |
| Developer deploys agent with resource bomb | AgentClass.maxLimits enforced; image allowlist prevents arbitrary images |
| Developer bypasses gateway by embedding credentials in image | No mitigation at the platform level — process/review concern; mitigate via image scanning and registry controls |
| Agent container executes LLM-generated code that attempts container escape | RuntimeClass (gVisor/Kata) provides kernel-level isolation; Pod Security Standards prevent privilege escalation |
| Platform credentials leak via etcd backup | Standard k8s concern; encrypt etcd at rest |
| One tenant exhausts another tenant's budget via shared provider | Per-namespace spend accounting; `allowedNamespaces` restricts access; budgets are soft limits with bounded overspend |
| Stale credentials after rotation cause silent failures | Gateway watches Secrets for changes; ModelProviderReconciler verifies credential validity on each health check |
| Compromised operator exploits cluster-wide Secret access | Operator no longer has cluster-wide Secret access. Default Secret access is scoped to `agentry-system` only. The only user-namespace Secret reach is via dynamic per-AgentChannel Roles, each `resourceNames`-scoped to the single auth Secret referenced by that channel and used solely to validate the configured `data` key. A compromised operator cannot enumerate or read arbitrary user-namespace Secrets. |
| Compromised gateway reads all LLM credentials | Gateway Secret access scoped to `agentry-system`; gateway image should be signed and verified; restrict who can update gateway Deployment |
| Agent makes requests to unauthorized provider | Gateway validates model against ModelProvider.models and namespace against allowedNamespaces |
| Budget guardrails exceeded under high concurrency | Budgets are documented as soft limits with bounded overspend; hard caps at the provider account level recommended for strict requirements |
| Compromised gateway writes malicious ConfigMaps to user namespaces | The `{taskName}-completion` ConfigMap is pre-created by the AgentTaskReconciler with the AgentTask as `ownerRef`; the gateway's per-task Role grants only `update, patch` on that exact name (`resourceNames`-scoped). The gateway has **no `create` verb** on user-namespace ConfigMaps, so a compromised gateway cannot introduce new ConfigMaps in user namespaces — it can mutate only the per-task and per-channel resources it has explicit name-scoped access to. |
| Malicious message from channel platform | Webhook adapter authenticates inbound events (bearer token, HMAC signature) before processing |
| Caller with channel A's credentials fetches channel B's async response by supplying channel B's `requestId` to the poll endpoint | The poll endpoint authenticates the caller against the AgentChannel named by the `channelPath` query parameter, then asserts that the stored response's `agentry.io/channel-namespace` / `agentry.io/channel-name` labels match that same AgentChannel before returning the payload. `requestId` values are UUIDs but not secrets; the label check prevents cross-channel data leakage. Mismatches return `403 Forbidden`. See [API_ENDPOINTS.md § Async Webhook Response poll semantics](./API_ENDPOINTS.md#async-webhook-response-gateway-managed). |
| Developer uses `AgentChannel.spec.webhook.callbackUrl` as SSRF against internal cluster services (e.g., `kubernetes-dashboard.kube-system`, cloud metadata at 169.254.169.254) | The gateway has stronger egress than any user namespace, so an unrestricted `callbackUrl` would let the developer turn the gateway into a confused deputy. The AgentChannelReconciler enforces at admission/reconcile time that `callbackUrl` uses `https://` and that its host does not resolve to loopback, link-local, RFC1918, unique-local IPv6, or cloud-metadata IPs (see [API_RESOURCES.md § Cross-Resource Validation rule 22](./API_RESOURCES.md#cross-resource-validation)). The gateway re-resolves the host and re-applies the check on every delivery attempt to defeat DNS rebinding (see [GATEWAY_USER.md § Request Flow step 8](./GATEWAY_USER.md#user-gateway--request-flow)). Platform teams may replace the deny-internal default with an explicit allowlist via the Helm value `gateway.callbackUrl.allowlist`. |
| Third party forges a callback POST to a developer's `callbackUrl` | The gateway signs every callback POST (success and error payloads alike) using `AgentChannel.spec.webhook.callbackAuth` — bearer (`Authorization: Bearer …`) or HMAC over the canonical string `"{requestId}\n{timestamp}\n{sha256(body)}"` with the timestamp in `X-Agentry-Timestamp`. `callbackAuth` is required by [API_RESOURCES.md cross-resource validation rule 25](./API_RESOURCES.md#cross-resource-validation) whenever `callbackUrl` is set — CRD CEL rejects AgentChannels that try to configure an unsigned callback, so unsigned callbacks cannot be deployed by accident. Receivers verify the signature using the same Secret material; replay is bounded by a 300s timestamp skew window (mirroring the polling-endpoint contract). See [API_ENDPOINTS.md § Callback authentication](./API_ENDPOINTS.md#async-webhook-response-gateway-managed). |
| Agent spoofs namespace to bypass budget/access controls (mTLS tier) | Namespace is extracted from the cert SAN, signed by `agentry-ca`. Agents use `{name}.{namespace}.svc.cluster.local`; AgentTasks use `{name}.{namespace}.task.agentry.io`. An agent cannot forge a cert for a different namespace (the CA key is not reachable from any agent Pod). Two additional defenses specifically prevent a dotted-name label-shift bypass: (1) Agent and AgentTask `metadata.name` are restricted to DNS-1123 **label** form (no dots) by CRD CEL — see [API_RESOURCES.md § Cross-Resource Validation rule 21](./API_RESOURCES.md#cross-resource-validation); and (2) the gateway's SAN parser requires the exact label count for each shape (5 for Service-DNS, 4 for `.task.agentry.io`) and rejects any cert whose SAN has extra labels — see [GATEWAY_LLM.md § Namespace Identification § Mode 1](./GATEWAY_LLM.md#mode-1--mtls-client-certificate-agentry-managed-pods). Source-IP → Pod cross-check validates the cert identity against the actual source Pod; all checks must agree. |
| Agent spoofs namespace (gateway-only-tier, token auth) | Namespace is extracted from the token's `status.user.username` returned by `TokenReview`, which the apiserver signs. An agent cannot forge a token for a different namespace — the token's signature is checked by the apiserver, not the gateway. Source-IP → Pod cross-check validates the Pod's actual namespace matches. Both cryptographic (apiserver signature) and topological (source IP) attestation must agree. |
| Gateway-only tenant uses a provider their AgentClass would have denied in the mTLS tier | **Expected behavior, not a vulnerability.** The gateway-only tier is deliberately not gated by AgentClass — those workloads have no Agent resource and therefore no `allowedProviders` to consult. Access control reduces to `ModelProvider.spec.allowedNamespaces` plus `spec.models`. Platform teams who need class-scoped provider policy must onboard workloads through the full Agent lifecycle tier. See [Provider Routing § Gateway-only tier](./GATEWAY_LLM.md#provider-routing). |
| Gateway-only-tier tenant uses a ServiceAccount token from another namespace to read another tenant's budget | Impossible: `TokenReview`'s returned `status.user.username` names the token's actual namespace of origin. The gateway uses *that* namespace for all authorization decisions, not any namespace the caller claims in the request body. A valid token from namespace A can only be used to act as namespace A. |
| Stolen `kubernetes.default.svc`-audience token reused against the gateway | Gateway's `TokenReview` request specifies audience `agentry-gateway`. Tokens minted for a different audience fail validation. Workloads must explicitly project a gateway-audience token. |
| Agentry-managed Agent/AgentTask Pod tries to downgrade auth by using its ServiceAccount token instead of mTLS | Rejected by the gateway's **Pod-ownership precheck** on the bearer-token path: before any `TokenReview` call, the gateway resolves the request's source IP to a Pod via its informer cache and returns `401 Unauthorized` if that Pod has an `ownerRef` to an `Agent` or `AgentTask` resource. The check runs on every request (it is not cached) and runs before `TokenReview`, so it is unaffected by token-cache hits or apiserver latency. The gateway also attempts mTLS first when a client cert is presented; if both auth materials are present the bearer header is ignored. mTLS remains the only accepted auth mode for that tier; cert rotation is the single revocation mechanism. See [GATEWAY_LLM.md § Namespace Identification — Mode 2](./GATEWAY_LLM.md#mode-2--serviceaccount-bearer-token-gateway-only-tier). |
| Channel credential leaked from agent namespace | Channel credentials are stored in the agent's namespace; blast radius is limited to that namespace's channels. The platform team (not the developer) is responsible for rotation. |
| Unauthorized agent wake-up via activator endpoint | The activator endpoint requires an mTLS client cert. The controller authorizes only clients whose cert SAN matches the gateway Service DNS — any other SAN, even one signed by `agentry-ca`, is rejected with `403 Forbidden`. Because the per-Agent cert SAN shape (`{name}.{namespace}.svc.cluster.local`) cannot match the gateway SAN, a compromised agent cannot use its own cert to trigger wake-ups. See § Internal Endpoint Authentication above. |
| Compromised in-cluster Pod with network reach to an agent's Service forges channel messages | The agent's `POST /v1/message` listener requires a client certificate whose SAN matches the gateway Service DNS (`agentry-gateway.agentry-system.svc.cluster.local` / `.svc`). A compromised non-gateway Pod cannot present such a cert (the `agentry-ca` private key is not reachable from any non-gateway Pod), so even if it bypasses or piggybacks on a misconfigured per-Agent NetworkPolicy the TLS handshake fails. NetworkPolicy is the first layer; the agent-side mTLS check is the second. See [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) bullet 4 and § In-cluster TLS above. |
| In-cluster traffic sniffed on shared nodes | All agent↔gateway and gateway↔controller traffic is TLS-encrypted. Certificates are cert-manager-managed and rooted at `agentry-ca`. See § In-cluster TLS above. |
| cert-manager not installed or unhealthy | Chart install fails fast if `agentry-ca-issuer` cannot be created. Runtime degradation of cert-manager delays new Agent/AgentTask provisioning (the `Certificate` Secret is not populated) and blocks cert rotation, but running agents continue until their current certs approach expiry. Operators should monitor cert-manager health as a cluster-critical dependency. |
| trust-manager not installed or unhealthy | Chart install fails fast if the `Bundle` resource cannot be created. Runtime degradation prevents the Agentry CA ConfigMap from appearing in new namespaces, so Pods scheduled into those namespaces fail to mount `/var/run/agentry/ca.crt` and cannot verify the gateway's TLS cert. Existing namespaces with the ConfigMap already projected are unaffected until the next CA rotation. Monitor trust-manager alongside cert-manager. |

---

## Recommendations for Deployment

1. **Install Agentry in a dedicated, locked-down namespace** (`agentry-system`). Restrict who can `exec` into or modify resources in this namespace.
2. **Expose the User Gateway via a dedicated Ingress or LoadBalancer** with TLS termination. The gateway's public endpoint receives inbound platform events.
3. **Enable k8s audit logging** at the `Metadata` level minimum, `RequestResponse` for Secret access if feasible.
4. **Standard Kubernetes NetworkPolicy is sufficient** for the agent → gateway egress rule and for CIDR-scoped external egress (`allowedCIDRs`) — no service mesh required. The cluster-level gateway architecture makes agent→gateway egress cross-Pod and fully enforceable. If you need FQDN-based egress (`allowedHosts`), install Cilium or Calico Enterprise; standard NetworkPolicy cannot express hostname rules. **This guarantee is automatic only for Agentry-managed Pods (Agents and AgentTasks).** Gateway-only-tier workloads do not receive an Agentry-synthesized NetworkPolicy — platform teams adopting that tier must apply their own default-deny egress posture on those namespaces if they want to prevent direct provider calls. See the matching threat-model rows above.
5. **Separate LLM credential management from platform engineering access** if possible (e.g., only a secrets-admin role can read/write credential Secrets in `agentry-system`). This requires cluster RBAC beyond Agentry's scope.
6. **Require an appropriate RuntimeClass for any AgentClass that allows LLM code execution.** Platform admins own RuntimeClass installation and compatibility validation.
7. **Review AgentClass configurations as code** (GitOps). Cluster-scoped resources that grant capabilities should never be edited by hand in production.