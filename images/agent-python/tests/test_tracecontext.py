# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Trace-context propagation: the capture primitive, and the injection every
ABI client performs (the gateway client and both httpx transports)."""

from __future__ import annotations

import asyncio

from aiohttp import web

import httpx

from kaalm_agent import tracecontext
from kaalm_agent.gateway import GatewayClient
from kaalm_agent.httpclient import _apply_trace_context, make_http_async_client


class GatewayFakeReloader:
    """CertReloader stand-in over plain HTTP (aiohttp: ssl=None)."""

    def __init__(self):
        self.generation = 1
        self.client_context = None


class HttpxFakeReloader:
    """CertReloader stand-in for the httpx transports."""

    def __init__(self):
        import ssl

        self.generation = 1
        self.client_context = ssl.create_default_context()

PARENT = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"


def test_capture_and_clear():
    tracecontext.set_from_headers({"traceparent": PARENT, "tracestate": "kaalm=1"})
    assert tracecontext.current() == {"traceparent": PARENT, "tracestate": "kaalm=1"}
    # a delivery without context clears the previous capture
    tracecontext.set_from_headers({})
    assert tracecontext.current() == {}


def test_tracestate_alone_is_not_a_context():
    tracecontext.set_from_headers({"tracestate": "kaalm=1"})
    assert tracecontext.current() == {"tracestate": "kaalm=1"}
    tracecontext.set_from_headers({})


def test_capture_is_task_confined():
    async def run():
        async def handled(parent: str) -> dict[str, str]:
            tracecontext.set_from_headers({"traceparent": parent})
            await asyncio.sleep(0)
            return tracecontext.current()

        first, second = await asyncio.gather(handled(PARENT), handled("00-b" + PARENT[4:]))
        assert first == {"traceparent": PARENT}
        assert second == {"traceparent": "00-b" + PARENT[4:]}
        # the launching task never saw either capture
        assert tracecontext.current() == {}

    asyncio.run(run())


async def _header_server(aiohttp_server):
    async def echo_headers(request: web.Request) -> web.Response:
        return web.json_response({
            "traceparent": request.headers.get("traceparent", ""),
            "tracestate": request.headers.get("tracestate", ""),
        })

    app = web.Application()
    app.router.add_route("*", "/echo", echo_headers)
    return await aiohttp_server(app)


async def test_gateway_client_injects_trace_context(aiohttp_server):
    server = await _header_server(aiohttp_server)
    client = GatewayClient(str(server.make_url("")), GatewayFakeReloader())
    try:
        tracecontext.set_from_headers({"traceparent": PARENT, "tracestate": "kaalm=1"})
        reply = await client.post("/echo", json={})
        assert reply.data == {"traceparent": PARENT, "tracestate": "kaalm=1"}

        tracecontext.set_from_headers({})
        reply = await client.post("/echo", json={})
        assert reply.data == {"traceparent": "", "tracestate": ""}
    finally:
        tracecontext.set_from_headers({})
        await client.close()


async def test_gateway_client_caller_headers_win(aiohttp_server):
    server = await _header_server(aiohttp_server)
    client = GatewayClient(str(server.make_url("")), GatewayFakeReloader())
    try:
        tracecontext.set_from_headers({"traceparent": PARENT})
        reply = await client.post("/echo", json={}, headers={"traceparent": "00-caller"})
        assert reply.data["traceparent"] == "00-caller"
    finally:
        tracecontext.set_from_headers({})
        await client.close()


async def test_httpx_async_transport_injects(aiohttp_server):
    server = await _header_server(aiohttp_server)
    client = make_http_async_client(HttpxFakeReloader())
    try:
        tracecontext.set_from_headers({"traceparent": PARENT})
        resp = await client.get(str(server.make_url("/echo")))
        assert resp.json()["traceparent"] == PARENT
        # explicit request headers win
        resp = await client.get(str(server.make_url("/echo")), headers={"traceparent": "00-caller"})
        assert resp.json()["traceparent"] == "00-caller"
    finally:
        tracecontext.set_from_headers({})
        await client.aclose()


def test_httpx_sync_injection_primitive():
    # The sync transport shares _apply_trace_context with the async one;
    # exercise the primitive directly on a request object.
    request = httpx.Request("GET", "http://gateway/echo")
    tracecontext.set_from_headers({"traceparent": PARENT, "tracestate": "kaalm=1"})
    try:
        _apply_trace_context(request)
        assert request.headers["traceparent"] == PARENT
        assert request.headers["tracestate"] == "kaalm=1"

        held = httpx.Request("GET", "http://gateway/echo", headers={"traceparent": "00-caller"})
        _apply_trace_context(held)
        assert held.headers["traceparent"] == "00-caller"
    finally:
        tracecontext.set_from_headers({})
