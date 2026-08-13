# Running Framework Agents

Kaalm never dictates what code produces a reply. The runtime contract
governs how a container behaves; a framework agent (LangGraph, LangChain,
or anything else) is just the code between envelope in and reply out. If
you already have one, you do not have to ditch it. There are two
integration levels, and the first needs no conversion at all.

## Level 1: gateway only, zero conversion

An existing framework deployment stays exactly as deployed. Three changes,
all configuration:

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

   The gateway reads it as a bearer token from the `Authorization` header,
   which is exactly where an OpenAI-format client puts its API key: pass
   the token as the `api_key`. The Anthropic SDK sends its key in a header
   the gateway never reads, so use its `auth_token` parameter instead,
   which also lands in `Authorization`.
3. **Qualify the model name.** `openai-shared/gpt-5.2` instead of
   `gpt-5.2`: the gateway splits the prefix, checks the gates, injects the
   real credential, and forwards. Whatever key your client was configured
   with is stripped before forwarding, so a placeholder is safe.

One wire fact to respect: the gateway is protocol-aware but does not
translate. An OpenAI-format client must target a provider of `spec.type`
`openai` or `openai-compatible`, an Anthropic-format client an `anthropic`
provider.

What this buys: the API key leaves your pods, and the namespace's budgets,
rate limits, and fallback apply to every call. What it does not: no Agent
resource means no lifecycle, no hibernation, no channels, and no per-agent
identity; your namespace is the tenancy unit. That trade is the design
book's tiered on-ramp (Operations, Deployment), and moving up a tier is
the rest of this page.

## Level 2: full lifecycle, the framework inside the handler

On the base image, the framework is a `pip install` in a `FROM` build and
the graph runs inside `handle_message(envelope)`. The base image keeps
owning the contract plumbing; your handler builds its model clients from
two ABI members made for exactly this:

```python
model = ChatOpenAI(
    model=os.environ["KAALM_MODEL"],
    base_url=os.environ["KAALM_GATEWAY_ENDPOINT"] + "/v1",
    api_key="managed-by-kaalm",
    http_client=kaalm.http_client(),
    http_async_client=kaalm.http_async_client(),
)
```

`kaalm.http_client()` and `kaalm.http_async_client()` mint standard httpx
clients that carry the pod's mTLS identity and keep it current across
certificate rotation. Their names mirror the keyword arguments the
framework SDKs take; pass both, because a framework that touches the async
path with only a sync client supplied would silently run without your
identity. Write the handler as `async def` and use the framework's async
invocation: the runtime executes sync handlers on a thread without an
event loop.

Three worked examples live in the repo, each runnable and each proving a
different combination:

- **`examples/langgraph-chat/`**: a conversational graph with a SQLite
  checkpointer on the agent's PVC, keyed by the envelope's `sessionId`.
  Graph state survives hibernation, which the framework alone cannot
  offer. Needs `spec.persistence.enabled: true`; the volume is mounted at
  `/var/agent/memory` by default.
- **`examples/langgraph-tools/`**: a tool-calling agent whose MCP tools
  arrive through the gateway's broker at `/v1/mcp/<toolProvider>`, so the
  tool credential never exists in the pod and the tool list is already
  filtered to the grant. The MCP connection reuses
  `kaalm.http_async_client` through the adapter's client factory. The
  calling side of that story is
  [Calling Tools Through the Gateway](calling-tools.md).
- **`examples/langgraph-task/`**: a run-to-completion summarize, critique,
  refine graph as an AgentTask (next section).

## Task mode is the custom-image rung

An AgentTask's work is its whole program, not a resident message loop, so
the handler mount is deliberately not its extension point. The
`langgraph-task` example owns the slice of the contract a task needs: it
reads its goal from its own `spec.env` (Kaalm injects no goal variables),
runs the graph, and reports through `POST /v1/task/complete` with
`status: "success"` and the artifacts declared in `spec.artifacts`,
retrying only the retryable rejection (`StalePodCompletion`, on 100ms,
500ms, 2s) and treating `TaskAlreadyCompleted` as done. See
[Running Tasks](running-tasks.md) for the task lifecycle itself.

## When the platform pushes back

Your graph's model calls are governed calls, and mid-graph a gate can
close. What the framework sees, from softest to hardest:

- **Fallback and degrade are invisible to your code.** A failing provider
  is retried down its fallback chain server-side, and a budget `degrade`
  policy rewrites the model; the reply's `model` field names what actually
  answered.
- **Throttling and rate limits are a 429** with a `Retry-After` header
  and an error envelope whose `error.type` is `budget_throttled` or
  `rate_limited`. Framework SDKs surface this as their rate-limit error
  and most retry it automatically; make sure retries honor `Retry-After`.
- **A blocked or hard-capped budget is a 429** with `budget_exhausted`,
  and it will not clear until the period resets or the cap is raised.
  Retrying inside the graph wastes steps: fail the run and surface the
  error. Branch on `error.type` from the response body, never on the
  message text.

A graph that checkpoints (as the chat example does) resumes from its last
completed node, which turns a mid-graph budget stop from lost work into a
pause.

## The rotation footnote

Kaalm rotates workload certificates on disk mid-pod-lifetime. The `kaalm`
client factories exist so this never becomes your problem; a client built
by hand from `$KAALM_TLS_CERT` files snapshots its SSL context and an
agent that neither hibernates nor restarts for most of the certificate's
90-day duration would eventually present a stale certificate. If you must
build your own client (as the task example does), that is fine precisely
when the pod is short-lived; long-lived hand-built clients should be
rebuilt when TLS errors appear.

---

*How this works: design book pages Runtime, Base Images (the ABI and the
on-ramp rungs), Gateways, LLM, Request Handling (formats, adapters, and
what is never translated), Gateways, API, Errors (the envelope), and
Operations, Deployment (the tiered on-ramp).*
