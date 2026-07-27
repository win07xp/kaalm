# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Store semantics ported from the Go starter: probe-not-assume, degrade to
memory, tolerate corruption, atomic writes; plus the dedup window that
contract item 7 requires to survive hibernation, and the user/ prefix wall."""

from __future__ import annotations

import json

import pytest

from kaalm_agent.memory import Store, UserMemory


def test_roundtrip_and_restart_persistence(tmp_path):
    d = str(tmp_path / "mem")
    Store(d).put("greeting", {"count": 3})
    assert Store(d).get("greeting") == {"count": 3}


def test_no_directory_degrades_to_memory():
    s = Store(None)
    s.put("k", "v")
    assert s.get("k") == "v"
    assert not s.persistent


def test_unusable_directory_degrades(tmp_path):
    blocker = tmp_path / "file"
    blocker.write_text("i am a regular file")
    s = Store(str(blocker / "sub"))  # mkdir under a file must fail
    assert not s.persistent
    s.put("k", "v")  # and the store still works, in memory
    assert s.get("k") == "v"


def test_corrupt_state_starts_empty_and_recovers(tmp_path):
    d = tmp_path / "mem"
    d.mkdir()
    (d / "state.json").write_text("{ not json")
    s = Store(str(d))
    assert s.get("anything") is None
    s.put("fresh", 1)  # the corrupt file must not poison future writes
    assert Store(str(d)).get("fresh") == 1


def test_non_object_state_starts_empty(tmp_path):
    d = tmp_path / "mem"
    d.mkdir()
    (d / "state.json").write_text("[1, 2, 3]")
    assert Store(str(d)).get("anything") is None


def test_write_is_atomic_no_temp_residue(tmp_path):
    d = tmp_path / "mem"
    s = Store(str(d))
    s.put("k", "v")
    leftovers = [p.name for p in d.iterdir()]
    assert leftovers == ["state.json"]
    json.loads((d / "state.json").read_text())  # parses whole


def test_delete(tmp_path):
    s = Store(str(tmp_path / "mem"))
    s.put("k", "v")
    s.delete("k")
    assert s.get("k") is None
    s.delete("never-existed")  # deleting a missing key is not an error


def test_non_json_value_rejected_before_touching_state(tmp_path):
    s = Store(str(tmp_path / "mem"))
    with pytest.raises(TypeError):
        s.put("bad", object())
    assert s.get("bad") is None


def test_dedup_recall_miss_and_roundtrip(tmp_path):
    s = Store(str(tmp_path / "mem"))
    assert s.recall("m1") is None
    s.remember("m1", {"content": "r1"})
    assert s.recall("m1") == {"content": "r1"}


def test_dedup_survives_restart(tmp_path):
    """The hibernation case (contract item 7): a wake-replacement process must
    recognize messageIds the previous life already answered."""
    d = str(tmp_path / "mem")
    Store(d).remember("m1", {"content": "r1"})
    assert Store(d).recall("m1") == {"content": "r1"}


def test_dedup_lru_evicts_oldest_first(tmp_path):
    s = Store(str(tmp_path / "mem"))
    for i in range(4):
        s.remember(f"m{i}", i, cap=3)
    assert s.recall("m0") is None  # evicted
    assert s.recall("m1") == 1


def test_dedup_recall_refreshes_recency(tmp_path):
    s = Store(str(tmp_path / "mem"))
    for i in range(3):
        s.remember(f"m{i}", i, cap=3)
    s.recall("m0")  # refresh the oldest
    s.remember("m3", 3, cap=3)  # evicts m1 now, not m0
    assert s.recall("m0") == 0
    assert s.recall("m1") is None


def test_dedup_rewrite_same_id_does_not_grow_order(tmp_path):
    s = Store(str(tmp_path / "mem"))
    for _ in range(5):
        s.remember("m1", "r", cap=2)
    s.remember("m2", "r2", cap=2)
    assert s.recall("m1") == "r"  # not self-evicted by duplicate order entries


def test_order_list_recovered_when_stale(tmp_path):
    """A state file whose dedupOrder lost entries (older writer, manual edit)
    must not orphan replies: order is re-derived."""
    d = tmp_path / "mem"
    d.mkdir()
    (d / "state.json").write_text(json.dumps({
        "kv": {},
        "dedup": {"a": 1, "b": 2},
        "dedupOrder": ["a"],
    }))
    s = Store(str(d))
    assert s.recall("b") == 2
    s.remember("c", 3, cap=2)  # eviction must work despite the recovered order
    assert len([m for m in ("a", "b", "c") if s.recall(m) is not None]) == 2


def test_user_memory_prefix_wall(tmp_path):
    s = Store(str(tmp_path / "mem"))
    user = UserMemory(s)
    s.remember("secret-id", {"content": "cached reply"})
    s.put("runtime-internal", "x")

    user.put("mine", 1)
    assert user.get("mine") == 1
    assert s.get("user/mine") == 1  # lives under the prefix
    # The wall, both directions: user keys cannot alias runtime keys.
    assert user.get("runtime-internal") is None
    assert user.get("secret-id") is None
    user.delete("runtime-internal")
    assert s.get("runtime-internal") == "x"


def test_user_memory_persistence_flag(tmp_path):
    assert UserMemory(Store(str(tmp_path / "mem"))).persistent
    assert not UserMemory(Store(None)).persistent
