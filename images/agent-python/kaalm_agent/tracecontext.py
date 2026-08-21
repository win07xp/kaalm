# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""W3C trace-context propagation for the message being handled.

The gateway's delivery request may carry ``traceparent`` and ``tracestate``
headers (runtime contract item 8). The runtime captures them in a context
variable before dispatching the handler, and every outbound client the ABI
hands out (``kaalm.gateway`` and the rotation-aware httpx factories) copies
them onto its requests, so the LLM and tool spans the gateway creates stay
children of the delivery that caused them. Handlers running their own
OpenTelemetry SDK read the same values through ``kaalm.trace_context()``.
"""

from __future__ import annotations

from contextvars import ContextVar
from typing import Any

_HEADERS = ("traceparent", "tracestate")

_current: ContextVar[dict[str, str] | None] = ContextVar("kaalm_trace_context", default=None)


def set_from_headers(headers: Any) -> None:
    """Capture the delivery's trace context for the current task.

    ``headers`` is any case-insensitive mapping with ``.get()`` (aiohttp's
    request headers). An absent traceparent clears the context, so a reused
    task never leaks a previous message's identity.
    """
    found = {name: headers.get(name) for name in _HEADERS}
    _current.set({k: v for k, v in found.items() if v} or None)


def current() -> dict[str, str]:
    """The trace context of the message being handled; ``{}`` outside one."""
    return dict(_current.get() or {})
