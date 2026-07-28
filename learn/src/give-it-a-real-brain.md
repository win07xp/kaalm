# Give It a Real Brain

Everything so far ran without an account anywhere, because the handler you
wrote counts instead of thinking. This chapter is about swapping that for a
real model.

It is the only chapter that costs money, and the only one this book cannot run
for you: the commands below need a provider account and an API key, so the
outputs are not captured from a walk the way every other chapter's are. Treat
it as the map from the tutorial to real work rather than a script to paste.

## The shape of the change

Three things have to line up:

1. A **ModelProvider**, which holds the credential and the price list.
2. The **class** must permit that provider, and the **agent** must reference it.
3. The **agent's code** must actually call a model, which your handler so far
   does not.

## 1. The provider

Put your key in a Secret, then describe the provider. Note where the key
lives: in a Secret in Kaalm's namespace, not in your agent's image, not in its
environment. Your code never sees it.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: anthropic-api-key
  namespace: kaalm-system
type: Opaque
stringData:
  token: sk-ant-your-key-here
---
apiVersion: kaalm.io/v1alpha1
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
  allowedNamespaces: ["default"]
```

The costs are not decoration. Kaalm multiplies them by the tokens your agents
actually spend, which is what makes budgets possible. `allowedNamespaces`
decides who may use this provider at all.

Check it came up:

```bash
kubectl get modelproviders
```

A `Ready` provider means Kaalm resolved the credential and reached the
endpoint. If it is not ready, `kubectl describe modelprovider anthropic-shared`
and read the conditions; a wrong or expired key says so there.

## 2. Point the agent at it

Two edits to `agent.yaml`. The class gains an allow-list:

```yaml
spec:
  allowedProviders:
    - name: anthropic-shared
```

and the agent claims it:

```yaml
spec:
  providers:
    - providerRef:
        name: anthropic-shared
```

Both are required, and that is deliberate: a platform team decides which
providers a class may use, and a developer picks from what the class permits.

## 3. Teach the code to think

Open `handler.py`, the file you wrote in [Make It Yours](make-it-yours.md).
To call a model, make the handler `async def` and use the pre-configured mTLS
client the runtime hands every handler: `await kaalm.gateway.post(...)`. You
address a model by its qualified name, `provider/model`, so here
`anthropic-shared/claude-opus-4-6`. The gateway recognizes your agent by its
certificate, checks it is allowed that provider, injects the API key, forwards
the call, and records what it cost. The wire details of the request live in
the guide's LLM chapter linked below.

That last sentence is the payoff for all the machinery in the earlier chapters:
your code sends a normal-looking model request with no credential in it, and
credential handling, permission checks, and accounting happen on the way past.

Then roll it out the way you shipped version one, as a new ConfigMap:

```bash
kubectl create configmap handler-v2 --from-file=handler.py
kubectl patch agent helper --type=merge \
  -p '{"spec":{"handler":{"configMapRef":{"name":"handler-v2"}}}}'
```

Kaalm notices the reference changed and replaces the pod. Talk to it through
the same channel as before, and this time the answer comes from a model. If
version two misbehaves, repointing back to `handler-v1` is the whole
rollback.

## Where the details live

This chapter is a signpost, not a manual. The full versions:

- [Providing LLM Access](https://github.com/win07xp/kaalm) in the User Guide,
  for providers, credentials, and the qualified model name.
- [Budgets, Limits, and Fallback](https://github.com/win07xp/kaalm), for
  capping spend and what happens at the cap.
- [Deploying from a Base Image](https://github.com/win07xp/kaalm), for
  everything the handler mount can do, and
  [Building Your Own Agent Image](https://github.com/win07xp/kaalm), for the
  day your handler outgrows a ConfigMap.

Next: [Where to Go Next](where-to-go-next.md).
