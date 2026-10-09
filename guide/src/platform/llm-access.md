# Providing LLM access

A ModelProvider gives teams LLM access without ever handing them the API key.
The credential lives in a Secret in `kaalm-system`; the gateway reads it
there and injects it server-side on every proxied call.

## 1. Create the credential Secret

In the operator namespace, not the team namespace. The Secret the sample
provider in `config/samples/kaalm_v1beta1_modelprovider.yaml` names:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: anthropic-api-key
  namespace: kaalm-system
  labels:
    kaalm.io/provider-credential: "true"
  annotations:
    kaalm.io/provider-hosts: api.anthropic.com
type: Opaque
stringData:
  token: API_KEY
```

Replace `API_KEY` with the provider's API key.

The label opts the Secret in to provider use, and the annotation lists the
host the key may be sent to. The annotation is a comma-separated list of
bare hostnames, and it must include the host of the provider's `endpoint`. A
Secret that several providers share lists every one of their hosts. Without
the label the provider reports `Ready=False` with reason `SecretNotOptedIn`,
and without the host it reports `EndpointHostNotApproved`. Both stop the
gateway from using the key. Set them on the Secret before you create the
provider, and set them again if a rotation re-creates the Secret. The fixes
are in [Troubleshooting](../reference/troubleshooting.md#modelprovider-readyfalse).

## 2. Create the ModelProvider

From `config/samples/kaalm_v1beta1_modelprovider.yaml`:

```yaml
apiVersion: kaalm.io/v1beta1
kind: ModelProvider
metadata:
  name: anthropic-shared
spec:
  type: anthropic
  endpoint: https://api.anthropic.com
  credentialsRef:
    name: anthropic-api-key
    key: token
  models:
    - id: claude-opus-4-6
      costPer1MInputTokens: "15.00"
      costPer1MOutputTokens: "75.00"
    - id: claude-sonnet-4-6
      costPer1MInputTokens: "3.00"
      costPer1MOutputTokens: "15.00"
  allowedNamespaces: ["team-*"]
  budget:
    period: monthly
    perNamespaceUSD: "500"
    policies:
      - atPercent: 80
        action: warn
      - atPercent: 100
        action: degrade
        degradeTo: claude-sonnet-4-6
```

What each block does:

- **`models`** is the catalog. Agents request models as
  `anthropic-shared/claude-opus-4-6`; a model not in this list is rejected.
  The prices drive budget accounting, so keep them current.
- **`allowedNamespaces`** is the namespace check. Globs are supported
  (`team-*`); an empty list means no namespace may use the provider.
- **`budget`** caps spend per namespace per calendar period, with escalating
  policies as the budget is consumed. Details on policies, rate limits, and
  fallback chains are on [Budgets, limits, and fallback](budgets-limits-fallback.md).
  At 100 percent the sample degrades to the cheaper `claude-sonnet-4-6`.
- **`endpoint`** must be `https://`; the schema rejects anything else because
  the gateway forwards the credential to this URL.

## 3. Verify

```bash
kubectl get modelproviders
```

The two columns to read: `Ready` means the spec is valid and the credential
resolves, including the label and the host annotation from step 1; `Healthy`
reports the periodic upstream probe. `Healthy` shows `Unknown` while a Secret
or spec problem keeps `Ready` `False`, and with the probe disabled, because the
probe did not run
([Status cheatsheet](../reference/status-cheatsheet.md#modelprovider)). A
provider can be Ready but Unhealthy (endpoint down); it recovers on its own
when the probe succeeds again. A key the provider rejects is different: the
probe reports it as `Ready=False` with reason `CredentialsInvalid` and a
`Warning` event, and the provider stays that
way until the Secret holds a key the provider accepts.

The probe is configurable through `spec.healthCheck` (`enabled`, default
true; `intervalSeconds`, default 60; `timeoutSeconds`, default 10). A probe
that keeps failing runs less often, up to every 10 minutes at the default
interval, until it succeeds. A new key in the Secret or a change to the spec re-probes at
once.

## 4. Trust a private CA

Both the gateway (which forwards requests) and the controller (which probes)
trust the system CA roots by default. A provider served with a certificate
from your own CA, an in-cluster model server for example, needs a trust value
on the chart. One value covers both components, because the controller's probe
trusts what the gateway's forwarding path trusts:

| Endpoint certificate issued by | Chart value |
|---|---|
| The Kaalm cluster CA (`kaalm-ca-issuer`) | `gateway.trustClusterCAForUpstream=true` |
| Any other CA | `gateway.upstreamCA.configMap=CONFIGMAP_NAME` |

For the second row, create a ConfigMap named `CONFIGMAP_NAME` in
`kaalm-system` whose `ca.crt` key holds the PEM bundle (the key name is
`gateway.upstreamCA.key`, default `ca.crt`). Both components re-read the
bundle when it rotates, with no restart. The two rows compose: enabling both
merges the cluster CA and your bundle into one trust pool.

The older `controller.trustClusterCAForProbes` and `controller.probeCA` values
are deprecated but still honored. They add CAs to the controller's probe pool
only, so a CA set there makes a provider `Healthy` without making forwarding
trust it. Move to the gateway values above; the chart's install notes warn
while either is set.

The same choice exists for async webhook callbacks to receivers under a
private CA: `gateway.trustClusterCAForCallbacks=true` for the cluster CA.

---

*How this works: design book pages Resources, ModelProvider (every field and
the status fields), Resources, Validation and defaulting (rules 49 and 50,
the label and the host annotation), Security, Credential handling (why keys live only in
kaalm-system), Gateways, LLM Gateway (how the proxy injects the credential),
and Controller, Reconcilers (the probe backoff and the google-vertex probe).*
