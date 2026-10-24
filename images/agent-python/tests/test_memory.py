# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""Store semantics ported from the Go starter: probe-not-assume, degrade to
memory, tolerate corruption, atomic writes; plus the dedup window that
contract item 7 requires to survive hibernation, and the user/ prefix wall."""

from __future__ import annotations

import json
import os
import time

import pytest

from kaalm_agent import memory
from kaalm_agent.memory import Store, UserMemory

# The fixture the Go suite also uses, byte for byte, so a volume written by
# either runtime reads the same in both.
FIXTURE_SNAPSHOT = '{"kv":{"user/a":1},"dedup":{"m1":{"content":"one"}},"dedupOrder":["m1"]}'
FIXTURE_LOG = (
    '{"op":"remember","id":"m2","reply":{"content":"two"}}\n'
    '{"op":"set","key":"user/b","value":"kept"}\n'
    '{"op":"delete","key":"user/a"}\n'
    '{"op":"remember","id":"m1","reply":{"content":"one"}}\n'
)


def write_state(d, snapshot, log_body):
    d.mkdir(exist_ok=True)
    if snapshot:
        (d / "state.json").write_text(snapshot)
    (d / "state.log").write_text(log_body)


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
    s.close()
    leftovers = {p.name for p in d.iterdir()}
    assert leftovers == {"state.json", "state.log"}
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


def test_remember_appends_to_log(tmp_path):
    """A mutation appends one record instead of rewriting the snapshot, so its
    cost no longer grows with the dedup window."""
    d = tmp_path / "mem"
    Store(str(d)).remember("m1", {"content": "one"})
    lines = (d / "state.log").read_text().splitlines()
    assert [json.loads(line) for line in lines] == [
        {"op": "remember", "id": "m1", "reply": {"content": "one"}}
    ]
    assert not (d / "state.json").exists()


def test_replays_log_over_snapshot(tmp_path):
    d = tmp_path / "mem"
    write_state(d, FIXTURE_SNAPSHOT, FIXTURE_LOG)
    s = Store(str(d))
    assert s._dedup == {"m1": {"content": "one"}, "m2": {"content": "two"}}
    assert s._dedup_order == ["m1", "m2"]
    assert s._kv == {"user/b": "kept"}
    # Load seals the replayed state into the snapshot and empties the log.
    assert (d / "state.log").read_text() == ""
    snap = json.loads((d / "state.json").read_text())
    assert "m2" in snap["dedup"] and "user/b" in snap["kv"]


def test_torn_last_line_is_dropped(tmp_path):
    """A crash mid-append leaves a partial last line; it is dropped, and the
    next append does not glue onto it."""
    d = tmp_path / "mem"
    write_state(d, FIXTURE_SNAPSHOT, FIXTURE_LOG + '{"op":"set","key":"user/c","val')
    s = Store(str(d))
    assert s.get("user/c") is None
    assert s.get("user/b") == "kept"
    assert s.recall("m2") == {"content": "two"}
    s.put("user/d", 4)
    third = Store(str(d))
    assert third.get("user/d") == 4
    assert third.get("user/b") == "kept"


def test_bad_middle_record_stops_replay(tmp_path):
    d = tmp_path / "mem"
    write_state(d, "", (
        '{"op":"set","key":"user/x","value":1}\n'
        "{not json}\n"
        '{"op":"set","key":"user/y","value":2}\n'
    ))
    s = Store(str(d))
    assert s.get("user/x") == 1
    assert s.get("user/y") is None


def test_compacts_when_log_outgrows_snapshot(tmp_path, monkeypatch):
    monkeypatch.setattr(memory, "COMPACT_LOG_MIN_BYTES", 256)
    monkeypatch.setattr(memory, "COMPACT_LOG_MULTIPLE", 1)
    d = tmp_path / "mem"
    s = Store(str(d))
    for i in range(100):
        s.remember(f"m{i}", {"content": "reply"}, cap=4)
        if (d / "state.log").read_text() == "":
            break
    else:
        pytest.fail("the log must be compacted once it outgrows the threshold")
    snap = json.loads((d / "state.json").read_text())
    assert snap["dedup"] == s._dedup
    s.remember("after", {"content": "x"}, cap=4)
    assert (d / "state.log").read_text() != ""


def test_close_compacts(tmp_path):
    d = tmp_path / "mem"
    s = Store(str(d))
    s.remember("m1", {"content": "one"})
    s.put("user/k", "v")
    s.close()
    assert (d / "state.log").read_text() == ""
    snap = json.loads((d / "state.json").read_text())
    assert "m1" in snap["dedup"] and snap["kv"]["user/k"] == "v"
    assert not (d / "state.json.tmp").exists()
    after = Store(str(d))
    assert after.recall("m1") == {"content": "one"}
    assert after.get("user/k") == "v"


def test_replay_over_snapshot_is_harmless(tmp_path):
    """A crash between the snapshot rename and the log truncate leaves records
    in the log that the snapshot already holds; replaying them changes
    nothing."""
    d = tmp_path / "mem"
    write_state(d, FIXTURE_SNAPSHOT, (
        '{"op":"remember","id":"m1","reply":{"content":"one"}}\n'
        '{"op":"set","key":"user/a","value":1}\n'
    ))
    s = Store(str(d))
    assert s._dedup_order == ["m1"]
    assert s._kv == {"user/a": 1}


def test_snapshot_newer_than_log_discards_log(tmp_path):
    """A log older than the snapshot means an older runtime rewrote state.json
    after the log's last append; its records are stale."""
    d = tmp_path / "mem"
    write_state(d, '{"kv":{},"dedup":{},"dedupOrder":[]}', '{"op":"set","key":"user/stale","value":1}\n')
    past = time.time() - 2
    os.utime(d / "state.log", (past, past))
    s = Store(str(d))
    assert s.get("user/stale") is None
    assert (d / "state.log").read_text() == ""


def block_compaction(d):
    """Make every snapshot write fail, as a volume with room for a small
    append but not a whole snapshot would, by putting a non-empty directory
    where the temp file goes. Returns a function that lifts the block."""
    tmp = d / "state.json.tmp"
    (tmp / "keep").mkdir(parents=True)

    def unblock():
        (tmp / "keep").rmdir()
        tmp.rmdir()

    return unblock


def test_append_after_failed_compaction_survives(tmp_path):
    """A torn last line is cut from the log during replay, not only by the
    compaction after it, so writes still survive when that compaction fails."""
    d = tmp_path / "mem"
    write_state(d, FIXTURE_SNAPSHOT, FIXTURE_LOG + '{"op":"set","key":"user/c","val')
    unblock = block_compaction(d)
    s = Store(str(d))
    s.put("user/d", 4)
    s.put("user/e", 5)
    unblock()
    nxt = Store(str(d))
    assert nxt.get("user/b") == "kept"
    assert nxt.get("user/d") == 4
    assert nxt.get("user/e") == 5
    assert nxt.get("user/c") is None


def test_append_after_unterminated_record_survives(tmp_path):
    """A valid last record without its newline is kept, and the next append
    starts on a line of its own instead of joining it."""
    d = tmp_path / "mem"
    write_state(d, FIXTURE_SNAPSHOT, FIXTURE_LOG + '{"op":"set","key":"user/c","value":3}')
    unblock = block_compaction(d)
    s = Store(str(d))
    s.put("user/d", 4)
    unblock()
    nxt = Store(str(d))
    assert nxt.get("user/b") == "kept"
    assert nxt.get("user/c") == 3
    assert nxt.get("user/d") == 4


def test_failed_append_leaves_no_partial_record(tmp_path):
    """A failed append leaves no partial bytes behind, so later appends are
    not stranded after a bad line when the compaction that follows also
    fails."""
    d = tmp_path / "mem"
    s = Store(str(d))
    s.put("user/a", 1)
    unblock = block_compaction(d)
    # Simulate a write that failed part way: partial bytes reach the file,
    # and the descriptor the store holds can no longer write.
    with open(d / "state.log", "a") as f:
        f.write('{"op":"set","ke')
    os.close(s._log_fd)
    s._log_fd = os.open(d / "state.log", os.O_RDONLY)
    s.put("user/b", 2)  # fails, and so does its compaction
    s.put("user/c", 3)
    unblock()
    nxt = Store(str(d))
    assert nxt.get("user/a") == 1
    assert nxt.get("user/c") == 3
