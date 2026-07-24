// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStore_CachesAndEvicts(t *testing.T) {
	s := newStore(t.TempDir())
	if _, ok := s.recall("m1"); ok {
		t.Fatal("empty store must miss")
	}
	s.remember("m1", ResponseEnvelope{Content: "one"}, 2)
	if r, ok := s.recall("m1"); !ok || r.Content != "one" {
		t.Fatalf("cached reply wrong: %+v ok=%v", r, ok)
	}
	s.remember("m2", ResponseEnvelope{Content: "two"}, 2)
	s.remember("m3", ResponseEnvelope{Content: "three"}, 2)
	if len(s.state.Dedup) != 2 {
		t.Errorf("window should hold 2 entries, holds %d", len(s.state.Dedup))
	}
	if _, ok := s.recall("m3"); !ok {
		t.Error("the newest entry must be present")
	}
}

// The point of persisting: a hibernated agent is replaced by a new Pod, and
// the runtime contract requires it to still recognize a messageId it answered
// before (item 7).
func TestStore_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	first := newStore(dir)
	first.remember("m1", ResponseEnvelope{Content: "one"}, 8)
	if n := first.note("alice"); n != 1 {
		t.Fatalf("first message from alice = %d, want 1", n)
	}

	// A brand new process over the same volume, as a woken Pod would be.
	second := newStore(dir)
	if r, ok := second.recall("m1"); !ok || r.Content != "one" {
		t.Errorf("cached reply did not survive the restart: %+v ok=%v", r, ok)
	}
	if n := second.note("alice"); n != 2 {
		t.Errorf("alice's count restarted at %d, want it to continue to 2", n)
	}
}

// Persistence is optional: an agent without a PVC must still serve.
func TestStore_WithoutVolumeStaysInMemory(t *testing.T) {
	s := newStore("")
	s.remember("m1", ResponseEnvelope{Content: "one"}, 8)
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
	blocked.remember("m1", ResponseEnvelope{Content: "one"}, 8)
	if _, ok := blocked.recall("m1"); !ok {
		t.Error("the fallback store must still work in memory")
	}
}

func TestShouldHeartbeat(t *testing.T) {
	t.Setenv("KAALM_TEMPLATE_HEARTBEAT", "auto")
	if !(&agent{isTask: false}).shouldHeartbeat() {
		t.Error("agent mode auto must heartbeat")
	}
	if (&agent{isTask: true}).shouldHeartbeat() {
		t.Error("task mode auto must not heartbeat")
	}
	t.Setenv("KAALM_TEMPLATE_HEARTBEAT", "off")
	if (&agent{isTask: false}).shouldHeartbeat() {
		t.Error("off must never heartbeat")
	}
}

func TestTaskAutocompleteStatus(t *testing.T) {
	if got := taskAutocompleteStatus(false, "success"); got != "" {
		t.Errorf("agent mode must never auto-complete, got %q", got)
	}
	if got := taskAutocompleteStatus(true, ""); got != "" {
		t.Errorf("unset env must not auto-complete, got %q", got)
	}
	if got := taskAutocompleteStatus(true, "success"); got != "success" {
		t.Errorf("task mode with env must return the status, got %q", got)
	}
}
