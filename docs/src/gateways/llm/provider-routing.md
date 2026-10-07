# Provider routing and adapters

After [Workload identity](workload-identity.md) has produced an authenticated namespace, the gateway answers two questions before it forwards anything: *is this caller allowed to use this provider?* and *how do I speak to that provider?* Provider routing answers the first. Provider adapters answer the second.

Routing differs between the two authentication tiers, because the gateway-only tier has no Agent resource to consult. The gateway-only variant is the same chain of checks with the class check and the workload's `providers` check removed; [Provider access checks](../../concepts/tenancy-and-tiers.md#provider-access-checks) draws both tiers as a single figure, with the denial code on every arm.

## mTLS tier (Kaalm-managed Pods)

Agents and AgentTasks created by the controller have an Agent (or AgentTask) resource with `spec.providers`, and an AgentClass with `allowedProviders`. The gateway walks the full chain:

1. **Source IP to Pod**: the Pod must be in the authenticated namespace (the cross-check in [Workload identity](workload-identity.md#source-ip-cross-check-both-modes)).
2. **Identity to workload**: the SAN names the Agent (or AgentTask), which must exist in that namespace.
3. **Agent to allowed providers**: the Agent's `spec.providers` lists the ModelProviders this agent may use. The referenced providers must also appear in the AgentClass's `allowedProviders`. The gateway resolves the class from the workload's `agentClassRef`. Before it reads `allowedProviders`, the gateway checks that the class admits the workload's namespace ([`allowedNamespaces`](../../resources/agentclass.md#allowednamespaces-keeps-a-class-to-some-teams)). A class that sets the field without matching the namespace gets `403 access_denied`. A class that leaves it unset admits every namespace. Gateway-only callers have no class, so this check does not apply to them.
4. **Model name to ModelProvider**: the gateway parses the `provider/model` qualified name from the request body (see [Model identification](request-handling.md#model-identification)). The provider prefix must match a `providerRef` in the Agent's `spec.providers`, or the request is rejected with `403 access_denied`.
5. **ModelProvider to upstream**: the gateway reads the ModelProvider's `spec.endpoint`, `spec.type`, and credentials to forward the request. The namespace must also be in the ModelProvider's `allowedNamespaces`.

An agent can therefore reach only ModelProviders listed in its `spec.providers` that its AgentClass allows and whose `allowedNamespaces` include the agent's namespace. The AgentClass must also admit that namespace.

## Gateway-only tier (TokenReview)

Existing workloads that authenticate with a projected ServiceAccount bearer token have **no Agent resource**, so steps 2 to 4 above do not apply. Routing is governed by the ModelProvider's own allowlist plus its model list:

1. **Token to namespace**: `TokenReview` yields the caller's authenticated namespace (see [Mode 2](workload-identity.md#mode-2-serviceaccount-bearer-token)).
2. **Model name to ModelProvider**: the gateway parses the `provider/model` qualified name from the request body (see [Model identification](request-handling.md#model-identification)). The provider prefix must resolve to an existing `ModelProvider` by `metadata.name`; if not, the request is rejected with `400 invalid_request`.
3. **Namespace allowlist**: the caller's namespace must match a `ModelProvider.spec.allowedNamespaces` entry (exact name or glob). If not, the request is rejected with `403 access_denied`.
4. **Model allowlist**: the requested model must appear in `ModelProvider.spec.models`. If not, the request is rejected with `400 invalid_request`.
5. **Forward**: the gateway reads `spec.endpoint`, `spec.type`, and credentials and forwards the request.

**AgentClass `allowedProviders` is deliberately not enforced in this tier.** AgentClass is the platform-team policy layer for the full-lifecycle tier; gateway-only workloads are not Agents and are not associated with any AgentClass. Platform teams who need class-scoped provider policy must onboard workloads through the full Agent lifecycle tier. The gateway-only tier trades that policy surface for a zero-CRD on-ramp, see [What Kaalm provides](../../concepts/vision-and-scope.md#what-kaalm-provides) and [Tiered on-ramp](../../operations/deployment.md#tiered-on-ramp).

## Credential handling

Provider credentials are stored as Secrets in `kaalm-system` and referenced by ModelProvider. Credentials never leave `kaalm-system`: there is no per-agent or per-namespace credential copying. The full storage-to-rotation lifecycle, including who else can read the Secret, is in [Lifecycle of an LLM API key](../../security/credentials.md#lifecycle-of-an-llm-api-key).

The credential type is adapter-specific: for Anthropic, OpenAI, and OpenAI-compatible providers, the referenced Secret holds a static API key that the adapter injects as the provider's auth header. For `google-vertex` the Secret holds a GCP service-account JSON key, and only the controller's liveness probe uses it, because the type is not served (see [the type's status](request-handling.md#the-google-vertex-type-is-reserved) and [The google-vertex probe](../../controller/reconcilers/modelprovider.md#the-google-vertex-probe)).

The gateway reads a credential only from a Secret that passes [rules 49 and 50](../../resources/validation/providers.md#provider-credentials): the Secret must carry the label `kaalm.io/provider-credential: "true"`, list the host of the provider's `spec.endpoint` in its `kaalm.io/provider-hosts` annotation, and hold the key. The checks run on every read, once per forwarding attempt, so each fallback candidate is checked against its own Secret and host. A label or annotation removed takes effect on the next request, and in-flight requests finish with the credential they already read. Each gateway replica checks against its own watch, so right after an edit one replica can still see the old Secret.

A refused credential is a connect-class failure, which the fallback walk treats like an unreachable provider ([Fallback triggers](fallback.md#fallback-triggers)): it tries the next candidate with that candidate's own Secret and host. When every attempt fails at the connect layer, the caller gets `503 provider_unavailable`. The caller's response body never carries the Secret's error. The gateway logs it instead, as the warning `llm credential unavailable` with the provider name and the refusal reason, so an operator can tell a refused credential from an unreachable endpoint, which the caller sees the same way. The line also appears when a fallback serves the request and the caller sees no error. Each gateway replica logs it at most once a minute per provider, so expect one line per replica, not one per failed request. The reason names the Secret and the check it failed, or the read error, never the credential value. The reconciler reports the same failure on the ModelProvider as `Ready=False` with reason `SecretNotOptedIn` (rule 49) or `EndpointHostNotApproved` (rule 50).

## Provider adapters

The gateway supports multiple upstream provider types through adapters (the `providerAdapter` interface in `internal/gateway/adapters.go`). Adapters exist for Anthropic, OpenAI, and OpenAI-compatible providers (Ollama, vLLM, and LiteLLM gateways). The `google-vertex` type is reserved and not served (see [the type's status](request-handling.md#the-google-vertex-type-is-reserved)). Each adapter carries what is specific to its format:

- **Path patterns**: the proxy maps each inbound path to an adapter, which is how the gateway detects the request format. See [Request format detection](request-handling.md#request-format-detection).
- **Credential header**: the adapter injects the provider's auth header.
- **Usage extraction**: the adapter knows where each provider puts token counts, for buffered and streamed responses: `usage.input_tokens` and `usage.output_tokens` for Anthropic, `usage.prompt_tokens` and `usage.completion_tokens` for OpenAI. See [Streaming responses](request-handling.md#streaming-responses).
- **Request fixups**: the OpenAI adapter injects `stream_options: {"include_usage": true}` into streaming requests when absent, so usage data is observable. See [Streaming responses](request-handling.md#streaming-responses).

The [ModelProviderReconciler](../../controller/reconcilers/modelprovider.md) runs provider health probes, not the adapters. Format translation for a fallback edge that crosses formats also lives outside the adapters ([Crossing formats](fallback.md#crossing-formats)), so an adapter never converts one provider's request format into another's.

## Upstream TLS configuration

The gateway always connects to upstream LLM providers over HTTPS. For enterprise environments that require custom CA bundles or HTTP proxies for outbound traffic, the gateway supports:

- **Custom CA bundle**: a ConfigMap in `kaalm-system` (`kaalm-upstream-ca`) containing additional CA certificates. You create the ConfigMap and set the Helm value `gateway.upstreamCA.configMap` to its name. The gateway watches the bundle for changes. All upstream HTTPS connections trust both the system CA bundle and the custom bundle.
- **Cluster CA (in-cluster providers)**: the Helm value `gateway.trustClusterCAForUpstream: true` adds the cluster's own CA (`kaalm-ca-system`) to the upstream trust pool. Use it for a self-hosted `openai-compatible` provider in the cluster whose endpoint is served by a `kaalm-ca-issuer` certificate, without supplying a separate `kaalm-upstream-ca` bundle. It composes with the custom bundle: both are added to the system roots.
- **HTTP proxy**: standard `HTTPS_PROXY` / `NO_PROXY` environment variables on the gateway Deployment. The gateway respects these for all upstream provider calls.

These are gateway-level settings, not per-ModelProvider, because they reflect cluster-wide network infrastructure. The cluster CA and custom bundle settings also govern the controller's health probe trust ([Probe TLS trust](../../controller/reconcilers/modelprovider.md#probe-tls-trust)).
