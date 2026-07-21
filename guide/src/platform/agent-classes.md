# Offering Agent Classes

An AgentClass is the policy contract between you and the teams deploying
agents: which images may run, how much storage an agent may claim, and what
lifecycle behavior is allowed. Teams reference a class by name; they cannot
exceed what it grants.

## A standard class

From `config/samples/agentry_v1alpha1_agentclass.yaml`:

```yaml
apiVersion: agentry.io/v1alpha1
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

The load-bearing decisions:

- **`image.allowedImages`** is a glob allowlist. An Agent or AgentTask whose
  image does not match is rejected at reconcile time, so this is your control
  over what code runs as an agent.
- **`allowedProviders`** narrows which ModelProviders workloads of this class
  may use. This gate stacks with the provider's own namespace allowlist: a
  request must pass both.
- **`persistence`** sets the default and ceiling for agent PVCs, and
  `pvcRetention` decides whether state survives agent deletion.
- **`lifecycle`** sets idle-timeout defaults and whether hibernation is
  allowed at all; agents can tighten these within the class maximums.

Apply and verify:

```bash
kubectl apply -f config/samples/agentry_v1alpha1_agentclass.yaml
kubectl get agentclasses
```

The list shows how many Agents and Tasks currently use each class. The Ready
condition (`kubectl describe agentclass standard`) goes True when the spec is
coherent (for example, every `allowedProviders` entry names a ModelProvider
that exists).

## A sandboxed class for code-executing agents

Agents that execute untrusted code (a coding agent running arbitrary build
commands) deserve a separate class with a stricter posture: a tighter image
allowlist and lower storage ceilings now, and the `agentSandbox` runtime
backend once it lands (post v1). Offer it as a second class, for example
`sandboxed`, rather than loosening `standard`; classes are cheap and teams
pick by name.

## Changing a class later

Class changes propagate to existing workloads on their next reconcile.
Tightening `allowedImages` does not kill a running agent whose image no longer
matches; it blocks the next provisioning. Plan tightening as a deprecation,
not an eviction.

---

*How this works: design book pages Resources, AgentClass (every field),
Controller, Change Propagation (exactly what a class edit triggers), and
Concepts, Tenancy and Tiers (how the class gate stacks with the other two
access gates).*
