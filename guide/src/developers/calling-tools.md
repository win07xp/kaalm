# Calling tools through the gateway

If your platform team has registered a ToolProvider for you, your agent can
call MCP tools through the gateway. You never see or carry the tool server's
credential: the gateway checks your grant, injects the credential, and
forwards the call. This page assumes two names from your platform team: the
ToolProvider (here `search-tools`) and confirmation that your class allows
it.

## 1. Ask for the grant

In your Agent spec, the pattern from `test/e2e/testdata/s18-toolplane.yaml`:

```yaml
tools:
  - providerRef:
      name: search-tools
    tools: ["web_search"]
```

The inner `tools` list narrows the grant to named tools; omit it to accept
everything the provider serves. AgentTask has the same field.

If the grant does not take effect, `kubectl describe agent` names the
reason: `ClassConstraintViolation` (the class does not allow the provider,
the provider does not exist, or your namespace is not admitted) or
`ToolNotInCatalog` (you named a tool outside the provider's declared
catalog). An Agent degrades recoverably and comes back when the gate opens;
an AgentTask denied at provisioning fails terminally.

## 2. Point your MCP client at the broker

The base URL is `$KAALM_GATEWAY_ENDPOINT/v1/mcp/{toolProvider}`, with the
same TLS setup as every other gateway call: present your client
certificate (`$KAALM_TLS_CERT` / `$KAALM_TLS_KEY`) and trust the cluster CA
(`$KAALM_CA_CERT`). If you use an MCP SDK, point its streamable HTTP
transport at that URL with those credentials; everything past the connection
is standard MCP.

On the wire it is JSON-RPC over POST. The walk on the 2025-03-26 revision,
condensed from `test/e2e/testdata/s18-caller.yaml` (the e2e caller that
proves this path):

```bash
gw="$KAALM_GATEWAY_ENDPOINT/v1/mcp/search-tools"
post() { # $1 body, $2 optional session id
  curl -sS -D /tmp/headers \
    --cacert "$KAALM_CA_CERT" \
    --cert "$KAALM_TLS_CERT" --key "$KAALM_TLS_KEY" \
    -H "Content-Type: application/json" \
    ${2:+-H "Mcp-Session-Id: $2"} \
    -d "$1" "$gw"
}

post '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"my-agent","version":"1.0"}}}'
sess=$(grep -i '^Mcp-Session-Id:' /tmp/headers | cut -d' ' -f2 | tr -d '\r')
post '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$sess"
post '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' "$sess"
post '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{}}}' "$sess"
```

Three wire facts:

- The session id you receive is the broker's, bound to your workload; treat
  it as opaque and echo it back on every request after `initialize`.
  Presenting one issued to another workload is rejected with
  `403 access_denied`.
- `tools/list` is already filtered to your grant before you see it. If a
  tool you expected is missing, check the grant with your platform team
  before suspecting the server.
- Only `initialize`, `server/discover`, `ping`, `notifications/*`,
  `tools/list`, and `tools/call` pass the broker. Any other method
  (`resources/*`, `prompts/*`, `sampling/*`) returns `403 tool_denied`
  naming the method, and JSON-RPC batch arrays are rejected with
  `400 invalid_request`.

On the 2026-07-28 revision, which a client selects with its
`MCP-Protocol-Version` header, there is no session: the first call is
`server/discover`, every POST carries an `Mcp-Method` header naming its
method, `tools/call` also carries `Mcp-Name`, and a missing or mismatched
header is answered `400` with JSON-RPC error `-32020` in the body rather than
the gateway envelope. The design book's tool plane page states both
revisions.

![Sequence diagram of one brokered tool call. The agent container POSTs to /v1/mcp/{toolProvider} on the gateway broker at :8443 with a JSON-RPC body. The broker establishes the caller identity, runs the checks in order, reads the credential from a Secret in kaalm-system, strips inbound auth and injects the credential, and forwards to the tool server with no redirects and an upstream timeout. Three outcomes: unreachable, redirect, 401, 403, or 5xx is 503 tool_unavailable; a timeout is 504 tool_timeout; a response, JSON or an SSE stream, has tools/list filtered with cacheScope set to private and a legacy Mcp-Session-Id wrapped, then is relayed. The call ends with one audit line and the metrics.](../diagrams/tool-broker-flow.svg)

## 3. What the errors mean at your call site

Gateway failures carry the standard error envelope; branch on `error.type`,
never on the message:

| Status | `error.type` | Retry? | What it means for you |
|---|---|---|---|
| 403 | `access_denied` | no | A tenancy gate: namespace, class, or session ownership. The fix is in the specs, not your code |
| 403 | `tool_denied` | no | The named tool is outside your grant, or the method is off the broker's allowlist |
| 413 | `request_too_large`, `response_too_large` | no | The request or the server's response exceeded the broker's body cap |
| 429 | `rate_limited` | after `Retry-After` | Your namespace hit the provider's `requestsPerMinute` ceiling |
| 503 | `tool_unavailable` | yes | The tool server is unreachable, refusing, or rejected the injected credential; a platform problem, not yours |
| 504 | `tool_timeout` | no | The call exceeded the broker's upstream timeout |

A protocol-level 4xx from the tool server itself relays verbatim (an
expired upstream session's 404, for example), so normal MCP session
recovery, re-initialize and retry, works unchanged through the broker.

## What you never handle

The tool credential. It is not mounted in your pod, it is not in your spec,
and it never exists in your namespace. If the server starts rejecting it,
you see `tool_unavailable` and your platform team sees the provider's
`Healthy` condition drop on its next probe; there is nothing to rotate or fix
on your side.

---

*How this works: design book pages Gateways, The tool plane (the broker end
to end), Gateways, API, Error reference (the envelope and the full status table),
and Runtime, The runtime contract (the TLS setup of every gateway call).*
