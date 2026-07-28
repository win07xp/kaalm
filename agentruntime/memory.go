// Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.

package agentruntime

import (
	"encoding/json"
	"fmt"
)

// userKeyPrefix walls handler state off from contract-owned store keys, so a
// handler can never collide with the dedup buffer or future runtime state.
// The same wall exists in kaalm-agent-python.
const userKeyPrefix = "user/"

// Memory is the handler's persistent key-value store: backed by the mounted
// volume when the Agent enables persistence, in-memory otherwise, with the
// same quiet degradation the rest of the runtime state has. Values are stored
// as JSON, so anything json.Marshal accepts round-trips.
type Memory struct {
	s *store
}

// Get unmarshals the value stored under key into out and reports whether the
// key existed. A missing key leaves out untouched and returns (false, nil).
func (m *Memory) Get(key string, out any) (bool, error) {
	raw, ok := m.s.getKV(userKeyPrefix + key)
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return true, fmt.Errorf("memory: decoding %q: %w", key, err)
	}
	return true, nil
}

// Set stores a value under key, replacing any previous value.
func (m *Memory) Set(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("memory: encoding %q: %w", key, err)
	}
	m.s.setKV(userKeyPrefix+key, raw)
	return nil
}

// Delete removes a key. Deleting an absent key is a no-op.
func (m *Memory) Delete(key string) {
	m.s.deleteKV(userKeyPrefix + key)
}
