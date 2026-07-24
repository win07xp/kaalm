// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// memoryDir is where the operator mounts spec.persistence, and so where state
// that must outlive the Pod belongs. It matters for hibernation: a woken agent
// is a brand new Pod, and anything held only in RAM is gone. Override with
// KAALM_MEMORY_DIR if the Agent sets a custom spec.persistence.mountPath.
const defaultMemoryDir = "/var/agent/memory"

// persistedState is everything the agent carries across a restart. It is
// deliberately small and rewritten whole: an agent's working set here is a
// dedup window and a per-user counter, not a database.
type persistedState struct {
	// Dedup maps a delivered messageId to the reply that was returned for it,
	// so a redelivery after a wake is answered from cache rather than
	// processed twice (runtime contract item 7).
	Dedup map[string]ResponseEnvelope `json:"dedup"`
	// Seen counts messages per userId. The starter agent uses it to show that
	// memory survived; a real agent would keep conversation state here.
	Seen map[string]int `json:"seen"`
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
		Dedup: map[string]ResponseEnvelope{},
		Seen:  map[string]int{},
	}}
	if dir == "" {
		return s
	}
	// Probe rather than assume: persistence is optional, and an agent without a
	// PVC must still run instead of crash-looping on a missing directory.
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
	if loaded.Dedup != nil {
		s.state.Dedup = loaded.Dedup
	}
	if loaded.Seen != nil {
		s.state.Seen = loaded.Seen
	}
	log.Printf("memory: recovered %d cached replies and %d known users from %s",
		len(s.state.Dedup), len(s.state.Seen), s.path)
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
// answered it (possibly in a previous life, before hibernation).
func (s *store) recall(messageID string) (ResponseEnvelope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply, ok := s.state.Dedup[messageID]
	return reply, ok
}

// remember records the reply for a messageId, trimming the oldest entries when
// the window grows past size.
func (s *store) remember(messageID string, reply ResponseEnvelope, size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Dedup[messageID] = reply
	for len(s.state.Dedup) > size {
		for k := range s.state.Dedup { // map order is random, which is fine for eviction
			delete(s.state.Dedup, k)
			break
		}
	}
	s.flush()
}

// note counts a message from a user and returns the running total, so the
// agent can show that it remembers them across restarts.
func (s *store) note(userID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Seen[userID]++
	count := s.state.Seen[userID]
	s.flush()
	return count
}
