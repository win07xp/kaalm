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
	"sync"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// heldEvent is one Event waiting for its status write.
type heldEvent struct {
	eventType, reason, message string
}

// heldEvents buffers the events a reconcile pass derives from a status change
// (a phase transition, a new Ready=False reason, a condition's rising edge)
// until the status write that persists the change succeeds. The informer cache can lag the pass's own
// last write, so a pass may see the old status, derive the same transition
// again, and lose its write to a conflict; emitting only after a successful
// write reports each transition once. Entries are keyed by the object the
// pass works on, which is unique to that pass, so concurrent reconciles never
// share one. The zero value is ready to use.
type heldEvents struct {
	mu sync.Mutex
	m  map[client.Object][]heldEvent
}

func (h *heldEvents) add(obj client.Object, eventType, reason, message string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[client.Object][]heldEvent{}
	}
	h.m[obj] = append(h.m[obj], heldEvent{eventType: eventType, reason: reason, message: message})
}

// take removes and returns the events held for obj.
func (h *heldEvents) take(obj client.Object) []heldEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	evs := h.m[obj]
	delete(h.m, obj)
	return evs
}

// flush removes the events held for obj and sends them when written is true:
// the pass's status write succeeded. A failed write drops them, and so does a
// pass that found its status already stored, because the pass that stored it
// sent them. rec may be nil.
func (h *heldEvents) flush(rec record.EventRecorder, obj client.Object, written bool) {
	evs := h.take(obj)
	if !written || rec == nil {
		return
	}
	for _, ev := range evs {
		rec.Event(obj, ev.eventType, ev.reason, ev.message)
	}
}
