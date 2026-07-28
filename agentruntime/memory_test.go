// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"testing"
)

func TestMemory_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Memory{s: newStore(dir)}

	var missing string
	if ok, err := m.Get("absent", &missing); ok || err != nil {
		t.Fatalf("absent key: ok=%v err=%v, want false nil", ok, err)
	}

	type prefs struct {
		Lang  string `json:"lang"`
		Count int    `json:"count"`
	}
	if err := m.Set("prefs/alice", prefs{Lang: "de", Count: 3}); err != nil {
		t.Fatal(err)
	}
	var got prefs
	if ok, err := m.Get("prefs/alice", &got); !ok || err != nil || got != (prefs{Lang: "de", Count: 3}) {
		t.Fatalf("round trip: ok=%v err=%v got=%+v", ok, err, got)
	}

	// Values persist: a fresh store over the same volume still has them.
	m2 := &Memory{s: newStore(dir)}
	got = prefs{}
	if ok, err := m2.Get("prefs/alice", &got); !ok || err != nil || got.Count != 3 {
		t.Fatalf("persisted value lost: ok=%v err=%v got=%+v", ok, err, got)
	}

	m2.Delete("prefs/alice")
	if ok, _ := m2.Get("prefs/alice", &got); ok {
		t.Error("deleted key must be gone")
	}
	m2.Delete("prefs/alice") // deleting an absent key is a no-op
}

// The "user/" prefix is a wall: handler keys live in a separate namespace
// from contract-owned state, so no handler key can ever collide with the
// dedup buffer.
func TestMemory_PrefixWall(t *testing.T) {
	s := newStore(t.TempDir())
	m := &Memory{s: s}
	if err := m.Set("dedup", "handler-owned"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.getKV("dedup"); ok {
		t.Error("handler keys must not land in the unprefixed namespace")
	}
	if raw, ok := s.getKV("user/dedup"); !ok || string(raw) != `"handler-owned"` {
		t.Errorf("handler key must live under user/: %q ok=%v", raw, ok)
	}
}

func TestMemory_DecodeMismatchReturnsError(t *testing.T) {
	m := &Memory{s: newStore("")}
	if err := m.Set("k", "a string"); err != nil {
		t.Fatal(err)
	}
	var wrong int
	ok, err := m.Get("k", &wrong)
	if !ok || err == nil {
		t.Fatalf("type mismatch must surface: ok=%v err=%v", ok, err)
	}
}
