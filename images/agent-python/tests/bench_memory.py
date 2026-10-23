# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Benchmark of the state write path at a full dedup window, the Python
counterpart of BenchmarkStore_RememberFullWindow in agentruntime.

Not collected by pytest. Run from images/agent-python:

    PYTHONPATH=. python tests/bench_memory.py

Set TMPDIR to a directory on the disk you want to measure: on tmpfs an fsync
costs nothing. Periodic compaction stays in the timed loop, so the result is
the amortized cost per message.
"""

from __future__ import annotations

import tempfile
import time

from kaalm_agent.memory import DEDUP_CAP, Store

CALLS = 2000


def bench(reply_bytes: int) -> float:
    """Microseconds per remember with a full window and replies of
    reply_bytes."""
    reply = {"content": "x" * reply_bytes, "attachments": [], "metadata": {}}
    with tempfile.TemporaryDirectory() as directory:
        store = Store(directory)
        for i in range(DEDUP_CAP):
            store.remember(f"fill{i}", reply)
        start = time.perf_counter()
        for i in range(CALLS):
            store.remember(f"b{i}", reply)
        elapsed = time.perf_counter() - start
        store.close()
    return elapsed / CALLS * 1e6


def main() -> None:
    for size in (2 << 10, 8 << 10):
        print(f"remember, full window, {size >> 10} KiB replies: {bench(size):.1f} us/call")


if __name__ == "__main__":
    main()
