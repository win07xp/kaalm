# Calling Tools Through the Gateway

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

The base URL is `$KAALM_GATEWAY_ENDPOINT/v1/mcp/<toolProvider>`, with the
same TLS posture as every other gateway call: present your client
certificate (`$KAALM_TLS_CERT` / `$KAALM_TLS_KEY`) and trust the cluster CA
(`$KAALM_CA_CERT`). If you use an MCP SDK, point its streamable HTTP
transport at that URL with those credentials; everything past the connection
is standard MCP.

On the wire it is JSON-RPC over POST. The walk, condensed from
`test/e2e/testdata/s18-caller.yaml` (the e2e caller that proves this path):

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

Three wire facts worth knowing:

- The session id is minted by the gateway, not the tool server. Treat it as
  opaque and echo it back on every request after `initialize`. Sessions are
  bound to your workload; presenting one minted for someone else is rejected
  with `403 access_denied`.
- `tools/list` is already filtered to your grant before you see it. If a
  tool you expected is missing, check the grant with your platform team
  before suspecting the server.
- Only `initialize`, `ping`, `notifications/*`, `tools/list`, and
  `tools/call` pass the broker. Any other method (`resources/*`,
  `prompts/*`, `sampling/*`) returns `403 tool_denied` naming the method,
  and JSON-RPC batch arrays are rejected with `400 invalid_request`.

## 3. What the errors mean at your call site

Gateway failures carry the standard error envelope; branch on `error.type`,
never on the message:

| Status | `error.type` | Retry? | What it means for you |
|---|---|---|---|
| 403 | `access_denied` | no | A tenancy gate: namespace, class, or session ownership. The fix is in the specs, not your code |
| 403 | `tool_denied` | no | The named tool is outside your grant, or the method is off the broker's allowlist |
| 429 | `rate_limited` | after `Retry-After` | Your namespace hit the provider's `requestsPerMinute` ceiling |
| 503 | `tool_unavailable` | yes | The tool server is unreachable, refusing, or rejected the injected credential; a platform problem, not yours |
| 504 | `tool_timeout` | no | The call exceeded the broker's upstream timeout |

A protocol-level 4xx from the tool server itself relays verbatim (an
expired upstream session's 404, for example), so normal MCP session
recovery, re-initialize and retry, works unchanged through the broker.

## What you never handle

The tool credential. It is not mounted in your pod, it is not in your spec,
and it never exists in your namespace. If the server starts rejecting it,
you see `tool_unavailable` and your platform team sees a Warning event on
the ToolProvider; there is nothing to rotate or fix on your side.

---

*How this works: design book pages Gateways, The Tool Plane (the broker end
to end), Gateways, API, Errors (the envelope and the full status table),
and Runtime, Runtime Contract (the TLS posture of every gateway call).*
