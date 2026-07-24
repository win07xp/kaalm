# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""TLS material loading and rotation reload for the Kaalm runtime contract.

The kubelet rotates a projected volume by atomically renaming the ``..data``
symlink under the mount directory. The leaf files are never rewritten in place,
so a leaf-path watch never fires. This module watches the mount DIRECTORY and
rebuilds both SSL contexts on the ``..data`` swap (runtime contract item 4).
"""

from __future__ import annotations

import ssl
import threading
from pathlib import Path
from typing import Callable

from watchdog.events import FileSystemEvent, FileSystemEventHandler
from watchdog.observers import Observer

GATEWAY_SANS = {
    "kaalm-gateway.kaalm-system.svc.cluster.local",
    "kaalm-gateway.kaalm-system.svc",
}


class CertReloader:
    """Holds the server and client SSL contexts, rebuilt on rotation."""

    def __init__(self, cert_file: str, key_file: str, ca_file: str, log: Callable[[str], None]):
        self._cert_file = cert_file
        self._key_file = key_file
        self._ca_file = ca_file
        self._log = log
        self._lock = threading.Lock()
        self._server_ctx: ssl.SSLContext | None = None
        self._client_ctx: ssl.SSLContext | None = None
        self.reload()

    def reload(self) -> None:
        # Server context: request a client cert but do not require one, so
        # kubelet probes (which present none) still complete the handshake.
        # Per-path enforcement happens in the /v1/message handler.
        server_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        server_ctx.load_cert_chain(self._cert_file, self._key_file)
        server_ctx.load_verify_locations(self._ca_file)
        server_ctx.verify_mode = ssl.CERT_OPTIONAL

        # Client context: present the agent cert and trust the CA for
        # outbound gateway calls.
        client_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        client_ctx.load_cert_chain(self._cert_file, self._key_file)
        client_ctx.load_verify_locations(self._ca_file)

        with self._lock:
            self._server_ctx = server_ctx
            self._client_ctx = client_ctx

    @property
    def server_context(self) -> ssl.SSLContext:
        with self._lock:
            assert self._server_ctx is not None
            return self._server_ctx

    @property
    def client_context(self) -> ssl.SSLContext:
        with self._lock:
            assert self._client_ctx is not None
            return self._client_ctx

    def start_watch(self) -> None:
        mount_dir = str(Path(self._cert_file).parent)
        handler = _DataSwapHandler(self._on_rotation)
        observer = Observer()
        observer.schedule(handler, mount_dir, recursive=False)
        observer.daemon = True
        observer.start()

    def _on_rotation(self) -> None:
        try:
            self.reload()
            self._log("reloaded TLS material after rotation")
        except Exception as exc:  # noqa: BLE001 - log and keep the old contexts
            self._log(f"cert reload failed: {exc}")


class _DataSwapHandler(FileSystemEventHandler):
    def __init__(self, on_swap: Callable[[], None]):
        self._on_swap = on_swap

    def _is_data(self, event: FileSystemEvent) -> bool:
        return Path(str(event.src_path)).name == "..data"

    def on_created(self, event: FileSystemEvent) -> None:
        if self._is_data(event):
            self._on_swap()

    def on_moved(self, event: FileSystemEvent) -> None:
        dest = getattr(event, "dest_path", "")
        if self._is_data(event) or Path(str(dest)).name == "..data":
            self._on_swap()


def peer_san_matches_gateway(transport_extra_ssl_object) -> bool:
    """Return True when the peer cert names the gateway Service DNS."""
    cert = transport_extra_ssl_object.getpeercert() if transport_extra_ssl_object else None
    if not cert:
        return False
    for typ, value in cert.get("subjectAltName", ()):
        if typ == "DNS" and value in GATEWAY_SANS:
            return True
    return False


def workload_is_task(cert_file: str) -> bool:
    """Detect AgentTask mode from the client cert SAN shape.

    AgentTask certs carry ``{name}.{namespace}.task.kaalm.io``; Agent certs
    carry the Service DNS shape. Parsed without extra dependencies via ssl.
    """
    try:
        cert = ssl._ssl._test_decode_cert(cert_file)  # type: ignore[attr-defined]
    except Exception:  # noqa: BLE001
        return False
    for typ, value in cert.get("subjectAltName", ()):
        if typ == "DNS" and value.endswith(".task.kaalm.io"):
            return True
    return False
