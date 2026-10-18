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

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ownWrites lets a provider reconciler's For() watch drop the update event
// its own status write raises. For a pass that reads the provider at or
// after that write, the event carries nothing it would read differently:
// the pass that wrote the status already acted on the spec, metadata, and
// status it wrote from, and it returned its own requeue for the next probe
// or budget fold. Without the filter, every status change runs a second full
// pass (the gateway Pod list, the spend folds, the credential read,
// validation).
//
// Only that one event is dropped. A spec, label, annotation, finalizer, or
// deletion change, a status write by any other client, a resync, and every
// create and delete are still admitted. The record is matched by the
// resourceVersion the write returned and used once. When the event arrives
// before the write returns, the pass runs as it would without the filter.
// The zero value is ready to use.
//
// A pass that reads the provider at any resourceVersion other than the
// recorded write drops the record (readStale), so the write's event runs one
// more pass. A read from before the write is the case that matters: the
// informer cache can trail the write, a pass on that stale copy can compute
// the status the copy already holds and skip its write, and dropping the
// write's event would then leave the status from before the change until the
// next requeue. A read newer than the write costs at most one extra pass.
type ownWrites struct {
	mu sync.Mutex
	rv map[client.ObjectKey]string
}

// record notes the resourceVersion of a status write the reconciler just
// made to obj.
func (w *ownWrites) record(obj client.Object) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rv == nil {
		w.rv = map[client.ObjectKey]string{}
	}
	w.rv[client.ObjectKeyFromObject(obj)] = obj.GetResourceVersion()
}

// readStale is called with the provider a pass just read. When the read is at
// any resourceVersion other than the recorded write, it drops the record, so
// that write's event runs one more pass. resourceVersions are opaque, so they
// are compared for equality only.
func (w *ownWrites) readStale(obj client.Object) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := client.ObjectKeyFromObject(obj)
	if rv, ok := w.rv[key]; ok && rv != obj.GetResourceVersion() {
		delete(w.rv, key)
	}
}

// forget drops the record for a provider that is gone.
func (w *ownWrites) forget(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.rv, client.ObjectKey{Name: name})
}

// skipOwn is the For() predicate: it rejects an update whose new object is
// the recorded write, and admits everything else. A resync (the same
// resourceVersion on both sides) is always admitted.
func (w *ownWrites) skipOwn() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			rv := e.ObjectNew.GetResourceVersion()
			if rv == e.ObjectOld.GetResourceVersion() {
				return true
			}
			key := client.ObjectKeyFromObject(e.ObjectNew)
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.rv[key] != rv {
				return true
			}
			delete(w.rv, key)
			return false
		},
	}
}
