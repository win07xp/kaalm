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
	"sync/atomic"
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
// capped every gateway replica at 20 requests per second and
// throttled the controller's per-minute channel validation.
//
// Subscribers learn when a watched Secret changes, so a caller can act on a
// change instead of waiting for its next read. A watcher with no subscriber
// (the gateway's) attaches no handler to its informers.
type Watcher struct {
	client kubernetes.Interface
	ctx    context.Context

	// SyncTimeout bounds how long a read waits for its informer's first GET
	// before reading live instead. A GET that fails is returned as the same
	// API error, so a misconfigured grant surfaces as that error, never as a
	// hang.
	SyncTimeout time.Duration
	// IdleTTL is how long an informer nobody has read outlives its last use.
	// Channel Secrets come and go with their channels; the janitor keeps the
	// watch count bounded by the Secrets still in use.
	IdleTTL time.Duration
	// ErrorTTL is how long reads of a Secret whose informer has not synced
	// return that informer's last failed GET before the next read replaces
	// it with a fresh informer. 0, the default, retries on every read, which
	// the controller relies on to retry a Forbidden while a new Role reaches
	// the authorizer. The informer's own list retry can pick up a grant
	// sooner.
	ErrorTTL time.Duration

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
	// failure is the informer's last failed list, or nil when its last list
	// succeeded or none has finished.
	failure atomic.Pointer[listFailure]
}

// listFailure is a failed informer GET and when it failed.
type listFailure struct {
	err error
	at  time.Time
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
// first use. A failed informer GET is returned as its API error, so the
// caller sees the same error a direct read would; see ErrorTTL for when the
// next read tries again. If the informer's GET neither succeeds nor fails
// within SyncTimeout the read goes live.
func (w *Watcher) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	entry := w.entryFor(namespace, name)
	synced, err := waitSynced(ctx, entry, w.SyncTimeout)
	if err != nil {
		return nil, err
	}
	if !synced {
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
		failure := entry.failure.Load()
		if entry.informer.HasSynced() || failure == nil || time.Since(failure.at) < w.ErrorTTL {
			entry.lastUsed = time.Now()
			return entry
		}
		// The informer never synced and its last GET failed ErrorTTL ago:
		// replace it, so this read issues one fresh GET. Concurrent readers
		// share the replacement.
		close(entry.stop)
	}
	entry := &secretInformer{
		stop:     make(chan struct{}),
		lastUsed: time.Now(),
	}
	lw := &cache.ListWatch{
		ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
			sec, err := w.client.CoreV1().Secrets(namespace).Get(w.ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				// An absent Secret is a valid, empty state: the watch below
				// delivers it when it appears.
				entry.failure.Store(nil)
				return &corev1.SecretList{}, nil
			}
			if err != nil {
				entry.failure.Store(&listFailure{err: err, at: time.Now()})
				return nil, err
			}
			entry.failure.Store(nil)
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
	entry.informer = cache.NewSharedInformer(lw, &corev1.Secret{}, 0)
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
// which would tax the very first read) until synced, a failed GET (returned
// as err), the timeout, or ctx.
func waitSynced(ctx context.Context, entry *secretInformer, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for !entry.informer.HasSynced() {
		if failure := entry.failure.Load(); failure != nil {
			return false, failure.err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true, nil
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
