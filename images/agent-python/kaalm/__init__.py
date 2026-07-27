# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The handler-facing ABI of the Kaalm reference base images.

Exactly two members in v0.3 (docs/src/runtime/base-images.md), and the surface
is append-only within a minor release series:

- ``kaalm.gateway``: a preconfigured client for $KAALM_GATEWAY_ENDPOINT
  carrying the Pod's mTLS identity and CA trust.
- ``kaalm.memory``: the runtime's persistent store, confined to the ``user/``
  key prefix. Backed by the PVC when persistence is enabled, in-memory
  otherwise.

The runtime binds both before the handler is imported; importing this module
anywhere else raises, on first attribute access, rather than handing out
half-configured objects.
"""

from __future__ import annotations

from typing import Any

_bound: dict[str, Any] = {}

_MEMBERS = ("gateway", "memory")


def _bind(*, gateway: Any, memory: Any) -> None:
    """Called once by the runtime at startup, before the handler is imported."""
    _bound["gateway"] = gateway
    _bound["memory"] = memory


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
