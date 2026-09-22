# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""complete_task against a scripted gateway: the 409 stale_pod rejection is
retried on the bounded schedule, TaskAlreadyCompleted is final, and any other
non-200 (a 403 included) fails at once (contract item 6)."""

from __future__ import annotations

import asyncio

import pytest

from kaalm_agent.__main__ import Agent
from kaalm_agent.gateway import GatewayReply

STALE = GatewayReply(409, {"error": {
    "type": "stale_pod", "retryable": True,
    "message": "StalePodCompletion: the calling Pod is not the task's current Pod"}})
DONE = GatewayReply(403, {"error": {
    "type": "access_denied", "retryable": False,
    "message": "TaskAlreadyCompleted: the task has reached a terminal phase"}})
OK = GatewayReply(200, "")


class ScriptedGateway:
    def __init__(self, *replies: GatewayReply):
        self.replies = list(replies)
        self.calls = 0

    async def post(self, path, json=None):
        assert path == "/v1/task/complete"
        self.calls += 1
        return self.replies.pop(0)


@pytest.fixture(autouse=True)
def no_backoff(monkeypatch):
    async def instant(_):
        return None

    monkeypatch.setattr(asyncio, "sleep", instant)


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


async def test_already_completed_is_final():
    gw = ScriptedGateway(DONE)
    await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 1


async def test_a_403_is_not_retried():
    gw = ScriptedGateway(GatewayReply(403, {"error": {
        "type": "access_denied", "message": "StalePodCompletion: old form"}}))
    with pytest.raises(RuntimeError, match="403"):
        await mk_agent(gw).complete_task("success", "done")
    assert gw.calls == 1
