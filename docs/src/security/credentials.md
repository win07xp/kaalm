# Credential handling

Kaalm handles three kinds of long-lived secret material: LLM API keys, which authenticate the gateway to model providers; tool server credentials, which authenticate the gateway's broker to MCP tool servers; and channel credentials, which verify inbound webhook callers and sign outbound callbacks. LLM keys and tool credentials live in one namespace, `kaalm-system`, and are never copied. Channel credentials live in each agent's own namespace.

One rule spans all three: agent containers never hold credential material. The gateway is the only component that uses a credential on the data path, and it is a separate Pod in `kaalm-system`, which is what lets a plain NetworkPolicy enforce the separation ([Protecting agent containers from LLM provider access](#protecting-agent-containers-from-llm-provider-access)). The operator reads credentials only to validate them and to run health probes.

TLS material (the workload certificates and the CA trust chain) follows a separate lifecycle under cert-manager ([In-cluster TLS](tls.md#in-cluster-tls)). Workload identity, which selects the grants, the budget, and the audit name a credential is used for, is specified on [Workload identity](../gateways/llm/workload-identity.md).

## Lifecycle of an LLM API key

1. **Stored** in a Secret in `kaalm-system` (for example `kaalm-system/anthropic-api-key`), created and managed by platform engineers.
2. **Referenced** by `ModelProvider.spec.credentialsRef`, resolved only in `kaalm-system`. Two ServiceAccounts can read it: the gateway, through its namespaced Role, and the operator, through its cluster-wide Secret read ([RBAC and authentication](rbac.md#operator-serviceaccount)).
3. **Loaded.** The gateway reads the Secret on first use and then follows it through a single-object watch, because it holds `get` and `watch` but never `list`. If the watch has not synced within two seconds, the read goes to the API server directly. The operator reads it on every ModelProvider pass to validate it and to run the health probe, which needs the key because `GET /v1/models` requires authentication ([ModelProviderReconciler](../controller/reconcilers.md#modelproviderreconciler), steps 1 and 5). As shipped both components hold the key in process memory: the gateway in its watch cache, the operator in the manager's Secret informer.
4. **Used.** The agent's request carries no credential. The gateway strips any inbound `Authorization`, `X-Api-Key`, or `Api-Key` header, so a framework SDK's placeholder key never reaches a provider, and injects the provider key on the upstream leg only. The completion returns to the agent without it. Agent Pods have no Secret mounted and no RBAC to fetch one.
5. **Rotated.** An in-place update of the Secret lands on the gateway's watch, and the gateway refreshes in memory without a restart. In-flight requests finish on the old key.
6. **Never copied.** There are no per-agent or per-namespace copies. Rotation is one update and one watch because there is one location.

![The life of an LLM API key: a platform engineer creates the Secret in kaalm-system; the gateway loads it with get and watch and the operator reads it on each pass; the agent's request carries no credential and the gateway injects the key upstream; an in-place update fires the watch.](../diagrams/llm-key-lifecycle.svg)

Vertex providers use the same class: the adapter sends the Secret value as a bearer token, so no separate credential type exists.

## Lifecycle of a tool server credential

A tool credential follows the LLM key's lifecycle on the [tool plane](../gateways/tool-plane.md), with these differences:

- It is referenced by `ToolProvider.spec.credentialsRef`, and the reference is optional. A ToolProvider without one is an unauthenticated tool server, and the broker injects nothing.
- The gateway resolves it per brokered call through the same watch cache and injects it as a bearer token on the upstream leg ([Credential injection](../gateways/tool-plane.md#credential-injection)). The operator reads it for the ToolProvider health probe and Ready validation.
- It is used only on calls the broker admits. A call denied by the namespace gate, the grant chain, or session ownership ends before the credential is read, so a rejected call never spends it.
- A credential the tool server rejects surfaces to the caller as `503 tool_unavailable`; [Failure modes](../gateways/tool-plane.md#failure-modes) lists the signals, and as shipped no Event is emitted.

The credential has no path into an agent's namespace: an Agent's `spec.tools` grant names the ToolProvider, and the Secret behind it stays in `kaalm-system`.

## Lifecycle of a channel credential (AgentChannel)

1. **Stored** in a Secret in the agent's namespace (for example `team-support/discord-bot-credentials`), created by the platform team or a provisioning service. Developers need no Secret access in their namespace.
2. **Referenced** by the AgentChannel. A webhook channel names its inbound Secret with `spec.webhook.auth.secretRef` (bearer) or `spec.webhook.auth.hmac.secretRef` (HMAC), and its outbound Secret with `spec.webhook.callbackAuth.secretRef` or `.hmac.secretRef`, which [rule 25](../resources/validation-and-defaulting.md#cross-resource-validation) requires whenever `callbackUrl` is set. A platform channel names one Secret with `spec.discord.credentialsRef` or `spec.whatsapp.credentialsRef`, whose keys are fixed by the type ([rule 40](../resources/validation-and-defaulting.md#cross-resource-validation)).
3. **Granted.** The AgentChannelReconciler ensures one Role in the channel's namespace, `get, watch` limited by `resourceNames` to the referenced Secrets, with a RoleBinding each for the gateway and the operator ([Per-channel Role](rbac.md#operator-serviceaccount)). The same reconciler reads the Secret to check that the configured keys exist and, for Discord, that the public key parses ([AgentChannelReconciler](../controller/reconcilers.md#agentchannelreconciler), step 3).
4. **Loaded.** The gateway resolves the Secret on the first request that needs it and follows it through the same single-object watch as an LLM key. The material is held in process and split by direction: the inbound `auth` material feeds the webhook verifier, and the outbound `callbackAuth` material signs the async callback POST. A platform channel's one Secret splits the same way: the Discord public key or the WhatsApp app secret and verify token feed the inbound verifier, and the bot token or access token is presented on the platform reply.
5. **Rotated.** An in-place update fires the watch, and the gateway refreshes without a restart.

![How a channel credential is stored, granted, loaded, and rotated: the Secret is created in the agent's namespace, the operator ensures the per-channel Role and validates the keys, the gateway loads the Secret with get and watch on first use, and an in-place update fires the watch.](../diagrams/channel-credential-grant.svg)

![One channel Secret in use: the gateway verifies the inbound webhook with the auth material, delivers the envelope to the agent with no credential, and either signs the async callback POST with the callbackAuth material or answers the platform inline.](../diagrams/channel-credential-use.svg)

Channel credentials are namespace-scoped so that each namespace holds only the credentials for its own agents' channels, and a leak in one namespace is bounded to that namespace's channels.

## Protecting agent containers from LLM provider access

The synthesized NetworkPolicy is what keeps an agent container from calling a provider directly; [What the synthesized NetworkPolicy protects](../runtime/child-resources.md#what-the-synthesized-networkpolicy-protects) specifies the object. Its rules, for an Agent named `support-assistant`:

```yaml
policyTypes: [Ingress, Egress]
ingress:
  - from:
      - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kaalm-system } }
        podSelector: { matchLabels: { app.kubernetes.io/component: gateway } }
    ports:
      - { port: 8080, protocol: TCP }   # the agent's health port: POST /v1/message
egress:
  - to:
      - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kaalm-system } }
        podSelector: { matchLabels: { app.kubernetes.io/component: gateway } }
    ports:
      - { port: 8443, protocol: TCP }   # every agent-to-gateway call
  - to:
      - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } }
    ports:
      - { port: 53, protocol: UDP }
      - { port: 53, protocol: TCP }
```

The namespace selector names the operator's namespace, `kaalm-system` on a default install. A `spec.network.egress.allowedCIDRs` entry on the class appends an egress rule to that CIDR on every port, and `spec.network.allowSameNamespaceIngress` appends an ingress rule from the namespace's other agent Pods on the agent's health port ([Network policy](model.md#network-policy)).

Enforcement is between Pods, so standard Kubernetes NetworkPolicy is enough and no service mesh or L7 CNI is required. Both gateway listeners serve TLS with the same `kaalm-gateway-tls` certificate ([TLS on the cluster listener](../gateways/listener-tls.md)), and external webhook traffic reaches the User listener through an Ingress configured for an HTTPS backend ([TLS and Ingress](../gateways/user/overview.md#tls-and-ingress)).

### The DNS egress rule

As shipped the DNS rule has a namespace selector only, so an agent can reach any Pod in `kube-system` on port 53. That matches kubeadm, EKS, GKE, AKS, and the standard CoreDNS chart, where the DNS Pods run in `kube-system`. A cluster whose DNS runs elsewhere, such as a custom CoreDNS chart in its own namespace, has no working DNS from agent Pods until the rule is changed. The chart value `controller.networkPolicy.dnsSelector` is accepted but not applied to the synthesized policy ([Helm chart contents](../operations/deployment.md#helm-chart-contents)).
