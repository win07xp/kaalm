# langgraph-tools: governed MCP tools in a LangGraph agent

A tool-calling LangGraph agent (`create_agent`) on the `kaalm-agent-python`
base image whose tools arrive through the Kaalm gateway's MCP broker rather
than from a directly-reached tool server.

What it proves:

- **Tools without credentials.** The MCP connection targets
  `$KAALM_GATEWAY_ENDPOINT/v1/mcp/<toolProvider>`. The gateway authenticates
  the workload by its mTLS identity, enforces the grant chain, injects the
  tool server's credential upstream, and filters `tools/list` to what this
  agent was granted. Nothing in this pod ever holds the tool credential.
- **A framework MCP client behind the broker.** `langchain-mcp-adapters`
  takes a client factory; ours wraps `kaalm.http_async_client`, so the MCP
  sessions carry the pod identity and follow certificate rotation.

## Build

```bash
docker build -t <your-registry>/langgraph-tools:0.1.0 .
```

On an unreleased tree, build the base image locally first and pass
`--build-arg BASE=kaalm-agent-python:dev` (see the langgraph-chat README).

## Deploy

You need three platform-side names: an AgentClass allowing the ToolProvider,
an MCP ToolProvider admitting your namespace (guide: Providing Tool Access),
and an OpenAI-format ModelProvider. Adjust `agent.yaml` and:

```bash
kubectl apply -f agent.yaml
```

Ask it something its granted tool can answer; the gateway's audit log shows
every brokered call (`msg":"mcp call"` records), including denials for tools
outside the grant.

The guide walks this example in Running Framework Agents
(`guide/src/developers/framework-agents.md`).
