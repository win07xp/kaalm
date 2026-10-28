# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The transport-independent message path: dedup in front of the handler,
persistence behind it, and the wake-replacement scenario end to end. Also the
built-in echo handler's contract."""

from __future__ import annotations

import asyncio
import threading
import time

import pytest

from kaalm_agent import echo
from kaalm_agent.__main__ import Agent
from kaalm_agent.memory import Store


class CountingHandler:
    def __init__(self):
        self.calls = 0

    async def __call__(self, envelope):
        self.calls += 1
        return {"content": f"reply {self.calls} to " + envelope.get("content", "")}


def mk_agent(store: Store) -> tuple[Agent, CountingHandler]:
    handler = CountingHandler()
    # gateway/reloader are not touched by respond(); the constructor's
    # task-mode probe reads a nonexistent cert path and lands on Agent mode.
    agent = Agent(handler=handler, store=store, gateway=None, reloader=None)
    return agent, handler


async def test_same_message_id_is_answered_from_cache(tmp_path):
    agent, handler = mk_agent(Store(str(tmp_path / "mem")))
    first = await agent.respond({"messageId": "m1", "content": "hi"})
    second = await agent.respond({"messageId": "m1", "content": "hi"})
    assert first == second
    assert handler.calls == 1


async def test_distinct_ids_each_reach_the_handler(tmp_path):
    agent, handler = mk_agent(Store(str(tmp_path / "mem")))
    await agent.respond({"messageId": "m1", "content": "a"})
    await agent.respond({"messageId": "m2", "content": "b"})
    assert handler.calls == 2


async def test_idless_envelopes_are_never_cached(tmp_path):
    """Deduplicating on the empty id would answer every id-less message with
    the first id-less reply forever."""
    agent, handler = mk_agent(Store(str(tmp_path / "mem")))
    r1 = await agent.respond({"content": "a"})
    r2 = await agent.respond({"content": "b"})
    assert handler.calls == 2
    assert r1 != r2


async def test_dedup_survives_a_wake_replacement(tmp_path):
    """Contract item 7: the wake-replacement Pod is a brand new process on the
    same PVC and must answer a redelivered messageId from cache."""
    d = str(tmp_path / "mem")
    first_life, first_handler = mk_agent(Store(d))
    original = await first_life.respond({"messageId": "m1", "content": "hi"})

    second_life, second_handler = mk_agent(Store(d))  # new process, same volume
    replayed = await second_life.respond({"messageId": "m1", "content": "hi"})
    assert replayed == original
    assert second_handler.calls == 0  # answered from the recovered window


async def test_dedup_survives_crash_without_close(tmp_path):
    """A crash gives the runtime no chance to compact: the reply written
    through respond() must survive from the log alone."""
    d = str(tmp_path / "mem")
    first_life, _ = mk_agent(Store(d))
    original = await first_life.respond({"messageId": "m1", "content": "hi"})
    # No close(): the first life dies without a clean shutdown.

    second_life, second_handler = mk_agent(Store(d))
    assert await second_life.respond({"messageId": "m1", "content": "hi"}) == original
    assert second_handler.calls == 0


async def test_echo_contract():
    assert (await echo.handle_message({"content": "hi"}))["content"] == "echo: hi"
    assert (await echo.handle_message({}))["content"] == "echo: "
    reply = await echo.handle_message({"content": "x"})
    assert reply["attachments"] == [] and reply["metadata"] == {}
    # Deterministic: same input, same output.
    assert reply == await echo.handle_message({"content": "x"})


def assert_nothing_logged(d):
    log_path = d / "state.log"
    assert not log_path.exists() or log_path.read_bytes() == b""


async def test_reply_with_nan_is_rejected_before_it_is_cached(tmp_path):
    async def handler(_envelope):
        return {"content": "x", "metadata": {"score": float("nan")}}

    d = tmp_path / "mem"
    store = Store(str(d))
    agent = Agent(handler=handler, store=store, gateway=None, reloader=None)
    with pytest.raises(TypeError, match="m1"):
        await agent.respond({"messageId": "m1", "content": "hi"})
    assert store.recall("m1") is None
    with pytest.raises(TypeError):
        await agent.respond({"content": "hi"})
    assert_nothing_logged(d)


async def test_unserializable_reply_leaves_the_window_untouched(tmp_path):
    async def handler(_envelope):
        return {"content": object()}

    d = tmp_path / "mem"
    store = Store(str(d))
    agent = Agent(handler=handler, store=store, gateway=None, reloader=None)
    with pytest.raises(TypeError):
        await agent.respond({"messageId": "m1", "content": "hi"})
    assert store.recall("m1") is None
    store.put("user/k", 1)
    store.close()
    assert Store(str(d)).get("user/k") == 1


async def test_respond_yields_to_the_loop_while_the_store_writes(tmp_path):
    """The store write runs off the loop thread, so other coroutines (probes,
    heartbeat, other deliveries) keep running during its fsync. The write here
    blocks until a coroutine on the loop runs; on the loop thread it would wait
    out the full timeout instead."""
    loop_ran = threading.Event()

    class SlowStore(Store):
        def remember(self, message_id, reply):
            loop_ran.wait(timeout=5)
            super().remember(message_id, reply)

    agent, _ = mk_agent(SlowStore(str(tmp_path / "mem")))
    ticks = 0

    async def ticker():
        nonlocal ticks
        while True:
            ticks += 1
            loop_ran.set()
            await asyncio.sleep(0.01)

    task = asyncio.create_task(ticker())
    start = time.monotonic()
    await agent.respond({"messageId": "m1", "content": "hi"})
    elapsed = time.monotonic() - start
    task.cancel()
    assert ticks >= 1
    assert elapsed < 4
