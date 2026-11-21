# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The kaalm module ABI: exactly eight members (seven bound by the runtime,
plus the TaskAlreadyCompleted exception class), loud when imported anywhere
else."""

from __future__ import annotations

import pytest

import kaalm


def test_unbound_access_raises_runtime_error():
    with pytest.raises(RuntimeError, match="kaalm.gateway is only available"):
        _ = kaalm.gateway
    with pytest.raises(RuntimeError, match="kaalm.memory is only available"):
        _ = kaalm.memory
    with pytest.raises(RuntimeError, match="kaalm.http_client is only available"):
        _ = kaalm.http_client
    with pytest.raises(RuntimeError, match="kaalm.http_async_client is only available"):
        _ = kaalm.http_async_client
    with pytest.raises(RuntimeError, match="kaalm.trace_context is only available"):
        _ = kaalm.trace_context
    with pytest.raises(RuntimeError, match="kaalm.complete_task is only available"):
        _ = kaalm.complete_task
    with pytest.raises(RuntimeError, match="kaalm.langchain_chat_anthropic is only available"):
        _ = kaalm.langchain_chat_anthropic


def test_bound_members_are_returned():
    gw, mem, sync_factory, async_factory, trace, complete, chat_anthropic = (object() for _ in range(7))
    kaalm._bind(
        gateway=gw, memory=mem, http_client=sync_factory, http_async_client=async_factory, trace_context=trace,
        complete_task=complete, langchain_chat_anthropic=chat_anthropic,
    )
    assert kaalm.gateway is gw
    assert kaalm.memory is mem
    assert kaalm.http_client is sync_factory
    assert kaalm.http_async_client is async_factory
    assert kaalm.trace_context is trace
    assert kaalm.complete_task is complete
    assert kaalm.langchain_chat_anthropic is chat_anthropic


def test_unknown_attribute_is_attribute_error():
    with pytest.raises(AttributeError):
        _ = kaalm.does_not_exist


def test_dir_lists_the_abi():
    assert dir(kaalm) == [
        "TaskAlreadyCompleted", "complete_task", "gateway", "http_async_client", "http_client",
        "langchain_chat_anthropic", "memory", "trace_context",
    ]


def test_task_already_completed_needs_no_binding():
    """The exception class is static: a handler can name it in an except
    clause at import time, before or without a bound runtime."""
    assert issubclass(kaalm.TaskAlreadyCompleted, Exception)
