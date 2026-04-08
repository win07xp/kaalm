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
- `create, get, update, patch, delete` on `Pods`, `PersistentVolumeClaims`, `Services`, `ConfigMaps` cluster-wide.
- `get, list, watch` on `RuntimeClass`, `StorageClass`, `Namespaces` (for validation).
- `create, patch` on `Events` (for event emission).
- Leader election resources (`Leases`) in its own namespace.
- `get, list, watch` on `Secrets` in `agentry-system` only (not cluster-wide) — the operator validates that ModelProvider credential Secrets exist.

**Unlike a sidecar model, the operator does not need cluster-wide Secret read/write access.** Credentials are held by the gateway ServiceAccount in `agentry-system` and never copied to user namespaces. This significantly reduces the operator's blast radius.

### Gateway ServiceAccount

The Agentry Gateway runs under a separate ServiceAccount (`agentry-system/agentry-gateway`) with:

- `get, watch` on `Secrets` in `agentry-system` (to read LLM provider credentials).
- `get, watch` on `ConfigMaps` in `agentry-system` (to receive budget configuration from the operator).
- `get, list, watch` on `Agent` resources cluster-wide (for provider routing resolution and hibernation state checks).
- `get, list, watch` on `AgentChannel` resources cluster-wide (to look up which Agent a channel message targets and to manage platform connections).
- `get, watch` on `Secrets` in user namespaces where `AgentChannel` resources reference them (for channel platform credentials like Discord bot tokens). The gateway only reads Secrets explicitly referenced by `AgentChannel.spec.credentialsRef` — it does not have blanket Secret access across user namespaces.
- `get, list, watch` on `Pods` cluster-wide (to maintain the Pod informer cache used for source IP → namespace resolution on LLM requests, and for annotation writes).
- `patch` on `Pod` annotations in user namespaces (to write activity timestamps and task completion status).
- `get` on `Services` in user namespaces (to resolve Agent endpoints for message delivery).

The gateway's LLM credential access is scoped to `agentry-system`. Channel credential access extends to user namespaces but is narrowly scoped: the gateway only reads Secrets explicitly referenced by `AgentChannel.spec.credentialsRef` fields, not arbitrary Secrets.

### Platform Engineer role

A `ClusterRole` named `agentry-platform-admin` with:
- Full access to `AgentClass` and `ModelProvider`.
- `get, list, watch` on `Agent`, `AgentTask` cluster-wide (for observability).
- `create, get, update, delete` on Secrets in the operator's namespace (to manage LLM credentials).
- `create, get, update, delete` on Secrets in agent namespaces (to provision channel credentials referenced by `AgentChannel.spec.credentialsRef`).

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

The gateway authenticates all inbound requests from agent containers — LLM API calls, heartbeats (`POST /v1/agent/heartbeat`), and task completion signals (`POST /v1/task/complete`) — via **source IP → Pod resolution** using the Pod informer cache. This is the same mechanism used for namespace identification (see Gateway Design doc). Since agent Pods have no Kubernetes API credentials by default, and the gateway identifies callers by their cluster-assigned source IP, no token-based authentication is needed between agent containers and the gateway.

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
2. **Referenced**: by AgentChannel.spec.credentialsRef.
3. **Loaded**: the gateway watches `AgentChannel` resources directly. When it sees a new or updated AgentChannel, it reads the referenced Secret from the agent's namespace using its scoped RBAC and holds the credential in-process for the channel adapter.
4. **Rotated**: same watch-based mechanism as LLM credentials — the gateway watches the referenced Secret for changes and refreshes in-memory credentials without a restart.

Channel credentials are namespace-scoped for organizational isolation — each namespace contains only the credentials for its own agents' channels. They are created by the platform team or a provisioning service; developers do not need Secret access in their namespace.

### Protecting agent containers from LLM provider access

Because the gateway is a separate Pod in `agentry-system`, NetworkPolicy can cleanly enforce agent isolation without any per-container workarounds:

```yaml
# NetworkPolicy for agent Pods: deny all egress except to the Agentry gateway
egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: agentry-system
      podSelector:
        matchLabels:
          app.kubernetes.io/name: agentry-gateway
    ports:
      - port: 8080
  - to:                    # DNS
    - namespaceSelector: {}
    ports:
      - port: 53
```

Agent containers that attempt to call LLM providers directly are blocked at the NetworkPolicy level. No service mesh or L7-capable CNI is required for this guarantee — standard Kubernetes NetworkPolicy is sufficient because the enforcement is cross-Pod.

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

- **Egress**: by default, deny all egress except to the Agentry gateway in `agentry-system` and DNS. Because the gateway is a separate Pod (not a sidecar), this is enforceable with standard Kubernetes NetworkPolicy without requiring a service mesh. Platform team adds explicit allowlist entries for MCP servers, external APIs, etc.
- **Ingress**: by default, deny all ingress. The Service makes the agent reachable within the cluster by the gateway; the developer adds additional ingress rules if needed.
- **Inter-agent**: agents in the same namespace can talk to each other by default. Platform teams can override.

### Resource Isolation

Every Agent/AgentTask has resource limits enforced via Pod `resources.limits`. AgentClass.maxLimits sets the cap. This prevents a runaway agent from exhausting node resources.

---

## Data Flow and Audit

### What flows where

- **Agent → LLM Gateway**: prompts and completions. In-cluster Service call (cross-namespace, encrypted if mTLS/service mesh is present; plaintext within cluster by default).
- **LLM Gateway → LLM Provider**: prompts and completions over egress. Always HTTPS.
- **Channel Platform → User Gateway**: inbound user messages. HTTPS inbound to the gateway's public endpoint.
- **User Gateway → Agent**: normalized message envelope via `POST /v1/message` to the agent's ClusterIP Service.
- **Gateway → Controller**: activity timestamps and task completion status via Pod annotation writes.
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
| Malicious message from channel platform | Platform adapter authenticates inbound events (Discord signature verification, etc.) before processing |
| Agent spoofs namespace to bypass budget/access controls | Gateway identifies namespace via source IP → Pod resolution from informer cache; source IPs are assigned by the cluster network and unforgeable from within a container |
| Channel credential leaked from developer namespace | Channel credentials are developer-namespaced; blast radius is limited to that namespace and channel |

---

## Recommendations for Deployment

1. **Install Agentry in a dedicated, locked-down namespace** (`agentry-system`). Restrict who can `exec` into or modify resources in this namespace.
2. **Expose the User Gateway via a dedicated Ingress or LoadBalancer** with TLS termination. The gateway's public endpoint receives inbound platform events.
3. **Enable k8s audit logging** at the `Metadata` level minimum, `RequestResponse` for Secret access if feasible.
4. **Standard Kubernetes NetworkPolicy is sufficient** for agent egress enforcement — no service mesh required. The cluster-level gateway architecture makes egress rules cross-Pod and fully enforceable.
5. **Separate LLM credential management from platform engineering access** if possible (e.g., only a secrets-admin role can read/write credential Secrets in `agentry-system`). This requires cluster RBAC beyond Agentry's scope.
6. **Require an appropriate RuntimeClass for any AgentClass that allows LLM code execution.** Platform admins own RuntimeClass installation and compatibility validation.
7. **Review AgentClass configurations as code** (GitOps). Cluster-scoped resources that grant capabilities should never be edited by hand in production.