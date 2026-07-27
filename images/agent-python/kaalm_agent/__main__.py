# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Kaalm reference base image entrypoint.

Startup order matters and is deliberate: TLS material first (the contract's
transport), then the store and gateway client, then kaalm._bind, and only then
the handler import, so a handler's `import kaalm` always finds bound objects.
A broken configured handler exits nonzero before the server ever binds a port,
so a bad rollout is CrashLoopBackOff, never a half-alive agent.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import os
import signal
from typing import Any

from aiohttp import web

import kaalm

from . import loader
from .gateway import GatewayClient
from .memory import Store, UserMemory
from .tls import CertReloader, peer_san_matches_gateway, workload_is_task

log = logging.getLogger("agent")

HEARTBEAT_PERIOD = 30  # seconds


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

    async def handle_v1_message(self, request: web.Request) -> web.Response:
        # Per-path mTLS enforcement (contract item 4).
        ssl_object = request.transport.get_extra_info("ssl_object") if request.transport else None
        peercert = ssl_object.getpeercert() if ssl_object else None
        if not peercert:
            return web.Response(status=401, text="client certificate required")
        if not peer_san_matches_gateway(ssl_object):
            return web.Response(status=403, text="gateway identity required")

        try:
            envelope = await request.json()
        except Exception:  # noqa: BLE001
            return web.Response(status=400, text="invalid message envelope")

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

    async def complete_task(self, status: str, message: str, artifacts: dict[str, str] | None = None) -> None:
        """Report AgentTask completion, retrying StalePodCompletion.

        Bounded backoff of 100ms, 500ms, 2s (contract item 6); a
        TaskAlreadyCompleted 403 is terminal.
        """
        body = {"status": status, "message": message, "artifacts": artifacts or {}}
        for delay in (0.0, 0.1, 0.5, 2.0):
            if delay:
                await asyncio.sleep(delay)
            reply = await self.gateway.post("/v1/task/complete", json=body)
            text = reply.data if isinstance(reply.data, str) else json.dumps(reply.data)
            if reply.status == 200:
                return
            if reply.status == 403 and "StalePodCompletion" in text:
                continue
            if reply.status == 403 and "TaskAlreadyCompleted" in text:
                log.info("task already completed; exiting")
                return
            raise RuntimeError(f"task completion failed: {reply.status} {text}")
        raise RuntimeError("task completion exhausted retries")


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

    # Bind the ABI before the handler import: a handler's top-level
    # `import kaalm` must observe bound members.
    kaalm._bind(gateway=gateway, memory=UserMemory(store))

    handler, source = loader.load_or_exit(log)
    log.info("handler: %s", source)
    return Agent(handler=handler, store=store, gateway=gateway, reloader=reloader)


async def main() -> None:
    logging.basicConfig(level=logging.INFO, format="[agent] %(message)s")
    agent = build()

    app = web.Application()
    app.router.add_get("/livez", lambda _: web.Response(text="ok"))
    app.router.add_get("/readyz", lambda _: web.Response(text="ok"))
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

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)
    await stop.wait()

    log.info("SIGTERM received; draining")
    if heartbeat:
        heartbeat.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await heartbeat
    await runner.cleanup()
    await agent.gateway.close()
    log.info("shut down cleanly")


if __name__ == "__main__":
    asyncio.run(main())
