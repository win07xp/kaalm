# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Persistent agent state on the mounted volume (runtime contract item 7).

A port of the Go starter's store semantics: probe the directory instead of
assuming it, degrade to memory-only when there is no usable volume, tolerate a
missing or corrupt state file, and write via temp-then-rename so a crash
mid-write cannot leave an unparseable file.

The one JSON file carries two disjoint areas: the runtime-owned dedup window
(messageId to cached reply, with LRU order), and a general key/value area.
Handler-visible state lives under the ``user/`` key prefix via UserMemory, so
handler keys can never collide with the dedup window.
"""

from __future__ import annotations

import json
import logging
import os
import threading
from pathlib import Path
from typing import Any

log = logging.getLogger("agent.memory")

DEDUP_CAP = 1024


class Store:
    """JSON-file-backed state with in-memory degradation."""

    def __init__(self, directory: str | None):
        self._lock = threading.Lock()
        self._kv: dict[str, Any] = {}
        self._dedup: dict[str, Any] = {}
        self._dedup_order: list[str] = []
        self._path: Path | None = None

        if not directory:
            return
        # Probe rather than assume: persistence is optional, and an agent
        # without a PVC must run instead of crash-looping on a missing dir.
        try:
            os.makedirs(directory, mode=0o700, exist_ok=True)
        except OSError as exc:
            log.warning("memory: %s unusable (%s); continuing without persistence", directory, exc)
            return
        self._path = Path(directory) / "state.json"
        self._load()

    @property
    def persistent(self) -> bool:
        return self._path is not None

    def _load(self) -> None:
        assert self._path is not None
        try:
            raw = self._path.read_text()
        except FileNotFoundError:
            return
        except OSError as exc:
            log.warning("memory: reading %s: %s; starting empty", self._path, exc)
            return
        try:
            loaded = json.loads(raw)
        except ValueError as exc:
            log.warning("memory: %s is not readable state (%s); starting empty", self._path, exc)
            return
        if not isinstance(loaded, dict):
            log.warning("memory: %s is not readable state (not an object); starting empty", self._path)
            return
        self._kv = dict(loaded.get("kv", {}))
        self._dedup = dict(loaded.get("dedup", {}))
        order = [m for m in loaded.get("dedupOrder", []) if m in self._dedup]
        # Re-derive order for any replies the order list lost track of.
        order.extend(m for m in self._dedup if m not in order)
        self._dedup_order = order
        log.info(
            "memory: recovered %d cached replies and %d keys from %s",
            len(self._dedup), len(self._kv), self._path,
        )

    def _flush_locked(self) -> None:
        if self._path is None:
            return
        raw = json.dumps({"kv": self._kv, "dedup": self._dedup, "dedupOrder": self._dedup_order})
        tmp = self._path.with_suffix(".json.tmp")
        try:
            tmp.write_text(raw)
            os.replace(tmp, self._path)
        except OSError as exc:
            log.warning("memory: writing %s: %s", self._path, exc)

    # ---- general key/value area ----

    def get(self, key: str, default: Any = None) -> Any:
        with self._lock:
            return self._kv.get(key, default)

    def put(self, key: str, value: Any) -> None:
        """Store a JSON-serializable value. Raises TypeError on anything else,
        loudly and immediately, rather than corrupting the state file later."""
        json.dumps(value)
        with self._lock:
            self._kv[key] = value
            self._flush_locked()

    def delete(self, key: str) -> None:
        with self._lock:
            self._kv.pop(key, None)
            self._flush_locked()

    # ---- runtime-owned dedup window (contract item 7) ----

    def recall(self, message_id: str) -> Any | None:
        """The cached reply for a messageId this agent already answered,
        possibly in a previous life, before hibernation."""
        with self._lock:
            reply = self._dedup.get(message_id)
            if reply is not None:
                self._dedup_order.remove(message_id)
                self._dedup_order.append(message_id)
            return reply

    def remember(self, message_id: str, reply: Any, cap: int = DEDUP_CAP) -> None:
        with self._lock:
            if message_id not in self._dedup:
                self._dedup_order.append(message_id)
            self._dedup[message_id] = reply
            while len(self._dedup_order) > cap:
                oldest = self._dedup_order.pop(0)
                self._dedup.pop(oldest, None)
            self._flush_locked()


class UserMemory:
    """The handler-facing view of the store: the same persistence and
    degradation semantics, confined to the ``user/`` prefix (the kaalm module
    ABI, docs/src/runtime/base-images.md)."""

    _PREFIX = "user/"

    def __init__(self, store: Store):
        self._store = store

    def get(self, key: str, default: Any = None) -> Any:
        return self._store.get(self._PREFIX + key, default)

    def put(self, key: str, value: Any) -> None:
        self._store.put(self._PREFIX + key, value)

    def delete(self, key: str) -> None:
        self._store.delete(self._PREFIX + key)

    @property
    def persistent(self) -> bool:
        return self._store.persistent
