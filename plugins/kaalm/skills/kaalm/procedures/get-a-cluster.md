# Get a Kubernetes cluster

Use this when [check-dependencies.md](check-dependencies.md) finds no reachable cluster. The user chooses between a local cluster and a managed one. Describe both in plain words, give a default for a user who is new to Kubernetes, and let the user decide.

| | Local k3d cluster | Managed cluster (EKS, GKE, or AKS) |
|---|---|---|
| What it is | A cluster that runs inside Docker on the user's own machine. | A cluster that a cloud company runs for you, in your cloud account. |
| Good for | Trying Kaalm, learning, and throwaway work. | Agents that must stay up, run for a team, or need more than one machine. |
| Needs | Docker, k3d, kubectl, and Helm on the machine. | A cloud account with billing, and the provider's command-line tool. |
| Cost | Nothing beyond the machine. | The cloud account is billed while the cluster exists. |
| Removal | One command. | The user deletes it in the cloud account. |

For a user who wants to try Kaalm, suggest the local cluster.

## Local k3d cluster

1. Check the tools with the commands in [Setting up your laptop](https://github.com/win07xp/kaalm/blob/v2.1.0/learn/src/setting-up.md#check-they-worked). If one is missing, give the user its install link from [Install them](https://github.com/win07xp/kaalm/blob/v2.1.0/learn/src/setting-up.md#install-them). Docker must be running, not only installed.
2. Read [The cluster](https://github.com/win07xp/kaalm/blob/v2.1.0/learn/src/a-cluster-in-one-command.md#the-cluster) and run its `k3d cluster create` command with `--kubeconfig-switch-context=false` added. It has no dry run, so explain it first: it starts a cluster named `kaalm-tutorial` in a Docker container. Ask for the OK. The flag keeps k3d from making the new cluster the user's current kube context, so a cluster the user already works with stays the default for their own `kubectl` commands. Every command here passes `--context`, so the skill does not need the switch.
3. The new context is `k3d-kaalm-tutorial`. Confirm it as [The cluster context](../SKILL.md#the-cluster-context) says, and save it.
4. Check it:

   ```bash
   kubectl --context k3d-kaalm-tutorial get nodes
   ```

   One node with status `Ready` means the cluster works.

To remove the cluster later, use the command in [Tear it down](https://github.com/win07xp/kaalm/blob/v2.1.0/learn/src/where-to-go-next.md#tear-it-down), and only with the user's OK.

## Managed cluster

You do not create the cluster. Tell the user what it involves: a cloud account with billing turned on, the provider's command-line tool signed in, a cluster created in a region, and then a kube context for `kubectl`. Ask which provider they use, and link its own getting-started page:

- Amazon EKS: [Getting started with Amazon EKS](https://docs.aws.amazon.com/eks/latest/userguide/getting-started.html)
- Google GKE: [Deploy an app in a GKE cluster](https://cloud.google.com/kubernetes-engine/docs/deploy-app-cluster)
- Azure AKS: [Deploy an AKS cluster with the Azure CLI](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-cli)

Tell the user to create the cluster with network policy enforcement turned on, and with Kubernetes 1.35 or newer. Each provider's page names the option. Offer no advice on size or price.

When the user says the cluster exists, run `kubectl config get-contexts -o name`, confirm the new context as [The cluster context](../SKILL.md#the-cluster-context) says, and continue with [check-dependencies.md](check-dependencies.md).
