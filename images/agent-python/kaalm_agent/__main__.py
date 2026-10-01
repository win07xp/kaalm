# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Kaalm reference base image entrypoint.

Startup order matters and is deliberate: TLS material first (the contract's
transport), then the store and gateway client, then kaalm._bind, and only then
the handler import, so a handler's `import kaalm` always finds bound objects.
A broken configured handler exits nonzero before the server ever binds a port,
so a bad rollout is CrashLoopBackOff, never a half-alive agent.

In task mode the server starts first and then two optional pieces of task work
run beside it: the handler module's ``run_task()`` and the
KAALM_TASK_AUTOCOMPLETE startup hook (agentruntime/agent.go has the same hook).
For an exitCode task the gateway answers every report with 403
TaskNotAgentReported; the runtime then shuts down cleanly and exits with the
task's outcome (0 or 1), because the container exit is how that task completes.
"""

from __future__ import annotations

import asyncio
import contextlib
import functools
import json
import logging
import os
import signal
import sys
from typing import Any

import aiohttp
from aiohttp import web

import kaalm

from . import loader, tracecontext
from .gateway import GatewayClient
from .httpclient import make_http_async_client, make_http_client
from .memory import Store, UserMemory
from .tls import CertReloader, gateway_sans, peer_san_matches_gateway, workload_is_task

log = logging.getLogger("agent")

HEARTBEAT_PERIOD = 30  # seconds

# complete_task's bounded backoff for the 409 stale_pod rejection and for
# transport errors (contract item 6), as in agentruntime/complete.go.
COMPLETE_RETRY_SCHEDULE = (0.0, 0.1, 0.5, 2.0)  # seconds; the first is no wait

# Errors raised before the gateway answers at all: complete_task retries
# these within its schedule, like agentruntime's CompleteTask.
TRANSPORT_ERRORS = (aiohttp.ClientError, asyncio.TimeoutError, OSError)

# The runtime's own completion reports (the KAALM_TASK_AUTOCOMPLETE hook and
# the report after run_task) retry like agentruntime/agent.go autocomplete: a
# report made as the Pod starts can race the gateway's source-IP check, which
# answers 401 until the kubelet posts this Pod's IP. Only answers that a later
# attempt may change are retried: transport errors, a 5xx, that 401, and a
# 409 stale_pod that outlasts complete_task's own schedule.
AUTOCOMPLETE_ATTEMPTS = 6
AUTOCOMPLETE_RETRY_DELAY = 5.0  # seconds

# The automatic failure report carries the exception text, cut to this many
# UTF-8 bytes.
FAILURE_MESSAGE_LIMIT = 4096


class CompletionRejected(RuntimeError):
    """The gateway answered a completion with a status complete_task does not
    retry. A RuntimeError, so handler code that catches it keeps working."""

    def __init__(self, status: int, text: str):
        super().__init__(f"task completion failed: {status} {text}")
        self.status = status
        self.text = text

    @property
    def not_agent_reported(self) -> bool:
        """The 403 TaskNotAgentReported: an exitCode task, which has no
        completion mailbox and completes through the container's exit."""
        return self.status == 403 and "TaskNotAgentReported" in self.text


def truncate_utf8(text: str, limit: int) -> str:
    """Cut text to at most limit UTF-8 bytes without splitting a character."""
    return text.encode("utf-8")[:limit].decode("utf-8", "ignore")


class Agent:
    def __init__(self, handler: loader.AsyncHandler, store: Store, gateway: GatewayClient, reloader: CertReloader):
        self.health_port = int(os.environ.get("KAALM_HEALTH_PORT", "8080"))
        self.gateway = gateway
        self.reloader = reloader
        self.store = store
        self.handler = handler
        self.is_task = workload_is_task(
            os.environ.get("KAALM_TLS_CERT", "/var/run/kaalm/tls.crt")
        )
        self.gateway_sans = gateway_sans()
        # The handler module's optional task entry point, set by build().
        self.run_task: loader.TaskEntry | None = None
        # True once the gateway accepted a completion or answered that the
        # task is already terminal. From then on no report is sent: the
        # runtime's own reports skip, and complete_task raises
        # kaalm.TaskAlreadyCompleted without sending.
        self.task_reported = False
        # True once the gateway answered 403 TaskNotAgentReported (an
        # exitCode task); the runtime's own reports skip then.
        self.not_agent_reported = False
        # One report at a time, so a report started while another is in
        # flight sees its outcome first.
        self._report_lock = asyncio.Lock()
        # Set to end serve(): by SIGTERM or SIGINT, or by request_exit with
        # the exit code an exitCode task's outcome gives.
        self.stop = asyncio.Event()
        self.exit_code: int | None = None

    async def respond(self, envelope: dict[str, Any]) -> dict[str, Any]:
        """Dedup, dispatch, remember. Transport-independent and test-covered.

        An envelope without a messageId is dispatched but never cached:
        deduplicating on the empty string would answer every id-less message
        with the first id-less reply forever.
        """
        message_id = envelope.get("messageId", "")
        if message_id:
            cached = self.store.recall(message_id)
            if cached is not None:
                return cached
        reply = await self.handler(envelope)
        if message_id:
            self.store.remember(message_id, reply)
        return reply

    async def handle_readyz(self, _: web.Request) -> web.Response:
        """Readiness (contract item 1): 200 only when the container can accept
        a message, which takes a loaded handler and a usable certificate."""
        if self.handler is None or self.reloader is None or not self.reloader.usable():
            return web.Response(status=503, text="not ready")
        return web.Response(text="ok")

    async def handle_v1_message(self, request: web.Request) -> web.Response:
        # Per-path mTLS enforcement (contract item 4).
        ssl_object = request.transport.get_extra_info("ssl_object") if request.transport else None
        peercert = ssl_object.getpeercert() if ssl_object else None
        if not peercert:
            return web.Response(status=401, text="client certificate required")
        if not peer_san_matches_gateway(ssl_object, self.gateway_sans):
            return web.Response(status=403, text="gateway identity required")

        try:
            envelope = await request.json()
        except Exception:  # noqa: BLE001
            return web.Response(status=400, text="invalid message envelope")

        # Each aiohttp request runs in its own task, so the captured trace
        # context is confined to this message's handling.
        tracecontext.set_from_headers(request.headers)
        return web.json_response(await self.respond(envelope))

    async def heartbeat_loop(self) -> None:
        while True:
            await asyncio.sleep(HEARTBEAT_PERIOD)
            try:
                await self.gateway.post("/v1/agent/heartbeat")
            except Exception as exc:  # noqa: BLE001
                log.warning("heartbeat failed: %s", exc)

    def should_heartbeat(self) -> bool:
        # auto (default): Agent mode only. off: never. No force-on for tasks.
        if os.environ.get("KAALM_TEMPLATE_HEARTBEAT") == "off":
            return False
        return not self.is_task

    async def complete_task(self, status: str, message: str = "", artifacts: dict[str, str] | None = None) -> None:
        """Report AgentTask completion, retrying the 409 stale_pod rejection
        and transport errors.

        Four attempts: immediately, then after 100ms, 500ms, 2s (contract
        item 6). A TaskAlreadyCompleted 403 is terminal and raises
        kaalm.TaskAlreadyCompleted, as does any call after a report was
        accepted (without sending). Any other answer raises
        CompletionRejected. Bound as kaalm.complete_task.
        """
        async with self._report_lock:
            if self.task_reported:
                raise kaalm.TaskAlreadyCompleted("this task already reported its completion")
            if self.not_agent_reported:
                # An exitCode task: the gateway gives every report the same
                # 403, so a report that waited on the lock sends nothing.
                raise CompletionRejected(403, "TaskNotAgentReported: this task completes via container exit")
            body = {"status": status, "message": message, "artifacts": artifacts or {}}
            last: Exception | None = None
            for delay in COMPLETE_RETRY_SCHEDULE:
                if delay:
                    await asyncio.sleep(delay)
                try:
                    reply = await self.gateway.post("/v1/task/complete", json=body)
                except TRANSPORT_ERRORS as exc:
                    last = exc
                    continue
                text = reply.data if isinstance(reply.data, str) else json.dumps(reply.data)
                if reply.status == 200:
                    self.task_reported = True
                    return
                if reply.status == 409 and '"stale_pod"' in text:
                    last = CompletionRejected(reply.status, text)
                    continue
                if reply.status == 403 and "TaskAlreadyCompleted" in text:
                    self.task_reported = True
                    raise kaalm.TaskAlreadyCompleted(text)
                rejected = CompletionRejected(reply.status, text)
                if rejected.not_agent_reported:
                    self.not_agent_reported = True
                raise rejected
            raise RuntimeError(f"task completion exhausted retries: {last}") from last

    async def report_completion(self, status: str, message: str) -> None:
        """The runtime's own completion report: complete_task with up to
        AUTOCOMPLETE_ATTEMPTS attempts AUTOCOMPLETE_RETRY_DELAY apart.

        Nothing is sent once any report was accepted or the task is known to
        be exitCode. An already-terminal task, an exitCode task, and any
        other 4xx (a 409 stale_pod aside, which complete_task retries, and a
        401 from the source-IP check at Pod start) end the attempts at once,
        since a retry gets the same answer."""
        if self.task_reported or self.not_agent_reported:
            log.info("task completion %r not sent: the task already has its outcome", status)
            return
        for attempt in range(1, AUTOCOMPLETE_ATTEMPTS + 1):
            try:
                await self.complete_task(status, message)
                log.info("task completion %r reported (attempt %d)", status, attempt)
                return
            except kaalm.TaskAlreadyCompleted:
                log.info("task already completed; completion %r not reported", status)
                return
            except CompletionRejected as exc:
                if exc.not_agent_reported:
                    log.info("task completes via container exit (exitCode); completion %r not reported", status)
                    return
                if 400 <= exc.status < 500 and exc.status != 401:
                    log.error("task completion %r rejected, not retrying: %s", status, exc)
                    return
                log.warning("task completion attempt %d failed: %s", attempt, exc)
            except Exception as exc:  # noqa: BLE001 - logged and retried
                log.warning("task completion attempt %d failed: %s", attempt, exc)
            if attempt < AUTOCOMPLETE_ATTEMPTS:
                await asyncio.sleep(AUTOCOMPLETE_RETRY_DELAY)
        log.error(
            "task completion giving up after %d attempts; status %r not reported",
            AUTOCOMPLETE_ATTEMPTS, status,
        )

    def request_exit(self, code: int) -> None:
        """End serve() after a clean shutdown, with this process exit code."""
        self.exit_code = code
        self.stop.set()

    async def autocomplete(self, status: str) -> None:
        """The KAALM_TASK_AUTOCOMPLETE startup hook: a smoke and e2e test aid,
        not how a real task completes. For an exitCode task without run_task
        the process exits: 0 for success, 1 for any other status."""
        await self.report_completion(status, "auto-complete on startup")
        if self.not_agent_reported and self.run_task is None:
            self.request_exit(0 if status == "success" else 1)

    async def run_task_and_complete(self) -> None:
        """Run the handler's run_task once, then report its outcome unless the
        task already reported: success (no artifacts) when it returns,
        failure with the exception text (at most 4 KiB) when it raises. For
        an exitCode task the process then exits: 0 when run_task returned,
        1 when it raised."""
        assert self.run_task is not None
        try:
            await self.run_task()
        except Exception as exc:  # noqa: BLE001 - the failure becomes the task's outcome
            log.exception("run_task raised")
            status = "failure"
            message = truncate_utf8(str(exc) or type(exc).__name__, FAILURE_MESSAGE_LIMIT)
        else:
            log.info("run_task returned")
            status, message = "success", ""
        await self.report_completion(status, message)
        if self.not_agent_reported:
            self.request_exit(0 if status == "success" else 1)

    def start_task_work(self) -> list[asyncio.Task]:
        """Start the task-mode background work: the KAALM_TASK_AUTOCOMPLETE
        hook (any non-empty value, as in agentruntime) and run_task. Both run
        when both are set. Outside task mode nothing starts."""
        if not self.is_task:
            if self.run_task is not None:
                log.info("run_task defined but this workload is not an AgentTask; not running it")
            return []
        tasks: list[asyncio.Task] = []
        status = os.environ.get("KAALM_TASK_AUTOCOMPLETE", "")
        if status:
            tasks.append(asyncio.create_task(self.autocomplete(status)))
        if self.run_task is not None:
            tasks.append(asyncio.create_task(self.run_task_and_complete()))
        return tasks


def build() -> Agent:
    """Construct the runtime in contract order. Split from main() so tests can
    build an Agent without binding sockets."""
    cert_file = os.environ.get("KAALM_TLS_CERT", "/var/run/kaalm/tls.crt")
    key_file = os.environ.get("KAALM_TLS_KEY", "/var/run/kaalm/tls.key")
    ca_file = os.environ.get("KAALM_CA_CERT", "/var/run/kaalm/ca.crt")
    gateway_url = os.environ.get("KAALM_GATEWAY_ENDPOINT", "").rstrip("/")

    reloader = CertReloader(cert_file, key_file, ca_file, log.info)
    reloader.start_watch()

    store = Store(os.environ.get("KAALM_MEMORY_DIR", "/var/agent/memory"))
    gateway = GatewayClient(gateway_url, reloader)
    # The Agent exists before the handler so kaalm.complete_task can be its
    # coroutine; /readyz answers 503 until the handler is set below.
    agent = Agent(handler=None, store=store, gateway=gateway, reloader=reloader)

    # Bind the ABI before the handler import: a handler's top-level
    # `import kaalm` must observe bound members. The http client factories
    # close over the reloader so every client they mint follows rotation.
    kaalm._bind(
        gateway=gateway,
        memory=UserMemory(store),
        http_client=functools.partial(make_http_client, reloader),
        http_async_client=functools.partial(make_http_async_client, reloader),
        trace_context=tracecontext.current,
        complete_task=agent.complete_task,
    )

    loaded = loader.load_or_exit(log)
    log.info("handler: %s", loaded.source)
    agent.handler = loaded.handler
    agent.run_task = loaded.run_task
    return agent


async def handle_livez(_: web.Request) -> web.Response:
    """Liveness (contract item 1): the process is up and serving."""
    return web.Response(text="ok")


async def serve(agent: Agent) -> int:
    """Serve until SIGTERM or SIGINT, or until an exitCode task's outcome
    requests the exit; then shut down cleanly and return the exit code."""
    app = web.Application()
    app.router.add_get("/livez", handle_livez)
    app.router.add_get("/readyz", agent.handle_readyz)
    app.router.add_post("/v1/message", agent.handle_v1_message)

    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "0.0.0.0", agent.health_port, ssl_context=agent.reloader.server_context)
    await site.start()
    log.info(
        "serving HTTPS on :%d (task-mode=%s, persistent-memory=%s)",
        agent.health_port, agent.is_task, agent.store.persistent,
    )

    heartbeat: asyncio.Task | None = None
    if agent.should_heartbeat():
        heartbeat = asyncio.create_task(agent.heartbeat_loop())
    task_work = agent.start_task_work()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, agent.stop.set)
    try:
        await agent.stop.wait()
    finally:
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.remove_signal_handler(sig)

    if agent.exit_code is None:
        log.info("SIGTERM received; draining")
    else:
        log.info("exitCode task finished; shutting down with exit code %d", agent.exit_code)
    for background in ([heartbeat] if heartbeat else []) + task_work:
        background.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await background
    await runner.cleanup()
    await agent.gateway.close()
    log.info("shut down cleanly")
    return agent.exit_code or 0


async def main() -> int:
    logging.basicConfig(level=logging.INFO, format="[agent] %(message)s")
    return await serve(build())


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
