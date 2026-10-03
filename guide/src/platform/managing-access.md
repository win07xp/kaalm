# Managing team access

Who may use which provider is decided by three stacked gates. Granting access
means opening all three; revoking means closing any one of them. This page is
the checklist for both, plus what your tenants observe when a gate closes.

## The three gates

For a Kaalm-managed workload (an Agent or AgentTask calling the gateway
with its client certificate), every LLM request passes:

1. **The workload's own list**: the provider must appear in the workload's
   `spec.providers`.
2. **The class gate**: the workload's AgentClass must admit the calling
   namespace in `allowedNamespaces` (when the class sets the field), and the
   provider must appear in its `allowedProviders`.
3. **The provider's namespace allowlist**: the calling namespace must match
   `allowedNamespaces` (globs supported).

Every failure returns `403` with error type `access_denied`. The provider's namespace gate is checked before model
existence, so a namespace without access never learns which models a provider
hosts.

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

2. Confirm the class allows the provider (once per provider, not per team),
   and, if the class sets `allowedNamespaces`, that it admits the team's
   namespace ([Keep a class to some teams](#keep-a-class-to-some-teams)):

   ```bash
   kubectl get agentclass standard -o jsonpath='{.spec.allowedProviders}'
   kubectl get agentclass standard -o jsonpath='{.spec.allowedNamespaces}'
   ```

3. The team lists the provider in their Agent's `spec.providers`.

Prefer exact namespace names in `allowedNamespaces`; use globs like
`team-*` only when your namespace naming convention makes them safe.

## Keep a class to some teams

RBAC cannot limit which AgentClass a developer names in `agentClassRef`, and
removing their read access to a class does not stop it
([RBAC and authentication](https://github.com/win07xp/kaalm/blob/main/docs/src/security/rbac.md#persona-roles)
says why). To keep a class to chosen teams, list their namespaces in the class's
`allowedNamespaces`. Each entry is a `path.Match` glob, such as `team-*`:

```bash
kubectl patch agentclass restricted --type=merge \
  -p='{"spec":{"allowedNamespaces":["team-new","team-data-*"]}}'
```

The controller and the gateway both check the list. An Agent whose namespace
the class does not admit goes `Degraded` with reason `NamespaceNotAllowed`, no
Pod is created for a new Agent, and the gateway answers `403 access_denied` to
its LLM and tool calls. A malformed pattern, such as `[`, matches nothing.

The field behaves differently from the provider allowlists:

- **Unset admits every namespace.** The chart's `standard` class
  leaves it unset. To admit every namespace explicitly,
  list `"*"`.
- **An empty list is rejected.** The API server refuses `[]` and tells you to
  omit the field to admit every namespace. On a provider, an empty list admits
  no namespace.
- **To open the class to everyone again, remove the field.** A JSON patch that
  removes the last entry leaves `[]`, which the API server rejects:

  ```bash
  kubectl patch agentclass restricted --type=json \
    -p='[{"op":"remove","path":"/spec/allowedNamespaces"}]'
  ```

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

Removing a team's namespace from a class's `allowedNamespaces` has the same
effect, with reason `NamespaceNotAllowed`: the gateway denies the next LLM and
tool call, the controller degrades the namespace's Agents, and their Pods keep
running. A new AgentTask of that class fails terminally with reason
`NamespaceNotAllowed`. A task that already
has a Pod keeps running, but the gateway refuses its calls. To restore access,
add the namespace back or remove the field, and the Agents return to their
earlier phase.

## Kubernetes roles for a team

The chart can install the Kubernetes roles a development team needs and bind
them from your values. They are off by default. Four roles exist: platform
administrators manage the catalog, catalog readers read it, developers run
agents in their own namespace, and secrets administrators manage credential
Secrets. [RBAC and
authentication](https://github.com/win07xp/kaalm/blob/main/docs/src/security/rbac.md#roles-for-people)
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
       namespaceSecretsAdmins:
         team-new:
           - kind: Group
             name: team-new-credential-managers
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
  developer group to it, so developers can look up the cluster-scoped
  AgentClasses and ModelProviders they name in a manifest.
- `developers` maps a namespace to the subjects who work in it. Each namespace
  must already exist, or the upgrade fails with `namespace not found`. Create a
  team's namespace first, and add its entry when you onboard the team. For a
  namespace you cannot list in advance, bind ClusterRole `kaalm-developer`
  with a RoleBinding in that namespace yourself.
- `secretsAdmins` binds the Secret role in `kaalm-system` only, for the LLM
  and tool credentials. These people also label each provider Secret and list
  its endpoint hosts ([Label provider Secrets](#label-provider-secrets)).
- `namespaceSecretsAdmins` maps a team namespace to that team's credential
  managers, who own its channel credentials. See
  [Keep channel credentials in team namespaces](#keep-channel-credentials-in-team-namespaces).

If `developers` or `namespaceSecretsAdmins` is not a map (for example, a
list), the upgrade fails and the error names the value.

### Keep channel credentials in team namespaces

Each team owns its channel credentials. Name a credential manager for the
team's namespace in `namespaceSecretsAdmins`, as in the example above. The
chart then binds the Secret role there, and the platform team holds no Secret
rights in team namespaces. The namespace must already exist. Do not list the
release namespace: the upgrade fails. `secretsAdmins` is the list for the
release namespace, which holds the provider and tool credentials.

The credential manager creates each channel Secret and labels it, because a
channel may use only Secrets that carry the label:

```bash
kubectl label secret SECRET_NAME -n NAMESPACE kaalm.io/channel-credential=true
```

Replace `SECRET_NAME` with the Secret's name and `NAMESPACE` with the team's
namespace. A Secret used as a bearer `callbackAuth` token also needs its
approved hosts in the `kaalm.io/callback-hosts` annotation. The label is
required in every namespace, whoever creates the Secret. Developers need no
Secret access to use the channel. [Troubleshooting](../reference/troubleshooting.md#channel-is-readyfalse-with-secretnotoptedin-or-callbackhostnotapproved)
shows what a channel reports when a Secret lacks the label.

### Label the Secrets a workload reads

An Agent or AgentTask can read a Secret in its `spec.env` through
`valueFrom.secretKeyRef` only when the Secret exists in the workload's
namespace and carries the label `kaalm.io/workload-secret` with the value
`true`. Whoever manages Secrets in the namespace sets it:

```bash
kubectl label secret SECRET_NAME -n NAMESPACE kaalm.io/workload-secret=true
```

Replace `SECRET_NAME` with the Secret's name and `NAMESPACE` with the
workload's namespace. The label is separate from
`kaalm.io/channel-credential` and `kaalm.io/provider-credential`: each label
covers one use, so a Secret used two ways needs both labels. There is no
switch to turn the check off. Developers need no Secret access to use a
labeled Secret.
[Troubleshooting](../reference/troubleshooting.md#workload-is-readyfalse-with-secretnotoptedin)
shows what a workload reports when a Secret lacks the label. For the rule, see
[Validation and defaulting](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/validation-and-defaulting.md).

### Label provider Secrets

The people in `secretsAdmins` own the provider and tool credentials in
`kaalm-system`. A provider may use only a Secret that carries the label
`kaalm.io/provider-credential: "true"` and lists the host of its `endpoint` in
the annotation `kaalm.io/provider-hosts`:

```bash
kubectl label secret SECRET_NAME -n kaalm-system kaalm.io/provider-credential=true
kubectl annotate secret SECRET_NAME -n kaalm-system kaalm.io/provider-hosts=HOSTS
```

Replace `SECRET_NAME` with the Secret's name and `HOSTS` with the bare
hostnames of the endpoints that use it, separated by commas. The platform
team that writes providers holds no Secret rights, so it cannot point a
provider at another Secret in `kaalm-system` or send a credential to a host
the credential manager did not approve. [Providing LLM access](llm-access.md#1-create-the-credential-secret) and
[Providing tool access](tool-access.md#1-create-the-credential-secret) show the
Secrets, and
[Troubleshooting](../reference/troubleshooting.md#modelprovider-readyfalse)
shows what a provider reports when a Secret lacks either.

### Let developers manage Secrets

When a team's developers manage Secrets themselves, for example in a dev
loop, set `rbac.personas.developerSecrets: true`. The developer role then
grants read and write access to Secrets in every namespace where
`kaalm-developer` is bound; the exact verbs are in
[RBAC and authentication](https://github.com/win07xp/kaalm/blob/main/docs/src/security/rbac.md#persona-roles).
The setting has no effect while `enabled` is `false`. Developers still label
the Secrets they create, because the channel rule and the workload rule have no
exception.

### Let developers exec into agents

Set `rbac.personas.developerExec: true` to add `pods/exec` to the developer
role, for debugging inside agent containers. Enable it only if your policy
allows developers into agent containers. It applies in every namespace where
`kaalm-developer` is bound.

### Revoke access

Removing a namespace from `developers` or `namespaceSecretsAdmins` and
upgrading deletes that namespace's RoleBinding, which revokes the team's
access. Setting `enabled` to `false` deletes all four ClusterRoles, so any
binding you wrote by hand that references them stays in place but grants
nothing.

### Adopt roles you created by hand

Enabling the roles fails with `exists and cannot be imported into the current
release` if an object the chart would render already exists with the same
kind, name, and namespace. The chart can render these objects:

- ClusterRoles `kaalm-platform-admin`, `kaalm-catalog-reader`,
  `kaalm-developer`, and `kaalm-secrets-admin`.
- ClusterRoleBindings `kaalm-platform-admin` and `kaalm-catalog-reader`.
- RoleBinding `kaalm-secrets-admin` in the release namespace.
- RoleBinding `kaalm-secrets-admin` in each namespace listed in
  `namespaceSecretsAdmins`.
- RoleBinding `kaalm-developer` in each namespace listed in `developers`.

A binding's `roleRef` cannot change after you create the binding, and the API
server rejects any update to it. Every binding the chart renders has
`roleRef` kind `ClusterRole` and points at the ClusterRole of the same name.
Choose by what the existing object references:

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

## Auditing with kubectl alone

```bash
# Who may use this provider?
kubectl get modelprovider anthropic-shared -o jsonpath='{.spec.allowedNamespaces}'

# Which teams may use this class? (no output: every namespace)
kubectl get agentclass restricted -o jsonpath='{.spec.allowedNamespaces}'

# How much is each class used?
kubectl get agentclasses          # Agents and Tasks columns count live users

# Is anything locked out?
kubectl get agents -A | grep Degraded
```

---

*How this works: design book pages Concepts, Multi-tenancy and adoption tiers (the gate
order and which error each returns), Resources, AgentClass (the
`allowedNamespaces` design note) and ModelProvider (glob
semantics), Controller, Change propagation (how a provider edit reaches
Agent status), and Security, RBAC and authentication (the persona roles).*
