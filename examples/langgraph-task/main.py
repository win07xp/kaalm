# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""A run-to-completion LangGraph worker as a Kaalm AgentTask.

An AgentTask's work is its whole program: no message loop, no heartbeat, no
probes. This one reads its goal from its own spec.env, runs a three-step
summarize / critique / refine graph with every model call brokered by the
gateway, and reports the result through POST /v1/task/complete with the
contract's bounded retry.

Unlike the Agent examples, this is the custom-image rung: no base image, no
kaalm module. The mTLS identity is built once from the mounted certificate
files, which is fine here precisely because a task lives minutes; the
rotation concern the kaalm client factories solve belongs to agents that run
for months.
"""

from __future__ import annotations

import asyncio
import os
import ssl
import sys
from typing import TypedDict

import httpx
from langchain_core.messages import HumanMessage
from langchain_openai import ChatOpenAI
from langgraph.graph import END, START, StateGraph

COMPLETE_RETRY_DELAYS = (0.0, 0.1, 0.5, 2.0)


class State(TypedDict, total=False):
    text: str
    summary: str
    critique: str


def client_ssl_context() -> ssl.SSLContext:
    ctx = ssl.create_default_context(
        cafile=os.environ.get("KAALM_CA_CERT", "/var/run/kaalm/ca.crt")
    )
    ctx.load_cert_chain(
        os.environ.get("KAALM_TLS_CERT", "/var/run/kaalm/tls.crt"),
        os.environ.get("KAALM_TLS_KEY", "/var/run/kaalm/tls.key"),
    )
    return ctx


async def complete_task(
    client: httpx.AsyncClient, gateway: str, status: str, message: str, artifacts: dict[str, str]
) -> None:
    """POST /v1/task/complete per the runtime contract: retry only the
    retryable rejection (403 StalePodCompletion) on a bounded schedule;
    403 TaskAlreadyCompleted is terminal; anything else non-200 is an error."""
    body = {"status": status, "message": message, "artifacts": artifacts}
    for delay in COMPLETE_RETRY_DELAYS:
        if delay:
            await asyncio.sleep(delay)
        resp = await client.post(gateway + "/v1/task/complete", json=body)
        if resp.status_code == 200:
            return
        if resp.status_code == 403 and "StalePodCompletion" in resp.text:
            continue
        if resp.status_code == 403 and "TaskAlreadyCompleted" in resp.text:
            return
        raise RuntimeError(f"task completion failed: {resp.status_code} {resp.text}")
    raise RuntimeError("task completion exhausted retries")


def build_graph(model: ChatOpenAI):
    async def ask(prompt: str) -> str:
        response = await model.ainvoke([HumanMessage(prompt)])
        return str(response.content)

    async def summarize(state: State) -> State:
        return {"summary": await ask("Summarize this in one paragraph:\n\n" + state["text"])}

    async def critique(state: State) -> State:
        return {
            "critique": await ask(
                "List what this summary misses or overstates about the original."
                f"\n\nOriginal:\n{state['text']}\n\nSummary:\n{state['summary']}"
            )
        }

    async def refine(state: State) -> State:
        return {
            "summary": await ask(
                "Rewrite the summary fixing these problems, one paragraph."
                f"\n\nSummary:\n{state['summary']}\n\nProblems:\n{state['critique']}"
            )
        }

    builder = StateGraph(State)
    builder.add_node("summarize", summarize)
    builder.add_node("critique", critique)
    builder.add_node("refine", refine)
    builder.add_edge(START, "summarize")
    builder.add_edge("summarize", "critique")
    builder.add_edge("critique", "refine")
    builder.add_edge("refine", END)
    return builder.compile()


async def run() -> None:
    gateway = os.environ["KAALM_GATEWAY_ENDPOINT"].rstrip("/")
    ctx = client_ssl_context()
    # Built once from the cert files: a short-lived task never sees a
    # rotation, so the snapshot is safe here.
    async with httpx.AsyncClient(verify=ctx, timeout=300.0, trust_env=False) as client:
        try:
            model = ChatOpenAI(
                model=os.environ["KAALM_MODEL"],
                base_url=gateway + "/v1",
                api_key="managed-by-kaalm",
                http_client=httpx.Client(verify=ctx, timeout=300.0, trust_env=False),
                http_async_client=client,
            )
            text = os.environ["TASK_INPUT_TEXT"]
            result = await build_graph(model).ainvoke({"text": text})
            await complete_task(
                client, gateway, "success", "summarized", {"summary": result["summary"]}
            )
        except Exception as exc:  # noqa: BLE001 - report, then re-raise for the pod log
            await complete_task(client, gateway, "failure", str(exc), {})
            raise


if __name__ == "__main__":
    try:
        asyncio.run(run())
    except Exception as exc:  # noqa: BLE001
        print(f"task failed: {exc}", file=sys.stderr)
        sys.exit(1)
