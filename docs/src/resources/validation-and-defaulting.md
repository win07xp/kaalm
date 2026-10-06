# Validation and defaulting

Kaalm runs no admission webhook server, validating or mutating. Every check is enforced by the apiserver at apply time, from the CRD schema, or by a reconciler, which reports a violation as status. That keeps the control plane free of a webhook availability dependency, and it lets rules that span resources surface on existing objects as recoverable status rather than blocked writes.

This page explains where each check is enforced and indexes the numbered rules. The pages under it state the rules, the schema checks, and the defaults.

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

Other pages cite rules by number, so the numbering never changes. This table is the index: each rule number links to the page that states the rule, and [The rules](#the-rules) lists those pages. The numbered rules cover cross-resource and semantic checks; [Schema validation](validation/schema-and-defaulting.md#schema-validation) lists the field-level checks.

| Rule | Guards | Enforced by | On violation |
|---|---|---|---|
| [1](validation/references-and-access.md) | `agentClassRef` names an existing AgentClass | Agent and AgentTask reconcilers | `Ready=False`, `InvalidReference` |
| [2](validation/class-policy.md) | The workload image is on the class allowlist | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| [3](validation/references-and-access.md) | `providers[].providerRef` names an existing ModelProvider | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| [4](validation/references-and-access.md) | The ModelProvider admits the workload's namespace | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| [5](validation/references-and-access.md) | The ModelProvider is on the class `allowedProviders` | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| [6](validation/class-policy.md) | Resource limits within the class cap | Agent and AgentTask reconcilers | Clamped to the cap, never rejected |
| [7](validation/class-policy.md) | Volume size within the class cap | Agent and AgentTask reconcilers | Clamped |
| [8](validation/class-policy.md) | Idle timeout within the class cap | AgentReconciler | Clamped |
| [9](validation/class-policy.md) | Wake timeout within the class cap | AgentReconciler | Clamped |
| [10](validation/class-policy.md) | Hibernation delay within the class cap | AgentReconciler | Clamped |
| [11](validation/providers.md) | Fallback chains terminate | ModelProviderReconciler | `Ready=False`, `FallbackIneligible` |
| [12](validation/providers.md) | Fallback stays within translatable formats | ModelProviderReconciler; the gateway per candidate | `Ready=False`, `FallbackIneligible` |
| [13](validation/references-and-access.md) | `agentRef` names an existing Agent | AgentChannelReconciler | `phase=Failed`, `Ready=False`, `AgentNotFound` |
| [14](validation/channels.md) | The target Agent has a Service | AgentChannelReconciler | `Ready=False`, `AgentServiceDisabled` |
| [15](validation/channels.md) | The channel path sits under its own namespace prefix and is unique | AgentChannelReconciler; the gateway | `Ready=False`, `InvalidPath` or `PathConflict` |
| [16](validation/channels.md) | The channel path is not under `/v1/` | CRD CEL | Rejected at apply |
| [17](validation/names-and-tasks.md) | An exit-code task declares no artifacts | CRD CEL | Rejected at apply |
| [18](validation/providers.md) | `degradeTo` names a catalog model | ModelProviderReconciler | `Ready=False`, `InvalidDegradeTarget` |
| [19](validation/class-policy.md) | Egress CIDRs are well-formed | AgentClassReconciler | `Ready=False`, `InvalidCIDR`, message names the entry |
| [20](validation/class-policy.md) | Egress hosts are DNS names, and the class reports FQDN support | AgentClassReconciler | Malformed: `Ready=False`, `InvalidReference`; unsupported: Warning `FQDNPolicyUnsupported`, Ready unaffected |
| [21](validation/names-and-tasks.md) | Agent and AgentTask names are DNS labels | CRD CEL | Rejected at apply |
| [22](validation/channels.md) | `callbackUrl` is HTTPS and not internal | AgentChannelReconciler; the gateway on every delivery | `Ready=False`, `InvalidCallbackUrl`; a host that does not resolve gets a `CallbackHostUnresolved` Warning event |
| [23](validation/references-and-access.md) | Image pull Secrets exist in the workload's namespace | Agent and AgentTask reconcilers | `Ready=False`, `ImagePullSecretMissing` |
| [24](validation/class-policy.md) | Persistence is allowed by the class | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `PersistenceNotAllowed` |
| [25](validation/channels.md) | `callbackUrl` has `callbackAuth`, whose Secret resolves | CRD CEL; AgentChannelReconciler | Rejected at apply; `Ready=False`, `CallbackAuthMissing` or `CallbackAuthInvalid` |
| [26](validation/class-policy.md) | Hibernation is allowed by the class | AgentReconciler | `Degraded`, `HibernationNotAllowed` |
| [27](validation/references-and-access.md) | `existingClaim` excludes `sizeGi`, and the claim exists | CRD CEL; AgentReconciler | Rejected at apply; `Ready=False`, `ExistingClaimNotFound` |
| [28](validation/names-and-tasks.md) | No workloads in the system namespace | Agent, AgentTask, and AgentChannel reconcilers | `Ready=False`, `SystemNamespaceForbidden`; an AgentChannel also gets `phase=Failed` |
| [29](validation/class-policy.md) | Hibernation requires persistence | AgentReconciler | `Degraded`, `HibernationRequiresPersistence` |
| [30](validation/class-policy.md) | Handler mounts are allowed by the class | AgentReconciler | `Degraded`, `HandlerMountNotAllowed` |
| [31](validation/references-and-access.md) | The handler ConfigMap exists | AgentReconciler | `Ready=False`, `HandlerConfigMapNotFound` |
| [32](validation/providers.md) | Hard enforcement has a block policy | CRD CEL | Rejected at apply |
| [33](validation/providers.md) | Hard enforcement has a fully priced catalog | ModelProviderReconciler | `Ready=False`, `HardBudgetUnpriced` |
| [34](validation/providers.md) | The boundary margin sits below every block threshold | CRD CEL | Rejected at apply |
| [35](validation/references-and-access.md) | `tools[].providerRef` names an existing ToolProvider | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| [36](validation/references-and-access.md) | The ToolProvider admits the workload's namespace | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| [37](validation/references-and-access.md) | The ToolProvider is on the class `allowedToolProviders` | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ClassConstraintViolation` |
| [38](validation/references-and-access.md) | Granted tools exist in the declared catalog | Agent and AgentTask reconcilers | Agent `Degraded`, AgentTask `Failed`, `ToolNotInCatalog` |
| [39](validation/channels.md) | The channel type matches its configuration block | CRD CEL | Rejected at apply |
| [40](validation/channels.md) | Platform credentials resolve with the required keys | AgentChannelReconciler | `Ready=False`, `CredentialsMissing` or `CredentialsInvalid` |
| [41](validation/providers.md) | A model map names real models on both ends | ModelProviderReconciler | `Ready=False`, `InvalidModelMap` |
| [42](validation/class-policy.md) | Task timeout within the class cap | AgentTaskReconciler | Clamped |
| [43](validation/class-policy.md) | Task retention within the class cap | AgentTaskReconciler | Clamped |
| [44](validation/class-policy.md) | `lifecycle.maxUnavailableOnDrift` is an integer of at least 1, or a percentage from 1% to 100% | CRD CEL | Rejected at apply |
| [45](validation/channels.md) | Every Secret a channel references carries the label `kaalm.io/channel-credential: "true"` | AgentChannelReconciler; the gateway on every read | `Ready=False`, `SecretNotOptedIn`, and a `Warning` event with the same reason |
| [46](validation/channels.md) | A Secret used as a bearer `callbackAuth` lists the `callbackUrl` host in its `kaalm.io/callback-hosts` annotation | AgentChannelReconciler; the gateway before every callback attempt | `Ready=False`, `CallbackHostNotApproved`, and a `Warning` event with the same reason |
| [47](validation/class-policy.md) | `AgentClass.spec.allowedNamespaces`, when set, is not empty, and admits the workload's namespace | CRD CEL for the list; Agent and AgentTask reconcilers and the gateway for the namespace | Empty list: rejected at apply. Namespace not admitted: Agent `Degraded`, AgentTask `Failed`, `NamespaceNotAllowed`, and the gateway answers `403 access_denied` |
| [48](validation/references-and-access.md) | Every Secret a workload's `spec.env` reads through `valueFrom.secretKeyRef` exists in its namespace and carries the label `kaalm.io/workload-secret: "true"` | Agent and AgentTask reconcilers | `Ready=False`, `SecretNotOptedIn`, and a `Warning` event with the same reason |
| [49](validation/providers.md) | Every Secret a ModelProvider names, or a ToolProvider names when `credentialsRef` is set, carries the label `kaalm.io/provider-credential: "true"` | ModelProviderReconciler and ToolProviderReconciler; the gateway on every credential read | `Ready=False`, `SecretNotOptedIn`, and a `Warning` event with the same reason |
| [50](validation/providers.md) | That Secret lists the host of the provider's `spec.endpoint` in its `kaalm.io/provider-hosts` annotation | ModelProviderReconciler and ToolProviderReconciler; the gateway on every credential read | `Ready=False`, `EndpointHostNotApproved`, and a `Warning` event |
| [51](validation/references-and-access.md) | Every `allowedNamespaces` entry on an AgentClass, ModelProvider, or ToolProvider is a well-formed glob pattern | AgentClassReconciler, ModelProviderReconciler, and ToolProviderReconciler | `Ready=False`, `InvalidNamespacePattern`, with the message naming the entry, and a `Warning` event |
| [52](validation/class-policy.md) | Every `image.allowedImages` entry on an AgentClass is a well-formed glob pattern | AgentClassReconciler | `Ready=False`, `InvalidImagePattern`, with the message naming the entry, and a `Warning` event |
| [53](validation/class-policy.md) | Container resource `claims` have no effect | Agent, AgentTask, and AgentClass reconcilers | A `Warning` event `ResourceClaimsIgnored` on the object that sets them, with `Ready` unaffected |

Two families behave differently from the rest. The class-mismatch family (rules 2 to 5, 24, 26, 29, 30, 35 to 38, and 47) is recoverable on an Agent and terminal on an AgentTask, and a change to an AgentClass or ModelProvider can trigger any of its rules on a workload that was fine a moment ago ([Change propagation](../controller/change-propagation.md#agentclass-change-handling)). The cap family (rules 6 to 10, 42, and 43) never rejects: the effective value is the smaller of what the workload asked for and the class cap.

Three reconcile-time outcomes carry no number. A per-channel Role or RoleBinding (the controller-only check Role or the gateway's credential Role) that the channel cannot write sets `Ready=False` on the AgentChannel: `reason=ChildConflict` when an object of that name exists that the channel does not control, and `reason=ChildWriteRejected` when the API server refuses the write. A transient API error sets no reason and is retried ([AgentChannel reconciler](../controller/reconcilers/agentchannel.md#per-channel-credential-roles)). The two ModelProvider advisory checks, `DegradeTargetNotCheapest` and `MaxOutputTokensUnset`, each set an advisory condition, emit a `Warning` event when it turns `True`, and leave `Ready` unaffected ([ModelProvider](modelprovider.md#degradeto-validation)).

### The rules

Each rule states what must hold, where it is enforced, and why. The rule pages group them by what they protect, and the [rules index](#where-each-rule-is-enforced) links each rule number to its page.

- [Reference and access rules](validation/references-and-access.md): references that must resolve, and the access gates on providers and tools.
- [Class policy rules](validation/class-policy.md): what the class allows, the caps it sets, and its egress entries.
- [Name, namespace, and task rules](validation/names-and-tasks.md): workload names, the system namespace, and task artifacts.
- [Channel rules](validation/channels.md): the target Agent, the channel path, callbacks, and channel credentials.
- [Provider rules](validation/providers.md): fallback, budgets, and provider credentials.
- [Schema validation and defaulting](validation/schema-and-defaulting.md): the field-level schema checks without a rule number, and the defaults.
