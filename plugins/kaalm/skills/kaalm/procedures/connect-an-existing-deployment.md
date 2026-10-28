# Connect an existing deployment to the gateway

Use this when a workload stays a plain Deployment (or StatefulSet or Job) and only sends its model calls through Kaalm's gateway. This is the gateway-only tier, Level 1 in [Running framework agents](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/framework-agents.md#level-1-gateway-only-zero-conversion). The workload keeps its image and its code. The model key leaves its Pods, and the namespace's budgets, rate limits, and fallback apply to every call. The tier is defined in [Tiered on-ramp](https://github.com/win07xp/kaalm/blob/main/docs/src/operations/deployment.md#tiered-on-ramp), and the caller's side in [Workload identity](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/workload-identity.md#caller-side-setup).

Tell the user up front what it costs and what it does not give:

- Reading the token on every request (step 5) is a code change. It needs an image rebuild and a redeploy of the workload.
- The model key that the workload used sat in its Pods, so step 7 removes it and rotates it.

- No Agent resource, so no lifecycle, no hibernation, no channels, and no per-agent identity. The namespace is the unit of tenancy.
- No NetworkPolicy for the workload's Pods. Budgets and access checks hold only for traffic that goes through the gateway, so a direct call to the model provider is not blocked unless the platform team writes a policy that denies it. See [Adoption tiers](https://github.com/win07xp/kaalm/blob/main/docs/src/concepts/tenancy-and-tiers.md#adoption-tiers).

## Steps

1. **Add the ModelProvider and key** with [add-a-model-key.md](add-a-model-key.md). Use a new key if the user can create one, because step 7 revokes the old key. The provider's `allowedNamespaces` must admit the workload's namespace. Its `spec.type` must match the client's format: `openai` or `openai-compatible` for an OpenAI-format client, `anthropic` for an Anthropic-format client.
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

   The `audience` must be `kaalm-gateway`; the default audience is rejected with `401`. Editing the Pod template rolls the workload's Pods: say so, and apply the edit with [change-safely.md](change-safely.md). Leave the old model key (its environment variable or mount) in the Pod template through steps 3 to 6, so a failed check can be rolled back to the old setup. Step 7 removes it.
4. **Point the client at the gateway.**
   - URL: `https://kaalm-gateway.KAALM_NAMESPACE.svc:8443` (`KAALM_NAMESPACE` is `install.namespace` in the config). An OpenAI-format client uses `base_url` with `/v1` added. The Anthropic SDK takes the URL without `/v1`.
   - Trust: the client verifies the gateway with `/var/run/ca/ca.crt`.
   - Model: the qualified name `PROVIDER_NAME/MODEL_ID`, such as `openai-shared/gpt-5.2`. The gateway reads the provider from the prefix and injects the real key.
   - Credential: the projected token, sent as `Authorization: Bearer`.
5. **Read the token file on every request.** The projected token expires after `expirationSeconds` (3600), and the kubelet replaces the file before then. A client that reads the file once at startup and passes it as a fixed `api_key` gets `401` after about an hour. For a Python client built on `httpx`, an auth hook rereads the file on each request and sets the header (the same hook as in the Level 1 section of [Running framework agents](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/framework-agents.md#level-1-gateway-only-zero-conversion)). Set any placeholder as the `api_key`:

   ```python
   import httpx

   class GatewayToken(httpx.Auth):
       def auth_flow(self, request):
           with open("/var/run/token/token") as f:
               request.headers["Authorization"] = "Bearer " + f.read().strip()
           yield request

   http_client = httpx.Client(verify="/var/run/ca/ca.crt", auth=GatewayToken())
   ```

   Pass `http_client` to the framework's model client (the OpenAI and Anthropic SDKs accept one), and use `httpx.AsyncClient` with the same class for an async client. A framework client that takes both `http_client` and `http_async_client`, such as LangChain's `ChatOpenAI`, needs the hook on both. Anthropic SDK 2.0 and later uses the `httpx2` package, not `httpx`. For it, import `httpx2` and use `httpx2.Auth` and `httpx2.Client` (or `httpx2.AsyncClient`) with the same class body. Passing an `httpx.Client` to it raises a `TypeError` when you construct the SDK client. Earlier versions take the `httpx` client shown. For another language, build the client so its credential function reads the file each time.
6. **Check it.** Send one request from the workload. A request from a new Pod can get `401` until the gateway has seen the Pod, so retry for a few seconds. Then read spend, which the provider's status syncs periodically:

   ```bash
   kubectl --context CONTEXT get modelprovider PROVIDER_NAME -o jsonpath='{.status.budgetUsage}'
   ```

   A `403` with `access_denied` means a provider check failed: [LLM call returns 403 access_denied](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#llm-call-returns-403-access_denied). A `401` has the causes listed under [Mode 2](https://github.com/win07xp/kaalm/blob/main/docs/src/gateways/llm/workload-identity.md#mode-2-serviceaccount-bearer-token): a token with the wrong audience, a token the apiserver rejects, or a Pod that Kaalm manages. Use [diagnose.md](diagnose.md) for the provider's own status.

7. **Remove the old key and rotate it.** The workload's own copy of the provider key sat in its Pods, in an environment variable or a Secret, so anyone who could read those had it. Start only after step 6's check succeeds, because until then the old key lets a failed check roll back to the old setup. Do these in order, each with the user's OK:
   1. Edit the workload to drop the key: remove the environment variable or mount from the Pod template, through [change-safely.md](change-safely.md).
   2. Delete the Secret that held it, if nothing else uses it: `kubectl --context CONTEXT delete secret SECRET_NAME -n NAMESPACE`, with `--dry-run=server` first. Do not read the Secret first.
   3. The user revokes the old key at the model provider's own console, so the copy that sat in the Pods stops working.
8. **Block direct calls to the provider.** Budgets and access checks hold only for traffic that goes through the gateway, and Kaalm writes no NetworkPolicy for this workload. [networkpolicy-gateway-only.yaml](../examples/networkpolicy-gateway-only.yaml) lets the workload's Pods reach the gateway (port 8443) and cluster DNS, and nothing else. Set the namespace, the `app` label that selects the Pods, and the Kaalm namespace. The policy also blocks every other destination the workload uses, such as a database, so add rules for those with the user. Apply it with [change-safely.md](change-safely.md).

   A NetworkPolicy has an effect only on a cluster whose network layer enforces them, and some managed clusters need that turned on: [Network policies](https://kubernetes.io/docs/concepts/services-networking/network-policies/) explains the object, and each provider documents its own setting: [Amazon EKS](https://docs.aws.amazon.com/eks/latest/userguide/cni-network-policy-configure.html), [Google GKE](https://cloud.google.com/kubernetes-engine/docs/how-to/network-policy), and [Azure AKS](https://learn.microsoft.com/azure/aks/use-network-policies). k3d enforces them.

To move the workload up a tier, with a lifecycle, a NetworkPolicy, and channels, see Level 2 in [choose-an-image.md](choose-an-image.md).
