# Enabling Tracing

Tracing connects one user message to the LLM and tool calls it caused,
across the gateway, the agent, and the provider. It is off by default: with
no endpoint configured the gateway installs no tracer, creates no trace
context, and handles requests exactly as before. Two values turn it on.

## 1. Point the gateway at a collector

The gateway exports spans over OTLP/HTTP. Add the endpoint to your install or
upgrade command; the e2e suite's install (the Makefile's `e2e-deploy`
target) does this against its own Jaeger:

```bash
--set gateway.tracing.otlpEndpoint=http://jaeger.tracing-e2e.svc:4318
```

`gateway.tracing.sampleRatio` (default `1.0`) is parent-based head sampling
for the traces the gateway starts; a sampling decision that arrives with a
caller's trace context is honored either way. An `https` endpoint is
verified against the gateway's upstream trust pool, the same pool
`gateway.trustClusterCAForUpstream` and `gateway.upstreamCA` configure.

On a running install, `helm upgrade` with the new value rolls the gateway:
the endpoint is a container argument, read at startup.

## 2. No collector yet? Jaeger in one file

For a first look, a throwaway Jaeger with in-memory storage is one manifest,
`test/e2e/testdata/tracing-jaeger.yaml` (namespace `tracing-e2e`, OTLP on
4318, UI on 16686):

```bash
kubectl apply -f test/e2e/testdata/tracing-jaeger.yaml
kubectl rollout status deployment/jaeger -n tracing-e2e
```

Then set the endpoint from step 1 to `http://jaeger.tracing-e2e.svc:4318`,
and open the UI:

```bash
kubectl port-forward -n tracing-e2e svc/jaeger 16686:16686
```

at `http://localhost:16686`. It keeps nothing across a restart; it is not a
production collector.

## 3. Send a message and find its trace

Send one message to any agent through its channel
([Connecting a Channel](../developers/connecting-a-channel.md)), or from the
console's test-chat panel ([Using the Console](console.md)). In Jaeger, pick
the service `kaalm-gateway` and filter by the tag `kaalm.namespace` set to
the agent's namespace. The e2e spec asks the same question of the Jaeger API:

```
GET /api/traces?service=kaalm-gateway&tags={"kaalm.namespace":"tracing-e2e"}
```

Spans are exported in batches on a schedule, so give a fresh trace a few
seconds to land in full.

## 4. Read a trace

Every span is created by the gateway. One message yields:

| Span | Where | What it covers |
|---|---|---|
| `channel.receive` | User Gateway | Receipt of the webhook or test-chat message, through the sync reply or the async `202` |
| `agent.deliver` | User Gateway | The delivery to the agent, retries included |
| `llm.request` | LLM proxy | One LLM request from the agent; a budget or rate-limit denial closes it with an error status, so a blocked request is visible |
| `llm.forward` | LLM proxy | One provider attempt, named for the candidate it tried; a fallback walk shows one per candidate |
| `tool.call` | Tool broker | One governed MCP call; broker denials carry an error status |
| `tool.forward` | Tool broker | The upstream half of a forwarded call |

Spans carry the correlation the logs already carry, as attributes:
`kaalm.message_id`, `kaalm.namespace`, `kaalm.agent`, `kaalm.workload`,
`kaalm.provider`, `kaalm.model`, `kaalm.channel_type`, `kaalm.method`, and
`kaalm.tool`, each where it applies.

The gap between `agent.deliver` and its first child is the agent thinking.
The base images carry no OpenTelemetry SDK; they forward the delivery's
trace context on every gateway call, which is what keeps the LLM and tool
spans attached to the message, and that propagation is the whole of the
agent hop.

## 5. Agent spans of your own

A framework that runs its own OpenTelemetry SDK can fill that gap. The
context to continue from is the current message's `traceparent` and
`tracestate`: `kaalm.trace_context()` in the Python base image returns them
as a dict, and `agentruntime.TraceContext(ctx)` returns them to a Go
handler. Start your spans under that parent and export them to the same
collector, and they appear inside the trace.

A custom image that does not use either runtime must do one thing to stay
connected: copy the `traceparent` and `tracestate` headers from the delivery
request onto every call it makes to the gateway while handling that message.
That is runtime contract item 8, and it is a header copy, not an SDK
requirement.

## 6. Turn it off

Set `gateway.tracing.otlpEndpoint` back to the empty string and upgrade.
No tracer is installed, no context is created or forwarded, and request
handling behaves as it did before tracing existed.

---

*How this works: design book pages Operations, Observability (the Tracing
section: span inventory, attributes, exporter, and what the controller does
not trace), Runtime, Contract (item 8, trace-context propagation), Runtime,
Base Images (`kaalm.trace_context()`), and Appendix, Scenarios (S20, one
message across the hops).*
