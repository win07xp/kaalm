# HTTP API

This chapter defines the HTTP endpoints exposed by the Kaalm Gateway and the contract for agent-implemented endpoints. For the CRD specifications behind these endpoints, see [Resource Overview](../../resources/overview.md).

The endpoints are split across the following pages: the [channel webhook](channel-webhook.md) covers the external entry point that delivers messages to Agents, and [task completion](task-complete.md) covers how AgentTask Pods report results. The [agent endpoints](agent-endpoints.md#post-v1message) page covers the [heartbeat](agent-endpoints.md#post-v1agentheartbeat) an Agent sends to the gateway and the `POST /v1/message` contract an Agent must itself implement, while [async webhook responses](async-responses.md) describes the gateway-managed mechanism for returning late replies to webhook callers. Finally, [internal endpoints](internal-endpoints.md#get-v1channelshealth) documents the controller-only [activity](internal-endpoints.md#get-v1activity) and channel-health APIs, and the [error reference](errors.md#llm-gateway-error-responses) collects the structured error envelopes for both the [LLM Gateway](errors.md#llm-gateway-error-responses) and the [User Gateway](errors.md#user-gateway-error-responses).

## Authentication at a Glance

The LLM proxy endpoints accept either **mTLS** (Kaalm-managed Agent/AgentTask Pods) or a **`TokenReview`-validated ServiceAccount bearer token** (gateway-only tier), with a **source-IP to Pod cross-check** in both modes. The agent-only internal endpoints, `POST /v1/task/complete` and `POST /v1/agent/heartbeat`, are **mTLS-only**: there is no SA-bearer alternative, and gateway-only-tier workloads (which have no AgentTask or Agent identity) cannot reach them. The controller-only endpoints, `GET /v1/activity` and `GET /v1/channels/health`, are likewise **mTLS-only** and additionally require the controller's SAN; Agent/AgentTask client certs are rejected with `403`. See [Namespace Identification](../llm/workload-identity.md) for the full flow and [Agent→Gateway Authentication](../../security/rbac.md#agent-to-gateway-authentication) for the threat-model analysis.

Kubelet liveness/readiness probe endpoints (`/healthz`, `/readyz`) terminate on a separate internal health port, not on `:8443` or `:8080`, and are documented in [Gateway Readiness](../llm/operations.md#gateway-readiness). They are out of scope for this chapter.

## Reserved Gateway Paths

The following path prefixes are reserved for gateway-internal use and must not be used as `AgentChannel.spec.webhook.path` values. These paths conflict with gateway-internal endpoints: `/v1/` is served on the LLM Gateway listener at `:8443` for gateway-internal calls and on the User Gateway listener at `:8080` for the async polling endpoint, and is otherwise reserved on `:8080` against webhook path collisions:

- `/v1/` reserves all current and future gateway-internal endpoints (LLM proxy paths, task completion, heartbeat, async polling, channel health, activity). The controller's `POST /v1/activate/{namespace}/{agentName}` lives on the controller Service (port 9443), not the gateway, and is therefore unreachable from a webhook path regardless of this rule. See [Operator Structure](../../controller/overview.md) and [Activator](../user/activation-and-activity.md#the-activator) for the activator wire contract.

AgentChannels whose `spec.webhook.path` begins with `/v1/` are rejected at apply time by the CRD CEL validation in [Cross-Resource Validation rule 16](../../resources/validation-and-defaulting.md#cross-resource-validation). Rule 16 overlaps rule 15 (the required `/channels/{namespace}/` prefix already excludes `/v1/`), but rule 15 is enforced at reconcile time (CRD CEL cannot read `metadata.namespace`), so rule 16 is the only apply-time guard on reserved paths: the resource is rejected at the API server before the AgentChannelReconciler ever observes it, and no `Ready=False` status is set. The recommended developer convention is to use `/channels/` as a prefix (e.g., `/channels/team-support/support-assistant`).

## LLM Proxy Endpoints

The LLM proxy accepts agent requests on the upstream provider's native API paths and forwards them to the resolved ModelProvider. Recognized path patterns:

- `/v1/messages`: Anthropic format
- `/v1/chat/completions`: OpenAI / OpenAI-compatible format (also vLLM, Ollama, LiteLLM)
- Provider-specific paths: see [Request Format Detection](../llm/request-handling.md#request-format-detection) for the full mapping

Request and response bodies are passthrough to the upstream provider's native format; Kaalm adds no envelope of its own. The gateway injects the provider credential (after stripping the caller's own auth headers, hop-by-hop headers, and `Accept-Encoding` per the forwarded-header contract in [Request Flow](../llm/request-handling.md#request-flow) step 7), strips the `provider/` prefix from the model name, and relays the response (including SSE streams transparently). Errors returned by the gateway itself (budget exhaustion, rate limit, fallback exhaustion) use the structured envelope documented in [LLM Gateway Error Responses](errors.md#llm-gateway-error-responses).

**Request body size.** Inbound LLM-proxy requests are capped at `gateway.maxLLMRequestBodyBytes` (Helm-configurable, default 4 MiB). Bodies above the cap are rejected with `413 request_too_large` before forwarding to the upstream provider. The cap exists for two reasons: it bounds per-request gateway memory (the request body is buffered before forwarding to support provider API-key injection and token-usage extraction), and it limits the blast radius of a single buggy or malicious agent. The default of 4 MiB accommodates large context windows (e.g., Claude long-context prompts); operators with stricter footprint requirements can lower it. The cap applies uniformly to every recognized LLM-proxy path (`/v1/messages`, `/v1/chat/completions`, provider-specific paths). See [LLM Gateway Error Responses](errors.md#llm-gateway-error-responses) for the 413 wire contract.

Auth, namespace identification, provider routing, budget enforcement, fallback, and streaming behavior are documented in [LLM Gateway](../llm/overview.md). The per-path auth profile for these endpoints is consolidated in [The Kaalm Gateway](../overview.md) (`:8443` listener auth profile table).
