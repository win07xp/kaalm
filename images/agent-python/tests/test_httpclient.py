# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The rotation-aware httpx factories: transports rebuild exactly on a
reloader generation bump, old transports are closed, and the factories
guard the kwargs that would defeat the machinery."""

from __future__ import annotations

import ssl

import httpx
import pytest
from aiohttp import web

from kaalm_agent.httpclient import (
    DEFAULT_TIMEOUT_SECONDS,
    _AsyncRotatingTransport,
    _RotatingTransport,
    make_http_async_client,
    make_http_client,
)


class FakeReloader:
    """Stands in for CertReloader: a real client SSLContext (unused over
    plain HTTP) plus the generation counter the transports watch."""

    def __init__(self):
        self.generation = 1
        self.client_context = ssl.create_default_context()


async def make_server(aiohttp_server):
    async def pong(_: web.Request) -> web.Response:
        return web.json_response({"ok": True})

    app = web.Application()
    app.router.add_get("/ping", pong)
    return await aiohttp_server(app)


def test_sync_transport_reused_within_a_generation():
    transport = _RotatingTransport(FakeReloader())
    first = transport._transport_for_current_certs()
    assert transport._transport_for_current_certs() is first
    transport.close()


def test_sync_transport_rebuilds_and_closes_on_rotation(monkeypatch):
    reloader = FakeReloader()
    transport = _RotatingTransport(reloader)
    first = transport._transport_for_current_certs()
    closed = []
    monkeypatch.setattr(first, "close", lambda: closed.append(True))
    reloader.generation += 1
    second = transport._transport_for_current_certs()
    assert second is not first
    assert closed == [True]
    transport.close()


async def test_async_client_follows_rotation(aiohttp_server):
    server = await make_server(aiohttp_server)
    reloader = FakeReloader()
    client = make_http_async_client(reloader)
    transport = client._transport
    assert isinstance(transport, _AsyncRotatingTransport)

    url = str(server.make_url("/ping"))
    assert (await client.get(url)).json() == {"ok": True}
    first = transport._inner
    assert (await client.get(url)).json() == {"ok": True}
    assert transport._inner is first

    reloader.generation += 1
    assert (await client.get(url)).json() == {"ok": True}
    assert transport._inner is not first
    await client.aclose()


def test_factories_return_httpx_clients_with_default_timeout():
    sync_client = make_http_client(FakeReloader())
    assert isinstance(sync_client, httpx.Client)
    assert sync_client.timeout == httpx.Timeout(DEFAULT_TIMEOUT_SECONDS)
    sync_client.close()

    async_client = make_http_async_client(FakeReloader())
    assert isinstance(async_client, httpx.AsyncClient)
    assert async_client.timeout == httpx.Timeout(DEFAULT_TIMEOUT_SECONDS)


def test_factory_kwargs_pass_through():
    client = make_http_client(FakeReloader(), headers={"X-Team": "a"}, timeout=7.0)
    assert client.headers["X-Team"] == "a"
    assert client.timeout == httpx.Timeout(7.0)
    client.close()


@pytest.mark.parametrize("name", ["transport", "verify", "cert"])
def test_reserved_kwargs_are_rejected(name):
    with pytest.raises(TypeError, match="owned by the kaalm client factory"):
        make_http_client(FakeReloader(), **{name: object()})
    with pytest.raises(TypeError, match="owned by the kaalm client factory"):
        make_http_async_client(FakeReloader(), **{name: object()})
