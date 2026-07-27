# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The built-in default handler, active only when no handler is configured.

Deterministic, makes no LLM calls (so it works on a provider-less Agent), and
its output is distinguishable from any real handler, which is what the e2e
suite keys on. See docs/src/runtime/base-images.md (The default handler).
"""

from __future__ import annotations

from typing import Any


async def handle_message(envelope: dict[str, Any]) -> dict[str, Any]:
    return {
        "content": "echo: " + str(envelope.get("content", "")),
        "attachments": [],
        "metadata": {},
    }
