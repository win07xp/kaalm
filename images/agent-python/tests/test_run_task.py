# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Task mode entry point: the handler module's optional ``async def
run_task()`` runs once at startup in task mode, and the runtime reports
completion for it when it returns (success) or raises (failure, with the
exception text), unless the task already reported. The handler reaches the
same completion path through ``kaalm.complete_task``."""

from __future__ import annotations

import asyncio
import subprocess

import pytest

import kaalm
from kaalm_agent import __main__ as runtime
from kaalm_agent.__main__ import Agent
from kaalm_agent.gateway import GatewayReply

OK = GatewayReply(200, "")
DONE = GatewayReply(403, {"error": {
    "type": "access_denied", "retryable": False,
    "message": "TaskAlreadyCompleted: the task has reached a terminal phase"}})


class RecordingGateway:
    """Answers each completion with the next scripted reply (OK once the
    script runs out) and records every body."""

    def __init__(self, *replies: GatewayReply):
        self.replies = list(replies)
        self.bodies: list[dict] = []

    async def post(self, path, json=None):
        assert path == "/v1/task/complete"
        self.bodies.append(json)
        return self.replies.pop(0) if self.replies else OK


@pytest.fixture(autouse=True)
def no_backoff(monkeypatch):
    async def instant(_):
        return None

    monkeypatch.setattr(asyncio, "sleep", instant)


def mk_task_agent(gateway, run_task=None) -> Agent:
    agent = Agent(handler=None, store=None, gateway=gateway, reloader=None)
    agent.is_task = True
    agent.run_task = run_task
    return agent


async def test_returning_run_task_completes_with_success():
    calls = []

    async def run_task():
        calls.append("ran")

    gw = RecordingGateway()
    await mk_task_agent(gw, run_task).run_task_and_complete()
    assert calls == ["ran"]
    assert gw.bodies == [{"status": "success", "message": "", "artifacts": {}}]


async def test_raising_run_task_completes_with_failure_and_the_exception_text():
    async def run_task():
        raise ValueError("upstream returned 500")

    gw = RecordingGateway()
    await mk_task_agent(gw, run_task).run_task_and_complete()
    assert gw.bodies == [{"status": "failure", "message": "upstream returned 500", "artifacts": {}}]


async def test_exception_without_text_reports_its_type():
    async def run_task():
        raise KeyError

    gw = RecordingGateway()
    await mk_task_agent(gw, run_task).run_task_and_complete()
    assert gw.bodies[0]["status"] == "failure"
    assert gw.bodies[0]["message"] == "KeyError"


async def test_run_task_that_reports_itself_is_not_completed_again():
    gw = RecordingGateway()
    agent = mk_task_agent(gw)

    async def run_task():
        await agent.complete_task("failure", "bad input", {"log": "s3://b/log"})

    agent.run_task = run_task
    await agent.run_task_and_complete()
    assert gw.bodies == [{"status": "failure", "message": "bad input", "artifacts": {"log": "s3://b/log"}}]


async def test_already_completed_after_run_task_is_fine():
    async def run_task():
        return None

    gw = RecordingGateway(DONE)
    await mk_task_agent(gw, run_task).run_task_and_complete()  # must not raise
    assert len(gw.bodies) == 1


async def test_already_completed_reaches_a_handler_that_reports():
    gw = RecordingGateway(DONE)
    agent = mk_task_agent(gw)
    seen = []

    async def run_task():
        try:
            await agent.complete_task("success")
        except kaalm.TaskAlreadyCompleted:
            seen.append("already")

    agent.run_task = run_task
    await agent.run_task_and_complete()
    assert seen == ["already"]
    assert len(gw.bodies) == 1


async def test_runtime_completion_retries_like_the_autocomplete_hook():
    """A completion reported the moment the Pod starts can race the gateway's
    source-IP check, so the runtime's own report retries (6 attempts)."""
    async def run_task():
        return None

    gw = RecordingGateway(*([GatewayReply(403, "unknown source")] * 2))
    await mk_task_agent(gw, run_task).run_task_and_complete()
    assert [b["status"] for b in gw.bodies] == ["success"] * 3


async def test_start_task_work_runs_run_task_in_task_mode(monkeypatch):
    monkeypatch.delenv("KAALM_TASK_AUTOCOMPLETE", raising=False)
    ran = asyncio.Event()

    async def run_task():
        ran.set()

    gw = RecordingGateway()
    tasks = mk_task_agent(gw, run_task).start_task_work()
    assert len(tasks) == 1
    await asyncio.gather(*tasks)
    assert ran.is_set()
    assert gw.bodies[0]["status"] == "success"


async def test_start_task_work_ignores_run_task_outside_task_mode(monkeypatch):
    monkeypatch.setenv("KAALM_TASK_AUTOCOMPLETE", "success")

    async def run_task():
        raise AssertionError("must not run in Agent mode")

    agent = mk_task_agent(RecordingGateway(), run_task)
    agent.is_task = False
    assert agent.start_task_work() == []


def test_build_binds_complete_task_and_run_task(tmp_path, monkeypatch, handler_dir):
    """build() binds kaalm.complete_task to the Agent's own coroutine before
    the handler import, and hands the handler's run_task to the Agent."""
    cert, key = tmp_path / "tls.crt", tmp_path / "tls.key"
    subprocess.run(
        [
            "openssl", "req", "-x509", "-newkey", "ec",
            "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "1",
            "-subj", "/CN=workload", "-addext", "subjectAltName=DNS:job.default.task.kaalm.io",
            "-keyout", str(key), "-out", str(cert),
        ],
        check=True,
        capture_output=True,
    )
    monkeypatch.setenv("KAALM_TLS_CERT", str(cert))
    monkeypatch.setenv("KAALM_TLS_KEY", str(key))
    monkeypatch.setenv("KAALM_CA_CERT", str(cert))
    monkeypatch.setenv("KAALM_MEMORY_DIR", str(tmp_path / "memory"))
    d = handler_dir(
        "import kaalm\n"
        "BOUND_AT_IMPORT = kaalm.complete_task\n"
        "async def handle_message(envelope):\n"
        "    return {}\n"
        "async def run_task():\n"
        "    return None\n"
    )
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(d))

    agent = runtime.build()
    import handler  # the loader registers the module under this name

    assert agent.is_task
    assert kaalm.complete_task == agent.complete_task
    assert handler.BOUND_AT_IMPORT == agent.complete_task
    assert agent.run_task is handler.run_task
    assert agent.handler is not None
