# Credential handling

Kaalm handles three kinds of long-lived secret material: LLM API keys, which authenticate the gateway to model providers; tool server credentials, which authenticate the gateway's broker to MCP tool servers; and channel credentials, which verify inbound webhook callers and sign outbound callbacks. LLM keys and tool credentials live in one namespace, `kaalm-system`, and are never copied. Channel credentials live in each agent's own namespace.

One rule spans all three: agent containers never hold credential material. The gateway is the only component that uses a credential on the data path, and it is a separate Pod in `kaalm-system`, which is what lets a plain NetworkPolicy enforce the separation ([Protecting agent containers from LLM provider access](#protecting-agent-containers-from-llm-provider-access)). The operator reads LLM keys and tool credentials to validate them and to run health probes, which are a second egress ([Health probes are a second credential egress](#health-probes-are-a-second-credential-egress)).

TLS material (the workload certificates and the CA trust chain) follows a separate lifecycle under cert-manager ([In-cluster TLS](tls.md#in-cluster-tls)). Workload identity, which selects the grants, the budget, and the audit name a credential is used for, is specified on [Workload identity](../gateways/llm/workload-identity.md).

## Lifecycle of an LLM API key

1. **Stored** in a Secret in `kaalm-system` (for example `kaalm-system/anthropic-api-key`), created and managed by platform engineers. The credential manager **labels** the Secret `kaalm.io/provider-credential: "true"` and **annotates** it with `kaalm.io/provider-hosts`, the bare hostnames of every provider that uses it ([rules 49 and 50](../resources/validation/providers.md#provider-credentials)). One Secret shared by several providers, including a ModelProvider and a ToolProvider, carries one label and one annotation that lists every host.
2. **Referenced** by `ModelProvider.spec.credentialsRef`, resolved only in `kaalm-system`. Two ServiceAccounts can read it: the gateway and the operator ([RBAC and authentication](rbac.md#operator-serviceaccount)).
3. **Loaded.** The gateway reads the Secret on first use and then follows it through a single-object watch, because it holds `get` and `watch` but never `list`. The operator reads it on every ModelProvider pass and runs the health probe only after the label, host, and key checks pass ([ModelProviderReconciler](../controller/reconcilers/modelprovider.md)). The gateway repeats the label and host checks on every read, so it uses only a labeled Secret for an approved host. Both components hold the key in process memory.
4. **Used.** The agent's request carries no credential. The gateway strips any inbound `Authorization`, `X-Api-Key`, or `Api-Key` header, so a framework SDK's placeholder key never reaches a provider, and injects the provider key on the upstream leg only. The completion returns to the agent without it. Agent Pods have no Secret mounted and no RBAC to fetch one.
5. **Rotated.** An in-place update of the Secret keeps the label and the annotation and lands on the gateway's watch, and the gateway refreshes in memory without a restart. In-flight requests finish on the old key. A rotation that re-creates the Secret (delete and create, or External Secrets) must keep both, or the provider goes not Ready with `SecretNotOptedIn` or `EndpointHostNotApproved`.
6. **Never copied.** There are no per-agent or per-namespace copies. Rotation is one update because there is one location.

![The life of an LLM API key: a platform engineer creates the Secret in kaalm-system with the label kaalm.io/provider-credential and the annotation kaalm.io/provider-hosts; the gateway loads it with get and watch and uses it only if it is labeled and approves the endpoint host; the operator reads it on each pass and checks the label, the host, and the key before it probes; the agent's request carries no credential and the gateway injects the key upstream; an in-place update fires the watch.](../diagrams/llm-key-lifecycle.svg)

A `google-vertex` provider uses the same label and annotation, and only the controller's health probe uses its service-account key; the gateway does not serve the type ([The google-vertex type is reserved](../gateways/llm/request-handling.md#the-google-vertex-type-is-reserved)). Rule 50 checks the `spec.endpoint` host, because that is where the minted access token goes. The JWT assertion goes to the key's `token_uri`, which lives inside the Secret that only the credential manager writes, and the rule does not check it.

The label and the annotation limit which Secret a provider may use and where it may send the credential. RBAC does not change: it still covers every Secret in `kaalm-system` ([who sets the label and the annotation](rbac.md#bindings-come-from-values)).

## Lifecycle of a tool server credential

A tool credential follows the LLM key's lifecycle on the [tool plane](../gateways/tool-plane.md), with these differences:

- It is referenced by `ToolProvider.spec.credentialsRef`, and the reference is optional. A ToolProvider without one is an unauthenticated tool server: the broker injects nothing, and rules 49 and 50 do not apply.
- Its Secret carries the same label and annotation as an LLM key, and the annotation lists the host of the ToolProvider's `spec.endpoint`. A Secret that both a ModelProvider and a ToolProvider use lists both hosts.
- The gateway resolves it per brokered call and injects it as a bearer token on the upstream leg ([Credential injection](../gateways/tool-plane.md#credential-injection)). The operator reads it for the health probe and Ready validation, and checks the label and the host before the probe. The broker checks both again on every brokered call.
- It is used only on calls the broker admits. A call denied by the namespace gate, the grant chain, or session ownership ends before the credential is read, so a rejected call never spends it.
- A credential the tool server rejects surfaces to the caller as `503 tool_unavailable` and raises a `CredentialsInvalid` Warning event on the ToolProvider, so the platform team learns that the Secret needs rotating. [Failure modes](../gateways/tool-plane.md#failure-modes) lists the signals.

The credential has no path into an agent's namespace: an Agent's `spec.tools` grant names the ToolProvider, and the Secret behind it stays in `kaalm-system`.

## Lifecycle of a channel credential (AgentChannel)

1. **Stored** in a Secret in the agent's namespace (for example `team-support/discord-bot-credentials`), created by the team's credential manager, a person holding `kaalm-secrets-admin` in that namespace, or by a provisioning service. The creator **labels** the Secret `kaalm.io/channel-credential: "true"`, which opts it in to channel use ([rule 45](../resources/validation/channels.md)). A Secret used as a bearer `callbackAuth` token also lists its approved hosts in the annotation `kaalm.io/callback-hosts` (rule 46). Developers need no Secret access in their namespace, unless the platform team sets `rbac.personas.developerSecrets` ([Roles for people](rbac.md#persona-roles)).
2. **Referenced** by the AgentChannel. A webhook channel names its inbound Secret with `spec.webhook.auth.secretRef` (bearer) or `spec.webhook.auth.hmac.secretRef` (HMAC), and its outbound Secret with `spec.webhook.callbackAuth.secretRef` or `.hmac.secretRef`, which [rule 25](../resources/validation/channels.md) requires whenever `callbackUrl` is set. A platform channel names one Secret with `spec.discord.credentialsRef` or `spec.whatsapp.credentialsRef`, whose keys are fixed by the type ([rule 40](../resources/validation/channels.md)).
3. **Granted.** RBAC cannot grant a read by label, so the operator alone first gets read access to every referenced Secret, to check the label, the approved callback host, and the configured keys. The gateway and the operator then get read access to the labeled Secrets only ([Per-channel Roles](rbac.md#operator-serviceaccount)), so the gateway never reads a Secret that has not opted in.
4. **Loaded.** The gateway resolves the Secret on the first request that needs it and follows it through the same single-object watch as an LLM key. It refuses a Secret without the label on every read, and a bearer callback token before every callback attempt unless the Secret lists the callback host. The material is held in process and split by direction: the inbound `auth` material feeds the webhook verifier, and the outbound `callbackAuth` material signs the async callback POST. A platform channel's one Secret splits the same way: its verification keys feed the inbound verifier, and its bot or access token is presented only on the platform reply.
5. **Rotated.** An in-place update fires the watch, and the gateway refreshes without a restart. The operator re-checks the channel on the same change, so a rotation that drops the label or a key shows on `Ready` at once ([Timing](../controller/reconcilers/agentchannel.md#timing)).

![How a channel credential is stored, granted, loaded, and rotated: the Secret is created in the agent's namespace, the credential manager labels it, the operator ensures the check and credential Roles and validates the label and keys, the gateway loads the labeled Secret with get and watch on first use, and an in-place update fires the watch.](../diagrams/channel-credential-grant.svg)

![One channel Secret in use: the gateway verifies the inbound webhook with the auth material, delivers the envelope to the agent with no credential, and either signs the async callback POST with the callbackAuth material or answers the platform inline.](../diagrams/channel-credential-use.svg)

Channel credentials are namespace-scoped so that each namespace holds only the credentials for its own agents' channels, and a leak in one namespace is bounded to that namespace's channels. The label keeps every other Secret in the namespace out of the gateway's reach.

## Health probes are a second credential egress

The rule that the gateway is the only component that uses a credential covers the data path only. The controller's health probes also send a credential to the provider's configured endpoint, in or outside the cluster. Most probes send the provider key or tool credential itself. A `google-vertex` probe sends an assertion signed with the key, and a token minted from it. Kaalm does not route the probes through the gateway, so the controller is a second credential egress. A reconcile pass that fails [rule 49 or 50](../resources/validation/providers.md#provider-credentials) ends before the probe, so a probe never sends a credential to a host the Secret does not approve.

Each probe is bounded as follows:

- **`anthropic`, `openai`, and `openai-compatible` ModelProviders, and ToolProviders:** the credential goes to `spec.endpoint` and nowhere else. A ToolProvider with no `credentialsRef` sends no credential.
- **`google-vertex`:** the probe makes two requests. It sends a JWT assertion, signed with the service-account key's private key, to the key's `token_uri`, and then sends the minted access token to `spec.endpoint`. The private key is never sent. The bound is `spec.endpoint` plus the key's `token_uri`, and the gateway sends this credential nowhere.
- **Redirects:** a probe never follows one, and a redirect counts as a transient error. A redirecting endpoint cannot move the credential to another host.
- **Time:** each probe, including the Vertex token request, is bounded by `healthCheck.timeoutSeconds` (default 10s).
- **TLS trust:** the probes trust the system roots plus the gateway's upstream trust, plus anything the deprecated `controller.trustClusterCAForProbes` and `controller.probeCA` values add ([Probe TLS trust](../controller/reconcilers/modelprovider.md#probe-tls-trust)).

The request each probe sends, including the headers that carry the credential, is specified under [Liveness probe](../controller/reconcilers/modelprovider.md#liveness-probe) and [The google-vertex probe](../controller/reconcilers/modelprovider.md#the-google-vertex-probe) for ModelProviders, and under [ToolProviderReconciler](../controller/reconcilers/toolprovider.md) for ToolProviders.

## Protecting agent containers from LLM provider access

The synthesized NetworkPolicy is what keeps an agent container from calling a provider directly. It allows egress only to the gateway and DNS, plus what the class opens ([Network policy](model.md#network-policy)). [What the synthesized NetworkPolicy protects](../runtime/child-resources.md#what-the-synthesized-networkpolicy-protects) specifies the object.

Enforcement is between Pods, so standard Kubernetes NetworkPolicy is enough and no service mesh or L7 CNI is required.

### The DNS egress rule

The DNS rule allows port 53, UDP and TCP, to the Pods the chart value `controller.networkPolicy.dnsSelector` selects: by default the `k8s-app: kube-dns` Pods in `kube-system`. A cluster whose DNS runs elsewhere sets the value to match its DNS Pods; until it does, agent Pods cannot resolve names ([Helm chart contents](../operations/deployment.md#helm-chart-contents)). The rule never widens beyond the selected Pods, so an agent cannot reach other workloads in the DNS namespace on port 53.
