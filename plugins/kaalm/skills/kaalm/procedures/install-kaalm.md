# Install Kaalm

Use this when Kaalm is not installed. [check-dependencies.md](check-dependencies.md) steps 1 to 3 must pass first: kubectl and Helm, a reachable cluster, and cert-manager and trust-manager.

The skill installs the Kaalm version in the line "This skill matches Kaalm v..." in [SKILL.md](../SKILL.md). Write it without the `v` as `VERSION` below.

## Steps

1. **Pick the namespace.** Use `kaalm-system`, the default in [Installation](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/getting-started/installation.md#install), unless the user asks for another. A *namespace* is a named folder for cluster objects; Kaalm's own parts go in one.
2. **Find cert-manager's cluster resource namespace.** It is `cert-manager` for a default cert-manager install. The chart value `certManager.clusterResourceNamespace` must match it. If you installed cert-manager from [check-dependencies.md](check-dependencies.md), use the namespace you noted.
3. **Install with Helm** through [change-safely.md](change-safely.md). *Helm* installs a *chart*, a package of cluster objects. The command from [Install](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/getting-started/installation.md#install):

   ```bash
   helm --kube-context CONTEXT install kaalm oci://ghcr.io/win07xp/charts/kaalm \
     --version VERSION \
     --namespace kaalm-system --create-namespace \
     --set certManager.clusterResourceNamespace=cert-manager \
     --wait
   ```

   On a first install the server dry run fails with `no matches for kind "AgentClass"`. That is expected: [A cluster that lacks a kind](change-safely.md#a-cluster-that-lacks-a-kind) says what to tell the user. Also tell the user that the install adds Kaalm's own kinds of object to the cluster (CRDs), two controller Pods, and two gateway Pods, and that `--wait` can take up to five minutes on a first install while certificates are issued.
4. **Verify** with the three checks in [Verifying the install](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/getting-started/verifying.md), using `--context CONTEXT`:

   ```bash
   kubectl --context CONTEXT get crds | grep kaalm.io
   kubectl --context CONTEXT get pods -n kaalm-system
   kubectl --context CONTEXT get agentclasses
   ```

   Expect six CRDs, two controller Pods and two gateway Pods all `Running`, and an AgentClass named `standard`. If Pods stay in `ContainerCreating`, follow [the two components](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/getting-started/verifying.md#2-the-two-components).
5. **Save what was installed:**

   ```bash
   python3 SKILL_DIR/scripts/config.py set install.chartVersion VERSION
   python3 SKILL_DIR/scripts/config.py set install.namespace kaalm-system
   ```

## What comes next

The `standard` class names no ModelProvider, so an Agent that asks for one is `Degraded` until a class lists it. Continue with the user's goal: [deploy-an-agent.md](deploy-an-agent.md), [run-a-task.md](run-a-task.md), or [team-setup.md](team-setup.md). A user who wants real model calls needs [add-a-model-key.md](add-a-model-key.md) first.
