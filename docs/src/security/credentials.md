# Credential Handling

Agentry handles two kinds of long-lived secret material: **LLM API keys**, which authenticate Agentry to model providers, and **channel credentials**, which authenticate inbound webhook callers to Agentry and sign Agentry's outbound callbacks. The two have deliberately different homes. LLM keys live in one cluster-wide location and are never copied; channel credentials live in each agent's own namespace.

One rule spans both: **agent containers never hold credential material.** The gateway is the only component that uses credentials on the data path, and it is a separate Pod in `agentry-system`. That separation is what makes the isolation enforceable with nothing more exotic than a Kubernetes NetworkPolicy, covered in [Protecting Agent Containers from LLM Provider Access](#protecting-agent-containers-from-llm-provider-access) below.

TLS certificate material (agent serving certs, AgentTask client certs, and the CA trust chain) follows a separate lifecycle managed by cert-manager. See [In-cluster TLS (Bidirectional)](tls.md#in-cluster-tls).

## Lifecycle of an LLM API Key

1. **Stored**: in a Secret in `agentry-system` (e.g., `agentry-system/anthropic-api-key`), created and managed by platform engineers.
2. **Referenced**: by `ModelProvider.spec.credentialsRef`. Read access is limited to two ServiceAccounts: the **gateway** (which uses the key on every proxied request) and the **operator** (which validates that the Secret exists and is well-formed, and uses the key for the ModelProviderReconciler's provider health probes, since `GET /v1/models` requires authentication; see [ModelProviderReconciler](../controller/reconcilers.md#modelproviderreconciler) step 2).
3. **Loaded**: the gateway reads the Secret at startup and on rotation events (Kubernetes watch). Credentials are held in the gateway process memory, and transiently in the operator for the duration of a health probe. The operator does not retain key material between probes.
4. **Used**: the gateway injects the API key into upstream requests on behalf of agent containers. Agent containers never have access to the credential. They do not have the Secret mounted and cannot reach `agentry-system` Secrets via the Kubernetes API.
5. **Rotated**: when the source Secret is updated, the gateway's Secret watch picks up the change and refreshes in-memory credentials without a restart.
6. **Never copied**: there are no per-agent or per-namespace copies of LLM credentials. The source Secret in `agentry-system` is the single authoritative location.

Step 6 is the reason rotation is a single Secret update: with no fan-out copies, there is exactly one place to change and one watch to fire.

![A sequence diagram of the six-step LLM API key lifecycle, with a user namespace holding the Agent Pod and an agentry-system frame holding the Secret, the operator ServiceAccount and the gateway ServiceAccount, and the LLM provider outside the cluster. A platform engineer creates the Secret in agentry-system; exactly two ServiceAccounts may read it, both through a namespaced Role. The gateway loads it at startup and holds it in process memory; the operator reads it once per health probe and retains nothing between probes. On the used step the agent sends an LLM request carrying no credential, the gateway injects the API key on the upstream leg only, and the completion returns to the agent still carrying no credential; the agent cannot read agentry-system Secrets at all. Rotation is a single in-place Secret update that fires the gateway's watch.](../diagrams/llm-key-lifecycle.svg)

**Reading the diagram.** The namespace frames are the argument. No credential-bearing arrow ever crosses out of `agentry-system`: the key reaches the provider on the gateway's upstream leg and nowhere else. Read the three holders by retention, too, since they differ: the gateway keeps the key in memory for the life of the process, the operator only for the length of one probe, and the agent never at all.

## Lifecycle of a Channel Credential (AgentChannel)

1. **Stored**: in a Secret in the agent's namespace (e.g., `team-support/discord-bot-credentials`), created by the platform team or a provisioning service.
2. **Referenced**: by the AgentChannel's webhook auth config: `spec.webhook.auth.secretRef` (inbound, bearer), `spec.webhook.auth.hmac.secretRef` (inbound, HMAC), and/or `spec.webhook.callbackAuth.secretRef` / `.hmac.secretRef` (outbound callback signing, required when `spec.webhook.callbackUrl` is set; see [rule 25](../resources/validation-and-defaulting.md#cross-resource-validation)). Future platform types (v1.1) will use a top-level `credentialsRef`.
3. **Loaded**: the gateway watches `AgentChannel` resources directly. When it sees a new or updated AgentChannel, it reads the referenced Secret(s) from the agent's namespace using its scoped RBAC and holds them in-process: inbound `auth` material for the webhook adapter's verifier, outbound `callbackAuth` material for the adapter's `SendReply` signer. The operator ServiceAccount also has a parallel scoped read path on the same Secret(s) via a dynamic per-channel Role (see [Operator ServiceAccount](rbac.md#operator-serviceaccount)), used solely by the AgentChannelReconciler to validate that the configured `data` key exists; the operator does not retain credential material in memory.
4. **Rotated**: same watch-based mechanism as LLM credentials. The gateway watches the referenced Secret for changes and refreshes in-memory credentials without a restart.

Channel credentials are namespace-scoped for organizational isolation: each namespace contains only the credentials for its own agents' channels. They are created by the platform team or a provisioning service; developers do not need Secret access in their namespace.

![A sequence diagram of the channel credential lifecycle, drawn in the same grammar as the LLM API key figure but with the Secret sitting inside the agent's own namespace rather than agentry-system, and a second namespace holding its own separate Secret. The AgentChannel's webhook auth config names the Secrets, and the AgentChannelReconciler mints two resourceNames-scoped Roles in the agent's namespace, one granting the gateway get and watch and one granting the operator get and watch, both owned by the AgentChannel and torn down with it. The gateway holds the material in process, split by direction: the inbound auth material feeds the webhook adapter's verifier and the outbound callbackAuth material feeds its SendReply signer. The operator reads the same Secret only to validate that the configured data key exists and retains nothing.](../diagrams/channel-credential-lifecycle.svg)

**Reading the diagram.** It is deliberately the mirror of [the LLM API key figure](#lifecycle-of-an-llm-api-key): same participants, same layout, one structural difference. The Secret has moved out of `agentry-system` and into the agent's namespace, and it fans out one per namespace. Every other contrast follows from that move, including why the grant has to be minted per channel and garbage-collected with the AgentChannel instead of shipping as a plain namespaced Role. One Secret feeds both directions: the same object backs the inbound verifier and the outbound `SendReply` signer.

## Protecting Agent Containers from LLM Provider Access

Because the gateway is a separate Pod in `agentry-system`, NetworkPolicy can cleanly enforce agent isolation without any per-container workarounds:

```yaml
# NetworkPolicy for agent Pods
policyTypes:
  - Ingress
  - Egress
ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: agentry-system
      podSelector:
        matchLabels:
          app.kubernetes.io/name: agentry-gateway
    ports:
      - port: 8080      # Agent HTTPS health/message port ($AGENTRY_HEALTH_PORT): gateway→agent channel message delivery
        protocol: TCP
egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: agentry-system
      podSelector:
        matchLabels:
          app.kubernetes.io/name: agentry-gateway
    ports:
      - port: 8443      # All agent→gateway TLS traffic (LLM calls, heartbeats, task completion)
        protocol: TCP
  - to:                    # DNS, scoped to kube-dns in kube-system
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: kube-system
      podSelector:
        matchLabels:
          k8s-app: kube-dns
    ports:
      - port: 53
        protocol: UDP
      - port: 53
        protocol: TCP
```

Agent containers that attempt to call LLM providers directly are blocked at the NetworkPolicy level. No service mesh or L7-capable CNI is required for this guarantee: standard Kubernetes NetworkPolicy is sufficient because the enforcement is cross-Pod. Both gateway listeners serve TLS using the same `agentry-gateway-tls` certificate: the LLM Gateway on port 8443 and the User Gateway on port 8080. External webhook traffic arrives via Ingress configured for backend re-encrypt or TLS pass-through, so there is no plaintext hop anywhere in the data path.

### The DNS egress rule

The DNS egress rule above is scoped to `kubernetes.io/metadata.name: kube-system` + `k8s-app: kube-dns`, which matches the upstream kube-dns/CoreDNS labelling used by kubeadm, EKS, GKE, AKS, and the standard CoreDNS chart. Clusters whose DNS Pod uses a different namespace or label set (custom CoreDNS chart, NodeLocal DNSCache only) must override the selector. The reconciler exposes this as the Helm value [`controller.networkPolicy.dnsSelector`](../operations/deployment.md#helm-chart-contents) (an object with `namespaceLabels` and `podLabels` keys) on the synthesized per-agent NetworkPolicy.

The narrow scoping is deliberate: an untrusted agent must not be able to reach arbitrary Pods on port 53. The previous `namespaceSelector: {}` rule allowed exactly that and is no longer acceptable.
