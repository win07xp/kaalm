# AgentClass

AgentClass is a cluster-scoped policy resource. It describes the runtime configuration, isolation, resource defaults, and allowed providers for a category of agents. It is analogous to StorageClass: developers reference an AgentClass by name in their Agent or AgentTask spec, and the platform team controls what each class permits.

This split is the core of Kaalm's governance model. Developers pick a class; the class decides what images may run, how much compute they get, where their traffic may go, and which LLM providers and tool servers they may call. When a live AgentClass is edited, the change propagates to running workloads along the paths in [AgentClass change handling](../controller/change-propagation.md#agentclass-change-handling). The reconciler is [AgentClassReconciler](../controller/reconcilers.md#agentclassreconciler).

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
    # Any string is accepted by the schema; an invalid value surfaces only
    # when the kubelet rejects the Pod. See the design notes.
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

  network:
    egress:
      # CIDR blocks the synthesized NetworkPolicy allows beyond the gateway
      # and cluster DNS, on every port. Enforced on any CNI that implements
      # NetworkPolicy. Malformed entries: rule 19.
      allowedCIDRs:
        - "10.42.0.0/16"
        - "140.82.112.0/20"
      # DNS names. Validated (rule 20), and the class reports whether the
      # CNI has an FQDN policy type (FQDNPolicySupported), but the
      # controller synthesizes no FQDN policy from it on any CNI.
      allowedHosts:
        - "mcp.internal.corp"
        - "api.github.com"
    # Accepted by the schema and never applied as shipped (#226).
    allowHostNetwork: false
    # When true, an Agent's NetworkPolicy admits the namespace's other
    # agent Pods on the agent's health port. Agents only; a task's policy
    # admits no ingress. Default false: only the gateway reaches the agent.
    allowSameNamespaceIngress: false

  # Applied verbatim to every workload Pod and container.
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
    # Default false: the mTLS certificate is the only credential. Set it only
    # when a Role bound to the workload's ServiceAccount must reach the API.
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
    # Accepted by the schema and not applied as shipped: the gateway reads
    # the Agent's own wakeTimeout and uses 2m when it is unset (rule 9,
    # #204).
    defaultWakeTimeout: "2m"
    maxWakeTimeout: "5m"
    terminationGracePeriodSeconds: 60

  # Merged onto every workload Pod.
  podMetadata:
    labels:
      cost-center: "platform"
    annotations: {}
```

`resources.defaults` is a full `ResourceRequirements` block, so the schema also accepts `claims`; the controller does not read it.

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
      status: "False"
      reason: NoHostsRequested
  agentsInUse: 14
  tasksInUse: 2
```

| Condition | Meaning |
|---|---|
| `Ready` | `True` with `reason: AllReferencesResolved` when every `allowedProviders` and `allowedToolProviders` entry names an existing provider, every `allowedCIDRs` entry parses (rule 19), and every `allowedHosts` entry is a DNS name (rule 20). Otherwise `False` with `reason: InvalidReference` and a message listing every problem, sorted and joined with `; `. Provider health is not consulted. |
| `FQDNPolicySupported` | Set on every pass: `reason: NoHostsRequested` while `allowedHosts` is empty; otherwise `FQDNPolicySupported` or `FQDNPolicyUnsupported` from the CNI probe described under the design notes. |

`agentsInUse` and `tasksInUse` count the Agents and AgentTasks referencing the class, so the platform team can see what a change affects. `kubectl get ac` prints both counts.

## Design notes

### An image allowlist is mandatory in practice

An empty `allowedImages` means any image, and nothing warns about it. Leave it empty only in throwaway environments.

### Defaults and maxLimits

`resources.defaults` applies whole, and only when the workload sets neither `requests` nor `limits`. A workload that sets either one gets no class defaults for the other.

`maxLimits` clamps and never rejects, at reconcile time (rule 6): a limit above the cap is lowered to it, a request above the cap is lowered to it, and a resource named in `maxLimits` that the workload leaves without a limit is given the cap as its limit. The same holds for `maxSizeGi`, `maxIdleTimeout`, and `maxHibernationDelay` (rules 7, 8, and 10). The full default-versus-cap table is on [Defaulting](validation-and-defaulting.md#defaulting).

### `pvcRetention`

The field governs the PVC Kaalm provisions for an Agent. Under `Retain`, the Agent finalizer strips the PVC's ownerRef before the Agent is removed, so cascade garbage collection leaves the PVC behind ([Finalizers](../controller/finalizers.md)). Two cases are outside it: a PVC referenced by `Agent.spec.persistence.existingClaim` never carries an ownerRef and survives deletion under either value, and an AgentTask's workspace PVC is always removed with the task, whatever the class sets.

### `allowHandlerMounts` guards the image review boundary, not code execution

Anyone who can create an Agent can already run arbitrary code: any image matching `allowedImages`. What a mounted handler ([`Agent.spec.handler`](agent.md), consumed by the [reference base images](../runtime/base-images.md)) adds is code that bypasses image review: the platform team approved `kaalm-agent-python`, not the handler source injected into it. The field therefore defaults to `false`, and enabling it is the class-level statement that ConfigMap authorship in a namespace is an acceptable code provenance for that category of workload. Classes for production fleets built from reviewed images leave it off; a starter or development class turns it on. Enforcement is rule 30, with the same recoverable `Degraded` handling as the persistence and hibernation gates, including on class drift. What the grant means in RBAC terms is stated once in the [threat model](../security/threat-model.md#workload-isolation).

### `automountServiceAccountToken` is the opt-in for API access

Each Agent and AgentTask runs as a ServiceAccount of its own with no RoleBinding, and by default its Pod does not mount that ServiceAccount's token. Setting `security.automountServiceAccountToken: true` mounts the token into every Pod of the class, so a Role and RoleBinding that a developer or platform team binds to the workload's ServiceAccount give the agent Kubernetes API access. The gateway still rejects the token from a Kaalm-managed Pod, so the opt-in adds an API server credential and nothing at the gateway. The field is outside the Pod spec hash, so a change reaches a running Agent when its Pod is next replaced. See [Agent Pod ServiceAccount](../security/rbac.md#agent-pod-serviceaccount).

### Image pattern glob semantics

Patterns in `allowedImages` use Go's [`path.Match`](https://pkg.go.dev/path#Match) rules:

- `*` does not cross path separators. `registry.internal.corp/agents/*` matches `registry.internal.corp/agents/foo:latest` but not `registry.internal.corp/agents/team/foo:latest`. Use a multi-segment pattern for nested paths.
- Digest references do match. `*` matches any run of non-`/` characters, `@` and `:` included, so `registry.internal.corp/agents/*` matches `registry.internal.corp/agents/foo@sha256:...`. To exclude digests, anchor the tag (`registry.internal.corp/agents/*:v*` works because a hex digest contains no `v`) or list permitted digests explicitly.

### `image.pullPolicy` is unvalidated

The schema accepts any string. A typo is stored and surfaces only when the kubelet rejects the Pod; issue #241 tracks an enum on the field.

### Network egress: `allowedCIDRs` and `allowedHosts`

`allowedCIDRs` is the portable primitive. Each entry becomes a `NetworkPolicy` egress rule to that `ipBlock`, on every port, which every CNI that implements NetworkPolicy enforces. The full synthesized rule set is on [Child resources](../runtime/child-resources.md#what-the-synthesized-networkpolicy-protects).

`allowedHosts` cannot be expressed in standard `NetworkPolicy`, and the controller synthesizes no FQDN policy from it on any CNI. What it does: validate the names, probe the cluster's API groups for an FQDN policy type (Cilium's `CiliumNetworkPolicy` or Calico Enterprise's equivalent) the first time a class sets the field, cache the answer for the process lifetime, and report it in `FQDNPolicySupported`, with a `Warning` event when the type is absent. Use `allowedCIDRs` for egress governance. A hostname allowlist is a CNI-native policy the platform team writes beside Kaalm's.

An invalid `allowedCIDRs` entry makes the class `Ready=False`, but the Agent reconciler does not consult the class's `Ready` condition, and the NetworkPolicy write for each Agent then fails with no Agent-visible condition; issue #241 tracks it.

### Provider access gates

`allowedProviders` is one gate in a chain. For a full-lifecycle Agent or AgentTask, the workload's own `spec.providers`, this class's `allowedProviders`, and the target `ModelProvider.allowedNamespaces` must all admit the request, and the model must exist in the provider's catalog. In the gateway-only tier the class layer does not exist: those callers reference no AgentClass, and `ModelProvider.allowedNamespaces` is the only tenancy check they face. The enforced chain, with the error each gate produces, is on [Provider access gating](../concepts/tenancy-and-tiers.md#provider-access-gating). The tool chain is the same, gate for gate, with `allowedToolProviders` (rule 37) in this class's place ([Grants](../gateways/tool-plane.md#grants)).

### `imagePullSecrets` namespace resolution

AgentClass is cluster-scoped, but `imagePullSecrets[*].name` references a Secret, and Secrets live in namespaces. The reconciler resolves each entry in the workload's namespace at Pod-creation time, never in `kaalm-system`, and copies nothing across namespaces. A missing Secret sets `Ready=False, reason=ImagePullSecretMissing` on the workload and the Pod is not created (rule 23).

### `runtime.backend` accepts only `pod`

The schema enum rejects any other value at apply time, so the author gets immediate feedback. The field exists so that another backend is an additive enum change on the frozen v1beta1 schema; isolation comes from `runtime.runtimeClassName` ([Runtime isolation](../concepts/system-architecture.md#runtime-isolation)).
