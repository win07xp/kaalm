# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The readiness probe (runtime contract item 1): /readyz answers 200 only
when the container can accept a message, which takes a loaded handler and a
certificate that is loaded and not expired. Otherwise it answers 503."""

from __future__ import annotations

import subprocess
import time

from aiohttp.test_utils import make_mocked_request

from kaalm_agent import echo
from kaalm_agent.__main__ import Agent
from kaalm_agent.tls import CertReloader


class FakeReloader:
    """Stands in for CertReloader: only the usability answer matters here."""

    def __init__(self, usable: bool):
        self._usable = usable

    def usable(self) -> bool:
        return self._usable


def mk_agent(handler, reloader) -> Agent:
    # The constructor's task-mode probe reads a nonexistent cert path and
    # lands on Agent mode; store and gateway are not touched by /readyz.
    return Agent(handler=handler, store=None, gateway=None, reloader=reloader)


async def probe(agent: Agent) -> tuple[int, str]:
    resp = await agent.handle_readyz(make_mocked_request("GET", "/readyz"))
    return resp.status, resp.text


async def test_ready_with_handler_and_usable_certificate():
    assert await probe(mk_agent(echo.handle_message, FakeReloader(True))) == (200, "ok")


async def test_not_ready_before_handler_loads():
    status, _ = await probe(mk_agent(None, FakeReloader(True)))
    assert status == 503


async def test_not_ready_without_a_usable_certificate():
    status, _ = await probe(mk_agent(echo.handle_message, FakeReloader(False)))
    assert status == 503


async def test_not_ready_before_certificate_reloader_exists():
    status, _ = await probe(mk_agent(echo.handle_message, None))
    assert status == 503


def test_certificate_is_usable_until_not_after(tmp_path):
    """A loaded certificate is usable until its NotAfter; past it, every
    gateway handshake fails, so the container cannot accept a message."""
    cert, key = tmp_path / "tls.crt", tmp_path / "tls.key"
    subprocess.run(
        [
            "openssl", "req", "-x509", "-newkey", "ec",
            "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "1",
            "-subj", "/CN=workload", "-keyout", str(key), "-out", str(cert),
        ],
        check=True,
        capture_output=True,
    )
    # Self-signed, so the leaf is its own CA bundle.
    reloader = CertReloader(str(cert), str(key), str(cert), lambda _: None)
    assert reloader.usable()
    assert not reloader.usable(now=time.time() + 2 * 86400)
