# Upgrading Kaalm

This page covers upgrades within 2.x. To move a cluster from 1.x or 0.x, see
[Moving from 1.x or 0.x](#moving-from-1x-or-0x).

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

## Moving from 1.x or 0.x

Kaalm 2.0.0 installs fresh. It has no upgrade path from 1.x or 0.x, so the two
steps above do not apply to those releases. Moving a cluster means removing
Kaalm completely, installing 2.0.0, and reapplying your manifests. If you run
0.x, first upgrade to 1.0.0 with the
[1.0.0 upgrade guide](https://github.com/win07xp/kaalm/blob/v1.0.0/guide/src/getting-started/upgrading.md),
because 0.5.0 and earlier serve only `kaalm.io/v1alpha1`, which 2.0.0 does
not serve. Then follow these steps.

**Removing Kaalm deletes every Kaalm resource in the cluster. It also deletes
each agent's PVC and the data on it, unless the agent's class sets
`persistence.pvcRetention: Retain`. Step 2 sets it.**

1. Keep a copy of your manifests. The selector leaves out the `standard`
   class that the chart installed, because reapplying it fails with a
   conflict and 2.0.0 creates its own:

   ```bash
   kubectl get agentchannels,agentclasses,agents,agenttasks,modelproviders,toolproviders -A \
     -l 'app.kubernetes.io/managed-by!=Helm' -o yaml > kaalm-backup.yaml
   ```

   The copy holds no Secrets, and no step deletes them. If you changed
   `standard`, the 2.0.0 chart takes `allowedProviders` and
   `allowedNamespaces` from the `standardAgentClass` values at install. Edit
   any other field on the new class afterward.

2. For each class that has agents with persistence, including `standard`, set
   `pvcRetention` to `Retain`:

   ```bash
   kubectl patch agentclass CLASS_NAME --type merge -p '{"spec":{"persistence":{"pvcRetention":"Retain"}}}'
   ```

   Replace `CLASS_NAME` with the class name. An agent that you reapply with
   the same name in the same namespace mounts the kept PVC and its data. The
   copy from step 1 still holds each class's original policy, so a reapplied
   class returns to it.

3. Delete the Kaalm resources while the old controller is still running. The
   controller holds a finalizer on each one and removes it when the object is
   deleted, so deleting after the uninstall leaves objects stuck in
   `Terminating`:

   ```bash
   kubectl delete agents,agenttasks,agentchannels -A --all
   kubectl delete agentclasses,modelproviders,toolproviders --all
   ```

4. Uninstall the release. Keep the `kaalm-system` namespace: it holds your
   provider credential Secrets.

   ```bash
   helm uninstall kaalm --namespace kaalm-system
   ```

5. Delete the six CRDs. Helm never deletes the `crds/` directory on uninstall,
   so the old CRDs stay behind until you delete them:

   ```bash
   kubectl delete crd agentchannels.kaalm.io agentclasses.kaalm.io agents.kaalm.io \
     agenttasks.kaalm.io modelproviders.kaalm.io toolproviders.kaalm.io
   ```

6. Edit `kaalm-backup.yaml`:

   - Remove `allowHostNetwork` from `spec.network` in each AgentClass. 2.0.0
     rejects the field, even when it is `false`.
   - Remove each finished AgentTask you do not want to run again. Reapplying
     creates a new object, so the task runs again.

7. Install 2.0.0 as described in [Installation](installation.md). If you reuse
   values from 1.x, such as the output of `helm get values kaalm --all`,
   remove `controller.trustClusterCAForProbes` and `controller.probeCA` first.
   The install fails while either is set. Set `gateway.trustClusterCAForUpstream`
   and `gateway.upstreamCA` instead, which provider health probes also trust.

8. Opt your Secrets in. 2.0.0 uses a Secret only when it carries the label or
   annotation for that use, so label the existing Secrets before you reapply:

   - Channel credentials: the `kaalm.io/channel-credential` label, and for a
     bearer `callbackAuth` token the `kaalm.io/callback-hosts` annotation. See
     [Keep channel credentials in team namespaces](../platform/managing-access.md#keep-channel-credentials-in-team-namespaces).
   - ModelProvider and ToolProvider credentials in `kaalm-system`: the
     `kaalm.io/provider-credential` label and the `kaalm.io/provider-hosts`
     annotation. See
     [Label provider Secrets](../platform/managing-access.md#label-provider-secrets).
   - Secrets that an Agent or AgentTask reads through `spec.env`: the
     `kaalm.io/workload-secret` label. See
     [Label the Secrets a workload reads](../platform/managing-access.md#label-the-secrets-a-workload-reads).

9. Reapply your manifests and check that everything is `Ready`:

   ```bash
   kubectl apply -f kaalm-backup.yaml
   kubectl get agentchannels,modelproviders,toolproviders,agents -A \
     -o custom-columns='KIND:.kind,NAMESPACE:.metadata.namespace,NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,REASON:.status.conditions[?(@.type=="Ready")].reason'
   ```

   A resource with `SecretNotOptedIn` or `EndpointHostNotApproved` is missing
   a label or annotation from step 8. See [Troubleshooting](../reference/troubleshooting.md).

---

*How this works: design book pages Operations, API versioning and deprecation
(the deprecation policy); Operations, Deployment (the rolling upgrade order).*
