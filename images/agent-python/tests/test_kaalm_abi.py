# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The kaalm module ABI: exactly two members, bound by the runtime, loud when
imported anywhere else."""

from __future__ import annotations

import pytest

import kaalm


def test_unbound_access_raises_runtime_error():
    with pytest.raises(RuntimeError, match="kaalm.gateway is only available"):
        _ = kaalm.gateway
    with pytest.raises(RuntimeError, match="kaalm.memory is only available"):
        _ = kaalm.memory


def test_bound_members_are_returned():
    gw, mem = object(), object()
    kaalm._bind(gateway=gw, memory=mem)
    assert kaalm.gateway is gw
    assert kaalm.memory is mem


def test_unknown_attribute_is_attribute_error():
    with pytest.raises(AttributeError):
        _ = kaalm.does_not_exist


def test_dir_lists_the_abi():
    assert dir(kaalm) == ["gateway", "memory"]
