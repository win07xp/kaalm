# langgraph-chat: a conversational LangGraph agent on the base image

A LangGraph `StateGraph` running inside `handle_message` on the
`kaalm-agent-python` base image. The base image keeps owning the runtime
contract (TLS, mTLS enforcement, dedup, heartbeats, hibernation wiring); the
graph is only the brains between envelope in and reply out.

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
through an AgentChannel; the same webhook session id continues the same
conversation thread.

The guide walks this example in Running Framework Agents
(`guide/src/developers/framework-agents.md`).
