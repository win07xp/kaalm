# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""A LangGraph conversational agent on the Kaalm Python base image.

The base image owns the runtime contract; LangGraph is the handler's brains.
The graph is a plain StateGraph over MessagesState whose model calls go
through the Kaalm gateway: ChatOpenAI is pointed at the gateway's OpenAI
format path with the qualified provider/model name, and both of its httpx
clients come from the kaalm factories, so the pod's mTLS identity follows
certificate rotation without any handler code.

Conversation memory is a LangGraph SQLite checkpointer on the agent's PVC,
keyed by the envelope's sessionId. Because the PVC survives hibernation, a
conversation resumes mid-thread after the agent wakes, a combination the
framework alone does not offer. The graph is built lazily on the first
message: the checkpointer needs a running event loop, and building it there
also keeps `import handler` working at Docker build time for the image's
self-check.
"""

from __future__ import annotations

import asyncio
import os
from typing import Any

import aiosqlite
from langchain_core.messages import HumanMessage
from langchain_openai import ChatOpenAI
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver
from langgraph.graph import END, START, MessagesState, StateGraph

import kaalm

_graph = None
_graph_lock = asyncio.Lock()


async def _ensure_graph():
    global _graph
    async with _graph_lock:
        if _graph is not None:
            return _graph

        # KAALM_MODEL must name a model on an OpenAI-format provider
        # (spec.type openai or openai-compatible): the gateway is
        # protocol-aware but does not translate between provider formats.
        model = ChatOpenAI(
            model=os.environ["KAALM_MODEL"],
            base_url=os.environ["KAALM_GATEWAY_ENDPOINT"] + "/v1",
            # The SDK requires a non-empty key; the gateway strips inbound
            # auth material and injects the real credential server-side.
            api_key="managed-by-kaalm",
            # Both clients, or the unsupplied path silently runs without
            # the pod's mTLS identity.
            http_client=kaalm.http_client(),
            http_async_client=kaalm.http_async_client(),
        )

        async def call_model(state: MessagesState):
            response = await model.ainvoke(state["messages"])
            return {"messages": [response]}

        # Graph state lives on the PVC, beside (never inside) the runtime's
        # own state file. Requires spec.persistence.enabled: true.
        state_dir = os.environ.get("KAALM_MEMORY_DIR", "/var/agent/memory")
        conn = await aiosqlite.connect(os.path.join(state_dir, "langgraph.sqlite"))

        builder = StateGraph(MessagesState)
        builder.add_node("model", call_model)
        builder.add_edge(START, "model")
        builder.add_edge("model", END)
        _graph = builder.compile(checkpointer=AsyncSqliteSaver(conn))
        return _graph


async def handle_message(envelope: dict[str, Any]) -> dict[str, Any]:
    graph = await _ensure_graph()
    # One checkpointer thread per channel session: the same webhook session
    # continues its conversation, across hibernation and wake.
    thread_id = envelope.get("sessionId") or envelope.get("messageId") or "default"
    result = await graph.ainvoke(
        {"messages": [HumanMessage(str(envelope.get("content", "")))]},
        {"configurable": {"thread_id": thread_id}},
    )
    return {
        "content": result["messages"][-1].content,
        "attachments": [],
        "metadata": {},
    }
