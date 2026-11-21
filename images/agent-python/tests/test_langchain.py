# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""kaalm.langchain_chat_anthropic: a ChatAnthropic whose Anthropic SDK clients
are the rotation-aware httpx2 clients, with gateway defaults, pass-through
arguments, and loud failures when LangChain is absent or its private client
properties change."""

from __future__ import annotations

import asyncio
import ssl
import sys

import httpx2
import pytest
from aiohttp import web
from langchain_anthropic import ChatAnthropic

import kaalm
from kaalm_agent import __main__ as runtime
from kaalm_agent import httpclient, langchain
from kaalm_agent.httpclient import make_http_async_client, make_http_client
from kaalm_agent.langchain import make_chat_anthropic

GATEWAY = "https://gw:8443"


class FakeReloader:
    """Stands in for CertReloader: a real client SSLContext (unused over
    plain HTTP) plus the generation counter the transports watch."""

    def __init__(self):
        self.generation = 1
        self.client_context = ssl.create_default_context()


class Factories:
    """The kaalm client factories, recording every client they mint."""

    def __init__(self):
        self.reloader = FakeReloader()
        self.sync_clients: list = []
        self.async_clients: list = []

    def http_client(self, **kwargs):
        client = make_http_client(self.reloader, **kwargs)
        self.sync_clients.append(client)
        return client

    def http_async_client(self, **kwargs):
        client = make_http_async_client(self.reloader, **kwargs)
        self.async_clients.append(client)
        return client

    def model(self, gateway_url: str = GATEWAY, **kwargs):
        return make_chat_anthropic(self.http_client, self.http_async_client, gateway_url, **kwargs)


@pytest.fixture(autouse=True)
def _fresh_model_class():
    """Each test sees a fresh import of langchain_anthropic and a fresh check
    of its private properties."""
    langchain._model_class.cache_clear()
    yield
    langchain._model_class.cache_clear()


def test_clients_are_the_kaalm_httpx2_clients():
    f = Factories()
    model = f.model(model="anthropic-shared/claude-sonnet-4-6")
    assert isinstance(model, ChatAnthropic)

    sync_transport, async_transport, _ = httpclient._httpx2_transports()
    sync_http = model._client._client
    async_http = model._async_client._client
    assert f.sync_clients == [sync_http]
    assert f.async_clients == [async_http]
    assert isinstance(sync_http, httpx2.Client)
    assert isinstance(async_http, httpx2.AsyncClient)
    assert isinstance(sync_http._transport, sync_transport)
    assert isinstance(async_http._transport, async_transport)


def test_defaults_point_at_the_gateway():
    model = Factories().model(model="m")
    assert model.anthropic_api_url == GATEWAY
    assert model.anthropic_api_key.get_secret_value() == "managed-by-kaalm"
    assert model._client_params["base_url"] == GATEWAY


def test_explicit_arguments_pass_through():
    model = Factories().model(
        model="m", base_url="https://other", api_key="k", max_retries=0,
        default_headers={"X-Team": "a"}, timeout=7,
    )
    params = model._client_params
    assert params["base_url"] == "https://other"
    assert params["api_key"] == "k"
    assert params["max_retries"] == 0
    assert params["default_headers"]["X-Team"] == "a"
    assert params["timeout"] == 7


def test_field_name_spellings_pass_through_without_defaults(monkeypatch):
    """ChatAnthropic accepts the field names too; the factory adds no default
    beside them, so pydantic never sees both spellings."""
    model = Factories().model(model="m", anthropic_api_url="https://other", anthropic_api_key="k")
    assert model.anthropic_api_url == "https://other"
    assert model.anthropic_api_key.get_secret_value() == "k"

    # ChatAnthropic resolves both spellings without an error, so the model's
    # fields cannot show an added default: check what the factory passes.
    passed: dict = {}
    with monkeypatch.context() as m:
        m.setattr(langchain, "_model_class", lambda *_: lambda **kw: passed.update(kw))
        Factories().model(model="m", anthropic_api_url="https://other", anthropic_api_key="k")
    assert passed == {"model": "m", "anthropic_api_url": "https://other", "anthropic_api_key": "k"}


MESSAGE = {
    "id": "msg_1", "type": "message", "role": "assistant", "model": "m",
    "content": [{"type": "text", "text": "hello"}],
    "stop_reason": "end_turn", "stop_sequence": None,
    "usage": {"input_tokens": 1, "output_tokens": 1},
}


async def test_sync_and_async_round_trip(aiohttp_server):
    seen: list = []

    async def messages(request: web.Request) -> web.Response:
        # The SDK sends the key as x-api-key or as a bearer token, by version.
        seen.append(request.headers.get("x-api-key") or request.headers.get("Authorization"))
        return web.json_response(MESSAGE)

    app = web.Application()
    app.router.add_post("/v1/messages", messages)
    server = await aiohttp_server(app)

    f = Factories()
    model = f.model(str(server.make_url("")).rstrip("/"), model="m")
    # The sync client must not block the loop the server runs on.
    sync_reply = await asyncio.to_thread(model.invoke, "hi")
    async_reply = await model.ainvoke("hi")

    assert sync_reply.content == "hello"
    assert async_reply.content == "hello"
    assert len(seen) == 2
    assert all(value.endswith("managed-by-kaalm") for value in seen)
    # Both requests went through the kaalm rotating transports.
    assert f.sync_clients[0]._transport._inner is not None
    assert f.async_clients[0]._transport._inner is not None


def test_missing_package_raises_import_error_with_hint(monkeypatch):
    monkeypatch.setitem(sys.modules, "langchain_anthropic", None)
    with pytest.raises(ImportError, match="pip install langchain-anthropic"):
        Factories().model(model="m")


def test_other_missing_module_is_not_blamed_on_langchain(monkeypatch):
    """A missing dependency of langchain_anthropic surfaces unchanged, so the
    hint never names the wrong package."""
    # Drop the submodules too, so the import of langchain_anthropic itself
    # reaches the missing anthropic package.
    for name in [n for n in sys.modules if n == "langchain_anthropic" or n.startswith("langchain_anthropic.")]:
        monkeypatch.delitem(sys.modules, name)
    monkeypatch.setitem(sys.modules, "anthropic", None)
    with pytest.raises(ModuleNotFoundError) as excinfo:
        Factories().model(model="m")
    assert excinfo.value.name == "anthropic"
    assert "pip install langchain-anthropic" not in str(excinfo.value)


@pytest.mark.parametrize("name", ["_client", "_async_client", "_client_params"])
def test_private_property_drift_raises(monkeypatch, name):
    monkeypatch.setattr(ChatAnthropic, name, property(lambda self: None))
    with pytest.raises(RuntimeError, match=name):
        Factories().model(model="m")


def test_anthropic_proxy_is_rejected(monkeypatch):
    """Rejected before LangChain is imported, so the error is the same with
    or without the package."""
    monkeypatch.setitem(sys.modules, "langchain_anthropic", None)
    with pytest.raises(TypeError, match="anthropic_proxy"):
        Factories().model(model="m", anthropic_proxy="http://proxy:3128")


def test_anthropic_proxy_environment_is_ignored(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_PROXY", "http://proxy:3128")
    f = Factories()
    model = f.model(model="m")
    assert model.anthropic_proxy == "http://proxy:3128"  # ChatAnthropic read it...
    assert model._client._client is f.sync_clients[0]  # ...and the clients ignore it
    assert model._async_client._client is f.async_clients[0]
    assert f.sync_clients[0]._mounts == {}
    assert f.async_clients[0]._mounts == {}


def test_build_binds_the_factory_with_the_gateway_url(tmp_path, monkeypatch, workload_cert):
    cert, key = workload_cert
    monkeypatch.setenv("KAALM_TLS_CERT", str(cert))
    monkeypatch.setenv("KAALM_TLS_KEY", str(key))
    monkeypatch.setenv("KAALM_CA_CERT", str(cert))
    monkeypatch.setenv("KAALM_MEMORY_DIR", str(tmp_path / "memory"))
    monkeypatch.setenv("KAALM_GATEWAY_ENDPOINT", "https://gw.example:8443/")

    runtime.build()
    model = kaalm.langchain_chat_anthropic(model="m")

    assert model.anthropic_api_url == "https://gw.example:8443"
    assert isinstance(model._client._client, httpx2.Client)
    assert isinstance(model._async_client._client, httpx2.AsyncClient)
