# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Task mode entry point: the handler module's optional ``async def
run_task()`` runs once at startup in task mode, and the runtime reports
completion for it when it returns (success) or raises (failure, with the
exception text), unless the task already reported. The handler reaches the
same completion path through ``kaalm.complete_task``.

For an exitCode task the gateway answers every report with 403
TaskNotAgentReported; the runtime then shuts down cleanly and exits 0 when
run_task returned and 1 when it raised, so the container exit is the
outcome."""

from __future__ import annotations

import asyncio
import logging
import subprocess

import aiohttp
import pytest

import kaalm
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
# A 5xx that complete_task does not retry itself, so only the runtime's own
# report retries it.
SERVER_ERROR = GatewayReply(500, {"error": {"type": "internal_error"}})
UNKNOWN_SOURCE = GatewayReply(401, {"error": {
    "type": "unauthenticated", "message": "source address is not a known Pod"}})
BAD_REQUEST = GatewayReply(400, {"error": {
    "type": "bad_request", "message": "missing declared artifact: report"}})


class RecordingGateway:
    """Answers each completion with the next scripted reply (OK once the
    script runs out) and records every body. An exception in the script is
    raised instead, as a transport error would be."""

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

    async def close(self):
        return None


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


async def test_runtime_completion_retries_a_retryable_answer():
    """The runtime's own report retries what may pass later (a 5xx, a 409
    stale_pod that outlasts complete_task's schedule) up to 6 attempts."""
    async def run_task():
        return None

    gw = RecordingGateway(SERVER_ERROR, SERVER_ERROR)
    await mk_task_agent(gw, run_task).run_task_and_complete()
    assert [b["status"] for b in gw.bodies] == ["success"] * 3


async def test_runtime_completion_lands_after_transport_errors():
    async def run_task():
        return None

    gw = RecordingGateway(
        aiohttp.ClientConnectionError("connection refused"),
        aiohttp.ClientConnectionError("connection refused"),
    )
    agent = mk_task_agent(gw, run_task)
    await agent.run_task_and_complete()
    assert [b["status"] for b in gw.bodies] == ["success"] * 3
    assert agent.task_reported


async def test_runtime_completion_stops_on_a_4xx(caplog):
    """Any 4xx but 409 stale_pod answers every retry the same way: logged
    once, no further attempt."""
    async def run_task():
        return None

    gw = RecordingGateway(*([BAD_REQUEST] * 6))
    with caplog.at_level(logging.WARNING, logger="agent"):
        await mk_task_agent(gw, run_task).run_task_and_complete()
    assert len(gw.bodies) == 1
    rejected = [r for r in caplog.records if "missing declared artifact" in r.getMessage()]
    assert len(rejected) == 1
    assert rejected[0].levelno == logging.ERROR


async def test_runtime_completion_retries_a_401_from_the_source_ip_check():
    """A Pod that has just started can answer the gateway's source-IP check
    with 401 until the kubelet posts its IP; the runtime's own report retries
    it, as agentruntime's hook does."""
    async def run_task():
        return None

    gw = RecordingGateway(UNKNOWN_SOURCE, UNKNOWN_SOURCE)
    agent = mk_task_agent(gw, run_task)
    await agent.run_task_and_complete()
    assert [b["status"] for b in gw.bodies] == ["success"] * 3
    assert agent.task_reported


async def test_report_after_exit_code_is_known_sends_nothing():
    """Once the task is known to be exitCode, a report waiting on the lock
    sends nothing (the hook and run_task can both be in flight)."""
    gw = RecordingGateway(NOT_AGENT_REPORTED)
    agent = mk_task_agent(gw)
    with pytest.raises(runtime.CompletionRejected):
        await agent.complete_task("success")
    with pytest.raises(runtime.CompletionRejected) as second:
        await agent.complete_task("failure")
    assert second.value.not_agent_reported
    assert len(gw.bodies) == 1


async def test_failure_message_is_cut_to_4_kib():
    async def run_task():
        raise RuntimeError("é" * 5000)  # 2 UTF-8 bytes each

    gw = RecordingGateway()
    await mk_task_agent(gw, run_task).run_task_and_complete()
    message = gw.bodies[0]["message"]
    assert len(message.encode("utf-8")) <= 4096
    assert message == "é" * 2048


async def test_success_report_has_no_artifacts():
    async def run_task():
        return None

    gw = RecordingGateway()
    await mk_task_agent(gw, run_task).run_task_and_complete()
    assert gw.bodies[0]["artifacts"] == {}


async def test_handler_report_after_an_accepted_one_raises_without_sending():
    gw = RecordingGateway()
    agent = mk_task_agent(gw)
    seen = []

    async def run_task():
        await agent.complete_task("success")
        try:
            await agent.complete_task("failure", "changed my mind")
        except kaalm.TaskAlreadyCompleted:
            seen.append("already")

    agent.run_task = run_task
    await agent.run_task_and_complete()
    assert seen == ["already"]
    assert len(gw.bodies) == 1


@pytest.mark.parametrize("raises, code", [(False, 0), (True, 1)])
async def test_exit_code_task_exits_with_the_run_task_outcome(raises, code):
    async def run_task():
        if raises:
            raise ValueError("boom")

    gw = RecordingGateway(*([NOT_AGENT_REPORTED] * 6))
    agent = mk_task_agent(gw, run_task)
    await agent.run_task_and_complete()
    assert len(gw.bodies) == 1
    assert agent.stop.is_set()
    assert agent.exit_code == code


async def test_agent_reported_task_keeps_serving_after_run_task():
    async def run_task():
        raise ValueError("boom")

    agent = mk_task_agent(RecordingGateway(), run_task)
    await agent.run_task_and_complete()
    assert not agent.stop.is_set()
    assert agent.exit_code is None


async def test_exit_code_task_with_the_hook_exits_on_the_run_task_outcome():
    """The hook learns the task is exitCode first; the process still waits
    for run_task, whose outcome is the exit code, and sends nothing more."""
    async def run_task():
        raise ValueError("boom")

    gw = RecordingGateway(NOT_AGENT_REPORTED)
    agent = mk_task_agent(gw, run_task)
    await agent.autocomplete("success")
    assert not agent.stop.is_set()
    await agent.run_task_and_complete()
    assert len(gw.bodies) == 1
    assert agent.exit_code == 1


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


def _task_cert(tmp_path):
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
    return cert, key


def test_build_binds_complete_task_and_run_task(tmp_path, monkeypatch, handler_dir):
    """build() binds kaalm.complete_task to the Agent's own coroutine before
    the handler import, and hands the handler's run_task to the Agent."""
    cert, key = _task_cert(tmp_path)
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


@pytest.mark.parametrize("raises, code", [(False, 0), (True, 1)])
async def test_serve_returns_the_exit_code_task_outcome(tmp_path, monkeypatch, handler_dir, raises, code):
    """End to end through serve(): the server starts, run_task runs, the
    gateway answers TaskNotAgentReported, and serve() shuts down cleanly and
    returns the process exit code."""
    cert, key = _task_cert(tmp_path)
    monkeypatch.setenv("KAALM_TLS_CERT", str(cert))
    monkeypatch.setenv("KAALM_TLS_KEY", str(key))
    monkeypatch.setenv("KAALM_CA_CERT", str(cert))
    monkeypatch.setenv("KAALM_MEMORY_DIR", str(tmp_path / "memory"))
    monkeypatch.setenv("KAALM_HEALTH_PORT", "0")
    monkeypatch.delenv("KAALM_TASK_AUTOCOMPLETE", raising=False)
    body = "    raise ValueError('boom')\n" if raises else "    return None\n"
    d = handler_dir(
        "async def handle_message(envelope):\n"
        "    return {}\n"
        "async def run_task():\n" + body
    )
    monkeypatch.setenv("KAALM_HANDLER_PATH", str(d))

    agent = runtime.build()
    gw = RecordingGateway(NOT_AGENT_REPORTED)
    agent.gateway = gw
    assert await asyncio.wait_for(runtime.serve(agent), timeout=10) == code
    assert len(gw.bodies) == 1
