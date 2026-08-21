# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The preconfigured gateway client behind kaalm.gateway.

One client for everything that talks to $KAALM_GATEWAY_ENDPOINT: handler LLM
calls, the heartbeat loop, and task completion. It carries the Pod's mTLS
identity and CA trust, and rebuilds its session whenever the cert reloader
reports a new generation, so a rotation reaches outbound calls without a
restart (the session otherwise snapshots the SSL context it was built with).
"""

from __future__ import annotations

import asyncio
from typing import Any, NamedTuple

import aiohttp

from . import tracecontext


class GatewayReply(NamedTuple):
    """status plus the parsed JSON body (or raw text when not JSON)."""

    status: int
    data: Any


class GatewayClient:
    def __init__(self, base_url: str, reloader: Any, timeout_seconds: float = 300.0):
        self._base_url = base_url.rstrip("/")
        self._reloader = reloader
        self._timeout = aiohttp.ClientTimeout(total=timeout_seconds)
        self._session: aiohttp.ClientSession | None = None
        self._generation: int | None = None
        self._lock = asyncio.Lock()

    async def _session_for_current_certs(self) -> aiohttp.ClientSession:
        async with self._lock:
            generation = getattr(self._reloader, "generation", 0)
            if self._session is None or self._session.closed or generation != self._generation:
                if self._session is not None and not self._session.closed:
                    await self._session.close()
                connector = aiohttp.TCPConnector(ssl=self._reloader.client_context)
                self._session = aiohttp.ClientSession(connector=connector, timeout=self._timeout)
                self._generation = generation
            return self._session

    async def request(self, method: str, path: str, **kwargs: Any) -> GatewayReply:
        session = await self._session_for_current_certs()
        # The handled message's trace context rides every gateway call;
        # caller-supplied headers win on collision.
        headers = {**tracecontext.current(), **dict(kwargs.pop("headers", None) or {})}
        async with session.request(method, self._base_url + path, headers=headers, **kwargs) as resp:
            if resp.content_type == "application/json":
                data: Any = await resp.json()
            else:
                data = await resp.text()
            return GatewayReply(resp.status, data)

    async def post(self, path: str, json: Any = None, **kwargs: Any) -> GatewayReply:
        return await self.request("POST", path, json=json, **kwargs)

    async def get(self, path: str, **kwargs: Any) -> GatewayReply:
        return await self.request("GET", path, **kwargs)

    async def close(self) -> None:
        async with self._lock:
            if self._session is not None and not self._session.closed:
                await self._session.close()
            self._session = None
