# ToolProvider

ToolProvider is a cluster-scoped resource that defines a managed external tool server, since v0.4.0. It holds the server's endpoint, an optional credential reference, an optional tool catalog, and the namespace tenancy gate. It deliberately rhymes with [ModelProvider](modelprovider.md), field for field, because the entire tenancy and credential model transfers: the resource provides tools the way a ModelProvider provides models, and the gateway brokers calls to it the way it proxies LLM calls. The design, the taxonomy behind it, and the broker mechanics live in [The Tool Plane](../gateways/tool-plane.md); this page is the resource reference.

Because it is cluster-scoped, a ToolProvider is a platform-team resource: the platform team registers the capability once, holds its credential in `kaalm-system`, and admits namespaces to it via `spec.allowedNamespaces`. Workloads reference it through the grant chain (`AgentClass.spec.allowedToolProviders` plus `spec.tools` on Agent and AgentTask), validated at reconcile time by rules 35 to 38; the broker that enforces the grants at call time lands with the rest of the v0.4.0 tool plane.

## Spec

The annotated example below shows every spec field.

```yaml
apiVersion: kaalm.io/v1alpha1
kind: ToolProvider
metadata:
  name: search-tools
spec:
  # Tool protocol. v0.4.0 supports exactly "mcp" (MCP streamable HTTP);
  # the slot exists so a future protocol is an addition, not a reshape.
  type: mcp

  # The tool server URL. Must use https:// (CRD schema pattern, the same
  # rule ModelProvider.spec.endpoint carries); in-cluster and external
  # endpoints are equally valid.
  endpoint: https://mcp-search.tools.svc:8443

  # Credentials: a reference to a Secret in the operator's namespace.
  # Resolved only from kaalm-system, never from a tenant namespace, and
  # never mounted into agent pods; the gateway injects it per brokered
  # call. OPTIONAL, unlike ModelProvider's: a server that requires no
  # authentication simply omits the block, and the probe and broker then
  # send no Authorization header.
  credentialsRef:
    name: search-tools-key
    key: token

  # Which namespaces may reference this provider.
  # "*" matches all namespaces. Empty list = no namespaces (provider is
  # inert). Glob semantics are ModelProvider's, documented once there
  # (see Glob semantics in allowedNamespaces).
  allowedNamespaces:
    - "team-*"

  # Optional declared catalog, keyed by id (a map-typed list, so the
  # apiserver rejects duplicate ids at apply time). When present it is a
  # ceiling: the broker rejects calls to uncataloged tools, grants are
  # validated against it (rule 38), and the audit metric's tool label is
  # bounded by it. When omitted, the server's own tools/list governs.
  tools:
    - id: web_search
    - id: fetch_page

  # Health check configuration. The probe speaks the protocol it governs:
  # an MCP initialize handshake followed by tools/list.
  healthCheck:
    enabled: true
    intervalSeconds: 60
    timeoutSeconds: 10
```

## Status

```yaml
status:
  observedGeneration: 2
  conditions:
    - type: Ready
      status: "True"
      reason: CredentialsValid
      message: provider is valid
    - type: Healthy
      status: "True"
      reason: UpstreamReachable
```

Two conditions summarize provider health, exactly as on ModelProvider. `Ready` reports whether the credential resolves: `False` with reason `CredentialsMissing` when the referenced Secret or key is absent or empty, and `False` with reason `CredentialsInvalid` when the server rejects the credential with a 401 or 403. A ToolProvider with no `credentialsRef` is `Ready` with a message noting that no credential is configured. `Healthy` reports the periodic MCP probe: `UpstreamReachable` on success, `ProviderUnhealthy` (with a Warning event) on network errors or protocol failures, which do not flip `Ready`.

The probe runs by default; set `healthCheck.enabled: false` to disable it (for example for an offline test fixture). `healthCheck.intervalSeconds` sets the probe cadence (default 60) and `healthCheck.timeoutSeconds` bounds each probe sequence (default 10).

## Design Notes

### Credential scoping, and why the ref is optional

Credentials are referenced from the operator's namespace and read only there, the invariant the LLM credential path hardcodes ([Credential Handling](../security/credentials.md)). They never reach agent containers: a workload that wants to call a tool goes through the gateway, which attaches the credential server-side. The one deliberate divergence from ModelProvider is that `credentialsRef` is optional. LLM providers universally require keys; tool servers do not (an in-cluster MCP server behind NetworkPolicy is a legitimate unauthenticated deployment), and requiring a dummy Secret would manufacture a credential where none exists.

### The probe speaks MCP

A generic HTTP 200 proves nothing about a tool server, so the health probe runs the real protocol: an `initialize` handshake (including the `notifications/initialized` notification and any `Mcp-Session-Id` the server issues) followed by `tools/list`. A server is `Healthy` only if both round trips succeed. A 401 or 403 anywhere in the sequence is classified as a credential failure and flips `Ready`, mirroring the ModelProvider probe's contract.

### The catalog is a ceiling, not a mirror

`spec.tools` does not have to enumerate what the server offers; leaving it empty delegates to the server's own `tools/list`. Declaring it buys three things at once: the broker refuses calls to anything outside it, grants are validated against it at reconcile time (rule 38), and the `tool` metric label's cardinality is bounded by it, which is why declared catalogs are recommended ([Audit and Metering](../gateways/tool-plane.md#audit-and-metering)).

### Deletion

A ToolProvider is held in Terminating while any Agent, AgentTask, or AgentClass references it, exactly as ModelProvider is: the finalizer releases when the last grant or allowlist entry goes away. The hold is by reference, not by validity, so even a workload whose grant is currently violating a rule keeps its provider pinned.
