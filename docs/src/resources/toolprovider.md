# ToolProvider

ToolProvider is a cluster-scoped resource that defines a managed external tool server. It holds the server's endpoint, an optional credential reference, an optional tool catalog, a request rate limit, and the namespace tenancy gate. The tenancy and credential model is [ModelProvider](modelprovider.md)'s: the resource provides tools the way a ModelProvider provides models, and the gateway brokers calls to it the way it proxies LLM calls. The design and the broker mechanics are on [The tool plane](../gateways/tool-plane.md); this page is the resource reference.

Because it is cluster-scoped, a ToolProvider is a platform-team resource: the platform team registers the capability once, holds its credential in `kaalm-system`, and admits namespaces with `spec.allowedNamespaces`. Workloads reference it through the grant chain (`AgentClass.spec.allowedToolProviders` plus `spec.tools` on Agent and AgentTask), validated at reconcile time by rules 35 to 38 and enforced at call time by the broker.

## Spec

The annotated example shows every spec field.

```yaml
apiVersion: kaalm.io/v1beta1
kind: ToolProvider
metadata:
  name: search-tools
spec:
  # Required. "mcp" (MCP streamable HTTP) is the only accepted value; the
  # field exists so another protocol is an addition, not a reshaping.
  type: mcp

  # Required. The schema pattern ^https:// rejects any other scheme.
  # In-cluster and external endpoints are equally valid. With credentialsRef
  # set, its hostname must be listed in the credential Secret's
  # kaalm.io/provider-hosts annotation (rule 50).
  endpoint: https://mcp-search.tools.svc:8443

  # Optional, unlike ModelProvider's. A Secret key in the operator
  # namespace. Omit it for a server that requires no authentication;
  # the probe and the broker then send no Authorization header. When set,
  # the Secret must carry the label kaalm.io/provider-credential: "true"
  # (rule 49) and list the endpoint host in its kaalm.io/provider-hosts
  # annotation (rule 50).
  credentialsRef:
    name: search-tools-key
    key: token

  # Glob patterns, with ModelProvider's semantics. "*" matches every
  # namespace; an empty list admits none.
  allowedNamespaces:
    - "team-*"

  # Optional declared catalog, keyed by id (a duplicate is rejected at
  # apply time). When present it is a ceiling: the broker rejects calls to
  # uncataloged tools, grants are validated against it (rule 38), and the
  # audit metric's tool label is bounded by it. When omitted, the server's
  # own tools/list governs.
  tools:
    - id: web_search
    - id: fetch_page

  # Cluster-wide per-namespace ceiling on brokered calls; each gateway
  # replica enforces its share. Zero or omitted means no limit.
  rateLimits:
    requestsPerMinute: 120

  # The controller's periodic MCP probe. An omitted block means an enabled
  # probe with the defaults.
  healthCheck:
    enabled: true
    intervalSeconds: 60
    timeoutSeconds: 10
```

`kubectl get tp` prints the type and the `Ready` and `Healthy` conditions.

## Status

```yaml
status:
  observedGeneration: 2
  mcpRevision: "2026-07-28"
  conditions:
    - type: Ready
      status: "True"
      reason: CredentialsValid
      message: provider is valid
    - type: Healthy
      status: "True"
      reason: UpstreamReachable
```

| Field | Meaning |
|---|---|
| `mcpRevision` | The MCP protocol revision the probe last negotiated (`2026-07-28`, or `2025-03-26` for a server on the handshake era). Empty until the first successful probe; it keeps its last value after a failed probe and when the probe is disabled. |
| `Ready` | Whether the credential resolves. `True` with `reason: CredentialsValid`, also when there is no `credentialsRef`. `False` with `CredentialsMissing` when the Secret or key is absent or empty, `SecretNotOptedIn` when the Secret lacks the label `kaalm.io/provider-credential: "true"` (rule 49), `EndpointHostNotApproved` when its `kaalm.io/provider-hosts` annotation omits the `spec.endpoint` host (rule 50), `CredentialsInvalid` when the server answers the probe with a 401 or 403, or `DeletionBlocked` while a delete waits on a referrer ([Cluster-scoped resources](../controller/finalizers.md#cluster-scoped-resources)). `CredentialsMissing`, `SecretNotOptedIn`, `EndpointHostNotApproved`, and `CredentialsInvalid` each send a `Warning` event with the same reason when it first appears on `Ready`, not on every pass while it holds ([Event emission](../controller/operations.md#event-emission)). |
| `Healthy` | The periodic probe. `True` with `UpstreamReachable`; `False` with `ProviderUnhealthy` and a `Warning` event on every failing pass for a network or protocol failure, which does not affect `Ready`; `False` with `CredentialsInvalid` on a 401 or 403, which also sets `Ready=False`. The `CredentialsInvalid` event follows `Ready`, as the `Ready` row describes. |

`healthCheck.enabled: false` disables the probe; `intervalSeconds` (default 60) sets its cadence for a healthy provider and `timeoutSeconds` (default 10) bounds each probe sequence. A failing probe requeues on a backoff instead of the plain interval, the same one ModelProvider uses ([Probe backoff](../controller/reconcilers/modelprovider.md#probe-backoff)).

## Design notes

### Credential scoping, and why the ref is optional

Credentials are referenced from the operator's namespace and read only there, the same invariant as LLM credentials ([Credential handling](../security/credentials.md)). They never reach agent containers: a workload that wants to call a tool goes through the gateway, which attaches the credential server-side. Unlike on a ModelProvider, `credentialsRef` is optional. LLM providers require keys; tool servers do not always (an in-cluster MCP server behind NetworkPolicy is a legitimate unauthenticated deployment), and requiring a placeholder Secret would force a Secret that holds no real credential. A ToolProvider without `credentialsRef` reads no Secret, so rules 49 and 50 do not apply and `Ready` stays `True`; with one, they do ([Provider credentials](validation/providers.md#provider-credentials)).

### The probe speaks MCP

A generic HTTP 200 proves nothing about a tool server, so the probe runs the real protocol in whichever revision the server speaks, and `status.mcpRevision` records the answer; the sequence is on [Protocol revisions](../gateways/tool-plane.md#protocol-revisions). A 401 or 403 anywhere in it is a credential failure and flips `Ready`, as on ModelProvider.

### The catalog is a ceiling, not a mirror

`spec.tools` does not have to enumerate what the server offers; an empty catalog delegates to the server's `tools/list`. Declaring one does three things at once: the broker refuses calls to anything outside it, grants are validated against it at reconcile time (rule 38), and the `tool` metric label's cardinality is bounded by it, which is why declared catalogs are recommended ([Audit and metering](../gateways/tool-plane.md#audit-and-metering)).

### Rate limits carry no token dimension

`rateLimits` has one knob because tool calls have one countable unit, the call. There is no USD budget for tools: a cap over unpriced calls cannot be reached, the same reasoning as rule 33 ([Audit and metering](../gateways/tool-plane.md#audit-and-metering)).

### Deletion

A ToolProvider is held in deletion while any Agent, AgentTask, or AgentClass references it, as a ModelProvider is; the finalizer releases when the last grant or allowlist entry goes away. The hold is by reference, not by validity, so a workload whose grant violates a rule still pins its provider ([Finalizers](../controller/finalizers.md)).
