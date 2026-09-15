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

with three ports: `8080` (user gateway, inbound webhooks), `8443` (LLM gateway,
agents call out through this), and `9090` (metrics):

```text
NAME            TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)                      AGE
kaalm-gateway   ClusterIP   10.43.238.49   <none>        8080/TCP,8443/TCP,9090/TCP   30s
```

If pods are stuck in `ContainerCreating` on the `kaalm-tls` volume, the
cert-manager Certificates have not been issued yet; check
`kubectl get certificates -n kaalm-system` and the cert-manager logs.

## 3. The starter AgentClass

The chart ships a `standard` AgentClass (disable with
`--set standardAgentClass.enabled=false`):

```bash
kubectl get agentclasses
kubectl describe agentclass standard   # conditions show Ready=True
```

```text
NAME       AGENTS   TASKS   AGE
standard                    28s
```

A Ready condition on the class means the controller is reconciling; the
empty `AGENTS` and `TASKS` columns fill in as workloads use the class. As
shipped, the class allows any image but no ModelProvider and no persistence,
so the platform team's first job is to give it a policy
([Offering agent classes](../platform/agent-classes.md)). From here:

- Platform engineer: continue to [Offering agent classes](../platform/agent-classes.md).
- Agent developer: jump to [Your first agent](../developers/first-agent.md)
  once your platform team has provided a class and a provider.

---

*How this works: design book pages Controller, Operator structure (what the
controller replicas do) and Gateways, Gateway overview (the two listeners and their
ports).*
