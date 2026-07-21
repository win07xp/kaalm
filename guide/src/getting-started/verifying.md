# Verifying the Install

Three checks confirm a working install: the CRDs exist, both components run,
and the bundled starter class is Ready.

## 1. The five CRDs

```bash
kubectl get crds | grep agentry.io
```

Expect all five: `agentclasses`, `agents`, `agenttasks`, `agentchannels`,
`modelproviders`.

## 2. The two components

```bash
kubectl get pods -n agentry-system
```

Expect two controller replicas and two gateway replicas, all `Running`. The
gateway also exposes a Service:

```bash
kubectl get svc -n agentry-system agentry-gateway
```

with three ports: `8080` (user gateway, inbound webhooks), `8443` (LLM gateway,
agents call out through this), and `9090` (metrics).

If pods are stuck in `ContainerCreating` on the `agentry-tls` volume, the
cert-manager Certificates have not been issued yet; check
`kubectl get certificates -n agentry-system` and the cert-manager logs.

## 3. The starter AgentClass

The chart ships a `standard` AgentClass (disable with
`--set standardAgentClass.enabled=false`):

```bash
kubectl get agentclasses
kubectl describe agentclass standard   # conditions show Ready=True
```

A Ready condition on the class means the controller is reconciling. From here:

- Platform engineer: continue to [Offering Agent Classes](../platform/agent-classes.md).
- Agent developer: jump to [Your First Agent](../developers/first-agent.md)
  once your platform team has provided a class and a provider.

---

*How this works: design book pages Controller, Operator Structure (what the
controller replicas do) and Gateways, Overview (the two listeners and their
ports).*
