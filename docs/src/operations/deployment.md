# Deployment

This page covers how Kaalm is packaged and installed: the Helm chart contents, the cert-manager and trust-manager certificate inventory, the NetworkPolicy CNI prerequisite, the two-tier on-ramp (gateway only, or the full agent lifecycle), and the upgrade path. For the system topology and component responsibilities, see [System architecture](../concepts/system-architecture.md).

Kaalm has three hard prerequisites, none of which the chart installs for you:

1. **cert-manager**, for TLS lifecycle management. It must run with `--enable-certificate-owner-ref=true`; [In-cluster TLS](../security/tls.md#in-cluster-tls) states why, and as shipped nothing checks the flag.
2. **trust-manager**, to project the Kaalm CA into every namespace.
3. **A CNI that enforces NetworkPolicy** (see [Network policy prerequisite](#network-policy-prerequisite)).

## Helm chart contents

The chart targets the `kaalm-system` namespace. Install with `--namespace kaalm-system --create-namespace`.

![What the Helm chart installs, by where each object lands: the three prerequisites it does not install; the six CRDs, two ClusterIssuers, two ClusterRoles with bindings, and the standard AgentClass at cluster scope; the two Deployments with PodDisruptionBudgets, two Services, ServiceAccounts, Roles, leaf Certificates, and the session-key Secret in kaalm-system; the console objects when enabled; and the CA Certificate and Bundle in the cluster resource namespace.](../diagrams/helm-install-inventory.svg)

The figure is an inventory. How the cert-manager objects chain together is drawn once, on [Trust chain](../security/tls.md#trust-chain). The leader-election Lease is not in the chart: controller-runtime creates it at runtime.

### CRDs

The six CRDs (AgentClass, ModelProvider, ToolProvider, Agent, AgentTask, AgentChannel) ship in the chart's `crds/` directory. Helm applies that directory on `helm install` and **never touches it on `helm upgrade`**, so CRD schema changes need an explicit step ([Rolling upgrade order](#rolling-upgrade-order)). Keeping CRDs in `crds/` rather than `templates/` also means `helm uninstall` never deletes them, and deleting a CRD would cascade-delete every resource of that kind cluster-wide. Each CRD serves `v1beta1` (the storage version) and the deprecated `v1alpha1`, and carries the conversion stanza that points the apiserver at the controller; see [API versioning and deprecation](api-versioning.md).

### The two Deployments

The chart installs the operator Deployment (with RBAC, ServiceAccount, and leader election) and the Kaalm Gateway Deployment (with its own RBAC and ServiceAccount). Both share the same availability settings, except that pod anti-affinity is set for the operator only:

| Setting | Value |
|---|---|
| Default replicas | `2` (`controller.replicas`, `gateway.replicas`) |
| PodDisruptionBudget | `minAvailable: 1` |
| Rolling update | `maxUnavailable: 1` |
| Anti-affinity (controller only) | Preferred, `topologyKey: kubernetes.io/hostname`: the scheduler spreads the replicas across nodes when it can, and both can share a node on a single-node cluster |
| Container resources | Requests `cpu: 100m`, `memory: 128Mi`; limit `memory: 512Mi`; no CPU limit, so both run as `Burstable` (`controller.resources`, `gateway.resources`) |

Both Deployments have a **hard floor of 2 replicas**, enforced by a Helm `fail` template guard that aborts rendering when the value drops below 2. An operator cannot accidentally go under it.

The floor is operational, not correctness-driven, on both components:

- **Controller.** Leader election picks one active replica and the second is a warm standby. The activator handler runs on every replica ([Control plane](../concepts/system-architecture.md#control-plane)), so the second replica keeps wake-on-demand reachable while the first drains. The PDB guards the same thing: without it a multi-node drain or an autoscaler downscale could evict both replicas at once, which surfaces as `controller_unavailable` 504s for every in-flight webhook to a hibernated Agent ([The activator](../gateways/user/activation-and-activity.md#the-activator)).
- **Gateway.** At one replica, `minAvailable: 1` blocks every voluntary eviction and a `maxUnavailable: 1` rolling update has no headroom, so a chart upgrade would take the gateway offline for LLM and webhook traffic. The multi-replica state model ([Multi-replica state](../gateways/overview.md#multi-replica-state)) degrades to one replica without loss, so correctness is not the reason for the floor.

With `console.enabled`, the chart adds a third, optional Deployment, `kaalm-console`, outside these settings: one replica, no PodDisruptionBudget, no floor. See the `console.enabled` note under [Configuration reference](#configuration-reference).

### Configuration reference

This table is the canonical list of Kaalm's Helm values. Every tunable named elsewhere in this book resolves to a row here.

| Value | Default | What it does |
|---|---|---|
| `controller.replicas` | `2` | Operator replica count. Rendering fails below `2`. |
| `gateway.replicas` | `2` | Gateway replica count. Rendering fails below `2`. |
| `controller.image.repository` / `.tag` / `.pullPolicy` | `ghcr.io/win07xp/kaalm-controller`, appVersion, `IfNotPresent` | The controller image. An empty tag resolves to the chart's `appVersion`. |
| `gateway.image.repository` / `.tag` / `.pullPolicy` | `ghcr.io/win07xp/kaalm-gateway`, appVersion, `IfNotPresent` | The gateway image. |
| `gateway.maxFallbackDepth` | `3` | Maximum fallback chain depth for LLM provider routing. Passed to the gateway as its `--max-fallback-depth` flag. See [Fallback logic](../gateways/llm/fallback.md). |
| `gateway.trustClusterCAForUpstream` | `false` | Also trust the cluster CA (`kaalm-ca`, already mounted) for upstream provider TLS, added to the system roots. Enables in-cluster or self-hosted providers whose HTTPS endpoint is served with a `kaalm-ca-issuer` certificate. Distinct from the operator-supplied `kaalm-upstream-ca` bundle; see [Upstream TLS configuration](../gateways/llm/provider-routing.md#upstream-tls-configuration). |
| `gateway.upstreamCA.configMap` | `""` | Name of an operator-supplied ConfigMap of additional CA certificates to trust for upstream provider TLS (the `kaalm-upstream-ca` bundle). The chart mounts it and points the gateway at it. Composes with `trustClusterCAForUpstream`: enabling both merges the cluster CA and this bundle into one trust pool. |
| `gateway.upstreamCA.key` | `ca.crt` | Key within that ConfigMap holding the PEM bundle. |
| `gateway.trustClusterCAForCallbacks` | `false` | Also trust the cluster CA for `AgentChannel.spec.webhook.callbackUrl` TLS, so async responses can be delivered to in-cluster or self-hosted receivers served with a `kaalm-ca-issuer` certificate. Public receivers need only the system roots. |
| `gateway.callbackUrl.allowlist` | `[]` | List of DNS-name suffixes or CIDR blocks whose `AgentChannel.spec.webhook.callbackUrl` targets are permitted despite the deny-internal default. Loopback, link-local and the cloud-metadata IPs stay refused even when listed. |
| `controller.networkPolicy.dnsSelector` | `{ namespaceLabels: { "kubernetes.io/metadata.name": "kube-system" }, podLabels: { "k8s-app": "kube-dns" } }` | Selectors for the DNS egress rule on every synthesized per-agent NetworkPolicy. As shipped the value is accepted but not applied: the rule allows port 53 to every Pod in `kube-system`. |
| `controller.trustClusterCAForProbes` | `false` | Also trust the cluster CA (`kaalm-ca`, already mounted) for ModelProvider and ToolProvider health probes, added to the system roots. The probe-side mirror of `gateway.trustClusterCAForUpstream`: enable both so an in-cluster provider under a `kaalm-ca-issuer` certificate is both forwarded to and probed `Healthy`. |
| `controller.probeCA.configMap` | `""` | Name of an operator-supplied ConfigMap of additional CA certificates to trust for health probes, mirroring `gateway.upstreamCA`. Composes with `trustClusterCAForProbes` into one additive pool; the controller re-reads it when it rotates. |
| `controller.probeCA.key` | `ca.crt` | Key within that ConfigMap holding the PEM bundle. |
| `controller.maxConcurrentReconciles` | `4` | Reconciles the Agent, AgentChannel, and AgentTask controllers may each run at once. controller-runtime serializes per object at any setting; this lets different objects reconcile in parallel. |
| `controller.certificate.duration` | `2160h` | `spec.duration` of every per-workload Certificate (`{name}-tls` for each Agent and AgentTask), as a Go duration. A leaked certificate stays valid until it expires, so a shorter value narrows that window. Applies to Certificates created after the change. See [Rotation defaults](../security/tls.md#rotation-defaults). |
| `controller.certificate.renewBefore` | `720h` | `spec.renewBefore` of every per-workload Certificate. Must be shorter than `controller.certificate.duration`, or the controller exits at startup. |
| `controller.logLevel` | `info` | Controller log level, passed as `--zap-log-level`: `debug`, `info`, or `error`. The chart also passes `--zap-encoder=json`. See [Logging](observability.md#logs). |
| `controller.client.qps` / `.burst` | `20`, `30` | The controller's Kubernetes API client rate limit per replica: sustained requests per second and the burst above it. Passed as `--client-qps` and `--client-burst`. |
| `controller.resources` | requests `cpu: 100m`, `memory: 128Mi`; limits `memory: 512Mi` | Resource requests and limits for the controller container, passed verbatim. The default sets no CPU limit. |
| `controller.pprofPort` | `0` | Port for a `net/http/pprof` listener on the controller, for profiling under load. `0` keeps it off. Unauthenticated and never behind a Service; reach it with a port-forward. See [Profiling](observability.md#profiling). |
| `gateway.externalHostnames` | `[]` | Additional DNS names appended to the `kaalm-gateway-tls` Certificate's SAN list. |
| `gateway.channelHealthWindow` | `5m` | Rolling window over which the gateway evaluates `AgentChannel.status.conditions[type=PlatformConnected]`. |
| `gateway.platforms.discord.apiBaseUrl` | `https://discord.com/api/v10` | Base URL the Discord adapter replies through. Operator-set and trusted like a ModelProvider endpoint: not subject to the `callbackUrl` deny ranges, `http://` accepted, so an e2e mock can stand in. |
| `gateway.platforms.whatsapp.apiBaseUrl` | `https://graph.facebook.com/v23.0` | Base URL the WhatsApp adapter replies through, including the Graph API version segment. Same trust as the Discord value. |
| `gateway.agentDeliveryConnectTimeout` | `1s` | Bounds the TCP connect attempt when delivering `POST /v1/message` to an Agent Service. |
| `gateway.syncDeliveryDeadline` | `30s` | Bounds total sync-mode wall-clock (wake plus delivery retries plus agent processing). Exceeded gives `504 sync_deadline_exceeded`. See [Request flow step 6a](../gateways/user/overview.md#request-flow). |
| `gateway.providerFirstByteTimeout` | `120s` | Bounds each upstream LLM-provider attempt. The chart passes it to the gateway as `--upstream-timeout`, and the bound covers the whole call, streams included. |
| `gateway.agentReadTimeout` | `10s` | Bounds the per-attempt read of an agent's `/v1/message` response. |
| `gateway.callbackReadTimeout` | `10s` | Bounds the per-attempt read of a callback receiver's response. |
| `gateway.agentDeliveryRetryBackoff` | `1s,5s,25s` | Backoff schedule for the agent-delivery pipeline (4 attempts total). |
| `gateway.callbackRetryBackoff` | `1s,5s,25s` | Backoff schedule for the callback-delivery pipeline (4 attempts total). Reused by the async response-`Patch` pipeline. |
| `gateway.maxResponseBodyBytes` | `900Ki` | Caps an agent's webhook response, uniformly in sync and async modes. Over-cap gives `response_too_large`. |
| `gateway.maxMessageBodyBytes` | `1Mi` | Caps inbound webhook bodies on `:8080`. Over-cap POSTs get `413` at the listener level, before path resolution and auth. See [Request flow step 2](../gateways/user/overview.md#request-flow). |
| `gateway.maxLLMRequestBodyBytes` | `4Mi` | Caps inbound LLM-proxy request bodies on `:8443`. Over-cap gives `413 request_too_large` before namespace identification. See [LLM proxy endpoints](../gateways/api/overview.md#llm-proxy-endpoints). |
| `gateway.healthPort` | `8081` | Port for the gateway's internal kubelet-probe listener (`/healthz`, `/readyz`; TLS, no client auth). See [Gateway readiness](../gateways/llm/operations.md#gateway-readiness). |
| `gateway.tracing.otlpEndpoint` | `""` | OTLP/HTTP base URL the gateway exports trace spans to (for example `http://collector.monitoring.svc:4318`). Empty means tracing is off entirely: no tracer installed, no trace context created or forwarded. An `https` endpoint is verified against the gateway's upstream trust pool. See [Tracing](observability.md#tracing). |
| `gateway.tracing.sampleRatio` | `1.0` | Parent-based head sampling ratio for traces the gateway starts; propagated sampling decisions are honored either way. |
| `gateway.logLevel` | `info` | Gateway log level, passed as `--log-level`: `debug`, `info`, `warn`, or `error`. See [Logging](observability.md#logs). |
| `gateway.client.qps` / `.burst` | `100`, `200` | The gateway's Kubernetes API client rate limit per replica, passed as `--client-qps` and `--client-burst`. Higher than the controller's because live reads on the request path (task-completion cross-checks, first-use Secret loads) must not queue behind the limiter. |
| `gateway.resources` | requests `cpu: 100m`, `memory: 128Mi`; limits `memory: 512Mi` | Resource requests and limits for the gateway container, passed verbatim. The default sets no CPU limit. |
| `gateway.pprofPort` | `0` | Port for a `net/http/pprof` listener on the gateway, for profiling under load. `0` keeps it off. Unauthenticated and never behind a Service; reach it with a port-forward. See [Profiling](observability.md#profiling). |
| `standardAgentClass.enabled` | `true` | Templates the sample `standard` AgentClass described under [Sample resources](#sample-resources). Set `false` to ship your own classes only. |
| `standardAgentClass.allowedProviders` | `[]` | ModelProvider names the `standard` class admits, one string per entry, rendered into `spec.allowedProviders`. Empty allows none ([rule 5](../resources/validation-and-defaulting.md#cross-resource-validation)), so an Agent that names a provider is `Degraded` until you set this. A name with no ModelProvider behind it leaves the class `Ready=False` with `InvalidReference` ([AgentClassReconciler](../controller/reconcilers.md#agentclassreconciler)), so set it once the providers exist. |
| `console.enabled` | `false` | Installs the optional [operator console](../console/overview.md): the `kaalm-console` Deployment, Service, RBAC, and certificate. Off renders none of them. |
| `console.image.repository` / `.tag` / `.pullPolicy` | `ghcr.io/win07xp/kaalm-console`, appVersion, `IfNotPresent` | The console image. |
| `console.healthPort` | `8081` | Port for the console's kubelet-probe listener (`/healthz`, `/readyz`; TLS, no client auth). |
| `console.logLevel` | `info` | Console log level, passed as `--log-level`: `debug`, `info`, `warn`, or `error`. |
| `console.resources` | `{}` | Resource requests and limits for the console container, passed verbatim. |
| `certManager.clusterResourceNamespace` | `"cert-manager"` | Namespace holding the CA `Certificate` and `kaalm-ca` Secret. Must match your cert-manager and trust-manager deployment. See [Certificate lifecycle](#certificate-lifecycle). |
| `trustManager.bundleSelector` | `{}` | Object with `matchLabels` or `matchExpressions`, passed verbatim as the `kaalm-ca` `Bundle`'s `target.namespaceSelector`. Empty projects into every namespace. A selector must still match `kaalm-system` ([Trust bundle projection](../security/tls.md#trust-bundle-projection)). |

The values that need more than a sentence of explanation follow.

**`gateway.callbackUrl.allowlist`.** Leaving it unset preserves the default: `https://` only, with loopback, link-local, RFC1918, unique-local IPv6, and cloud-metadata IPs denied. When you set it, the [AgentChannelReconciler](../controller/reconcilers.md#agentchannelreconciler) and the gateway's delivery-time re-check admit only hosts matching one of the configured entries. Entries parse strictly at startup: an entry containing `/` must be a valid CIDR, a bare IP address means that one address, and anything else must be a DNS-name suffix. A malformed entry fails startup for both binaries instead of leaving the allowlist half-applied. See [Cross-resource validation rule 22](../resources/validation-and-defaulting.md#cross-resource-validation).

**`console.enabled`.** The console Deployment is fixed at one replica with no PodDisruptionBudget and no replica floor: login sessions are held in memory, so a second replica would break logins rather than add availability, and a read surface carries no wake-on-demand style dependency. The chart deliberately ships no `console.replicas` knob. Exposure is also deliberate: no Ingress and no LoadBalancer are templated; operators reach it by port-forward or front it themselves. See [Console overview](../console/overview.md).

**`controller.networkPolicy.dnsSelector`.** The object takes the form `{ namespaceLabels: {...}, podLabels: {...} }` and supplies the `namespaceSelector` and `podSelector` for the DNS egress rule. The default matches kubeadm, EKS, GKE, AKS, and the upstream CoreDNS chart. As shipped the value is accepted but not applied: the synthesized rule has a namespace selector for `kube-system` only. See [Protecting agent containers from LLM provider access](../security/credentials.md#protecting-agent-containers-from-llm-provider-access).

**`gateway.externalHostnames`.** Required when the User listener is exposed through a TLS pass-through Ingress; [Where the listener's TLS material comes from](../gateways/listener-tls.md#where-the-listeners-tls-material-comes-from) states why. Changing it re-issues `kaalm-gateway-tls`.

**`gateway.upstreamCA.configMap`.** The optional `kaalm-upstream-ca` ConfigMap ([Upstream TLS configuration](../gateways/llm/provider-routing.md#upstream-tls-configuration)) holds operator-supplied CA certificates: you create it, and the chart mounts it when you set this value to its name. The chart never templates its contents. It is projected next to the cluster CA under a distinct filename, because a projected volume cannot concatenate two ConfigMap keys into one file; the gateway merges every configured path into one trust pool and re-reads them when they rotate.

**`gateway.platforms.*.apiBaseUrl`.** The two platform adapters reply through these URLs and nowhere else. They are chart values rather than AgentChannel fields on purpose: a tenant's channel Secret holds that tenant's platform credentials, and the gateway presents them only to the host the operator configured, so no channel can redirect a bearer token. Change the WhatsApp value to move to a newer Graph API version; the Discord value rarely changes. Both values must parse as `http(s)://host[/path]` with no query or fragment; a malformed value fails gateway startup instead of surfacing as delivery failures. See [Reply delivery](../gateways/user/platform-adapters.md#reply-delivery).

**`gateway.channelHealthWindow`.** Within the window, a channel reports `True` if any inbound request succeeded, `False` if the window holds only failures, and `Unknown` (`reason=NoRecentTraffic`) if no observations exist on a replica that has been up the full window. Tune it larger for low-traffic webhooks to avoid spurious `Unknown` flapping, and smaller for high-traffic channels where staler "last success" data is undesirable. See [Channel health tracking](../gateways/user/platform-adapters.md#channel-health-tracking).

**`gateway.agentDeliveryConnectTimeout`.** On iptables-mode kube-proxy, a hibernated Agent's empty Service fails fast with a TCP reset, and this timeout is unused. On IPVS, Cilium kube-proxy replacement, and eBPF data paths that drop packets to empty-endpoint ClusterIPs, it is the per-wake latency cap before the gateway falls through to the activator wake. See [Child resources](../runtime/child-resources.md) and [Activator](../gateways/user/activation-and-activity.md#the-activator).

**`gateway.providerFirstByteTimeout`.** The per-attempt timeout behind the fallback table's "timeout before any response bytes" trigger and the `504 provider_timeout` exhaustion mapping ([Fallback logic](../gateways/llm/fallback.md)). The chart passes the value to the gateway as `--upstream-timeout`. The bound covers the whole upstream call through the last response byte, streams included. The default is generous because a provider can take a minute or more to the first byte on a long prompt, and the worst-case wait before an error is `maxFallbackDepth` times the bound.

**The read timeouts and retry backoffs.** `gateway.agentDeliveryRetryBackoff` and `gateway.callbackRetryBackoff` are independently tunable; they merely share a default. Together with the read timeouts they define the retry-budget wall-clock derived in [Async webhook responses](../gateways/api/async-responses.md).

**`gateway.maxResponseBodyBytes`.** The `900Ki` default sits well below the Kubernetes ~1 MiB ConfigMap object cap, so async responses fit in the per-request `kaalm-async-{requestId}` ConfigMap with envelope headroom. It also bounds the gateway's per-request memory footprint while it buffers the response in either mode. The payload is stored as text in the ConfigMap's `data` field (a JSON envelope), never in `binaryData`: base64 encoding would inflate 900Ki past the object cap and silently break the headroom math. Over-cap responses return `response_too_large` to the caller (sync) or to `callbackUrl` / the polling endpoint (async). See [Gateway overview](../gateways/overview.md) and [Request flow](../gateways/user/overview.md#request-flow).

### Services

The chart installs two ClusterIP `Service`s (three with `console.enabled`). Every certificate SAN and every internal endpoint address uses these names:

| Service | Ports |
|---|---|
| `kaalm-gateway` | `:8080` user listener, `:8443` LLM/internal mTLS listener, `:9090` metrics |
| `kaalm-controller` | `:9443` activator, `:9444` conversion webhook, `:8080` metrics |
| `kaalm-console` (optional) | `:8443` pages and read API |

`$KAALM_GATEWAY_ENDPOINT` and every internal call in the other pages assume these names in `kaalm-system`.

### The session-key Secret

The chart installs a Secret named `kaalm-gateway-session-key` holding a random key the gateway reads as `KAALM_MCP_SESSION_KEY` to mint MCP session ids ([The broker](../gateways/tool-plane.md#the-broker)). The template looks up the live Secret and reuses its value, so open MCP sessions survive a `helm upgrade`. Deleting the Secret and upgrading rotates the key and invalidates every open session. A pipeline that renders with `helm template` and applies the output has no live Secret to look up, so it rotates the key on every apply.

### Metrics

Prometheus metrics are served on dedicated plain-HTTP ports: controller `:8080/metrics` and gateway `:9090/metrics`, both unauthenticated. The chart ships no `ServiceMonitor` or `PodMonitor` and no NetworkPolicy for `kaalm-system`, so scrape integration and a policy that admits only the Prometheus Pods are the platform team's ([Metrics](observability.md#metrics), [Recommendations for deployment](../security/model.md#recommendations-for-deployment)). Three Grafana dashboards ship as JSON in `config/grafana/` ([Dashboards](observability.md#dashboards)).

### Sample resources

- A single default AgentClass (`standard`, `standardAgentClass.enabled`) that platform teams can customize or delete. It carries `helm.sh/resource-policy: keep`, so an uninstall does not orphan running Agents, and setting `standardAgentClass.enabled: false` on an upgrade leaves the live object in place. It leaves `runtimeClassName` unset, so Pods run under the cluster's default runtime ([RuntimeClass](../security/model.md#runtimeclass)).

  It grants persistence (5Gi by default, 50Gi ceiling) and hibernation, because its lifecycle timings govern nothing on a class that permits neither ([rules 24, 26, 29](../resources/validation-and-defaulting.md#cross-resource-validation)). What it cannot decide is the provider list: the chart installs no ModelProvider, and a class naming one that does not exist is `Ready=False`, so `spec.allowedProviders` is the install-time value `standardAgentClass.allowedProviders` and is empty until you set it.

  Its fields are written in the API's canonical serialization (durations as `30m0s`, not `30m`). That is the rule for any custom resource a chart authors: the controller's reconcile-time defaulting re-serializes what it reads, a field whose value changes in the process moves to the controller's field manager, and Helm's server-side apply would then conflict with the controller on the next upgrade. A value that round-trips byte-identical keeps shared ownership.
- No sandboxed class. A class that pins the gVisor [`RuntimeClass`](../security/model.md#runtimeclass) as a live default would put any Agent that selected it into [`Degraded`](../controller/agent-lifecycle.md) on clusters without gVisor, so operators author one after confirming the matching `RuntimeClass` is installed. The e2e suite's `test/e2e/testdata/sandboxed-class.yaml` is a reference manifest.
- No ModelProvider. Credentials are yours, so the chart ships none; the repository's `config/samples/` directory holds a starting manifest for each of the six kinds.

## Certificate lifecycle

cert-manager and trust-manager are required dependencies, and the chart ships only the `ClusterIssuer`, `Certificate`, and `Bundle` objects Kaalm needs. [In-cluster TLS](../security/tls.md#in-cluster-tls) specifies the trust chain those objects form, the rotation defaults, the re-key runbook, and the failure modes. This page keeps the inventory and the chart values that bind it.

### Resource inventory

| Resource | Name | Namespace | Purpose |
|---|---|---|---|
| `ClusterIssuer` | `kaalm-selfsigned` | cluster-scoped | Issues the CA `Certificate` |
| `Certificate` | `kaalm-ca` | `certManager.clusterResourceNamespace` | The Kaalm root; cert-manager writes the `kaalm-ca` Secret beside it |
| `ClusterIssuer` | `kaalm-ca-issuer` | cluster-scoped | Signs every leaf from the `kaalm-ca` Secret |
| `Certificate` | `kaalm-gateway-tls` | `kaalm-system` | The gateway's serving certificate for both listeners and its client certificate for the activator |
| `Certificate` | `kaalm-controller-tls` | `kaalm-system` | The controller's activator and conversion listeners, and its client certificate for the gateway; the CRDs' `caBundle` is injected from it |
| `Certificate` | `kaalm-console-tls` | `kaalm-system`, with `console.enabled` | The console's serving and client certificate |
| `Certificate` | `{name}-tls`, per Agent and per AgentTask | the workload's namespace | Created by the reconcilers, not the chart ([Lifecycle of an Agent TLS serving certificate](../security/tls.md#lifecycle-of-an-agent-tls-serving-certificate)) |
| `Bundle` | `kaalm-ca` | cluster-scoped, sources read from the trust namespace | Projects the CA as the `kaalm-ca` ConfigMap into every namespace |

### Chart values that bind the PKI

- `certManager.clusterResourceNamespace` must name the namespace both controllers resolve cluster-scoped Secrets in (cert-manager's `--cluster-resource-namespace` and trust-manager's `--trust-namespace`, both `cert-manager` by default). A mismatch fails issuance cluster-wide with `SecretNotFound` ([Trust chain](../security/tls.md#trust-chain)).
- `trustManager.bundleSelector` narrows the projection. Empty, the default, projects into every namespace. A selector must still match `kaalm-system`, or the gateway and controller lose their trust material ([Trust bundle projection](../security/tls.md#trust-bundle-projection)).
- `gateway.externalHostnames` extends the gateway certificate's SANs for a TLS pass-through Ingress ([TLS and Ingress](../gateways/user/overview.md#tls-and-ingress)).

## Network policy prerequisite

A CNI that enforces NetworkPolicy is a required prerequisite alongside cert-manager and trust-manager: the synthesized per-workload policies are inert without one. See [What the synthesized NetworkPolicy protects](../runtime/child-resources.md#what-the-synthesized-networkpolicy-protects) and [Network policy](../security/model.md#network-policy).

## Tiered on-ramp

A platform team can use the gateway before adopting the agent lifecycle. [Adoption tiers](../concepts/tenancy-and-tiers.md#adoption-tiers) defines the two tiers; this section gives the install steps for each. For teams arriving with existing framework agents (LangGraph, LangChain), the user guide's Running framework agents page maps both tiers onto that starting point, with worked examples under `examples/langgraph-*/`.

### Tier 1: gateway only

Install the chart, configure a [ModelProvider](../resources/modelprovider.md), and point existing workloads at the gateway for LLM traffic and spend tracking. No AgentClass, Agent, AgentTask, or AgentChannel resources need to be created.

Existing workloads authenticate with a projected ServiceAccount token ([Mode 2](../gateways/llm/workload-identity.md#mode-2-serviceaccount-bearer-token)). Kaalm does not mutate their Pods, so three things are on the workload owner:

1. **Project the token** with a `serviceAccountToken` volume whose `audience` is `kaalm-gateway`. The default `kubernetes.default.svc` audience is rejected with `401`.
2. **Supply the gateway URL** in the manifest: `https://kaalm-gateway.kaalm-system.svc:8443`. The controller injects `$KAALM_GATEWAY_ENDPOINT` only into Pods it creates.
3. **Trust the CA** by mounting the `kaalm-ca` ConfigMap trust-manager projects into the namespace and pointing the HTTP client at it.

Access control in this tier is `ModelProvider.spec.allowedNamespaces` plus `spec.models`; there is no AgentClass to consult ([Adoption tiers](../concepts/tenancy-and-tiers.md#adoption-tiers)).

### Tier 2: full agent lifecycle

Configure [AgentClasses](../resources/agentclass.md), deploy Agents and AgentTasks with hibernation and wake-on-demand, and connect them to user-facing channels through AgentChannels. Channel integration is included in this tier because wake-on-demand requires a channel to be fully testable.

Kaalm-managed Pods authenticate with the per-workload certificates cert-manager issues, and the gateway enforces the full routing chain: the Agent's providers, the class `allowedProviders`, then the provider's `allowedNamespaces` and `models` ([Provider routing and adapters](../gateways/llm/provider-routing.md)).

## Upgrade and migration

This section gives the operational order of an upgrade and the chart-level notes. The API versions, the conversion webhook, storage-version migration, the window between the two upgrade steps, and the deprecation policy are specified on [API versioning and deprecation](api-versioning.md).

### Rolling upgrade order

**Apply the CRDs first.** `helm upgrade` never touches `crds/` ([CRDs](#crds)), so every chart upgrade starts with:

```bash
kubectl apply --server-side --force-conflicts -f charts/kaalm/crds/
```

`--force-conflicts` is part of the command because Helm is the field manager for every CRD field from the install. When a release adds an API version, this step adds it and its conversion stanza, and the window between it and the `helm upgrade` has a bounded cost, stated under [Upgrading in place](api-versioning.md#upgrading-in-place).

**Apply order is not a rollout barrier.** Helm's kind-sorted apply does not help here: the controller and gateway Deployments are applied together and roll **concurrently**, so there is no ordering guarantee between the controller observing new CRD fields and the gateway consuming what the controller writes (status fields, budget `_canonical`, per-task RBAC). Version-skew tolerance, not apply order, is what makes the rollout safe. Both Deployments roll with `maxUnavailable: 1` under their PDBs, so one replica of each stays serving throughout: wake-on-demand and the LLM proxy remain available across the upgrade.

**Version-skew tolerance is one chart version, and only for the duration of an in-progress rollout.** The internal contracts (activity and channel-health response bodies, the budget ConfigMap layout, the activator wire contract, the per-request async ConfigMap labels) evolve additively within a minor version, so a mixed old and new controller and gateway pair works mid-rollout. Running mixed versions as a steady state is unsupported.

### CRD schema evolution

Within a served version, changes are additive only: new optional fields with reconcile-time defaulting (per [Defaulting](../resources/validation-and-defaulting.md#defaulting)), new condition reasons, new enum values. Anything breaking ships as a new API version with conversion in both directions, never in place. The full rule set, the `v1alpha1` window, and the conversion mechanics are on [Deprecation policy](api-versioning.md#deprecation-policy).

### Helm chart upgrades

Values whose change has workload-visible effects are the ones to review before an upgrade:

| Value | Effect on change |
|---|---|
| `gateway.replicas` | Rate-limit buckets re-divide on the next refill cycle. See [Rate limiting](../gateways/llm/budgets-and-rate-limits.md#rate-limiting). |
| `gateway.channelHealthWindow` | Changes `PlatformConnected` flapping behavior. |
| The body-size caps | In-flight requests sized between the old and new caps change fate. |
| `gateway.externalHostnames` | Re-issues `kaalm-gateway-tls`; the gateway re-reads it on the next handshake. |
| `trustManager.bundleSelector` | A selector that stops matching `kaalm-system` leaves both Deployments without trust material. |
| `certManager.clusterResourceNamespace` | Moves the CA `Certificate`; issuance fails until the Secret exists in the new namespace. |
| `standardAgentClass.enabled: false` | The live `standard` AgentClass stays, because of its `keep` policy. |
| Deleting `kaalm-gateway-session-key` before the upgrade | Rotates the MCP session key and invalidates every open session ([The session-key Secret](#the-session-key-secret)). |

The chart's `fail` template guards (the `replicas ≥ 2` floors) abort rendering on known-bad values.

A gateway rollout also resets in-memory activity state: expect idle and hibernation transitions to defer for `idleTimeout` afterwards, per [Activity detection](../controller/hibernation-and-wake.md#activity-detection). Schedule chart upgrades accordingly on clusters with multi-hour idle timeouts.

### ModelProvider credential rotation, end-to-end

No Pod restarts are needed at any step, in either tier.

1. The platform engineer updates the credential Secret in `kaalm-system`.
2. The gateway's Secret watch refreshes in-memory credentials without a restart (see [Lifecycle of an LLM API key](../security/credentials.md#lifecycle-of-an-llm-api-key)). In-flight requests complete on the old key.
3. The next ModelProviderReconciler health probe validates the new key, when `healthCheck.enabled` is on (the default): a bad rotation surfaces as `Ready=False, reason=CredentialsInvalid` within one probe interval (default 60s). With the probe off nothing re-validates the key, because the reconciler watches no Secrets ([ModelProviderReconciler](../controller/reconcilers.md#modelproviderreconciler), step 1). Live traffic that hits an upstream `401` or `403` emits a `CredentialsInvalid` Warning on the provider and falls back ([Fallback triggers](../gateways/llm/fallback.md#fallback-triggers)).
4. Audit. The Secret update lands in the Kubernetes audit log (enable `RequestResponse` level for `kaalm-system` Secrets per [Recommendations for deployment](../security/model.md#recommendations-for-deployment)); confirm cut-over on the provider's own key-usage dashboard.

### Breaking spec changes

A breaking spec change ships as a new API version with conversion, so an upgrade never replaces existing objects ([Deprecation policy](api-versioning.md#deprecation-policy)). The replace-not-migrate procedure (let in-flight AgentTasks run to completion or delete them, hibernate or delete Agents, apply the new CRDs, then re-apply updated manifests) is the path for a fresh install or a rollback across the graduation.

Agent state survives a replace through the PVC path: `pvcRetention: Retain` on delete, or snapshot and remount through [`persistence.existingClaim`](../resources/agent.md).

Channel receivers see a bounded gap. Webhook paths return `401` while the AgentChannel is absent (indistinguishable from an unregistered path, per the [401 contract](../gateways/api/channel-webhook.md)), and stored async responses survive independently in `kaalm-system` until their 1-hour TTL, unless the channel is deleted, in which case the finalizer sweeps them (see [Finalizers](../controller/finalizers.md)).
