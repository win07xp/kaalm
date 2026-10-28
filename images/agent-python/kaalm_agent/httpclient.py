# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Rotation-aware HTTP clients behind kaalm.http_client / http_async_client.

Framework SDKs (LangChain, the OpenAI and Anthropic clients) accept a
standard httpx or httpx2 client object, but a client built by hand snapshots
its SSL context at construction and keeps presenting the same leaf
certificate for the life of the process. Kaalm rotates leaf certificates on
disk mid-pod (90d duration, renewed 30d early), so a long-lived agent with a
hand-built client eventually presents an expired certificate while its
health probes stay green.

These factories close that edge the same way kaalm.gateway does: a custom
transport compares the cert reloader's generation on every request and
rebuilds its inner transport when a rotation has landed. The handler builds
a client once and keeps it forever.

The factories build from httpx by default, or from httpx2 with
package="httpx2": the Anthropic SDK 2.0 and later accepts only httpx2
clients. The rotation logic is shared; each package gets its own transport
classes, because each client calls its transport's __enter__/__exit__,
which only that package's BaseTransport provides.
"""

from __future__ import annotations

import functools
import threading
from typing import Any

import httpx

from . import tracecontext

DEFAULT_TIMEOUT_SECONDS = 300.0

# The factories own these: both feed the SSL context, and a caller supplying
# either would silently defeat the rotation machinery.
_RESERVED_KWARGS = ("transport", "verify", "cert")


class _RotatingTransportLogic:
    """Delegates to an inner HTTPTransport rebuilt on cert rotation.

    _lib is the HTTP package (httpx or httpx2) the inner transport comes from.
    """

    _lib: Any

    def __init__(self, reloader: Any):
        self._reloader = reloader
        self._lock = threading.Lock()
        self._inner: Any = None
        self._generation: int | None = None

    def _transport_for_current_certs(self) -> Any:
        with self._lock:
            generation = getattr(self._reloader, "generation", 0)
            if self._inner is None or generation != self._generation:
                if self._inner is not None:
                    self._inner.close()
                self._inner = self._lib.HTTPTransport(verify=self._reloader.client_context)
                self._generation = generation
            return self._inner

    def handle_request(self, request: Any) -> Any:
        _apply_trace_context(request)
        # Return the inner response untouched: constructing a new Response
        # would detach its stream and break SSE bodies.
        return self._transport_for_current_certs().handle_request(request)

    def close(self) -> None:
        with self._lock:
            if self._inner is not None:
                self._inner.close()
            self._inner = None


class _AsyncRotatingTransportLogic:
    """The async twin of _RotatingTransportLogic."""

    _lib: Any

    def __init__(self, reloader: Any):
        self._reloader = reloader
        self._lock = threading.Lock()
        self._inner: Any = None
        self._generation: int | None = None

    async def _transport_for_current_certs(self) -> Any:
        old: Any = None
        with self._lock:
            generation = getattr(self._reloader, "generation", 0)
            if self._inner is None or generation != self._generation:
                old = self._inner
                self._inner = self._lib.AsyncHTTPTransport(verify=self._reloader.client_context)
                self._generation = generation
            inner = self._inner
        if old is not None:
            await old.aclose()
        return inner

    async def handle_async_request(self, request: Any) -> Any:
        _apply_trace_context(request)
        transport = await self._transport_for_current_certs()
        return await transport.handle_async_request(request)

    async def aclose(self) -> None:
        with self._lock:
            inner, self._inner = self._inner, None
        if inner is not None:
            await inner.aclose()


class _RotatingTransport(_RotatingTransportLogic, httpx.BaseTransport):
    """The httpx rotating transport."""

    _lib = httpx


class _AsyncRotatingTransport(_AsyncRotatingTransportLogic, httpx.AsyncBaseTransport):
    """The httpx async rotating transport."""

    _lib = httpx


@functools.cache
def _httpx2_transports() -> tuple[type, type, Any]:
    """The httpx2 rotating transports and the httpx2 module, built on first use.

    httpx2 is imported here, not at module load, so a missing package fails
    only the calls that ask for it, with an error that names it.
    """
    try:
        import httpx2
    except ModuleNotFoundError as exc:
        raise ImportError(
            "package='httpx2' needs the httpx2 package; the kaalm-agent-python "
            "image bundles it, install it in this environment"
        ) from exc

    class _Httpx2RotatingTransport(_RotatingTransportLogic, httpx2.BaseTransport):
        """The httpx2 rotating transport."""

        _lib = httpx2

    class _AsyncHttpx2RotatingTransport(_AsyncRotatingTransportLogic, httpx2.AsyncBaseTransport):
        """The httpx2 async rotating transport."""

        _lib = httpx2

    return _Httpx2RotatingTransport, _AsyncHttpx2RotatingTransport, httpx2


def _transports_for(package: str) -> tuple[type, type, Any]:
    """(sync transport class, async transport class, package module)."""
    if package == "httpx":
        return _RotatingTransport, _AsyncRotatingTransport, httpx
    if package == "httpx2":
        return _httpx2_transports()
    raise ValueError(f"package must be 'httpx' or 'httpx2', got {package!r}")


def _apply_trace_context(request: Any) -> None:
    """Copy the handled message's trace context onto the outbound request.

    Works on an httpx or an httpx2 request. Explicit caller headers win;
    outside message handling this is a no-op.
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


def make_http_client(reloader: Any, *, package: str = "httpx", **kwargs: Any) -> Any:
    """A Client carrying the Pod's mTLS identity, surviving rotation.

    package picks the HTTP library: "httpx" (the default) returns an
    httpx.Client, "httpx2" an httpx2.Client (the Anthropic SDK 2.0 and later
    accepts only httpx2 clients). Remaining keyword arguments pass through to
    that package's Client (headers, auth, timeout, follow_redirects, ...);
    the reserved ones are rejected for both packages.
    """
    sync_cls, _, lib = _transports_for(package)
    _check_kwargs(kwargs)
    return lib.Client(transport=sync_cls(reloader), **kwargs)


def make_http_async_client(reloader: Any, *, package: str = "httpx", **kwargs: Any) -> Any:
    """The async twin of make_http_client: an httpx or httpx2 AsyncClient."""
    _, async_cls, lib = _transports_for(package)
    _check_kwargs(kwargs)
    return lib.AsyncClient(transport=async_cls(reloader), **kwargs)
