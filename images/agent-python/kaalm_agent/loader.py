# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Handler resolution for the reference base image.

The controller injects KAALM_HANDLER_PATH if and only if Agent.spec.handler is
set; a FROM-built image declares the same variable itself. The variable is the
single signal (docs/src/runtime/base-images.md):

- absent: serve the built-in echo handler.
- set and loadable: run the loaded handler.
- set and broken in any way: log the exact failure and exit nonzero. The
  container enters CrashLoopBackOff, which is the loud outcome a
  configured-but-broken handler must have. There is no silent fallback.

The handler ABI is exactly ``handle_message(envelope)``, sync or async, with
capabilities reached via ``import kaalm``.
"""

from __future__ import annotations

import asyncio
import functools
import importlib.util
import inspect
import logging
import os
import sys
from pathlib import Path
from typing import Any, Awaitable, Callable

HANDLER_ENV = "KAALM_HANDLER_PATH"
HANDLER_FILE = "handler.py"

AsyncHandler = Callable[[dict[str, Any]], Awaitable[dict[str, Any]]]


class HandlerLoadError(Exception):
    """A configured handler could not be loaded. Fatal by design."""


def resolve() -> tuple[AsyncHandler, str]:
    """Return (normalized async handler, human-readable source)."""
    path = os.environ.get(HANDLER_ENV)
    if not path:
        from . import echo

        return echo.handle_message, "built-in echo handler (no handler configured)"

    file = Path(path) / HANDLER_FILE
    if not file.is_file():
        raise HandlerLoadError(
            f"{HANDLER_ENV} is set to {path!r} but {file} does not exist; "
            "a configured handler must load or the container must not serve"
        )

    # The handler directory joins sys.path so sibling keys in the same
    # ConfigMap are importable as modules (docs/src/runtime/base-images.md).
    if path not in sys.path:
        sys.path.insert(0, path)

    spec = importlib.util.spec_from_file_location("handler", file)
    if spec is None or spec.loader is None:
        raise HandlerLoadError(f"{file} could not be prepared for import")
    module = importlib.util.module_from_spec(spec)
    sys.modules["handler"] = module
    try:
        spec.loader.exec_module(module)
    except BaseException as exc:  # noqa: BLE001 - any import failure is fatal and must name itself
        raise HandlerLoadError(f"importing {file} failed: {exc!r}") from exc

    fn = getattr(module, "handle_message", None)
    if not callable(fn):
        raise HandlerLoadError(f"{file} defines no callable handle_message(envelope)")

    _check_signature(fn, file)
    return _normalize(fn), f"handler loaded from {file}"


def _check_signature(fn: Callable[..., Any], file: Path) -> None:
    try:
        sig = inspect.signature(fn)
    except (TypeError, ValueError):
        return  # unintrospectable callables get the benefit of the doubt
    required = [
        p
        for p in sig.parameters.values()
        if p.kind in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD)
        and p.default is inspect.Parameter.empty
    ]
    has_var_positional = any(
        p.kind is inspect.Parameter.VAR_POSITIONAL for p in sig.parameters.values()
    )
    if len(required) == 1 or has_var_positional:
        return
    if len(required) == 2:
        raise HandlerLoadError(
            f"{file}: handle_message takes two required arguments; the old "
            "handle_message(agent, envelope) form was removed in v0.3.0. "
            "Define handle_message(envelope) and reach the runtime via 'import kaalm'."
        )
    raise HandlerLoadError(
        f"{file}: handle_message must take exactly one required argument "
        f"(the envelope); found {len(required)}"
    )


def _normalize(fn: Callable[..., Any]) -> AsyncHandler:
    """Return an awaitable-of-envelope wrapper around a sync or async handler.

    Sync handlers run in the default executor so a slow or blocking handler
    cannot stall the event loop that serves health probes.
    """
    if inspect.iscoroutinefunction(fn):
        return fn

    async def _run_sync(envelope: dict[str, Any]) -> dict[str, Any]:
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(None, functools.partial(fn, envelope))

    return _run_sync


def load_or_exit(logger: logging.Logger) -> tuple[AsyncHandler, str]:
    """resolve(), with the fatal path applied: log precisely, exit nonzero."""
    try:
        return resolve()
    except HandlerLoadError as exc:
        logger.error("fatal: %s", exc)
        raise SystemExit(1) from exc
