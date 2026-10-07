# langgraph-chat: a conversational LangGraph agent on the base image

A LangGraph `StateGraph` running inside `handle_message` on the
`kaalm-agent-python` base image. The base image implements the runtime
contract (TLS, mTLS enforcement, dedup, heartbeats, and the persistent store
that survives hibernation); the graph only turns each envelope into a reply.

What it proves:

- **Framework model calls through the gateway.** `ChatOpenAI` points at the
  gateway's OpenAI-format path with a qualified `provider/model` name. Its
  httpx clients come from `kaalm.http_client()` / `kaalm.http_async_client()`,
  so the pod's mTLS identity survives certificate rotation with no handler
  code.
- **Graph state that survives hibernation.** The SQLite checkpointer writes
  to the agent's PVC, keyed by the envelope's `sessionId`. Hibernate the
  agent mid-conversation, wake it, and the thread continues.

## Build

Against a published base image:

```bash
docker build -t <your-registry>/langgraph-chat:0.1.0 .
```

On an unreleased tree, build the base image locally first and point BASE at
it:

```bash
make python-image PYTHON_AGENT_IMG=kaalm-agent-python:dev
docker build --build-arg BASE=kaalm-agent-python:dev -t langgraph-chat:dev examples/langgraph-chat
```

## Deploy

Push the image somewhere your AgentClass's `allowedImages` admits, adjust
`agent.yaml` (class, provider, model, image), then:

```bash
kubectl apply -f agent.yaml
```

Requirements: an OpenAI-format ModelProvider (`spec.type` `openai` or
`openai-compatible`) that allows your namespace, and
`spec.persistence.enabled: true` for the checkpointer. Send it messages
through an AgentChannel with `spec.session.enabled: true` and a `userId`
extractor ([Connecting a channel](../../guide/src/developers/connecting-a-channel.md)).
The handler keys each thread by the envelope's `sessionId`, which the channel
derives from the user ID. Without sessions, every message starts a new thread.
With an empty `userId`, all senders share one.

The guide walks through this example in Running framework agents
(`guide/src/developers/framework-agents.md`).
