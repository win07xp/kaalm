# Rules: references and access

## Referenced objects must exist

**Rule 1: Class references must resolve.** `Agent.spec.agentClassRef` and `AgentTask.spec.agentClassRef` must name an existing AgentClass. *Reconcile time; `Ready=False, reason=InvalidReference`.*

**Rule 3: Provider references must resolve.** Every `providers[].providerRef` on an Agent or AgentTask must name an existing ModelProvider. *Reconcile time; the class-mismatch handling, `reason=ClassConstraintViolation`.* It shares its outcome with rules 4 and 5 because a provider that vanishes and a provider that stops admitting the workload look the same to the workload.

**Rule 13: Channel targets must resolve.** `AgentChannel.spec.agentRef` must name an existing Agent in the channel's namespace. *Reconcile time; `phase=Failed`, `Ready=False, reason=AgentNotFound`.*

**Rule 23: Image pull Secrets named by the class must exist where the workload runs.** Every `AgentClass.spec.image.imagePullSecrets[*].name` must exist as a Secret in the referencing workload's namespace. *Reconcile time, before the Pod is created; `Ready=False, reason=ImagePullSecretMissing`, message naming the namespace and Secret.* AgentClass is cluster-scoped but Secrets are namespaced, and the controller never copies Secrets across namespaces ([AgentClass design notes](../agentclass.md#design-notes)). The controller holds no standing Secret read in a user namespace, so it reads each named Secret under a Role scoped to those names ([Operator ServiceAccount](../../security/rbac.md#operator-serviceaccount)).

**Rule 27: A pre-existing claim excludes a provisioning size, and the claim must exist.** `Agent.spec.persistence.existingClaim` must not be combined with `persistence.sizeGi`, and must name a PersistentVolumeClaim in the Agent's namespace. *The exclusion is CRD CEL on the `persistence` block; the existence check is reconcile time, before the Pod is created, `Ready=False, reason=ExistingClaimNotFound`.* Rule 24 still applies whether the PVC is Kaalm-provisioned or pre-existing. The field exists only on the Agent schema ([Agent design notes](../agent.md#persistenceexistingclaim)).

**Rule 31: The handler ConfigMap must exist where the Agent runs.** `Agent.spec.handler.configMapRef.name` must name a ConfigMap in the Agent's namespace. *Reconcile time, before the Pod is created; `Ready=False, reason=HandlerConfigMapNotFound`, message naming the namespace and ConfigMap.* A clear condition beats a Pod wedged in `ContainerCreating` on a missing volume source. The reconciler adds no ownerRef and tracks no content ([Handler update semantics](../../runtime/base-images.md#handler-update-semantics)).

**Rule 48: A workload's env may read only Secrets that opted in.** Every Secret that an Agent's or AgentTask's `spec.env` reads through `valueFrom.secretKeyRef` must exist in the workload's namespace and carry the label `kaalm.io/workload-secret` with the exact value `"true"`. Optional references (`optional: true`) are checked too, and a missing optional Secret also gates. Only `spec.env` is checked, not the `KAALM_*` variables the controller injects. *Reconcile time, before the Pod is created and never as an apply-time rejection; `Ready=False, reason=SecretNotOptedIn`, and a `Warning` event with the same reason when the reason first appears on `Ready`.*

The condition message names the environment variable and the Secret, and is the same for a missing Secret and an unlabeled one, so status does not tell a developer whether a Secret name exists. A `secretKeyRef` with an empty name gets `Ready=False, reason=InvalidReference`. A labeled Secret that lacks the referenced key is not checked: the Pod shows `CreateContainerConfigError`.

The two kinds gate differently:

- An Agent is checked on every pass that reaches the ready gates ([Agent Reconcile steps](../../controller/reconcilers/agent.md)). A running Pod stays in place when its Secret loses the label or is deleted: the Agent keeps its phase and shows `Ready=False`, and no replacement Pod is made (for drift, Pod loss, or wake) until the Secret is labeled or the reference is removed. Labeling the Secret clears the gate on the next pass.
- An AgentTask is checked only while it has no Pod and is `Pending` or `Provisioning`, which covers the first attempt and every `backoffLimit` retry. The gate is not terminal: the task stays out of `Failed` and waits. A task that has a Pod or has finished is not checked, so a finished task needs no label ([AgentTaskReconciler](../../controller/reconcilers/agenttask.md)).

While the check fails, either kind re-checks every 30 seconds, because Secrets are not watched. The label is separate from `kaalm.io/channel-credential` (rule 45) and `kaalm.io/provider-credential` (rule 49): each label covers one use, and a Secret used two ways needs both labels. There is no per-class or global switch, and the rule applies in every namespace, including one where developers can manage Secrets ([Roles for people](../../security/rbac.md#persona-roles)). A workload can name any Secret in its namespace, so the label separates a Secret meant for workloads from every other Secret there; whoever manages Secrets sets it, and writing a workload does not. To read the label, the operator reads every Secret the env names, labeled or not, under a per-workload Role ([Per-workload env-Secret Role](../../security/rbac.md#operator-serviceaccount)).

**Rule 35: Tool references must resolve.** Every `tools[].providerRef` on an Agent or AgentTask must name an existing ToolProvider. *Reconcile time; the class-mismatch handling, `reason=ClassConstraintViolation`.* The rule 3 analog for the [tool plane](../../gateways/tool-plane.md).

## Access gates on providers and tools

These rules and rules 3 and 35 are the gate chain drawn on [Core concepts](../../concepts/core-concepts.md#the-custom-resources): the class allows the provider, the workload asks for it, and the provider admits the namespace. All of them run at reconcile time, because each compares two resources, and all of them share one outcome: `phase=Degraded, reason=ClassConstraintViolation` on an Agent, `phase=Failed` on an AgentTask.

**Rule 4: A provider must admit the workload's namespace.** Every referenced ModelProvider must list the workload's namespace in `allowedNamespaces`.

**Rule 5: A provider must be on the class allowlist.** Every referenced ModelProvider must appear in the AgentClass's `allowedProviders`.

**Rule 36: A ToolProvider must admit the workload's namespace.** Every referenced ToolProvider must list the workload's namespace in `allowedNamespaces`, the rule 4 analog.

**Rule 37: A ToolProvider must be on the class allowlist.** Every referenced ToolProvider must appear in the AgentClass's `allowedToolProviders`, the rule 5 analog.

**Rule 38: Granted tools must exist in a declared catalog.** When a ToolProvider declares a `tools` catalog, every tool name in a workload's grant must appear in it. *Reconcile time; the class-mismatch handling with its own `reason=ToolNotInCatalog`, naming the missing tools.* When no catalog is declared, the server's own `tools/list` governs and this rule does not apply.
