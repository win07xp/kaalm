# Providing Tool Access

A ToolProvider gives teams access to an MCP tool server without ever handing
them its credential. The shape is the one you already know from
[Providing LLM Access](llm-access.md): the credential lives in a Secret in
`kaalm-system`, the gateway injects it server-side on every brokered call,
and teams see tool names, never keys. Agents reach the server only through
the gateway, so there is no per-team egress hole to punch or audit.

## 1. Create the credential Secret

In the operator namespace, not a team namespace. The pattern from
`test/e2e/testdata/s18-toolplane.yaml`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: search-tools-key
  namespace: kaalm-system
type: Opaque
stringData:
  token: <the tool server's API token>
```

If the server needs no authentication, skip this step and omit
`credentialsRef` below.

## 2. Create the ToolProvider

From `config/samples/kaalm_v1alpha1_toolprovider.yaml`:

```yaml
apiVersion: kaalm.io/v1alpha1
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
  healthCheck:
    enabled: true
    intervalSeconds: 60
```

What each block buys you:

- **`endpoint`** must be `https://`; the schema rejects anything else because
  the gateway forwards the credential to this URL. In-cluster and external
  endpoints are equally valid, and the broker never follows redirects.
- **`allowedNamespaces`** is the tenancy gate, read exactly as
  ModelProvider's: globs are supported, and an empty list means no namespace
  may use the provider.
- **`tools`** is the optional declared catalog, and it is a ceiling: grants
  are validated against it and the broker rejects calls to anything outside
  it. It also keeps the metrics readable, because tool names outside a
  declared catalog collapse to one `uncataloged` label value. Declare it
  whenever you know the server's tool set.
- **`healthCheck`** drives the `Healthy` column with a probe that speaks
  MCP itself: an `initialize` followed by `tools/list`. The probe trusts
  system CA roots by default. For a server with a private CA (a
  cluster-internal certificate, for example), give the controller the same
  trust you give the gateway: `controller.trustClusterCAForProbes=true`
  adds the cluster CA, and `controller.probeCA.configMap` names a ConfigMap
  with any other bundle, the probe-side mirror of
  `gateway.trustClusterCAForUpstream` and `gateway.upstreamCA`. Enable both
  sides, so the server is both forwarded to and probed `Healthy`.

## 3. Open the grant chain

Tool access stacks the same three gates as model access
([Managing Team Access](managing-access.md)): the class must allow the
provider, the provider must admit the namespace, and the workload must ask
for it. You own the first (the second is step 2 above); the team owns the
third. The pattern from `test/e2e/testdata/s18-toolplane.yaml`, on the
AgentClass:

```yaml
allowedToolProviders:
  - name: search-tools
```

As with `allowedProviders`, an empty list allows none. The team then lists
the provider in their Agent's or AgentTask's `spec.tools`, optionally
narrowed to named tools; that side is covered in
[Calling Tools Through the Gateway](../developers/calling-tools.md).

A grant that fails any gate is visible in status, not silently ignored: an
Agent goes `Degraded` with reason `ClassConstraintViolation` (provider not
in the class allowlist, missing, or namespace not admitted) or
`ToolNotInCatalog` (a granted tool is outside the declared catalog), with
the message naming the failed gate. An AgentTask denied at provisioning
fails terminally. Revocation behaves exactly as it does for models: the
broker denies the namespace's next call immediately with
`403 access_denied`, and the controller degrades the affected Agents within
a reconcile.

## 4. Rate limits

Tool calls carry no token or dollar dimension, so metering is rate limits
and audit, not budgets:

```yaml
rateLimits:
  requestsPerMinute: 300
```

The ceiling is per namespace and cluster-wide; each gateway replica divides
it by the live replica count, exactly as ModelProvider rate limits work. A
namespace over its ceiling gets `429 rate_limited` with a `Retry-After`
header. Zero or omitted means no limit.

## 5. Read the audit trail and metrics

Every brokered call, allowed or denied, emits one structured log record
from the gateway:

```bash
kubectl logs -n kaalm-system -l app.kubernetes.io/component=gateway --tail=-1 \
  | grep '"msg":"mcp call"'
```

Each record carries the calling workload and its kind, the namespace, the
provider, the JSON-RPC method, the real tool name (even one outside the
declared catalog), the HTTP status, the error type and a human-readable
`detail` on denials, the duration, and the request and response sizes. A
denied call is in the log but never reached the tool server; that is the
broker doing its job, and the e2e suite proves it by counting requests on a
mock server.

On the metrics side:

- `kaalm_tool_calls_total{provider, namespace, tool, status}` counts every
  brokered call. `status` is `ok`, an error type such as `tool_denied`, or
  `upstream_error` for a server-side failure the broker relayed.
- `kaalm_tool_call_duration_seconds{provider, tool}` observes forwarded
  calls only, so local denials cannot drag the percentiles down.

Tools that run inside an LLM provider (a model's built-in web search, for
example) never cross the broker; the LLM path counts those separately as
`kaalm_llm_server_tool_use_total`.

## 6. Verify

```bash
kubectl get toolproviders
```

The columns read as ModelProvider's do: `Ready` means the spec is valid and
the credential Secret resolves; `Healthy` reports the periodic probe. Ready
without Healthy means valid config, unreachable server, and it recovers on
its own when the probe succeeds again.

---

*How this works: design book pages Gateways, The Tool Plane (the broker,
the grant chain, and every enforcement point), Security, Credentials (why
the token lives only in kaalm-system), and Operations, Observability (the
metric catalog and its cardinality rules).*
