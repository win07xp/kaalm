# Provider routing and adapters

Once the gateway knows which namespace a request came from, it still has to answer two more questions before it can forward anything upstream: *is this caller allowed to use this provider?* and *how do I speak to that provider?* Provider routing answers the first. Provider adapters answer the second.

Both variants of routing run only after [Workload identity](workload-identity.md) has produced an authenticated namespace. Routing itself differs between the two authentication tiers, because the gateway-only tier has no Agent resource to consult.

The two variants below are the same gate chain with the class-level and workload-level gates removed; [Provider access gating](../../concepts/tenancy-and-tiers.md#provider-access-gating) draws both tiers as a single figure, with the denial code on every arm.

## mTLS tier (Kaalm-managed Pods)

Agents and AgentTasks created by the controller have an Agent (or AgentTask) resource with `spec.providers`, and an AgentClass with `allowedProviders`. The gateway walks the full chain:

1. **Source IP to Pod**: resolved from the Pod informer cache (see [Workload identity](workload-identity.md)).
2. **Pod to Agent**: the Pod's ownerRef identifies the Agent (or AgentTask) resource. The gateway maintains an Agent informer cache for this lookup.
3. **Agent to allowed providers**: the Agent's `spec.providers` lists the ModelProviders this agent may use. The referenced providers must also appear in the AgentClass's `allowedProviders`. The gateway resolves the class from the workload's `agentClassRef` in its AgentClass informer (see [Gateway readiness](operations.md#gateway-readiness) and [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions)).
4. **Model name to ModelProvider**: the gateway parses the `provider/model` qualified name from the request body (see [Model identification](request-handling.md#model-identification)). The provider prefix must match a `providerRef` in the Agent's `spec.providers`. If it does not, the request is rejected.
5. **ModelProvider to upstream**: the gateway reads the ModelProvider's `spec.endpoint`, `spec.type`, and credentials to forward the request. The namespace must also be in the ModelProvider's `allowedNamespaces`.

This chain ensures that an agent can only reach ModelProviders explicitly listed in its spec, which in turn must be in the AgentClass's `allowedProviders` and must include the agent's namespace in `allowedNamespaces`. All three access checks (Agent, then ModelProvider, then namespace) must pass.

## Gateway-only tier (TokenReview)

Existing workloads that authenticate with a projected ServiceAccount bearer token have **no Agent resource**, so steps 2 to 4 above do not apply. Routing is governed by the ModelProvider's own allowlist plus its model list:

1. **Token to namespace**: `TokenReview` yields the caller's authenticated namespace (see [Mode 2](workload-identity.md#mode-2-serviceaccount-bearer-token)).
2. **Model name to ModelProvider**: the gateway parses the `provider/model` qualified name from the request body (see [Model identification](request-handling.md#model-identification)). The provider prefix must resolve to an existing `ModelProvider` by `metadata.name`; if not, the request is rejected with `400 invalid_request`.
3. **Namespace allowlist**: the caller's namespace must match a `ModelProvider.spec.allowedNamespaces` entry (exact name or glob). If not, the request is rejected with `403 access_denied`.
4. **Model allowlist**: the requested model must appear in `ModelProvider.spec.models`. If not, the request is rejected with `400 invalid_request`.
5. **Forward**: the gateway reads `spec.endpoint`, `spec.type`, and credentials and forwards the request.

**AgentClass `allowedProviders` is deliberately not enforced in this tier.** AgentClass is the platform-team policy layer for the full-lifecycle tier; gateway-only workloads are not Agents and are not associated with any AgentClass. Platform teams who need class-scoped provider policy must onboard workloads through the full Agent lifecycle tier. The gateway-only tier trades that policy surface for a zero-CRD on-ramp, see [What Kaalm provides](../../concepts/vision-and-scope.md#what-kaalm-provides) and [Tiered on-ramp](../../operations/deployment.md#tiered-on-ramp).

## Credential handling

Provider credentials are stored as Secrets in `kaalm-system` and referenced by ModelProvider. The gateway reads these Secrets directly at startup and watches them for rotation. Credentials never leave `kaalm-system`: there is no per-agent or per-namespace credential copying. The full storage-to-rotation lifecycle, including who else can read the Secret, is in [Lifecycle of an LLM API key](../../security/credentials.md#lifecycle-of-an-llm-api-key).

The credential's shape is adapter-specific: for Anthropic, OpenAI, and OpenAI-compatible providers, the referenced Secret holds a static API key that the adapter injects as the provider's auth header. The reserved `google-vertex` type is not served: it would need OAuth2 access tokens minted from a GCP service-account key rather than a static key (see [the type's status](request-handling.md#the-google-vertex-type-is-reserved)).

When a credential Secret is updated, the gateway's Secret watcher picks up the change and refreshes the in-memory credential without a restart.

## Provider adapters

The gateway supports multiple upstream provider types through a small adapter interface. Each adapter carries the per-provider knowledge the shared proxy depends on:

```go
type providerAdapter interface {
    formatName() string                                  // "anthropic", "openai": the format for logs and metrics
    injectCredential(h http.Header, credential string)   // the provider's auth header
    extractUsage(body []byte) (Usage, bool)              // token counts from a buffered response
    accumulateStreamUsage(data []byte, u *Usage)         // token counts folded from one SSE payload
    fixupRequestBody(body map[string]any)                // per-provider request rewrites
    upstreamPath(inboundPath, modelID string) string     // the upstream path for the inbound one
}
```

Adapters exist for Anthropic, OpenAI, and OpenAI-compatible providers (Ollama, vLLM, and LiteLLM gateways). The `google-vertex` type is reserved and not served; its adapter's outbound pieces remain in the code but no inbound path reaches them (see [the type's status](request-handling.md#the-google-vertex-type-is-reserved)).

Pre-call token estimation is not used for budget gating (it adds latency and is inaccurate). Budget checks use the last-known spend state. Post-call actual usage is authoritative for accounting.

In detail:

- **Path patterns**: the proxy maps each inbound path to an adapter, which is how the gateway detects the request format. See [Request format detection](request-handling.md#request-format-detection).
- **Usage extraction**: `extractUsage` and `accumulateStreamUsage` know where each provider puts token counts (`usage.input_tokens` / `usage.output_tokens` for Anthropic, `usage.prompt_tokens` / `usage.completion_tokens` for OpenAI, `usageMetadata.promptTokenCount` / `usageMetadata.candidatesTokenCount` for Google Vertex), for both buffered and streamed responses. See [Streaming responses](request-handling.md#streaming-responses).
- **Request fixups**: the OpenAI adapter injects `stream_options: {"include_usage": true}` into streaming requests when absent, and the Vertex adapter appends `?alt=sse` to `:streamGenerateContent` requests when absent and rewrites the URL path's `{model}` segment to the raw model ID. Both fixups exist so usage data is observable, see [Streaming responses](request-handling.md#streaming-responses).

Provider health probes are not an adapter concern: the [ModelProviderReconciler](../../controller/reconcilers.md#modelproviderreconciler) runs them, using the same credential material as live traffic. The adapter interface is deliberately narrow. Format translation for a fallback edge that crosses formats lives outside the adapters ([Crossing formats](fallback.md#crossing-formats)), so an adapter never converts one provider's request shape into another's.

## Upstream TLS configuration

The gateway always connects to upstream LLM providers over HTTPS. For enterprise environments that require custom CA bundles or HTTP proxies for outbound traffic, the gateway supports:

- **Custom CA bundle**: a ConfigMap in `kaalm-system` (`kaalm-upstream-ca`) containing additional CA certificates. You create the ConfigMap; set the Helm value `gateway.upstreamCA.configMap` to its name and the chart mounts it and points the gateway at it. The gateway loads these on startup and watches for changes. All upstream HTTPS connections trust both the system CA bundle and the custom bundle.
- **Cluster CA (in-cluster providers)**: the Helm value `gateway.trustClusterCAForUpstream: true` adds the cluster's own CA (`kaalm-ca`, already mounted at `/var/run/kaalm/ca.crt`) to the upstream trust pool. This is the turnkey path for a self-hosted `openai-compatible` provider running in the cluster with an endpoint served by a `kaalm-ca-issuer` certificate, without supplying a separate `kaalm-upstream-ca` bundle. It composes with the custom bundle above; both are added to the system roots.
- **HTTP proxy**: standard `HTTPS_PROXY` / `NO_PROXY` environment variables on the gateway Deployment. The gateway respects these for all upstream provider calls.

These are gateway-level settings (not per-ModelProvider) because they typically reflect cluster-wide network infrastructure.
