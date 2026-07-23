# A Cluster in One Command

Making the cluster really is one command. Kaalm then needs three installs on
top of it: two things it depends on, and Kaalm itself.

## The cluster

```bash
k3d cluster create kaalm-tutorial --wait --k3s-arg "--disable=traefik@server:0"
```

That takes a few seconds and ends with:

```
INFO[0012] Cluster 'kaalm-tutorial' created successfully!
INFO[0012] You can now use it like this:
kubectl cluster-info
```

k3d also points `kubectl` at the new cluster for you, so every command from
here on lands in the right place.

> **Teardown, stated early so you can experiment freely.** When you are done
> with this book, or any time you want to start over:
>
> ```bash
> k3d cluster delete kaalm-tutorial
> ```
>
> That deletes everything: the cluster, the agent, the storage, all of it.
> Nothing outside Docker is touched.

## The two prerequisites

Kaalm gives every agent its own TLS identity, which is how the pieces prove to
each other who they are. It does not implement that itself; it builds on two
standard cluster add-ons, and it expects them to already be there.

```bash
helm repo add jetstack https://charts.jetstack.io
helm repo update jetstack

helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --version v1.16.2 \
  --set crds.enabled=true \
  --set 'extraArgs={--enable-certificate-owner-ref=true}' \
  --wait

helm install trust-manager jetstack/trust-manager \
  --namespace cert-manager \
  --version v0.13.0 \
  --set app.trust.namespace=cert-manager \
  --wait
```

cert-manager issues the certificates; trust-manager distributes the shared
certificate authority so everyone can verify everyone else. The
`--enable-certificate-owner-ref` flag matters: it makes cert-manager clean up a
certificate's secret when the certificate goes away, which is how Kaalm avoids
leaving credentials behind when you delete an agent.

> **Helm** is the installer, and a **chart** is the package it installs: a
> bundle of Kubernetes objects with knobs you can set from the command line.
> `--wait` tells Helm not to return until the things it installed are actually
> running, which is why these commands take a moment.

> **A namespace** is a folder for cluster objects. `--create-namespace` makes
> one called `cert-manager` and puts the add-ons in it, so they sit apart from
> your own work.

## Kaalm

```bash
helm install kaalm oci://ghcr.io/win07xp/charts/kaalm \
  --version 0.2.0 \
  --namespace kaalm-system --create-namespace \
  --set certManager.clusterResourceNamespace=cert-manager
```

`0.2.0` is the release this book was walked against. Newer releases are on the
[releases page](https://github.com/win07xp/kaalm/releases) and install the same
way.

## What just got installed

Two commands tell you it worked. First, the programs:

```bash
kubectl get pods -n kaalm-system
```

```
NAME                               READY   STATUS    RESTARTS   AGE
kaalm-controller-6b948dcf4-df862   1/1     Running   0          27s
kaalm-controller-6b948dcf4-ptv4p   1/1     Running   0          27s
kaalm-gateway-578ddc44d4-8wc5r     1/1     Running   0          27s
kaalm-gateway-578ddc44d4-kskhx     1/1     Running   0          27s
```

> **A pod** is one running instance of your program, the smallest thing
> Kubernetes schedules. Two of each here so that losing one does not take the
> system down.

Those are the two halves of Kaalm. The **controller** is the part that watches
what you have declared and makes it real: it creates agents, gives them
storage, puts them to sleep. The **gateway** is the part traffic goes through:
messages coming in to your agents, and model calls going out.

> **An operator** is the pattern those two implement: a program that runs
> inside the cluster, watches for objects of a kind it understands, and
> continuously drives the world toward what those objects say. You declare;
> the operator does.

Second, the new vocabulary Kaalm taught your cluster:

```bash
kubectl get crds -o name | grep kaalm.io
```

```
customresourcedefinition.apiextensions.k8s.io/agentchannels.kaalm.io
customresourcedefinition.apiextensions.k8s.io/agentclasses.kaalm.io
customresourcedefinition.apiextensions.k8s.io/agents.kaalm.io
customresourcedefinition.apiextensions.k8s.io/agenttasks.kaalm.io
customresourcedefinition.apiextensions.k8s.io/modelproviders.kaalm.io
```

> **A CRD**, a custom resource definition, teaches the cluster a new kind of
> object. Kubernetes ships knowing about pods and a few dozen other things; it
> does not ship knowing what an agent is. Installing Kaalm taught it five new
> nouns, which is why `kubectl get agents` will work in the next chapter even
> though "agent" is not a Kubernetes concept.

Next: [Looking Around](looking-around.md).
