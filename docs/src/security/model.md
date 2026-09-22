# Security model and isolation

This part of the book defines Kaalm's security design: who trusts whom, how agent Pods are isolated, what traffic crosses which boundary, what the audit trail records, and how to deploy Kaalm safely. This page is the map. Each mechanism it names is specified once, on its own page: [RBAC and authentication](rbac.md) for the ServiceAccounts, their grants, and how callers authenticate; [Credential handling](credentials.md) for the lifecycle of every secret Kaalm touches; [TLS and certificates](tls.md#in-cluster-tls) for the trust chain; and [Threat model](threat-model.md) for the threats and the decision on each.

## Trust model

Kaalm assumes four trust tiers:

1. **Cluster administrator**: trusted to install Kaalm, manage CRDs, and deploy the operator.
2. **Platform engineer**: trusted to create AgentClasses, ModelProviders, and ToolProviders, and to manage credentials. Keep this role distinct from agent developers.
3. **Agent developer**: trusted to deploy workloads in their namespace within the guardrails the platform team sets. Not trusted with credentials or cross-namespace access.
4. **Agent container**: not trusted. Even a developer-authored agent may execute LLM-generated code, so the container is treated as adversarial.

Most controls on the following pages exist because of tier 4.

## Isolation

Isolation is layered. The RuntimeClass decides how strongly the kernel is separated from the container, the Pod security context constrains what the container may ask the kernel for, NetworkPolicy constrains what it can reach, and resource limits constrain what it can consume. All four come from the AgentClass ([AgentClass](../resources/agentclass.md)).

### RuntimeClass

An AgentClass may set `spec.runtime.runtimeClassName`, naming a Kubernetes `RuntimeClass` that must already exist on the cluster; the controller copies it onto every Pod of that class. Pod admission fails with `RuntimeClass not found` otherwise. Stock clusters (kubeadm, EKS, GKE, AKS) define none, and there is no built-in RuntimeClass named `runc`: leaving the field unset is what selects the cluster's default runtime. The chart's `standard` class leaves it unset.

| `runtimeClassName` | Isolation | Use when |
|---|---|---|
| unset (default) | The cluster's default container runtime, runc in practice | The agent only calls APIs and runs no generated code |
| `gvisor` or `runsc` | Userspace kernel with syscall filtering | The agent executes untrusted code |
| `kata` | A lightweight VM per Pod | Strong multi-tenancy is required |
| `firecracker` (through Kata or Agent Sandbox) | microVM isolation | The strongest available boundary is required |

Platform teams create one AgentClass per isolation tier (`standard` with the field unset, `sandboxed` requiring gVisor, and so on), and developers pick a class.

### Pod Security Standards

The Pod and container security contexts of every Kaalm-created Pod are exactly what its AgentClass declares in `spec.security.podSecurityContext` and `spec.security.containerSecurityContext`. The controller applies no defaults of its own, and the chart's `standard` class declares none, so a Pod under that class runs with the cluster's defaults. A class that must satisfy the `restricted` Pod Security Standard declares the block shown on [AgentClass](../resources/agentclass.md#spec). Pod Security Admission on the workload namespaces enforces the standard on the namespace, outside the class; as shipped, Kaalm emits no warning for a class that declares less.

### Network policy

The controller synthesizes one NetworkPolicy per Agent and per AgentTask from the class's `spec.network` fields, plus a CiliumNetworkPolicy for `allowedHosts` on Cilium; [What the synthesized NetworkPolicy protects](../runtime/child-resources.md#what-the-synthesized-networkpolicy-protects) specifies the object. The policy denies both directions and then allows:

- **Egress** to the gateway Pods in `kaalm-system` on the cluster listener port, and DNS on port 53 to the cluster DNS Pods (`controller.networkPolicy.dnsSelector`, by default `k8s-app: kube-dns` in `kube-system`). Because the gateway is a separate Pod and not a sidecar, standard Kubernetes NetworkPolicy enforces the rule on every CNI that implements it; no service mesh is needed. MCP tool servers are reached through the gateway's [tool plane](../gateways/tool-plane.md), so tools need no per-agent egress.
- **Egress** to each CIDR in `spec.network.egress.allowedCIDRs`, on every port. This is the portable escape hatch for direct external access.
- **Egress** to each host in `spec.network.egress.allowedHosts`, on every port, when the CNI is Cilium. Standard NetworkPolicy has no hostname rule, so the controller writes these hosts into a second policy, a `CiliumNetworkPolicy` named `{name}-fqdn` with a `toFQDNs` rule ([FQDN egress policy](../runtime/child-resources.md#fqdn-egress-policy)). On any other CNI the hosts are ignored, and the AgentClass reports it with `FQDNPolicySupported=False` and a `Warning` event ([rule 20](../resources/validation-and-defaulting.md#cross-resource-validation)).
- **Ingress** from the gateway Pods on the agent's health port, which carries `POST /v1/message`. The agent-side mTLS check on that path ([The runtime contract](../runtime/contract.md), item 4) is a second layer under the policy.
- **Ingress** from the other agent Pods in the same namespace, on the agent's health port, when the class sets `spec.network.allowSameNamespaceIngress: true`. The rule selects the `kaalm.io/workload: agent` label, so a Pod Kaalm does not manage never reaches the agent, and the port list keeps the opt-in to delivery traffic. The default is off.

Gateway-only-tier workloads have no Agent resource and get no synthesized policy.

### Resource isolation

A Pod gets the resources its Agent spec sets, or the class's `spec.resources.defaults` when the spec sets none, and `spec.resources.maxLimits` clamps both limits and requests to the class cap. A class that sets neither leaves the Pod without limits, so a class meant to contain a runaway agent must set at least `maxLimits`.

## Data flow and audit

### What flows where

Fourteen traffic classes carry Kaalm's data. Seven stay inside the cluster and seven cross its boundary. Two are optional.

| Class | From, to | Carries | Transport |
|---|---|---|---|
| 1 | Agent to gateway | Prompts and completions, heartbeats, task completion, tool calls | TLS to the cluster listener; the agent presents its client certificate ([Mode 1](../gateways/llm/workload-identity.md#mode-1-mtls-client-certificate)) |
| 2 | Gateway to agent | The normalized message envelope on `POST /v1/message` | mTLS both ways; each side verifies the other's SAN |
| 3 | Gateway to controller | `POST /v1/activate` | mTLS, authorized by SAN ([Internal endpoint authentication](rbac.md#internal-endpoint-authentication)) |
| 4 | Controller to gateway | `GET /v1/activity`, `GET /v1/channels/health` | mTLS, authorized by SAN |
| 5 | Gateway to API server | `TokenReview`; budget, spend, and async ConfigMaps in `kaalm-system`; the completion ConfigMap in a task's namespace; the channel-disconnected annotation | The API server's TLS |
| 6 | Controller and API server | Watches, status writes, child objects, Events; the conversion webhook the API server calls on `:9444` | The API server's TLS; the webhook serves `kaalm-controller-tls` |
| 7 | Console (optional) | Fleet reads under its own ServiceAccount; `POST /v1/test-chat` and `GET /v1/spend` on the gateway | mTLS, authorized by SAN ([Console](../console/overview.md)) |
| 8 | Channel platform to gateway | Inbound webhook events | HTTPS through the Ingress to the User listener |
| 9 | Gateway to LLM provider | Prompts and completions, with the provider key injected | HTTPS; custom CA bundles through [Upstream TLS configuration](../gateways/llm/provider-routing.md#upstream-tls-configuration) |
| 10 | Gateway to MCP tool server | Brokered tool calls, with the tool credential injected | HTTPS to the ToolProvider endpoint, in or outside the cluster ([The broker](../gateways/tool-plane.md#the-broker)) |
| 11 | Gateway to `callbackUrl` | Async response and error payloads, which may carry PII | HTTPS, every POST signed ([rule 25](../resources/validation-and-defaulting.md#cross-resource-validation)); the receiver is chosen by the channel owner and is not trusted, so the target passes the deny ranges and the checked IP is pinned into the dial ([SSRF through callbackUrl](threat-model.md#ssrf-through-callbackurl)) |
| 12 | Gateway to platform reply API | Agent replies, carrying the channel's platform credential | HTTPS to the operator-set `gateway.platforms.<type>.apiBaseUrl` ([The platform adapters](../gateways/user/platform-adapters.md)) |
| 13 | Controller to LLM provider and tool server | Health probes, carrying the provider key or tool credential | HTTPS ([Liveness probe](../controller/reconcilers.md#liveness-probe)) |
| 14 | Gateway to OTLP collector (optional) | Trace spans with request metadata, never prompt or reply content | HTTPS verified against the upstream trust pool when one is configured, else the system roots; `http://` endpoints send in the clear ([Tracing](../operations/observability.md#tracing)) |

![The seven traffic classes that stay inside the cluster: the agent's calls to the gateway, delivery back to the agent, the two internal endpoint directions between gateway and controller, both components' API server traffic, and the optional console's calls.](../diagrams/traffic-inside.svg)

![The seven traffic classes that cross the cluster boundary: the inbound webhook, and the outbound edges from the gateway to the LLM provider, the MCP tool server, the callbackUrl receiver, and the platform reply API, plus the controller's health probes and the optional OTLP export.](../diagrams/traffic-crossing.svg)

Six classes leave the cluster carrying a credential or content: 9, 10, 11, 12, 13, and 14. Class 11 is the only one whose target the platform team does not choose, which is why it carries the most controls. Classes 9, 10, 12, and 13 go to hosts an operator or platform engineer configured, and defending the gateway against its own operator is out of scope by the trust model.

### Audit trail

The operator emits Kubernetes Events for phase transitions on Agents and AgentTasks, hibernation and wake, task completion, reconcile-time validation failures, and provider health changes ([Event emission](../controller/operations.md#event-emission) lists the reasons). The gateway emits `Warning` events on the resources it governs at runtime: `FallbackIneligible` and `CredentialsInvalid` on a ModelProvider during a fallback walk, and `CallbackRejected` on an AgentChannel when a platform refuses or exhausts a reply. A callback receiver that fails the pre-dial check or refuses a delivery is recorded on the channel's health status, not as an Event ([Channel health](../gateways/user/platform-adapters.md#channel-health-tracking)). Event messages carry reasons and status codes, never credential material; a `CallbackRejected` message quotes at most a short prefix of the platform's response body.

Per-request decisions are not Events. Provider access grants and denials, budget threshold crossings, and brokered tool calls are counted as [metrics](../operations/observability.md#metrics), and tool calls are also written as audit log lines ([Audit and metering](../gateways/tool-plane.md#audit-and-metering)). Credential rotation writes nothing: the gateway follows the Secret through a watch.

Events persist in etcd for the cluster's Event retention. For long-term audit, ship them to an external audit log with Kubernetes audit logging or a tool such as Falco.

Kaalm does not log prompts or completions. LLM payloads may carry PII or proprietary data, and the default build compiles the body logger out ([PII safety](../operations/observability.md#pii-safety)). If you need prompt auditing, implement it outside Kaalm, for example as a provider adapter that duplicates traffic to a log sink.

## Recommendations for deployment

1. **Install Kaalm in a dedicated namespace** (`kaalm-system`) and restrict who can `exec` into it or modify resources in it.
2. **Write a NetworkPolicy for `kaalm-system`.** As shipped the chart creates none for its own components, so the conversion listener on `:9444`, the controller metrics port `:8080`, and the gateway metrics port `:9090` are reachable from any Pod with network reach. The metrics ports are unauthenticated and label spend by namespace and model ([Deployment](../operations/deployment.md#helm-chart-contents)).
3. **Expose the User listener through a dedicated Ingress or LoadBalancer** with an HTTPS backend; the listener is TLS-only ([TLS and Ingress](../gateways/user/overview.md#tls-and-ingress)).
4. **Enable Kubernetes audit logging** at the `Metadata` level at least, and `RequestResponse` for Secret access where the volume allows it.
5. **Rely on standard NetworkPolicy** for the agent-to-gateway rule and for CIDR egress. It is enforced on every CNI that implements NetworkPolicy and needs no service mesh. Hostname egress (`allowedHosts`) needs Cilium; on another CNI, use `allowedCIDRs` or write the CNI's own hostname policy beside Kaalm's. Workloads in the gateway-only tier get no synthesized policy, so apply a default-deny egress policy on those namespaces yourself if direct provider calls must be prevented.
6. **Separate credential management from platform engineering** where you can: a secrets-admin Role for the credential Secrets in `kaalm-system` and the channel namespaces, distinct from the catalog role ([Roles for people](rbac.md#roles-for-people)).
7. **Require a RuntimeClass on any AgentClass that runs LLM-generated code.** Installing the RuntimeClass and validating it on the cluster is the platform team's job.
