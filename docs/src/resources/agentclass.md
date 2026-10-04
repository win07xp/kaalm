# AgentClass

AgentClass is a cluster-scoped policy resource. It describes the runtime configuration, isolation, resource defaults, and allowed providers for a category of agents. It is analogous to StorageClass: developers reference an AgentClass by name in their Agent or AgentTask spec, and the platform team controls what each class permits.

This split is the core of Kaalm's governance model. Developers pick a class; the class decides which namespaces may use it, what images may run, how much compute they get, where their traffic may go, and which LLM providers and tool servers they may call. When a live AgentClass is edited, the change propagates to running workloads along the paths in [AgentClass change handling](../controller/change-propagation.md#agentclass-change-handling). The reconciler is [AgentClassReconciler](../controller/reconcilers/agentclass.md).

## Spec

The full spec, annotated:

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentClass
metadata:
  name: standard
spec:
  runtime:
    # "pod" is the only accepted value and the schema default.
    backend: pod
    # Optional. When set, must name a RuntimeClass that exists on the cluster
    # (for example gvisor); Pod admission fails otherwise. Omit to use the
    # cluster's default container runtime.
    # runtimeClassName: gvisor

  image:
    # Glob patterns (path.Match). A workload image must match one when the
    # list is non-empty. Empty list: any image, with no warning.
    allowedImages:
      - "registry.internal.corp/agents/*"
      - "ghcr.io/myorg/agents/*:v*"
    # Applied at reconcile time when the workload omits its image.
    defaultImage: "registry.internal.corp/agents/base:v1"
    # Whether Agents of this class may mount a handler ConfigMap into a
    # reference base image (Agent.spec.handler). Default false; rule 30.
    allowHandlerMounts: false
    # Always | Never | IfNotPresent.
    pullPolicy: IfNotPresent
    # Resolved in the workload's namespace at Pod creation; rule 23.
    imagePullSecrets:
      - name: registry-credentials

  resources:
    # Applied whole, and only when the workload sets neither requests nor
    # limits. See the design notes.
    defaults:
      requests: { cpu: "500m", memory: "1Gi" }
      limits:   { cpu: "1",    memory: "2Gi" }
    # Clamps at reconcile time, never rejects; rule 6.
    maxLimits:
      cpu: "4"
      memory: "8Gi"

  persistence:
    # When false, workloads of this class cannot request a PVC; rule 24.
    enabled: true
    # Applied at reconcile time when the workload omits sizeGi.
    defaultSizeGi: 5
    # Clamps sizeGi; rule 7.
    maxSizeGi: 50
    storageClassName: "standard"
    # What happens to a Kaalm-provisioned Agent PVC when the Agent is
    # deleted. Schema default Delete. Distinct from the PV reclaim policy.
    pvcRetention: Delete           # Delete | Retain

  # ModelProviders workloads of this class may reference; rule 5.
  # Empty list: none.
  allowedProviders:
    - name: anthropic-shared
    - name: openai-fallback

  # ToolProviders workloads of this class may be granted; rule 37.
  # Empty list: none.
  allowedToolProviders:
    - name: search-tools

  # Namespaces whose Agents and AgentTasks may use this class, as path.Match
  # globs. Unset: every namespace. An empty list is rejected at apply, so omit
  # the field to admit every namespace; rule 47.
  allowedNamespaces:
    - "team-*"

  network:
    egress:
      # CIDR blocks the synthesized NetworkPolicy allows beyond the gateway
      # and cluster DNS, on every port. Enforced on any CNI that implements
      # NetworkPolicy. Malformed entries: rule 19.
      allowedCIDRs:
        - "10.42.0.0/16"
        - "140.82.112.0/20"
      # DNS names. Validated (rule 20). On Cilium each workload gets a
      # CiliumNetworkPolicy that allows these hosts on every port; on other
      # CNIs the hosts are ignored (FQDNPolicySupported).
      allowedHosts:
        - "mcp.internal.corp"
        - "api.github.com"
    # Deprecated, no effect: no Pod Kaalm creates uses host networking,
    # whatever the value. Setting it to true reports DeprecatedFields.
    allowHostNetwork: false
    # When true, an Agent's NetworkPolicy admits the namespace's other
    # agent Pods on the agent's health port. Agents only; a task's policy
    # admits no ingress. Default false: only the gateway reaches the agent.
    allowSameNamespaceIngress: false

  # Merged over the restricted baseline: a set field wins, an unset field
  # takes the baseline value. This block is the baseline plus runAsUser.
  security:
    podSecurityContext:
      runAsNonRoot: true
      runAsUser: 10001
      seccompProfile: { type: RuntimeDefault }
    containerSecurityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: { drop: ["ALL"] }
    # Mounts the workload ServiceAccount token into every Pod of the class.
    # Default false: the mTLS certificate is the only credential.
    automountServiceAccountToken: false

  lifecycle:
    # Default and clamp for Agent.spec.lifecycle.idleTimeout; rule 8.
    defaultIdleTimeout: "30m"
    maxIdleTimeout: "24h"
    # When false, Agents of this class cannot enable hibernation; rule 26.
    hibernationAllowed: true
    # Default and clamp for hibernationDelay; rule 10.
    defaultHibernationDelay: "30m"
    maxHibernationDelay: "2h"
    # Default and clamp for wakeTimeout; rule 9.
    defaultWakeTimeout: "2m"
    maxWakeTimeout: "5m"
    # Default and clamp for AgentTask.spec.completion.timeout; rule 42.
    # Unset default: a task that sets no timeout has none.
    defaultTaskTimeout: "1h"
    maxTaskTimeout: "6h"
    # Default and clamp for AgentTask.spec.ttlSecondsAfterFinished; rule 43.
    # Unset default: a settled task that sets no TTL is kept.
    defaultTTLSecondsAfterFinished: 86400
    maxTTLSecondsAfterFinished: 604800
    terminationGracePeriodSeconds: 60
    # Caps concurrent Pod replacements for drift: an integer of at
    # least 1, or a percentage of the class's Agents from 1% to 100%,
    # rounded up. Schema default 25%; rule 44.
    maxUnavailableOnDrift: "25%"

  # Merged onto every workload Pod.
  podMetadata:
    labels:
      cost-center: "platform"
    annotations: {}
```

## Status

```yaml
status:
  observedGeneration: 3
  conditions:
    - type: Ready
      status: "True"
      reason: AllReferencesResolved
      message: "class is valid"
      lastTransitionTime: "2026-04-05T12:00:00Z"
    - type: FQDNPolicySupported
      status: "True"
      reason: NoHostsRequested
    - type: SecurityBaseline
      status: "True"
      reason: RestrictedBaseline
    - type: CertificateCleanup
      status: "True"
      reason: OwnerRefEnabled
  agentsInUse: 14
  tasksInUse: 2
  agentsReplacing: 1
  agentsPendingReplacement: 3
```

| Condition | Meaning |
|---|---|
| `Ready` | `True` with `reason: AllReferencesResolved` when every `allowedProviders` and `allowedToolProviders` entry names an existing provider, every `allowedCIDRs` entry parses (rule 19), every `allowedHosts` entry is a DNS name (rule 20), and every `allowedNamespaces` entry is a valid pattern (rule 51). Otherwise `False`, and the message lists every problem. The reason is `InvalidCIDR` when any `allowedCIDRs` entry does not parse, else `InvalidNamespacePattern` when any `allowedNamespaces` entry is malformed, else `InvalidReference` (a missing provider or tool provider, or a malformed host). Provider health is not consulted. While a delete waits on a referrer, `Ready` is `False` with `reason: DeletionBlocked` instead ([Cluster-scoped resources](../controller/finalizers.md#cluster-scoped-resources)). |
| `FQDNPolicySupported` | Present on every class: `reason: NoHostsRequested` while `allowedHosts` is empty; otherwise `FQDNPolicySupported` or `FQDNPolicyUnsupported` from the CNI probe described under the design notes, which also sends a `Warning` event of that reason when the condition first turns `False`. |
| `SecurityBaseline` | Present on every class: `True, reason: RestrictedBaseline` when no declared `security` field falls below the restricted Pod Security Standard; otherwise `False, reason: BelowRestrictedBaseline` with a message naming each relaxed field, and a `Warning` event of the same reason when the relaxation first appears. An unset field is never a deviation: it takes the baseline value. |
| `DeprecatedFields` | Advisory, and present only once the class sets a deprecated field. `True, reason: DeprecatedFieldSet` while it does, with a message naming each such field. The only deprecated field is `network.allowHostNetwork` (see [Deprecation policy](../operations/api-versioning.md#deprecation-policy)). A `Warning` event of reason `DeprecatedFieldSet` follows each time the condition turns `True`. When the class stops setting the field, the condition becomes `False, reason: NoDeprecatedFields`, and no event is sent. A class that never set a deprecated field has no such condition. It never affects `Ready` or any workload. |
| `CertificateCleanup` | A cluster capability, present on every class like `FQDNPolicySupported`: whether cert-manager runs with `--enable-certificate-owner-ref=true` ([In-cluster TLS](../security/tls.md#in-cluster-tls)). `True, reason: OwnerRefEnabled` when the flag is set, so deleting an Agent or AgentTask deletes its TLS Secret. `False, reason: OwnerRefDisabled` when it is not, so a deleted workload's TLS Secret is orphaned; setting the flag later flips this to `True` without recreating workloads. `Unknown, reason: ControllerSecretNotFound` when the controller's own `kaalm-controller-tls` Secret is missing. It never affects `Ready` and emits no Event. |

`agentsInUse` and `tasksInUse` count the Agents and AgentTasks referencing the class, so the platform team can see what a change affects. They still count a workload whose namespace the class does not admit, because it still references the class. `agentsReplacing` and `agentsPendingReplacement` break down the Agents already counted in `agentsInUse` that are mid drift replacement: holding a `maxUnavailableOnDrift` slot, or waiting for one. `kubectl get ac` prints `agentsInUse`, `tasksInUse`, and `agentsReplacing` as the `Agents`, `Tasks`, and `Replacing` columns.

## Design notes

### An image allowlist is mandatory in practice

An empty `allowedImages` admits any image with no warning, so leave it empty only in throwaway environments.

### Defaults and maxLimits

`resources.defaults` applies whole, and only when the workload sets neither `requests` nor `limits`. A workload that sets either one gets no class defaults for the other.

The schema accepts `claims` in `resources.defaults` and in an Agent's or AgentTask's `spec.resources`, because each is a full `ResourceRequirements` block. The controller removes `claims` from every workload container, whether or not the class sets `maxLimits`. A container claim must name an entry in the Pod's `spec.resourceClaims`, which Kaalm never sets, so a Kaalm workload cannot use Dynamic Resource Allocation claims. Setting `claims` has no effect, and a block with only `claims` counts as unset, so the class defaults still apply.

`maxLimits` clamps and never rejects, at reconcile time (rule 6): a limit above the cap is lowered to it, a request above the cap is lowered to it, and a resource named in `maxLimits` that the workload leaves without a limit is given the cap as its limit. The same holds for `maxSizeGi`, `maxIdleTimeout`, `maxHibernationDelay`, `maxWakeTimeout`, `maxTaskTimeout`, and `maxTTLSecondsAfterFinished` (rules 7 to 10, 42, and 43). A lifecycle max bounds a value and never supplies one, so a class that bounds every task sets the default as well ([AgentTask](agenttask.md#the-class-bounds-timeout-and-retention)). The full default-versus-cap table is on [Defaulting](validation/schema-and-defaulting.md#defaulting).

### `pvcRetention`

The field governs the PVC Kaalm provisions for an Agent. Under `Retain`, the PVC survives the Agent's deletion ([Finalizers](../controller/finalizers.md)). Two cases are outside it: a PVC referenced by `Agent.spec.persistence.existingClaim` never carries an ownerRef and survives deletion under either value, and an AgentTask's workspace PVC is always removed with the task, whatever the class sets.

### `allowHandlerMounts` guards the image review boundary, not code execution

Anyone who can create an Agent can already run arbitrary code: any image matching `allowedImages`. What a mounted handler ([`Agent.spec.handler`](agent.md), consumed by the [reference base images](../runtime/base-images.md)) adds is code that bypasses image review: the platform team approved `kaalm-agent-python`, not the handler source injected into it. The field therefore defaults to `false`, and enabling it is the class-level statement that ConfigMap authorship in a namespace is an acceptable code provenance for that category of workload. Classes for production fleets built from reviewed images leave it off; a starter or development class turns it on. Enforcement is rule 30. What the grant means in RBAC terms is stated once in the [threat model](../security/threat-model.md#workload-isolation).

### `maxUnavailableOnDrift` paces spec-drift replacements

The field bounds how many of the class's Agents may have their Pod replaced for spec drift at once, whether the drift comes from an edit to the Agent's own spec, from a class or provider change, or from a Certificate that names a [different TLS Secret than the Pod mounts](../controller/change-propagation.md#a-pod-that-mounts-another-tls-secret-is-replaced). The mechanics, including how a slot is granted and freed and what happens when a replacement fails, are under [Drift replacements are capped per class](../controller/change-propagation.md#drift-replacements-are-capped-per-class).

### `automountServiceAccountToken` is the opt-in for API access

Each Agent and AgentTask runs as a ServiceAccount of its own with no RoleBinding, and by default its Pod does not mount that ServiceAccount's token. Setting `security.automountServiceAccountToken: true` mounts the token into every Pod of the class, so a Role and RoleBinding that a developer or platform team binds to the workload's ServiceAccount give the agent Kubernetes API access. The gateway still rejects the token from a Kaalm-managed Pod, so the opt-in adds an API server credential and nothing at the gateway. A change to the field replaces every running Agent Pod of the class ([Change propagation](../controller/change-propagation.md#bucket-1-recreate-and-clamp-default)). See [Agent Pod ServiceAccount](../security/rbac.md#agent-pod-serviceaccount).

### Image pattern glob semantics

Patterns in `allowedImages` use Go's [`path.Match`](https://pkg.go.dev/path#Match) rules:

- `*` does not cross path separators. `registry.internal.corp/agents/*` matches `registry.internal.corp/agents/foo:latest` but not `registry.internal.corp/agents/team/foo:latest`. Use a multi-segment pattern for nested paths.
- Digest references do match. `*` matches any run of non-`/` characters, `@` and `:` included, so `registry.internal.corp/agents/*` matches `registry.internal.corp/agents/foo@sha256:...`. To exclude digests, anchor the tag (`registry.internal.corp/agents/*:v*` works because a hex digest contains no `v`) or list permitted digests explicitly.

### `image.pullPolicy` values

The schema accepts `Always`, `Never`, or `IfNotPresent`, the three Kubernetes pull policies, and rejects any other value at apply time.

### `security` starts from the restricted baseline

The controller applies the `restricted` Pod Security Standard to every workload Pod and merges the class's `security` block over it field by field ([Pod Security Standards](../security/model.md#pod-security-standards)). A class therefore declares only what it changes: `runAsUser` to pin a UID, or `readOnlyRootFilesystem: false` for an image that writes outside `/tmp` and its volumes. Relaxing a field is allowed, and the `SecurityBaseline` condition and its `Warning` make it visible. A read-only root gets an `emptyDir` at `/tmp`. A change to the block replaces every running Agent Pod of the class ([Change propagation](../controller/change-propagation.md#bucket-1-recreate-and-clamp-default)).

### Network egress: `allowedCIDRs` and `allowedHosts`

`allowedCIDRs` is the portable primitive. Each entry becomes a `NetworkPolicy` egress rule to that `ipBlock`, on every port, which every CNI that implements NetworkPolicy enforces. The full synthesized rule set is on [Child resources](../runtime/child-resources.md#what-the-synthesized-networkpolicy-protects).

`allowedHosts` cannot be expressed in standard `NetworkPolicy`, so the controller writes it as a second, CNI-specific policy. The controller checks once per process whether the API server serves the `ciliumnetworkpolicies` resource in `cilium.io/v2`, so installing Cilium later takes effect after a controller restart. It reports the answer in `FQDNPolicySupported`, with a `Warning` event when the class sets `allowedHosts` and the resource is not served ([CNI FQDN-policy probe](../controller/reconcilers/agentclass.md#cni-fqdn-policy-probe)). When the CNI is Cilium, every Agent and AgentTask of the class gets a `CiliumNetworkPolicy` named `{name}-fqdn` with a `toFQDNs` rule for each host on every port, plus the DNS rule Cilium needs to learn the hosts' addresses ([Child resources](../runtime/child-resources.md#fqdn-egress-policy)). Cilium allows the union of this policy and the NetworkPolicy. On a CNI without Cilium, `allowedHosts` has no effect and `allowedCIDRs` alone governs egress.

An invalid `allowedCIDRs` entry makes the class `Ready=False` (rule 19). Every Agent and new AgentTask under the class then reports `Ready=False, reason=InvalidReference`, and the reconciler writes no Certificate, NetworkPolicy, or Pod for it. A running Pod keeps running. Fixing the entry lets the workloads converge on their next pass.

### Provider access gates

`allowedProviders` is one gate in a chain. For a full-lifecycle Agent or AgentTask, the workload's own `spec.providers`, this class's `allowedProviders`, and the target `ModelProvider.allowedNamespaces` must all admit the request, and the model must exist in the provider's catalog. In the gateway-only tier the class layer does not exist: those callers reference no AgentClass, and `ModelProvider.allowedNamespaces` is the only tenancy check they face. The enforced chain, with the error each gate produces, is on [Provider access gating](../concepts/tenancy-and-tiers.md#provider-access-gating). The tool chain is the same, gate for gate, with `allowedToolProviders` (rule 37) in this class's place ([Grants](../gateways/tool-plane.md#grants)).

### `allowedNamespaces` keeps a class to some teams

RBAC cannot restrict which AgentClass a developer names in `agentClassRef` ([Persona roles](../security/rbac.md#persona-roles)). `allowedNamespaces` does: it lists `path.Match` glob patterns, such as `team-*`, for the namespaces whose Agents and AgentTasks may use the class (rule 47).

The field reads differently from the provider fields of the same name:

- **Unset admits every namespace.** `["*"]` admits every namespace explicitly.
- **An empty list is rejected at apply**, by the CRD CEL rule `size(self) > 0`. On a ModelProvider or ToolProvider, an empty `allowedNamespaces` admits none. A JSON patch that removes the last entry leaves `[]` and is rejected, so remove the field instead.
- **A malformed pattern matches nothing**, such as `[`, as it does on a provider. The class shows `Ready=False, reason: InvalidNamespacePattern` naming the entry ([rule 51](validation/references-and-access.md#access-gates-on-providers-and-tools)), and its valid entries keep admitting their namespaces.

When the class does not admit a workload's namespace:

- **An Agent goes `Degraded`** (`Ready=False` and a `Warning` event) with `reason: NamespaceNotAllowed`. This is the first Degraded check, so it is the reported reason when several mismatches exist. No Certificate, child, or Pod is created for a new Agent, and a running Agent keeps its Pod. Adding the namespace back, or removing the field, restores the prior phase. Rules 28 and 1 run earlier and keep priority ([Degraded](../controller/agent-lifecycle.md#degraded)).
- **An AgentTask with no Pod**, in `Pending` or `Provisioning` or retrying, settles terminal `Failed` (`Completed=False` and `Ready=False`, `reason: NamespaceNotAllowed`, and a `Warning` event), whatever `backoffLimit` remains. This is the first pre-Pod check. A task that already has a Pod keeps running, and a terminal task is unaffected ([AgentTask lifecycle](../controller/task-lifecycle.md#transition-triggers)).
- **The gateway answers `403 access_denied`** with a message naming the namespace and the class on LLM and MCP tool calls from a Kaalm-managed workload, before it checks the class's `allowedProviders` or `allowedToolProviders`. Gateway-only callers have no class and are not affected ([Provider access gating](../concepts/tenancy-and-tiers.md#provider-access-gating)).

The list check is CEL because it reads one field. The namespace match is reconcile time because CEL cannot read `metadata.namespace`.

A downgrade, or a chart upgrade in progress, drops the restriction silently: a CRD, controller, or gateway that lacks the field ignores it.

### `imagePullSecrets` namespace resolution

AgentClass is cluster-scoped, but `imagePullSecrets[*].name` references a Secret, and Secrets live in namespaces. The reconciler resolves each entry in the workload's namespace at Pod-creation time, never in `kaalm-system`, and copies nothing across namespaces. A missing Secret sets `Ready=False, reason=ImagePullSecretMissing` on the workload and the Pod is not created (rule 23).

### `runtime.backend` accepts only `pod`

The schema enum rejects any other value at apply time, so the author gets immediate feedback. The field exists so that another backend is an additive enum change on the frozen v1beta1 schema. Isolation comes from `runtime.runtimeClassName` ([Runtime isolation](../concepts/system-architecture.md#runtime-isolation)).
