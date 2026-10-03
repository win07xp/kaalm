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

// Package secretwatch serves Secret reads from one GET-backed, name-filtered
// watch per referenced Secret, and tells subscribers when a watched Secret
// changes. The gateway and the controller share it: both hold get and watch
// on individual Secrets but never list.
package secretwatch

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Watcher serves Secret reads from single-object informers. The gateway and
// the controller hold get and watch on Secrets (the operator namespace
// outright, user-namespace Secrets through resourceNames-scoped grants) but
// never list, so an ordinary informer would hang on a forbidden LIST. Each
// referenced Secret therefore gets its own list-watch whose "list" is a GET
// wrapped as a one-item list and whose watch is filtered to metadata.name. A
// read is then a cache hit and a rotation lands on the watch event, which is
// the contract docs/src/security/credentials.md describes. Without a watcher
// every read is a live GET through the client's rate limiter, which is what
// capped every gateway replica at 20 requests per second (#170) and
// throttled the controller's per-minute channel validation.
//
// Subscribers learn when a watched Secret changes, so a caller can act on a
// change instead of waiting for its next read. A watcher with no subscriber
// (the gateway's) attaches no handler to its informers.
type Watcher struct {
	client kubernetes.Interface
	ctx    context.Context

	// SyncTimeout bounds how long a first read waits for its informer's
	// initial GET before reading live instead; a misconfigured grant then
	// surfaces as the API error it always was, never as a hang.
	SyncTimeout time.Duration
	// IdleTTL is how long an informer nobody has read outlives its last use.
	// Channel Secrets come and go with their channels; the janitor keeps the
	// watch count bounded by the Secrets still in use.
	IdleTTL time.Duration

	mu        sync.Mutex
	informers map[types.NamespacedName]*secretInformer

	// subMu guards subscribers and is never held while taking mu, so a
	// notification never waits on entryFor or the janitor. The only nesting
	// is mu then subMu.
	subMu       sync.RWMutex
	subscribers []func(types.NamespacedName)
}

type secretInformer struct {
	informer cache.SharedInformer
	stop     chan struct{}
	lastUsed time.Time
	// changes is the subscribers' change handler on informer; nil while the
	// watcher has no subscriber.
	changes cache.ResourceEventHandlerRegistration
}

// New builds a watcher whose informers stop when ctx ends.
func New(ctx context.Context, client kubernetes.Interface) *Watcher {
	w := &Watcher{
		client:      client,
		ctx:         ctx,
		SyncTimeout: 2 * time.Second,
		IdleTTL:     time.Hour,
		informers:   map[types.NamespacedName]*secretInformer{},
	}
	go w.janitor()
	return w
}

// Get returns the current Secret from its informer, starting the informer on
// first use. If the informer has not completed its initial GET within
// SyncTimeout the read goes live, so the caller sees the same error a direct
// read would.
func (w *Watcher) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	entry := w.entryFor(namespace, name)
	if !waitSynced(ctx, entry.informer, w.SyncTimeout) {
		return w.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	obj, exists, err := entry.informer.GetStore().GetByKey(namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, apierrors.NewNotFound(corev1.Resource("secrets"), name)
	}
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return nil, apierrors.NewNotFound(corev1.Resource("secrets"), name)
	}
	return sec, nil
}

func (w *Watcher) entryFor(namespace, name string) *secretInformer {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if entry, ok := w.informers[key]; ok {
		entry.lastUsed = time.Now()
		return entry
	}
	lw := &cache.ListWatch{
		ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
			sec, err := w.client.CoreV1().Secrets(namespace).Get(w.ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				// An absent Secret is a valid, empty state: the watch below
				// delivers it when it appears.
				return &corev1.SecretList{}, nil
			}
			if err != nil {
				return nil, err
			}
			return &corev1.SecretList{
				ListMeta: metav1.ListMeta{ResourceVersion: sec.ResourceVersion},
				Items:    []corev1.Secret{*sec},
			}, nil
		},
		WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
			return w.client.CoreV1().Secrets(namespace).Watch(w.ctx, opts)
		},
	}
	entry := &secretInformer{
		informer: cache.NewSharedInformer(lw, &corev1.Secret{}, 0),
		stop:     make(chan struct{}),
		lastUsed: time.Now(),
	}
	if w.hasSubscribers() {
		// Subscribers live on the watcher, so an informer the janitor
		// stopped and a later read restarted gets the handler again. A
		// handler added before Run cannot fail.
		entry.changes, _ = entry.informer.AddEventHandler(w.changeHandler(key))
	}
	go entry.informer.Run(entry.stop)
	w.informers[key] = entry
	return entry
}

// Subscribe registers fn to learn when a watched Secret changes: it is
// created, updated to a new resourceVersion, or deleted after its watch's
// first sync. The informer's initial read is never reported, nor is the
// replay a running informer gives a newly added handler. fn runs on the
// informer's handler goroutine and must not block for long.
func (w *Watcher) Subscribe(fn func(types.NamespacedName)) {
	w.subMu.Lock()
	w.subscribers = append(w.subscribers, fn)
	w.subMu.Unlock()

	w.mu.Lock()
	defer w.mu.Unlock()
	for key, entry := range w.informers {
		if entry.changes != nil {
			continue
		}
		// An error means the informer is stopping; the janitor is about
		// to drop it.
		if reg, err := entry.informer.AddEventHandler(w.changeHandler(key)); err == nil {
			entry.changes = reg
		}
	}
}

func (w *Watcher) hasSubscribers() bool {
	w.subMu.RLock()
	defer w.subMu.RUnlock()
	return len(w.subscribers) > 0
}

// notify calls every subscriber with key, outside subMu.
func (w *Watcher) notify(key types.NamespacedName) {
	w.subMu.RLock()
	subs := append([]func(types.NamespacedName){}, w.subscribers...)
	w.subMu.RUnlock()
	for _, fn := range subs {
		fn(key)
	}
}

// changeHandler reports changes to the Secret at key. It skips the initial
// list (and a late handler's replay), relists that bring back an unchanged
// object, and any other object: the API server filters the watch by name,
// but a fake clientset does not.
func (w *Watcher) changeHandler(key types.NamespacedName) cache.ResourceEventHandlerDetailedFuncs {
	matches := func(obj interface{}) bool {
		m, err := apimeta.Accessor(obj)
		return err == nil && m.GetNamespace() == key.Namespace && m.GetName() == key.Name
	}
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj interface{}, isInInitialList bool) {
			if isInInitialList || !matches(obj) {
				return
			}
			w.notify(key)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if !matches(newObj) {
				return
			}
			oldSec, okOld := oldObj.(*corev1.Secret)
			newSec, okNew := newObj.(*corev1.Secret)
			if okOld && okNew && newSec.ResourceVersion != "" &&
				oldSec.ResourceVersion == newSec.ResourceVersion {
				return
			}
			w.notify(key)
		},
		DeleteFunc: func(obj interface{}) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				if tomb.Key == key.String() {
					w.notify(key)
				}
				return
			}
			if !matches(obj) {
				return
			}
			w.notify(key)
		},
	}
}

// waitSynced polls HasSynced closely (the standard helper polls at 100 ms,
// which would tax the very first read) until synced, the timeout, or ctx.
func waitSynced(ctx context.Context, inf cache.SharedInformer, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !inf.HasSynced() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// janitor stops informers idle past IdleTTL and every informer when the
// watcher's context ends.
func (w *Watcher) janitor() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			w.mu.Lock()
			for key, entry := range w.informers {
				close(entry.stop)
				delete(w.informers, key)
			}
			w.mu.Unlock()
			return
		case <-ticker.C:
			w.mu.Lock()
			for key, entry := range w.informers {
				if time.Since(entry.lastUsed) > w.IdleTTL {
					close(entry.stop)
					delete(w.informers, key)
				}
			}
			w.mu.Unlock()
		}
	}
}
