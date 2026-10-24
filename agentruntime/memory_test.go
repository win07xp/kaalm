// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"encoding/json"
	"strings"
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

// Memory writes go to the log under their full user/-prefixed key, so a
// replay restores them in the same namespace.
func TestMemory_SetIsLoggedWithPrefix(t *testing.T) {
	dir := t.TempDir()
	m := &Memory{s: newStore(dir)}
	if err := m.Set("k", 1); err != nil {
		t.Fatal(err)
	}
	m.Delete("gone")

	lines := strings.Split(strings.TrimSuffix(readLog(t, dir), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two log lines, got %q", lines)
	}
	var set, del map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &set); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &del); err != nil {
		t.Fatal(err)
	}
	if set["op"] != "set" || set["key"] != "user/k" || set["value"] != float64(1) {
		t.Errorf("set record = %s", lines[0])
	}
	if del["op"] != "delete" || del["key"] != "user/gone" {
		t.Errorf("delete record = %s", lines[1])
	}

	var got int
	if ok, err := (&Memory{s: newStore(dir)}).Get("k", &got); !ok || err != nil || got != 1 {
		t.Errorf("a new Memory must see the logged value: ok=%v err=%v got=%d", ok, err, got)
	}
}
