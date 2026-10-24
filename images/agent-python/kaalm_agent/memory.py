# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Persistent agent state on the mounted volume (runtime contract item 7).

A port of the Go runtime's store semantics: probe the directory instead of
assuming it, degrade to memory-only when there is no usable volume, and
tolerate a missing or corrupt state file.

State lives in two files. ``state.json`` is the snapshot; ``state.log`` is an
append-only JSON-lines log of writes since the snapshot. Each write appends one
record and fsyncs it before returning, so its cost does not grow with the dedup
window and a write that returned survives a process or node crash. The log is
folded into the snapshot (temp file, fsync, rename) when it outgrows a few
times the snapshot and at a clean shutdown. The log's record shape and the
snapshot's keys are the same in the Go runtime, so each runtime parses the
other's files and a volume written by either reads in both.

The state carries two disjoint areas: the runtime-owned dedup window
(messageId to cached reply, with LRU order), and a general key/value area.
Handler-visible state lives under the ``user/`` key prefix via UserMemory, so
handler keys can never collide with the dedup window.
"""

from __future__ import annotations

import contextlib
import json
import logging
import os
import threading
from pathlib import Path
from typing import Any

log = logging.getLogger("agent.memory")

DEDUP_CAP = 1024

# Compaction folds the log into the snapshot once the log outgrows
# COMPACT_LOG_MULTIPLE times the snapshot, with COMPACT_LOG_MIN_BYTES as a
# floor so a small or missing snapshot does not compact on every write. Read
# at call time, so tests can shrink them.
COMPACT_LOG_MULTIPLE = 4
COMPACT_LOG_MIN_BYTES = 1 << 20


def _truncate_log(path: Path, size: int) -> None:
    """Cut the log at path to size bytes and fsync it."""
    fd = os.open(path, os.O_WRONLY)
    try:
        os.ftruncate(fd, size)
        os.fsync(fd)
    finally:
        os.close(fd)


def _sync_dir(directory: Path) -> None:
    """fsync a directory, so a file created or renamed in it survives a node
    crash. A failure is logged, not fatal: the data itself is synced."""
    try:
        fd = os.open(directory, os.O_RDONLY)
    except OSError as exc:
        log.warning("memory: opening %s to sync: %s", directory, exc)
        return
    try:
        os.fsync(fd)
    except OSError as exc:
        log.warning("memory: syncing %s: %s", directory, exc)
    finally:
        os.close(fd)


class Store:
    """Snapshot-plus-log state with in-memory degradation."""

    def __init__(self, directory: str | None):
        self._lock = threading.Lock()
        self._kv: dict[str, Any] = {}
        self._dedup: dict[str, Any] = {}
        self._dedup_order: list[str] = []
        self._path: Path | None = None
        self._log_path: Path | None = None
        self._log_fd: int | None = None  # opened on the first append
        # Length of the log's good prefix: whole records only. A failed append
        # truncates back to it.
        self._log_bytes = 0
        # Set when the good prefix ends in a record without its newline, so
        # the next append starts a new line first.
        self._log_unterminated = False
        self._snapshot_bytes = 0

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
        self._log_path = Path(directory) / "state.log"
        self._load()
        try:
            self._snapshot_bytes = self._path.stat().st_size
        except OSError:
            self._snapshot_bytes = 0
        if self._replay_log():
            # Seal the recovered state in the snapshot and empty the log, so
            # the next append never lands after a torn or bad line.
            with self._lock:
                self._compact_locked()

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

    def _replay_log(self) -> bool:
        """Apply state.log over the loaded snapshot; report whether the log
        held any bytes. A torn last line (a crash mid-append) is dropped; a bad
        line before it stops the replay and keeps what came before. Either way
        the log is cut back to the records that applied, so a later append
        cannot join a bad line even when the compaction after replay fails."""
        assert self._path is not None and self._log_path is not None
        try:
            raw = self._log_path.read_bytes()
        except FileNotFoundError:
            return False
        except OSError as exc:
            log.warning("memory: reading %s: %s; using the snapshot alone", self._log_path, exc)
            return False
        if not raw:
            return False
        # This runtime truncates the log after every snapshot rename, so a log
        # strictly older than the snapshot was left behind by a runtime that
        # rewrote state.json without it. Its records predate the snapshot.
        try:
            stale = self._path.stat().st_mtime_ns > self._log_path.stat().st_mtime_ns
        except OSError:
            stale = False
        if stale:
            log.warning("memory: %s is older than %s; discarding it", self._log_path, self._path)
            try:
                os.truncate(self._log_path, 0)
            except OSError as exc:
                log.warning("memory: truncating %s: %s", self._log_path, exc)
            return False
        lines = raw.split(b"\n")
        applied = 0
        pos = good = 0  # good: end of the last line that applied or was blank
        for number, line in enumerate(lines, start=1):
            last = number == len(lines)
            end = pos + len(line) + (0 if last else 1)
            if not line.strip():
                if not last:
                    good = end
                pos = end
                continue
            try:
                record = json.loads(line)
            except ValueError:
                record = None
            if not self._apply_locked(record):
                if last:
                    log.warning("memory: dropping an incomplete last record in %s", self._log_path)
                else:
                    log.warning(
                        "memory: %s line %d is not a valid record; replay stops there",
                        self._log_path, number,
                    )
                break
            applied += 1
            good = end
            self._log_unterminated = last
            pos = end
        log.info("memory: replayed %d records from %s", applied, self._log_path)
        self._log_bytes = len(raw)
        if good < self._log_bytes:
            try:
                _truncate_log(self._log_path, good)
            except OSError as exc:
                log.warning("memory: truncating %s: %s", self._log_path, exc)
            else:
                self._log_bytes = good
        return True

    def _apply_locked(self, record: Any) -> bool:
        """Apply one log record to RAM; report whether it was valid."""
        if not isinstance(record, dict):
            return False
        op = record.get("op")
        if op == "remember":
            message_id = record.get("id")
            reply = record.get("reply")
            if not isinstance(message_id, str) or not message_id or reply is None:
                return False
            self._remember_locked(message_id, reply)
        elif op in ("set", "delete"):
            key = record.get("key")
            if not isinstance(key, str) or not key:
                return False
            if op == "set":
                self._kv[key] = record.get("value")
            else:
                self._kv.pop(key, None)
        else:
            return False
        return True

    def _append_locked(self, record: dict[str, Any]) -> None:
        """Append one record to the log and fsync it, so the write survives a
        crash once the caller returns."""
        if self._path is None or self._log_path is None:
            return
        data = (json.dumps(record, separators=(",", ":")) + "\n").encode()
        if self._log_unterminated:
            data = b"\n" + data
        try:
            if self._log_fd is None:
                created = not self._log_path.exists()
                self._log_fd = os.open(self._log_path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
                if created:
                    # A new file's directory entry must be durable too.
                    _sync_dir(self._log_path.parent)
            view = memoryview(data)
            while view:
                view = view[os.write(self._log_fd, view):]
            os.fsync(self._log_fd)
        except OSError as exc:
            # The write is applied in RAM; a snapshot captures it if the volume
            # accepts one. The log is truncated only after a successful
            # rename, so a failed compaction loses nothing already logged.
            log.warning("memory: appending to %s: %s", self._log_path, exc)
            self._drop_partial_append()
            self._compact_locked()
            return
        self._log_unterminated = False
        self._log_bytes += len(data)
        if self._log_bytes > max(COMPACT_LOG_MULTIPLE * self._snapshot_bytes, COMPACT_LOG_MIN_BYTES):
            self._compact_locked()

    def _drop_partial_append(self) -> None:
        """Cut the log back to its good prefix after a failed append, so bytes
        a partial write left behind cannot strand later records. Truncating
        needs no free space. The descriptor is closed and reopened on the next
        append, since a write error can leave it unusable."""
        assert self._log_path is not None
        if self._log_fd is not None:
            with contextlib.suppress(OSError):
                os.close(self._log_fd)
            self._log_fd = None
        try:
            _truncate_log(self._log_path, self._log_bytes)
        except FileNotFoundError:
            pass
        except OSError as exc:
            log.warning("memory: truncating %s: %s", self._log_path, exc)

    def _compact_locked(self) -> None:
        """Write the whole state as a new snapshot, then empty the log. The
        snapshot goes through a temp file, fsync, and rename, so a crash leaves
        either the old snapshot or the new one, never half of one."""
        if self._path is None or self._log_path is None:
            return
        raw = json.dumps({"kv": self._kv, "dedup": self._dedup, "dedupOrder": self._dedup_order}).encode()
        tmp = self._path.with_suffix(".json.tmp")
        try:
            with os.fdopen(os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "wb") as f:
                f.write(raw)
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp, self._path)
        except OSError as exc:
            with contextlib.suppress(OSError):
                tmp.unlink()
            log.warning("memory: writing %s: %s", self._path, exc)
            return
        _sync_dir(self._path.parent)
        self._snapshot_bytes = len(raw)
        # A crash before this truncate leaves records the snapshot already
        # holds; replaying them on the next start changes nothing.
        try:
            if self._log_fd is not None:
                os.ftruncate(self._log_fd, 0)
                os.fsync(self._log_fd)
            elif self._log_path.exists():
                os.truncate(self._log_path, 0)
        except OSError as exc:
            # The log keeps records the snapshot now holds; replaying them is
            # harmless, and a later append still lands after its good prefix.
            log.warning("memory: truncating %s: %s", self._log_path, exc)
            return
        self._log_bytes = 0
        self._log_unterminated = False

    def close(self) -> None:
        """Fold logged writes into the snapshot, so after a clean shutdown
        state.json alone is complete, and close the log. A write after close
        reopens the log, so it is still durable."""
        with self._lock:
            if self._path is None:
                return
            if self._log_bytes > 0:
                self._compact_locked()
            if self._log_fd is not None:
                try:
                    os.close(self._log_fd)
                except OSError as exc:
                    log.warning("memory: closing %s: %s", self._log_path, exc)
                self._log_fd = None

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
            self._append_locked({"op": "set", "key": key, "value": value})

    def delete(self, key: str) -> None:
        with self._lock:
            self._kv.pop(key, None)
            self._append_locked({"op": "delete", "key": key})

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
            self._remember_locked(message_id, reply, cap)
            self._append_locked({"op": "remember", "id": message_id, "reply": reply})

    def _remember_locked(self, message_id: str, reply: Any, cap: int = DEDUP_CAP) -> None:
        """Apply a remember to RAM. Remembering a known id replaces its reply
        without adding a second order entry."""
        if message_id not in self._dedup:
            self._dedup_order.append(message_id)
        self._dedup[message_id] = reply
        while len(self._dedup_order) > cap:
            oldest = self._dedup_order.pop(0)
            self._dedup.pop(oldest, None)


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
