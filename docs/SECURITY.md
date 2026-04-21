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
- `create, get, update, patch, delete` on `Pods`, `PersistentVolumeClaims`, `Services`, `ConfigMaps`, `NetworkPolicies` cluster-wide.
- `get, list, watch` on `RuntimeClass`, `StorageClass`, `Namespaces` (for validation).
- `create, patch` on `Events` (for event emission).
- Leader election resources (`Leases`) in its own namespace.
- `get, list, watch` on `Secrets` in `agentry-system` only (not cluster-wide) — the operator validates that ModelProvider credential Secrets exist.

**Unlike a sidecar model, the operator does not need cluster-wide Secret read/write access.** Credentials are held by the gateway ServiceAccount in `agentry-system` and never copied to user namespaces. This significantly reduces the operator's blast radius.

### Gateway ServiceAccount permissions

The Agentry Gateway runs under a separate ServiceAccount (`agentry-system/agentry-gateway`) with:

- `get, watch` on `Secrets` in `agentry-system` (to read LLM provider credentials).
- `get, watch` on `ConfigMaps` in `agentry-system` (to receive budget configuration from the operator).
- **`create` on `tokenreviews.authentication.k8s.io`** (cluster-scoped, no resource name needed — `TokenReview` is a virtual resource). This permits the gateway to validate projected ServiceAccount bearer tokens presented by gateway-only-tier workloads. Without this, the gateway cannot accept non-mTLS authentication. See [LLM Gateway § Namespace Identification mode 2](./GATEWAY_LLM.md#mode-2--serviceaccount-bearer-token-gateway-only-tier).
- `get, list, watch` on `Agent` resources cluster-wide (for provider routing resolution and hibernation state checks).
- `get, list, watch` on `AgentTask` resources cluster-wide (for task completion handling: the gateway resolves the calling Pod's ownerRef to identify the associated AgentTask, validates that declared artifact names in the completion payload match `spec.artifacts`, and reads the AgentTask's UID to set the ownerRef on the completion ConfigMap it creates).
- `get, list, watch` on `AgentChannel` resources cluster-wide (to look up which Agent a channel message targets and to manage platform connections).
- `patch` on `AgentChannel` resources cluster-wide (to write the `agentry.io/channel-disconnected` annotation during the finalizer handoff — see [Finalizers](./CONTROLLER_LIFECYCLE.md#finalizers)).
- `get, list, watch` on `ModelProvider` resources cluster-wide (for model validation, `allowedNamespaces` checks, budget configuration, and fallback chain resolution).
- `get, watch` on specific Secrets in user namespaces referenced by AgentChannel webhook auth config (`spec.webhook.auth.secretRef` for bearer, `spec.webhook.auth.hmac.secretRef` for HMAC — for channel platform credentials like webhook auth tokens). This is implemented via **dynamic per-namespace Roles**: when the AgentChannelReconciler creates or updates an AgentChannel, it ensures a Role and RoleBinding exist in the agent's namespace granting the gateway ServiceAccount `get, watch` on the specific Secret referenced by the active auth type. The reconciler cleans up these Roles when the AgentChannel is deleted. The gateway does not have blanket Secret access across user namespaces.
- `get, list, watch` on `Pods` cluster-wide (to maintain the Pod informer cache used for source IP → namespace resolution on LLM requests).
- Per-task dynamic `Role` and `RoleBinding` for ConfigMap write access: when the AgentTaskReconciler provisions a task, it creates a `Role` in the task's namespace granting the gateway ServiceAccount `create, update` on the specific ConfigMap named `{taskName}-completion`, along with a `RoleBinding` to `agentry-system/agentry-gateway`. Both are owned by the AgentTask (via ownerRef) and cascade-deleted on task cleanup. This follows the same scoped-access pattern used for channel credentials (see AgentChannel webhook auth config). The gateway does **not** have blanket ConfigMap write access across user namespaces.
- `get` on `Services` in user namespaces (to resolve Agent endpoints for message delivery).

The gateway's LLM credential access is scoped to `agentry-system`. Channel credential access extends to user namespaces via dynamic per-namespace Roles created by the AgentChannelReconciler — the gateway only reads the specific Secrets referenced by the AgentChannel's active webhook auth config, not arbitrary Secrets. These Roles are cleaned up when the AgentChannel is deleted. The `patch` permission on AgentChannel is narrowly used: the gateway writes only the `agentry.io/channel-disconnected` annotation as part of the finalizer handshake when a channel is deleted. ConfigMap write access in user namespaces follows the same scoped-Role pattern: the AgentTaskReconciler creates a per-task Role granting the gateway `create, update` on only the `{taskName}-completion` ConfigMap in the task's namespace; both Role and RoleBinding are owned by the AgentTask and cascade-deleted. The gateway has no blanket ConfigMap access across user namespaces. Async webhook response ConfigMaps (`agentry-async-{requestId}`) are stored in `agentry-system` (where the gateway already has full ConfigMap access), not in agent namespaces — this is why no additional per-channel Role is needed for async response writes. The AgentChannelReconciler prunes these ConfigMaps in `agentry-system` using label selectors (`agentry.io/channel-namespace`, `agentry.io/channel-name`) rather than ownerRefs (cross-namespace ownerRefs do not trigger Kubernetes GC). Activity tracking does not require any etcd writes — the gateway maintains activity timestamps in-memory and serves them to the controller via an internal HTTP API.

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

Each Agent Pod runs with a ServiceAccount **distinct** from both the operator and the developer. By default, Agentry creates a per-agent ServiceAccount with **no RBAC permissions**. The agent has no access to the Kubernetes API.

If an agent needs cluster API access (e.g., a Kubernetes-administering agent), the developer or platform team must explicitly create a Role and RoleBinding. This is opt-in, not default.

### Agent→Gateway Authentication

The gateway supports **two authentication modes** for inbound requests, mapped to the two Helm tiers.

**Mode 1 — mTLS client certificate (Agentry-managed Pods).** This is the only authentication path available to Pods created by the AgentReconciler (Agent / AgentTask workloads). Agents present the cert-manager-issued certificate at `$AGENTRY_TLS_CERT` as a client cert on the LLM Gateway's TLS listener. The gateway verifies against `agentry-ca` and extracts (namespace, agent name) from the SAN (`{name}.{namespace}.svc.cluster.local`). Identity is cryptographically attested; the CA private key is not reachable from any agent Pod.

**Agent/AgentTask Pods must use mTLS — their ServiceAccount tokens are not accepted by the gateway.** This is a deliberate asymmetry: accepting SA tokens from Agentry-managed Pods would create two attack paths (cert path plus token path), and a compromised agent could use the token path to bypass cert revocation after rotation. Rejecting SA tokens from the cert-path tier forces cert rotation to remain the single revocation mechanism for that tier.

**Mode 2 — projected ServiceAccount bearer token via `TokenReview` (gateway-only tier).** For existing workloads in user namespaces that the platform team has granted LLM-provider access to without adopting the Agent CRD. The caller mounts a projected ServiceAccount token with audience `agentry-gateway` and sends it as `Authorization: Bearer <token>`. The gateway POSTs the token to `authentication.k8s.io/v1/tokenreviews`, requires `status.authenticated: true`, and extracts the namespace from `status.user.username` (`system:serviceaccount:<ns>:<sa>`). Validation results are cached by the token's SHA-256 hash until `status.expirationTimestamp` minus a 60s safety margin.

Audience binding is critical: the gateway requests the `agentry-gateway` audience in its `TokenReview`, so a generic `kubernetes.default.svc`-audience token (such as a stolen kubelet token) cannot authenticate. Callers must configure their projected-volume `audience: agentry-gateway`.

**Secondary cross-check (both modes):** the gateway resolves the source IP to a Pod via its Pod informer and confirms the Pod is in the namespace identity resolution produced. This closes a stolen-credential scenario where a cert or token from one Pod is presented from a different Pod. Cross-check failure → request rejected.

See [GATEWAY_LLM.md § Namespace Identification](./GATEWAY_LLM.md#namespace-identification) for the full request-time flow.

**Client cert presentation:** starter templates (see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)) configure mTLS client-cert presentation and the cert-file watch-and-reload pattern. Custom Agent/AgentTask images must present `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY` on every agent→gateway call (LLM requests, heartbeats, task completion). Activity timestamps and heartbeats are tracked in-memory in the gateway — no etcd writes are involved in agent→gateway communication.

### Internal Endpoint Authentication (Activator & Activity API)

The controller's activator endpoint (`POST /v1/activate/{namespace}/{agentName}`) and the gateway's activity endpoint (`GET /v1/activity`) are both authenticated using the same HMAC scheme. The operator generates a shared HMAC key on installation and stores it in a Secret in `agentry-system` (`agentry-activator-key`). Both the controller and gateway use this key:

- **Activator** (gateway → controller): the gateway includes an HMAC-signed `Authorization` header on each activation request; the controller validates the signature and rejects requests with stale timestamps (>30s). This ensures only the gateway can trigger agent wake-ups.
- **Activity API** (controller → gateway): the controller includes the same HMAC-signed header when querying `/v1/activity`; the gateway validates it. This prevents arbitrary Pods from querying per-agent activity data across namespaces.

**Key rotation uses a key-ring to eliminate the transition failure window.** The `agentry-activator-key` Secret stores two fields (`current-key` and `previous-key`). During rotation, both keys are accepted simultaneously for a configurable transition window (default: 60s) before the old key is removed. This ensures the gateway and controller — which pick up Secret changes independently via their respective watches — are never in a state where one component has the new key and the other does not. See [Activator Authentication](./GATEWAY_USER.md#activator-authentication) for the full rotation sequence.

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
2. **Referenced**: by the AgentChannel's webhook auth config (`spec.webhook.auth.secretRef` for bearer, `spec.webhook.auth.hmac.secretRef` for HMAC). Future platform types (v1.1) will use a top-level `credentialsRef`.
3. **Loaded**: the gateway watches `AgentChannel` resources directly. When it sees a new or updated AgentChannel, it reads the referenced Secret from the agent's namespace using its scoped RBAC and holds the credential in-process for the channel adapter.
4. **Rotated**: same watch-based mechanism as LLM credentials — the gateway watches the referenced Secret for changes and refreshes in-memory credentials without a restart.

Channel credentials are namespace-scoped for organizational isolation — each namespace contains only the credentials for its own agents' channels. They are created by the platform team or a provisioning service; developers do not need Secret access in their namespace.

### Lifecycle of an agent TLS serving certificate

1. **Created**: by the AgentReconciler when provisioning the agent's Pod. The reconciler creates a cert-manager `Certificate` resource named `{agentName}-tls` in the Agent's namespace, owner-referenced to the Agent. Its `issuerRef` points at `agentry-issuer` in `agentry-system`; SAN list covers `{name}.{namespace}.svc.cluster.local`, `{name}.{namespace}.svc`, `{name}.{namespace}`; usages are `server auth` and `client auth` (the same cert serves both directions).
2. **Stored**: cert-manager writes the output Secret (name = `Certificate.spec.secretName`, e.g., `team-support/support-assistant-tls`) in the Agent's namespace.
3. **Mounted**: into the agent Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The agent serves HTTPS using this certificate and presents it as a client cert on agent→gateway calls.
4. **Verified**: the gateway verifies the agent's certificate against the Agentry CA (`agentry-ca`) on every message delivery request and on every inbound mTLS call.
5. **Rotated**: cert-manager continuously re-issues the cert within `spec.renewBefore` of expiry (chart defaults: 90d duration, 30d renewBefore). kubelet updates the projected volume in the running Pod when the Secret changes; the agent reloads via a cert-file watch (starter templates demonstrate the pattern — see [STARTER_TEMPLATES.md](./STARTER_TEMPLATES.md)).
6. **Deleted**: the `Certificate` resource is owner-referenced to the Agent, so deleting the Agent cascade-deletes the `Certificate`; cert-manager in turn cleans up the output Secret.

### Lifecycle of an AgentTask TLS client certificate

1. **Created**: by the AgentTaskReconciler when provisioning the task Pod. The reconciler creates a cert-manager `Certificate` resource named `{taskName}-tls` in the AgentTask's namespace, owner-referenced to the AgentTask. `issuerRef` points at `agentry-issuer` in `agentry-system`; the SAN is a single entry `{taskName}.{namespace}.task.agentry.io` (non-Service shape, since tasks have no Service); usages is `client auth` only.
2. **Stored**: cert-manager writes the output Secret (`{taskName}-tls`) in the AgentTask's namespace.
3. **Mounted**: into the task Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The task presents this cert on every call to `$AGENTRY_GATEWAY_ENDPOINT` (LLM requests, heartbeats, task completion). Tasks are not delivery targets for channel messages, so the cert does not need to serve TLS.
4. **Verified**: the gateway verifies the task's cert against the Agentry CA on every inbound mTLS call and extracts the namespace from the SAN.
5. **Rotated**: same mechanism as Agent certs — cert-manager re-issues within `spec.renewBefore`, kubelet propagates the update to the projected volume, the task's HTTP client reloads via cert-file watch.
6. **Deleted**: the `Certificate` is owner-referenced to the AgentTask, so task cleanup cascade-deletes it and cert-manager removes the output Secret.

### In-cluster TLS (Bidirectional)

All traffic between agent containers and the gateway is encrypted with TLS in both directions. **Agentry uses cert-manager (with the `trust-manager` sub-controller) as its sole CA and leaf-cert management stack.** The Helm chart installs the cert-manager/trust-manager resources (not the controllers themselves — both must already be present in the cluster):

- A cluster-scoped self-signed `ClusterIssuer` (`agentry-selfsigned`).
- A `Certificate` for the Agentry root (`agentry-ca` in `agentry-system`, `isCA: true`, chart default 5y lifetime).
- A namespace-scoped `Issuer` (`agentry-issuer` in `agentry-system`, sourcing from `agentry-ca`) with `spec.ca.crossNamespace: true` so the AgentReconciler and AgentTaskReconciler can create per-workload `Certificate` resources in user namespaces that reference the `agentry-system` issuer.
- A `Certificate` for the gateway (`agentry-gateway-tls`) and one for the controller (`agentry-controller-tls`), both issued from `agentry-issuer`. The gateway cert is used by both listeners — LLM (8443) and User (8080) — and there is no plaintext gateway listener.
- A `trust-manager` `Bundle` that projects `agentry-ca` into a ConfigMap in every namespace hosting an Agent or AgentTask; agent Pods mount it at `/var/run/agentry/ca.crt`.

cert-manager and trust-manager are **required dependencies**. Teams with existing deployments reuse them; the chart ships the Agentry-specific `Certificate`/`Issuer`/`Bundle` resources but leaves the controllers themselves out of scope. This decision supersedes an earlier self-managed-CA design; the earlier design was rejected because the operator code needed to manage CA generation, bundle rotation, staged leaf re-issuance, and cross-namespace cert distribution was large, had no analogue to borrow from, and duplicated functionality that cert-manager/trust-manager already provide correctly.

**Agent → Gateway (LLM traffic):** the LLM Gateway listener serves TLS using the `agentry-gateway-tls` Secret. The Agentry CA (`agentry-ca`) is distributed into every agent Pod's namespace as a trust bundle (via the `trust-manager` `Bundle` resource above, which projects the CA into a ConfigMap per namespace) and mounted into the agent Pod at `/var/run/agentry/ca.crt` (`$AGENTRY_CA_CERT`). Agents use this CA to verify the gateway's cert on `$AGENTRY_GATEWAY_ENDPOINT`. See [TLS on the LLM Gateway Listener](./GATEWAY_LLM.md#tls-on-the-llm-gateway-listener) for the full trust chain.

**Gateway → Agent (channel message delivery):** the gateway verifies the agent's cert-manager-issued `{agentName}-tls` certificate against `agentry-ca` when delivering messages via `POST /v1/message`. This protects user messages (which may contain PII or sensitive data) from network-level sniffing on shared nodes.

**Controller endpoints (activator, activity, health):** the controller's HTTPS endpoints on port 9443 use `agentry-controller-tls` (issued from the same `agentry-issuer`). The gateway trusts `agentry-ca` to verify. See [CONTROLLER_RECONCILERS.md § Operator Structure](./CONTROLLER_RECONCILERS.md#operator-structure).

**CA rotation:** cert-manager re-issues the `agentry-ca` `Certificate` within `spec.renewBefore` of expiry. During the overlap window both the old and new CA certificates are present in the trust-bundle Secret; kubelet propagates the update into projected volumes; every leaf cert remains valid under at least one of the two CAs until cert-manager has rotated all leaves to the new root. No operator code implements CA rotation — this was the main motivation for adopting cert-manager. The earlier bundle-based 4-step rotation sequence has been removed; cert-manager provides the equivalent behavior natively.

### Protecting agent containers from LLM provider access

Because the gateway is a separate Pod in `agentry-system`, NetworkPolicy can cleanly enforce agent isolation without any per-container workarounds:

```yaml
# NetworkPolicy for agent Pods
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
  - to:                    # DNS
    - namespaceSelector: {}
    ports:
      - port: 53
```

Agent containers that attempt to call LLM providers directly are blocked at the NetworkPolicy level. No service mesh or L7-capable CNI is required for this guarantee — standard Kubernetes NetworkPolicy is sufficient because the enforcement is cross-Pod. Both gateway listeners serve TLS using the same `agentry-gateway-tls` certificate: the LLM Gateway on port 8443 and the User Gateway on port 8080. External webhook traffic arrives via Ingress configured for backend re-encrypt or TLS pass-through, so there is no plaintext hop anywhere in the data path.

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
- **Ingress**: by default, deny all ingress except from the Agentry gateway (which delivers channel messages via `POST /v1/message`). The Service makes the agent reachable within the cluster by the gateway; no other inbound traffic is allowed by default.
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
- **User Gateway → Agent**: normalized message envelope via `POST /v1/message` to the agent's ClusterIP Service over HTTPS (gateway verifies agent's cert-manager-issued TLS certificate against `agentry-ca`). See § In-cluster TLS above.
- **Controller → Gateway**: activity timestamp queries via `GET /v1/activity` (HMAC-authenticated, internal ClusterIP Service). See § Internal Endpoint Authentication above.
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
| Malicious agent container tries to call LLM providers directly | Credentials never leave `agentry-system`; NetworkPolicy (standard k8s, no service mesh required) blocks direct egress from agent Pods to provider IPs |
| Developer deploys agent with resource bomb | AgentClass.maxLimits enforced; image allowlist prevents arbitrary images |
| Developer bypasses gateway by embedding credentials in image | No mitigation at the platform level — process/review concern; mitigate via image scanning and registry controls |
| Agent container executes LLM-generated code that attempts container escape | RuntimeClass (gVisor/Kata) provides kernel-level isolation; Pod Security Standards prevent privilege escalation |
| Platform credentials leak via etcd backup | Standard k8s concern; encrypt etcd at rest |
| One tenant exhausts another tenant's budget via shared provider | Per-namespace spend accounting; `allowedNamespaces` restricts access; budgets are soft limits with bounded overspend |
| Stale credentials after rotation cause silent failures | Gateway watches Secrets for changes; ModelProviderReconciler verifies credential validity on each health check |
| Compromised operator exploits cluster-wide Secret access | Operator no longer has cluster-wide Secret access; operator Secret access scoped to `agentry-system` only |
| Compromised gateway reads all LLM credentials | Gateway Secret access scoped to `agentry-system`; gateway image should be signed and verified; restrict who can update gateway Deployment |
| Agent makes requests to unauthorized provider | Gateway validates model against ModelProvider.models and namespace against allowedNamespaces |
| Budget guardrails exceeded under high concurrency | Budgets are documented as soft limits with bounded overspend; hard caps at the provider account level recommended for strict requirements |
| Compromised gateway writes malicious ConfigMaps to user namespaces | Per-task dynamic Roles scope gateway ConfigMap write access to only `{taskName}-completion` in the task's namespace, for the task's lifetime. No blanket namespace access — a compromised gateway cannot write arbitrary ConfigMaps. |
| Malicious message from channel platform | Webhook adapter authenticates inbound events (bearer token, HMAC signature) before processing |
| Agent spoofs namespace to bypass budget/access controls (mTLS tier) | Namespace is extracted from the cert SAN, signed by `agentry-ca`. Agents use `{name}.{namespace}.svc.cluster.local`; AgentTasks use `{name}.{namespace}.task.agentry.io`. An agent cannot forge a cert for a different namespace (the CA key is not reachable from any agent Pod). Source-IP → Pod cross-check validates the cert identity against the actual source Pod; both must match. |
| Agent spoofs namespace (gateway-only-tier, token auth) | Namespace is extracted from the token's `status.user.username` returned by `TokenReview`, which the apiserver signs. An agent cannot forge a token for a different namespace — the token's signature is checked by the apiserver, not the gateway. Source-IP → Pod cross-check validates the Pod's actual namespace matches. Both cryptographic (apiserver signature) and topological (source IP) attestation must agree. |
| Gateway-only-tier tenant uses a ServiceAccount token from another namespace to read another tenant's budget | Impossible: `TokenReview`'s returned `status.user.username` names the token's actual namespace of origin. The gateway uses *that* namespace for all authorization decisions, not any namespace the caller claims in the request body. A valid token from namespace A can only be used to act as namespace A. |
| Stolen `kubernetes.default.svc`-audience token reused against the gateway | Gateway's `TokenReview` request specifies audience `agentry-gateway`. Tokens minted for a different audience fail validation. Workloads must explicitly project a gateway-audience token. |
| Agentry-managed Agent/AgentTask Pod tries to downgrade auth by using its ServiceAccount token instead of mTLS | Rejected by policy: the gateway does not accept SA-token auth from Pods created by the AgentReconciler. mTLS is the only accepted path for that tier, so cert revocation remains the single authoritative revocation mechanism. (Enforcement: the gateway attempts mTLS first; if a client presents a cert, the token is ignored. If mTLS fails and the Pod's IP belongs to an Agent/AgentTask Pod, the request is rejected regardless of a presented token.) |
| Channel credential leaked from agent namespace | Channel credentials are stored in the agent's namespace; blast radius is limited to that namespace's channels. The platform team (not the developer) is responsible for rotation. |
| Unauthorized agent wake-up via activator endpoint | Activator endpoint is TLS-protected (cert-manager-issued `agentry-controller-tls`, verified by the gateway against `agentry-ca`) and HMAC-authenticated inside the tunnel; only the gateway (which holds the shared key) can trigger wake-ups. See § Internal Endpoint Authentication above. |
| In-cluster traffic sniffed on shared nodes | All agent↔gateway and gateway↔controller traffic is TLS-encrypted. Certificates are cert-manager-managed and rooted at `agentry-ca`. See § In-cluster TLS above. |
| cert-manager not installed or unhealthy | Chart install fails fast if `agentry-issuer` cannot be created. Runtime degradation of cert-manager delays new Agent/AgentTask provisioning (the `Certificate` Secret is not populated) and blocks cert rotation, but running agents continue until their current certs approach expiry. Operators should monitor cert-manager health as a cluster-critical dependency. |
| trust-manager not installed or unhealthy | Chart install fails fast if the `Bundle` resource cannot be created. Runtime degradation prevents the Agentry CA ConfigMap from appearing in new namespaces, so Pods scheduled into those namespaces fail to mount `/var/run/agentry/ca.crt` and cannot verify the gateway's TLS cert. Existing namespaces with the ConfigMap already projected are unaffected until the next CA rotation. Monitor trust-manager alongside cert-manager. |

---

## Recommendations for Deployment

1. **Install Agentry in a dedicated, locked-down namespace** (`agentry-system`). Restrict who can `exec` into or modify resources in this namespace.
2. **Expose the User Gateway via a dedicated Ingress or LoadBalancer** with TLS termination. The gateway's public endpoint receives inbound platform events.
3. **Enable k8s audit logging** at the `Metadata` level minimum, `RequestResponse` for Secret access if feasible.
4. **Standard Kubernetes NetworkPolicy is sufficient** for the agent → gateway egress rule and for CIDR-scoped external egress (`allowedCIDRs`) — no service mesh required. The cluster-level gateway architecture makes agent→gateway egress cross-Pod and fully enforceable. If you need FQDN-based egress (`allowedHosts`), install Cilium or Calico Enterprise; standard NetworkPolicy cannot express hostname rules.
5. **Separate LLM credential management from platform engineering access** if possible (e.g., only a secrets-admin role can read/write credential Secrets in `agentry-system`). This requires cluster RBAC beyond Agentry's scope.
6. **Require an appropriate RuntimeClass for any AgentClass that allows LLM code execution.** Platform admins own RuntimeClass installation and compatibility validation.
7. **Review AgentClass configurations as code** (GitOps). Cluster-scoped resources that grant capabilities should never be edited by hand in production.