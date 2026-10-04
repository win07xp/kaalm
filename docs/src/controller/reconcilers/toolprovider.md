# ToolProviderReconciler

## What it's for

The ToolProviderReconciler decides whether a [ToolProvider](../../resources/toolprovider.md) is `Ready`: its optional credential must pass rules 49 and 50, and the server must not reject it. It probes the server over MCP, reports the server's liveness on `Healthy`, and records the MCP revision it speaks in `status.mcpRevision`. On delete it [holds](../finalizers.md#cluster-scoped-resources) the ToolProvider while any Agent, AgentTask, or AgentClass references it.

## What it owns and watches

It owns nothing. It reads the credential Secret from `kaalm-system` only, so a same-named tenant Secret never satisfies the ref. [What each reconciler watches](../reconcilers.md#what-each-reconciler-watches) lists what re-runs it.

## What it checks

The first failing check sets `Ready=False` with its reason code and ends the pass, so the probe never sends a credential the Secret has not approved. No `credentialsRef` is valid: the Secret checks are skipped and the probe sends no credential.

| Check | Reason when it fails | Rule |
|---|---|---|
| The Secret exists | `CredentialsMissing` | |
| It carries the label `kaalm.io/provider-credential: "true"` | `SecretNotOptedIn` | [49](../../resources/validation/providers.md#provider-credentials) |
| Its `kaalm.io/provider-hosts` annotation lists the `spec.endpoint` host | `EndpointHostNotApproved` | [50](../../resources/validation/providers.md#provider-credentials) |
| It holds the key, non-empty | `CredentialsMissing` | |
| The probe gets no `401` or `403` | `CredentialsInvalid` | |

The probe runs unless `healthCheck.enabled` is `false`. It sends `Authorization: Bearer <credential>` to `spec.endpoint`, never follows a redirect, and speaks MCP in whichever revision the server does ([Protocol revisions](../../gateways/tool-plane.md#protocol-revisions)). Its outcomes and timeout are the [ModelProvider probe](modelprovider.md#liveness-probe)'s, and it uses the same [trust pool](modelprovider.md#probe-tls-trust).

Rules 35 to 38, the tool grants, are checked on the [Agent](agent.md) and [AgentTask](agenttask.md) reconcilers, not here, and the gateway's [broker](../../gateways/tool-plane.md#the-broker) enforces them at call time.

## What it reports

`Ready` is `True` with `CredentialsValid` when no check fails, also while `Healthy` is `False` for `ProviderUnhealthy`. Otherwise it carries the table's reason, or `DeletionBlocked` during a delete hold. A `Warning` event with the reason fires when a `Ready=False` reason first appears, and `ProviderUnhealthy` fires on every failing probe pass. A pass that ends at a Secret check, with the probe disabled, or in a delete hold sets `Healthy=Unknown` with `NotProbed`, for the [reason a ModelProvider does](modelprovider.md#what-it-reports). [Status](../../resources/toolprovider.md#status) lists the rest.

## Timing

- **A spec, Secret, or referrer change** re-runs the pass at once, without waiting out a probe backoff. A failed Secret check has no timed re-check.
- **A probed server**, including one that answers `CredentialsInvalid`, is re-probed every `healthCheck.intervalSeconds` (default 60), and a failing probe [backs off](modelprovider.md#probe-backoff), so a recovered server can take up to the backoff cap (10 minutes at the default interval) to show `Healthy`. With the probe disabled, nothing re-probes.

## Design choices

- **`credentialsRef` is optional**, because unauthenticated servers exist ([Credential scoping](../../resources/toolprovider.md#credential-scoping-and-why-the-ref-is-optional)).
- **The probe speaks MCP**, because an HTTP 200 proves nothing about a tool server ([The probe speaks MCP](../../resources/toolprovider.md#the-probe-speaks-mcp)).
