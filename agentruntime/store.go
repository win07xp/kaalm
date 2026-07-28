// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
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

// persistedState is everything the agent carries across a restart, rewritten
// whole on each mutation: an agent's working set here is a dedup window and
// handler key-values, not a database. The JSON keys deliberately match the
// kaalm-agent-python state file, so the two runtimes stay comparable.
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

// store persists agent state to the mounted volume. With no writable volume it
// degrades to memory only, which is correct for an agent that does not enable
// hibernation: the contract requires persistence only when hibernationEnabled
// is set, and that in turn requires a PVC.
type store struct {
	path string // empty when there is no usable volume

	mu    sync.Mutex
	state persistedState
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
	s.load()
	return s
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
	// A state file from the v0.2.0 starter carries the dedup map without the
	// order list; synthesize one so eviction still works. Entries in the
	// order list without a dedup entry (or vice versa) are reconciled.
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

// flush writes the current state. The caller holds the lock.
func (s *store) flush() {
	if s.path == "" {
		return
	}
	raw, err := json.Marshal(s.state)
	if err != nil {
		log.Printf("memory: encoding state: %v", err)
		return
	}
	// Write a temp file and rename, so a crash mid-write cannot leave a
	// half-written state file that fails to parse on the next start.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("memory: writing %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("memory: replacing %s: %v", s.path, err)
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
	if _, known := s.state.Dedup[messageID]; !known {
		s.state.DedupOrder = append(s.state.DedupOrder, messageID)
	}
	s.state.Dedup[messageID] = reply
	for len(s.state.DedupOrder) > size {
		oldest := s.state.DedupOrder[0]
		s.state.DedupOrder = s.state.DedupOrder[1:]
		delete(s.state.Dedup, oldest)
	}
	s.flush()
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
	s.flush()
}

// deleteKV removes a key and persists.
func (s *store) deleteKV(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state.KV, key)
	s.flush()
}
