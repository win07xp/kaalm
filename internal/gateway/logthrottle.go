/*
Copyright 2026 The Kaalm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"sync"
	"time"
)

// logThrottle paces a log line per key: allow grants at most one line per key
// in each interval. The zero value is ready to use. Entries are never pruned,
// because the keys are ModelProvider names and that set is bounded by the
// cluster's provider count.
type logThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time // nil means time.Now; tests inject a fake clock
}

// allow reports whether a line for key may be written now, and if so records
// the time. A key is allowed when it has no entry or when at least every has
// passed since its last granted line.
func (t *logThrottle) allow(key string, every time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	at := now()
	if last, ok := t.last[key]; ok && at.Sub(last) < every {
		return false
	}
	if t.last == nil {
		t.last = make(map[string]time.Time)
	}
	t.last[key] = at
	return true
}
