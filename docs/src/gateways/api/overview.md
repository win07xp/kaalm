# HTTP API

This chapter is the wire contract for every HTTP endpoint the gateway serves, and for the one endpoint an agent serves to the gateway. The CRD fields behind the endpoints are in [Resource overview](../../resources/overview.md).

| Page | Endpoints |
|---|---|
| [Channel webhook](channel-webhook.md) | `POST /channels/{namespace}/{channel-path}` for `spec.type: webhook` |
| [Discord channel](channel-discord.md) | The same route for `spec.type: discord` |
| [WhatsApp channel](channel-whatsapp.md) | The same route for `spec.type: whatsapp`, plus the verification `GET` |
| [Async webhook responses](async-responses.md) | The `202` contract, callback signing, and `GET /v1/channels/responses/{requestId}` |
| [Agent endpoints](agent-endpoints.md) | `POST /v1/agent/heartbeat`, and `POST /v1/message` as served by the agent |
| [Task completion](task-complete.md) | `POST /v1/task/complete` |
| [Internal endpoints](internal-endpoints.md) | `GET /v1/activity`, `GET /v1/channels/health`, `POST /v1/test-chat`, `GET /v1/spend` |
| [Error reference](errors.md) | The error envelope and the status tables for both gateways |

## Endpoints by listener and caller

The gateway serves two TLS listeners. Which listener a path lives on decides who can reach it, and the client-auth regime decides who is accepted. The regimes are specified in [The :8443 listener profile](../overview.md#the-8443-listener-profile) and enforced as [per-path middleware](../listener-tls.md#per-path-client-auth-enforcement).

| Listener | Path | Caller | Client auth |
|---|---|---|---|
| `:8443` | `/v1/messages`, `/v1/chat/completions`, `/v1/completions` | Agent and AgentTask Pods, gateway-only-tier workloads | mTLS SAN or bearer token |
| `:8443` | `/v1/mcp/{toolProvider}` | The same callers | mTLS SAN or bearer token |
| `:8443` | `/v1/agent/heartbeat` | Agent Pods | mTLS, Agent SAN |
| `:8443` | `/v1/task/complete` | AgentTask Pods | mTLS, AgentTask SAN |
| `:8443` | `/v1/activity`, `/v1/channels/health` | The controller | mTLS, controller SAN |
| `:8443` | `/v1/test-chat`, `/v1/spend` | The console | mTLS, console SAN |
| `:8080` | `/channels/{namespace}/{channel-path}` | External callers through the Ingress | Per AgentChannel |
| `:8080` | `/v1/channels/responses/{requestId}` | The webhook caller polling for a late reply | The originating AgentChannel's auth |

Any other path on `:8443` answers `400 invalid_request` with the constant message `unrecognized path`; the paths on that listener are this fixed API, so the answer reveals nothing about a tenant. Any other path on `:8080` answers the same `401 unauthorized` as a failed credential, because channel paths on that listener belong to tenants ([User Gateway error responses](errors.md#user-gateway-error-responses)).

The kubelet probe endpoints, `/healthz` and `/readyz`, are on the separate health port, not on either listener ([Ports](../overview.md#ports)).

## Reserved gateway paths

The `/v1/` prefix is reserved for the gateway's own endpoints on both listeners. An AgentChannel path must not begin with it, in any of the three path fields (`spec.webhook.path`, `spec.discord.path`, `spec.whatsapp.path`).

Two rules enforce this, at different times:

- **Rule 16, at apply time.** The CRD's CEL validation rejects a path that begins with `/v1/` before the object is stored. The reconciler never sees it and sets no status.
- **Rule 15, at reconcile time.** The path must begin with `/channels/{namespace}/`, where `{namespace}` is the AgentChannel's own namespace, and no other AgentChannel may register the same path. A violation is `Ready=False` with `reason: InvalidPath` or `reason: PathConflict`, and the gateway never routes the path. CEL cannot read `metadata.namespace`, so this rule cannot run at apply time.

Rule 15 already excludes `/v1/`, so rule 16 adds only the earlier failure. Both rules are in [Cross-resource validation](../../resources/validation-and-defaulting.md#cross-resource-validation).

The controller's activator, `POST /v1/activate/{namespace}/{agentName}`, is served on the controller Service, port 9443, not on the gateway ([The activator](../user/activation-and-activity.md#the-activator)).

## LLM proxy endpoints

The proxy accepts requests on three paths and forwards each to the resolved ModelProvider in that provider's native format:

| Path | Request format |
|---|---|
| `/v1/messages` | Anthropic Messages |
| `/v1/chat/completions` | OpenAI chat completions, also served by vLLM, Ollama, and LiteLLM |
| `/v1/completions` | OpenAI completions |

The path names the inbound format; the provider's type names the outbound one. When the two differ, the gateway translates ([Request format detection](../llm/request-handling.md#request-format-detection)). Bodies pass through otherwise unchanged: the gateway adds no envelope of its own. It strips the caller's auth headers, hop-by-hop headers, and `Accept-Encoding`, injects the provider credential, strips the `{providerRef}/` prefix from the model name, and relays the response, including SSE streams ([Request flow](../llm/request-handling.md#request-flow), step 7).

Errors the gateway raises itself use the envelope in [LLM Gateway error responses](errors.md#llm-gateway-error-responses).

**Request body size.** Each of the three paths reads at most `gateway.maxLLMRequestBodyBytes` (default 4 MiB) and answers `413 request_too_large` above that. The body is buffered before forwarding, for credential injection and token-usage extraction, so the cap bounds per-request gateway memory. The tool broker reads its body under a separate cap ([The tool plane](../tool-plane.md)).

Auth, provider routing, budgets, fallback, and streaming are specified in [LLM Gateway](../llm/overview.md).
