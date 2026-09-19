# Running an agent

An agent is code in a container. You do not have to write or build any of it
to get one running: Kaalm publishes reference images that already speak the
operator's protocol, and out of the box they answer every message by echoing
it back. That is not clever, and that is deliberate: it lets everything
else in this book work without an API key, and without Docker builds. Two
chapters from here you hand this agent your own code, still without building
anything.

## Write the manifest

Two objects: a class that sets the rules, and an agent that follows them. The
class combines two of the e2e suite's fixtures, `s16-starter` and
`s7-hibernating` in `test/e2e/testdata/`, with 30-second timers instead of
their 2-second ones; the agent is `config/samples/kaalm_v1beta1_agent.yaml`
with the published Python image, this class, and no model provider. Put both
in a file called `agent.yaml`.

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentClass
metadata:
  name: tutorial
spec:
  runtime:
    backend: pod
  image:
    allowHandlerMounts: true
  persistence:
    enabled: true
    defaultSizeGi: 1
  lifecycle:
    hibernationAllowed: true
    defaultIdleTimeout: 30s
    defaultHibernationDelay: 30s
---
apiVersion: kaalm.io/v1beta1
kind: Agent
metadata:
  name: helper
  namespace: default
spec:
  agentClassRef:
    name: tutorial
  image: ghcr.io/win07xp/kaalm-agent-python:0.7.0
  persistence:
    enabled: true
  lifecycle:
    hibernationEnabled: true
    activitySource: gatewayTraffic
```

Reading the class: agents of this class run as pods, may use storage, may be
put to sleep, and may run code you hand them as configuration
(`allowHandlerMounts: true`) instead of code baked into an image. That last
permission is what this tutorial is built on; you use it in
[Make it yours](make-it-yours.md).

The two timers are set aggressively short so that you can watch hibernation
happen in this sitting. A real class would use something like the 30 minutes
the shipped `standard` class defaults to.

Reading the agent: run the published Python reference image under the
`tutorial` class, give it storage, and allow it to sleep. It is short because
the class already answered most of the questions. The image comes straight
from Kaalm's registry; the cluster downloads it the first time an agent asks
for it, and nothing on your laptop builds anything.

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
helper   Running   True    tutorial   7s
```

It passes through `Pending` and `Provisioning` on the way, so if you are quick
you will catch one of those. `Running` with `READY True` means the container
started and is answering its health checks.

## What happened behind the scenes

From those few lines of YAML, Kaalm created a pod to run the image, a piece
of storage and attached it, a certificate giving this agent its own identity,
a service so other things can reach it, a service account with no permissions
so the agent cannot read the cluster, and a network policy. The policy works
in both directions: nothing reaches the agent except the gateway, and the
agent reaches nothing except the gateway, which is what keeps model keys out
of its hands later. You can see the storage it made:

```bash
kubectl get pvc
```

```
NAME            STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   VOLUMEATTRIBUTESCLASS   AGE
helper-memory   Bound    pvc-5babc3ae-fbec-4fa9-856d-1e6ee45113e5   1Gi        RWO            local-path     <unset>                 7s
```

`Bound` means real disk is attached and ready. Note the name: `helper-memory`,
derived from the agent's. Kaalm names the things it creates after the thing you
asked for, so you can find them and reason about them by name.

> **A service** is a stable name and address inside the cluster for a program
> that may be restarted or replaced. The agent's service is how the gateway
> finds it, and you will use the gateway's own service to reach the gateway in
> the next chapter.

For everything that gets created and why, the design book's
[Child resources](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/child-resources.md) has the full inventory. For
this chapter it is enough to know that deleting the agent later cleans all of
it up.

Next: [Talking to your agent](talking-to-your-agent.md).
