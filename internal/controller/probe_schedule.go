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

package controller

import (
	"crypto/sha256"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// probeKey names the inputs a provider's liveness probe depends on: the
// object (its UID, so a provider recreated under the same name probes
// afresh), its spec (the generation), and the credential the probe sends,
// kept only as a hash so the schedule never holds the key itself.
type probeKey struct {
	uid        types.UID
	generation int64
	credential [sha256.Size]byte
}

func newProbeKey(obj client.Object, credential string) probeKey {
	return probeKey{uid: obj.GetUID(), generation: obj.GetGeneration(), credential: sha256.Sum256([]byte(credential))}
}

// probeRecord is the last probe of one provider: the inputs it ran with,
// its result, and when the next probe is due (the interval, or the backoff
// while failing).
type probeRecord[R any] struct {
	key    probeKey
	result R
	due    time.Time
}

// probeSchedule lets a provider reconciler dial the upstream only when a probe
// is due. A provider reconciles on many events that do not touch the probe's
// inputs (a referrer created, a budget write, a gateway Pod turning Ready),
// and a dial on each of them costs an upstream request per event. A pass
// probes when the schedule holds no record for the provider, when the spec or
// credential changed since the recorded probe, or when the recorded due time
// has come; every other pass reuses the recorded result. The record is in
// memory only, so a restarted or newly elected controller probes each
// provider once on its first pass. The zero value is ready to use.
type probeSchedule[R any] struct {
	mu   sync.Mutex
	last map[string]probeRecord[R]
}

// cached returns the recorded result and the time left until the next
// probe, when the provider has a record for the same inputs that is not yet
// due.
func (g *probeSchedule[R]) cached(name string, key probeKey, now time.Time) (R, time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.last[name]
	if !ok || rec.key != key || !now.Before(rec.due) {
		var zero R
		return zero, 0, false
	}
	return rec.result, rec.due.Sub(now), true
}

// record stores a probe's result and when the next one is due.
func (g *probeSchedule[R]) record(name string, key probeKey, result R, due time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[string]probeRecord[R]{}
	}
	g.last[name] = probeRecord[R]{key: key, result: result, due: due}
}

// forget drops the provider's record, so its next probing pass dials at
// once. A pass that ends before the probe calls it: whatever ended the pass
// (a missing Secret, a bad fallback, a held delete, a disabled probe) may be
// fixed without a spec or credential change, and the fix must show at once.
func (g *probeSchedule[R]) forget(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.last, name)
}
