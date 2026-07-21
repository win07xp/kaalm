# Providing LLM Access

A ModelProvider gives teams LLM access without ever handing them the API key.
The credential lives in a Secret in `agentry-system`; the gateway reads it
there and injects it server-side on every proxied call. Teams see model names
and budgets, never keys.

## 1. Create the credential Secret

In the operator namespace, not the team namespace. The pattern from
`test/e2e/testdata/secrets.yaml`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: anthropic-api-key
  namespace: agentry-system
type: Opaque
stringData:
  token: <your-api-key>
```

## 2. Create the ModelProvider

From `config/samples/agentry_v1alpha1_modelprovider.yaml`:

```yaml
apiVersion: agentry.io/v1alpha1
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
  allowedNamespaces: ["team-*"]
  budget:
    period: monthly
    perNamespaceUSD: "500"
    policies:
      - atPercent: 80
        action: warn
      - atPercent: 100
        action: degrade
        degradeTo: claude-opus-4-6
```

What each block buys you:

- **`models`** is the catalog. Agents request models as
  `anthropic-shared/claude-opus-4-6`; a model not in this list is rejected.
  The prices drive budget accounting, so keep them current.
- **`allowedNamespaces`** is the tenancy gate. Globs are supported
  (`team-*`); an empty list means no namespace may use the provider.
- **`budget`** caps spend per namespace per calendar period, with escalating
  policies as the budget is consumed. Details on policies, rate limits, and
  fallback chains are on [the next page](budgets-limits-fallback.md).
- **`endpoint`** must be `https://`; the schema rejects anything else because
  the gateway forwards the credential to this URL.

## 3. Verify

```bash
kubectl get modelproviders
```

The columns tell the story: `Ready` means the spec is valid and the credential
Secret resolves; `Healthy` reports the periodic upstream probe. A provider can
be Ready but Unhealthy (endpoint down); it recovers on its own when the probe
succeeds again.

The probe is configurable via `spec.healthCheck` (`enabled`, default true;
`intervalSeconds`, default 60; `timeoutSeconds`, default 10). Disabling it is
useful for offline fixtures or provider types with no probe.

---

*How this works: design book pages Resources, ModelProvider (every field and
the status shape), Security, Credentials (why keys live only in
agentry-system), and Gateways, LLM (how the proxy injects the credential).*
