# Running framework agents

Kaalm does not dictate what code produces a reply. The runtime contract
governs how a container behaves; a framework agent (LangGraph, LangChain,
or anything else) is the code between envelope in and reply out, so you can
keep one you already have. There are two integration levels, and the first
needs no conversion at all.

## Level 1: gateway only, zero conversion

An existing framework deployment keeps its framework and its graph. Three
changes: two are configuration, and the credential (step 2) needs a small
change in the client code.

1. **Repoint the model client.** Set its `base_url` to the gateway. For
   OpenAI-format clients that is
   `https://kaalm-gateway.kaalm-system.svc:8443/v1`; the Anthropic SDK
   takes the endpoint without the `/v1`. Trust the cluster CA from the
   `kaalm-ca` ConfigMap.
2. **Authenticate with a ServiceAccount token.** Project a token with
   audience `kaalm-gateway` into the pod (the pattern from
   `test/e2e/testdata/llm-caller.yaml`):

   ```yaml
   volumes:
     - name: token
       projected:
         sources:
           - serviceAccountToken:
               audience: kaalm-gateway
               expirationSeconds: 3600
               path: token
   ```

   The gateway reads the token as a bearer token from the `Authorization`
   header. The kubelet replaces the token file before the token expires
   (`expirationSeconds`), so the client must read the file on every
   request. A client that takes the token once, when it is built, keeps
   sending the first one and gets `401` on every call after it expires,
   until the Pod restarts. The file is the volume's `mountPath` plus
   `path`. With `mountPath: /var/run/token`, it is `/var/run/token/token`.
   With the `kaalm-ca` ConfigMap mounted at `/var/run/ca`, the CA is
   `/var/run/ca/ca.crt`.

   In Python, an `httpx` auth hook reads the file for each request and sets
   the header. Pass the client to the OpenAI or Anthropic SDK as
   `http_client`:

   ```python
   import httpx

   class GatewayToken(httpx.Auth):
       def auth_flow(self, request):
           with open("/var/run/token/token") as f:
               request.headers["Authorization"] = "Bearer " + f.read().strip()
           yield request

   http_client = httpx.Client(verify="/var/run/ca/ca.crt", auth=GatewayToken())
   ```

   The Anthropic SDK uses the `httpx2` package, not `httpx`. For it, import
   `httpx2` and use `httpx2.Auth` and `httpx2.Client` (or
   `httpx2.AsyncClient`) with the same class body. Passing an `httpx.Client`
   to it raises a `TypeError` when you construct the SDK client.

   Set any placeholder as the client's `api_key`, for both SDKs: they fail
   without a credential, and the hook sets the header the gateway reads.
   Step 3 says the gateway strips the client's own key before forwarding.

   For an async client, use `httpx.AsyncClient` with the same class, for
   `AsyncOpenAI` and `AsyncAnthropic` (with `httpx2.AsyncClient` for the
   Anthropic SDK). A framework client that takes both `http_client` and
   `http_async_client`, such as LangChain's `ChatOpenAI`, needs the hook on
   both, for the reason given under
   [Level 2](#level-2-full-lifecycle-the-framework-inside-the-handler).
   In another language, build the client so it reads the file for each
   request's credential, or rebuild the client when the file changes.
3. **Qualify the model name.** `openai-shared/gpt-5.2` instead of
   `gpt-5.2`: the gateway reads the provider from the prefix and injects the
   real credential. Whatever key your client was configured with is
   stripped before forwarding, so a placeholder is safe.

One wire fact to respect: the primary provider must speak your client's
format. An OpenAI-format client targets a provider of `spec.type` `openai`
or `openai-compatible`, an Anthropic-format client an `anthropic` provider.
The gateway translates only on a cross-format fallback, where it rewrites
the answer back into your client's format.

What you get: the API key leaves your pods, and the namespace's budgets,
rate limits, and fallback apply to every call. What it does not: no Agent
resource means no lifecycle, no hibernation, no channels, and no per-agent
identity; your namespace is the tenancy unit. That trade is the design
book's tiered on-ramp (Operations, Deployment). Level 2 moves up a tier.

## Level 2: full lifecycle, the framework inside the handler

On the base image, the framework is a `pip install` in a `FROM` build and
the graph runs inside `handle_message(envelope)`. The base image keeps
providing the contract code; your handler builds its model clients from
two ABI members made for exactly this:

```python
model = ChatOpenAI(
    model=os.environ["KAALM_MODEL"],   # set in the Agent's spec.env; Kaalm injects no model name
    base_url=os.environ["KAALM_GATEWAY_ENDPOINT"] + "/v1",
    api_key="managed-by-kaalm",
    http_client=kaalm.http_client(),
    http_async_client=kaalm.http_async_client(),
)
```

`kaalm.http_client()` and `kaalm.http_async_client()` mint standard httpx
clients that carry the pod's mTLS identity and keep it current across
certificate rotation. Pass both: a framework that touches the async path with
only a sync client supplied would silently run without your identity. Write the handler as `async def` and use the framework's async
invocation: the runtime executes sync handlers on a thread without an
event loop.

Three worked examples live in the repository, each runnable and each proving a
different combination:

- **`examples/langgraph-chat/`**: a conversational graph with a SQLite
  checkpointer on the agent's PVC, keyed by the envelope's `sessionId`.
  Graph state survives hibernation, which the framework alone cannot
  offer. Needs `spec.persistence.enabled: true`; the example reads the
  volume path from `$KAALM_MEMORY_DIR`, so a custom `mountPath` works too.
  The channel needs `spec.session.enabled: true` and a `userId` extractor
  for a conversation to continue ([Connecting a channel](connecting-a-channel.md)).
- **`examples/langgraph-tools/`**: a tool-calling agent whose MCP tools
  arrive through the gateway's broker, and whose MCP connection reuses
  `kaalm.http_async_client` through the adapter's client factory. The
  calling side is
  [Calling tools through the gateway](calling-tools.md).
- **`examples/langgraph-task/`**: a run-to-completion summarize, critique,
  refine graph as an AgentTask (next section).

## Task mode: a built image, either way

An AgentTask's work is its whole program, not a resident message loop, so
the handler mount is deliberately not its extension point. A Python task can
be a `FROM`-built base image whose `handler.py` defines `async def
run_task()`; the runtime runs it and reports the outcome. See
[Running tasks](running-tasks.md) for that path and the task lifecycle.

The `langgraph-task` example is the custom-image alternative. It implements
the slice of the contract a task needs: it reads its goal from its own
`spec.env` (Kaalm injects no goal variables), runs the graph, and reports
through `POST /v1/task/complete` with `status: "success"` and the artifacts
declared in `spec.artifacts`, retrying `409 stale_pod` and
`503 internal_unavailable` and treating `TaskAlreadyCompleted` as done. That is item 6 of the checklist in
[Building your own agent image](building-your-own-image.md).

## Errors your graph will see

Your graph's model calls are governed calls, and the gateway can slow or
stop one mid-graph. What the framework sees, from softest to hardest:

- **Fallback and degrade are invisible to your code.** A failing provider
  is retried down its fallback chain server-side, and a budget `degrade`
  policy rewrites the model; the reply's `model` field names the model that
  answered.
- **Throttling and rate limits are a 429** with a `Retry-After` header
  and an error envelope whose `error.type` is `budget_throttled` or
  `rate_limited`. Framework SDKs surface this as their rate-limit error
  and most retry it automatically; make sure retries honor `Retry-After`.
- **A blocked or hard-capped budget is a 429** with `budget_exhausted`,
  and it will not clear until the period resets or the cap is raised.
  Retrying inside the graph wastes steps: fail the run and surface the
  error; the envelope marks it `retryable: false`. Branch on `error.type`
  from the response body, never on the message text.

A graph that checkpoints (as the chat example does) resumes from its last
completed node, which turns a mid-graph budget stop from lost work into a
pause.

## The rotation footnote

Kaalm rotates workload certificates on disk while the pod runs. The `kaalm`
client factories handle this. A client you build by hand from the
`$KAALM_TLS_CERT` files snapshots its SSL context, so a long-lived agent keeps
presenting the old certificate after rotation (certificates last 90 days by
default). A hand-built client is fine in a short-lived pod, as in the task
example; in a long-lived one, rebuild the client when TLS errors appear.

---

*How this works: design book pages Runtime, Reference base images (the ABI and the
on-ramp rungs), Gateways, LLM, Request handling (formats, adapters, and
what is never translated), Gateways, API, Error reference (the envelope), and
Operations, Deployment (the tiered on-ramp).*
