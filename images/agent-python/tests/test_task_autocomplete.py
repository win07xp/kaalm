# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The KAALM_TASK_AUTOCOMPLETE startup hook, matching agentruntime/agent.go:
in task mode a non-empty value is reported once at startup ("auto-complete on
startup"), with up to 6 attempts 5 seconds apart; outside task mode it is
ignored. It runs alongside run_task, not instead of it."""

from __future__ import annotations

import asyncio

import pytest

from kaalm_agent import __main__ as runtime
from kaalm_agent.__main__ import Agent
from kaalm_agent.gateway import GatewayReply

OK = GatewayReply(200, "")
DONE = GatewayReply(403, {"error": {
    "type": "access_denied", "retryable": False,
    "message": "TaskAlreadyCompleted: the task has reached a terminal phase"}})
UNKNOWN_SOURCE = GatewayReply(403, "source address is not a known Pod")


class RecordingGateway:
    def __init__(self, *replies: GatewayReply):
        self.replies = list(replies)
        self.bodies: list[dict] = []

    async def post(self, path, json=None):
        assert path == "/v1/task/complete"
        self.bodies.append(json)
        return self.replies.pop(0) if self.replies else OK


@pytest.fixture()
def sleeps(monkeypatch):
    recorded: list[float] = []

    async def instant(delay):
        recorded.append(delay)

    monkeypatch.setattr(asyncio, "sleep", instant)
    return recorded


def mk_agent(gateway, is_task=True) -> Agent:
    agent = Agent(handler=None, store=None, gateway=gateway, reloader=None)
    agent.is_task = is_task
    return agent


async def test_reports_the_env_status_in_task_mode(monkeypatch, sleeps):
    monkeypatch.setenv("KAALM_TASK_AUTOCOMPLETE", "failure")
    gw = RecordingGateway()
    tasks = mk_agent(gw).start_task_work()
    await asyncio.gather(*tasks)
    assert gw.bodies == [{"status": "failure", "message": "auto-complete on startup", "artifacts": {}}]


async def test_ignored_outside_task_mode(monkeypatch, sleeps):
    monkeypatch.setenv("KAALM_TASK_AUTOCOMPLETE", "success")
    assert mk_agent(RecordingGateway(), is_task=False).start_task_work() == []


async def test_unset_or_empty_does_nothing(monkeypatch, sleeps):
    monkeypatch.setenv("KAALM_TASK_AUTOCOMPLETE", "")
    assert mk_agent(RecordingGateway()).start_task_work() == []
    monkeypatch.delenv("KAALM_TASK_AUTOCOMPLETE")
    assert mk_agent(RecordingGateway()).start_task_work() == []


async def test_retries_six_times_five_seconds_apart(sleeps):
    gw = RecordingGateway(*([UNKNOWN_SOURCE] * 10))
    await mk_agent(gw).autocomplete("success")
    assert len(gw.bodies) == 6
    assert sleeps == [runtime.AUTOCOMPLETE_RETRY_DELAY] * 5
    assert runtime.AUTOCOMPLETE_RETRY_DELAY == 5.0


async def test_succeeds_on_a_later_attempt(sleeps):
    gw = RecordingGateway(UNKNOWN_SOURCE, UNKNOWN_SOURCE)
    await mk_agent(gw).autocomplete("success")
    assert len(gw.bodies) == 3


async def test_stops_when_the_task_already_completed(sleeps):
    gw = RecordingGateway(DONE)
    await mk_agent(gw).autocomplete("success")
    assert len(gw.bodies) == 1


async def test_runs_alongside_run_task(monkeypatch, sleeps):
    """Both set: the hook reports at startup and run_task still runs, as in
    Go, where the hook goroutine starts whatever the user code does."""
    monkeypatch.setenv("KAALM_TASK_AUTOCOMPLETE", "success")
    ran = []

    async def run_task():
        ran.append(True)

    gw = RecordingGateway(OK, DONE)
    agent = mk_agent(gw)
    agent.run_task = run_task
    tasks = agent.start_task_work()
    assert len(tasks) == 2
    await asyncio.gather(*tasks)
    assert ran == [True]
    assert gw.bodies[0] == {"status": "success", "message": "auto-complete on startup", "artifacts": {}}
