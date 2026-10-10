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
// its own status write raises. That event carries nothing the next pass
// would read differently: the pass that wrote the status already acted on
// the spec, metadata, and status it wrote from, and it returned its own
// requeue for the next probe or budget fold. Without the filter, every
// status change runs a second full pass (the gateway Pod list, the spend
// folds, the credential read, validation).
//
// Only that one event is dropped. A spec, label, annotation, finalizer, or
// deletion change, a status write by any other client, a resync, and every
// create and delete are still admitted. The record is matched by the
// resourceVersion the write returned and used once. When the event arrives
// before the write returns, the pass runs as it would without the filter.
// The zero value is ready to use.
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
