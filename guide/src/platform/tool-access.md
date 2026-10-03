# Providing tool access

A ToolProvider gives teams access to an MCP tool server without ever handing
them its credential. The pattern is the same as in
[Providing LLM access](llm-access.md): the credential lives in a Secret in
`kaalm-system`, the gateway injects it server-side on every brokered call,
and teams see tool names, never keys. Agents reach the server only through
the gateway, so there is no per-team egress exception to add or audit.

## 1. Create the credential Secret

In the operator namespace, not a team namespace. The Secret the sample
provider in `config/samples/kaalm_v1beta1_toolprovider.yaml` names:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: search-tools-key
  namespace: kaalm-system
  labels:
    kaalm.io/provider-credential: "true"
  annotations:
    kaalm.io/provider-hosts: mcp-search.tools.svc
type: Opaque
stringData:
  token: TOOL_SERVER_TOKEN
```

Replace `TOOL_SERVER_TOKEN` with the tool server's API token, and the
annotation value with the host of the server's endpoint.

The label opts the Secret in to provider use, and the annotation lists the
host the token may be sent to. The annotation is a comma-separated list of
bare hostnames, and it must include the host of the ToolProvider's
`endpoint`. A Secret that several providers share lists every one of their
hosts. Without the label the provider reports `Ready=False` with reason
`SecretNotOptedIn`, and without the host it reports `EndpointHostNotApproved`.
Both stop the gateway from using the token, and a brokered call returns
`503 tool_unavailable`. Set them on the Secret before you create the
provider, and set them again if a rotation re-creates the Secret. The fixes
are in [Troubleshooting](../reference/troubleshooting.md#modelprovider-readyfalse).

If the server needs no authentication, skip this step and omit
`credentialsRef` below.

## 2. Create the ToolProvider

From `config/samples/kaalm_v1beta1_toolprovider.yaml`:

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
  healthCheck:
    enabled: true
    intervalSeconds: 60
```

What each block does:

- **`endpoint`** must be `https://`; the schema rejects anything else because
  the gateway forwards the credential to this URL. The broker never follows
  redirects, so give the final URL.
- **`allowedNamespaces`** is the tenancy gate, read exactly as
  ModelProvider's: globs are supported, and an empty list means no namespace
  may use the provider.
- **`tools`** is the optional declared catalog, and it is a ceiling: grants
  are validated against it and the broker rejects calls to anything outside
  it. Declare it whenever you know the server's tool set: without it, every
  tool shows as `uncataloged` in the metrics.
- **`healthCheck`** drives the `Healthy` column with a probe that speaks
  MCP. The revision it negotiated lands in `status.mcpRevision`. For a
  server with a private CA, set `gateway.trustClusterCAForUpstream=true`
  for the cluster CA, or `gateway.upstreamCA.configMap` for any other
  bundle. One value covers both the broker and the probe
  ([Trust a private CA](llm-access.md#4-trust-a-private-ca)).

![Flowchart of every check on POST /v1/mcp/{toolProvider} in the order the broker runs them, as four rows. Route and namespace: ToolProvider exists, else 400 invalid_request; caller namespace in allowedNamespaces, else 403 access_denied. Workload grant, for mTLS callers only: ToolProvider in the workload's spec.tools providerRef, the AgentClass allowedNamespaces admits the caller's namespace, and ToolProvider in the AgentClass allowedToolProviders, else 403 access_denied. Request: token bucket per namespace and ToolProvider, else 429 rate_limited; body within the cap, else 413 request_too_large; one JSON-RPC message, else 400 invalid_request; method on the allowlist, else 403 tool_denied. Tool and session: modern headers match the body, else 400 with JSON-RPC error -32020; tools/call names a tool in the grant and catalog, else 403 tool_denied; a legacy session id is owned by this caller, else 403 access_denied; then inject the credential and forward.](../diagrams/tool-grant-chain.svg)

## 3. Open the grant chain

Tool access stacks the same three gates as model access
([Managing team access](managing-access.md)): the class must allow the
provider, the provider must admit the namespace, and the workload must ask
for it. You set the first (the second is step 2 above); the team sets the
third. The pattern from `test/e2e/testdata/s18-toolplane.yaml`, on the
AgentClass:

```yaml
allowedToolProviders:
  - name: search-tools
```

As with `allowedProviders`, an empty list allows none. The team then lists
the provider in their Agent's or AgentTask's `spec.tools`, optionally
narrowed to named tools; that side is covered in
[Calling tools through the gateway](../developers/calling-tools.md).

A grant that fails any gate is visible in status, not silently ignored: an
Agent goes `Degraded` with reason `ClassConstraintViolation` (provider not
in the class allowlist, missing, or namespace not admitted) or
`ToolNotInCatalog` (a granted tool is outside the declared catalog). An AgentTask denied at provisioning
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

The ceiling is per namespace and cluster-wide, as for ModelProvider rate
limits. A namespace over its ceiling gets `429 rate_limited` with a `Retry-After`
header. Zero or omitted means no limit.

## 5. Read the audit trail and metrics

Every brokered call, allowed or denied, emits one structured log record
from the gateway:

```bash
kubectl logs -n kaalm-system -l app.kubernetes.io/component=gateway --tail=-1 \
  | grep '"msg":"mcp call"'
```

Each record names the calling workload, the provider, the tool, the outcome,
and the duration; [The tool plane](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/tool-plane.md#audit-and-metering)
in the design book lists every field. A denied call is in the log but never
reached the tool server.

On the metrics side:

- `kaalm_tool_calls_total{provider, namespace, tool, status}` counts every
  brokered call. `status` is `ok`, an error type such as `tool_denied`, or
  `upstream_error` for a server-side failure the broker relayed.
- `kaalm_tool_call_duration_seconds{provider, tool}` observes forwarded
  calls only, so local denials cannot drag the percentiles down.

Tools that run inside an LLM provider (a model's built-in web search, for
example) never pass through the broker; the LLM path counts those separately as
`kaalm_llm_server_tool_use_total`.

## 6. Verify

```bash
kubectl get toolproviders
```

The columns read as ModelProvider's do: `Ready` means the spec is valid and
the credential Secret, when one is named, resolves and carries the label and
host annotation from step 1; `Healthy` reports the periodic probe. Ready
without Healthy means valid config, unreachable server, and it recovers on
its own when the probe succeeds again.

---

*How this works: design book pages Gateways, The tool plane (the broker,
the grant chain, and every enforcement point), Security, Credential handling (why
the token lives only in kaalm-system), Operations, Observability (the
metric catalog and its cardinality rules), and Resources, Validation and
defaulting (rules 49 and 50, the label and the host annotation).*
