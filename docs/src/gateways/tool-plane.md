# The Tool Plane

Since v0.3.0 the design includes a **tool plane**: gateway-brokered access to MCP tool servers, governed the way LLM access has been governed since v1. This chapter is the design; the implementation is the whole of the v0.4.0 milestone, and nothing described here ships in v0.3.0. Where a section states wire behavior, it states the v0.4.0 contract.

The world before this chapter is asymmetric, and the book already says so in its own words. LLM provider traffic is gateway-mediated with no direct provider egress, "what makes the spend and rate-limit controls unbypassable" ([Tenancy and Tiers](../concepts/tenancy-and-tiers.md#networkpolicy-as-the-cross-tenant-boundary)). Tool traffic is the opposite: a class-wide CIDR hole in the default-deny egress policy, port-unrestricted, credential-unmediated, unmetered, and audit-invisible. An agent that calls a tool server today holds that server's credential in its own pod, which is precisely the arrangement [Credential Handling](../security/credentials.md) exists to forbid for LLM keys. The tool plane closes the asymmetry by extending the pattern Kaalm already proved once: the agent calls the gateway, the gateway authenticates the workload, injects the credential, applies the tenancy gates, and forwards. Nothing about the existing LLM plane, the runtime contract, or the direct-egress escape hatch changes; the tool plane is an addition that re-ranks the options, not a replacement.

## What Counts as a Tool

"Tool" names three different things that cross three different boundaries, and the design treats them separately because no single mechanism can honestly govern all three. Claude Code is the running example, since it uses all three at once.

| Category | Example | Where it executes | What Kaalm can honestly do |
|---|---|---|---|
| In-process | a file-read or shell function inside the agent | The agent's own pod | Govern the blast radius, observe indirectly, never intercept |
| Provider-side | an LLM provider's built-in web search | Inside the LLM provider, during the API call | Observe and eventually price at the LLM gateway; it already sits on this wire |
| External server | an MCP server | A network service with its own credential | Broker, meter, and enforce: this chapter's subject |

The governing principle, stated once and used everywhere: **a tool is modeled by the resource that owns the wire it crosses.** [ModelProvider](../resources/modelprovider.md) governs everything that happens inside the LLM wire, including provider-side tools. ToolProvider (below) governs wires the gateway opens for tools. The pod's sandbox governs tools that never touch a wire. Every category has exactly one home, and no resource pretends to see traffic it does not carry.

### In-process tools

An in-process tool call never leaves the pod: the model's completion carries a `tool_use` block, the agent's own code runs a local function, and the result returns as a `tool_result` block in the next LLM request. Kaalm cannot intercept that function call and does not pretend to. What it already governs is the tool's blast radius, in the vocabulary this book already has: the image allowlist (rule 2) reviewed the code that is the tool, the synthesized NetworkPolicy bounds what a shell tool can reach, the RuntimeClass bounds what it can escape to, and resource limits bound what it can consume.

Two observability options are recorded for future milestones, neither in v0.4.0 scope. First, for any agent running a standard API tool loop, every in-process call is visible at the LLM gateway anyway, inside the `tool_use` and `tool_result` blocks that pass through the proxy; per-tool counts are derivable there without any agent cooperation. That is observability, not enforcement (an agent can be written around the pattern), and it carries per-provider body-format and label-cardinality costs, so it is designed but deferred. Second, a cooperative SDK hook through which the runtime self-reports tool calls; self-reported data is a different trust class from gateway-observed fact and would be labeled advisory telemetry, never audit. In-process metering is rejected outright: no marginal cost crosses a boundary Kaalm bills against, and the pod's own CPU and egress are already metered by Kubernetes-native means.

### Provider-side tools

A provider-side tool (an LLM provider's built-in web search, for example) executes inside the provider's infrastructure during the API round trip and is billed by the provider on top of tokens. It has no endpoint of its own, no credential of its own, no separate connection, and no health to probe: every operational field a standalone resource would carry is empty or a duplicate of the ModelProvider's. A dedicated CRD for it would be a policy flag wearing an infrastructure object's costume, so per the principle above it is a **ModelProvider facet**, sketched here and delivered incrementally:

```yaml
# ModelProvider.spec, future facet alongside models[]
serverTools:
  - id: web_search
    costPerCallUSD: "0.01"
```

Three consequences, in delivery order. In **v0.4.0**, the provider adapters (which already parse response bodies for token usage) additionally extract server-tool-use counts and emit them as `kaalm_llm_server_tool_use_total{provider, namespace, tool}`; this is the cheap half and lands with the broker's audit surface. Per-adapter honesty about what the wire actually carries: Anthropic responses report authoritative counts in `usage.server_tool_use` (for example `web_search_requests`, cumulative on streamed `message_delta` events); Vertex grounding reports the executed queries in `groundingMetadata.webSearchQueries`, counted as `google_search`; the OpenAI chat-completions format carries no usage-level server-tool counts on the paths the gateway proxies, so that adapter extracts none. **Named blind spot, priced later**: provider-side tool charges are real money the budget ledger cannot see today, the same class of hole as usage-less responses in [Hard Enforcement](llm/budgets-and-rate-limits.md#hard-enforcement)'s fine print; the `costPerCallUSD` slot closes it when it lands, and rule 33's argument (an unpriced call costs zero in the ledger, so a cap over it is a lie) extends to declared server tools as a natural corollary. **Enforcement lever, future**: the gateway parses and already mutates request bodies (`stream_options` injection), so stripping or rejecting an undeclared server tool is the same class of operation; it is named here as a lever, not promised. No new grant surface exists for any of this: a workload's access to provider-side tools is scoped by the provider grant chain it already has (`spec.providers`, class `allowedProviders`, provider `allowedNamespaces`), optionally narrowed by the facet.

## The ToolProvider Resource

External tool servers get a cluster-scoped CRD that deliberately rhymes with ModelProvider, because the entire tenancy and credential model transfers:

```yaml
apiVersion: kaalm.io/v1alpha1
kind: ToolProvider
metadata:
  name: search-tools
spec:
  # The protocol slot, mirroring ModelProvider.spec.type. v0.4.0 supports
  # exactly "mcp" (MCP streamable HTTP); the slot exists so a future
  # openapi type is an addition, not a reshape.
  type: mcp
  # https only, same schema pattern as ModelProvider.spec.endpoint.
  # In-cluster and external endpoints are equally valid, exactly as they
  # are for ModelProvider.
  endpoint: https://mcp-search.tools.svc:8443
  # Resolved ONLY from the operator namespace, never from a tenant
  # namespace: the same hardcoded invariant the LLM credential path has.
  # Agent pods never hold this value.
  credentialsRef:
    name: search-tools-key
    key: token
  # Glob patterns; the same gate ModelProvider.allowedNamespaces is.
  allowedNamespaces:
    - "team-*"
  # Optional catalog, mirroring models[]. When declared it becomes a
  # ceiling: the broker rejects calls to uncataloged tools, rule 38
  # validates grants against it, and the audit metric's tool label is
  # bounded by it. When omitted, the server's own tools/list governs.
  tools:
    - id: web_search
    - id: fetch_page
  healthCheck:
    enabled: true
    intervalSeconds: 60
```

Cluster-scoped because tool access is platform policy, exactly as model access is: the platform team registers the capability once, holds its credential in `kaalm-system`, and namespaces are admitted to it. The health probe speaks the protocol it governs: an MCP `initialize` followed by `tools/list`, surfacing `Healthy` the way ModelProvider's upstream probe does. The name ToolProvider (over ToolServer) is deliberate: the symmetry with ModelProvider is the design, and the resource provides tools the way a ModelProvider provides models.

Validation follows the established families, numbered additively and enforced when v0.4.0 lands (each rule is annotated accordingly in [Cross-Resource Validation](../resources/validation-and-defaulting.md#cross-resource-validation)):

- **Rule 35**: every workload's `spec.tools[].providerRef` must resolve to an existing ToolProvider (the rule 3 analog).
- **Rule 36**: every referenced ToolProvider must admit the workload's namespace via `allowedNamespaces` (the rule 4 analog).
- **Rule 37**: every referenced ToolProvider must appear in the class's `allowedToolProviders` (the rule 5 analog). Rules 35 to 37 join the class-mismatch handling family of rules 2 through 5: recoverable `Degraded`, never a stranded workload.
- **Rule 38**: when the ToolProvider declares a `tools` catalog, every tool name in a workload's grant must appear in it; a violation is a recoverable `Ready=False` condition naming the missing tools, following the rule 18 shape.

## Grants

Access is granted per server, narrowed per tool, mirroring the provider grant chain gate for gate:

```yaml
# AgentClass.spec
allowedToolProviders:
  - name: search-tools

# Agent.spec and AgentTask.spec, identical shape
tools:
  - providerRef:
      name: search-tools
    # Optional narrowing. Empty or omitted means every tool the server
    # offers (bounded by the declared catalog when one exists).
    tools: ["web_search"]
```

Grants live on both workload kinds. A goal-driven one-shot task is, if anything, the tool-hungriest workload shape, so an AgentTask declares `spec.tools` exactly as an Agent does and rules 35 to 38 gate it identically; the only difference is how a violation settles, terminal `Failed` rather than recoverable `Degraded`, because tasks have no Degraded phase (the same split rules 2, 5, and 24 already follow).

The broker enforces narrowing at both protocol surfaces. A `tools/call` naming an ungranted tool is rejected with `403 tool_denied`, the distinct status that makes per-tool policy auditable. A `tools/list` response is filtered to the granted set before it reaches the agent, so the model never sees a tool it cannot call; filtering the list is strictly stronger than rejecting the call, because it removes the temptation from the prompt itself. For gateway-only-tier callers, which carry no workload identity, the grant chain reduces to `allowedNamespaces` alone, exactly as it does for `spec.providers` today ([Workload Identity](llm/workload-identity.md)).

## The Broker

Brokered tool traffic terminates on the existing `:8443` listener as `POST /v1/mcp/{toolProviderName}`. No third listener: the [listener profile](overview.md#internal-endpoints-and-ports) already states the precedent (internal endpoints share `:8443` so authenticated callers reach them without a new listener), and the `:8080` rule (no mTLS paths on the Ingress-fronted listener) rules out the alternative. The path family joins the `:8443` auth table under the shared **dual-mode** caller-identity profile, the same one the LLM proxy paths use: Kaalm-managed pods authenticate by mTLS SAN (their SA tokens are deliberately rejected), gateway-only workloads by `TokenReview`-validated bearer token. The profile establishes only who is calling; authorization is per plane, and the broker enforces the grant chain itself.

**Transport.** v0.4.0 speaks MCP streamable HTTP: JSON-RPC over `POST`, with SSE response streams relayed by the same machinery the LLM proxy uses for streamed completions. The legacy HTTP+SSE dual-endpoint transport is out of scope (it is aging out of the protocol), stdio is out by construction (the gateway hosts no processes), and the server-initiated `GET` notification stream is deferred past v0.4.0: brokered calls are request-scoped in v0.4.0, and a tool server that needs to push must wait for a later milestone. Three wire postures follow from the same request-scoped stance. JSON-RPC batch arrays are rejected with `400 invalid_request` (per-element enforcement and response splitting buy nothing, and the newest protocol revision dropped batching). The broker carries an explicit **method allowlist**: `initialize`, `ping`, `notifications/*`, `tools/list`, and `tools/call`; any other method (`resources/*`, `prompts/*`, `sampling/*`) is rejected with `403 tool_denied` naming the method, because silently proxying surfaces the grant chain has no vocabulary for would be governance theater, and widening an allowlist later is additive while narrowing one is breaking. And a filtered `tools/list` answer is returned as plain JSON regardless of the upstream encoding: both encodings are legal for the caller, and rewriting inside an SSE stream buys nothing.

**Credential injection.** Identical to the LLM path: inbound auth material is stripped, the ToolProvider's credential is injected upstream per its `type`, and no credential-bearing byte crosses out of `kaalm-system`. The e2e proof obligation carries over from the LLM plane: the tool credential is absent from the agent pod, by inspection.

**Session ownership.** MCP sessions ride an `Mcp-Session-Id` header, and a broker that passes them through verbatim would let one workload resume another's session. With replicated gateways, an in-memory binding table breaks on cross-replica routing, so the binding is stateless: the broker never reveals the upstream session id. What the agent receives is a wrapped id binding the upstream id to the caller's identity under an HMAC keyed by gateway-shared material, `wrapped = base64(upstreamID) || hmac(key, upstreamID || callerIdentity)`. On every subsequent request, any replica recomputes the HMAC from the presented id and the authenticated caller identity; a mismatch is rejected before anything is forwarded. No shared state, no session table, no cross-replica coordination, and a session is unresumable by anyone but its owner by construction.

**Limits and SSRF posture.** Request and response bodies are size-capped (own knobs, LLM-proxy defaults as the starting point) and each call carries an upstream timeout. ToolProvider endpoints are platform-RBAC-authored, exactly as ModelProvider endpoints are, so the dynamic-URL paranoia of rule 22's callback policy does not transfer wholesale: the schema requires `https://`, and the broker never follows redirects, which closes the confused-deputy path a compromised tool server could otherwise open. The contrast is deliberate and worth stating: callback URLs are user-supplied data and get the full [callback policy](../resources/validation-and-defaulting.md) treatment; tool endpoints are operator-declared configuration and get the same trust as provider endpoints.

## Audit and Metering

Every brokered call emits a structured audit record: one `info`-level log line carrying timestamp, caller identity (name, kind, and namespace for mTLS callers; namespace for the bearer tier), ToolProvider, tool name, JSON-RPC method, outcome, duration, and request and response sizes. Bodies are never logged: tool-call content is exactly the category the [log-redaction rule](../operations/observability.md#pii-safety) names as sensitive. The bodylog debug facility extends to the MCP routes under its existing gate, for operators who accept that trade during an investigation.

Metrics: `kaalm_tool_calls_total{provider, namespace, tool, status}` and the `kaalm_tool_call_duration_seconds{provider, tool}` histogram. The `status` label is `ok` for a relayed 2xx, the wire error type (`access_denied`, `tool_denied`, `rate_limited`, and the rest of the [error vocabulary](api/errors.md)) for every failure the broker produces itself, and `upstream_error` for a relayed protocol-level non-2xx. The histogram observes forwarded calls only: local denials complete in microseconds, and folding them in would drag the percentiles toward zero exactly when a flood of denials coincides with real upstream latency. The `tool` label's cardinality is bounded by the **declared** catalog: on a provider with `spec.tools`, cataloged ids appear verbatim and anything else collapses to `uncataloged`; on a provider without one, every tool collapses, because wire-supplied names are unbounded and a compromised server could inflate the label set at will. The audit record always carries the real name. Declared catalogs are recommended for exactly this reason, per the [cardinality rules](../operations/observability.md#cardinality).

Metering is **rate limits and audit, not budgets**. Tool calls carry no token-price dimension, and rule 33's own argument cuts against pretending: an unpriced call costs zero in a ledger, and a cap over zeros is a lie. v0.4.0 ships per-(namespace, ToolProvider) rate limits configured by `ToolProvider.spec.rateLimits.requestsPerMinute` (a cluster-wide ceiling each replica divides, exactly as ModelProvider rate limits are enforced; there is no token dimension), reusing the token-bucket machinery the LLM plane already divides across replicas. USD budgets for tools are future work whose schema slot is already named (`costPerCallUSD`, above, for provider-side tools; a per-call cost on the ToolProvider catalog is the analog when someone needs it), not a v0.4.0 promise.

## Failure Modes

| Condition | Caller sees |
|---|---|
| Namespace or class gate fails (rules 36, 37) | `403 access_denied`, the same type the LLM tenancy chain uses |
| Granted-tool narrowing fails | `403 tool_denied`, distinct so per-tool policy is auditable |
| JSON-RPC method outside the broker allowlist | `403 tool_denied` naming the method |
| Tool server unreachable, 5xx, or redirecting | `503 tool_unavailable`, retryable; protocol-level 4xx from the server (an expired session's 404, for example) relays verbatim so MCP session semantics survive the broker |
| Tool call exceeds the upstream timeout | `504 tool_timeout` |
| Oversized request or response | The existing `request_too_large` / `response_too_large` types |
| Credential invalid at the server | `503 tool_unavailable` to the caller; a Warning event on the ToolProvider, mirroring the LLM path's credential events |
| Session ownership mismatch | `403 access_denied`; the audit record names the mismatch |

Error rows land in the [error schema](api/errors.md#llm-gateway-error-responses) marked "since v0.4.0", following the precedent that schema entries land at design time.

## Relationship to Direct Egress

The tool plane does not remove the escape hatch; it re-ranks it. In order of decreasing governance:

| Path | Credential | Metering and audit | Fits when |
|---|---|---|---|
| Brokered (`/v1/mcp/*`) | Held by the gateway, injected per call | Rate limits, per-call audit, metrics | The default for MCP tool servers from v0.4.0 |
| Direct egress (`allowedCIDRs` / `allowedHosts`) | Held by the agent pod | None beyond IP-level policy | Non-MCP protocols, or tools the platform team deliberately exempts |

One piece of pre-tool-plane API did not survive this ranking. `Agent.spec.mcpServers` shipped inert: nothing in the controller ever read it, and the egress scoping its comment promised was never implemented, so no behavior existed for anyone to depend on. Under the tool plane its premise is inverted anyway, since brokered tools need no per-agent egress at all. **The field was removed in v0.4.0.** The direct tier remains expressible through the class egress fields, which are the mechanism that actually works today and the honest name for what "direct" means: an IP-level hole, with the governance trade the table states.

## Versioning and Delivery

This chapter was merged as design in v0.3.0 and is implemented by the v0.4.0 milestone, tracked in its umbrella issue: the [ToolProvider resource](../resources/toolprovider.md), its reconciler, the grant fields on both workload kinds, reconcile-time enforcement of rules 35 to 38, the broker (`/v1/mcp/*` with call-time enforcement at both protocol surfaces, credential injection, stateless session ownership, and rate limits), the audit record and metric surface above, and the removal of `Agent.spec.mcpServers`. The S18 scenario is proven on a real cluster by the e2e suite; see the [scenario coverage map](../appendix/scenario-coverage.md).

## Acceptance Scenario

[S18](../appendix/scenarios.md#s18-grant-an-agent-a-governed-tool) exercises the plane end to end: a platform engineer registers a ToolProvider whose credential lives in `kaalm-system`, a class allows it, an agent lists and calls tools through the gateway with no tool credential in its pod, a namespace outside the allowlist is denied, and a call to an ungranted tool is rejected with the distinct status. The e2e spec proving it on a cluster is `Governed tool access (S18)`, recorded in the [scenario coverage map](../appendix/scenario-coverage.md).
