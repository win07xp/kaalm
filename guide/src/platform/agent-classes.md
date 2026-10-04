# Offering agent classes

An AgentClass is the policy contract between you and the teams deploying
agents: which images may run, how much storage an agent may claim, and what
lifecycle behavior is allowed. Teams reference a class by name; they cannot
exceed what it grants.

## A standard class

The chart installs a class named `standard`. It allows any image, storage up
to 50Gi, and hibernation, and it stops a task that sets no timeout after one
hour. It names no ModelProvider, so an agent under it gets a volume but no
model access. Name your providers on it with a chart value, alongside the rest
of your install values:

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
- **`allowedNamespaces`** keeps the class to chosen team namespaces, as
  `path.Match` globs such as `team-*`. Unset admits every namespace, and an
  empty list is rejected. See [Keep a class to some
  teams](managing-access.md#keep-a-class-to-some-teams).
- **`persistence`** sets the default and ceiling for agent PVCs, and
  `pvcRetention` decides whether state survives agent deletion.
- **`lifecycle`** sets idle-timeout defaults and whether hibernation is
  allowed at all; agents can tighten these within the class maximums. It also
  sets a default and a maximum for a task's `completion.timeout`
  (`defaultTaskTimeout`, `maxTaskTimeout`) and `ttlSecondsAfterFinished`
  (`defaultTTLSecondsAfterFinished`, `maxTTLSecondsAfterFinished`). A maximum
  never supplies a value, so to bound every task, set the default too. A
  default TTL deletes each finished task's record, results included, when it
  expires.

Apply and verify:

```bash
kubectl apply -f config/samples/kaalm_v1beta1_agentclass.yaml
kubectl get agentclasses
```

Applying over the chart's class prints one warning about a missing
`last-applied-configuration` annotation, because Helm created the object. The
apply still succeeds, and fields the sample does not set (the chart's resource
defaults and lifecycle ceilings) keep their values.

The `AGENTS` and `TASKS` columns count the live users of each class. The
`Ready` condition (`kubectl describe agentclass standard`) goes True when the
spec is valid and every `allowedProviders` entry names an existing
ModelProvider.

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
      - ghcr.io/win07xp/kaalm-agent-go:1.0.0
      - ghcr.io/win07xp/kaalm-agent-python:1.0.0
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
image that a broader `allowedImages` list admits. Setting
`allowHandlerMounts` back to `false` later degrades existing Agents that mount
handlers, with reason `HandlerMountNotAllowed`. As with the other class gates in
[Changing a class later](#changing-a-class-later), the Pod keeps running and
the Agent recovers when the class or the Agent changes back.

## A sandboxed class for code-executing agents

Agents that execute untrusted code (a coding agent running arbitrary build
commands) need a separate class with stricter settings: a
`runtime.runtimeClassName` naming a gVisor or Kata `RuntimeClass` the cluster
has installed, a tighter image allowlist, and lower storage ceilings. Offer it
as a second class, for example `sandboxed`, rather than loosening `standard`;
a class is one object and teams pick by name. Pin a `RuntimeClass` only where
it exists: a class that names one the cluster lacks makes the apiserver reject
every Pod create of the class, so its Agents wait with `Ready=False` and
reason `PodCreateRejected`, and its AgentTasks fail after the provisioning
deadline. For how this shows up, see
[Security model and isolation](https://github.com/win07xp/kaalm/blob/main/docs/src/security/model.md#runtimeclass).
The class field `network.allowHostNetwork` is deprecated and has no effect:
no Pod Kaalm creates uses host networking, whatever its value. A class that
sets it to `true` reports the `DeprecatedFields` condition and a `Warning`
event.

The repository's `config/samples/kaalm_v1beta1_agentclass_sandboxed.yaml` is
a starting point. It names the class `sandboxed` and runs its Pods under the
`gvisor` RuntimeClass (handler `runsc`). The class gives you:

- One allowed image repository, with `allowHandlerMounts` left at `false`.
- Default resource requests and limits, and a maximum limit above which a
  workload cannot go.
- Opt-in persistence with a storage size cap.
- `anthropic-shared` as the only allowed provider.
- No `network` block, so egress is the gateway and cluster DNS only.
- The restricted security baseline, written out.
- A default and a maximum task timeout.

Replace the image repository with yours before you apply the sample. Confirm
that the cluster has the `gvisor` RuntimeClass with
`kubectl get runtimeclass gvisor`. The file is not in
`config/samples/kustomization.yaml`, so apply it on its own:

```bash
kubectl apply -f config/samples/kaalm_v1beta1_agentclass_sandboxed.yaml
```

## Pod security defaults

Every Pod Kaalm creates meets the `restricted` Pod Security Standard by
default: non-root, the runtime default seccomp profile, no privilege
escalation, every capability dropped, and a read-only root with an `emptyDir`
at `/tmp`. The class's `security` block is merged over that baseline field by
field for every Pod created under it from then on; see
[Changing a class later](#changing-a-class-later) for existing Pods. Declare
only what you change. The chart's `standard` class writes the baseline out,
and a class that also pins the UID looks like this:

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
`SecurityContext` fields. Relaxing one is allowed but never silent: the
class reports `SecurityBaseline=False` naming the field, with a `Warning`
event the first time. Check the images your teams run before relying on
the read-only root: the reference base images write their memory store
under `/var/agent/memory`, which is a volume only when persistence is on,
and temp files under `/tmp`, which is always writable. An image that
writes elsewhere needs `readOnlyRootFilesystem: false`.

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
`kaalm.io/workload`) are added after yours and cannot be overridden.

## Changing a class later

Which class fields reach a running agent depends on the field:

- `network.egress.allowedCIDRs` and `network.allowSameNamespaceIngress` reach
  the existing NetworkPolicy without a restart.
- `network.egress.allowedHosts` reaches the agent's CiliumNetworkPolicy without
  a restart, on Cilium only.
- `lifecycle` defaults and ceilings apply on the next activity evaluation.
  The task timeout and TTL bounds are different: a task records them when
  its Pod is created, so an edit reaches only tasks whose Pod, including a
  retry's Pod, is created after it.
- `resources.defaults`, `resources.maxLimits`, `image.pullPolicy`,
  `image.imagePullSecrets`, `security`, `podMetadata`,
  `runtime.runtimeClassName`, and `lifecycle.terminationGracePeriodSeconds`
  change the desired Pod, so the Pod is replaced on the next reconcile.
  `lifecycle.maxUnavailableOnDrift` caps how many of the class's Pods are
  replaced at once (default 25% of the class, rounded up); the rest wait
  their turn as replaced Pods come back Ready. The cap paces the rollout; it
  does not skip any agent Pod, so edit a shared class only when the restarts
  are acceptable. Hibernated agents pick up the change on their next wake.

Tightening `allowedImages`, `allowedProviders`, or `allowedNamespaces` does not
stop a running agent: the Agent goes `Degraded` at once (with
`ClassConstraintViolation`, or `NamespaceNotAllowed` when the class no longer
admits its namespace), its Pod keeps running, and it recovers when the class
or the Agent changes back. An AgentTask that has no Pod yet settles `Failed`.
Plan tightening as a deprecation, not an eviction.

![Flowchart of what a class or provider edit does to a provisioned Agent, as one cascade. If the stored spec is no longer admitted by the class and providers, the Agent becomes Degraded with the Pod untouched. Otherwise, if the Pod spec hash is unchanged, the children are converged in place with no restart. Otherwise a Hibernated Agent applies the change on its next wake, and any other Agent goes to Provisioning, where the Pod is deleted and created from the new spec.](../diagrams/agentclass-propagation.svg)

---

*How this works: design book pages Resources, AgentClass (every field),
Controller, Change propagation (exactly what a class edit triggers), and
Concepts, Multi-tenancy and adoption tiers (how the class gate stacks with the other two
access gates).*
