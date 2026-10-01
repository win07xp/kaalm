# Managing team access

Who may use which provider is decided by three stacked gates. Granting access
means opening all three; revoking means closing any one of them. This page is
the checklist for both, plus what your tenants observe when a gate closes.

## The three gates

For a Kaalm-managed workload (an Agent or AgentTask calling the gateway
with its client certificate), every LLM request passes:

1. **The workload's own list**: the provider must appear in the workload's
   `spec.providers`.
2. **The class gate**: the provider must appear in the workload's AgentClass
   `allowedProviders`.
3. **The provider's namespace allowlist**: the calling namespace must match
   `allowedNamespaces` (globs supported).

All three failures return `403` with error type `access_denied` and a message
naming the failed gate. The namespace gate is checked before model existence,
so a namespace without access never learns which models a provider hosts.

Existing (non-Kaalm) workloads calling the gateway with a ServiceAccount
token face only gate 3 plus the model catalog; they have no workload spec or
class.

## Granting a team access

Three edits, one per gate; you make the first two and the team makes the
third.

1. Add the namespace to the provider's allowlist:

   ```bash
   kubectl patch modelprovider anthropic-shared --type=json \
     -p='[{"op":"add","path":"/spec/allowedNamespaces/-","value":"team-new"}]'
   ```

2. Confirm the class allows the provider (once per provider, not per team):

   ```bash
   kubectl get agentclass standard -o jsonpath='{.spec.allowedProviders}'
   ```

3. The team lists the provider in their Agent's `spec.providers`.

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

## Kubernetes roles for a team

The chart can install the Kubernetes roles a development team needs and bind
them from your values. They are off by default. Four roles exist: platform
administrators manage the catalog, catalog readers read it, developers run
agents in their own namespace, and secrets administrators manage credential
Secrets. [Roles for
people](https://github.com/win07xp/kaalm/blob/main/docs/src/security/rbac.md#roles-for-people)
in the design book lists what each role grants.

To give a team its roles:

1. Enable the roles and list the subjects in a values file, such as
   `access-values.yaml`:

   ```yaml
   rbac:
     personas:
       enabled: true
       platformAdmins:
         - kind: Group
           name: platform-team
           apiGroup: rbac.authorization.k8s.io
       catalogReaders:
         - kind: Group
           name: team-new-developers
           apiGroup: rbac.authorization.k8s.io
       secretsAdmins:
         - kind: Group
           name: platform-team
           apiGroup: rbac.authorization.k8s.io
       developers:
         team-new:
           - kind: Group
             name: team-new-developers
             apiGroup: rbac.authorization.k8s.io
   ```

2. Upgrade the release with the file. Pass your other install values too,
   because `helm upgrade` resets anything you leave out:

   ```bash
   helm upgrade kaalm oci://ghcr.io/win07xp/charts/kaalm \
     --version VERSION -n kaalm-system \
     -f access-values.yaml
   ```

Subjects are Kubernetes RBAC `Subject` objects, rendered as you write them. A
`User` or `Group` needs `apiGroup: rbac.authorization.k8s.io`, and a
`ServiceAccount` needs a `namespace`. A list you leave empty renders no
binding, and subject values are ignored while `enabled` is `false`.

Each list does one job:

- `catalogReaders` binds the catalog read role cluster-wide. Add every
  developer group to it, because the namespace binding cannot grant the
  cluster-scoped AgentClasses and ModelProviders a developer names in a
  manifest.
- `developers` maps a namespace to the subjects who work in it. Each namespace
  must already exist, or the upgrade fails with `namespace not found`. Create a
  team's namespace first, and add its entry when you onboard the team. For a
  namespace you cannot list in advance, bind ClusterRole `kaalm-developer`
  with a RoleBinding in that namespace yourself.
- `secretsAdmins` binds the Secret role in `kaalm-system` only, for the LLM
  and tool credentials.

If `developers` is not a map (for example, a list), the upgrade fails with
`kaalm: rbac.personas.developers must be a map of namespace to a list of
subjects`.

### Keep channel credentials in team namespaces

Neither the developer role nor the catalog role grants Secrets, so a team creates its
channel and platform credential Secrets under whatever Secret access your
namespace policy already gives it. To let a group manage the Secrets in one
team namespace, bind the Secret role there:

```bash
kubectl create rolebinding kaalm-secrets-admin \
  --clusterrole=kaalm-secrets-admin --group=team-new-leads -n team-new
```

Bind it with a RoleBinding, never a ClusterRoleBinding: a RoleBinding limits
the role to that namespace.

### Let developers exec into agents

Set `rbac.personas.developerExec: true` to add `pods/exec` to the developer
role, for debugging inside agent containers. Enable it only if your policy
allows developers into agent containers. It applies in every namespace where `kaalm-developer` is bound.

### Revoke access

Removing a namespace from `developers` and upgrading deletes that namespace's
RoleBinding, which revokes the team's access. Setting `enabled` to `false`
deletes all four ClusterRoles, so any binding you wrote by hand that
references them stays in place but grants nothing.

### Adopt roles you created by hand

Enabling the roles fails with `exists and cannot be imported into the current
release` if an object the chart would render already exists with the same
kind, name, and namespace. The chart can render these objects:

- ClusterRoles `kaalm-platform-admin`, `kaalm-catalog-reader`,
  `kaalm-developer`, and `kaalm-secrets-admin`.
- ClusterRoleBindings `kaalm-platform-admin` and `kaalm-catalog-reader`.
- RoleBinding `kaalm-secrets-admin` in the release namespace.
- RoleBinding `kaalm-developer` in each namespace listed in `developers`.

The common cases are a hand-written ClusterRole `kaalm-catalog-reader`, its
ClusterRoleBinding if you also named it `kaalm-catalog-reader`, and a
RoleBinding `kaalm-developer`.

A binding's `roleRef` cannot change after you create the binding, and the API
server rejects any update to it. The chart renders every binding with
`roleRef` kind `ClusterRole`: each RoleBinding `kaalm-developer` points at
ClusterRole `kaalm-developer`, RoleBinding `kaalm-secrets-admin` points at
ClusterRole `kaalm-secrets-admin`, and each ClusterRoleBinding points at the
ClusterRole of the same name. Choose by what the existing object references:

- **A ClusterRole, or a binding whose `roleRef` already matches the chart's:**
  delete the object, or adopt it into the release.
- **A binding whose `roleRef` differs from the chart's:** delete it. Adopting it
  makes the upgrade fail with `cannot change roleRef`. The main case is a
  RoleBinding `kaalm-developer` that references a namespaced Role
  `kaalm-developer` (`roleRef` kind `Role`).

To adopt an object, add the Helm label and the release annotations:

```bash
kubectl label clusterrole kaalm-catalog-reader \
  app.kubernetes.io/managed-by=Helm
kubectl annotate clusterrole kaalm-catalog-reader \
  meta.helm.sh/release-name=kaalm meta.helm.sh/release-namespace=kaalm-system
```

For a binding with a matching `roleRef`, run the same two commands on
`clusterrolebinding NAME`, or on `rolebinding NAME` with `-n NAMESPACE`.

To remove a binding that references a namespaced Role, delete it, then run the
upgrade again. Replace `NAMESPACE` with the binding's namespace:

```bash
kubectl delete rolebinding kaalm-developer -n NAMESPACE
```

Nothing then references the namespaced Role `kaalm-developer`, so you can
delete it too:

```bash
kubectl delete role kaalm-developer -n NAMESPACE
```

A namespaced Role named `kaalm-developer` does not collide with the
ClusterRole.

## Auditing with kubectl alone

```bash
# Who may use this provider?
kubectl get modelprovider anthropic-shared -o jsonpath='{.spec.allowedNamespaces}'

# How much is each class used?
kubectl get agentclasses          # Agents and Tasks columns count live users

# Is anything locked out?
kubectl get agents -A | grep Degraded
```

---

*How this works: design book pages Concepts, Multi-tenancy and adoption tiers (the gate
order and which error each returns), Resources, ModelProvider (glob
semantics), Controller, Change propagation (how a provider edit reaches
Agent status), and Security, RBAC and authentication (the persona roles).*
