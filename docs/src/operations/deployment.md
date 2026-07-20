# Deployment

This page covers how Agentry is packaged and installed: the Helm chart contents, the cert-manager and trust-manager certificate inventory, the NetworkPolicy CNI prerequisite, the two-tier on-ramp (gateway-only vs full agent lifecycle), and the upgrade path. For the system topology and component responsibilities, see [System Architecture](../concepts/system-architecture.md).

Agentry has three hard prerequisites, none of which the chart installs for you:

1. **cert-manager**, for TLS lifecycle management. It must run with `--enable-certificate-owner-ref=true`; see [In-cluster TLS](../security/tls.md#in-cluster-tls) for why.
2. **trust-manager**, to project the Agentry CA into user namespaces.
3. **A NetworkPolicy-enforcing CNI** (see [Network Policy Prerequisite](#network-policy-prerequisite)).

## Helm Chart Contents

The chart targets the `agentry-system` namespace. Install with `--namespace agentry-system --create-namespace`.

![A deployment inventory of the Helm chart, organised by where each object lands. A red frame at the top holds the three hard prerequisites the chart does not install: the cert-manager controller, the trust-manager controller, and a NetworkPolicy-enforcing CNI. A cluster-scoped frame holds the five CRDs from the chart's crds/ directory, which helm install applies and helm upgrade never touches, the two ClusterIssuers, the ClusterRoles and ClusterRoleBindings, and the sample standard AgentClass. An agentry-system frame holds the controller and gateway Deployments with their replica floors, PDBs and controller-only anti-affinity, the two ClusterIP Services with their ports, the ServiceAccounts and namespaced Roles, the leader-election Lease, and the gateway and controller leaf Certificates. A cert-manager frame holds the agentry-ca Certificate and Secret and the trust-manager Bundle. A user namespace frame holds the projected agentry-ca ConfigMap.](../diagrams/helm-install-inventory.svg)

**Reading the diagram.** It answers "what lands where", not "how the pieces relate". The three dashed grey boxes in the red frame are prerequisites: everything else is chart-installed. The trust chain those cert-manager objects form is drawn once, on [In-cluster TLS](../security/tls.md#trust-chain).

### CRDs

The five CRDs (AgentClass, ModelProvider, Agent, AgentTask, AgentChannel) ship in the chart's `crds/` directory. Helm applies that directory on `helm install` and **never touches it on `helm upgrade`**, so CRD schema changes need an explicit step. See [Upgrade and Migration](#upgrade-and-migration).

### The two Deployments

The chart installs the operator Deployment (with RBAC, ServiceAccount, and leader election) and the Agentry Gateway Deployment (with its own RBAC and ServiceAccount). Both share the same availability settings, except that pod anti-affinity is specified for the operator only:

| Setting | Value |
|---|---|
| Default replicas | `2` (`controller.replicas`, `gateway.replicas`) |
| PodDisruptionBudget | `minAvailable: 1` |
| Rolling update | `maxUnavailable: 1` |
| Anti-affinity (controller only) | `topologyKey: kubernetes.io/hostname`, so the two replicas land on different nodes |

Both Deployments have a **hard floor of 2 replicas**, enforced by a Helm `fail` template guard that aborts rendering when the value drops below 2. An operator cannot accidentally go under it.

The floor is operational, not correctness-driven, on both components:

- **Controller.** Leader election picks one active replica; the second is a warm standby for failover. Critically, the second replica also serves the activator endpoint, because the activator handler runs on **every** replica (see [Control Plane](../concepts/system-architecture.md#control-plane)), so two replicas keep the activator reachable across voluntary disruptions. Leader election and the activator both work with one replica, but a single-replica controller breaks wake-on-demand availability during drains and single-replica involuntary failures, which contradicts the "hard control-plane dependency" framing in [The Agentry Gateway](../gateways/overview.md). The PDB matters for the same reason: without it, a multi-node drain or an autoscaler downscale could evict both replicas simultaneously, surfacing as `controller_unavailable` 504s for any in-flight webhook to a hibernated Agent (see [Activator](../gateways/user/activation-and-activity.md#the-activator) step 5).
- **Gateway.** At one replica, `minAvailable: 1` blocks all voluntary eviction (node drains stall) and `maxUnavailable: 1` rolling updates have no headroom, so chart upgrades would briefly take the gateway offline for both LLM proxy and inbound webhook traffic. The multi-replica state model in [The Agentry Gateway](../gateways/overview.md) (spend ConfigMap exchange, divide-by-replicas rate buckets, controller activity fan-out) degrades gracefully to one replica, so correctness is not the reason for the floor.

### Configuration reference

This table is the canonical list of Agentry's Helm values. Every tunable named elsewhere in this book resolves to a row here.

| Value | Default | What it does |
|---|---|---|
| `controller.replicas` | `2` | Operator replica count. Rendering fails below `2`. |
| `gateway.replicas` | `2` | Gateway replica count. Rendering fails below `2`. |
| `gateway.maxFallbackDepth` | `3` | Maximum fallback chain depth for LLM provider routing. Sets `AGENTRY_MAX_FALLBACK_DEPTH` on the gateway Deployment. See [Fallback Logic](../gateways/llm/fallback.md). |
| `gateway.callbackUrl.allowlist` | unset | List of DNS-name suffixes or CIDR blocks. When set, **replaces** the default deny-internal rule for `AgentChannel.spec.webhook.callbackUrl`. |
| `controller.networkPolicy.dnsSelector` | `{ namespaceLabels: { "kubernetes.io/metadata.name": "kube-system" }, podLabels: { "k8s-app": "kube-dns" } }` | Selectors for the DNS egress rule on every synthesized per-agent NetworkPolicy. |
| `gateway.externalHostnames` | unset | Additional DNS names appended to the `agentry-gateway-tls` Certificate's SAN list. |
| `gateway.channelHealthWindow` | `5m` | Rolling window over which the gateway evaluates `AgentChannel.status.conditions[type=PlatformConnected]`. |
| `gateway.agentDeliveryConnectTimeout` | `1s` | Bounds the TCP connect attempt when delivering `POST /v1/message` to an Agent Service. |
| `gateway.syncDeliveryDeadline` | `30s` | Bounds total sync-mode wall-clock (wake plus delivery retries plus agent processing). Exceeded gives `504 sync_deadline_exceeded`. See [Request Flow step 6a](../gateways/user/overview.md#request-flow). |
| `gateway.providerFirstByteTimeout` | `120s` | Bounds each upstream LLM-provider attempt from connection start through the first response byte, and the idle gap between SSE chunks. |
| `gateway.agentReadTimeout` | `10s` | Bounds the per-attempt read of an agent's `/v1/message` response. |
| `gateway.callbackReadTimeout` | `10s` | Bounds the per-attempt read of a callback receiver's response. |
| `gateway.agentDeliveryRetryBackoff` | `1s,5s,25s` | Backoff schedule for the agent-delivery pipeline (4 attempts total). |
| `gateway.callbackRetryBackoff` | `1s,5s,25s` | Backoff schedule for the callback-delivery pipeline (4 attempts total). Reused by the async response-`Patch` pipeline. |
| `gateway.maxResponseBodyBytes` | `900Ki` | Caps an agent's webhook response, uniformly in sync and async modes. Over-cap gives `response_too_large`. |
| `gateway.maxMessageBodyBytes` | `1Mi` | Caps inbound webhook bodies on `:8080`. Over-cap POSTs get `413` at the listener level, before path resolution and auth. See [Request Flow step 2](../gateways/user/overview.md#request-flow). |
| `gateway.maxLLMRequestBodyBytes` | `4Mi` | Caps inbound LLM-proxy request bodies on `:8443`. Over-cap gives `413 request_too_large` before namespace identification. See [LLM Proxy Endpoints](../gateways/api/overview.md#llm-proxy-endpoints). |
| `gateway.healthPort` | `8081` | Port for the gateway's internal kubelet-probe listener (`/healthz`, `/readyz`; TLS, no client auth). See [Gateway Readiness](../gateways/llm/operations.md#gateway-readiness). |
| `certManager.clusterResourceNamespace` | `"cert-manager"` | Namespace holding the CA `Certificate` and `agentry-ca` Secret. Must match your cert-manager and trust-manager deployment. See [Certificate Lifecycle](#certificate-lifecycle). |
| `trustManager.bundleSelector` | unset | Object with `matchLabels` / `matchExpressions`, passed verbatim into the `agentry-ca` `Bundle`'s `target.namespaceSelector`. |

The values that need more than a sentence of explanation follow.

**`gateway.callbackUrl.allowlist`.** Leaving it unset preserves the default: `https://` only, with loopback, link-local, RFC1918, unique-local IPv6, and cloud-metadata IPs denied. When you set it, the [AgentChannelReconciler](../controller/reconcilers.md#agentchannelreconciler) and the gateway's delivery-time re-check admit only hosts matching one of the configured entries. See [Cross-Resource Validation rule 22](../resources/validation-and-defaulting.md#cross-resource-validation).

**`controller.networkPolicy.dnsSelector`.** The object has the shape `{ namespaceLabels: {...}, podLabels: {...} }` and supplies the `namespaceSelector` and `podSelector` for the DNS egress rule. The default matches kubeadm, EKS, GKE, AKS, and the upstream CoreDNS chart. Override it for clusters that run DNS in a non-standard namespace or with custom labels. See [Protecting agent containers from LLM provider access](../security/credentials.md#protecting-agent-containers-from-llm-provider-access).

**`gateway.externalHostnames`.** Required when the User Gateway is exposed via TLS pass-through Ingress, so that external clients see a cert whose SAN matches the public hostname they dialed. Backend re-encrypt Ingress works without it, because the Ingress controller dials the in-cluster Service DNS, which is already in the default SAN set. See [TLS and Ingress](../gateways/user/overview.md#tls-and-ingress).

**`gateway.channelHealthWindow`.** Within the window, a channel reports `True` if any inbound request succeeded, `False` if the window holds only failures, and `Unknown` (`reason=NoRecentTraffic`) if no observations exist on a replica that has been up the full window. Tune it larger for low-traffic webhooks to avoid spurious `Unknown` flapping, and smaller for high-traffic channels where staler "last success" data is undesirable. See [Channel Health Tracking](../gateways/user/platform-adapters.md#channel-health-tracking).

**`gateway.agentDeliveryConnectTimeout`.** On iptables-mode kube-proxy, hibernated Agents fail-fast via RST and this timeout is essentially unused. On IPVS, Cilium kube-proxy replacement, and eBPF data paths that drop packets to empty-endpoint ClusterIPs, it is the per-wake latency cap before the gateway falls through to the activator wake. See [Per-Agent and Per-Task Child Resources](../runtime/child-resources.md) and [Activator](../gateways/user/activation-and-activity.md#the-activator).

**`gateway.providerFirstByteTimeout`.** This is the per-attempt timeout behind the fallback table's "timeout before any response bytes" trigger and the `504 provider_timeout` exhaustion mapping. The same value also applies mid-stream as an idle-bytes bound between SSE chunks, so an upstream that stalls without closing cannot hold gateway and agent connections open indefinitely. The default is deliberately generous, because LLM providers can legitimately take a minute-plus to first byte on long prompts, and it bounds the worst-case pre-error wait at `maxFallbackDepth × providerFirstByteTimeout`. See [Fallback Logic](../gateways/llm/fallback.md).

**The read timeouts and retry backoffs.** `gateway.agentDeliveryRetryBackoff` and `gateway.callbackRetryBackoff` are independently tunable; they merely share a default. Together with the read timeouts they define the retry-budget wall-clock derived in [Async Webhook Response](../gateways/api/async-responses.md).

**`gateway.maxResponseBodyBytes`.** The `900Ki` default sits well below the Kubernetes ~1 MiB ConfigMap object cap, so async responses fit in the per-request `agentry-async-{requestId}` ConfigMap with envelope headroom. It also bounds the gateway's per-request memory footprint while it buffers the response in either mode. The payload is stored as text in the ConfigMap's `data` field (a JSON envelope), never in `binaryData`: base64 encoding would inflate 900Ki past the object cap and silently break the headroom math. Over-cap responses return `response_too_large` to the caller (sync) or to `callbackUrl` / the polling endpoint (async). See [The Agentry Gateway](../gateways/overview.md) and [Request Flow](../gateways/user/overview.md#request-flow).

### Services

The chart installs two ClusterIP `Service`s. The whole SAN and endpoint design hangs on their stable DNS names:

| Service | Ports |
|---|---|
| `agentry-gateway` | `:8080` user listener, `:8443` LLM/internal mTLS listener, `:9090` metrics |
| `agentry-controller` | `:9443` activator, `:8080` metrics |

Every certificate SAN, every `$AGENTRY_GATEWAY_ENDPOINT` value, and every internal RPC in the other docs assumes these names in `agentry-system`.

The optional `agentry-upstream-ca` ConfigMap ([Upstream TLS Configuration](../gateways/llm/provider-routing.md#upstream-tls-configuration)) is operator-supplied, not chart-templated. The chart only documents its name.

### Metrics

Prometheus metrics are served on dedicated ports: controller `:8080/metrics` and gateway `:9090/metrics`, both unauthenticated in-cluster. The chart does **not** ship `ServiceMonitor` or `PodMonitor` manifests; scrape integration (and a NetworkPolicy admitting only the Prometheus ServiceAccount, if desired) is left to the platform team. See [Metrics](observability.md#metrics).

### Sample resources

- A single default AgentClass (`standard`) that platform teams can customize or delete. It leaves `runtimeClassName` unset, so Pods run under the cluster's default container runtime (runc in practice). Stock clusters define no `RuntimeClass` objects at all, so pinning a named one (even `runc`) would fail Pod admission with "RuntimeClass not found" everywhere it isn't explicitly created.
- A `sandboxed` example manifest (gVisor [`RuntimeClass`](../security/model.md#runtimeclass)) in the chart's `examples/` directory. Operators apply it after confirming the matching `RuntimeClass` is installed on the cluster. Shipping it as a live default would put any Agent that selected it into [`Degraded`](../controller/agent-lifecycle.md) on clusters without gVisor.
- Optionally, a sample ModelProvider manifest stub (keys not included) as a starting template.

## Certificate Lifecycle

**cert-manager and trust-manager are required dependencies.** The chart does not install the cert-manager or trust-manager controllers themselves, so teams with an existing cert-manager deployment reuse them. It ships the `ClusterIssuer`, `Certificate`, and `Bundle` resources Agentry needs. The trust chain those resources form, and the mTLS topology built on it, are described in [In-cluster TLS](../security/tls.md#in-cluster-tls); this page covers only the resource inventory and the operational constraints. The [chart inventory figure](#helm-chart-contents) above shows where each of the resources below lands.

Admission webhooks are not used. The cert-manager dependency is solely for TLS lifecycle management.

### Resource inventory

| Resource | Name | Purpose |
|---|---|---|
| `ClusterIssuer` (self-signed) | `agentry-selfsigned` | Creates the `Certificate` for the Agentry CA. |
| `ClusterIssuer` (CA) | `agentry-ca-issuer` | Sources the `agentry-ca` Secret and signs all Agentry-issued leaf certs. |
| `Certificate` | `agentry-gateway-tls` | Gateway serving cert, used by both listeners. |
| `Certificate` | `agentry-controller-tls` | The controller's activator endpoint. |
| `Certificate` (one per Agent) | per-Agent | Created by the [AgentReconciler](../controller/reconcilers.md#agentreconciler) at provisioning time, owned by the Agent via ownerRef. See [Lifecycle of an agent TLS serving certificate](../security/tls.md#lifecycle-of-an-agent-tls-serving-certificate). |
| `Certificate` (one per AgentTask) | per-AgentTask | Created by the [AgentTaskReconciler](../controller/reconcilers.md#agenttaskreconciler) at provisioning time, owned by the AgentTask via ownerRef. See [Lifecycle of an AgentTask TLS client certificate](../security/tls.md#lifecycle-of-an-agenttask-tls-client-certificate). |
| `Bundle` (trust-manager) | `agentry-ca` | Projects the Agentry CA as a ConfigMap into every non-system user namespace. |

A `ClusterIssuer` (rather than a namespaced `Issuer`) is used for `agentry-ca-issuer` so that per-namespace `Certificate` resources can reference the same signing key.

### The cluster resource namespace constraint

The CA `Certificate` and the `agentry-ca` Secret it writes live in cert-manager's **cluster resource namespace** (Helm value `certManager.clusterResourceNamespace`, default `"cert-manager"`), **not** in `agentry-system`. This is not a stylistic choice:

- A CA `ClusterIssuer` resolves `spec.ca.secretName` only in that namespace (cert-manager's `--cluster-resource-namespace` flag).
- trust-manager likewise reads `Bundle` sources only from its trust namespace (`--trust-namespace`, default `cert-manager`).

Operators who run either controller with a non-default namespace must set the value to match, or issuance fails cluster-wide with `SecretNotFound`.

### The gateway serving cert

`agentry-gateway-tls` serves both gateway listeners: the LLM listener on port 8443 and the User listener on port 8080, from the same cert. **Despite the conventional HTTP association of port 8080, the User listener is TLS-only.** An Ingress fronting it must use HTTPS as its backend protocol. External webhook traffic arrives via Ingress configured for backend re-encrypt (or TLS pass-through); there is no plaintext listener on the gateway. See [TLS and Ingress](../gateways/user/overview.md#tls-and-ingress).

### CA projection into user namespaces

The `agentry-ca` `Bundle` projects the CA into every non-system user namespace, including future ones added after install. Agent and AgentTask Pods mount the resulting ConfigMap at `/var/run/agentry/ca.crt` to verify the gateway's TLS cert. Platform teams that need a tighter projection override `trustManager.bundleSelector`.

## Network Policy Prerequisite

**An NP-enforcing CNI is a required prerequisite** alongside cert-manager and trust-manager. See the NetworkPolicy bullet under [Per-Agent and Per-Task Child Resources](../runtime/child-resources.md) and [Network Policy](../security/model.md#network-policy).

## Tiered On-Ramp

The Helm chart supports a tiered on-ramp, so a platform team can get value from the gateway before adopting the agent lifecycle.

### Tier 1: gateway only

Install the chart, configure a [ModelProvider](../resources/modelprovider.md), and point existing workloads at the gateway for LLM traffic and spend tracking. No AgentClass, Agent, AgentTask, or AgentChannel resources need to be created.

Existing workloads authenticate to the gateway using their own projected ServiceAccount tokens. No client certificate is required in this tier (see [Agent→Gateway Authentication](../security/rbac.md#agent-to-gateway-authentication) Mode 2 and [Namespace Identification](../gateways/llm/workload-identity.md)). Three things are on the workload owner in this tier:

1. **Mint the token for the right audience.** The token **must** be minted for audience `agentry-gateway` via a `serviceAccountToken` projected volume (`audience: agentry-gateway`). The gateway's `TokenReview` names that audience, so a Pod's default `kubernetes.default.svc`-audience token is rejected. A workload that skips this step gets `401` on every call.
2. **Supply the gateway URL.** Because Agentry does not mutate non-managed Pods in this tier, the workload manifest must hard-code or template the URL itself. `https://agentry-gateway.agentry-system.svc:8443` is the in-cluster Service DNS. The controller injects `$AGENTRY_GATEWAY_ENDPOINT` only into full-lifecycle-tier Pods.
3. **Trust the CA.** Existing workloads must mount the `agentry-ca` ConfigMap (projected by trust-manager into every non-system namespace) and configure their HTTP client to trust it. Otherwise calls to the gateway fail TLS verification.

Access control in this tier is governed by `ModelProvider.spec.allowedNamespaces` plus `spec.models` only. AgentClass `allowedProviders` does not apply, because there is no Agent resource to reconcile against. Platform teams who need class-scoped provider policy must use the full lifecycle tier.

### Tier 2: full agent lifecycle

Configure [AgentClasses](../resources/agentclass.md), deploy Agents and AgentTasks with hibernation and wake-on-demand, and connect them to user-facing channels via AgentChannels (webhook in v1). Channel integration is included in this tier because wake-on-demand requires a channel to be fully testable.

Agentry-managed Pods authenticate via mTLS using per-agent certificates issued by cert-manager. The LLM Gateway enforces the full routing chain (Agent → AgentClass `allowedProviders` → ModelProvider `allowedNamespaces`/`models`) for this tier. See [Provider Routing](../gateways/llm/provider-routing.md).

## Upgrade and Migration

This section is scoped to what `v1alpha1` promises. Multi-version CRD conversion machinery (conversion webhooks, storage-version migration, deprecation windows) is deliberately deferred until the API graduates past alpha: while `v1alpha1` is the only served and stored version there is nothing for such machinery to convert, and shipping it early would contradict the no-admission-webhook posture in [Operator Structure](../controller/overview.md) for zero benefit.

### Rolling upgrade order

**Apply the CRDs first.** CRDs live in the chart's `crds/` directory, which `helm install` applies but `helm upgrade` **never touches**. CRD schema changes must be applied explicitly:

```bash
kubectl apply --server-side -f crds/
```

That command is the documented first step of every chart upgrade, before `helm upgrade`. Keeping CRDs in `crds/` rather than `templates/` also means `helm uninstall` never deletes them, which matters because deleting a CRD cascade-deletes every CR of that kind cluster-wide.

**Apply order is not a rollout barrier.** Helm's kind-sorted apply does not help here: the controller and gateway Deployments are applied together and roll **concurrently**, so there is no ordering guarantee between the controller observing new CRD fields and the gateway consuming what the controller writes (status fields, budget `_canonical`, per-task RBAC). Version-skew tolerance, not apply order, is what makes the rollout safe. Both Deployments roll with `maxUnavailable: 1` under their PDBs, so one replica of each stays serving throughout: wake-on-demand and the LLM proxy remain available across the upgrade.

**Version-skew tolerance is one chart version, and only for the duration of an in-progress rollout.** The internal contracts (activity and channel-health response bodies, the budget ConfigMap shape, the activator wire contract, the per-request async ConfigMap labels) evolve additively within a minor version, so a mixed old/new controller↔gateway pair works mid-rollout. Running mixed versions as a steady state is unsupported.

### CRD schema evolution (within v1alpha1)

Changes are additive-only: new optional fields with reconcile-time defaulting (per [Defaulting](../resources/validation-and-defaulting.md#defaulting)), new condition reasons, new enum values. Anything breaking ships as a replacement alpha version in a new chart release, with the migration documented in that release's notes. The alpha contract is `kubectl get -o yaml` → adapt → re-apply, not automated conversion. `v1` API stability is explicitly not a goal for the initial release (see [Resource Overview](../resources/overview.md)).

### Helm chart upgrades

Values whose change has workload-visible effects are the ones to review before an upgrade:

| Value | Effect on change |
|---|---|
| `gateway.replicas` | Rate-limit buckets re-divide on the next refill cycle. See [Rate Limiting](../gateways/llm/budgets-and-rate-limits.md#rate-limiting). |
| `controller.networkPolicy.dnsSelector` | Regenerates every synthesized per-agent NetworkPolicy. |
| `gateway.channelHealthWindow` | Changes `PlatformConnected` flapping behavior. |
| The body-size caps | In-flight requests sized between the old and new caps change fate. |

The chart's `fail` template guards (the `replicas ≥ 2` floors) abort rendering on known-bad values.

A gateway rollout also resets in-memory activity state: expect idle and hibernation transitions to defer for `idleTimeout` afterwards, per [Activity Detection](../controller/hibernation-and-wake.md#activity-detection). Schedule chart upgrades accordingly on clusters with multi-hour idle timeouts.

### ModelProvider credential rotation, end-to-end

No Pod restarts are needed at any step, in either tier.

1. The platform engineer updates the credential Secret in `agentry-system`.
2. The gateway's Secret watch refreshes in-memory credentials without a restart (see [Lifecycle of an LLM API key](../security/credentials.md#lifecycle-of-an-llm-api-key)). In-flight requests complete on the old key.
3. The next ModelProviderReconciler health probe validates the new key. A bad rotation surfaces as `Ready=False, reason=CredentialsInvalid` within one probe interval (default 60s), plus a `Warning` event from any live traffic that hits upstream `401`/`403` and falls back (see [Fallback triggers](../gateways/llm/fallback.md#fallback-triggers)).
4. Audit. The Secret update lands in the Kubernetes audit log (enable `RequestResponse` level for `agentry-system` Secrets per [Recommendations](../security/model.md#recommendations-for-deployment)); confirm cut-over on the provider's own key-usage dashboard.

### Breaking spec changes (within alpha)

Breaking Agent, AgentTask, and AgentChannel spec changes are replace-not-migrate: let in-flight AgentTasks run to completion (or delete them), hibernate or delete Agents, apply the new CRDs, then re-apply updated manifests.

Agent state survives the replace through the PVC path: `pvcRetention: Retain` on delete, or snapshot-and-remount via [`persistence.existingClaim`](../resources/agent.md).

Channel receivers see a bounded gap. Webhook paths return `401` while the AgentChannel is absent (indistinguishable from an unregistered path, per the [401 contract](../gateways/api/channel-webhook.md)), and stored async responses survive independently in `agentry-system` until their 1-hour TTL, unless the channel is deleted, in which case the finalizer sweeps them (see [Finalizers](../controller/finalizers.md)).
