# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The rotation-aware httpx factories: transports rebuild exactly on a
reloader generation bump, old transports are closed, and the factories
guard the kwargs that would defeat the machinery. Each behavior holds for
both packages the factories build from, httpx and httpx2."""

from __future__ import annotations

import ssl
import sys

import httpx
import httpx2
import pytest
from aiohttp import web

from kaalm_agent import httpclient
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


PACKAGES = {"httpx": httpx, "httpx2": httpx2}


@pytest.mark.parametrize("package", ["httpx", "httpx2"])
def test_factory_kwargs_pass_through(package):
    lib = PACKAGES[package]
    client = make_http_client(FakeReloader(), package=package, headers={"X-Team": "a"}, timeout=7.0)
    assert isinstance(client, lib.Client)
    assert client.headers["X-Team"] == "a"
    assert client.timeout == lib.Timeout(7.0)
    client.close()


@pytest.mark.parametrize("package", ["httpx", "httpx2"])
@pytest.mark.parametrize("name", ["transport", "verify", "cert"])
def test_reserved_kwargs_are_rejected(name, package):
    with pytest.raises(TypeError, match="owned by the kaalm client factory"):
        make_http_client(FakeReloader(), package=package, **{name: object()})
    with pytest.raises(TypeError, match="owned by the kaalm client factory"):
        make_http_async_client(FakeReloader(), package=package, **{name: object()})


def test_explicit_httpx_package_matches_default():
    sync_client = make_http_client(FakeReloader(), package="httpx")
    assert isinstance(sync_client, httpx.Client)
    assert isinstance(sync_client._transport, _RotatingTransport)
    sync_client.close()

    async_client = make_http_async_client(FakeReloader(), package="httpx")
    assert isinstance(async_client, httpx.AsyncClient)
    assert isinstance(async_client._transport, _AsyncRotatingTransport)


def test_httpx2_factories_return_httpx2_clients_with_default_timeout():
    # The Anthropic SDK 2.0 and later checks isinstance(http_client,
    # httpx2.Client) at construction and raises TypeError otherwise.
    sync_client = make_http_client(FakeReloader(), package="httpx2")
    assert isinstance(sync_client, httpx2.Client)
    assert sync_client.timeout == httpx2.Timeout(DEFAULT_TIMEOUT_SECONDS)
    assert isinstance(sync_client._transport, httpx2.BaseTransport)
    assert not isinstance(sync_client._transport, httpx.BaseTransport)
    sync_client.close()

    async_client = make_http_async_client(FakeReloader(), package="httpx2")
    assert isinstance(async_client, httpx2.AsyncClient)
    assert async_client.timeout == httpx2.Timeout(DEFAULT_TIMEOUT_SECONDS)
    assert isinstance(async_client._transport, httpx2.AsyncBaseTransport)
    assert not isinstance(async_client._transport, httpx.AsyncBaseTransport)


async def test_httpx2_async_client_follows_rotation(aiohttp_server):
    server = await make_server(aiohttp_server)
    reloader = FakeReloader()
    client = make_http_async_client(reloader, package="httpx2")
    transport = client._transport

    url = str(server.make_url("/ping"))
    assert (await client.get(url)).json() == {"ok": True}
    first = transport._inner
    assert isinstance(first, httpx2.AsyncHTTPTransport)
    assert (await client.get(url)).json() == {"ok": True}
    assert transport._inner is first

    reloader.generation += 1
    assert (await client.get(url)).json() == {"ok": True}
    assert transport._inner is not first
    assert isinstance(transport._inner, httpx2.AsyncHTTPTransport)
    await client.aclose()


def test_httpx2_sync_transport_rebuilds_and_closes_on_rotation(monkeypatch):
    sync_cls = httpclient._httpx2_transports()[0]
    reloader = FakeReloader()
    transport = sync_cls(reloader)
    first = transport._transport_for_current_certs()
    assert isinstance(first, httpx2.HTTPTransport)
    assert transport._transport_for_current_certs() is first
    closed = []
    monkeypatch.setattr(first, "close", lambda: closed.append(True))
    reloader.generation += 1
    second = transport._transport_for_current_certs()
    assert second is not first
    assert isinstance(second, httpx2.HTTPTransport)
    assert closed == [True]
    transport.close()


@pytest.mark.parametrize("package", ["requests", None, "HTTPX", httpx])
def test_unknown_package_is_rejected(package):
    with pytest.raises(ValueError, match="package must be"):
        make_http_client(FakeReloader(), package=package)
    with pytest.raises(ValueError, match="package must be"):
        make_http_async_client(FakeReloader(), package=package)


def test_missing_httpx2_is_a_clear_error(monkeypatch):
    httpclient._httpx2_transports.cache_clear()
    monkeypatch.setitem(sys.modules, "httpx2", None)
    try:
        with pytest.raises(ImportError, match="httpx2"):
            make_http_client(FakeReloader(), package="httpx2")
        with pytest.raises(ImportError, match="httpx2"):
            make_http_async_client(FakeReloader(), package="httpx2")
    finally:
        httpclient._httpx2_transports.cache_clear()
