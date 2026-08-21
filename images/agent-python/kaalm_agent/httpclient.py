# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Rotation-aware httpx clients behind kaalm.http_client / http_async_client.

Framework SDKs (LangChain, the OpenAI and Anthropic clients) accept a
standard httpx client object, but a client built by hand snapshots its SSL
context at construction and keeps presenting the same leaf certificate for
the life of the process. Kaalm rotates leaf certificates on disk mid-pod
(90d duration, renewed 30d early), so a long-lived agent with a hand-built
client eventually presents an expired certificate while its health probes
stay green.

These factories close that edge the same way kaalm.gateway does: a custom
transport compares the cert reloader's generation on every request and
rebuilds its inner httpx transport when a rotation has landed. The handler
builds a client once and keeps it forever.
"""

from __future__ import annotations

import threading
from typing import Any

import httpx

from . import tracecontext

DEFAULT_TIMEOUT_SECONDS = 300.0

# The factories own these: both feed the SSL context, and a caller supplying
# either would silently defeat the rotation machinery.
_RESERVED_KWARGS = ("transport", "verify", "cert")


class _RotatingTransport(httpx.BaseTransport):
    """Delegates to an inner HTTPTransport rebuilt on cert rotation."""

    def __init__(self, reloader: Any):
        self._reloader = reloader
        self._lock = threading.Lock()
        self._inner: httpx.HTTPTransport | None = None
        self._generation: int | None = None

    def _transport_for_current_certs(self) -> httpx.HTTPTransport:
        with self._lock:
            generation = getattr(self._reloader, "generation", 0)
            if self._inner is None or generation != self._generation:
                if self._inner is not None:
                    self._inner.close()
                self._inner = httpx.HTTPTransport(verify=self._reloader.client_context)
                self._generation = generation
            return self._inner

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        _apply_trace_context(request)
        # Return the inner response untouched: constructing a new Response
        # would detach its stream and break SSE bodies.
        return self._transport_for_current_certs().handle_request(request)

    def close(self) -> None:
        with self._lock:
            if self._inner is not None:
                self._inner.close()
            self._inner = None


class _AsyncRotatingTransport(httpx.AsyncBaseTransport):
    """The async twin of _RotatingTransport."""

    def __init__(self, reloader: Any):
        self._reloader = reloader
        self._lock = threading.Lock()
        self._inner: httpx.AsyncHTTPTransport | None = None
        self._generation: int | None = None

    async def _transport_for_current_certs(self) -> httpx.AsyncHTTPTransport:
        old: httpx.AsyncHTTPTransport | None = None
        with self._lock:
            generation = getattr(self._reloader, "generation", 0)
            if self._inner is None or generation != self._generation:
                old = self._inner
                self._inner = httpx.AsyncHTTPTransport(verify=self._reloader.client_context)
                self._generation = generation
            inner = self._inner
        if old is not None:
            await old.aclose()
        return inner

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        _apply_trace_context(request)
        transport = await self._transport_for_current_certs()
        return await transport.handle_async_request(request)

    async def aclose(self) -> None:
        with self._lock:
            inner, self._inner = self._inner, None
        if inner is not None:
            await inner.aclose()


def _apply_trace_context(request: httpx.Request) -> None:
    """Copy the handled message's trace context onto the outbound request.

    Explicit caller headers win; outside message handling this is a no-op.
    """
    for name, value in tracecontext.current().items():
        if name not in request.headers:
            request.headers[name] = value


def _check_kwargs(kwargs: dict[str, Any]) -> None:
    for name in _RESERVED_KWARGS:
        if name in kwargs:
            raise TypeError(
                f"{name!r} is owned by the kaalm client factory: the transport and "
                "TLS material come from the runtime's rotation-aware machinery"
            )
    kwargs.setdefault("timeout", DEFAULT_TIMEOUT_SECONDS)
    # Proxy env vars would mount proxy transports AROUND the rotation-aware
    # transport, silently dropping the mTLS identity. Explicit opt-in only.
    kwargs.setdefault("trust_env", False)


def make_http_client(reloader: Any, **kwargs: Any) -> httpx.Client:
    """An httpx.Client carrying the Pod's mTLS identity, surviving rotation.

    Remaining keyword arguments pass through to httpx.Client (headers, auth,
    timeout, follow_redirects, ...).
    """
    _check_kwargs(kwargs)
    return httpx.Client(transport=_RotatingTransport(reloader), **kwargs)


def make_http_async_client(reloader: Any, **kwargs: Any) -> httpx.AsyncClient:
    """The async twin of make_http_client."""
    _check_kwargs(kwargs)
    return httpx.AsyncClient(transport=_AsyncRotatingTransport(reloader), **kwargs)
