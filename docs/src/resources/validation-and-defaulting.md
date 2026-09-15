# Validation and defaulting

Kaalm runs no admission webhook server, validating or mutating. Every rule on this page is enforced either by CEL expressions embedded in the CRD schema, which the apiserver evaluates at apply time, or by a reconciler, which reports violations as status. That keeps the control plane free of a webhook availability dependency, and it lets rules that span resources surface on pre-existing objects as recoverable status rather than blocked writes.

## Cross-resource validation

Every rule is enforced in one of two places:

- **CRD CEL** (`x-kubernetes-validations` in the CRD schema): the apiserver evaluates it at apply time and rejects the write, so a violating object never exists.
- **A reconciler**: it reads the stored object and reports a violation as status.

The line between them is etcd. A rule that spans two resources (a workload against its AgentClass or ModelProvider) cannot be CEL, because CEL on one object cannot read another; a rule that needs `metadata.namespace` cannot be CEL either, because CRD validation reaches only `metadata.name` and `metadata.generateName`; and a rule whose violation must surface on pre-existing objects as recoverable status, rather than block their next write, is placed at reconcile time on purpose. Kaalm runs no admission webhook server, so there is no third place (the conversion webhook translates between API versions and enforces nothing; see [API versioning and deprecation](../operations/api-versioning.md)).

![Swimlane flow from Developer through apiserver, etcd, Reconciler, and status. A kubectl apply first meets the CRD CEL rules; a violation is rejected and the object never exists. Everything that passes is stored, the reconciler reads the stored object and applies the reconcile-time rules, and a violation takes one of three shapes: Ready=False with a reason naming the rule; a class mismatch, which puts an Agent into phase=Degraded with preDegradedPhase kept until the specs align and an AgentTask into phase=Failed; or a Warning event that leaves Ready unaffected.](../diagrams/validation-enforcement.svg)

Reading the figure: a violation to the left of the etcd lane comes back on the developer's terminal; a violation to the right of it can only be reported on an object that already exists. The class-mismatch arm is the only one that recovers: the Agent keeps `preDegradedPhase` and returns to it when the developer or the platform team aligns the specs ([AgentClass change handling](../controller/change-propagation.md#agentclass-change-handling) draws that path). An AgentTask has no `Degraded` phase, so the same mismatch fails it.

### Where each rule is enforced

Rule numbers are stable identifiers. Other pages cite them by number, so the numbering never changes; this table is the index, and [The rules](#the-rules) below states each one.

| Rule | Guards | Enforced by | On violation |
|---|---|---|---|
| 1 | `agentClassRef` names an existing AgentClass | Agent and AgentTask reconcilers | `Ready=False`, `InvalidReference` |
| 2 | The workload image is on the class allowlist | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| 3 | `providers[].providerRef` names an existing ModelProvider | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| 4 | The ModelProvider admits the workload's namespace | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| 5 | The ModelProvider is on the class `allowedProviders` | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| 6 | Resource limits within the class cap | Agent and AgentTask reconcilers | Clamped to the cap, never rejected |
| 7 | Volume size within the class cap | Agent and AgentTask reconcilers | Clamped |
| 8 | Idle timeout within the class cap | AgentReconciler | Clamped |
| 9 | Wake timeout within the class cap | AgentReconciler | Clamped |
| 10 | Hibernation delay within the class cap | AgentReconciler | Clamped |
| 11 | Fallback chains terminate | ModelProviderReconciler | `Ready=False`, `FallbackIneligible` |
| 12 | Fallback stays within translatable formats | ModelProviderReconciler; the gateway per candidate | `Ready=False`, `FallbackIneligible` |
| 13 | `agentRef` names an existing Agent | AgentChannelReconciler | `phase=Failed`, `Ready=False`, `AgentNotFound` |
| 14 | The target Agent has a Service | AgentChannelReconciler | `Ready=False`, `AgentServiceDisabled` |
| 15 | The channel path sits under its own namespace prefix and is unique | AgentChannelReconciler; the gateway | `Ready=False`, `InvalidPath` or `PathConflict` |
| 16 | The channel path is not under `/v1/` | CRD CEL | Rejected at apply |
| 17 | An exit-code task declares no artifacts | CRD CEL | Rejected at apply |
| 18 | `degradeTo` names a catalog model | ModelProviderReconciler | `Ready=False`, `InvalidDegradeTarget` |
| 19 | Egress CIDRs are well-formed | AgentClassReconciler | `Ready=False`, `InvalidReference`, message names the entry |
| 20 | Egress hosts are DNS names the CNI can enforce | AgentClassReconciler | Malformed: `Ready=False`, `InvalidReference`; unenforceable: Warning `FQDNPolicyUnsupported`, Ready unaffected |
| 21 | Agent and AgentTask names are DNS labels | CRD CEL | Rejected at apply |
| 22 | `callbackUrl` is HTTPS and not internal | AgentChannelReconciler; the gateway on every delivery | `Ready=False`, `InvalidCallbackUrl` |
| 23 | Image pull Secrets exist in the workload's namespace | Agent and AgentTask reconcilers | `Ready=False`, `ImagePullSecretMissing` |
| 24 | Persistence is allowed by the class | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `PersistenceNotAllowed` |
| 25 | `callbackUrl` has `callbackAuth`, whose Secret resolves | CRD CEL; AgentChannelReconciler | Rejected at apply; `Ready=False`, `CredentialsMissing` |
| 26 | Hibernation is allowed by the class | AgentReconciler | `Degraded`, `HibernationNotAllowed` |
| 27 | `existingClaim` excludes `sizeGi`, and the claim exists | CRD CEL; AgentReconciler | Rejected at apply; `Ready=False`, `ExistingClaimNotFound` |
| 28 | No workloads in the system namespace | Agent, AgentTask, and AgentChannel reconcilers | `Ready=False`, `SystemNamespaceForbidden` |
| 29 | Hibernation requires persistence | AgentReconciler | `Degraded`, `HibernationRequiresPersistence` |
| 30 | Handler mounts are allowed by the class | AgentReconciler | `Degraded`, `HandlerMountNotAllowed` |
| 31 | The handler ConfigMap exists | AgentReconciler | `Ready=False`, `HandlerConfigMapNotFound` |
| 32 | Hard enforcement has a block policy | CRD CEL | Rejected at apply |
| 33 | Hard enforcement has a fully priced catalog | ModelProviderReconciler | `Ready=False`, `HardBudgetUnpriced` |
| 34 | The boundary margin sits below every block threshold | CRD CEL | Rejected at apply |
| 35 | `tools[].providerRef` names an existing ToolProvider | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| 36 | The ToolProvider admits the workload's namespace | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| 37 | The ToolProvider is on the class `allowedToolProviders` | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| 38 | Granted tools exist in the declared catalog | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ToolNotInCatalog` |
| 39 | The channel type matches its configuration block | CRD CEL | Rejected at apply |
| 40 | Platform credentials resolve with the required keys | AgentChannelReconciler | `Ready=False`, `CredentialsMissing` or `CredentialsInvalid` |
| 41 | A model map names real models on both ends | ModelProviderReconciler | `Ready=False`, `InvalidModelMap` |

Two families behave differently from the rest. The class-mismatch family (rules 2 to 5, 24, 26, 29, 30, and 35 to 38) is recoverable on an Agent and terminal on an AgentTask, and a change to an AgentClass or ModelProvider can trigger any of its rules on a workload that was fine a moment ago ([Change propagation](../controller/change-propagation.md#agentclass-change-handling)). The cap family (rules 6 to 10) never rejects: the effective value is the smaller of what the workload asked for and the class cap, which is what lets a class tighten a cap without degrading every workload that asked for more.

### The rules

Each rule states what must hold, where it is enforced, and why. They are grouped by what they protect; the numbers are the stable identifiers.

#### Referenced objects must exist

**Rule 1: Class references must resolve.** `Agent.spec.agentClassRef` and `AgentTask.spec.agentClassRef` must name an existing AgentClass. *Reconcile time; `Ready=False, reason=InvalidReference`.*

**Rule 3: Provider references must resolve.** Every `providers[].providerRef` on an Agent or AgentTask must name an existing ModelProvider. *Reconcile time; the class-mismatch handling, `reason=ClassConstraintViolation`.* It shares its outcome with rules 4 and 5 because a provider that vanishes and a provider that stops admitting the workload look the same to the workload.

**Rule 13: Channel targets must resolve.** `AgentChannel.spec.agentRef` must name an existing Agent in the channel's namespace. *Reconcile time; `phase=Failed`, `Ready=False, reason=AgentNotFound`.*

**Rule 23: Image pull Secrets named by the class must exist where the workload runs.** Every `AgentClass.spec.image.imagePullSecrets[*].name` must exist as a Secret in the referencing workload's namespace. *Reconcile time, before the Pod is created; `Ready=False, reason=ImagePullSecretMissing`, message naming the namespace and Secret.* AgentClass is cluster-scoped but Secrets are namespaced, and the controller never copies Secrets across namespaces; see the [AgentClass design notes](agentclass.md#design-notes).

**Rule 27: A pre-existing claim excludes a provisioning size, and the claim must exist.** `Agent.spec.persistence.existingClaim` must not be combined with `persistence.sizeGi`, and must name a PersistentVolumeClaim in the Agent's namespace. *The exclusion is CRD CEL on the `persistence` block (`!has(self.sizeGi) || !has(self.existingClaim)`); the existence check is reconcile time, before the Pod is created, `Ready=False, reason=ExistingClaimNotFound`.* Rule 24 still applies to `persistence.enabled: true` whether the PVC is Kaalm-provisioned or pre-existing. The reconciler adds no ownerRef to a pre-existing PVC, so `pvcRetention` governs only Kaalm-provisioned PVCs; see the [Agent design notes](agent.md). The field exists only on the Agent schema.

**Rule 31: The handler ConfigMap must exist where the Agent runs.** `Agent.spec.handler.configMapRef.name` must name a ConfigMap in the Agent's namespace. *Reconcile time, before the Pod is created; `Ready=False, reason=HandlerConfigMapNotFound`, message naming the namespace and ConfigMap.* A clear condition beats a Pod wedged in `ContainerCreating` on a missing volume source. The reconciler adds no ownerRef: the ConfigMap is developer-owned, survives Agent deletion, and its content is not tracked (see [Handler update semantics](../runtime/base-images.md#handler-update-semantics)).

**Rule 35: Tool references must resolve.** Every `tools[].providerRef` on an Agent or AgentTask must name an existing ToolProvider. *Reconcile time; the class-mismatch handling, `reason=ClassConstraintViolation`.* The rule 3 analog for the [tool plane](../gateways/tool-plane.md).

#### Access gates on providers and tools

These rules and rules 3 and 35 are the gate chain drawn on [Core concepts](../concepts/core-concepts.md#the-custom-resources): the class allows the provider, the workload asks for it, and the provider admits the namespace. All of them run at reconcile time, because each compares two resources, and all of them share one outcome: `phase=Degraded, reason=ClassConstraintViolation` on an Agent, `phase=Failed` on an AgentTask.

**Rule 4: A provider must admit the workload's namespace.** Every referenced ModelProvider must list the workload's namespace in `allowedNamespaces`.

**Rule 5: A provider must be on the class allowlist.** Every referenced ModelProvider must appear in the AgentClass's `allowedProviders`.

**Rule 36: A ToolProvider must admit the workload's namespace.** Every referenced ToolProvider must list the workload's namespace in `allowedNamespaces`, the rule 4 analog.

**Rule 37: A ToolProvider must be on the class allowlist.** Every referenced ToolProvider must appear in the AgentClass's `allowedToolProviders`, the rule 5 analog. The class remains the authority on which capabilities workloads of its category may reach.

**Rule 38: Granted tools must exist in a declared catalog.** When a ToolProvider declares a `tools` catalog, every tool name in a workload's grant must appear in it. *Reconcile time; the class-mismatch handling with its own `reason=ToolNotInCatalog`, naming the missing tools.* When no catalog is declared, the server's own `tools/list` governs and this rule does not apply.

#### What the class allows

Each of these compares a workload's opt-in against its AgentClass, so none can be CRD CEL. A violation follows the class-mismatch handling: the Agent moves to `phase=Degraded` with `preDegradedPhase` set and its Pod is not created (or not recreated, when class drift introduced the conflict after the Pod was running), and it returns to its prior phase when the specs align. Rules 26, 29, and 30 are Agent-only, because hibernation and handler mounts do not apply to one-shot tasks; for rules 2 and 24 an AgentTask moves to `phase=Failed` instead.

**Rule 2: The workload image must be on the class allowlist.** `Agent.spec.image` and `AgentTask.spec.image` must match at least one pattern in `AgentClass.spec.image.allowedImages` when the list is non-empty. *`reason=ClassConstraintViolation`.*

**Rule 24: A workload cannot enable persistence that its class forbids.** `persistence.enabled: true` on an Agent or AgentTask requires `spec.persistence.enabled: true` on the class. *`reason=PersistenceNotAllowed`; on an AgentTask this matches the class-drift handling for backoff retries in [AgentTaskReconciler](../controller/reconcilers.md#agenttaskreconciler).* The class is the authority on whether workloads of its category get a PVC.

**Rule 26: An Agent cannot enable hibernation that its class forbids.** `Agent.spec.lifecycle.hibernationEnabled: true` requires `spec.lifecycle.hibernationAllowed: true` on the class. *`reason=HibernationNotAllowed`.*

**Rule 29: Hibernation requires persistence on the same Agent.** `Agent.spec.lifecycle.hibernationEnabled: true` requires `spec.persistence.enabled: true` on the same Agent. *`reason=HibernationRequiresPersistence`.* Hibernation deletes the Pod and recreates it with the same mount ([Hibernation mechanics](../controller/hibernation-and-wake.md#hibernation-mechanics)), and [runtime contract](../runtime/contract.md) item 7 requires a hibernation-enabled agent to persist its message-dedup buffer across restarts; neither works without a PVC. The rule is spec-internal, so CEL could express it; it runs at reconcile time so that a pre-existing Agent surfaces the violation as recoverable status rather than being stranded behind a new apply-time rule.

**Rule 30: An Agent cannot mount a handler that its class forbids.** `Agent.spec.handler` requires `spec.image.allowHandlerMounts: true` on the class (default `false`). *`reason=HandlerMountNotAllowed`, including when a platform team flips the field to `false` on a live class.* Rule 2 makes `allowedImages` an image review boundary; a mounted handler ([Reference base images](../runtime/base-images.md)) injects code into an image that review already approved, so the class decides whether ConfigMap-sourced code is acceptable for its category. The field exists only on the Agent schema (see [Task mode](../runtime/base-images.md#task-mode)).

#### Class caps

Resource limits, volume size, and the three lifecycle timeouts are bounded by the class, but a workload that asks for more is not rejected: the effective value is **clamped** to the cap at reconcile time (the `resources.limits` clamp is drawn in [Change propagation](../controller/change-propagation.md#agentclass-change-handling)).

**Rule 6: Resource limits are capped by the class.** Resource `limits` must not exceed `AgentClass.spec.resources.maxLimits`.

**Rule 7: Volume size is capped by the class.** `persistence.sizeGi` must not exceed `AgentClass.spec.persistence.maxSizeGi`.

**Rule 8: Idle timeout is capped by the class.** `lifecycle.idleTimeout` must not exceed `AgentClass.spec.lifecycle.maxIdleTimeout`.

**Rule 9: Wake timeout is capped by the class.** `lifecycle.wakeTimeout` must not exceed `AgentClass.spec.lifecycle.maxWakeTimeout`. As shipped the reconciler does not enforce this rule, and the gateway uses the Agent's own value.

**Rule 10: Hibernation delay is capped by the class.** `lifecycle.hibernationDelay` must not exceed `AgentClass.spec.lifecycle.maxHibernationDelay`.

#### Names and namespaces

**Rule 21: Agent and AgentTask names are plain DNS labels.** `metadata.name` must match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` and be at most 63 characters: lowercase alphanumerics and hyphens, no dots. *Root-scoped CRD CEL on both kinds (`self.metadata.name.matches(...) && size(self.metadata.name) <= 63`).* Names are used verbatim as single DNS labels in certificate SANs and as per-Agent Service names, both capped at 63. Kubernetes would allow a 253-character DNS subdomain with dots; Kaalm restricts these two kinds to the label form so the gateway's SAN parsing, which reads the namespace as label index 1 of the dot-split SAN, cannot be tricked by a dotted name. The threat scenario is on the [Agent](agent.md) and [AgentTask](agenttask.md) design notes; the gateway's label-count check is [Workload identity](../gateways/llm/workload-identity.md), mode 1.

**Rule 28: Workloads are forbidden in the system namespace.** Agent, AgentTask, and AgentChannel objects in `kaalm-system` are never provisioned. *Reconcile time; `Ready=False, reason=SystemNamespaceForbidden`, no child resources.* A SAN-integrity guard: per-Agent certificates carry the SAN `{name}.{namespace}.svc.cluster.local`, so an Agent named `kaalm-gateway` in `kaalm-system` would hold the gateway's own identity, which every internal SAN-based authorization trusts. Reconcile time because CRD CEL cannot reach `metadata.namespace`; the collision scenario is in the [threat model](../security/threat-model.md).

#### Channels

**Rule 14: A channel needs a reachable agent Service.** The referenced Agent must have `spec.service.enabled: true`. *Reconcile time; `Ready=False, reason=AgentServiceDisabled`.*

**Rule 15: A channel's path must live under its own namespace prefix, and be unique there.** The path (`spec.webhook.path`, `spec.discord.path`, or `spec.whatsapp.path`, whichever block the type selects) must begin with `/channels/{namespace}/` for the channel's own namespace. *Reconcile time by the AgentChannelReconciler, `Ready=False, reason=InvalidPath`; on a conflict within a namespace the newer channel by `creationTimestamp` gets `reason=PathConflict`. The gateway independently refuses to register a non-conforming path and routes only to channels whose `Ready` condition is `True` ([Request flow](../gateways/user/overview.md#request-flow), step 4).* CRD CEL cannot express this rule because `metadata.namespace` is not reachable from CRD validation (contrast rule 21). Namespace-scoping removes cross-tenant path conflicts at the routing layer: two namespaces cannot claim the same prefix, and a violating channel never receives traffic.

**Rule 16: A channel's path must not claim the reserved API prefix.** The path must not begin with `/v1/`. *CRD CEL on each path field.* Rule 15 already implies this, but rule 15 is reconcile-time; this rule is the only apply-time guard on reserved paths, so a `/v1/` path is rejected before the reconciler ever sees it. See [Reserved gateway paths](../gateways/api/overview.md#reserved-gateway-paths).

**Rule 22: Callback URLs must be HTTPS and must not point into internal address space.** `AgentChannel.spec.webhook.callbackUrl`, when set, must use `https://`, and its host must not resolve to loopback, link-local, RFC 1918 private ranges, unique-local IPv6, shared address space (100.64.0.0/10), benchmarking space (198.18.0.0/15), or the cloud-metadata addresses (169.254.169.254, fd00:ec2::254). *Reconcile time, `Ready=False, reason=InvalidCallbackUrl`; the gateway re-resolves the host and repeats the check on every delivery attempt, which defeats DNS rebinding ([Callback delivery](../gateways/api/async-responses.md)).* The Helm value `gateway.callbackUrl.allowlist` (DNS-name suffixes and CIDR blocks) opens the deny-internal default for specific targets on both checks; loopback, link-local, and the cloud-metadata addresses stay refused even when listed, so an allowlist can reach in-cluster receivers without re-exposing the SSRF-critical targets.

**Rule 25: A callback URL without callback auth is invalid.** When `spec.webhook.callbackUrl` is set, `spec.webhook.callbackAuth` must be set, and its Secret must exist in the channel's namespace with the configured key. *The presence rule is CRD CEL (`has(self.spec.webhook.callbackUrl) ? has(self.spec.webhook.callbackAuth) : true`); the Secret check is reconcile time, `Ready=False, reason=CredentialsMissing`, the same shape as the inbound `auth` check.* Outbound callbacks must be attributable to the gateway so receivers can reject forged POSTs; an unsigned `callbackUrl` would let anyone who learns the URL deliver fake response or error payloads. The wire contract is [Callback delivery](../gateways/api/async-responses.md); the forgery row is in the [threat model](../security/threat-model.md).

**Rule 39: A channel's type and its configuration block must match.** `spec.type` selects exactly one of `spec.webhook`, `spec.discord`, and `spec.whatsapp`; that block must be set and the other two absent. *CRD CEL on the spec (`(self.type == 'webhook') == has(self.webhook) && (self.type == 'discord') == has(self.discord) && (self.type == 'whatsapp') == has(self.whatsapp)`).* The adapter is chosen by `type`; the block is what the adapter reads ([platform types](agentchannel.md#platform-types)).

**Rule 40: Platform credentials must resolve with the keys the type requires.** `spec.discord.credentialsRef` and `spec.whatsapp.credentialsRef` must name a Secret in the channel's namespace carrying the type's keys: `publicKey` for Discord (`botToken` optional); `verifyToken`, `appSecret`, and `accessToken` for WhatsApp. *Reconcile time, after the per-channel credential Role is scoped to the Secret; `Ready=False, reason=CredentialsMissing` for a missing Secret or key, `reason=CredentialsInvalid` for a Discord `publicKey` that is not 32 bytes of hex, because no verifier can be built from it and every inbound request would fail with a misleading `401`.* See [AgentChannelReconciler](../controller/reconcilers.md#agentchannelreconciler).

#### Providers: fallback and budgets

These rules concern a single ModelProvider but run at reconcile time: rules 11, 12, and 41 must follow references to other providers, and rules 18 and 33 must match the gateway's model lookup and price parsing exactly, which CRD CEL cannot replicate ([ModelProviderReconciler](../controller/reconcilers.md#modelproviderreconciler)).

**Rule 11: Fallback chains must terminate.** `ModelProvider.spec.fallback` chains must not be circular. *Reconcile time, walking the full chain up to `maxFallbackDepth`; `Ready=False, reason=FallbackIneligible`.*

**Rule 12: Fallback stays within formats the gateway can translate.** Each `spec.fallback[]` entry must have the same `spec.type` as the provider naming it, or a type the gateway translates to: `anthropic` and `openai` or `openai-compatible` may reference each other in either direction; `google-vertex` may reference only `google-vertex` and be referenced only by it. *Reconcile time, in the same walk as rule 11, `Ready=False, reason=FallbackIneligible`; the gateway repeats the check per candidate as static eligibility.* See [Crossing formats](../gateways/llm/fallback.md#crossing-formats).

**Rule 18: A budget degrade target must name a real model.** `budget.policies[].degradeTo` must match a model `id` in the same provider's `spec.models`. *Reconcile time; `Ready=False, reason=InvalidDegradeTarget`.* A missing target would fail routing silently the moment the threshold is crossed.

**Rule 32: Hard budget enforcement requires a block policy.** `budget.enforcement: hard` requires at least one `policies[]` entry with `action: block`. *CRD CEL on the budget block (`self.enforcement != 'hard' || self.policies.exists(p, p.action == 'block')`).* Hard mode's whole effect is to turn block thresholds into a guaranteed cap ([Hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement)); without a block policy it would be a silent no-op.

**Rule 33: Hard budget enforcement requires a fully priced catalog.** With `budget.enforcement: hard`, every model in `spec.models` must carry `costPer1MInputTokens` and `costPer1MOutputTokens` values the gateway's ledger can parse. *Reconcile time, alongside rule 18; `Ready=False, reason=HardBudgetUnpriced`, message naming the unpriced models.* An unpriced model costs zero in the ledger, so a cap over it is never reached.

**Rule 34: The boundary margin must sit strictly below every block threshold.** `budget.hard.boundaryMarginPercent` must be at least 0 and, for every `policies[]` entry with `action: block`, strictly less than that policy's `atPercent`. *CRD CEL on the budget block (`!has(self.hard) || self.policies.all(p, p.action != 'block' || self.hard.boundaryMarginPercent < p.atPercent)`).* A margin at or above a block threshold would start the boundary region at zero utilization and serialize all traffic from the first request. The configured margin is a floor; the runtime widening is [Hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement)'s job.

**Rule 41: A model map must name real models on both ends.** In `spec.fallback[].modelMap`, every key must match a model `id` in this provider's `spec.models` and every value a model `id` in the referenced provider's `spec.models`. *Reconcile time, alongside rules 11 and 12; `Ready=False, reason=InvalidModelMap` on the provider carrying the map, naming the entry.* A map on an edge that does not cross formats is allowed and applied the same way; the gateway then trusts the map and tests only the mapped model's presence ([Crossing formats](../gateways/llm/fallback.md#crossing-formats)).

#### Class egress

**Rule 19: Egress CIDR entries must be well-formed.** Every `AgentClass.spec.network.egress.allowedCIDRs` entry must parse as an IPv4 or IPv6 CIDR block. *Reconcile time; `Ready=False, reason=InvalidReference` on the AgentClass, message naming the entry.*

**Rule 20: Egress host entries must be valid DNS names, and the class reports whether the CNI could enforce them.** Every `AgentClass.spec.network.egress.allowedHosts` entry must be a valid RFC 1123 DNS name. *Reconcile time. A malformed entry: `Ready=False, reason=InvalidReference`. A well-formed list on a CNI whose startup probe found no FQDN egress policy type (Cilium's `CiliumNetworkPolicy` or Calico Enterprise's equivalent): a `Warning` event, `reason=FQDNPolicyUnsupported`, with `Ready` unaffected; the `FQDNPolicySupported` condition records the probe's answer either way.* The controller synthesizes no FQDN policy from the field on any CNI, so `allowedCIDRs` alone governs egress.

#### Tasks

**Rule 17: An exit-code task cannot collect artifacts.** An AgentTask with `spec.completion.condition: exitCode` must not declare `spec.artifacts`. *CRD CEL.* Artifacts travel in the `POST /v1/task/complete` payload, which exists only in `agentReported` mode.

## Defaulting

AgentClass defaults are applied at reconcile time when Agent/AgentTask fields are absent:

| Workload field | Defaults from |
|---|---|
| `resources` | `AgentClass.spec.resources.defaults` |
| `persistence.sizeGi` | `AgentClass.spec.persistence.defaultSizeGi` (see note below) |
| `image` | `AgentClass.spec.image.defaultImage` |
| `lifecycle.idleTimeout` | `AgentClass.spec.lifecycle.defaultIdleTimeout` |
| `lifecycle.hibernationDelay` | `AgentClass.spec.lifecycle.defaultHibernationDelay` |
| `lifecycle.wakeTimeout` | `AgentClass.spec.lifecycle.defaultWakeTimeout` (not applied as shipped; the gateway uses 2m when the Agent leaves it unset) |

The `persistence.sizeGi` default is not applied when `persistence.existingClaim` is set: no PVC is provisioned in that case, so a size is meaningless (see rule 27).

Defaults are applied at reconcile time rather than admission. The stored spec reflects what the developer wrote. Effective values can be derived by merging the AgentClass defaults at read time. This avoids a mutating webhook dependency while keeping the behavior predictable.
