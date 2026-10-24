// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// defaultMemoryDir is where the operator mounts spec.persistence, and so where
// state that must outlive the Pod belongs. It matters for hibernation: a woken
// agent is a brand new Pod, and anything held only in RAM is gone. Override
// with KAALM_MEMORY_DIR if the Agent sets a custom spec.persistence.mountPath.
const defaultMemoryDir = "/var/agent/memory"

// dedupBufferSize bounds the redelivery window (contract item 7).
const dedupBufferSize = 1024

// Compaction folds the log into the snapshot once the log outgrows
// compactLogMultiple times the snapshot, with compactLogMinBytes as a floor
// so a small or missing snapshot does not compact on every write. Variables,
// not constants, so tests can shrink them.
var (
	compactLogMultiple = int64(4)
	compactLogMinBytes = int64(1 << 20)
)

// persistedState is everything the agent carries across a restart. It is the
// snapshot in state.json; writes since the snapshot live in state.log. The
// JSON keys deliberately match the kaalm-agent-python state file, so a volume
// written by either runtime reads the same in both.
type persistedState struct {
	// KV holds handler state, written through Memory under a "user/" key
	// prefix so it can never collide with contract-owned keys.
	KV map[string]json.RawMessage `json:"kv"`
	// Dedup maps a delivered messageId to the reply that was returned for it,
	// so a redelivery after a wake is answered from cache rather than
	// processed twice (runtime contract item 7).
	Dedup map[string]Response `json:"dedup"`
	// DedupOrder tracks messageIds oldest-first, so eviction at the buffer
	// cap drops the least recently delivered id, not a random one.
	DedupOrder []string `json:"dedupOrder"`
}

// logRecord is one line of state.log. The record shape is the same in
// kaalm-agent-python, so each runtime parses the other's log: remember
// carries id and reply, set carries key and value, delete carries key. Keys
// are full keys, user/ prefix included.
type logRecord struct {
	Op    string          `json:"op"`
	ID    string          `json:"id,omitempty"`
	Reply *Response       `json:"reply,omitempty"`
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// store persists agent state to the mounted volume as a snapshot (state.json)
// plus an append-only log of writes since it (state.log). Each write appends
// one fsynced record, so its cost does not grow with the dedup window, and a
// write that returned survives a process or node crash. With no writable
// volume it degrades to memory only, which is correct for an agent that does
// not enable hibernation: the contract requires persistence only when
// hibernationEnabled is set, and that in turn requires a PVC.
type store struct {
	path    string // snapshot; empty when there is no usable volume
	logPath string

	mu      sync.Mutex
	state   persistedState
	logFile *os.File // opened on the first append
	// logBytes is the length of the log's good prefix: whole records only.
	// A failed append truncates back to it.
	logBytes int64
	// logUnterminated is set when the good prefix ends in a record without
	// its newline, so the next append starts a new line first.
	logUnterminated bool
	snapshotBytes   int64
}

func newStore(dir string) *store {
	s := &store{state: persistedState{
		KV:    map[string]json.RawMessage{},
		Dedup: map[string]Response{},
	}}
	if dir == "" {
		return s
	}
	// Probe rather than assume: persistence is optional, and an agent without
	// a PVC must still run instead of crash-looping on a missing directory.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("memory: %s unusable (%v); continuing without persistence", dir, err)
		return s
	}
	s.path = filepath.Join(dir, "state.json")
	s.logPath = filepath.Join(dir, "state.log")
	s.load()
	if fi, err := os.Stat(s.path); err == nil {
		s.snapshotBytes = fi.Size()
	}
	if s.replayLog() {
		// Seal the recovered state in the snapshot and empty the log, so the
		// next append never lands after a torn or bad line.
		s.mu.Lock()
		s.compactLocked()
		s.mu.Unlock()
	}
	return s
}

// replayLog applies state.log over the loaded snapshot and reports whether
// the log held any bytes. A torn last line (a crash mid-append) is dropped; a
// bad line before it stops the replay and keeps what came before. Either way
// the log is cut back to the records that applied, so a later append cannot
// join a bad line even when the compaction after replay fails.
func (s *store) replayLog() bool {
	raw, err := os.ReadFile(s.logPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("memory: reading %s: %v; using the snapshot alone", s.logPath, err)
		}
		return false
	}
	if len(raw) == 0 {
		return false
	}
	// Under this runtime the log is truncated after every snapshot rename, so
	// a log strictly older than the snapshot was left behind by a runtime that
	// rewrote state.json without it. Its records predate the snapshot.
	if snap, err := os.Stat(s.path); err == nil {
		if lf, err := os.Stat(s.logPath); err == nil && snap.ModTime().After(lf.ModTime()) {
			log.Printf("memory: %s is older than %s; discarding it", s.logPath, s.path)
			if err := os.Truncate(s.logPath, 0); err != nil {
				log.Printf("memory: truncating %s: %v", s.logPath, err)
			}
			return false
		}
	}
	lines := bytes.Split(raw, []byte("\n"))
	applied := 0
	var pos, good int64 // good: end of the last line that applied or was blank
	for i, line := range lines {
		last := i == len(lines)-1
		end := pos + int64(len(line))
		if !last {
			end++ // the newline
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if !last {
				good = end
			}
			pos = end
			continue
		}
		var rec logRecord
		ok := json.Unmarshal(line, &rec) == nil && s.applyLocked(rec)
		if !ok {
			if last {
				log.Printf("memory: dropping an incomplete last record in %s", s.logPath)
			} else {
				log.Printf("memory: %s line %d is not a valid record; replay stops there", s.logPath, i+1)
			}
			break
		}
		applied++
		good = end
		s.logUnterminated = last
		pos = end
	}
	log.Printf("memory: replayed %d records from %s", applied, s.logPath)
	s.logBytes = int64(len(raw))
	if good < s.logBytes {
		if err := truncateLog(s.logPath, good); err != nil {
			log.Printf("memory: truncating %s: %v", s.logPath, err)
		} else {
			s.logBytes = good
		}
	}
	return true
}

// applyLocked applies one log record to RAM and reports whether it was valid.
// The caller holds the lock, or owns the store during construction.
func (s *store) applyLocked(rec logRecord) bool {
	switch rec.Op {
	case "remember":
		if rec.ID == "" || rec.Reply == nil {
			return false
		}
		s.rememberLocked(rec.ID, *rec.Reply, dedupBufferSize)
	case "set":
		if rec.Key == "" {
			return false
		}
		value := rec.Value
		if value == nil {
			value = json.RawMessage("null")
		}
		s.state.KV[rec.Key] = value
	case "delete":
		if rec.Key == "" {
			return false
		}
		delete(s.state.KV, rec.Key)
	default:
		return false
	}
	return true
}

// load reads prior state. A missing or corrupt file is not fatal: the agent
// starts empty rather than refusing to serve.
func (s *store) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("memory: reading %s: %v; starting empty", s.path, err)
		}
		return
	}
	var loaded persistedState
	if err := json.Unmarshal(raw, &loaded); err != nil {
		log.Printf("memory: %s is not readable state (%v); starting empty", s.path, err)
		return
	}
	if loaded.KV != nil {
		s.state.KV = loaded.KV
	}
	if loaded.Dedup != nil {
		s.state.Dedup = loaded.Dedup
	}
	// A state file whose order list is missing or out of step with the dedup
	// map (hand-edited, or written by an older runtime) gets one synthesized
	// so eviction still works. Entries in the order list without a dedup
	// entry (or vice versa) are reconciled.
	s.state.DedupOrder = nil
	for _, id := range loaded.DedupOrder {
		if _, ok := s.state.Dedup[id]; ok {
			s.state.DedupOrder = append(s.state.DedupOrder, id)
		}
	}
	if len(s.state.DedupOrder) < len(s.state.Dedup) {
		known := map[string]bool{}
		for _, id := range s.state.DedupOrder {
			known[id] = true
		}
		for id := range s.state.Dedup {
			if !known[id] {
				s.state.DedupOrder = append(s.state.DedupOrder, id)
			}
		}
	}
	log.Printf("memory: recovered %d cached replies and %d keys from %s",
		len(s.state.Dedup), len(s.state.KV), s.path)
}

// appendRecord writes one record to the log and fsyncs it, so the write
// survives a crash once the caller returns. The caller holds the lock.
func (s *store) appendRecord(rec logRecord) {
	if s.path == "" {
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		log.Printf("memory: encoding a %s record: %v", rec.Op, err)
		return
	}
	raw = append(raw, '\n')
	if s.logUnterminated {
		raw = append([]byte{'\n'}, raw...)
	}
	if err := s.writeLog(raw); err != nil {
		// The write is applied in RAM; a snapshot captures it if the volume
		// accepts one. The log is truncated only after a successful rename,
		// so a failed compaction loses nothing already logged.
		log.Printf("memory: appending to %s: %v", s.logPath, err)
		s.dropPartialAppend()
		s.compactLocked()
		return
	}
	s.logUnterminated = false
	s.logBytes += int64(len(raw))
	if s.logBytes > max(compactLogMultiple*s.snapshotBytes, compactLogMinBytes) {
		s.compactLocked()
	}
}

// dropPartialAppend cuts the log back to its good prefix after a failed
// append, so bytes a partial write left behind cannot strand later records.
// Truncating needs no free space. The handle is closed and reopened on the
// next append, since a write error can leave it unusable. The caller holds
// the lock.
func (s *store) dropPartialAppend() {
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
	if err := truncateLog(s.logPath, s.logBytes); err != nil && !os.IsNotExist(err) {
		log.Printf("memory: truncating %s: %v", s.logPath, err)
	}
}

// truncateLog cuts the log at path to size bytes and fsyncs it.
func truncateLog(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeLog appends raw to the log in one write and fsyncs it, opening the
// log on first use. The caller holds the lock.
func (s *store) writeLog(raw []byte) error {
	if s.logFile == nil {
		_, statErr := os.Stat(s.logPath)
		f, err := os.OpenFile(s.logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		s.logFile = f
		if os.IsNotExist(statErr) {
			// A new file's directory entry must be durable too.
			syncDir(filepath.Dir(s.logPath))
		}
	}
	if _, err := s.logFile.Write(raw); err != nil {
		return err
	}
	return s.logFile.Sync()
}

// compactLocked writes the whole state as a new snapshot and then empties
// the log. The snapshot goes through a temp file, fsync, and rename, so a
// crash leaves either the old snapshot or the new one, never half of one.
// The caller holds the lock.
func (s *store) compactLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.Marshal(s.state)
	if err != nil {
		log.Printf("memory: encoding state: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := writeSynced(tmp, raw); err != nil {
		_ = os.Remove(tmp)
		log.Printf("memory: writing %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		log.Printf("memory: replacing %s: %v", s.path, err)
		return
	}
	syncDir(filepath.Dir(s.path))
	s.snapshotBytes = int64(len(raw))
	// A crash before this truncate leaves records the snapshot already holds;
	// replaying them on the next start changes nothing.
	if s.logFile != nil {
		if err = s.logFile.Truncate(0); err == nil {
			err = s.logFile.Sync()
		}
	} else if err = os.Truncate(s.logPath, 0); os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		// The log keeps records the snapshot now holds; replaying them is
		// harmless, and a later append still lands after its good prefix.
		log.Printf("memory: truncating %s: %v", s.logPath, err)
		return
	}
	s.logBytes = 0
	s.logUnterminated = false
}

// close folds any logged writes into the snapshot, so after a clean shutdown
// state.json alone is complete, and closes the log. A write after close
// reopens the log, so it is still durable.
func (s *store) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return
	}
	if s.logBytes > 0 {
		s.compactLocked()
	}
	if s.logFile != nil {
		if err := s.logFile.Close(); err != nil {
			log.Printf("memory: closing %s: %v", s.logPath, err)
		}
		s.logFile = nil
	}
}

// writeSynced writes raw to a new file at path and fsyncs it before closing.
func writeSynced(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir fsyncs a directory, so a file created or renamed in it survives a
// node crash. A failure is logged, not fatal: the data itself is synced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		log.Printf("memory: opening %s to sync: %v", dir, err)
		return
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		log.Printf("memory: syncing %s: %v", dir, err)
	}
}

// recall returns the cached reply for a messageId, if this agent already
// answered it (possibly in a previous life, before hibernation). A hit
// refreshes the id's recency, so an actively redelivered id is not the one
// evicted at the cap.
func (s *store) recall(messageID string) (Response, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply, ok := s.state.Dedup[messageID]
	if ok {
		s.touchLocked(messageID)
	}
	return reply, ok
}

// remember records the reply for a messageId, evicting the least recently
// delivered ids when the window grows past size.
func (s *store) remember(messageID string, reply Response, size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rememberLocked(messageID, reply, size)
	s.appendRecord(logRecord{Op: "remember", ID: messageID, Reply: &reply})
}

// rememberLocked applies a remember to RAM. Remembering a known id replaces
// its reply without adding a second order entry. The caller holds the lock.
func (s *store) rememberLocked(messageID string, reply Response, size int) {
	if _, known := s.state.Dedup[messageID]; !known {
		s.state.DedupOrder = append(s.state.DedupOrder, messageID)
	}
	s.state.Dedup[messageID] = reply
	for len(s.state.DedupOrder) > size {
		oldest := s.state.DedupOrder[0]
		s.state.DedupOrder = s.state.DedupOrder[1:]
		delete(s.state.Dedup, oldest)
	}
}

// touchLocked moves a known messageId to the most-recent end of the order.
// The caller holds the lock.
func (s *store) touchLocked(messageID string) {
	for i, id := range s.state.DedupOrder {
		if id == messageID {
			s.state.DedupOrder = append(
				append(s.state.DedupOrder[:i:i], s.state.DedupOrder[i+1:]...), messageID)
			return
		}
	}
}

// getKV returns the raw value for a key, if present.
func (s *store) getKV(key string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.state.KV[key]
	return raw, ok
}

// setKV stores a raw value under a key and persists.
func (s *store) setKV(key string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.KV[key] = raw
	s.appendRecord(logRecord{Op: "set", Key: key, Value: raw})
}

// deleteKV removes a key and persists.
func (s *store) deleteKV(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state.KV, key)
	s.appendRecord(logRecord{Op: "delete", Key: key})
}
