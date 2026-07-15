# Provider Routing and Adapters

Once the gateway knows which namespace a request came from, it still has to answer two more questions before it can forward anything upstream: *is this caller allowed to use this provider?* and *how do I speak to that provider?* Provider routing answers the first. Provider adapters answer the second.

Both variants of routing run only after [Namespace Identification](workload-identity.md) has produced an authenticated namespace. Routing itself differs between the two authentication tiers, because the gateway-only tier has no Agent resource to consult.

The two variants below are the same gate chain with the class-level and workload-level gates removed; [Provider access gating](../../concepts/tenancy-and-tiers.md#provider-access-gating) draws both tiers as a single figure, with the denial code on every arm.

## mTLS tier (Agentry-managed Pods)

Agents and AgentTasks created by the controller have an Agent (or AgentTask) resource with `spec.providers`, and an AgentClass with `allowedProviders`. The gateway walks the full chain:

1. **Source IP -> Pod**: resolved from the Pod informer cache (see [Namespace Identification](workload-identity.md)).
2. **Pod -> Agent**: the Pod's ownerRef identifies the Agent (or AgentTask) resource. The gateway maintains an Agent informer cache for this lookup.
3. **Agent -> allowed providers**: the Agent's `spec.providers` lists the ModelProviders this agent may use. The referenced providers must also appear in the AgentClass's `allowedProviders`. The gateway resolves the class from the workload's `agentClassRef` via its AgentClass informer (see [Gateway Readiness](operations.md#gateway-readiness) and [Gateway ServiceAccount permissions](../../security/rbac.md#gateway-serviceaccount-permissions)).
4. **Model name -> ModelProvider**: the gateway parses the `provider/model` qualified name from the request body (or, for Vertex-format requests, from the URL path's `{model}` segment, see [Model Identification](request-handling.md#model-identification)). The provider prefix must match a `providerRef` in the Agent's `spec.providers`. If it does not, the request is rejected.
5. **ModelProvider -> upstream**: the gateway reads the ModelProvider's `spec.endpoint`, `spec.type`, and credentials to forward the request. The namespace must also be in the ModelProvider's `allowedNamespaces`.

This chain ensures that an agent can only reach ModelProviders explicitly listed in its spec, which in turn must be in the AgentClass's `allowedProviders` and must include the agent's namespace in `allowedNamespaces`. All three access checks (Agent -> ModelProvider -> Namespace) must pass.

## Gateway-only tier (TokenReview)

Existing workloads that authenticate with a projected ServiceAccount bearer token have **no Agent resource**, so steps 2 to 4 above do not apply. Routing is governed by the ModelProvider's own allowlist plus its model list:

1. **Token -> namespace**: `TokenReview` yields the caller's authenticated namespace (see [Mode 2](workload-identity.md#mode-2-serviceaccount-bearer-token)).
2. **Model name -> ModelProvider**: the gateway parses the `provider/model` qualified name from the request body (or the URL path for Vertex-format requests, see [Model Identification](request-handling.md#model-identification)). The provider prefix must resolve to an existing `ModelProvider` by `metadata.name`; if not, the request is rejected with `400 invalid_request`.
3. **Namespace allowlist**: the caller's namespace must match a `ModelProvider.spec.allowedNamespaces` entry (exact name or glob). If not, the request is rejected with `403 access_denied`.
4. **Model allowlist**: the requested model must appear in `ModelProvider.spec.models`. If not, the request is rejected with `400 invalid_request`.
5. **Forward**: the gateway reads `spec.endpoint`, `spec.type`, and credentials and forwards the request.

**AgentClass `allowedProviders` is deliberately not enforced in this tier.** AgentClass is the platform-team policy layer for the full-lifecycle tier; gateway-only workloads are not Agents and are not associated with any AgentClass. Platform teams who need class-scoped provider policy must onboard workloads through the full Agent lifecycle tier. The gateway-only tier trades that policy surface for a zero-CRD on-ramp, see [What Agentry Provides](../../concepts/vision-and-scope.md#what-agentry-provides) and [Tiered On-Ramp](../../operations/deployment.md#tiered-on-ramp).

## Credential Handling

Provider credentials are stored as Secrets in `agentry-system` and referenced by ModelProvider. The gateway reads these Secrets directly at startup and watches them for rotation. Credentials never leave `agentry-system`: there is no per-agent or per-namespace credential copying. The full storage-to-rotation lifecycle, including who else can read the Secret, is in [Lifecycle of an LLM API key](../../security/credentials.md#lifecycle-of-an-llm-api-key).

The credential's shape is adapter-specific. For Anthropic, OpenAI, and OpenAI-compatible providers, the referenced Secret holds a static API key that the adapter injects as the provider's auth header. Google Vertex does not accept static API keys: its Secret holds a GCP service-account JSON key, and the Vertex adapter mints OAuth2 access tokens from it (cached in memory and refreshed roughly 5 minutes before the ~1-hour expiry), attaching the current token as the `Authorization: Bearer` header on LLM requests and health probes alike.

When a credential Secret is updated, the gateway's Secret watcher picks up the change and refreshes the in-memory credential without a restart (for Vertex, the next token mint uses the new service-account key).

## Provider Adapters

The gateway supports multiple upstream provider types via a pluggable adapter interface:

```go
type ProviderAdapter interface {
    Type() string                               // "anthropic", "openai", etc.
    ExtractUsage(resp Response) (Usage, error)
    ForwardRequest(ctx, req, credentials) (Response, error)
    HealthCheck(ctx, endpoint, credentials) error
}
```

v1 ships adapters for: Anthropic, OpenAI, Google Vertex, OpenAI-compatible (Ollama/vLLM/LiteLLM gateways).

Pre-call token estimation is not used for budget gating (it adds latency and is inaccurate). Budget checks use the last-known spend state. Post-call actual usage is authoritative for accounting.

Each adapter carries the per-provider knowledge the rest of the gateway depends on:

- **Path patterns**: each adapter registers the URL paths it recognizes, which is how the gateway detects the request format. See [Request Format Detection](request-handling.md#request-format-detection).
- **Usage extraction**: `ExtractUsage` knows where each provider puts token counts (`usage.input_tokens` / `usage.output_tokens` for Anthropic, `usage.prompt_tokens` / `usage.completion_tokens` for OpenAI, `usageMetadata.promptTokenCount` / `usageMetadata.candidatesTokenCount` for Google Vertex), for both buffered and streamed responses. See [Streaming Responses](request-handling.md#streaming-responses).
- **Request fixups**: the OpenAI adapter injects `stream_options: {"include_usage": true}` into streaming requests when absent, and the Vertex adapter appends `?alt=sse` to `:streamGenerateContent` requests when absent and rewrites the URL path's `{model}` segment to the raw model ID. Both fixups exist so usage data is actually observable, see [Streaming Responses](request-handling.md#streaming-responses).
- **Health probes**: `HealthCheck` is what the ModelProviderReconciler calls to mark a provider healthy, using the same credential material as live traffic.

The adapter interface is deliberately narrow. The gateway is protocol-aware but does not translate between formats, so an adapter never has to convert one provider's request shape into another's.

## Upstream TLS Configuration

The gateway always connects to upstream LLM providers over HTTPS. For enterprise environments that require custom CA bundles or HTTP proxies for outbound traffic, the gateway supports:

- **Custom CA bundle**: a ConfigMap in `agentry-system` (`agentry-upstream-ca`) containing additional CA certificates. The gateway loads these on startup and watches for changes. All upstream HTTPS connections trust both the system CA bundle and the custom bundle.
- **HTTP proxy**: standard `HTTPS_PROXY` / `NO_PROXY` environment variables on the gateway Deployment. The gateway respects these for all upstream provider calls.

These are gateway-level settings (not per-ModelProvider) because they typically reflect cluster-wide network infrastructure.
