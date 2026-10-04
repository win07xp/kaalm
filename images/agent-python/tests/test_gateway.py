# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""GatewayClient against a real aiohttp server: reply parsing, session reuse,
and the rotation contract (a reloader generation bump rebuilds the session)."""

from __future__ import annotations

from aiohttp import web

from kaalm_agent.gateway import GatewayClient, GatewayReply


class FakeReloader:
    """Stands in for CertReloader over plain HTTP: no TLS, just a generation."""

    def __init__(self):
        self.generation = 1
        self.client_context = None  # aiohttp: ssl=None means default/plain


async def make_server(aiohttp_server):
    async def echo_json(request: web.Request) -> web.Response:
        body = await request.json()
        return web.json_response({"got": body})

    async def plain_text(_: web.Request) -> web.Response:
        return web.Response(status=503, text="upstream sad")

    async def unavailable(_: web.Request) -> web.Response:
        return web.json_response(
            {"error": {"type": "internal_unavailable", "retryable": True}},
            status=503, headers={"Retry-After": "1"})

    async def unavailable_dated(_: web.Request) -> web.Response:
        return web.json_response(
            {"error": {"type": "internal_unavailable", "retryable": True}},
            status=503, headers={"Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT"})

    app = web.Application()
    app.router.add_post("/v1/chat", echo_json)
    app.router.add_get("/text", plain_text)
    app.router.add_post("/v1/task/complete", unavailable)
    app.router.add_post("/dated", unavailable_dated)
    return await aiohttp_server(app)


async def test_post_parses_json_reply(aiohttp_server):
    server = await make_server(aiohttp_server)
    client = GatewayClient(str(server.make_url("")), FakeReloader())
    reply = await client.post("/v1/chat", json={"model": "m"})
    assert reply.status == 200
    assert reply.data == {"got": {"model": "m"}}
    await client.close()


async def test_non_json_reply_is_text(aiohttp_server):
    server = await make_server(aiohttp_server)
    client = GatewayClient(str(server.make_url("")), FakeReloader())
    reply = await client.get("/text")
    assert reply.status == 503
    assert reply.data == "upstream sad"
    await client.close()


async def test_session_reused_within_a_generation(aiohttp_server):
    server = await make_server(aiohttp_server)
    client = GatewayClient(str(server.make_url("")), FakeReloader())
    await client.post("/v1/chat", json={})
    first = client._session
    await client.post("/v1/chat", json={})
    assert client._session is first
    await client.close()


async def test_generation_bump_rebuilds_session(aiohttp_server):
    """The rotation contract from docs/src/runtime/base-images.md: kaalm.gateway
    is kept current by the rotation watch, so new certs must reach new
    connections without a process restart."""
    server = await make_server(aiohttp_server)
    reloader = FakeReloader()
    client = GatewayClient(str(server.make_url("")), reloader)
    await client.post("/v1/chat", json={})
    first = client._session
    reloader.generation += 1  # a rotation happened
    await client.post("/v1/chat", json={})
    assert client._session is not first
    assert first.closed  # the stale session was closed, not leaked
    await client.close()


async def test_close_is_idempotent(aiohttp_server):
    server = await make_server(aiohttp_server)
    client = GatewayClient(str(server.make_url("")), FakeReloader())
    await client.post("/v1/chat", json={})
    await client.close()
    await client.close()


async def test_reply_carries_retry_after_seconds(aiohttp_server):
    server = await make_server(aiohttp_server)
    client = GatewayClient(str(server.make_url("")), FakeReloader())
    reply = await client.post("/v1/task/complete", json={"status": "success"})
    assert reply.status == 503
    assert reply.retry_after == 1.0
    assert (await client.get("/text")).retry_after is None
    assert (await client.post("/v1/chat", json={})).retry_after is None
    # Only integer delta-seconds is read; an HTTP-date counts as absent.
    assert (await client.post("/dated")).retry_after is None
    await client.close()


def test_reply_still_unpacks_as_status_and_data():
    reply = GatewayReply(200, "", retry_after=2.0)
    status, data = reply
    assert (status, data) == (200, "")
    assert GatewayReply(200, "") == (200, "")
    assert GatewayReply(200, "").retry_after is None
    assert reply._replace(status=503).retry_after is None
