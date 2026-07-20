# Copyright 2026. Licensed under the Apache License, Version 2.0.
"""The single developer-owned extension point."""

from __future__ import annotations

from typing import Any


async def handle_message(agent: Any, envelope: dict[str, Any]) -> dict[str, Any]:
    """Replace this body with your agent logic.

    The gateway has already authenticated the caller and deduplicated retries
    before this is invoked. Return a response envelope whose ``content`` is the
    agent's reply; the gateway relays it to the webhook caller (sync mode) or
    to the callbackUrl/polling endpoint (async mode).

    To call an LLM, POST to ``agent.gateway_url`` using an aiohttp session with
    ``agent.reloader.client_context`` as its SSL context; the gateway proxies
    to your ModelProviders.
    """
    return {
        "content": "starter-python received: " + envelope.get("content", ""),
        "attachments": [],
        "metadata": {},
    }
