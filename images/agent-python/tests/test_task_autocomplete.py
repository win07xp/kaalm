# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The KAALM_TASK_AUTOCOMPLETE startup hook, matching agentruntime/agent.go:
in task mode a non-empty value is reported once at startup ("auto-complete on
startup"), with up to 6 attempts 5 seconds apart; outside task mode it is
ignored. It runs alongside run_task, not instead of it. A 4xx other than 409
stale_pod ends the attempts; an exitCode task (403 TaskNotAgentReported)
without run_task exits 0 for success and 1 for failure."""

from __future__ import annotations

import asyncio

import aiohttp
import pytest

from kaalm_agent import __main__ as runtime
from kaalm_agent.__main__ import Agent
from kaalm_agent.gateway import GatewayReply

OK = GatewayReply(200, "")
DONE = GatewayReply(403, {"error": {
    "type": "access_denied", "retryable": False,
    "message": "TaskAlreadyCompleted: the task has reached a terminal phase"}})
NOT_AGENT_REPORTED = GatewayReply(403, {"error": {
    "type": "access_denied", "retryable": False,
    "message": "TaskNotAgentReported: this task completes via container exit"}})
# A generic answer that no retry can change. The start-of-Pod race on the
# gateway's identity check answers 409 stale_pod, which complete_task retries.
NON_RETRYABLE = GatewayReply(403, {"error": {"type": "access_denied", "message": "denied"}})
UNAVAILABLE = GatewayReply(503, {"error": {"type": "internal_unavailable", "retryable": True}})


class RecordingGateway:
    """Answers with the next scripted reply (OK once the script runs out);
    an exception in the script is raised, as a transport error would be."""

    def __init__(self, *replies: GatewayReply | Exception):
        self.replies = list(replies)
        self.bodies: list[dict] = []

    async def post(self, path, json=None):
        assert path == "/v1/task/complete"
        self.bodies.append(json)
        reply = self.replies.pop(0) if self.replies else OK
        if isinstance(reply, Exception):
            raise reply
        return reply


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
    gw = RecordingGateway(*([UNAVAILABLE] * 10))
    await mk_agent(gw).autocomplete("success")
    assert len(gw.bodies) == 6
    assert sleeps == [runtime.AUTOCOMPLETE_RETRY_DELAY] * 5
    assert runtime.AUTOCOMPLETE_RETRY_DELAY == 5.0


async def test_succeeds_on_a_later_attempt(sleeps):
    gw = RecordingGateway(UNAVAILABLE, UNAVAILABLE)
    await mk_agent(gw).autocomplete("success")
    assert len(gw.bodies) == 3


async def test_lands_after_transport_errors(sleeps):
    gw = RecordingGateway(
        aiohttp.ClientConnectionError("connection refused"),
        aiohttp.ClientConnectionError("connection reset"),
    )
    agent = mk_agent(gw)
    await agent.autocomplete("success")
    assert len(gw.bodies) == 3
    assert agent.task_reported


async def test_stops_on_a_non_retryable_answer(sleeps):
    gw = RecordingGateway(*([NON_RETRYABLE] * 6))
    await mk_agent(gw).autocomplete("success")
    assert len(gw.bodies) == 1


@pytest.mark.parametrize("status, code", [("success", 0), ("failure", 1)])
async def test_exit_code_task_exits_with_the_hook_status(sleeps, status, code):
    gw = RecordingGateway(*([NOT_AGENT_REPORTED] * 6))
    agent = mk_agent(gw)
    await agent.autocomplete(status)
    assert len(gw.bodies) == 1
    assert agent.stop.is_set()
    assert agent.exit_code == code


async def test_agent_reported_task_keeps_serving_after_the_hook(sleeps):
    agent = mk_agent(RecordingGateway())
    await agent.autocomplete("success")
    assert not agent.stop.is_set()
    assert agent.exit_code is None


async def test_no_hook_report_after_an_accepted_one(sleeps):
    gw = RecordingGateway()
    agent = mk_agent(gw)
    await agent.complete_task("failure", "reported by the handler")
    await agent.autocomplete("success")
    assert len(gw.bodies) == 1


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

    gw = RecordingGateway()
    agent = mk_agent(gw)
    agent.run_task = run_task
    tasks = agent.start_task_work()
    assert len(tasks) == 2
    await asyncio.gather(*tasks)
    assert ran == [True]
    # The hook's report is accepted first, so the report after run_task is
    # never sent.
    assert gw.bodies == [{"status": "success", "message": "auto-complete on startup", "artifacts": {}}]
