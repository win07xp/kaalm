# Validation and defaulting

Kaalm runs no admission webhook server, validating or mutating. Every check is enforced by the apiserver at apply time, from the CRD schema, or by a reconciler, which reports a violation as status. That keeps the control plane free of a webhook availability dependency, and it lets rules that span resources surface on existing objects as recoverable status rather than blocked writes.

## Cross-resource validation

A check is enforced in one of three places:

| Where | Mechanism | A violation becomes |
|---|---|---|
| Apply time, schema | OpenAPI `pattern`, `enum`, `required`, `minimum`, and map-typed lists | The write is rejected |
| Apply time, CEL | `x-kubernetes-validations` in the CRD schema | The write is rejected |
| Reconcile time | A reconciler reads the stored object | Status on the object |

The line between apply time and reconcile time is etcd. A rule that spans two resources cannot be CEL, because CEL on one object cannot read another; a rule that needs `metadata.namespace` cannot be CEL either, because CRD validation reaches only `metadata.name` and `metadata.generateName`; and a rule whose violation must surface on existing objects as recoverable status is placed at reconcile time on purpose. The conversion webhook translates between API versions and enforces nothing ([API versioning and deprecation](../operations/api-versioning.md)).

![Flow from a kubectl apply through the apiserver to etcd and the reconciler. The apiserver evaluates the schema and CEL rules and rejects a violating write, so the object never exists. A stored object is read by the reconciler, which applies the reconcile-time rules and reports on status.](../diagrams/validation-enforcement.svg)

A reconcile-time violation takes one of four forms:

![Four outcomes of a reconcile-time rule. A reference or validity failure sets Ready=False with the rule's reason, and on an AgentChannel also phase=Failed. A class mismatch puts an Agent into phase=Degraded, keeping preDegradedPhase until the specs align, and an AgentTask into phase=Failed. A cap is clamped to the class value with no status change. An advisory check emits a Warning event and leaves Ready unaffected.](../diagrams/validation-outcomes.svg)

The class-mismatch arm is the only one that recovers: the Agent keeps `preDegradedPhase` and returns to it when the developer or the platform team aligns the specs ([AgentClass change handling](../controller/change-propagation.md#agentclass-change-handling)). An AgentTask has no `Degraded` phase, so the same mismatch fails it. The cap arm is what lets a class tighten a cap without degrading every workload that asked for more.

### Where each rule is enforced

Rule numbers are stable identifiers. Other pages cite them by number, so the numbering never changes; this table is the index, and [The rules](#the-rules) states each one. The numbered rules cover cross-resource and semantic checks; the field-level schema checks are listed separately under [Schema validation](#schema-validation).

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
| 19 | Egress CIDRs are well-formed | AgentClassReconciler | `Ready=False`, `InvalidCIDR`, message names the entry |
| 20 | Egress hosts are DNS names, and the class reports FQDN support | AgentClassReconciler | Malformed: `Ready=False`, `InvalidReference`; unsupported: Warning `FQDNPolicyUnsupported`, Ready unaffected |
| 21 | Agent and AgentTask names are DNS labels | CRD CEL | Rejected at apply |
| 22 | `callbackUrl` is HTTPS and not internal | AgentChannelReconciler; the gateway on every delivery | `Ready=False`, `InvalidCallbackUrl` |
| 23 | Image pull Secrets exist in the workload's namespace | Agent and AgentTask reconcilers | `Ready=False`, `ImagePullSecretMissing` |
| 24 | Persistence is allowed by the class | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `PersistenceNotAllowed` |
| 25 | `callbackUrl` has `callbackAuth`, whose Secret resolves | CRD CEL; AgentChannelReconciler | Rejected at apply; `Ready=False`, `CallbackAuthMissing` or `CallbackAuthInvalid` |
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

Two families behave differently from the rest. The class-mismatch family (rules 2 to 5, 24, 26, 29, 30, and 35 to 38) is recoverable on an Agent and terminal on an AgentTask, and a change to an AgentClass or ModelProvider can trigger any of its rules on a workload that was fine a moment ago ([Change propagation](../controller/change-propagation.md#agentclass-change-handling)). The cap family (rules 6 to 10) never rejects: the effective value is the smaller of what the workload asked for and the class cap.

Two reconcile-time outcomes carry no number. A failure to write the per-channel credential Role sets `Ready=False, reason=InvalidReference` on the AgentChannel. The two ModelProvider advisory checks, `DegradeTargetNotCheapest` and `MaxOutputTokensUnset`, emit a `Warning` event and leave `Ready` unaffected ([ModelProvider](modelprovider.md#degradeto-validation)).

### The rules

Each rule states what must hold, where it is enforced, and why. They are grouped by what they protect; the numbers are the stable identifiers.

#### Referenced objects must exist

**Rule 1: Class references must resolve.** `Agent.spec.agentClassRef` and `AgentTask.spec.agentClassRef` must name an existing AgentClass. *Reconcile time; `Ready=False, reason=InvalidReference`.*

**Rule 3: Provider references must resolve.** Every `providers[].providerRef` on an Agent or AgentTask must name an existing ModelProvider. *Reconcile time; the class-mismatch handling, `reason=ClassConstraintViolation`.* It shares its outcome with rules 4 and 5 because a provider that vanishes and a provider that stops admitting the workload look the same to the workload.

**Rule 13: Channel targets must resolve.** `AgentChannel.spec.agentRef` must name an existing Agent in the channel's namespace. *Reconcile time; `phase=Failed`, `Ready=False, reason=AgentNotFound`.*

**Rule 23: Image pull Secrets named by the class must exist where the workload runs.** Every `AgentClass.spec.image.imagePullSecrets[*].name` must exist as a Secret in the referencing workload's namespace. *Reconcile time, before the Pod is created; `Ready=False, reason=ImagePullSecretMissing`, message naming the namespace and Secret.* AgentClass is cluster-scoped but Secrets are namespaced, and the controller never copies Secrets across namespaces ([AgentClass design notes](agentclass.md#design-notes)). The controller holds no standing Secret read in a user namespace, so it reads each named Secret under a Role scoped to those names ([Operator ServiceAccount](../security/rbac.md#operator-serviceaccount)).

**Rule 27: A pre-existing claim excludes a provisioning size, and the claim must exist.** `Agent.spec.persistence.existingClaim` must not be combined with `persistence.sizeGi`, and must name a PersistentVolumeClaim in the Agent's namespace. *The exclusion is CRD CEL on the `persistence` block (`!(has(self.sizeGi) && has(self.existingClaim))`); the existence check is reconcile time, before the Pod is created, `Ready=False, reason=ExistingClaimNotFound`.* Rule 24 still applies whether the PVC is Kaalm-provisioned or pre-existing. The field exists only on the Agent schema ([Agent design notes](agent.md#persistenceexistingclaim)).

**Rule 31: The handler ConfigMap must exist where the Agent runs.** `Agent.spec.handler.configMapRef.name` must name a ConfigMap in the Agent's namespace. *Reconcile time, before the Pod is created; `Ready=False, reason=HandlerConfigMapNotFound`, message naming the namespace and ConfigMap.* A clear condition beats a Pod wedged in `ContainerCreating` on a missing volume source. The reconciler adds no ownerRef and tracks no content ([Handler update semantics](../runtime/base-images.md#handler-update-semantics)).

**Rule 35: Tool references must resolve.** Every `tools[].providerRef` on an Agent or AgentTask must name an existing ToolProvider. *Reconcile time; the class-mismatch handling, `reason=ClassConstraintViolation`.* The rule 3 analog for the [tool plane](../gateways/tool-plane.md).

#### Access gates on providers and tools

These rules and rules 3 and 35 are the gate chain drawn on [Core concepts](../concepts/core-concepts.md#the-custom-resources): the class allows the provider, the workload asks for it, and the provider admits the namespace. All of them run at reconcile time, because each compares two resources, and all of them share one outcome: `phase=Degraded, reason=ClassConstraintViolation` on an Agent, `phase=Failed` on an AgentTask.

**Rule 4: A provider must admit the workload's namespace.** Every referenced ModelProvider must list the workload's namespace in `allowedNamespaces`.

**Rule 5: A provider must be on the class allowlist.** Every referenced ModelProvider must appear in the AgentClass's `allowedProviders`.

**Rule 36: A ToolProvider must admit the workload's namespace.** Every referenced ToolProvider must list the workload's namespace in `allowedNamespaces`, the rule 4 analog.

**Rule 37: A ToolProvider must be on the class allowlist.** Every referenced ToolProvider must appear in the AgentClass's `allowedToolProviders`, the rule 5 analog.

**Rule 38: Granted tools must exist in a declared catalog.** When a ToolProvider declares a `tools` catalog, every tool name in a workload's grant must appear in it. *Reconcile time; the class-mismatch handling with its own `reason=ToolNotInCatalog`, naming the missing tools.* When no catalog is declared, the server's own `tools/list` governs and this rule does not apply.

#### What the class allows

Each of these compares a workload's opt-in against its AgentClass, so none can be CRD CEL. A violation follows the class-mismatch handling: the Agent moves to `phase=Degraded` with `preDegradedPhase` set and its Pod is not created (or not recreated, when class drift introduced the conflict), and it returns to its prior phase when the specs align. Rules 26, 29, and 30 are Agent-only, because hibernation and handler mounts do not apply to one-shot tasks; for rules 2 and 24 an AgentTask moves to `phase=Failed` instead.

**Rule 2: The workload image must be on the class allowlist.** `Agent.spec.image` and `AgentTask.spec.image` must match at least one pattern in `AgentClass.spec.image.allowedImages` when the list is non-empty. *`reason=ClassConstraintViolation`.*

**Rule 24: A workload cannot enable persistence that its class forbids.** `persistence.enabled: true` on an Agent or AgentTask requires `spec.persistence.enabled: true` on the class. *`reason=PersistenceNotAllowed`.*

**Rule 26: An Agent cannot enable hibernation that its class forbids.** `Agent.spec.lifecycle.hibernationEnabled: true` requires `spec.lifecycle.hibernationAllowed: true` on the class. *`reason=HibernationNotAllowed`.*

**Rule 29: Hibernation requires persistence on the same Agent.** `Agent.spec.lifecycle.hibernationEnabled: true` requires `spec.persistence.enabled: true` on the same Agent. *`reason=HibernationRequiresPersistence`.* Hibernation deletes the Pod and recreates it with the same mount ([Hibernation mechanics](../controller/hibernation-and-wake.md#hibernation-mechanics)), and [runtime contract](../runtime/contract.md#7-message-deduplication) item 7 requires a hibernation-enabled agent to persist its dedup buffer across restarts; neither works without a PVC. The rule is spec-internal, so CEL could express it; it runs at reconcile time so that an existing Agent surfaces the violation as recoverable status rather than being stranded behind a new apply-time rule.

**Rule 30: An Agent cannot mount a handler that its class forbids.** `Agent.spec.handler` requires `spec.image.allowHandlerMounts: true` on the class (default `false`). *`reason=HandlerMountNotAllowed`, including when a platform team flips the field to `false` on a live class.* Rule 2 makes `allowedImages` an image review boundary; a mounted handler injects code into an image that review already approved, so the class decides whether ConfigMap-sourced code is acceptable for its category ([AgentClass](agentclass.md#allowhandlermounts-guards-the-image-review-boundary-not-code-execution)).

#### Class caps

Resource limits, volume size, and the lifecycle timeouts are bounded by the class, and a workload that asks for more is not rejected: the effective value is clamped to the cap at reconcile time (the `resources.limits` clamp is drawn in [Change propagation](../controller/change-propagation.md#agentclass-change-handling)).

**Rule 6: Resource limits are capped by the class.** A limit above `AgentClass.spec.resources.maxLimits` is lowered to the cap, a request above it likewise, and a resource named in `maxLimits` with no limit of its own is given the cap.

**Rule 7: Volume size is capped by the class.** `persistence.sizeGi` above `AgentClass.spec.persistence.maxSizeGi` is lowered to it.

**Rule 8: Idle timeout is capped by the class.** `lifecycle.idleTimeout` above `AgentClass.spec.lifecycle.maxIdleTimeout` is lowered to it.

**Rule 9: Wake timeout is capped by the class.** `lifecycle.wakeTimeout` above `AgentClass.spec.lifecycle.maxWakeTimeout` is lowered to it. The reconciler writes the result to `status.effectiveWakeTimeout`, which the gateway reads.

**Rule 10: Hibernation delay is capped by the class.** `lifecycle.hibernationDelay` above `AgentClass.spec.lifecycle.maxHibernationDelay` is lowered to it.

#### Names and namespaces

**Rule 21: Agent and AgentTask names are plain DNS labels.** `metadata.name` must match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` and be at most 63 characters. *Root-scoped CRD CEL on both kinds (`self.metadata.name.matches(...) && size(self.metadata.name) <= 63`).* Names are used verbatim as single DNS labels in certificate SANs and as per-Agent Service names, both capped at 63, and the gateway reads the namespace by position in the dot-split SAN, so a dotted name would shift it ([Name validation](agent.md#name-validation-dns-1123-label-enforced-at-the-schema-root)).

**Rule 28: Workloads are forbidden in the system namespace.** Agent, AgentTask, and AgentChannel objects in `kaalm-system` are never provisioned. *Reconcile time; `Ready=False, reason=SystemNamespaceForbidden`, no child resources; an AgentChannel's `phase` stays unset.* A SAN-integrity guard: an Agent named `kaalm-gateway` in `kaalm-system` would hold the gateway's own identity, which every internal SAN-based authorization trusts. Reconcile time because CRD CEL cannot reach `metadata.namespace` ([threat model](../security/threat-model.md)).

#### Channels

**Rule 14: A channel needs a reachable agent Service.** The referenced Agent must have `spec.service.enabled: true`. *Reconcile time; `Ready=False, reason=AgentServiceDisabled`.*

**Rule 15: A channel's path must live under its own namespace prefix, and be unique there.** The path (`spec.webhook.path`, `spec.discord.path`, or `spec.whatsapp.path`, whichever block the type selects) must begin with `/channels/{namespace}/` for the channel's own namespace. *Reconcile time, `Ready=False, reason=InvalidPath`; on a conflict within a namespace the newer channel by `creationTimestamp`, or by name when the timestamps are equal, gets `reason=PathConflict`. The gateway checks the prefix again on every request it resolves and routes only `Ready=True` channels.* CRD CEL cannot express this rule because `metadata.namespace` is not reachable (contrast rule 21). Namespace scoping removes cross-tenant path conflicts at the routing layer.

**Rule 16: A channel's path must not claim the reserved API prefix.** The path must not begin with `/v1/`. *CRD CEL on each path field (`!self.startsWith('/v1/')`).* Rule 15 already implies this, but rule 15 is reconcile time; this is the only apply-time guard on reserved paths ([Reserved gateway paths](../gateways/api/overview.md#reserved-gateway-paths)).

**Rule 22: Callback URLs must be HTTPS and must not point into internal address space.** `AgentChannel.spec.webhook.callbackUrl`, when set, must use `https://`, and its host must not resolve to loopback, link-local (which covers the cloud metadata address `169.254.169.254`), the unspecified address, RFC 1918 private ranges, unique-local IPv6, shared address space (`100.64.0.0/10`), or benchmarking space (`198.18.0.0/15`). *Reconcile time, `Ready=False, reason=InvalidCallbackUrl`; the gateway re-resolves the host and repeats the check before every delivery attempt, which defeats DNS rebinding ([Callback delivery](../gateways/api/async-responses.md)).* The Helm value `gateway.callbackUrl.allowlist` (DNS-name suffixes and CIDR blocks) opens the deny-internal default for specific targets on both checks; loopback, link-local, and the unspecified address stay refused even when listed. As shipped, a host that does not resolve at reconcile time passes the check, on the reasoning that the gateway re-checks before every dial; issue #242 tracks reporting it.

**Rule 25: A callback URL without callback auth is invalid.** When `spec.webhook.callbackUrl` is set, `spec.webhook.callbackAuth` must be set, and its Secret must exist in the channel's namespace with the configured key. *The presence rule is CRD CEL on the `webhook` block (`!has(self.callbackUrl) || has(self.callbackAuth)`); the Secret check is reconcile time: `Ready=False, reason=CallbackAuthMissing` when the Secret or key does not exist or cannot be read, and `Ready=False, reason=CallbackAuthInvalid` when the key is empty or the block names no Secret for its type. The inbound `auth` Secret reports `CredentialsMissing` instead.* Outbound callbacks must be attributable to the gateway so receivers can reject forged POSTs ([Callback authentication](../gateways/api/async-responses.md#callback-authentication)).

**Rule 39: A channel's type and its configuration block must match.** `spec.type` selects exactly one of `spec.webhook`, `spec.discord`, and `spec.whatsapp`; that block must be set and the other two absent. *CRD CEL on the spec, with `type` read as `webhook` when unset: `((has(self.type) ? self.type : 'webhook') == 'webhook') == has(self.webhook)`, and the same for the other two.* The adapter is chosen by `type`; the block is what the adapter reads ([Platform types](agentchannel.md#platform-types)).

**Rule 40: Platform credentials must resolve with the keys the type requires.** `spec.discord.credentialsRef` and `spec.whatsapp.credentialsRef` must name a Secret in the channel's namespace carrying the type's keys: `publicKey` for Discord (`botToken` optional); `verifyToken`, `appSecret`, and `accessToken` for WhatsApp. *Reconcile time, after the per-channel credential Role is scoped to the Secret; `Ready=False, reason=CredentialsMissing` for a missing Secret or key, `reason=CredentialsInvalid` for a Discord `publicKey` that is not 32 bytes of hex, because no verifier can be built from it.* See [AgentChannelReconciler](../controller/reconcilers.md#agentchannelreconciler).

#### Providers: fallback and budgets

These rules concern a single ModelProvider but run at reconcile time: rules 11, 12, and 41 must follow references to other providers, and rules 18 and 33 must match the gateway's model lookup and price parsing exactly ([ModelProviderReconciler](../controller/reconcilers.md#modelproviderreconciler)).

**Rule 11: Fallback chains must terminate.** `ModelProvider.spec.fallback` chains must not be circular. *Reconcile time, walking the full tree; `Ready=False, reason=FallbackIneligible`.* As shipped a provider reached twice through different branches is also reported as circular ([Fallback trees](modelprovider.md#fallback-trees)).

**Rule 12: Fallback stays within formats the gateway can translate.** Each `spec.fallback[]` entry must have the same `spec.type` as the provider naming it, or a type the gateway translates to: `anthropic` and `openai` or `openai-compatible` may reference each other in either direction; `google-vertex` may reference only `google-vertex` and be referenced only by it. *Reconcile time, in the same walk as rule 11, `Ready=False, reason=FallbackIneligible`; the gateway repeats the check per candidate.* See [Crossing formats](../gateways/llm/fallback.md#crossing-formats).

**Rule 18: A budget degrade target must name a real model.** `budget.policies[].degradeTo` must match a model `id` in the same provider's `spec.models`. *Reconcile time; `Ready=False, reason=InvalidDegradeTarget`.* A missing target would fail routing the moment the threshold is crossed.

**Rule 32: Hard budget enforcement requires a block policy.** `budget.enforcement: hard` requires at least one `policies[]` entry with `action: block`. *CRD CEL on the budget block: `!has(self.enforcement) || self.enforcement != 'hard' || (has(self.policies) && self.policies.exists(p, p.action == 'block'))`.* Hard mode's whole effect is to turn block thresholds into a guaranteed cap ([Hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement)); without a block policy it would be a silent no-op.

**Rule 33: Hard budget enforcement requires a fully priced catalog.** With `budget.enforcement: hard`, every model in `spec.models` must carry `costPer1MInputTokens` and `costPer1MOutputTokens` values the ledger can parse. *Reconcile time; `Ready=False, reason=HardBudgetUnpriced`, message naming the unpriced models.* An unpriced model costs zero in the ledger, so a cap over it is never reached.

**Rule 34: The boundary margin must sit strictly below every block threshold.** `budget.hard.boundaryMarginPercent` (0 to 100, default 5) must, for every `policies[]` entry with `action: block`, be strictly less than that policy's `atPercent`. *CRD CEL on the budget block: `!has(self.hard) || !has(self.policies) || self.policies.all(p, p.action != 'block' || self.hard.boundaryMarginPercent < p.atPercent)`.* A margin at or above a block threshold would start the boundary region at zero utilization and serialize all traffic from the first request.

**Rule 41: A model map must name real models on both ends.** In `spec.fallback[].modelMap`, every key must match a model `id` in this provider's `spec.models` and every value a model `id` in the referenced provider's. *Reconcile time, alongside rules 11 and 12; `Ready=False, reason=InvalidModelMap` on the provider carrying the map, naming the entry.* A map on a same-type edge is allowed and applied the same way ([Crossing formats](../gateways/llm/fallback.md#crossing-formats)).

#### Class egress

**Rule 19: Egress CIDR entries must be well-formed.** Every `AgentClass.spec.network.egress.allowedCIDRs` entry must parse as an IPv4 or IPv6 CIDR block. *Reconcile time; `Ready=False, reason=InvalidCIDR` on the AgentClass, message naming the entry. When the class also has other problems, the message lists them all and the reason stays `InvalidCIDR`.*

**Rule 20: Egress host entries must be valid DNS names, and the class reports whether the CNI could enforce them.** Every `AgentClass.spec.network.egress.allowedHosts` entry must be a valid RFC 1123 DNS name. *Reconcile time. A malformed entry: `Ready=False, reason=InvalidReference`. A well-formed list on a CNI whose probe found no FQDN egress policy type: a `Warning` event, `reason=FQDNPolicyUnsupported`, with `Ready` unaffected; the `FQDNPolicySupported` condition records the probe's answer either way.* When the CNI supports FQDN policy, the hosts become a `toFQDNs` rule in each workload's CiliumNetworkPolicy ([FQDN egress policy](../runtime/child-resources.md#fqdn-egress-policy)); otherwise they are ignored, with the condition and the `Warning` above, and `allowedCIDRs` alone governs egress.

#### Tasks

**Rule 17: An exit-code task cannot collect artifacts.** An AgentTask with `spec.completion.condition: exitCode` must not declare `spec.artifacts`. *CRD CEL on the spec: `!(has(self.artifacts) && has(self.completion) && has(self.completion.condition) && self.completion.condition == 'exitCode')`; the `has()` guards are needed because reading an absent optional field is an evaluation error in CRD CEL.* Artifacts travel in the `POST /v1/task/complete` payload, which exists only in `agentReported` mode.

### Schema validation

Field-level checks the apiserver enforces from the CRD schema, without a rule number. Each rejects the write at apply time.

| Kind | Field | Check |
|---|---|---|
| Every kind | `*Ref.name`, `credentialsRef.key`, `models[].id`, `tools[].id`, `artifacts[].name` | Required, at least one character |
| AgentClass | `runtime.backend` | Enum `pod` |
| AgentClass | `persistence.pvcRetention` | Enum `Delete`, `Retain` |
| ModelProvider | `type` | Enum `anthropic`, `openai`, `google-vertex`, `openai-compatible` |
| ModelProvider, ToolProvider | `endpoint` | Pattern `^https://` |
| ModelProvider, ToolProvider | `models`, `tools` | Map-typed list keyed by `id`: duplicate ids rejected |
| ModelProvider | `models[].maxOutputTokens` | Minimum 1 |
| ModelProvider | `budget.period`, `budget.enforcement`, `budget.policies[].action` | Enums `monthly`, `weekly`, `daily`, `none`; `soft`, `hard`; `block`, `warn`, `degrade` |
| ModelProvider | `budget.policies[].atPercent`, `budget.hard.boundaryMarginPercent` | 0 to 100 |
| ToolProvider | `type` | Enum `mcp` |
| Agent | `lifecycle.activitySource` | Enum `gatewayTraffic`, `agentHeartbeat`, `both` |
| AgentTask | `completion.condition`, `completion.onTimeout` | Enums `agentReported`, `exitCode`; `Fail`, `Succeed` |
| AgentChannel | `type` | Enum `webhook`, `discord`, `whatsapp` |
| AgentChannel | `webhook.auth`, `webhook.callbackAuth` | `type` is `bearer` or `hmac`; `bearer` requires `secretRef`, `hmac` requires `hmac` (CEL) |
| AgentChannel | `webhook.auth.hmac.algorithm`, `.encoding` | Enums `sha256`, `sha1`; `hex`, `base64` |
| AgentChannel | `webhook.userId`, `webhook.content` | `fromHeader` and `fromBody` mutually exclusive (CEL); `fromBody` pattern `^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)*$` |
| AgentChannel | `webhook.responseMode` | Enum `sync`, `async` |
| AgentChannel | `discord.guildId`, `discord.allowedChannelIds[]` | Pattern `^[0-9]{17,20}$` |
| AgentChannel | `discord.contentOption` | Pattern `^[-_a-z0-9]{1,32}$` |
| AgentChannel | `whatsapp.phoneNumberId` | Pattern `^[0-9]+$` |

## Defaulting

Defaults come from two places.

**Schema defaults** are applied by the apiserver at admission and stored in the spec. A field the developer omits reads back with the default:

| Kind | Field | Default |
|---|---|---|
| AgentClass | `runtime.backend` | `pod` |
| AgentClass | `persistence.pvcRetention` | `Delete` |
| ModelProvider | `budget.period` | `none` |
| ModelProvider | `budget.enforcement` | `soft` |
| ModelProvider | `budget.hard.boundaryMarginPercent` | `5` |
| ModelProvider, ToolProvider | `healthCheck.enabled` | `true`, when the block is present |
| Agent | `lifecycle.activitySource` | `gatewayTraffic` |
| Agent | `service.enabled` | `true`, when the block is present |
| AgentTask | `completion.condition` | `agentReported` |
| AgentTask | `completion.onTimeout` | `Fail` |
| AgentChannel | `type` | `webhook` |
| AgentChannel | `webhook.responseMode` | `sync` |
| AgentChannel | `webhook.maxPendingAsyncResponses` | `100` |
| AgentChannel | `webhook.auth.hmac.algorithm`, `.encoding` | `sha256`, `hex` |
| AgentChannel | `discord.contentOption` | `message` |

A default on a field inside an optional block fires only when the block is present. An omitted `healthCheck` or `service` block is read as enabled by the reconciler, which is the same outcome.

**Class defaults** are applied by the reconciler when it derives the effective spec, and the stored spec is not changed. Effective values can be derived by merging the class defaults at read time:

| Workload field | Defaults from | Applies when |
|---|---|---|
| `image` | `AgentClass.spec.image.defaultImage` | The workload omits `image` |
| `resources` | `AgentClass.spec.resources.defaults`, applied whole | The workload sets neither `requests` nor `limits` |
| `persistence.sizeGi` | `AgentClass.spec.persistence.defaultSizeGi` | The workload omits `sizeGi` and sets no `existingClaim` |
| `lifecycle.idleTimeout` | `AgentClass.spec.lifecycle.defaultIdleTimeout` | The Agent omits it |
| `lifecycle.hibernationDelay` | `AgentClass.spec.lifecycle.defaultHibernationDelay` | The Agent omits it |
| `lifecycle.wakeTimeout` | `AgentClass.spec.lifecycle.defaultWakeTimeout` | The Agent omits it; with no class default either, the gateway uses 120 seconds |

Fixed values with no class source: the health port is 8080, the Service port defaults to 8080, an Agent's memory mounts at `/var/agent/memory`, and a task's workspace at `/var/task/workspace`. An AgentTask's `completion.timeout` and `ttlSecondsAfterFinished` have no default from either source; unset, they are unbounded ([AgentTask](agenttask.md#timeout-and-retention-are-unbounded-by-default)).
