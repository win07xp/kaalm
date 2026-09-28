# langgraph-tools: governed MCP tools in a LangGraph agent

A tool-calling LangGraph agent (`create_agent`) on the `kaalm-agent-python`
base image whose tools arrive through the Kaalm gateway's MCP broker rather
than from a tool server that the agent calls directly.

What it proves:

- **Tools without credentials.** The MCP connection targets
  `$KAALM_GATEWAY_ENDPOINT/v1/mcp/<toolProvider>`. The gateway authenticates
  the workload by its mTLS identity, enforces the grant chain, injects the
  tool server's credential upstream, and filters `tools/list` to what this
  agent was granted. Nothing in this pod ever holds the tool credential.
- **A framework MCP client behind the broker.** `langchain-mcp-adapters`
  takes a client factory; this example's factory builds its clients with
  `kaalm.http_async_client`, so the MCP sessions carry the pod's mTLS
  identity and reload the certificate after it rotates.

## Build

```bash
docker build -t <your-registry>/langgraph-tools:0.1.0 .
```

On an unreleased tree, build the base image locally first and pass
`--build-arg BASE=kaalm-agent-python:dev` (see the langgraph-chat README).

## Deploy

You need the names of three platform resources: an AgentClass allowing the
ToolProvider, an MCP ToolProvider admitting your namespace (guide: Providing
tool access), and an OpenAI-format ModelProvider. Adjust `agent.yaml` and:

```bash
kubectl apply -f agent.yaml
```

Ask it something its granted tool can answer; the gateway's audit log shows
every brokered call (`msg":"mcp call"` records), including denials for tools
outside the grant.

The guide walks through this example in Running framework agents
(`guide/src/developers/framework-agents.md`).
