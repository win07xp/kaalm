# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The HTTPS listener after a certificate rotation (runtime contract
"Certificate reload on rotation"): once the mounted files are replaced and the
reloader runs, new handshakes present the new certificate and verify client
certificates against the new CA only, so the old CA is no longer trusted. The
listener is started once through serve() and never restarted."""

from __future__ import annotations

import asyncio
import ssl
import subprocess
from pathlib import Path

import aiohttp
import pytest

from kaalm_agent import __main__ as runtime

AGENT_SAN = "agent.default.svc.cluster.local"
GATEWAY_SAN = "kaalm-gateway.kaalm-system.svc.cluster.local"


def _openssl(*args: str) -> None:
    subprocess.run(["openssl", *args], check=True, capture_output=True)


def _make_generation(d: Path) -> dict[str, Path]:
    """Write a CA and two leaves it signs (agent server, gateway client)."""
    d.mkdir(parents=True)
    ca_crt, ca_key = d / "ca.crt", d / "ca.key"
    _openssl(
        "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
        "-nodes", "-days", "1", "-subj", f"/CN={d.name}-ca",
        "-addext", "basicConstraints=critical,CA:TRUE",
        "-addext", "keyUsage=critical,keyCertSign",
        "-keyout", str(ca_key), "-out", str(ca_crt),
    )
    out = {"ca": ca_crt}
    for name, san in (("server", AGENT_SAN), ("gateway", GATEWAY_SAN)):
        key, csr, crt = d / f"{name}.key", d / f"{name}.csr", d / f"{name}.crt"
        _openssl(
            "req", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
            "-nodes", "-subj", f"/CN={d.name}-{name}",
            "-addext", f"subjectAltName=DNS:{san}",
            "-keyout", str(key), "-out", str(csr),
        )
        _openssl(
            "x509", "-req", "-in", str(csr), "-CA", str(ca_crt), "-CAkey", str(ca_key),
            "-CAcreateserial", "-days", "1", "-copy_extensions", "copy", "-out", str(crt),
        )
        out[f"{name}_crt"], out[f"{name}_key"] = crt, key
    return out


def _install(gen: dict[str, Path], mount: Path) -> None:
    """Write a generation's agent material into the mount directory."""
    (mount / "tls.crt").write_bytes(gen["server_crt"].read_bytes())
    (mount / "tls.key").write_bytes(gen["server_key"].read_bytes())
    (mount / "ca.crt").write_bytes(gen["ca"].read_bytes())


@pytest.fixture()
async def rotating_runtime(tmp_path, monkeypatch, unused_tcp_port):
    """Start serve() on generation 1; yield (agent, port, gen1, gen2, rotate),
    where rotate() writes generation 2 into the mount and reloads."""
    gen1 = _make_generation(tmp_path / "gen1")
    gen2 = _make_generation(tmp_path / "gen2")
    mount = tmp_path / "mount"
    mount.mkdir()
    _install(gen1, mount)

    port = unused_tcp_port
    monkeypatch.setenv("KAALM_TLS_CERT", str(mount / "tls.crt"))
    monkeypatch.setenv("KAALM_TLS_KEY", str(mount / "tls.key"))
    monkeypatch.setenv("KAALM_CA_CERT", str(mount / "ca.crt"))
    monkeypatch.setenv("KAALM_MEMORY_DIR", str(tmp_path / "memory"))
    monkeypatch.setenv("KAALM_HEALTH_PORT", str(port))
    monkeypatch.delenv("KAALM_OPERATOR_NAMESPACE", raising=False)
    monkeypatch.delenv("KAALM_TASK_AUTOCOMPLETE", raising=False)

    agent = runtime.build()
    serving = asyncio.create_task(runtime.serve(agent))
    for _ in range(100):
        try:
            _, writer = await asyncio.open_connection("127.0.0.1", port)
        except OSError:
            await asyncio.sleep(0.05)
            continue
        writer.close()
        await writer.wait_closed()
        break
    else:
        pytest.fail("listener did not start")

    def rotate() -> None:
        _install(gen2, mount)
        agent.reloader.reload()

    try:
        yield agent, port, gen1, gen2, rotate
    finally:
        agent.stop.set()
        await asyncio.wait_for(serving, 10)


def _session(ca: Path, client: tuple[Path, Path] | None = None) -> aiohttp.ClientSession:
    """A client that trusts only ``ca``. It dials 127.0.0.1 with no server
    name, as the kubelet's probe does, so the listener sees no SNI."""
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.load_verify_locations(ca)
    if client is not None:
        ctx.load_cert_chain(*client)
    return aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=ctx))


async def test_listener_presents_the_rotated_certificate(rotating_runtime):
    agent, port, gen1, gen2, rotate = rotating_runtime
    url = f"https://127.0.0.1:{port}/readyz"

    async with _session(gen1["ca"]) as s, s.get(url) as resp:
        assert resp.status == 200

    rotate()
    assert agent.reloader.generation == 2

    async with _session(gen2["ca"]) as s, s.get(url) as resp:
        assert resp.status == 200
    async with _session(gen1["ca"]) as s:
        with pytest.raises(aiohttp.ClientConnectorCertificateError):
            await s.get(url)


async def test_listener_verifies_clients_against_the_rotated_ca(rotating_runtime):
    _, port, gen1, gen2, rotate = rotating_runtime
    url = f"https://127.0.0.1:{port}/v1/message"
    body = {"messageId": "m1", "text": "hi"}

    rotate()

    new_gateway = (gen2["gateway_crt"], gen2["gateway_key"])
    async with _session(gen2["ca"], new_gateway) as s, s.post(url, json=body) as resp:
        assert resp.status == 200

    old_gateway = (gen1["gateway_crt"], gen1["gateway_key"])
    async with _session(gen2["ca"], old_gateway) as s:
        with pytest.raises(aiohttp.ClientError):
            async with s.post(url, json=body) as resp:
                await resp.read()
