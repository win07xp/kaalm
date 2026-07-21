# Troubleshooting

Symptom-first. Every entry starts from what you see and names the command
that tells you which cause you have.

## Agent stuck in Provisioning

```bash
kubectl describe agent <name>        # conditions carry the reason
```

- **Certificate not issued**: the Pod is gated on its serving certificate.
  `kubectl get certificates -n <ns>` and check cert-manager logs.
- **Image not allowed**: reason `ClassConstraintViolation`; the image must
  match the class's `allowedImages` globs.
- **Image pull Secret missing**: reason `ImagePullSecretMissing`.
- **PVC unbound**: `kubectl get pvc -n <ns>`; usually a StorageClass problem.

## Agent shows Degraded

Degraded means running but missing something it needs. `kubectl describe
agent` gives the reason:

- **Provider access revoked or provider deleted**: reason
  `ClassConstraintViolation` (see S5 in the scenarios); LLM calls return 403
  until access is restored.
- **Budget exhausted**: the Degraded condition appears while phase stays
  Running; clears at the period reset or on a budget increase.

## Webhook returns 401 or 403

- Wrong or missing bearer token: compare with the channel's Secret.
- Channel not `Active`: `kubectl get agentchannels`.
- A NetworkPolicy between the caller and the gateway. On k3d or kube-router
  specifically: a freshly created client Pod can be denied for around 20
  seconds after start while the CNI catches up on allow rules; retry before
  concluding the policy is wrong.

## Webhook returns 404

The path must be exactly `/channels/{namespace}/{channel-name}` and the
channel must exist. Remember `/v1/` paths are the gateway API, not channels.

## Sync webhook returns 504 sync_deadline_exceeded

The agent was hibernated and a cold wake takes longer than the sync delivery
deadline (30s default versus a 120s wake budget). Switch the channel to
`responseMode: async`; this is the recommended mode for any channel backing
a hibernation-enabled agent.

## LLM call returns 403 access_denied

One of the three gates denied; the error message names which:

- not in the workload's `spec.providers`,
- not in the AgentClass `allowedProviders`,
- namespace not in the provider's `allowedNamespaces`.

## LLM call returns 400

- Model not qualified: the model field must be
  `{providerRef}/{modelId}`, for example `anthropic-shared/claude-opus-4-6`.
- Model not in the provider's catalog, or the provider name is unknown.

## LLM call returns 429

Read the error type; the two cases behave differently:

- **`rate_limited`**: per (namespace, model) requests or tokens per minute;
  clears in seconds. Back off and retry.
- **`budget_exhausted`**: the namespace or cluster budget is spent;
  `Retry-After` is the seconds until the period resets. Retrying sooner is
  pointless; ask your platform team or wait.

## Task never completes

- `completion.condition: agentReported` but the image never calls
  `POST /v1/task/complete`: the task will sit until `completion.timeout`.
  Either the image should report, or the task should use `exitCode`.
- The completion call is rejected: only the task's current Pod may report
  (an identity gate against stale retries); a completion sent by anything
  else is refused.

## ModelProvider Ready=False

`kubectl describe modelprovider <name>`:

- `CredentialsMissing` / `CredentialsInvalid`: the Secret named by
  `credentialsRef` is absent in `agentry-system` or lacks the key.
- `InvalidDegradeTarget`: a budget policy's `degradeTo` is not in
  `spec.models`.
- `FallbackIneligible`: a fallback provider is missing or has a different
  `spec.type`.

`Healthy=False` with Ready=True is different: the spec is fine but the
periodic upstream probe is failing; check the endpoint and the provider's
status page.

---

*How this works: design book pages Gateways, API, Errors (the full error
catalog), Controller, Operations (error-handling philosophy), and Appendix,
Scenarios (S5, S7, S10, S14 cover the revocation, hibernation, and budget
stories behind these symptoms).*
