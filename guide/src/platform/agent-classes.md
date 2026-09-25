# Offering agent classes

An AgentClass is the policy contract between you and the teams deploying
agents: which images may run, how much storage an agent may claim, and what
lifecycle behavior is allowed. Teams reference a class by name; they cannot
exceed what it grants.

## A standard class

The chart installs a class named `standard` that allows any image, storage up
to 50Gi, and hibernation, and that names no ModelProvider, so an agent under
it gets a volume but no model access. Name your providers on it with a chart
value, alongside the rest of your install values:

```bash
helm upgrade kaalm oci://ghcr.io/win07xp/charts/kaalm -n kaalm-system \
  --set standardAgentClass.allowedProviders={anthropic-shared}
```

The class reports `Ready=False` with `InvalidReference` while it names a
provider that does not exist yet, so create the ModelProvider first. Either
replace the class with your own policy or disable it
(`--set standardAgentClass.enabled=false`) and ship classes under your own
names. The sample from
`config/samples/kaalm_v1beta1_agentclass.yaml` is a complete policy:

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentClass
metadata:
  name: standard
spec:
  runtime:
    backend: pod
  image:
    allowedImages: ["ghcr.io/win07xp/*"]
  persistence:
    enabled: true
    defaultSizeGi: 5
    maxSizeGi: 50
    pvcRetention: Retain
  allowedProviders:
    - name: anthropic-shared
  lifecycle:
    defaultIdleTimeout: 30m
    hibernationAllowed: true
```

The decisions that matter:

- **`image.allowedImages`** is a glob allowlist. An Agent or AgentTask whose
  image does not match is rejected at reconcile time, so this is your control
  over what code runs as an agent. An empty list allows every image.
- **`allowedProviders`** narrows which ModelProviders workloads of this class
  may use. This gate stacks with the provider's own namespace allowlist: a
  request must pass both.
- **`persistence`** sets the default and ceiling for agent PVCs, and
  `pvcRetention` decides whether state survives agent deletion.
- **`lifecycle`** sets idle-timeout defaults and whether hibernation is
  allowed at all; agents can tighten these within the class maximums.

Apply and verify:

```bash
kubectl apply -f config/samples/kaalm_v1beta1_agentclass.yaml
kubectl get agentclasses
```

Applying over the chart's class prints one warning about a missing
`last-applied-configuration` annotation, because Helm created the object;
`kubectl` patches the annotation in and the apply succeeds. Fields the sample
does not set (the chart's resource defaults and lifecycle ceilings) stay as
they were.

The `AGENTS` and `TASKS` columns count the live users of each class. The Ready
condition (`kubectl describe agentclass standard`) goes True when the spec is
coherent (for example, every `allowedProviders` entry names a ModelProvider
that exists).

## A starter class for handler mounts

The reference base images can run handler source that developers ship as a
ConfigMap (`Agent.spec.handler`; the developer's side is
[Deploying from a base image](../developers/deploying-from-a-base-image.md)).
That capability is off by default, because it changes what `allowedImages`
means: your allowlist is an image review boundary, and a mounted handler
injects code that no image review ever saw. Granting it is a per-class
statement that, for workloads of this category, namespace-level ConfigMap
authorship is acceptable code provenance.

Offer it as its own class rather than loosening a production one:

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentClass
metadata:
  name: starter
spec:
  runtime:
    backend: pod
  image:
    allowedImages:
      - ghcr.io/win07xp/kaalm-agent-go:0.7.0
      - ghcr.io/win07xp/kaalm-agent-python:0.7.0
    allowHandlerMounts: true
  persistence:
    enabled: true
    defaultSizeGi: 1
    maxSizeGi: 5
  lifecycle:
    defaultIdleTimeout: 30m
    hibernationAllowed: true
```

Pinning `allowedImages` to exactly the published base images keeps the grant
narrow: mounted code runs only inside the runtime you chose, never inside an
arbitrary allowlisted image. Flipping `allowHandlerMounts` back to `false`
later degrades existing Agents that mount handlers, with the same recoverable
handling as the other class gates.

## A sandboxed class for code-executing agents

Agents that execute untrusted code (a coding agent running arbitrary build
commands) need a separate class with stricter settings: a
`runtime.runtimeClassName` naming a gVisor or Kata `RuntimeClass` the cluster
has installed, a tighter image allowlist, and lower storage ceilings. Offer it
as a second class, for example `sandboxed`, rather than loosening `standard`;
a class is one object and teams pick by name. Pin a `RuntimeClass` only where
it exists: a class that names one the cluster lacks leaves every Pod that
selects it unschedulable with `RuntimeClass not found`. The class field
`network.allowHostNetwork` is accepted but, as shipped, read by nothing: no
Pod Kaalm creates uses host networking.

## Pod security defaults

The class's `security` block is applied to every Pod and container created
under it from then on; see [Changing a class later](#changing-a-class-later)
for existing Pods. Nothing is set by default; a hardened class looks like this:

```yaml
spec:
  security:
    podSecurityContext:
      runAsNonRoot: true
      runAsUser: 10001
      seccompProfile: { type: RuntimeDefault }
    containerSecurityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: { drop: ["ALL"] }
```

Both fields take the standard Kubernetes `PodSecurityContext` and
`SecurityContext` fields. Check the images your teams run before setting
`readOnlyRootFilesystem`: the reference base images write their memory
store under `/var/agent/memory`, which is a volume only when persistence is
on.

Agent and AgentTask Pods do not mount their ServiceAccount token, so they
have no Kubernetes API access. For a class whose agents need it, such as a
cluster-operations agent, set `security.automountServiceAccountToken: true`
and bind a Role to each workload's ServiceAccount (`agent-{name}` for an
Agent, `task-{name}` for an AgentTask). Leave it off everywhere else: the
token is a second credential a compromised agent could use.

## Labels and annotations on every Pod

`podMetadata` adds labels and annotations to every Pod of the class, for
cost allocation, scheduling, or a service mesh:

```yaml
spec:
  podMetadata:
    labels:
      cost-center: platform
    annotations:
      prometheus.io/scrape: "false"
```

Kaalm's own labels (`kaalm.io/agent` or `kaalm.io/task`, and
`kaalm.io/workload`) are added after yours and cannot be overridden. A
`podMetadata` change reaches an existing Pod only when that Pod is next
replaced for another reason.

## Changing a class later

Which class fields reach a running agent depends on the field:

- `network.egress.allowedCIDRs` and `network.allowSameNamespaceIngress` are
  applied to the existing NetworkPolicy on the next reconcile.
- `lifecycle` defaults and ceilings apply on the next activity evaluation.
- `resources.defaults`, `resources.maxLimits`, and `image.pullPolicy` change
  the desired Pod, so the Pod is replaced on the next reconcile.
- `security`, `podMetadata`, `runtime.runtimeClassName`,
  `image.imagePullSecrets`, and `lifecycle.terminationGracePeriodSeconds` reach
  a Pod only when it is next replaced for another reason.

Tightening `allowedImages` or `allowedProviders` does not stop a running
agent: the Agent goes `Degraded` with `ClassConstraintViolation` at once, its
Pod keeps running, and it recovers when the class or the Agent changes back.
An AgentTask that has no Pod yet settles `Failed`. Plan tightening as a
deprecation, not an eviction.

![Flowchart of what a class or provider edit does to a provisioned Agent, as one cascade. If the stored spec is no longer admitted by the class and providers, the Agent becomes Degraded with the Pod untouched. Otherwise, if the Pod spec hash is unchanged, the children are converged in place with no restart. Otherwise a Hibernated Agent applies the change on its next wake, and any other Agent goes to Provisioning, where the Pod is deleted and created from the new spec.](../diagrams/agentclass-propagation.svg)

---

*How this works: design book pages Resources, AgentClass (every field),
Controller, Change propagation (exactly what a class edit triggers), and
Concepts, Multi-tenancy and adoption tiers (how the class gate stacks with the other two
access gates).*
