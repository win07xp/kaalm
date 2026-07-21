# Copyright 2026. Licensed under the Apache License, Version 2.0.
"""Minimal Kaalm runtime-contract agent using aiohttp.

Copy this directory and replace handle_message in handler.py. Everything else
is contract boilerplate: HTTPS serving, mTLS, cert reload, dedup, heartbeats,
and task completion. See docs/src/runtime/starter-templates.md.
"""

from __future__ import annotations

import asyncio
import collections
import contextlib
import json
import logging
import os
import signal
from typing import Any

import aiohttp
from aiohttp import web

from .handler import handle_message
from .tls import CertReloader, peer_san_matches_gateway, workload_is_task

log = logging.getLogger("agent")

DEDUP_SIZE = 1024
HEARTBEAT_PERIOD = 30  # seconds


class Agent:
    def __init__(self) -> None:
        self.health_port = int(os.environ.get("KAALM_HEALTH_PORT", "8080"))
        self.cert_file = os.environ.get("KAALM_TLS_CERT", "/var/run/kaalm/tls.crt")
        self.key_file = os.environ.get("KAALM_TLS_KEY", "/var/run/kaalm/tls.key")
        self.ca_file = os.environ.get("KAALM_CA_CERT", "/var/run/kaalm/ca.crt")
        self.gateway_url = os.environ.get("KAALM_GATEWAY_ENDPOINT", "").rstrip("/")

        self.reloader = CertReloader(self.cert_file, self.key_file, self.ca_file, log.info)
        self.reloader.start_watch()
        self.is_task = workload_is_task(self.cert_file)
        # LRU dedup of messageId -> cached response (contract item 7).
        self._dedup: "collections.OrderedDict[str, dict[str, Any]]" = collections.OrderedDict()

    def _dedup_get(self, message_id: str) -> dict[str, Any] | None:
        cached = self._dedup.get(message_id)
        if cached is not None:
            self._dedup.move_to_end(message_id)
        return cached

    def _dedup_put(self, message_id: str, reply: dict[str, Any]) -> None:
        self._dedup[message_id] = reply
        self._dedup.move_to_end(message_id)
        while len(self._dedup) > DEDUP_SIZE:
            self._dedup.popitem(last=False)

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

        message_id = envelope.get("messageId", "")
        cached = self._dedup_get(message_id)
        if cached is not None:
            return web.json_response(cached)

        reply = await handle_message(self, envelope)
        self._dedup_put(message_id, reply)
        return web.json_response(reply)

    async def heartbeat_loop(self) -> None:
        connector = aiohttp.TCPConnector(ssl=self.reloader.client_context)
        async with aiohttp.ClientSession(connector=connector) as session:
            while True:
                await asyncio.sleep(HEARTBEAT_PERIOD)
                try:
                    async with session.post(f"{self.gateway_url}/v1/agent/heartbeat") as resp:
                        await resp.read()
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
        body = json.dumps({"status": status, "message": message, "artifacts": artifacts or {}})
        connector = aiohttp.TCPConnector(ssl=self.reloader.client_context)
        async with aiohttp.ClientSession(connector=connector) as session:
            for delay in (0.0, 0.1, 0.5, 2.0):
                if delay:
                    await asyncio.sleep(delay)
                async with session.post(
                    f"{self.gateway_url}/v1/task/complete",
                    data=body,
                    headers={"Content-Type": "application/json"},
                ) as resp:
                    text = await resp.text()
                    if resp.status == 200:
                        return
                    if resp.status == 403 and "StalePodCompletion" in text:
                        continue
                    if resp.status == 403 and "TaskAlreadyCompleted" in text:
                        log.info("task already completed; exiting")
                        return
                    raise RuntimeError(f"task completion failed: {resp.status} {text}")
            raise RuntimeError("task completion exhausted retries")


async def main() -> None:
    logging.basicConfig(level=logging.INFO, format="[agent] %(message)s")
    agent = Agent()

    app = web.Application()
    app.router.add_get("/livez", lambda _: web.Response(text="ok"))
    app.router.add_get("/readyz", lambda _: web.Response(text="ok"))
    app.router.add_post("/v1/message", agent.handle_v1_message)

    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "0.0.0.0", agent.health_port, ssl_context=agent.reloader.server_context)
    await site.start()
    log.info("serving HTTPS on :%d (task-mode=%s)", agent.health_port, agent.is_task)

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
    log.info("shut down cleanly")


if __name__ == "__main__":
    asyncio.run(main())
