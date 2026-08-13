# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""A tool-using LangGraph agent whose tools come through the Kaalm broker.

The agent's MCP tools are served by whatever ToolProvider the platform team
granted this workload: the MCP client connects to the gateway's broker path,
not to the tool server, so the tool credential never exists in this pod and
tools/list is already filtered to the grant. The connection authenticates
with the pod's mTLS identity via a client factory built on
kaalm.http_async_client, which also keeps it valid across certificate
rotation.

The model side is the same pattern as the langgraph-chat example: ChatOpenAI
through the gateway with the kaalm httpx clients. Everything is built lazily
on the first message so `import handler` stays safe at Docker build time.
"""

from __future__ import annotations

import asyncio
import os
from typing import Any

import httpx
from langchain.agents import create_agent
from langchain_core.messages import HumanMessage
from langchain_mcp_adapters.client import MultiServerMCPClient
from langchain_openai import ChatOpenAI

import kaalm

_agent = None
_agent_lock = asyncio.Lock()


def _mcp_client_factory(
    headers: dict[str, str] | None = None,
    timeout: httpx.Timeout | None = None,
    auth: httpx.Auth | None = None,
) -> httpx.AsyncClient:
    """The MCP adapter's client factory, carrying the pod's mTLS identity.

    The adapter opens and closes a client per session, so minting a fresh
    one per call is exactly right.
    """
    kwargs: dict[str, Any] = {
        k: v for k, v in {"headers": headers, "timeout": timeout, "auth": auth}.items() if v is not None
    }
    return kaalm.http_async_client(follow_redirects=True, **kwargs)


async def _ensure_agent():
    global _agent
    async with _agent_lock:
        if _agent is not None:
            return _agent

        gateway = os.environ["KAALM_GATEWAY_ENDPOINT"]
        provider = os.environ.get("KAALM_TOOL_PROVIDER", "search-tools")
        mcp = MultiServerMCPClient(
            {
                provider: {
                    "transport": "streamable_http",
                    # The broker path, not the tool server: the gateway
                    # authenticates the workload, enforces the grant, and
                    # injects the tool credential upstream.
                    "url": f"{gateway}/v1/mcp/{provider}",
                    "httpx_client_factory": _mcp_client_factory,
                }
            }
        )
        tools = await mcp.get_tools()

        model = ChatOpenAI(
            model=os.environ["KAALM_MODEL"],
            base_url=gateway + "/v1",
            api_key="managed-by-kaalm",
            http_client=kaalm.http_client(),
            http_async_client=kaalm.http_async_client(),
        )
        _agent = create_agent(model, tools)
        return _agent


async def handle_message(envelope: dict[str, Any]) -> dict[str, Any]:
    agent = await _ensure_agent()
    result = await agent.ainvoke(
        {"messages": [HumanMessage(str(envelope.get("content", "")))]}
    )
    return {
        "content": result["messages"][-1].content,
        "attachments": [],
        "metadata": {},
    }
