# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Shared fixtures: isolated handler dirs, and cleanup of the process-global
state the loader and the kaalm module deliberately use (sys.path, sys.modules,
the ABI binding), so every test starts from a cold runtime."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

import kaalm


@pytest.fixture(autouse=True)
def _clean_process_state(monkeypatch):
    """Every test runs as if in a fresh container: no handler env, no cached
    handler module, no leaked sys.path entry, no bound ABI."""
    monkeypatch.delenv("KAALM_HANDLER_PATH", raising=False)
    saved_path = list(sys.path)
    yield
    sys.modules.pop("handler", None)
    sys.path[:] = saved_path
    kaalm._bound.clear()


@pytest.fixture()
def handler_dir(tmp_path):
    """Write a handler ConfigMap directory: handler_dir(source, **siblings)."""

    def _write(source: str, **siblings: str) -> Path:
        d = tmp_path / "handler-mount"
        d.mkdir(exist_ok=True)
        (d / "handler.py").write_text(source)
        for name, body in siblings.items():
            (d / f"{name}.py").write_text(body)
        return d

    return _write
