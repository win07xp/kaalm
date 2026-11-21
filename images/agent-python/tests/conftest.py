# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Shared fixtures: isolated handler dirs, a workload certificate, and cleanup of the process-global
state the loader and the kaalm module deliberately use (sys.path, sys.modules,
the ABI binding), so every test starts from a cold runtime."""

from __future__ import annotations

import subprocess
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
def workload_cert(tmp_path):
    """A self-signed (cert, key) pair with an AgentTask workload SAN, so
    build() can start the runtime from real certificate files (pass the cert
    as the CA too)."""
    cert, key = tmp_path / "tls.crt", tmp_path / "tls.key"
    subprocess.run(
        [
            "openssl", "req", "-x509", "-newkey", "ec",
            "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "1",
            "-subj", "/CN=workload", "-addext", "subjectAltName=DNS:job.default.task.kaalm.io",
            "-keyout", str(key), "-out", str(cert),
        ],
        check=True,
        capture_output=True,
    )
    return cert, key


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
