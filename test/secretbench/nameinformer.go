//go:build secretbench

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

package main

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// nameInformerReader is prototype C: one informer per Secret, as today, but
// its list is a LIST filtered to metadata.name and its watch resumes from
// that LIST's resourceVersion. Today's watcher lists with a GET, which
// returns the Secret's own resourceVersion; that version can fall out of the
// API server's watch cache, so today's watch starts at the most recent
// version and needs a confirming GET to catch a delete in between. A LIST
// returns the collection's current version instead, the point the WATCH
// then starts from, so a delete between the two arrives as a watch event and
// no confirming GET is needed. That is one LIST and one WATCH per Secret
// instead of two GETs and one WATCH.
//
// A LIST, even a name-filtered one, needs the list verb on Secrets, which
// resourceNames-scoped grants do not give; the benchmark measures cost, not
// RBAC.
type nameInformerReader struct {
	client kubernetes.Interface

	mu      sync.Mutex
	entries map[types.NamespacedName]*informerEntry
}

func newNameInformerReader(_ context.Context, client kubernetes.Interface) *nameInformerReader {
	return &nameInformerReader{client: client, entries: map[types.NamespacedName]*informerEntry{}}
}

func (r *nameInformerReader) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return r.entryFor(namespace, name).read(ctx, r.client, namespace, name)
}

func (r *nameInformerReader) entryFor(namespace, name string) *informerEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if e, ok := r.entries[key]; ok {
		return e
	}
	e := &informerEntry{stop: make(chan struct{})}
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	secrets := r.client.CoreV1().Secrets(namespace)
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			opts.FieldSelector = selector
			list, err := secrets.List(ctx, opts)
			e.recordList(err)
			if err != nil {
				return nil, err
			}
			return list, nil
		},
		// The reflector passes the LIST's resourceVersion in opts, so the
		// watch resumes exactly where the list left off.
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = selector
			return secrets.Watch(ctx, opts)
		},
	}
	e.informer = cache.NewSharedInformer(listThenWatch{lw}, &corev1.Secret{}, 0)
	go e.informer.Run(e.stop)
	r.entries[key] = e
	return e
}

func (r *nameInformerReader) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, e := range r.entries {
		close(e.stop)
		delete(r.entries, key)
	}
}
