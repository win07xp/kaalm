# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The kaalm module ABI: exactly seven members (six bound by the runtime, plus
the TaskAlreadyCompleted exception class), loud when imported anywhere else."""

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


def test_bound_members_are_returned():
    gw, mem, sync_factory, async_factory, trace, complete = (object() for _ in range(6))
    kaalm._bind(
        gateway=gw, memory=mem, http_client=sync_factory, http_async_client=async_factory, trace_context=trace,
        complete_task=complete,
    )
    assert kaalm.gateway is gw
    assert kaalm.memory is mem
    assert kaalm.http_client is sync_factory
    assert kaalm.http_async_client is async_factory
    assert kaalm.trace_context is trace
    assert kaalm.complete_task is complete


def test_unknown_attribute_is_attribute_error():
    with pytest.raises(AttributeError):
        _ = kaalm.does_not_exist


def test_dir_lists_the_abi():
    assert dir(kaalm) == [
        "TaskAlreadyCompleted", "complete_task", "gateway", "http_async_client", "http_client", "memory",
        "trace_context",
    ]


def test_task_already_completed_needs_no_binding():
    """The exception class is static: a handler can name it in an except
    clause at import time, before or without a bound runtime."""
    assert issubclass(kaalm.TaskAlreadyCompleted, Exception)
