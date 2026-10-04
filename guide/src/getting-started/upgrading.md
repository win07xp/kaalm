# Upgrading Kaalm

An upgrade touches two things: the chart's resources, which `helm upgrade`
manages, and the CRDs, which it does not. Helm installs the `crds/` directory
once, on first install, and never upgrades it. So every Kaalm upgrade is two
steps, CRDs first:

## 1. Apply the new chart's CRDs

Pull the chart version you are upgrading to and apply its `crds/` directory:

```bash
helm pull oci://ghcr.io/win07xp/charts/kaalm --version VERSION --untar
kubectl apply --server-side --force-conflicts -f kaalm/crds/
```

`--force-conflicts` is part of the command: Helm is the field manager for every
CRD field from the install, and the first server-side apply takes that
management over.

Replace `VERSION` with the release you are upgrading to, listed on the
[Releases page](https://github.com/win07xp/kaalm/releases). If you work from
a checkout of the repository, `charts/kaalm/crds/` is the same directory.

## 2. Upgrade the release

```bash
helm upgrade kaalm oci://ghcr.io/win07xp/charts/kaalm \
  --version VERSION \
  --namespace kaalm-system \
  --set certManager.clusterResourceNamespace=cert-manager \
  --wait
```

Pass the same `--set` values as your install; `helm upgrade` resets anything
you leave out to the chart's defaults. With `--wait`, the command returns when
the controller and gateway rollouts are complete. The design book's
[Deployment](https://github.com/win07xp/kaalm/blob/main/docs/src/operations/deployment.md#helm-chart-upgrades)
page lists the values whose change has workload-visible effects.

Running agents are not restarted by either step. The controller replaces an
agent Pod only when the Pod's own spec changes, and a Kaalm upgrade does not
change it. One effect is visible: the gateway rollout resets its in-memory
activity state, so idle and hibernation transitions defer for one
`idleTimeout` afterwards; on a cluster with multi-hour idle timeouts, schedule
the upgrade with that in mind. Before and after the two steps, a running
agent and a finished task read the same:

```bash
kubectl get agents,agenttasks -n team-demo
```

```text
NAME                               PHASE     READY   CLASS      AGE
agent.kaalm.io/support-assistant   Running   True    standard   4m39s

NAME                                PHASE       CLASS      AGE
agenttask.kaalm.io/nightly-report   Succeeded   standard   102s
```

and their Pods keep their names and ages, while `helm history kaalm -n
kaalm-system` shows the new revision as `deployed`.

## Upgrading across v0.6.0

v0.6.0 graduates the API: `v1beta1` becomes the storage version and the
version the components speak, and `v1alpha1` stays served, deprecated, and
converted by the controller. The upgrade is still the same two steps, with
one window to know about.

**Between step 1 and step 2**, reads at `v1alpha1` keep working, but a write
at `v1alpha1`, including `kubectl apply` of an existing manifest, and a read
at `v1beta1` of an object still stored at `v1alpha1` fail with a conversion
error, because no replica of the old controller serves the conversion webhook.
The old controller's own status writes fail the same way, so expect reconcile
errors in its log and `Warning` events on workloads for the length of the
rollout, typically under a minute. Everything recovers on its own the moment
the first new replica is Ready; nothing needs to be reapplied. Message
delivery and LLM traffic continue throughout. The design book explains the
window on
[API versioning and deprecation](https://github.com/win07xp/kaalm/blob/main/docs/src/operations/api-versioning.md#upgrading-in-place).

During the upgrade, Helm reports that it skipped deleting the `standard`
AgentClass. That is expected: the chart leaves its sample class alone for
this one upgrade and adopts it again on your next `helm upgrade`. The class
stays in place throughout.

**After step 2**, verify the storage migration finished. The controller
migrates the stored objects to `v1beta1` on its own:

```bash
kubectl get crd agents.kaalm.io -o jsonpath='{.status.storedVersions}'
```

Expect `["v1beta1"]` (and the same for the other five CRDs, usually within
seconds of the rollout).

Nothing in your manifests has to change. Everything that says
`apiVersion: kaalm.io/v1alpha1` keeps applying and reading back, with one
warning per request:

```text
Warning: kaalm.io/v1alpha1 Agent is deprecated; use kaalm.io/v1beta1
```

The schema is identical, so moving a manifest to `v1beta1` is a change to its
`apiVersion` line and nothing else. Do it as each manifest is next touched;
`v1alpha1` stays served at least through v1.0.0, and a release announces the
removal before it happens.

**Downgrading across v0.6.0 is not supported.** After objects are stored at
`v1beta1`, a chart whose CRDs predate that version cannot serve them. Keep
your manifests; if you must roll back, reinstall the old version fresh and
reapply them.

## Upgrading across v1.1.0

From v1.1.0, three Secret checks are on, with no switch and no grandfathering,
and nothing is rejected at apply:

- An AgentChannel may use only Secrets that carry the label
  `kaalm.io/channel-credential: "true"`.
- An Agent or AgentTask may read through `spec.env` only Secrets that carry the
  label `kaalm.io/workload-secret: "true"`.
- A ModelProvider may use only a Secret that carries the label
  `kaalm.io/provider-credential: "true"` and lists the host of its `endpoint`
  in the annotation `kaalm.io/provider-hosts`. A ToolProvider follows the
  same rule when it sets `credentialsRef`.

Each label covers one use, so a Secret used two ways needs both labels. The
previous release ignores the labels and the annotations, so set them before
you upgrade and nothing goes down. The first three parts cover channels,
workloads, and providers. The fourth runs the upgrade, and the last covers
the TLS Secret names.

### Label channel Secrets

After the upgrade, a channel whose Secret has no label shows `Ready=False`
with the reason `SecretNotOptedIn` and a `Warning` event, and the gateway
stops routing to it until you label the Secret.

1. List the Secrets that AgentChannels reference. Each row shows the
   namespace, the channel, and the Secret names from the inbound `auth`
   Secret (`secretRef`, then `hmac`), the `callbackAuth` Secret (`secretRef`,
   then `hmac`), the Discord Secret, and the WhatsApp Secret. A channel
   leaves the columns it does not use empty:

   ```bash
   kubectl get agentchannels -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.webhook.auth.secretRef.name}{"\t"}{.spec.webhook.auth.hmac.secretRef.name}{"\t"}{.spec.webhook.callbackAuth.secretRef.name}{"\t"}{.spec.webhook.callbackAuth.hmac.secretRef.name}{"\t"}{.spec.discord.credentialsRef.name}{"\t"}{.spec.whatsapp.credentialsRef.name}{"\n"}{end}'
   ```

2. Review each Secret. A channel can name any Secret in its namespace, so
   confirm that each one was created for the channel.
3. Label only the Secrets created for a channel:

   ```bash
   kubectl label secret SECRET_NAME -n NAMESPACE kaalm.io/channel-credential=true
   ```

   Replace `SECRET_NAME` with the Secret's name and `NAMESPACE` with its
   namespace. Only the exact value `true` counts.
4. For a channel whose `callbackAuth` is `bearer`, also list the host of its
   `callbackUrl` on the Secret:

   ```bash
   kubectl annotate secret SECRET_NAME -n NAMESPACE kaalm.io/callback-hosts=HOST
   ```

   Replace `HOST` with the bare hostname, such as `receiver.example.com`. For
   more than one host, separate the names with commas. HMAC callbacks need no
   annotation.

A Secret you label after the upgrade brings its channel back to `Ready=True`
as soon as you label it. The controller also narrows the channel's
credential Role to the labeled Secrets; the design book lists the Roles on
[RBAC and authentication](https://github.com/win07xp/kaalm/blob/main/docs/src/security/rbac.md#operator-serviceaccount).

### Label workload Secrets

After the upgrade, an Agent whose `spec.env` names a Secret without the
workload label shows `Ready=False` with the reason `SecretNotOptedIn` and a
`Warning` event. The Agent keeps its phase and its running Pod. Until you
label the Secret, the controller makes no replacement Pod, no idle or
hibernation transition, and no phase update from the Pod. A finished AgentTask
needs nothing. A pending or retrying AgentTask waits without failing.

1. List the Secrets that Agents and AgentTasks read in `spec.env`. Each row
   shows the namespace, the workload, and the Secret names:

   ```bash
   kubectl get agents,agenttasks -A -o jsonpath='{range .items[*]}{.kind}{"\t"}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.env[*].valueFrom.secretKeyRef.name}{"\n"}{end}'
   ```

2. Review each Secret. A workload can name any Secret in its namespace, so
   confirm that each one was created for workloads.
3. Label only the Secrets created for workloads:

   ```bash
   kubectl label secret SECRET_NAME -n NAMESPACE kaalm.io/workload-secret=true
   ```

   Replace `SECRET_NAME` with the Secret's name and `NAMESPACE` with its
   namespace. Only the exact value `true` counts.

A Secret that fails the review is one that a workload reads but that was not
created for workloads. Do not label it. Treat it as exposed, because the
workload's Pod already holds its value in the environment:

1. Remove the `spec.env` entry that reads the Secret. The spec change replaces
   an Agent's Pod. If the Pod stays, delete it. For an AgentTask that has a
   Pod, delete the task.
2. Rotate the Secret.

A workload that fails the check is re-checked every 30 seconds, so a Secret you
label after the upgrade clears the workload on the next pass. The controller
also creates a Role and RoleBinding for each workload that reads a Secret
(see [RBAC and authentication](https://github.com/win07xp/kaalm/blob/main/docs/src/security/rbac.md#operator-serviceaccount)).

### Label provider and tool Secrets

After the upgrade, a provider whose Secret has no label shows `Ready=False`
with the reason `SecretNotOptedIn`. A provider whose Secret has the label but
does not list the endpoint host shows `Ready=False` with the reason
`EndpointHostNotApproved`. Each reason comes with a `Warning` event. The
gateway applies the same checks on every call, so an LLM call falls back to
the next provider or returns `503 provider_unavailable`, and a tool call
returns `503 tool_unavailable`, until you fix the Secret.

1. List each ModelProvider with its endpoint and its Secret, then each
   ToolProvider the same way. A ToolProvider with no `credentialsRef` leaves
   the Secret column empty and needs nothing:

   ```bash
   kubectl get modelproviders -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.endpoint}{"\t"}{.spec.credentialsRef.name}{"\n"}{end}'
   kubectl get toolproviders -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.endpoint}{"\t"}{.spec.credentialsRef.name}{"\n"}{end}'
   ```

2. Review each Secret. A provider can name any Secret in `kaalm-system`, so
   confirm that each one was created as the credential of the provider or
   tool server that names it. Never label `kaalm-gateway-session-key` or a
   cert-manager `*-tls` Secret: the label opts the Secret in, which defeats
   rule 49.
3. Label only the Secrets created as provider credentials:

   ```bash
   kubectl label secret SECRET_NAME -n kaalm-system kaalm.io/provider-credential=true
   ```

   Replace `SECRET_NAME` with the Secret's name. Only the exact value `true`
   counts.
4. Annotate each labeled Secret with the hostnames of the endpoints that use it:

   ```bash
   kubectl annotate secret SECRET_NAME -n kaalm-system kaalm.io/provider-hosts=HOSTS --overwrite
   ```

   Replace `HOSTS` with the hostname from each endpoint, without the scheme,
   port, or path, such as `api.anthropic.com` for `https://api.anthropic.com`.
   For more than one host, separate the names with commas. A Secret that
   several providers share, including a ModelProvider and a ToolProvider,
   lists every one of their hosts.

A provider that fails a check recovers on its own: setting the label or the
annotation on the Secret brings it back to `Ready=True` on the next pass,
with no edit to the provider. If you re-create a Secret later, by delete and
create or through External Secrets, keep both.

### Run the upgrade

With the Secrets labeled, run the two upgrade steps at the top of this page.

To find a held-back channel, workload, or provider, see these sections of
[Troubleshooting](../reference/troubleshooting.md):

- [Channel is `Ready=False` with `SecretNotOptedIn` or `CallbackHostNotApproved`](../reference/troubleshooting.md#channel-is-readyfalse-with-secretnotoptedin-or-callbackhostnotapproved)
- [Workload is `Ready=False` with `SecretNotOptedIn`](../reference/troubleshooting.md#workload-is-readyfalse-with-secretnotoptedin)
- [ModelProvider `Ready=False`](../reference/troubleshooting.md#modelprovider-readyfalse)

The design book states the rules, 45, 46, 48, 49, and 50, on
[Validation and defaulting](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/validation-and-defaulting.md).

### TLS Secret names

An Agent or AgentTask created after the upgrade writes
its certificate to a Secret named `{name}-tls-` plus the first eight characters
of the workload's UID, such as `support-assistant-tls-3f9c2a1e`. The
Certificate is still named `{name}-tls`. A workload whose Certificate existed
before the upgrade keeps its `{name}-tls` Secret, and the upgrade doesn't
replace its Pods. A workload that had no Certificate yet, such as an AgentTask
still `Pending`, gets the UID-suffixed name when the controller creates its
Certificate.

A script or Pod that reads a workload's TLS Secret by name should read
`spec.secretName` from the workload's Certificate instead:

```bash
kubectl get certificate WORKLOAD_NAME-tls -n NAMESPACE -o jsonpath='{.spec.secretName}'
```

Replace `WORKLOAD_NAME` with the name of the Agent or AgentTask and
`NAMESPACE` with its namespace.

If you roll the controller back to v1.0.0, a workload created after the
upgrade can't start a new Pod until you delete its Certificate:

```bash
kubectl delete certificate WORKLOAD_NAME-tls -n NAMESPACE
```

The v1.0.0 controller then re-creates the Certificate with the old Secret name.
The design book states the naming rule on
[Agent certificate](https://github.com/win07xp/kaalm/blob/main/docs/src/controller/reconcilers/agent.md#agent-certificate).

---

*How this works: design book pages Operations, API versioning and deprecation
(the storage migration, the conversion webhook, and the deprecation policy);
Operations, Deployment (the rolling upgrade order); Resources, Validation and
defaulting (rules 45, 46, 48, 49, and 50); Controller, Reconcilers (the Agent certificate).*
