# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The handler-facing ABI of the Kaalm reference base images.

Exactly seven members (docs/src/runtime/base-images.md), and the surface is
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
- ``await kaalm.complete_task(status, message="", artifacts=None)``: reports
  AgentTask completion (contract item 6). It is the runtime's own completion
  coroutine, so it retries the 409 stale_pod rejection and transport errors
  on the bounded schedule and raises ``kaalm.TaskAlreadyCompleted`` on the
  terminal 403, like agentruntime's CompleteTask and ErrTaskAlreadyCompleted.
  Once any report was accepted, it raises ``kaalm.TaskAlreadyCompleted``
  without sending. Any other answer raises a RuntimeError.
- ``kaalm.TaskAlreadyCompleted``: the exception class for that terminal 403.
  It is a plain class, usable without a running runtime.

The runtime binds the other members before the handler is imported; importing
this module anywhere else raises, on first attribute access, rather than
handing out half-configured objects.
"""

from __future__ import annotations

from typing import Any

_bound: dict[str, Any] = {}

_MEMBERS = ("complete_task", "gateway", "http_async_client", "http_client", "memory", "trace_context")


class TaskAlreadyCompleted(Exception):
    """The gateway rejected a completion because the task is already in a
    terminal phase (the 403 TaskAlreadyCompleted). Final: do not retry."""


def _bind(
    *,
    gateway: Any,
    memory: Any,
    http_client: Any,
    http_async_client: Any,
    trace_context: Any,
    complete_task: Any,
) -> None:
    """Called once by the runtime at startup, before the handler is imported."""
    _bound["gateway"] = gateway
    _bound["memory"] = memory
    _bound["http_client"] = http_client
    _bound["http_async_client"] = http_async_client
    _bound["trace_context"] = trace_context
    _bound["complete_task"] = complete_task


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
    return sorted((*_MEMBERS, "TaskAlreadyCompleted"))
