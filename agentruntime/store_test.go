// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

// Compaction goes through a temp file and rename, so a reader never sees a
// half-written state file.
func TestStore_CompactionIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	s.remember("m1", Response{Content: "one"}, 8)
	s.close()
	if _, err := os.Stat(s.path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file must not linger after compaction: %v", err)
	}
	readSnapshot(t, dir)
}

// The fixture the Python suite also uses, byte for byte, so a volume written
// by either runtime reads the same in both.
const (
	fixtureSnapshot = `{"kv":{"user/a":1},"dedup":{"m1":{"content":"one"}},"dedupOrder":["m1"]}`
	fixtureLog      = `{"op":"remember","id":"m2","reply":{"content":"two"}}` + "\n" +
		`{"op":"set","key":"user/b","value":"kept"}` + "\n" +
		`{"op":"delete","key":"user/a"}` + "\n" +
		`{"op":"remember","id":"m1","reply":{"content":"one"}}` + "\n"
)

func writeStateFiles(t *testing.T, dir, snapshot, logBody string) {
	t.Helper()
	if snapshot != "" {
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(snapshot), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "state.log"), []byte(logBody), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readLog(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "state.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func readSnapshot(t *testing.T, dir string) persistedState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st persistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state.json must parse whole: %v", err)
	}
	return st
}

// A mutation appends one record instead of rewriting the snapshot, so its
// cost no longer grows with the dedup window.
func TestStore_RememberAppendsToLog(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	s.remember("m1", Response{Content: "one"}, 8)

	lines := strings.Split(strings.TrimSuffix(readLog(t, dir), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one log line, got %d: %q", len(lines), lines)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if string(rec["op"]) != `"remember"` || string(rec["id"]) != `"m1"` {
		t.Errorf("record = %s", lines[0])
	}
	var reply Response
	if err := json.Unmarshal(rec["reply"], &reply); err != nil || reply.Content != "one" {
		t.Errorf("reply in record = %s (%v)", rec["reply"], err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Errorf("a single remember must not rewrite the snapshot: %v", err)
	}
}

func TestStore_ReplaysLogOverSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeStateFiles(t, dir, fixtureSnapshot, fixtureLog)
	s := newStore(dir)

	if r, ok := s.state.Dedup["m2"]; !ok || r.Content != "two" {
		t.Errorf("m2 from the log must load: %+v ok=%v", r, ok)
	}
	if _, ok := s.state.Dedup["m1"]; !ok {
		t.Error("m1 from the snapshot must load")
	}
	if !slices.Equal(s.state.DedupOrder, []string{"m1", "m2"}) {
		t.Errorf("DedupOrder = %v, want [m1 m2]", s.state.DedupOrder)
	}
	if raw, ok := s.state.KV["user/b"]; !ok || string(raw) != `"kept"` {
		t.Errorf("user/b = %s ok=%v", raw, ok)
	}
	if _, ok := s.state.KV["user/a"]; ok {
		t.Error("the delete record must remove user/a")
	}
	if len(s.state.KV) != 1 {
		t.Errorf("kv = %v, want only user/b", s.state.KV)
	}
	// Load seals the replayed state into the snapshot and empties the log.
	if got := readLog(t, dir); got != "" {
		t.Errorf("log must be empty after load, holds %q", got)
	}
	snap := readSnapshot(t, dir)
	if _, ok := snap.Dedup["m2"]; !ok {
		t.Error("snapshot must hold m2 after load")
	}
	if _, ok := snap.KV["user/b"]; !ok {
		t.Error("snapshot must hold user/b after load")
	}
}

// A crash mid-append leaves a partial last line; it is dropped, and the
// next append does not glue onto it.
func TestStore_TornLastLineIsDropped(t *testing.T) {
	dir := t.TempDir()
	writeStateFiles(t, dir, fixtureSnapshot, fixtureLog+`{"op":"set","key":"user/c","val`)
	s := newStore(dir)
	if _, ok := s.getKV("user/c"); ok {
		t.Error("a torn record must not load")
	}
	if _, ok := s.getKV("user/b"); !ok {
		t.Error("records before the torn line must load")
	}
	if _, ok := s.recall("m2"); !ok {
		t.Error("m2 before the torn line must load")
	}
	s.setKV("user/d", []byte(`4`))

	third := newStore(dir)
	if _, ok := third.getKV("user/d"); !ok {
		t.Error("a write after recovering from a torn line must survive a restart")
	}
	if _, ok := third.getKV("user/b"); !ok {
		t.Error("user/b must survive the second restart")
	}
}

// A record that does not parse in the middle of the log stops replay there;
// what came before it is kept.
func TestStore_BadMiddleRecordStopsReplay(t *testing.T) {
	dir := t.TempDir()
	writeStateFiles(t, dir, "", `{"op":"set","key":"user/x","value":1}`+"\n"+
		"{not json}\n"+
		`{"op":"set","key":"user/y","value":2}`+"\n")
	s := newStore(dir)
	if _, ok := s.getKV("user/x"); !ok {
		t.Error("the record before the bad one must load")
	}
	if _, ok := s.getKV("user/y"); ok {
		t.Error("records after a bad one must not load")
	}
}

func TestStore_CompactsWhenLogOutgrowsSnapshot(t *testing.T) {
	restoreMin, restoreMul := compactLogMinBytes, compactLogMultiple
	compactLogMinBytes, compactLogMultiple = 256, 1
	t.Cleanup(func() { compactLogMinBytes, compactLogMultiple = restoreMin, restoreMul })

	dir := t.TempDir()
	s := newStore(dir)
	compacted := false
	for i := range 100 {
		s.remember(string(rune('a'+i%26))+strings.Repeat("x", i), Response{Content: "reply"}, 4)
		if readLog(t, dir) == "" {
			compacted = true
			break
		}
	}
	if !compacted {
		t.Fatal("the log must be compacted once it outgrows the threshold")
	}
	snap := readSnapshot(t, dir)
	for id := range s.state.Dedup {
		if _, ok := snap.Dedup[id]; !ok {
			t.Errorf("snapshot is missing surviving id %q", id)
		}
	}
	if len(snap.Dedup) != len(s.state.Dedup) {
		t.Errorf("snapshot holds %d ids, store holds %d", len(snap.Dedup), len(s.state.Dedup))
	}
	s.remember("after", Response{Content: "x"}, 4)
	if readLog(t, dir) == "" {
		t.Error("a remember after compaction must append again")
	}
}

func TestStore_CloseCompacts(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	s.remember("m1", Response{Content: "one"}, 8)
	s.setKV("user/k", []byte(`"v"`))
	s.close()

	if got := readLog(t, dir); got != "" {
		t.Errorf("log must be empty after close, holds %q", got)
	}
	snap := readSnapshot(t, dir)
	if _, ok := snap.Dedup["m1"]; !ok {
		t.Error("snapshot must hold m1 after close")
	}
	if _, ok := snap.KV["user/k"]; !ok {
		t.Error("snapshot must hold user/k after close")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("no temp file may linger: %v", err)
	}
	next := newStore(dir)
	if _, ok := next.recall("m1"); !ok {
		t.Error("m1 must survive close and restart")
	}
	if _, ok := next.getKV("user/k"); !ok {
		t.Error("user/k must survive close and restart")
	}
}

// A crash between the snapshot rename and the log truncate leaves records in
// the log that the snapshot already holds; replaying them changes nothing.
func TestStore_ReplayOverSnapshotIsHarmless(t *testing.T) {
	dir := t.TempDir()
	writeStateFiles(t, dir,
		`{"kv":{"user/a":1},"dedup":{"m1":{"content":"one"}},"dedupOrder":["m1"]}`,
		`{"op":"remember","id":"m1","reply":{"content":"one"}}`+"\n"+
			`{"op":"set","key":"user/a","value":1}`+"\n")
	s := newStore(dir)
	if !slices.Equal(s.state.DedupOrder, []string{"m1"}) {
		t.Errorf("DedupOrder = %v, want [m1]", s.state.DedupOrder)
	}
	if len(s.state.KV) != 1 {
		t.Errorf("kv = %v, want one key", s.state.KV)
	}
}

// A log older than the snapshot means an older runtime rewrote state.json
// after the log's last append; its records are stale and must not be
// replayed over the newer snapshot.
func TestStore_SnapshotNewerThanLogDiscardsLog(t *testing.T) {
	dir := t.TempDir()
	writeStateFiles(t, dir, `{"kv":{},"dedup":{},"dedupOrder":[]}`,
		`{"op":"set","key":"user/stale","value":1}`+"\n")
	past := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "state.log"), past, past); err != nil {
		t.Fatal(err)
	}
	s := newStore(dir)
	if _, ok := s.getKV("user/stale"); ok {
		t.Error("a record from a log older than the snapshot must not load")
	}
	if got := readLog(t, dir); got != "" {
		t.Errorf("a stale log must be emptied, holds %q", got)
	}
}
