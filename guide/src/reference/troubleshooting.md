# Troubleshooting

Every entry starts from what you see and names the command that tells you
which cause you have.

## Agent stuck in `Pending` or `Provisioning`

```bash
kubectl describe agent AGENT_NAME        # conditions carry the reason
```

- **Image pull Secret missing** (`Pending`): reason `ImagePullSecretMissing`;
  the Agent waits before any child is created.
- **Certificate not issued** (`Provisioning`): the Pod is gated on its
  serving certificate. `kubectl get certificates -n NAMESPACE` and check
  cert-manager logs.
- **PVC unbound** (`Provisioning`): `kubectl get pvc -n NAMESPACE`; usually a
  StorageClass problem.

An image the class does not allow is not a `Provisioning` symptom: the Agent
goes `Degraded` instead.

## Agent shows `Degraded`

`kubectl describe agent` gives the reason:

- **Image not allowed, provider or tool grant revoked, provider deleted**: the
  agent's *phase* goes to `Degraded`, reason `ClassConstraintViolation` (see
  S5 in the scenarios); the Pod keeps running and LLM calls return `403`
  until the class or the Agent changes back.
- **Budget exhausted**: the agent keeps its phase but carries a `Degraded`
  *condition*, reason `BudgetExhausted` (see S10). LLM calls return
  `429 budget_exhausted` and the provider status shows `state: Blocked` for
  the namespace. The condition clears when the budget frees up (period reset,
  ceiling increase, or spend drop).

## Agent never goes `Idle` or `Hibernated`

```bash
kubectl describe agent AGENT_NAME        # conditions carry the reason
```

- **Idle detection is off**: the `IdleDetection` condition is `False` with
  reason `Disabled`. Neither the Agent's `spec.lifecycle.idleTimeout` nor its
  class's `defaultIdleTimeout` sets a value, so the controller never checks
  for activity. A class `maxIdleTimeout` alone sets no value. Set
  `idleTimeout` on the Agent or `defaultIdleTimeout` on the class; the
  condition disappears once a nonzero timeout applies.
- **The gateway does not answer**: the `GatewayReachable` condition is `False`
  with reason `GatewayUnavailable`. The controller keeps the phase and makes
  no idle or hibernation transition without activity data. It checks again
  after a delay that grows from 30 seconds to 5 minutes, and at once when a
  gateway Pod turns Ready. Check the gateway with
  `kubectl get pods -n kaalm-system`.

Hibernation also needs `hibernationEnabled` on the Agent and
`hibernationAllowed` on the class; see [Hibernation, observed](../developers/lifecycle.md#hibernation-observed).

## Channel is `Ready=False` with `SecretNotOptedIn` or `CallbackHostNotApproved`

The `Ready` condition's message names the Secret:

```bash
kubectl get agentchannel CHANNEL_NAME -n NAMESPACE \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
```

A channel is not `Ready` until each Secret it references passes these checks.
The gateway routes only `Ready=True` channels, so an unlabeled Secret stops
traffic to the channel:

- **`SecretNotOptedIn`**: the message says `Secret "NAME" does not carry the
  label kaalm.io/channel-credential: "true"`. Confirm that the Secret was
  created for the channel, then label it. Only the exact value `true`
  counts; `True`, `yes`, and an empty value do not.

  ```bash
  kubectl label secret SECRET_NAME -n NAMESPACE kaalm.io/channel-credential=true
  ```

  A message that starts `callbackAuth:` names the callback Secret. The
  message never lists the Secret's keys.
- **`CallbackHostNotApproved`**: a Secret that holds a `bearer` callback token
  must list the host of the channel's `callbackUrl` in its
  `kaalm.io/callback-hosts` annotation. The annotation is a comma-separated
  list of bare hostnames. An entry must equal the hostname, ignoring case and
  surrounding spaces, with no wildcards and no suffixes, and the port in
  `callbackUrl` plays no part. A channel with an `hmac` callback needs no
  annotation.

  ```bash
  kubectl annotate secret SECRET_NAME -n NAMESPACE kaalm.io/callback-hosts=HOST --overwrite
  ```

  Replace `HOST` with the hostname, or with a comma-separated list that
  keeps the hosts already approved.

The channel re-checks every minute, so it turns `Ready=True` on the next pass
after you fix the Secret, or at once when you edit the channel. The gateway
does not wait for that pass: it refuses an unlabeled Secret on every read,
and before every callback attempt it checks the label and the host again.
When it refuses a callback, nothing is delivered, the response is still
available by polling, and the gateway records `CallbackInvalid` channel health
with a message that starts `callbackAuth secret unavailable:`. The channel's
`PlatformConnected` condition shows `CallbackInvalid` only when no delivery
succeeded within the health window. If one did, `PlatformConnected` stays
`True`, and the next pass reports `SecretNotOptedIn` or
`CallbackHostNotApproved` on `Ready`. Who may label a Secret depends on your
platform team: see [Managing team access](../platform/managing-access.md).

## Webhook returns `401`

![Flowchart of every check on POST /channels/{namespace}/{path} in the order the User Gateway runs them, as three rows. Intake: body within maxMessageBodyBytes, else 413 request_too_large; path registered to a Ready=True AgentChannel, else 401; channel not Terminating, else 401; bearer or HMAC auth passes, else 401. Envelope and target: body normalizes, else 400 invalid_request; referenced Agent exists, else 502 delivery_failed; then responseMode: sync wakes if needed, delivers, and answers inline. Async accept: pending records below maxPendingAsyncResponses, else 503 internal_unavailable; placeholder ConfigMap created, else 503 internal_unavailable; then 202 with requestId and channelPath.](../diagrams/user-webhook-intake.svg)

The intake answers `401` with one message for every refusal before
authentication succeeds, on purpose:

- The path is not registered: a channel's path starts with
  `/channels/{namespace}/`, and `/v1/` paths are the gateway API, not
  channels. `kubectl get agentchannels -n NAMESPACE` lists the paths.
- The channel is not `Ready`, or is `Terminating`: a `Degraded` channel still
  accepts traffic. A `Ready=False` channel with reason `SecretNotOptedIn`
  needs its Secret labeled; see
  [Channel is `Ready=False` with `SecretNotOptedIn` or `CallbackHostNotApproved`](#channel-is-readyfalse-with-secretnotoptedin-or-callbackhostnotapproved).
- Wrong or missing bearer token: compare with the channel's Secret.

A WhatsApp verification `GET` whose verify token does not match answers the
same `401`. A connection that never answers at all is
usually a NetworkPolicy between the caller and the gateway; on k3d or
kube-router a freshly created client Pod can be denied for around 20 seconds
after start while the CNI catches up, so retry before concluding the policy
is wrong.

## Sync webhook returns `504 sync_deadline_exceeded`

The agent was hibernated and a cold wake takes longer than the sync delivery
deadline (30s default versus a 120s wake budget). Switch the channel to
`responseMode: async`; this is the recommended mode for any channel backing
a hibernation-enabled agent.

## LLM call returns `403 access_denied`

One of the three gates denied; the error message names which:

- not in the workload's `spec.providers`,
- not in the AgentClass `allowedProviders`,
- namespace not in the provider's `allowedNamespaces`.

## LLM call returns `400`

- Model not qualified: the model field must be
  `{providerRef}/{modelId}`, for example `anthropic-shared/claude-opus-4-6`.
- Model not in the provider's catalog, or the provider name is unknown.

## LLM call returns `429`

Read the error type; the three cases behave differently:

- **`rate_limited`**: per (namespace, model) requests per minute or tokens
  per minute. `Retry-After` is the seconds until the limit admits you again, at
  least 1, and the limit clears then. That is usually seconds, but a request
  limit below the replica count can take longer: 1 request per minute on 3
  replicas gives `Retry-After: 180`. Wait that long and retry.
- **`budget_throttled`**: a hard-enforcement provider is serializing requests
  near its ceiling; `Retry-After: 1`. Retry on a short backoff.
- **`budget_exhausted`**: the namespace or cluster budget is spent;
  `Retry-After` is the seconds until the period resets. Retrying sooner is
  pointless; ask your platform team or wait.

## Task never completes

- `completion.condition: agentReported` but the image never calls
  `POST /v1/task/complete`: the task sits until its timeout. The timeout is
  `completion.timeout`, or the class's `defaultTaskTimeout` when that's unset.
  With neither set, the task sits until you delete it. Either
  report from the image, or set `completion.condition: exitCode`.
- The completion call is rejected: only the task's current Pod may report
  (an identity gate against stale retries); a completion sent by anything
  else is refused.

## Deleting a provider, tool provider, or class never finishes

`kubectl delete` on a ModelProvider, ToolProvider, or AgentClass hangs when a
finalizer holds it for an Agent, AgentTask, or AgentClass that still
references it:

```bash
kubectl describe modelprovider PROVIDER_NAME   # or toolprovider, agentclass
```

Look for `Ready=False, reason=DeletionBlocked`; the message names a
referrer, or the total count and the first when there is more than one. A
`Warning` event with the same reason and message fires when the hold first
appears, so `kubectl get events` finds it too. Remove or repoint the named
referrer, then retry the delete; the object releases on the next pass after
the last reference clears.

## ModelProvider `Ready=False`

`kubectl describe modelprovider PROVIDER_NAME`:

- `CredentialsMissing` or `CredentialsInvalid`: the Secret named by
  `credentialsRef` is absent in `kaalm-system` or lacks the key, or the
  provider rejected the key on the probe. ToolProvider reports both the
  same way.
- `InvalidDegradeTarget`: a budget policy's `degradeTo` is not in
  `spec.models`.
- `FallbackIneligible`: a fallback provider is missing, has a type the
  gateway cannot translate to, or the chain loops back on itself.
- `InvalidModelMap`: a `modelMap` on a fallback edge names a model that one
  end's catalog does not have.
- `HardBudgetUnpriced`: hard enforcement is on and a model in the catalog
  has no prices.

Each of these reasons also fires a `Warning` event with the same reason the
first time `Ready` turns `False` with it.

A `FallbackIneligible` `Warning` event with `Ready` still `True` is
different: the reconciler's eligibility scan found a fallback candidate that
one of your namespaces or models can never reach (an `allowedNamespaces` or
`spec.models` mismatch on the candidate). It is advisory and clears on its
own once the candidate's configuration is fixed.

`Healthy=False` with Ready=True is different too: the spec is fine but the
periodic upstream probe is failing; check the endpoint and the provider's
status page. If the endpoint serves a certificate from a private CA (an
in-cluster provider, for example) and `Healthy=False` comes from TLS
verification, the chart does not trust that CA. The controller's probe and
the gateway's forwarding path take their trust from the same chart values,
so one value fixes both. Set `gateway.trustClusterCAForUpstream=true` for a
`kaalm-ca-issuer` certificate, or `gateway.upstreamCA.configMap` for any
other CA
([Trust a private CA](../platform/llm-access.md#4-trust-a-private-ca)). The
same applies to a ToolProvider's `Healthy` column.

---

*How this works: design book pages Gateways, API, Error reference (the full error
catalog), Controller, Errors, events, and testing (error-handling philosophy),
Controller, Reconcilers (the reconcile-time fallback eligibility scan and the
probe backoff), Controller, Finalizers (the deletion-hold mechanics), and
Appendix, Scenarios (S5, S7, S10, S14 cover the revocation, hibernation, and
budget stories behind these symptoms).*
