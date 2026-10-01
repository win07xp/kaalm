# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""complete_task against a scripted gateway: the 409 stale_pod rejection is
retried on the bounded schedule, TaskAlreadyCompleted raises
kaalm.TaskAlreadyCompleted at once, and any other non-200 (a 403 included)
fails at once (contract item 6)."""

from __future__ import annotations

import asyncio

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


class ScriptedGateway:
    def __init__(self, *replies: GatewayReply):
        self.replies = list(replies)
        self.calls = 0
        self.bodies: list = []

    async def post(self, path, json=None):
        assert path == "/v1/task/complete"
        self.calls += 1
        self.bodies.append(json)
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
