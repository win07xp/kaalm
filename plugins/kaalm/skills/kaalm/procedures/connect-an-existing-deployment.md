# Connect an existing deployment to the gateway

Use this when a workload stays a plain Deployment (or StatefulSet or Job) and only sends its model calls through Kaalm's gateway. This is the gateway-only tier, Level 1 in [Running framework agents](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/framework-agents.md#level-1-gateway-only-zero-conversion). The workload keeps its image and its code. The model key leaves its Pods, and the namespace's budgets, rate limits, and fallback apply to every call. The tier is defined in [Tiered on-ramp](https://github.com/win07xp/kaalm/blob/main/docs/src/operations/deployment.md#tiered-on-ramp), and the caller's side in [Workload identity](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/workload-identity.md#caller-side-setup).

Tell the user what this tier does not give:

- No Agent resource, so no lifecycle, no hibernation, no channels, and no per-agent identity. The namespace is the unit of tenancy.
- No NetworkPolicy for the workload's Pods. Budgets and access checks hold only for traffic that goes through the gateway, so a direct call to the model provider is not blocked unless the platform team writes a policy that denies it. See [Adoption tiers](https://github.com/win07xp/kaalm/blob/main/docs/src/concepts/tenancy-and-tiers.md#adoption-tiers).

## Steps

1. **Add the ModelProvider and key** with [add-a-model-key.md](add-a-model-key.md). The provider's `allowedNamespaces` must admit the workload's namespace. Its `spec.type` must match the client's format: `openai` or `openai-compatible` for an OpenAI-format client, `anthropic` for an Anthropic-format client.
2. **Check that the CA reaches the namespace.** The client must trust the gateway's certificate, which the `kaalm-ca` ConfigMap holds:

   ```bash
   kubectl --context CONTEXT get configmap kaalm-ca -n NAMESPACE
   ```

   trust-manager projects it into every namespace unless the chart value `trustManager.bundleSelector` narrows it ([values.yaml](https://github.com/win07xp/kaalm/blob/main/charts/kaalm/values.yaml)). If the ConfigMap is missing, the selector excludes the namespace. Widening it is a Helm upgrade by the platform team: use [change-safely.md](change-safely.md), including its rule for when the user may not make the change.
3. **Add the token and the CA to the workload's Pod template.** Kaalm does not change these Pods, so the owner does. The pattern is from [llm-caller.yaml](https://github.com/win07xp/kaalm/blob/main/test/e2e/testdata/llm-caller.yaml):

   ```yaml
   spec:
     containers:
       - name: app
         volumeMounts:
           - name: gateway-token
             mountPath: /var/run/token
             readOnly: true
           - name: gateway-ca
             mountPath: /var/run/ca
             readOnly: true
     volumes:
       - name: gateway-token
         projected:
           sources:
             - serviceAccountToken:
                 audience: kaalm-gateway
                 expirationSeconds: 3600
                 path: token
       - name: gateway-ca
         configMap:
           name: kaalm-ca
           items:
             - key: ca.crt
               path: ca.crt
   ```

   The `audience` must be `kaalm-gateway`; the default audience is rejected with `401`. Editing the Pod template rolls the workload's Pods: say so, and apply the edit with [change-safely.md](change-safely.md).
4. **Point the client at the gateway.**
   - URL: `https://kaalm-gateway.KAALM_NAMESPACE.svc:8443` (`KAALM_NAMESPACE` is `install.namespace` in the config). An OpenAI-format client uses `base_url` with `/v1` added. The Anthropic SDK takes the URL without `/v1`.
   - Trust: the client verifies the gateway with `/var/run/ca/ca.crt`.
   - Model: the qualified name `PROVIDER_NAME/MODEL_ID`, such as `openai-shared/gpt-5.2`. The gateway reads the provider from the prefix and injects the real key.
   - Credential: the projected token, sent as `Authorization: Bearer`.
5. **Read the token file on every request.** The projected token expires after `expirationSeconds` (3600), and the kubelet replaces the file before then. A client that reads the file once at startup and passes it as a fixed `api_key` gets `401` after about an hour. [Running framework agents](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/framework-agents.md#level-1-gateway-only-zero-conversion) says to pass the token as the `api_key`; use the hook below instead, with any placeholder as the `api_key`. For a Python client built on `httpx`, an auth hook rereads the file on each request and sets the header:

   ```python
   import httpx

   class GatewayToken(httpx.Auth):
       def auth_flow(self, request):
           with open("/var/run/token/token") as f:
               request.headers["Authorization"] = "Bearer " + f.read().strip()
           yield request

   http_client = httpx.Client(verify="/var/run/ca/ca.crt", auth=GatewayToken())
   ```

   Pass `http_client` to the framework's model client (the OpenAI and Anthropic SDKs accept one), and use `httpx.AsyncClient` with the same class for an async client. For another language, build the client so its credential function reads the file each time.
6. **Check it.** Send one request from the workload. A request from a new Pod can get `401` until the gateway has seen the Pod, so retry for a few seconds. Then read spend, which the provider's status syncs periodically:

   ```bash
   kubectl --context CONTEXT get modelprovider PROVIDER_NAME -o jsonpath='{.status.budgetUsage}'
   ```

   A `403` with `access_denied` means a provider check failed: [LLM call returns 403 access_denied](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#llm-call-returns-403-access_denied). A `401` has the causes listed under [Mode 2](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/workload-identity.md#mode-2-serviceaccount-bearer-token): a token with the wrong audience, a token the apiserver rejects, or a Pod that Kaalm manages. Use [diagnose.md](diagnose.md) for the provider's own status.

To move the workload up a tier, with a lifecycle, a NetworkPolicy, and channels, see Level 2 in [choose-an-image.md](choose-an-image.md).
