# Verifying the install

Three checks confirm a working install: the CRDs exist, both components run,
and the bundled starter class is Ready.

## 1. The six CRDs

```bash
kubectl get crds | grep kaalm.io
```

Expect all six: `agentclasses`, `agents`, `agenttasks`, `agentchannels`,
`modelproviders`, `toolproviders`.

## 2. The two components

```bash
kubectl get pods -n kaalm-system
```

Expect two controller replicas and two gateway replicas, all `Running`:

```text
NAME                                READY   STATUS    RESTARTS   AGE
kaalm-controller-667c6785dc-dp4pt   1/1     Running   0          30s
kaalm-controller-667c6785dc-wbvmv   1/1     Running   0          30s
kaalm-gateway-5b566f6b5b-prz4p      1/1     Running   0          30s
kaalm-gateway-5b566f6b5b-s75ql      1/1     Running   0          30s
```

The gateway also exposes a Service:

```bash
kubectl get svc -n kaalm-system kaalm-gateway
```

with three ports: `8080` (the User listener, inbound webhooks), `8443` (the
cluster listener: LLM calls, task completion, the tool plane, and the internal
endpoints), and `9090` (metrics):

```text
NAME            TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)                      AGE
kaalm-gateway   ClusterIP   10.43.238.49   <none>        8080/TCP,8443/TCP,9090/TCP   30s
```

If pods are stuck in `ContainerCreating` on the `kaalm-tls` volume, the
certificates have not been issued yet. Check that the `kaalm-ca-issuer`
ClusterIssuer is Ready (`kubectl get clusterissuer`): the CA Certificate lives
in the cluster resource namespace, not in `kaalm-system`, and the usual cause
is a `certManager.clusterResourceNamespace` that does not match your
cert-manager and trust-manager installs.

## 3. The starter AgentClass

The chart ships a `standard` AgentClass (`--set standardAgentClass.enabled=false`
at install time leaves it out; on an upgrade the live class stays, because
of its keep policy):

```bash
kubectl get agentclasses
kubectl describe agentclass standard   # conditions show Ready=True
```

```text
NAME       AGENTS   TASKS   AGE
standard                    28s
```

A Ready condition on the class means the controller has reconciled it; the
empty `AGENTS` and `TASKS` columns fill in as workloads use the class. The
class allows any image, storage, and hibernation, but it lists no
ModelProvider and no ToolProvider, so an Agent that names a provider under it
goes `Degraded` until the platform team names one
([Offering agent classes](../platform/agent-classes.md)). From here:

- Platform engineer: continue to [Offering agent classes](../platform/agent-classes.md).
- Agent developer: jump to [Your first agent](../developers/first-agent.md)
  once your platform team has provided a class and a provider.

---

*How this works: design book pages Controller, Operator structure (what the
controller replicas do) and Gateways, Gateway overview (the two listeners and their
ports).*
