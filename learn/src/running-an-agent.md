# Running an Agent

An agent is your code in a container. Kaalm does not supply the code, so the
first job is to build an image, and the starter template is the shortest path
to one that already speaks Kaalm's protocol correctly.

## Build the agent image

```bash
git clone https://github.com/win07xp/kaalm.git
cd kaalm
docker build -t my-agent:1 examples/starter-go
```

That image is a small Go program that answers messages by echoing them back.
It is not clever, and that is the point for now: it lets everything else in
this book work without an API key. Later you replace one function in it with
your own logic.

The cluster cannot reach images sitting on your laptop, so hand it over:

```bash
k3d image import my-agent:1 -c kaalm-tutorial
```

```
INFO[0003] Successfully imported 1 image(s) into 1 cluster(s)
```

## Write the manifest

Two objects: a class that sets the rules, and an agent that follows them. Put
both in a file called `agent.yaml`.

```yaml
apiVersion: kaalm.io/v1alpha1
kind: AgentClass
metadata:
  name: tutorial
spec:
  runtime:
    backend: pod
  image:
    pullPolicy: IfNotPresent
  persistence:
    enabled: true
    defaultSizeGi: 1
  lifecycle:
    hibernationAllowed: true
    defaultIdleTimeout: 30s
    defaultHibernationDelay: 30s
---
apiVersion: kaalm.io/v1alpha1
kind: Agent
metadata:
  name: helper
  namespace: default
spec:
  agentClassRef:
    name: tutorial
  image: my-agent:1
  persistence:
    enabled: true
  lifecycle:
    hibernationEnabled: true
    activitySource: gatewayTraffic
```

Reading the class: agents of this class run as pods, may use storage, and may
be put to sleep. `pullPolicy: IfNotPresent` tells the cluster to use the image
you imported instead of trying to download it, which matters because
`my-agent:1` exists nowhere on the internet.

The two timers are set aggressively short so that you can watch hibernation
happen in this sitting. A real class would use something like the 30 minutes
the shipped `standard` class defaults to.

Reading the agent: run `my-agent:1` under the `tutorial` class, give it
storage, and allow it to sleep. It is short because the class already answered
most of the questions.

> **A PVC**, a persistent volume claim, is a request for a piece of disk that
> outlives the program using it. `persistence.enabled: true` asks Kaalm for
> one. This is what will let your agent remember things after it has been shut
> down and started again, which is exactly what happens when it sleeps and
> wakes.

## Apply it

```bash
kubectl apply -f agent.yaml
```

```
agentclass.kaalm.io/tutorial created
agent.kaalm.io/helper created
```

Now watch it come up:

```bash
kubectl get agents
```

```
NAME     PHASE     READY   CLASS      AGE
helper   Running   True    tutorial   20s
```

It passes through `Pending` and `Provisioning` on the way, so if you are quick
you will catch one of those. `Running` with `READY True` means the container
started and is answering its health checks.

## What happened behind the scenes

From those few lines of YAML, Kaalm created a pod to run your image, a piece
of storage and attached it, a certificate giving this agent its own identity, a
service so other things can reach it, and a network policy restricting who is
allowed to. You can see the storage it made:

```bash
kubectl get pvc
```

```
NAME            STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   AGE
helper-memory   Bound    pvc-6a91ee09-50a1-461b-a2e4-da964ead5f77   1Gi        RWO            local-path     82s
```

`Bound` means real disk is attached and ready. Note the name: `helper-memory`,
derived from the agent's. Kaalm names the things it creates after the thing you
asked for, which makes them easy to find and easy to reason about.

If you want the whole story of what gets created and why, the design book
covers [child resources](https://github.com/win07xp/kaalm) in detail. For now
it is enough to know that deleting the agent later cleans all of it up.

Next: [Talking to Your Agent](talking-to-your-agent.md).
