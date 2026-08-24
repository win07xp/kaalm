# Upgrading Kaalm

An upgrade touches two things: the chart's resources, which `helm upgrade`
manages, and the CRDs, which it does not. Helm installs the `crds/` directory
once, on first install, and never upgrades it. So every Kaalm upgrade is two
steps, CRDs first:

## 1. Apply the new chart's CRDs

Pull the chart version you are upgrading to and apply its `crds/` directory:

```bash
helm pull oci://ghcr.io/win07xp/charts/kaalm --version <version> --untar
kubectl apply --server-side --force-conflicts -f kaalm/crds/
```

`--force-conflicts` is part of the command: Helm owns every CRD field from
the install, and the first server-side apply takes that ownership over.

Find `<version>` on the
[Releases page](https://github.com/win07xp/kaalm/releases). If you work from
a checkout of the repository, `charts/kaalm/crds/` is the same directory.

## 2. Upgrade the release

```bash
helm upgrade kaalm oci://ghcr.io/win07xp/charts/kaalm \
  --version <version> \
  -n kaalm-system \
  --set certManager.clusterResourceNamespace=cert-manager
```

Pass the same `--set` values as your install; `helm upgrade` resets anything
you leave out to the chart's defaults. With `--wait`, the command returns when
the controller and gateway rollouts are complete.

Running agents are not restarted by either step. The controller replaces an
agent Pod only when the Pod's own spec changes, and a Kaalm upgrade does not
change it.

## Upgrading across v0.6.0

v0.6.0 graduates the API: `v1beta1` becomes the storage version and the
version the components speak, and `v1alpha1` stays served, deprecated, and
converted by the controller. The upgrade is still the same two steps, with
one window worth knowing about.

**Between step 1 and step 2**, reads keep working, but a write at `v1alpha1`,
including `kubectl apply` of an existing manifest, fails with a conversion
error: the new CRDs already store at `v1beta1`, and no replica of the old
controller serves the conversion webhook. The old controller's own status
writes hit the same error, so expect reconcile errors in its log and
`Warning` events on workloads for the length of the rollout, typically under
a minute. Everything recovers on its own the moment the first new replica is
Ready; nothing needs to be reapplied. The gateway does not write these
objects, so message delivery and LLM traffic continue throughout.

During the upgrade, Helm reports that it skipped deleting the `standard`
AgentClass. That is expected: the chart leaves its sample class alone for
this one upgrade (nothing can read or write it until the new controller is
up) and adopts it again on your next `helm upgrade`. The class stays in
place throughout.

**After step 2**, verify the storage migration finished. The controller
rewrites every stored object at `v1beta1` on its first start and then trims
each CRD's status:

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

**Downgrading across v0.6.0 is not supported.** Once objects are stored at
`v1beta1`, a chart whose CRDs predate that version cannot serve them. Keep
your manifests; if you must roll back, reinstall the old version fresh and
reapply them.

*How this works: design book pages Operations, API Versioning and Deprecation
(the storage migration, the conversion webhook, and the deprecation policy);
Operations, Deployment (the rolling upgrade order).*
