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
Kaalm completely and installing 2.0.0. **Removing Kaalm deletes every Kaalm
resource in the cluster.**

1. Keep a copy of your manifests:

   ```bash
   kubectl get agentchannels,agentclasses,agents,agenttasks,modelproviders,toolproviders -A -o yaml > kaalm-backup.yaml
   ```

2. Delete the Kaalm resources while the old controller is still running. The
   controller holds a finalizer on each one and removes it when the object is
   deleted, so deleting after the uninstall leaves objects stuck in
   `Terminating`:

   ```bash
   kubectl delete agents,agenttasks,agentchannels -A --all
   kubectl delete agentclasses,modelproviders,toolproviders --all
   ```

3. Uninstall the release:

   ```bash
   helm uninstall kaalm --namespace kaalm-system
   ```

4. Delete the six CRDs. Helm never deletes the `crds/` directory on uninstall,
   so the old CRDs stay behind until you delete them:

   ```bash
   kubectl delete crd agentchannels.kaalm.io agentclasses.kaalm.io agents.kaalm.io \
     agenttasks.kaalm.io modelproviders.kaalm.io toolproviders.kaalm.io
   ```

5. Install 2.0.0 as described in [Installation](installation.md), then
   reapply your manifests.

---

*How this works: design book pages Operations, API versioning and deprecation
(the deprecation policy); Operations, Deployment (the rolling upgrade order).*
