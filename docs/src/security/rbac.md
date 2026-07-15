# RBAC and Authentication

Agentry's RBAC model follows one rule: no component holds a standing permission it only needs occasionally, and no component holds a cluster-wide permission that a namespaced one would satisfy. This page walks through the five identities that matter (operator, gateway, platform engineer, agent developer, agent Pod) and then through how the gateway authenticates the callers that reach it.

Two Kubernetes facts shape almost every decision below, so they are worth stating up front:

- **A ClusterRole bound with a ClusterRoleBinding applies in every namespace.** There is no way to say "this grant, but only in `agentry-system`". Anything that must be namespace-limited has to ship as a separate namespaced `Role` plus `RoleBinding`. Both the operator and the gateway therefore have a ClusterRole/Role pair, not a single object.
- **RBAC `resourceNames` constrains `get`, `update`, `patch`, `delete`, and `watch`, but not `list` and not `create`.** Any grant scoped to a named object must omit those two verbs, or the scoping silently evaporates.

## Operator ServiceAccount

The operator runs under `agentry-system/agentry-controller`, with a `ClusterRole` (bound via `ClusterRoleBinding`) plus a companion namespaced `Role` in `agentry-system`. The namespace-scoped grants below are delivered as the Role, because a ClusterRole bound cluster-wide cannot be namespace-limited. Together they grant:

**Agentry CRDs.** Full access (`get, list, watch, create, update, patch, delete`) to all Agentry CRDs, plus:

- `update, patch` on their **status subresources** (`agents/status`, `agenttasks/status`, `agentchannels/status`, `modelproviders/status`, `agentclasses/status`). With the status subresource enabled, write access on the main resource does not permit status writes, so these are separate grants.
- `update` on their **finalizers subresources** (`agents/finalizers`, `agenttasks/finalizers`, and so on). The reconcilers set controller ownerRefs with `blockOwnerDeletion: true`, and clusters running the `OwnerReferencesPermissionEnforcement` admission plugin authorize that against the owner's `finalizers` subresource.

**Core workload objects.** `get, list, watch, create, update, patch, delete` on `Pods`, `PersistentVolumeClaims`, `Services`, `ConfigMaps`, `NetworkPolicies`, `ServiceAccounts` cluster-wide.

`NetworkPolicy` and `ServiceAccount` are owned per-Agent and per-AgentTask: the reconcilers synthesize one of each during provisioning and owner-reference them to the parent resource for cascade GC, so the operator must be able to create and delete them in user namespaces. `list` and `watch` are required because controller-runtime drives owned-resource reconciliation through informers. They are also required cluster-wide on `Pods` because the AgentReconciler enumerates gateway Pods in `agentry-system` for the activity fan-out (see [AgentReconciler](../controller/reconcilers.md#agentreconciler) step 8 and [Multi-replica fan-out](../gateways/user/activation-and-activity.md#activity-tracking-api)).

On clusters where FQDN egress synthesis is enabled (`allowedHosts` on a supported CNI), the ClusterRole additionally carries the CNI's policy CRD group: `ciliumnetworkpolicies.cilium.io` on Cilium, the Calico Enterprise equivalent likewise. The chart templates these rules only when that feature is switched on, so a cluster without it never grants them.

**Cluster metadata.** `get, list, watch` on `RuntimeClass`, `StorageClass`, `Namespaces` (for validation).

**Events.** `create, patch` on `Events` (for event emission).

**Leases.** `get, list, watch, create, update, patch, delete` on `Leases` in `agentry-system`. Controller-runtime's leader-election lock requires the full set; it is granted via the namespaced Role, and that is what actually restricts it to the operator's own namespace.

**Secrets.** `get, list, watch` on `Secrets` in `agentry-system` only, granted via the namespaced Role, not cluster-wide. The operator validates that ModelProvider credential Secrets exist.

In addition, the AgentChannelReconciler creates **dynamic, per-AgentChannel Roles in user namespaces** granting the operator ServiceAccount `get, watch` scoped via `resourceNames` to the Secret(s) referenced by that AgentChannel's active webhook auth config: the inbound Secret (`spec.webhook.auth.secretRef` for bearer, `spec.webhook.auth.hmac.secretRef` for HMAC) and, when `callbackUrl` is set, the outbound `spec.webhook.callbackAuth` Secret. Three details make this scoping real:

- `list` is deliberately omitted. RBAC `resourceNames` cannot constrain a plain `list` request, so the verb would be dead weight.
- Name-scoped `watch` requests must set `fieldSelector metadata.name=<secret>` to satisfy the `resourceNames` check.
- These Roles are owned by the AgentChannel and torn down on deletion, the same way the gateway-side per-channel Roles are.

This scoped read path is what the reconciler uses to verify that the configured `data` key is present in the Secret (see [AgentChannelReconciler step 3](../controller/reconcilers.md#agentchannelreconciler)). The operator has no broader Secret read access in user namespaces.

**Certificates.** `get, list, watch, create, update, patch, delete` on `cert-manager.io/v1/Certificate` in user namespaces (the per-Agent and per-AgentTask certificates live alongside the workload, not in `agentry-system`). `list` and `watch` are required so the reconciler can observe `Certificate.status.conditions[type=Ready]` before creating the dependent Pod.

**Roles and RoleBindings.** `get, list, watch, create, update, delete` on `roles` and `rolebindings` (`rbac.authorization.k8s.io`) cluster-wide. The AgentChannelReconciler and AgentTaskReconciler create the dynamic per-channel and per-task Roles/RoleBindings described above and under [Gateway ServiceAccount permissions](#gateway-serviceaccount-permissions), in whichever user namespace the owning resource lives.

Kubernetes **escalation prevention** forbids creating a Role that grants permissions the creator does not itself hold, and the per-channel Roles grant user-namespace Secret reads the operator deliberately lacks as a standing permission. The operator's ClusterRole therefore additionally carries the **`escalate`** and **`bind`** verbs on `roles`/`rolebindings`:

- `escalate` satisfies the check when creating or updating the Role itself.
- `bind` is separately required for the RoleBinding half. Kubernetes permits creating a RoleBinding that references a Role only if the creator either holds everything that Role grants or holds `bind` on it, and again, the per-channel Roles grant Secret reads the operator deliberately lacks.

This is a deliberate trade. The alternative that satisfies the escalation check without `escalate` is granting the operator standing Secret read across all user namespaces, which is strictly worse: it converts a create-time capability into an always-on read surface. Platform teams auditing RBAC should treat the operator ServiceAccount as privileged accordingly; the [threat model](threat-model.md) row on operator compromise covers what `escalate`/`bind` do and do not change.

**Unlike a sidecar model, the operator does not need cluster-wide Secret read/write access.** Credentials are held by the gateway ServiceAccount in `agentry-system` and never copied to user namespaces. This significantly reduces the operator's blast radius.

## Gateway ServiceAccount Permissions

The Agentry Gateway runs under a separate ServiceAccount, `agentry-system/agentry-gateway`, whose grants (like the operator's) split into a `ClusterRole` for cluster-wide access and a companion namespaced `Role` in `agentry-system` for namespace-scoped access.

### Namespaced grants in `agentry-system`

- `get, watch` on `Secrets` (to read LLM provider credentials).
- `get, list, watch, create, patch` on `ConfigMaps`. Read paths: internal configuration and the `_canonical` budget totals written by the operator. Write paths: each replica server-side-applies its per-replica spend partials to the `agentry-budget-{providerName}` ConfigMaps (`patch`, plus `create` for the first write of a provider's ConfigMap, see [Budget State Management](../gateways/llm/budgets-and-rate-limits.md#budget-state-management)), and the async webhook pipeline `create`s the per-request `agentry-async-{requestId}` placeholder at 202-acceptance and later `patch`es the payload in (see [Request Flow](../gateways/user/overview.md#request-flow) step 5a). `delete` is deliberately absent: cleanup of both families is controller-side (the ModelProviderReconciler prunes stale budget keys, the AgentChannelReconciler prunes and finalizer-sweeps async ConfigMaps).

### Cluster-wide grants

**`create` on `tokenreviews.authentication.k8s.io`** (cluster-scoped, no resource name needed, since `TokenReview` is a virtual resource). This permits the gateway to validate projected ServiceAccount bearer tokens presented by gateway-only-tier workloads. Without it, the gateway cannot accept non-mTLS authentication. See [LLM Gateway § Namespace Identification mode 2](../gateways/llm/workload-identity.md#mode-2-serviceaccount-bearer-token).

**`get, list, watch` on `Agent`** for provider routing: the gateway resolves the calling Pod's `ownerRef` to its `Agent` on every LLM request and reads `spec.providers` to validate the qualified `provider/model` name (see [LLM Gateway § Provider Routing](../gateways/llm/provider-routing.md)). Hibernation is detected by Service-endpoint absence in the User Gateway path, not by reading `Agent.status`, see [Activator](../gateways/user/activation-and-activity.md#the-activator).

**`get, list, watch` on `AgentTask`** for task completion handling. The gateway resolves the calling Pod's ownerRef to identify the associated AgentTask, short-circuits the `completion.condition: exitCode` case with `403 access_denied` before any patch attempt, validates that the artifact names in the completion payload match `spec.artifacts` exactly, **and** reads `status.currentPodUID` and `status.phase` to enforce the `/v1/task/complete` identity gate: `403 StalePodCompletion` when the calling Pod's UID does not match `status.currentPodUID`, `403 TaskAlreadyCompleted` when `status.phase` is terminal. See [POST /v1/task/complete](../gateways/api/task-complete.md) and [Per-Agent and Per-Task Child Resources](../runtime/child-resources.md) for the gateway/reconciler protocol that stamps these fields. The same cluster-wide `Pod`/`AgentTask` cache also backs the source-IP to Pod cross-check on every LLM request.

The gateway does **not** create the per-task completion ConfigMap or set its ownerRef. The AgentTaskReconciler creates it at task provisioning time, and the gateway holds `get, update, patch` only via the per-task name-scoped Role; `create` is intentionally excluded. See [The Agentry Gateway](../gateways/overview.md).

**`get, list, watch` on `AgentChannel`** (to look up which Agent a channel message targets and to manage platform connections), plus **`patch` on `AgentChannel`** (to write the `agentry.io/channel-disconnected` annotation during the finalizer handoff, see [Finalizers](../controller/finalizers.md)).

**`get, list, watch` on `ModelProvider`** for model validation, `allowedNamespaces` checks, budget configuration, and fallback chain resolution.

**`get, list, watch` on `AgentClass`.** The mTLS-tier provider-routing chain enforces `AgentClass.spec.allowedProviders` on every request: the gateway resolves the calling workload's `agentClassRef` (from its Agent/AgentTask cache) and checks the requested provider against the class before forwarding. See [Provider Routing](../gateways/llm/provider-routing.md). Gateway-only-tier requests skip this check, because there is no AgentClass to consult.

**`get, list, watch` on `Pods`** (to maintain the Pod informer cache used for source IP to namespace resolution on LLM requests).

**`get` on `Services`** (to resolve Agent endpoints for message delivery). RBAC cannot scope a ClusterRoleBinding-delivered grant to "user namespaces only", so this is an honest cluster-wide `get` in the ClusterRole. Service objects carry no secret material, so the extra reach is benign.

### Dynamic per-namespace grants: channel credentials

The gateway holds `get, watch` on specific Secrets in user namespaces referenced by AgentChannel webhook auth config, covering both directions:

- **Inbound** auth (`spec.webhook.auth.secretRef` for bearer, `spec.webhook.auth.hmac.secretRef` for HMAC), used to verify inbound webhook signatures from channel platforms.
- **Outbound** callback auth (`spec.webhook.callbackAuth.secretRef` / `.hmac.secretRef`), used to sign outbound callback POSTs to `callbackUrl`; required by [cross-resource validation rule 25](../resources/validation-and-defaulting.md#cross-resource-validation) whenever `callbackUrl` is set.

This is implemented via **dynamic per-namespace Roles**: when the AgentChannelReconciler creates or updates an AgentChannel, it ensures a Role and RoleBinding exist in the agent's namespace granting the gateway ServiceAccount `get, watch` `resourceNames`-scoped to the Secret(s) referenced by the active inbound and outbound auth types. When `auth` and `callbackAuth` reference the same Secret, the Role lists it once; when they differ, the Role lists both. The reconciler cleans up these Roles when the AgentChannel is deleted. The gateway does not have blanket Secret access across user namespaces.

### Dynamic per-namespace grants: task completion ConfigMaps

When the AgentTaskReconciler provisions a task, it pre-creates an empty `{taskName}-completion` ConfigMap (owned by the AgentTask) and creates a `Role` in the task's namespace granting the gateway ServiceAccount `get, update, patch` (not `create`) on that exact ConfigMap name, along with a `RoleBinding` to `agentry-system/agentry-gateway`. Both Role and RoleBinding are owned by the AgentTask (via ownerRef) and cascade-deleted on task cleanup.

Two verb choices are load-bearing:

- `get` is included because a completion write is a read-modify-write update, and `resourceNames` does constrain `get`.
- `create` is deliberately omitted because RBAC's `resourceNames` does **not** constrain `create` requests. Granting `create` on a named ConfigMap would silently widen the gateway's access to all ConfigMaps in the namespace. Pre-creating the resource and granting only `get, update, patch` is what makes the name-scoping enforceable.

This follows the same scoped-access pattern used for channel credentials above. The gateway does **not** have blanket ConfigMap write access across user namespaces.

### Summary of the gateway's reach

The gateway's LLM credential access is scoped to `agentry-system`. Channel credential access extends to user namespaces only via the dynamic per-namespace Roles, reading only the specific Secrets referenced by the AgentChannel's active webhook auth config. The `patch` permission on AgentChannel is narrowly used: the gateway writes only the `agentry.io/channel-disconnected` annotation as part of the finalizer handshake when a channel is deleted.

Async webhook response ConfigMaps (`agentry-async-{requestId}`) are stored in `agentry-system`, where the gateway already has full ConfigMap access, not in agent namespaces. That is why no additional per-channel Role is needed for async response writes. The AgentChannelReconciler prunes these ConfigMaps in `agentry-system` using label selectors (`agentry.io/channel-namespace`, `agentry.io/channel-name`) rather than ownerRefs: a cross-namespace ownerReference is invalid (the GC resolves the owner in the dependent's namespace, treats it as missing, and deletes the dependent), so linkage must be by labels, with cleanup reconciler-enforced.

Activity tracking does not require any etcd writes. The gateway maintains activity timestamps in-memory and serves them to the controller via the [activity tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api).

## Platform Engineer Role

A `ClusterRole` named `agentry-platform-admin`, assigned via `ClusterRoleBinding` to users/groups who should manage platform-level configuration, with:

- Full access to `AgentClass` and `ModelProvider`.
- `get, list, watch` on `Agent`, `AgentTask` cluster-wide (for observability).

Secret management is deliberately **not** part of this ClusterRole. A grant delivered via ClusterRoleBinding applies in every namespace, so putting Secret CRUD here would silently hand platform engineers cluster-wide Secret access. Instead the chart ships a companion namespaced Role, `agentry-secrets-admin` (`create, get, update, delete` on Secrets), instantiated with a RoleBinding in `agentry-system` (to manage LLM credentials) and in each designated agent namespace (to provision channel credentials referenced by AgentChannel webhook auth config).

## Agent Developer Role

A `Role` (namespaced) named `agentry-developer` with:

- Full access to `Agent`, `AgentTask`, and `AgentChannel` in their namespace.
- `get` on Pods, PVCs, Services, ConfigMaps in their namespace.
- `get` on Events in their namespace.
- `create` on `pods/exec` for debugging (optional, platform team decides).

A namespaced Role can never grant access to cluster-scoped resources, so catalog visibility ships separately: a small ClusterRole `agentry-catalog-reader` (`get, list` on `AgentClass` and `ModelProvider`, read-only, because developers need to know what is available to reference), bound via `ClusterRoleBinding` to developer groups.

No access to Secrets, no ability to create/modify AgentClass or ModelProvider, no access to other namespaces.

## Agent Pod ServiceAccount

Each Agent Pod runs with a ServiceAccount **distinct** from both the operator and the developer. The AgentReconciler creates this ServiceAccount at provisioning time as an owned child resource: a `ServiceAccount` named `agent-{agentName}`, in the Agent's namespace, owner-referenced to the Agent so cascade deletion cleans it up. No RoleBindings are attached by default, so the agent has no access to the Kubernetes API. The AgentTaskReconciler creates an analogous `task-{taskName}` ServiceAccount for AgentTask Pods with the same defaults.

If an agent needs cluster API access (for example, a Kubernetes-administering agent), the developer or platform team must explicitly create a Role and RoleBinding against the per-agent ServiceAccount. This is opt-in, not default. See [AgentReconciler](../controller/reconcilers.md#agentreconciler) step 6 and [AgentTaskReconciler](../controller/reconcilers.md#agenttaskreconciler) step 4 for the convergence step.

## Agent to Gateway Authentication

The gateway supports **two authentication modes** for inbound requests, mapped to the two Helm tiers. The full request-time mechanics (SAN parsing, the Agentry-managed label set, TokenReview call shape, and cache behaviour) are documented once in [Namespace Identification](../gateways/llm/workload-identity.md); what follows is the security reasoning behind them.

### Mode 1: mTLS client certificate (Agentry-managed Pods)

This is the only authentication path available to Pods created by the AgentReconciler (Agent / AgentTask workloads). Agents present the cert-manager-issued certificate at `$AGENTRY_TLS_CERT` as a client cert on the LLM Gateway's TLS listener. The gateway verifies it against `agentry-ca` and extracts (namespace, agent name) from the SAN (`{name}.{namespace}.svc.cluster.local`). Identity is cryptographically attested, and the CA private key is not reachable from any agent Pod. See [Mode 1](../gateways/llm/workload-identity.md#mode-1-mtls-client-certificate).

**Agent/AgentTask Pods must use mTLS: their ServiceAccount tokens are not accepted by the gateway.** This is a deliberate asymmetry. Accepting SA tokens from Agentry-managed Pods would create two attack paths (cert path plus token path), and a compromised agent could use the token path as a second credential after its cert-based access is contained. Rejecting SA tokens keeps that tier's credential surface to a single artifact: a bounded-lifetime (90d default `notAfter`), namespace-pinned client cert.

Be precise about what that buys you: this is containment, not revocation. Re-issuing a leaf does nothing to the old one (there is no CRL or OCSP, and Go's `crypto/tls` performs no revocation checking), so a leaked cert and key stay valid until their `notAfter` regardless of any rotation. A known-compromised leaf is invalidated only by the [CA re-key runbook](tls.md#in-cluster-tls) or by waiting out `notAfter`. Clusters that need a tighter compromise bound should shorten the per-Agent `Certificate` `duration`.

### Mode 2: projected ServiceAccount bearer token via TokenReview (gateway-only tier)

This mode exists for existing workloads in user namespaces that the platform team has granted LLM-provider access to without adopting the Agent CRD. The caller mounts a projected ServiceAccount token with audience `agentry-gateway` and sends it as `Authorization: Bearer <token>`; the gateway validates it against the API server via `TokenReview` and derives the namespace from the authenticated username. Validation results are cached for a bounded TTL derived from the token's own expiry. See [Mode 2](../gateways/llm/workload-identity.md#mode-2-serviceaccount-bearer-token) for the exact call, parsing, and cache rules.

Three properties of this mode are security-critical:

**The mTLS tier is exclusive, and a precheck enforces it.** Before validating any token, the gateway resolves the request's source IP to a Pod via its informer cache and rejects the request with `401 Unauthorized` if the Pod has an `ownerRef` to an `Agent` or `AgentTask` resource or carries the Agentry-managed label set. This is what prevents an Agentry-managed Pod from falling back to bearer-token auth and reopening the second attack path Mode 1 closes. The precheck is **not cached**: it re-runs on every request.

**Audience binding is critical.** The gateway requests the `agentry-gateway` audience in its `TokenReview`, so a generic `kubernetes.default.svc`-audience token (such as a stolen kubelet token) cannot authenticate. Callers must configure their projected-volume `audience: agentry-gateway`.

**Cache TTL is bounded by the token, not by the API.** `TokenReviewStatus` returns no expiry field, so the gateway derives the TTL from the token's own `exp` claim (parsed without signature verification, which is safe because the API server has already authenticated the token and `exp` only bounds cache lifetime), with a 60s safety margin and a 5 minute cap. Opaque non-JWT tokens get the fixed 5 minute cap.

### Secondary cross-check (both modes)

The gateway resolves the source IP to a Pod via its Pod informer and confirms the Pod is in the namespace identity resolution produced. This closes a stolen-credential scenario where a cert or token from one Pod is presented from a different Pod. Cross-check failure means the request is rejected with `401 unauthorized`, using the same envelope as the [LLM Gateway 401 row](../gateways/api/errors.md#llm-gateway-error-responses).

For [`POST /v1/task/complete`](../gateways/api/task-complete.md) specifically, the gateway falls back to a live API-server `List Pods` (in the cert-SAN-derived namespace, filtered by source IP) before declaring cross-check failure. This avoids leaking the new-Pod informer-lag race as a terminal `401` instead of the retryable `403 StalePodCompletion` documented at that endpoint. Other endpoints rely on the informer cache only: heartbeats are periodic and recover on the next tick, and LLM-proxy callers retry their request normally.

### Client cert presentation

Starter templates (see [Starter Templates](../runtime/starter-templates.md)) configure mTLS client-cert presentation and the cert-file watch-and-reload pattern. Custom Agent/AgentTask images must present `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY` on every agent-to-gateway call: LLM requests, task completion, and (Agents only) heartbeats. Activity timestamps and heartbeats are tracked in-memory in the gateway, so no etcd writes are involved in agent-to-gateway communication.

## Internal Endpoint Authentication

Three endpoints are for Agentry's own components, not for agents or users: the controller's `POST /v1/activate/{namespace}/{agentName}`, and the gateway's `GET /v1/activity` and `GET /v1/channels/health`. All three authenticate callers **via mTLS with SAN-based authorization**. There is no separate shared-secret layer on top of TLS.

- **Activator** (gateway to controller): the controller's TLS listener on port 9443 is shared with kubelet's `/healthz` and `/readyz` probes, so it is configured with `ClientAuth: tls.VerifyClientCertIfGiven`. Cert-less probes can complete the handshake, while per-path HTTP middleware enforces mTLS-with-SAN on `/v1/activate`. The gateway presents `agentry-gateway-tls` as its client cert; the controller verifies it against `agentry-ca` and authorizes the request only if the cert's SAN matches the gateway Service DNS (`agentry-gateway.agentry-system.svc.cluster.local` or `.svc`). Any other SAN is rejected as `403 Forbidden`. A request missing the cert entirely is rejected as `401 Unauthorized` at the handler.
- **Activity API and Channel Health** (controller to gateway): the gateway's TLS listener on port 8443 requires a client certificate on `/v1/activity` and `/v1/channels/health`. The controller presents `agentry-controller-tls`; the gateway verifies against `agentry-ca` and authorizes only if the SAN matches the controller Service DNS (`agentry-controller.agentry-system.svc.cluster.local` or `.svc`).

Both peer certificates are issued by cert-manager from `agentry-ca-issuer`, live only in `agentry-system`, and rotate continuously via cert-manager. There is no Secret-based shared key to rotate separately. The gateway cert declares `usages: [server auth, client auth]` (the gateway also dials the controller); the controller cert declares the same pair (the controller also dials the gateway activity API). See [In-cluster TLS](tls.md#in-cluster-tls) for the trust chain.

**Authorization is by SAN, not by mere possession of a cert signed by `agentry-ca`.** Per-Agent certs are signed by the same CA but have different SANs (`{name}.{namespace}.svc.cluster.local`), so a compromised agent cannot present its own cert to reach either internal endpoint: the authorization layer rejects the SAN before the handler runs.

Both `/v1/activity` and `/v1/channels/health` are served on the gateway's `:8443` listener with TLS configured as `ClientAuth: tls.VerifyClientCertIfGiven`, so token-auth callers on adjacent paths can complete the TLS handshake without a client cert (see [Per-path client-auth enforcement](../gateways/llm/listener-tls.md#per-path-client-auth-enforcement)). The path handlers explicitly require a client cert with the controller SAN before invoking any business logic: a missing cert returns `401`, a non-matching SAN returns `403`. The TLS-handshake check alone is not sufficient, since the listener accepts cert-less connections for the gateway-only tier.
