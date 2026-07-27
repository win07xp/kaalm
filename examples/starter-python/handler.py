# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The single developer-owned extension point.

The base image owns the whole runtime contract; this function is everything
you write. It receives the normalized message envelope after the runtime has
authenticated the caller and deduplicated redeliveries, and its return value
travels back through the gateway to the webhook caller (sync mode) or the
callbackUrl / polling endpoint (async mode).

Capabilities come from the kaalm module (docs/src/runtime/base-images.md):

- ``kaalm.gateway``: POST LLM requests through the gateway with the Pod's
  mTLS identity, e.g.
  ``await kaalm.gateway.post("/v1/chat/completions", json={...})`` with a
  qualified model name like ``anthropic-shared/claude-opus-4-6``.
- ``kaalm.memory``: persistent key/value state, backed by the PVC when
  spec.persistence is enabled (and hence surviving hibernation), in-memory
  otherwise.
"""

from __future__ import annotations

from typing import Any

import kaalm


async def handle_message(envelope: dict[str, Any]) -> dict[str, Any]:
    user = envelope.get("userId", "someone")
    count = (kaalm.memory.get(f"seen/{user}") or 0) + 1
    kaalm.memory.put(f"seen/{user}", count)
    return {
        "content": f"starter-python received message {count} from you: "
        + str(envelope.get("content", "")),
        "attachments": [],
        "metadata": {},
    }
