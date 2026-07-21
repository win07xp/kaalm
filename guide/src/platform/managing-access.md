# Managing Team Access

Who may use which provider is decided by three stacked gates. Granting access
means opening all three; revoking means closing any one of them. This page is
the checklist for both, plus what your tenants observe when a gate closes.

## The three gates

For an Agentry-managed workload (an Agent or AgentTask calling the gateway
with its client certificate), every LLM request passes:

1. **The workload's own list**: the provider must appear in the workload's
   `spec.providers`.
2. **The class gate**: the provider must appear in the workload's AgentClass
   `allowedProviders`.
3. **The provider's namespace allowlist**: the calling namespace must match
   `allowedNamespaces` (globs supported).

All three failures return `403` with error type `access_denied` and a message
naming the failed gate. The namespace gate is checked **before** model
existence, deliberately: a namespace without access never learns which models
a provider hosts.

Existing (non-Agentry) workloads calling the gateway with a ServiceAccount
token face only gate 3 plus the model catalog; they have no workload spec or
class.

## Granting a team access

Three edits, one per gate; in practice you own the first two and the team
owns the third:

```bash
# 1. Provider allowlist (yours)
kubectl patch modelprovider anthropic-shared --type=json \
  -p='[{"op":"add","path":"/spec/allowedNamespaces/-","value":"team-new"}]'

# 2. Class allowlist (yours, once per provider, not per team)
kubectl get agentclass standard -o jsonpath='{.spec.allowedProviders}'

# 3. The team lists the provider in their Agent's spec.providers (theirs)
```

Prefer exact namespace names in `allowedNamespaces`; use globs like
`team-*` only when your namespace naming convention makes them safe.

## Revoking a team

Remove the namespace from `allowedNamespaces`. Two things happen, in order:

- **Immediately**: the gateway denies the namespace's next LLM call with
  `403 access_denied`.
- **Within a reconcile**: the controller, watching the ModelProvider,
  transitions the affected Agents to `phase=Degraded,
  reason=ClassConstraintViolation`, so the revocation is visible in plain
  `kubectl get agents` output in the team's namespace.

The Pods keep running; only LLM access is gone. That is deliberate: revocation
is not an eviction, and the team can still drain state off the agents before
you delete the namespace.

An AgentTask denied at provisioning time (rather than mid-run) fails
terminally instead of degrading; there is no point retrying a gate that will
not open.

## Auditing with kubectl alone

```bash
# Who may use this provider?
kubectl get modelprovider anthropic-shared -o jsonpath='{.spec.allowedNamespaces}'

# How much is each class used?
kubectl get agentclasses          # Agents and Tasks columns count live users

# Is anything currently locked out?
kubectl get agents -A | grep Degraded
```

---

*How this works: design book pages Concepts, Tenancy and Tiers (the gate
order and which error each returns), Resources, ModelProvider (glob
semantics), and Controller, Change Propagation (how a provider edit reaches
Agent status).*
