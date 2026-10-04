# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""complete_task against a scripted gateway: the 409 stale_pod rejection, the
503 internal_unavailable answer (each wait after it raised to at least
Retry-After), and transport errors are retried on the bounded schedule, TaskAlreadyCompleted
raises kaalm.TaskAlreadyCompleted at once, and any other non-200 (a 403
included) fails at once (contract item 6). Once a report is accepted, a
later call raises kaalm.TaskAlreadyCompleted without sending."""

from __future__ import annotations

import asyncio

import aiohttp
import pytest

import kaalm

from kaalm_agent.__main__ import Agent
from kaalm_agent.gateway import GatewayReply

STALE = GatewayReply(409, {"error": {
    "type": "stale_pod", "retryable": True,
    "message": "StalePodCompletion: the calling Pod is not the task's current Pod"}})
DONE = GatewayReply(403, {"error": {
    "type": "access_denied", "retryable": False,
    "message": "TaskAlreadyCompleted: the task has reached a terminal phase"}})
OK = GatewayReply(200, "")
UNAVAILABLE_BODY = {"error": {"type": "internal_unavailable", "retryable": True}}
UNAVAILABLE = GatewayReply(503, UNAVAILABLE_BODY)


class ScriptedGateway:
    """Answers each post with the next scripted reply; an exception in the
    script is raised instead, as a transport error would be."""

    def __init__(self, *replies: GatewayReply | Exception):
        self.replies = list(replies)
        self.calls = 0
        self.bodies: list = []

    async def post(self, path, json=None):
        assert path == "/v1/task/complete"
        self.calls += 1
        self.bodies.append(json)
        reply = self.replies.pop(0)
        if isinstance(reply, Exception):
            raise reply
        return reply


@pytest.fixture(autouse=True)
def sleeps(monkeypatch):
    """Skips every backoff and records each requested delay."""
    recorded: list[float] = []

    async def instant(delay):
        recorded.append(delay)

    monkeypatch.setattr(asyncio, "sleep", instant)
    return recorded


def mk_agent(gateway: ScriptedGateway) -> Agent:
    return Agent(handler=None, store=None, gateway=gateway, reloader=None)


async def test_stale_pod_is_retried_then_succeeds():
    gw = ScriptedGateway(STALE, STALE, OK)
    await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 3


async def test_stale_pod_exhausts_the_schedule():
    gw = ScriptedGateway(STALE, STALE, STALE, STALE)
    with pytest.raises(RuntimeError, match="exhausted"):
        await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 4


async def test_already_completed_raises_and_is_final():
    gw = ScriptedGateway(DONE)
    agent = mk_agent(gw)
    with pytest.raises(kaalm.TaskAlreadyCompleted):
        await agent.complete_task("success", "done")
    assert gw.calls == 1
    assert agent.task_reported


async def test_success_marks_the_task_reported():
    agent = mk_agent(ScriptedGateway(OK))
    assert not agent.task_reported
    await agent.complete_task("success")
    assert agent.task_reported


async def test_message_and_artifacts_are_optional():
    gw = ScriptedGateway(OK)
    await mk_agent(gw).complete_task("failure")
    assert gw.bodies == [{"status": "failure", "message": "", "artifacts": {}}]


async def test_artifacts_are_sent():
    gw = ScriptedGateway(OK)
    await mk_agent(gw).complete_task("success", "done", {"report": "s3://bucket/r.md"})
    assert gw.bodies[0]["artifacts"] == {"report": "s3://bucket/r.md"}


async def test_a_403_is_not_retried():
    gw = ScriptedGateway(GatewayReply(403, {"error": {
        "type": "access_denied", "message": "StalePodCompletion: old form"}}))
    with pytest.raises(RuntimeError, match="403"):
        await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 1


async def test_transport_errors_are_retried_then_succeed():
    """Like agentruntime's CompleteTask, an error before any answer (a refused
    or reset connection) is retried within the same bounded attempts."""
    gw = ScriptedGateway(
        aiohttp.ClientConnectionError("connection refused"),
        aiohttp.ClientConnectionError("connection reset"),
        OK,
    )
    await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 3


async def test_transport_errors_exhaust_the_schedule():
    gw = ScriptedGateway(*([aiohttp.ClientConnectionError("connection refused")] * 4))
    with pytest.raises(RuntimeError, match="exhausted.*connection refused"):
        await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 4


async def test_no_report_is_sent_after_an_accepted_one():
    gw = ScriptedGateway(OK)
    agent = mk_agent(gw)
    await agent.complete_task("success")
    with pytest.raises(kaalm.TaskAlreadyCompleted):
        await agent.complete_task("failure", "second thoughts")
    assert gw.calls == 1


async def test_unavailable_is_retried_then_succeeds(sleeps):
    gw = ScriptedGateway(UNAVAILABLE, UNAVAILABLE, OK)
    await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 3
    # No Retry-After: each wait after a 503 is raised to the 1-second minimum.
    assert sleeps == [1.0, 1.0]


async def test_unavailable_waits_at_least_retry_after(sleeps):
    slow = GatewayReply(503, UNAVAILABLE_BODY, retry_after=3.0)
    gw = ScriptedGateway(slow, slow, slow, OK)
    await mk_agent(gw).complete_task("success", "done")
    assert sleeps == [3.0, 3.0, 3.0]


async def test_retry_after_is_capped(sleeps):
    huge = GatewayReply(503, UNAVAILABLE_BODY, retry_after=600.0)
    gw = ScriptedGateway(huge, huge, huge, huge)
    with pytest.raises(RuntimeError, match="exhausted"):
        await mk_agent(gw).complete_task("success", "done")
    assert sleeps == [30.0, 30.0, 30.0]
    assert gw.calls == 4


async def test_unavailable_exhausts_the_schedule(sleeps):
    gw = ScriptedGateway(UNAVAILABLE, UNAVAILABLE, UNAVAILABLE, UNAVAILABLE)
    with pytest.raises(RuntimeError, match="exhausted.*503"):
        await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 4
    assert sleeps == [1.0, 1.0, 2.0]


async def test_floor_applies_only_after_a_503(sleeps):
    gw = ScriptedGateway(UNAVAILABLE, STALE, OK)
    await mk_agent(gw).complete_task("success", "done")
    assert sleeps == [1.0, 0.5]


async def test_a_bare_503_is_not_retried():
    gw = ScriptedGateway(GatewayReply(503, "upstream sad"))
    with pytest.raises(RuntimeError, match="503"):
        await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 1
