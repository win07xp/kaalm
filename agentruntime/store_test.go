// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStore_CachesAndEvictsOldestFirst(t *testing.T) {
	s := newStore(t.TempDir())
	if _, ok := s.recall("m1"); ok {
		t.Fatal("empty store must miss")
	}
	s.remember("m1", Response{Content: "one"}, 2)
	if r, ok := s.recall("m1"); !ok || r.Content != "one" {
		t.Fatalf("cached reply wrong: %+v ok=%v", r, ok)
	}
	s.remember("m2", Response{Content: "two"}, 2)
	s.remember("m3", Response{Content: "three"}, 2)
	if len(s.state.Dedup) != 2 {
		t.Errorf("window should hold 2 entries, holds %d", len(s.state.Dedup))
	}
	// Eviction is ordered: m1 was the least recent, so it goes first.
	if _, ok := s.recall("m1"); ok {
		t.Error("the oldest entry must be the one evicted")
	}
	if _, ok := s.recall("m2"); !ok {
		t.Error("m2 must survive the eviction of m1")
	}
	if _, ok := s.recall("m3"); !ok {
		t.Error("the newest entry must be present")
	}
}

func TestStore_RecallRefreshesRecency(t *testing.T) {
	s := newStore(t.TempDir())
	s.remember("m1", Response{Content: "one"}, 2)
	s.remember("m2", Response{Content: "two"}, 2)
	// m1 is redelivered: it becomes the most recent, so the next eviction
	// must drop m2 instead.
	if _, ok := s.recall("m1"); !ok {
		t.Fatal("m1 must be cached")
	}
	s.remember("m3", Response{Content: "three"}, 2)
	if _, ok := s.recall("m2"); ok {
		t.Error("m2 was least recently seen and must be evicted")
	}
	if _, ok := s.recall("m1"); !ok {
		t.Error("recalled m1 must survive: recall refreshes recency")
	}
}

// The point of persisting: a hibernated agent is replaced by a new Pod, and
// the runtime contract requires it to still recognize a messageId it answered
// before (item 7).
func TestStore_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	first := newStore(dir)
	first.remember("m1", Response{Content: "one"}, 8)
	first.setKV("user/note", []byte(`"kept"`))

	// A brand new process over the same volume, as a woken Pod would be.
	second := newStore(dir)
	if r, ok := second.recall("m1"); !ok || r.Content != "one" {
		t.Errorf("cached reply did not survive the restart: %+v ok=%v", r, ok)
	}
	if raw, ok := second.getKV("user/note"); !ok || string(raw) != `"kept"` {
		t.Errorf("kv did not survive the restart: %q ok=%v", raw, ok)
	}
	// The order survives too, so eviction stays oldest-first after a wake.
	second.remember("m2", Response{Content: "two"}, 2)
	second.remember("m3", Response{Content: "three"}, 2)
	if _, ok := second.recall("m1"); ok {
		t.Error("m1 must be evicted first after the restart")
	}
}

// Persistence is optional: an agent without a PVC must still serve.
func TestStore_WithoutVolumeStaysInMemory(t *testing.T) {
	s := newStore("")
	s.remember("m1", Response{Content: "one"}, 8)
	if _, ok := s.recall("m1"); !ok {
		t.Error("a volume-less store must still dedup within the process")
	}
	if s.path != "" {
		t.Errorf("no volume means no state file, got %q", s.path)
	}
	// A path that cannot become a directory (its parent is a regular file)
	// must degrade to memory, not crash the agent at startup.
	notADir := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := newStore(filepath.Join(notADir, "memory"))
	if blocked.path != "" {
		t.Error("an unusable directory must fall back to memory only")
	}
	blocked.remember("m1", Response{Content: "one"}, 8)
	if _, ok := blocked.recall("m1"); !ok {
		t.Error("the fallback store must still work in memory")
	}
}

// A corrupt state file (crash mid-write on a pre-atomic runtime, disk fault)
// must mean a fresh start, not a crash loop.
func TestStore_CorruptStateStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newStore(dir)
	if _, ok := s.recall("m1"); ok {
		t.Error("corrupt state must load as empty")
	}
	// And the store must still be writable afterward.
	s.remember("m1", Response{Content: "one"}, 8)
	if _, ok := newStore(dir).recall("m1"); !ok {
		t.Error("store must recover to a working persisted state")
	}
}

// A state file written by the v0.2.0 starter template has a dedup map but no
// dedupOrder; the runtime synthesizes an order so eviction keeps working.
func TestStore_LegacyStateWithoutOrder(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"dedup":{"m1":{"content":"one"},"m2":{"content":"two"}},"seen":{"alice":3}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newStore(dir)
	if r, ok := s.recall("m1"); !ok || r.Content != "one" {
		t.Errorf("legacy dedup entry must load: %+v ok=%v", r, ok)
	}
	if got := len(s.state.DedupOrder); got != 2 {
		t.Errorf("synthesized order must cover the dedup map, got %d entries", got)
	}
	s.remember("m3", Response{Content: "three"}, 2)
	if len(s.state.Dedup) != 2 {
		t.Errorf("eviction must work on legacy state, dedup holds %d", len(s.state.Dedup))
	}
}

// flush goes through a temp file and rename, so a reader never sees a
// half-written state file.
func TestStore_FlushIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	s.remember("m1", Response{Content: "one"}, 8)
	if _, err := os.Stat(s.path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file must not linger after flush: %v", err)
	}
}
