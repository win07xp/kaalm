# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The handler-facing ABI of the Kaalm reference base images.

Exactly five members (docs/src/runtime/base-images.md), and the surface is
append-only within a minor release series:

- ``kaalm.gateway``: a preconfigured client for $KAALM_GATEWAY_ENDPOINT
  carrying the Pod's mTLS identity and CA trust.
- ``kaalm.memory``: the runtime's persistent store, confined to the ``user/``
  key prefix. Backed by the PVC when persistence is enabled, in-memory
  otherwise.
- ``kaalm.http_client()`` / ``kaalm.http_async_client()``: factories for
  httpx.Client / httpx.AsyncClient objects carrying the same mTLS identity
  and CA trust, rebuilt internally on certificate rotation. Their names
  mirror the ``http_client=`` / ``http_async_client=`` keyword arguments the
  framework SDKs take; extra keyword arguments pass through to the httpx
  constructor. Since v0.4.0.
- ``kaalm.trace_context()``: the W3C trace context of the message being
  handled, as a ``{"traceparent": ..., "tracestate": ...}`` dict (empty
  outside message handling). The runtime already forwards it on every
  gateway call the ABI clients make; handlers running their own
  OpenTelemetry SDK continue the trace from these values. Since v0.5.0.

The runtime binds all members before the handler is imported; importing this
module anywhere else raises, on first attribute access, rather than handing
out half-configured objects.
"""

from __future__ import annotations

from typing import Any

_bound: dict[str, Any] = {}

_MEMBERS = ("gateway", "http_async_client", "http_client", "memory", "trace_context")


def _bind(*, gateway: Any, memory: Any, http_client: Any, http_async_client: Any, trace_context: Any) -> None:
    """Called once by the runtime at startup, before the handler is imported."""
    _bound["gateway"] = gateway
    _bound["memory"] = memory
    _bound["http_client"] = http_client
    _bound["http_async_client"] = http_async_client
    _bound["trace_context"] = trace_context


def __getattr__(name: str) -> Any:
    if name in _MEMBERS:
        try:
            return _bound[name]
        except KeyError:
            raise RuntimeError(
                f"kaalm.{name} is only available inside a running Kaalm base "
                "image runtime (the runtime binds it before importing the handler)"
            ) from None
    raise AttributeError(f"module 'kaalm' has no attribute {name!r}")


def __dir__() -> list[str]:
    return sorted(_MEMBERS)
