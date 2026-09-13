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
  ceiling increase, or spend drop), on the next pass in which the Agent is
  not in the `Degraded` phase for another reason.

## Webhook returns `401`

![Flowchart of every check on POST /channels/{namespace}/{path} in the order the User Gateway runs them, as three rows. Intake: body within maxMessageBodyBytes, else 413 request_too_large; path registered to a Ready=True AgentChannel, else 401; channel not Terminating, else 401; bearer or HMAC auth passes, else 401. Envelope and target: body normalizes, else 400 invalid_request; referenced Agent exists, else 502 delivery_failed; then responseMode: sync wakes if needed, delivers, and answers inline. Async accept: pending records below maxPendingAsyncResponses, else 503 internal_unavailable; placeholder ConfigMap created, else 503 internal_unavailable; then 202 with requestId and channelPath.](../diagrams/user-webhook-intake.svg)

The intake answers `401` with one message for every refusal before
authentication succeeds, on purpose:

- The path is not registered: a channel's path starts with
  `/channels/{namespace}/`, and `/v1/` paths are the gateway API, not
  channels. `kubectl get agentchannels -n NAMESPACE` lists the paths.
- The channel is not `Ready`, or is `Terminating`: a `Degraded` channel still
  accepts traffic.
- Wrong or missing bearer token: compare with the channel's Secret.

A `403` on a channel path comes only from a WhatsApp verification `GET` whose
verify token does not match. A connection that never answers at all is
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

- **`rate_limited`**: per (namespace, model) requests per minute;
  clears in seconds. Back off and retry.
- **`budget_throttled`**: a hard-enforcement provider is serializing requests
  near its ceiling; `Retry-After: 1`. Retry on a short backoff.
- **`budget_exhausted`**: the namespace or cluster budget is spent;
  `Retry-After` is the seconds until the period resets. Retrying sooner is
  pointless; ask your platform team or wait.

## Task never completes

- `completion.condition: agentReported` but the image never calls
  `POST /v1/task/complete`: the task sits until `completion.timeout`. Either
  report from the image, or set `completion.condition: exitCode`.
- The completion call is rejected: only the task's current Pod may report
  (an identity gate against stale retries); a completion sent by anything
  else is refused.

## ModelProvider `Ready=False`

`kubectl describe modelprovider PROVIDER_NAME`:

- `CredentialsMissing` or `CredentialsInvalid`: the Secret named by
  `credentialsRef` is absent in `kaalm-system` or lacks the key, or the
  provider rejected the key on the probe.
- `InvalidDegradeTarget`: a budget policy's `degradeTo` is not in
  `spec.models`.
- `FallbackIneligible`: a fallback provider is missing, has a type the
  gateway cannot translate to, or the chain loops back on itself.
- `InvalidModelMap`: a `modelMap` on a fallback edge names a model that one
  end's catalog does not have.

`Healthy=False` with Ready=True is different: the spec is fine but the
periodic upstream probe is failing; check the endpoint and the provider's
status page. If the endpoint serves a certificate from a private CA (an
in-cluster provider, for example) and the gateway forwards to it fine, the
probe is missing the trust the gateway has: set
`controller.trustClusterCAForProbes=true` or `controller.probeCA.configMap`
on the chart. The same applies to a ToolProvider's `Healthy` column.

---

*How this works: design book pages Gateways, API, Error reference (the full error
catalog), Controller, Errors, events, and testing (error-handling philosophy), and Appendix,
Scenarios (S5, S7, S10, S14 cover the revocation, hibernation, and budget
stories behind these symptoms).*
