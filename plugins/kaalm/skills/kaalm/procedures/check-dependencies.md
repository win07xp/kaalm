# Check what Kaalm needs

Check these in order. Stop at the first piece that is missing. Check each piece before you change anything, say in a sentence what the piece is for, and fix it only with the user's OK, using [change-safely.md](change-safely.md). A piece that passes gets one line.

The full list of what Kaalm expects is in [What you need before starting](https://github.com/win07xp/kaalm/blob/main/guide/src/introduction.md#what-you-need-before-starting) and [Prerequisites](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/installation.md#prerequisites).

## 1. kubectl and helm

```bash
kubectl version --client
helm version --short
```

*kubectl* is the remote control for a cluster. *Helm* is the installer that puts Kaalm on it. Helm must be 3.8 or newer, as the learn book says in [Check they worked](https://github.com/win07xp/kaalm/blob/main/learn/src/setting-up.md#check-they-worked), because Kaalm's chart comes from a container registry.

If one is missing, give the user the install page: [kubectl](https://kubernetes.io/docs/tasks/tools/) or [Helm](https://helm.sh/docs/intro/install/). Run an installer yourself only when the user says to.

## 2. A reachable cluster

```bash
kubectl --context CONTEXT version
kubectl --context CONTEXT get nodes
```

A *cluster* is the set of machines that run the user's programs. Kaalm needs Kubernetes 1.35 or newer. It also needs a network layer that enforces NetworkPolicy, because Kaalm uses network rules to keep each agent away from everything except the gateway. k3d clusters enforce them. For a managed cluster, ask whether network policy is turned on.

With no context or no answer from the server, use [get-a-cluster.md](get-a-cluster.md).

## 3. cert-manager and trust-manager

```bash
kubectl --context CONTEXT get crd certificates.cert-manager.io bundles.trust.cert-manager.io
kubectl --context CONTEXT get pods -n cert-manager
```

*cert-manager* issues each agent its own TLS certificate, which is its identity. *trust-manager* shares the certificate authority, so the parts can check each other's identity. Both must be running before Kaalm is installed, and cert-manager needs `--enable-certificate-owner-ref=true`. Every Pod in the namespace must be `Running` with all its containers ready (`READY` such as `1/1`). That covers cert-manager's controller, webhook, and cainjector Pods and trust-manager's Pod. Use the namespace cert-manager is installed in, `cert-manager` by default.

If either is missing, read the "The prerequisites" section of [A cluster in one command](https://github.com/win07xp/kaalm/blob/main/learn/src/a-cluster-in-one-command.md#the-prerequisites) and run its Helm commands for the user. Keep its `--set` values, and note the namespace you install into, which Kaalm's install needs as `certManager.clusterResourceNamespace`.

## 4. Kaalm and its version

Run the version check in the skill's [SKILL.md](../SKILL.md#the-version-check). If Kaalm is missing, use [install-kaalm.md](install-kaalm.md). If it is installed, check it as [Verifying the install](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/verifying.md) describes.

## 5. A model provider key

An agent needs a key only when it calls a language model. The first agent in [try-it-out.md](try-it-out.md) needs none.

```bash
kubectl --context CONTEXT get modelproviders
```

A *ModelProvider* is Kaalm's record of a model service, its prices, and who may use it. If the goal needs a model and `Ready` is not `True`, or the list is empty, use [add-a-model-key.md](add-a-model-key.md), after the agent's namespace exists ([deploy-an-agent.md](deploy-an-agent.md) step 1). The user needs an account with a model provider and a key from it. Do not create the account.
