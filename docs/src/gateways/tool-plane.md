# The tool plane

The **tool plane** is gateway-brokered access to MCP tool servers, governed the way LLM access is governed: the agent calls the gateway, the gateway authenticates the workload, applies the tenancy gates, injects the credential, and forwards. The LLM plane, the runtime contract, and the direct-egress exception are unchanged by it.

Without the broker, tool traffic is the ungoverned twin of LLM traffic: a class-wide CIDR exception in the default-deny egress policy, with the tool server's credential in the agent pod, unmetered and absent from the audit log. That is the arrangement [Credential handling](../security/credentials.md) forbids for LLM keys, and the reason LLM traffic has no direct provider egress ([Multi-tenancy and adoption tiers](../concepts/tenancy-and-tiers.md#networkpolicy-as-the-cross-tenant-boundary)). The tool plane applies the same pattern to tool servers.

## What counts as a tool

"Tool" names three things that cross three different boundaries, and no single mechanism governs all three. Claude Code is the running example, since it uses all three at once.

| Category | Example | Where it executes | What Kaalm can do |
|---|---|---|---|
| In-process | a file-read or shell function inside the agent | The agent's own pod | Govern the blast radius, observe indirectly, never intercept |
| Provider-side | an LLM provider's built-in web search | Inside the LLM provider, during the API call | Observe at the LLM gateway; it already sits on this wire |
| External server | an MCP server | A network service with its own credential | Broker, meter, and enforce: this chapter's subject |

The governing principle: **a tool is modeled by the resource that governs the connection it crosses.** [ModelProvider](../resources/modelprovider.md) governs everything that happens inside the LLM connection, including provider-side tools. ToolProvider governs the connections the gateway opens for tools. The pod's sandbox governs tools that never open a connection. Every category has exactly one home, and no resource claims to see traffic it does not carry.

### In-process tools

An in-process tool call never leaves the pod: the completion carries a `tool_use` block, the agent's own code runs a local function, and the result returns as a `tool_result` block in the next LLM request. Kaalm cannot intercept that call. It governs the blast radius with the controls the book already has: the image allowlist (rule 2) reviewed the code that is the tool, the synthesized NetworkPolicy bounds what a shell tool can reach, the RuntimeClass bounds what it can escape to, and resource limits bound what it can consume.

Observing in-process calls, either at the LLM gateway inside the `tool_use` and `tool_result` blocks that pass through the proxy or through a cooperative SDK hook, is designed and not built; both are [roadmap](../ROADMAP.md#beyond) items. In-process metering is out of scope: no marginal cost crosses a boundary Kaalm bills against, and the pod's CPU and egress are metered by Kubernetes.

### Provider-side tools

A provider-side tool (an LLM provider's built-in web search, for example) runs inside the provider during the API round trip and is billed by the provider on top of tokens. It has no endpoint, credential, connection, or health of its own, so under the principle it belongs to the ModelProvider rather than to a resource of its own.

What ships is observation. The provider adapters, which already parse response bodies for token usage, extract server-tool-use counts and emit `kaalm_llm_server_tool_use_total{provider, namespace, tool}`. Each adapter reports what its wire format carries:

| Format | Source | `tool` label |
|---|---|---|
| Anthropic | `usage.server_tool_use`, cumulative on streamed `message_delta` events | the counter name, for example `web_search_requests` |
| Vertex | `groundingMetadata.webSearchQueries`, one per executed query | `google_search` |
| OpenAI chat completions | no usage-level server-tool counts on the paths the gateway proxies | none emitted |

Pricing and enforcement do not ship. Provider-side tool charges are money the budget ledger cannot see, the same gap as usage-less responses under [Hard enforcement](llm/budgets-and-rate-limits.md#hard-enforcement): rule 33 refuses to cap what the ledger prices at zero, and that applies to server tools too. A `serverTools[].costPerCallUSD` facet on the ModelProvider is the designed form and a [roadmap](../ROADMAP.md#beyond) item, as is stripping or rejecting an undeclared server tool in the request body. No new grant surface exists for provider-side tools: a workload's access is scoped by the provider grant chain it already has (`spec.providers`, class `allowedProviders`, provider `allowedNamespaces`).

## The ToolProvider resource

External tool servers get a cluster-scoped CRD that mirrors ModelProvider, because the tenancy and credential model transfers whole: the platform team registers the server once, holds its credential in `kaalm-system`, and admits namespaces to it. The name ToolProvider, over ToolServer, states the symmetry: the resource provides tools the way a ModelProvider provides models.

```yaml
apiVersion: kaalm.io/v1beta1
kind: ToolProvider
metadata:
  name: search-tools
spec:
  type: mcp
  endpoint: https://mcp-search.tools.svc:8443
  credentialsRef:
    name: search-tools-key
    key: token
  allowedNamespaces:
    - "team-*"
  tools:
    - id: web_search
    - id: fetch_page
  rateLimits:
    requestsPerMinute: 120
  healthCheck:
    enabled: true
    intervalSeconds: 60
```

Every field, its default, and its design note is on the [ToolProvider](../resources/toolprovider.md) reference page. Three facts carry into the rest of this chapter: `credentialsRef` is optional and resolved only from `kaalm-system`; `tools` is a ceiling when declared and absent otherwise; and the health probe speaks MCP in whichever revision the server does, recording it in `status.mcpRevision` (see [Protocol revisions](#protocol-revisions)).

Validation follows the established families. Each rule is specified under [Cross-resource validation](../resources/validation-and-defaulting.md#cross-resource-validation).

| Rule | Checks | Analog | Reason on failure |
|---|---|---|---|
| 35 | every `spec.tools[].providerRef` names an existing ToolProvider | rule 3 | `ClassConstraintViolation` |
| 36 | every referenced ToolProvider admits the workload's namespace | rule 4 | `ClassConstraintViolation` |
| 37 | every referenced ToolProvider is in the class's `allowedToolProviders` | rule 5 | `ClassConstraintViolation` |
| 38 | with a declared catalog, every granted tool name is in it | none | `ToolNotInCatalog`, naming the missing tools |

All four join the class-mismatch handling family of rules 2 through 5: a violation is a recoverable `Degraded` on an Agent, never a stranded workload.

## Grants

Access is granted per server and narrowed per tool, mirroring the provider grant chain gate for gate:

```yaml
# AgentClass.spec
allowedToolProviders:
  - name: search-tools

# Agent.spec and AgentTask.spec, identical
tools:
  - providerRef:
      name: search-tools
    tools: ["web_search"]
```

The inner `tools` list narrows the grant. Empty or omitted means every tool the server offers, bounded by the declared catalog when one exists.

Grants live on both workload kinds. A goal-driven one-shot task is, if anything, the workload that needs tools most, so an AgentTask declares `spec.tools` exactly as an Agent does and rules 35 to 38 gate it the same way. The only difference is how a violation settles: terminal `Failed` rather than recoverable `Degraded`, because tasks have no Degraded phase, the split rules 2, 5, and 24 already follow.

![Flowchart of every check on POST /v1/mcp/{toolProvider} in the order the broker runs them, as four rows. Route and namespace: ToolProvider exists, else 400 invalid_request; caller namespace in allowedNamespaces, else 403 access_denied. Workload grant, for mTLS callers only: ToolProvider in the workload's spec.tools providerRef, and in the AgentClass allowedToolProviders, else 403 access_denied. Request: token bucket per namespace and ToolProvider, else 429 rate_limited; body within the cap, else 413 request_too_large; one JSON-RPC message, else 400 invalid_request; method on the allowlist, else 403 tool_denied. Tool and session: modern headers match the body, else 400 with JSON-RPC error -32020; tools/call names a tool in the grant and catalog, else 403 tool_denied; a legacy session id is owned by this caller, else 403 access_denied; then inject the credential and forward.](../diagrams/tool-grant-chain.svg)

The figure is the [provider gate chain](../concepts/tenancy-and-tiers.md#provider-access-gating) with per-tool checks added. The namespace, grant, and class gates answer `403 access_denied`, as the LLM chain does. The method allowlist and the tool check answer `403 tool_denied`, a distinct type so per-tool policy is auditable on its own. What each check reads is tabled under [The broker](#the-broker).

A gateway-only caller carries no workload identity, so the workload grant row does not apply to it: its access is `allowedNamespaces`, the declared catalog, and the method allowlist ([Workload identity](llm/workload-identity.md)). A `tools/list` answer is filtered by the tool check instead of rejected by it, so the model never sees a tool it cannot call.

## The broker

Brokered tool traffic terminates on the `:8443` listener as `POST /v1/mcp/{toolProviderName}`. There is no third listener: internal endpoints share `:8443` so authenticated callers reach them without a new listener ([listener profile](overview.md#internal-endpoints)), and the `:8080` rule forbids mTLS paths on the Ingress-fronted listener. The path uses the **dual-mode** caller-identity profile the LLM proxy paths use: Kaalm-managed pods authenticate by mTLS SAN (their ServiceAccount tokens are rejected), and gateway-only workloads by `TokenReview`-validated bearer token. The profile establishes who is calling; the broker enforces the grant chain itself.

![Sequence diagram of one brokered tool call. The agent container POSTs to /v1/mcp/{toolProvider} on the gateway broker at :8443 with a JSON-RPC body. The broker establishes the caller identity, runs the checks in order, reads the credential from a Secret in kaalm-system, strips inbound auth and injects the credential, and forwards to the tool server with no redirects and an upstream timeout. Three outcomes: unreachable, redirect, 401, 403, or 5xx is 503 tool_unavailable; a timeout is 504 tool_timeout; a response, JSON or an SSE stream, has tools/list filtered with cacheScope set to private and a legacy Mcp-Session-Id wrapped, then is relayed. The call ends with one audit line and the metrics.](../diagrams/tool-broker-flow.svg)

The checks in step 3, in the order the broker runs them:

| Check | Reads | Applies to | On failure |
|---|---|---|---|
| ToolProvider exists | the path segment against the ToolProvider informer | both tiers | `400 invalid_request` |
| Namespace gate | `allowedNamespaces`, exact name or glob | both tiers | `403 access_denied` |
| Workload grant | the Agent or AgentTask `spec.tools[].providerRef` | mTLS callers | `403 access_denied` |
| Class allowlist | the AgentClass `allowedToolProviders` | mTLS callers | `403 access_denied` |
| Rate limit | the token bucket for the (namespace, ToolProvider) pair | both tiers | `429 rate_limited`, `Retry-After: 1` |
| Body cap | the request body against the broker's cap | both tiers | `413 request_too_large` |
| Single message | the body is one JSON-RPC message, not a batch array | both tiers | `400 invalid_request` |
| Method allowlist | the JSON-RPC `method` (see [Transport](#transport)) | both tiers | `403 tool_denied`, naming the method |
| Modern headers | `Mcp-Method` and `Mcp-Name` against the body on 2026-07-28 requests | both tiers | `400` with JSON-RPC error `-32020` |
| Tool | on `tools/call`, `params.name` against the grant narrowed by the catalog | both tiers | `403 tool_denied`, naming the tool |
| Session owner | a presented `Mcp-Session-Id` against the caller identity, on legacy requests | both tiers | `403 access_denied` |

Every denial fires before the credential is read, so a rejected call never touches the Secret and never dials the tool server. After the checks the broker reads the credential (`503 tool_unavailable` when the Secret is unreadable), forwards, and relays. Step 7 narrows a `tools/list` answer to what the caller may call, and step 8 wraps a legacy session id so that only its owner can resume it.

### Transport

The broker speaks MCP streamable HTTP: JSON-RPC over `POST`, with SSE response streams relayed line by line as they arrive, the same relay the LLM proxy uses for streamed completions. Out of scope: the legacy HTTP+SSE dual-endpoint transport, which is aging out of the protocol; stdio, because the gateway hosts no processes; and the server-initiated `GET` notification stream. Brokered calls are request-scoped, so a tool server that needs to push has no path through the broker; the [roadmap](../ROADMAP.md#beyond) names it.

Three wire rules follow from the request-scoped design. JSON-RPC batch arrays are rejected with `400 invalid_request` in every revision: the newest revision dropped batching, and per-element enforcement would add nothing. The broker carries an explicit method allowlist: silently proxying a surface the grant chain has no vocabulary for would be no governance at all, and widening an allowlist is additive while narrowing one is breaking. And a filtered `tools/list` answer is returned as plain JSON whatever the upstream encoding, because both encodings are legal for the caller and rewriting inside an SSE stream gains nothing.

| Method | Era | Brokered |
|---|---|---|
| `server/discover`, `tools/list`, `tools/call` | both | yes |
| `initialize`, `ping`, `notifications/*` | the legacy handshake surface, retired by 2026-07-28 | yes, for the deprecation window |
| `resources/*`, `prompts/*`, `sampling/*`, `subscriptions/listen`, anything else | both | no: `403 tool_denied` naming the method |

### Credential injection

The same as the LLM path: inbound auth material is stripped, and the ToolProvider's credential is sent upstream as `Authorization: Bearer`. A ToolProvider without `credentialsRef` sends no `Authorization` header. No credential-bearing byte leaves `kaalm-system`, and the e2e proof obligation carries over from the LLM plane: the tool credential is absent from the agent pod, by inspection.

### Session ownership (legacy revisions)

Through the 2025 revisions, MCP sessions travel in an `Mcp-Session-Id` header, and a broker that passed them through verbatim would let one workload resume another's session. The binding is stateless, because an in-memory table would break on cross-replica routing. The broker never reveals the upstream id. What the agent receives is:

```text
base64url(upstreamID) "." base64url(HMAC-SHA256(key, upstreamID || 0x00 || callerIdentity))
```

The key is the gateway-shared `KAALM_MCP_SESSION_KEY`, a chart-managed Secret, and the caller identity is `namespace/Kind/name` for a workload and `ns/namespace` for the bearer tier. On every later request any replica recomputes the HMAC from the presented id and the authenticated identity; a mismatch is `403 access_denied` before anything is forwarded. The wrap applies to every relayed response that carries the header, including a relayed 4xx, so a session is unresumable by anyone but its owner by construction.

The 2026-07-28 revision removes protocol-level sessions, so this control applies exactly as long as the header does. On a modern request the broker ignores a presented `Mcp-Session-Id`, as the revision instructs, and the property the binding protected is carried by the per-request caller identity the broker authenticates on every call.

### Protocol revisions

MCP revision **2026-07-28** makes the protocol stateless. Kaalm is dual-era for the revision's twelve-month deprecation window, which keeps 2025-era servers legitimate until about mid-2027: the probe negotiates per server, and the broker enforces per request. The legacy paths (the handshake, the session wrap) stay for as long as the window runs. Their removal is a breaking change for tool servers, announced under the [deprecation policy](../operations/api-versioning.md#deprecation-policy), never a change to Kaalm's own API.

| | Legacy (2025 revisions) | 2026-07-28 or later |
|---|---|---|
| Handshake | `initialize`, then requests | none; every request carries its version and capabilities in `_meta` |
| Version signal | no `MCP-Protocol-Version` header, or an earlier value | the `MCP-Protocol-Version` header, which the broker keys on |
| Session | `Mcp-Session-Id`, wrapped by the broker | none; a presented header is ignored |
| Mirrored headers | none | `Mcp-Method` on every POST, `Mcp-Name` on `tools/call` |
| List results | no cache fields | `ttlMs` and `cacheScope` |
| Health probe | `initialize`, then `tools/list` | `server/discover`, then `tools/list` with its version in `_meta` |

**The request's own revision selects the rules.** On a modern request the broker validates what the revision makes mandatory: `Mcp-Method` must be present and equal the body's `method`, and `Mcp-Name` must be present and equal `params.name` on `tools/call`, with Base64 sentinel values decoded first. A missing or mismatched header is `400` with JSON-RPC error `-32020` (`HeaderMismatch`), the response the spec requires of any body-processing server, sent in the JSON-RPC form rather than the gateway envelope so the caller's MCP client can read it. The body stays the source of truth for policy, because the body is what the upstream executes; the headers are a required consistency proof. The validated headers and any `Mcp-Param-*` headers are forwarded, and the server repeats the validation. On a legacy request none of these headers exist to check. The broker never translates between eras: a legacy caller against a modern-only server fails in the protocol's own vocabulary.

**The probe.** The health probe opens with `server/discover`. A success identifies a 2026-07-28 server, and the probe completes with a `_meta`-versioned `tools/list`. A modern JSON-RPC error answering it, an unsupported version for example, fails the probe with no legacy fallback. A `401` or `403` is a rejected credential. Any other answer identifies a legacy server, and the probe falls back to `initialize` followed by `tools/list`. A healthy probe records the negotiated revision in `status.mcpRevision`, so an operator can see which era each server speaks and when the fleet has moved.

**Caching a filtered list.** The `tools/list` relay narrows the catalog to the caller's grant, which turns a possibly `public` upstream answer into a caller-specific one. On modern responses the broker rewrites `cacheScope` to `private` before relaying, so no cache between the broker and the model serves one workload's tool surface to another. Legacy responses carry no such field and get none.

### Limits and SSRF protection

Request and response bodies are capped, and each call carries an upstream timeout. As shipped both reuse the LLM proxy's values: the cap is `gateway.maxLLMRequestBodyBytes`, and the timeout is `gateway.providerFirstByteTimeout`, which bounds the whole call as described under [Request handling](llm/request-handling.md).

ToolProvider endpoints are operator-declared configuration, exactly as ModelProvider endpoints are, so they get the trust provider endpoints get rather than the full [callback policy](../resources/validation-and-defaulting.md) that user-supplied callback URLs receive. The schema requires `https://`, and the broker never follows redirects, which closes the confused-deputy path a compromised tool server could otherwise open.

## Audit and metering

Every brokered call emits one `info`-level structured log line. Bodies are never logged: tool-call content is the category the [log-redaction rule](../operations/observability.md#pii-safety) names as sensitive. The bodylog debug facility covers the MCP routes under its existing gate, for operators who accept that trade during an investigation.

| Field | Value |
|---|---|
| `namespace`, `workload`, `workload_kind` | the caller; the workload fields appear for mTLS callers only |
| `provider`, `method`, `tool` | the ToolProvider, the JSON-RPC method, and the real tool name |
| `status`, `error_type` | the HTTP status and the wire error type, when the broker produced the error |
| `detail` | the denial reason, for example which gate refused or that a session id belongs to another caller |
| `duration_seconds`, `request_bytes`, `response_bytes` | timing and sizes |

| Metric | Labels | Notes |
|---|---|---|
| `kaalm_tool_calls_total` | `provider`, `namespace`, `tool`, `status` | `status` is `ok` for a relayed 2xx, the wire error type for every failure the broker produces (`access_denied`, `tool_denied`, `rate_limited`, `header_mismatch`, and the rest of the [error vocabulary](api/errors.md)), and `upstream_error` for a relayed non-2xx |
| `kaalm_tool_call_duration_seconds` | `provider`, `tool` | forwarded calls only: local denials complete in microseconds and would drag the percentiles toward zero exactly when a flood of denials coincides with real upstream latency |

The `tool` label is bounded by the **declared** catalog. On a provider with `spec.tools`, cataloged ids appear verbatim and anything else collapses to `uncataloged`. On a provider without one, every tool collapses, because wire-supplied names are unbounded and a compromised server could inflate the label set at will. The audit record always carries the real name. Declare catalogs for this reason, per the [cardinality rules](../operations/observability.md#cardinality).

Metering is **rate limits and audit, not budgets**. Tool calls carry no token-price dimension, and rule 33's argument against capping unpriced calls applies here as it does to [provider-side tools](#provider-side-tools). `ToolProvider.spec.rateLimits.requestsPerMinute` is a cluster-wide ceiling per (namespace, ToolProvider) pair that each replica divides by the live replica count, on the same token buckets the LLM plane uses. There is no USD budget for tools; the per-call cost a budget would need is a [roadmap](../ROADMAP.md#beyond) item.

## Failure modes

| Condition | Caller sees |
|---|---|
| Unknown ToolProvider name in the path | `400 invalid_request` |
| Namespace, grant, or class gate fails | `403 access_denied`, the type the LLM tenancy chain uses |
| Tool outside the grant or catalog | `403 tool_denied`, naming the tool |
| JSON-RPC method outside the allowlist | `403 tool_denied`, naming the method |
| Batch array, or not a JSON-RPC message | `400 invalid_request` |
| Modern header missing or mismatched | `400` with JSON-RPC error `-32020` |
| Rate limit exceeded | `429 rate_limited`, `Retry-After: 1` |
| Oversized request or response | `413 request_too_large` or `413 response_too_large` |
| Session id owned by another caller | `403 access_denied`; the audit record names the mismatch |
| Credential Secret unreadable | `503 tool_unavailable`, retryable |
| Tool server unreachable, redirecting, or 5xx | `503 tool_unavailable`, retryable, `Retry-After: 1` |
| Tool server rejects the gateway credential (401 or 403) | `503 tool_unavailable`, not retryable. The health probe sets `Healthy` and `Ready` to `False` with reason `CredentialsInvalid`; as shipped no Warning event is emitted on the ToolProvider for it |
| `tools/list` response the broker cannot parse | `503 tool_unavailable`, not retryable |
| Tool call exceeds the upstream timeout | `504 tool_timeout`, retryable |
| Other protocol-level 4xx from the server | relayed verbatim (an expired session's 404, for example), so MCP session semantics survive the broker |

The rows are in the [error schema](api/errors.md#llm-gateway-error-responses) with the rest of the cluster listener's error types. `retryable` follows the cause, not the status: the two `503` rows marked not retryable repeat until an operator fixes the credential or the tool server.

## Relationship to direct egress

The tool plane does not remove the direct-egress exception; it re-ranks it. In order of decreasing governance:

| Path | Credential | Metering and audit | Fits when |
|---|---|---|---|
| Brokered (`/v1/mcp/*`) | Held by the gateway, injected per call | Rate limits, per-call audit, metrics | The default for MCP tool servers |
| Direct egress (`allowedCIDRs` / `allowedHosts`) | Held by the agent pod | None beyond IP-level policy | Non-MCP protocols, or tools the platform team deliberately exempts |

There is no per-agent MCP egress field, and none is needed: brokered tools need no per-agent egress, and the direct tier is expressed through the class egress fields, an IP-level exception in the egress policy with the governance trade the table states.

## Acceptance scenario

[S18](../appendix/scenarios.md#s18-grant-an-agent-a-governed-tool) exercises the plane end to end: a platform engineer registers a ToolProvider whose credential lives in `kaalm-system`, a class allows it, an agent lists and calls tools through the gateway with no tool credential in its pod, a namespace outside the allowlist is denied, and a call to an ungranted tool is rejected with the distinct status. Two e2e specs prove it on a cluster, `Governed tool access (S18)` against a legacy mock and `MCP 2026-07-28 revision (S18, dual-era)` against a modern-only one, recorded in the [scenario coverage map](../appendix/scenario-coverage.md).
