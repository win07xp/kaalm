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
// wrapped as a one-item list and whose watch is filtered to metadata.name and
// starts at the API server's most recent version. A read is then a cache hit
// and a rotation lands on the watch event, which is the contract
// docs/src/security/credentials.md describes. Without a watcher every read is
// a live GET through the client's rate limiter, which is what capped every
// gateway replica at 20 requests per second and throttled the controller's
// per-minute channel validation.
//
// Once a watch from the most recent version has opened, a second GET
// confirms a Secret the informer holds: a Secret deleted between the read
// and the watch's start is in neither, and the informer would otherwise keep
// serving it.
//
// An informer whose GET is refused as Forbidden stops: the process may no
// longer read that Secret, and a later read starts a fresh informer.
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
	// ErrorTTL is how long reads of a Secret whose informer has not synced,
	// or whose GET was refused as Forbidden, return that informer's last
	// failed GET before the next read replaces it with a fresh informer. An
	// informer whose GET is refused as Forbidden stops at once instead of
	// retrying on its own, so a Secret the process may no longer read costs
	// nothing until it is read again. 0, the default, retries on every read,
	// which the controller relies on to retry a Forbidden while a new Role
	// reaches the authorizer.
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
	// revoked is set when the informer's GET was refused as Forbidden and
	// the informer was stopped for it. Reads answer failure until ErrorTTL,
	// then the next read replaces the entry.
	revoked atomic.Bool
	// stopOnce makes halt idempotent: the watch error handler, entryFor's
	// replacement, the janitor, and shutdown can each stop the same entry.
	stopOnce sync.Once

	// seen is the Secret as the informer's reflector last saw it: the latest
	// list's GET result, then each event a watch started at the most recent
	// version has delivered; nil when absent. seenGen counts lists, so a
	// watch that outlives its list never overwrites the next list's result.
	seenMu  sync.Mutex
	seen    *corev1.Secret
	seenGen uint64
}

// halt stops the informer; later calls do nothing.
func (e *secretInformer) halt() { e.stopOnce.Do(func() { close(e.stop) }) }

// setSeen records a list's result and starts a new list generation.
func (e *secretInformer) setSeen(sec *corev1.Secret) {
	e.seenMu.Lock()
	defer e.seenMu.Unlock()
	e.seenGen++
	e.seen = sec
}

// currentSeen returns the list generation and the Secret last seen in it.
func (e *secretInformer) currentSeen() (uint64, *corev1.Secret) {
	e.seenMu.Lock()
	defer e.seenMu.Unlock()
	return e.seenGen, e.seen
}

// recordSeen records sec as last seen, unless a later list has run since
// generation gen.
func (e *secretInformer) recordSeen(gen uint64, sec *corev1.Secret) {
	e.seenMu.Lock()
	defer e.seenMu.Unlock()
	if gen == e.seenGen {
		e.seen = sec
	}
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
// next read tries again. A read refused as Forbidden is answered from the
// stopped informer until ErrorTTL, never from its cache. If the informer's
// GET neither succeeds nor fails within SyncTimeout the read goes live.
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
		revoked := entry.revoked.Load()
		if (!revoked && (entry.informer.HasSynced() || failure == nil)) ||
			(failure != nil && time.Since(failure.at) < w.ErrorTTL) {
			entry.lastUsed = time.Now()
			return entry
		}
		// The informer never synced, or was stopped for a refused GET, and
		// its last GET failed ErrorTTL ago: replace it, so this read issues
		// one fresh GET. Concurrent readers share the replacement.
		entry.halt()
	}
	entry := &secretInformer{
		stop:     make(chan struct{}),
		lastUsed: time.Now(),
	}
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, _ metav1.ListOptions) (runtime.Object, error) {
			sec, err := w.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				// An absent Secret is a valid, empty state: the watch below
				// delivers it when it appears.
				entry.failure.Store(nil)
				entry.setSeen(nil)
				return &corev1.SecretList{}, nil
			}
			if err != nil {
				entry.failure.Store(&listFailure{err: err, at: time.Now()})
				return nil, err
			}
			entry.failure.Store(nil)
			entry.setSeen(sec)
			// A GET returns the Secret's own resourceVersion, the version
			// of its last write. Once enough writes anywhere in the cluster
			// push that version out of the API server's watch cache, a
			// watch from it ends at once with 410 Expired, and every relist
			// GETs the same version again. So the list reports no version
			// and the watch starts at the most recent one. It opens by
			// replaying the Secret as it is now, so a create or change since
			// the GET lands as an add or update. A Secret deleted since the
			// GET is in neither the GET nor the replay; the watch function
			// below confirms it.
			return &corev1.SecretList{Items: []corev1.Secret{*sec}}, nil
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
			secrets := w.client.CoreV1().Secrets(namespace)
			src, err := secrets.Watch(ctx, opts)
			if err != nil {
				return nil, err
			}
			if opts.ResourceVersion != "" {
				// A watch resumed from a known version delivers every later
				// event, deletes included.
				return src, nil
			}
			// The API server fixes a watch's start version before it
			// answers, so a GET that finds the Secret gone after that
			// proves the opening replay cannot carry it. An API server that
			// serves these watches from etcd rather than its watch cache
			// fixes the version a moment after it answers, so a delete in
			// that moment can still be missed.
			gen, seen := entry.currentSeen()
			var dead *corev1.Secret
			if seen != nil {
				// Any error but NotFound is left alone: the list's GET just
				// succeeded, the next relist's GET catches a refusal, and
				// this GET never records a failure.
				if _, err := secrets.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
					dead = seen
				}
			}
			return newConfirmedWatch(src, entry, gen, key, dead), nil
		},
	}
	entry.informer = cache.NewSharedInformer(getThenWatch{lw}, &corev1.Secret{}, 0)
	// A handler set before Run cannot fail.
	_ = entry.informer.SetWatchErrorHandlerWithContext(stopOnForbidden(entry))
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
// replay a running informer gives a newly added handler, nor the watch's
// opening replay of a Secret unchanged since the read. fn runs on the
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
		// An error means the informer has stopped: the janitor drops it,
		// or the next read replaces it.
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
// list (and a late handler's replay), relists and watch openings that bring
// back an unchanged object, and any other object: the API server filters the
// watch by name, but a fake clientset does not.
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

// stopOnForbidden is the informer's watch error handler. It logs as the
// default handler does, then stops the informer when its latest GET was
// refused as Forbidden: the process may no longer read the Secret (its Role
// was deleted with its channel or workload, or shrank), so retrying until
// IdleTTL only costs a forbidden GET and a log line per backoff. A refused
// watch alone is left to the reflector's relist, whose GET decides. It takes
// no watcher lock: the entry stays in the map for entryFor to replace.
func stopOnForbidden(entry *secretInformer) cache.WatchErrorHandlerWithContext {
	return func(ctx context.Context, r *cache.Reflector, err error) {
		cache.DefaultWatchErrorHandler(ctx, r, err)
		if f := entry.failure.Load(); f != nil && apierrors.IsForbidden(f.err) {
			entry.revoked.Store(true)
			entry.halt()
		}
	}
}

// waitSynced polls HasSynced closely (the standard helper polls at 100 ms,
// which would tax the very first read) until synced, a failed GET (returned
// as err), the timeout, or ctx. A revoked informer answers its refusal even
// if it had synced, so its stale cache is never served.
func waitSynced(ctx context.Context, entry *secretInformer, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		if entry.revoked.Load() {
			return false, entry.failure.Load().err
		}
		if entry.informer.HasSynced() {
			return true, nil
		}
		if failure := entry.failure.Load(); failure != nil {
			return false, failure.err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
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
				entry.halt()
				delete(w.informers, key)
			}
			w.mu.Unlock()
			return
		case <-ticker.C:
			w.mu.Lock()
			for key, entry := range w.informers {
				if time.Since(entry.lastUsed) > w.IdleTTL {
					entry.halt()
					delete(w.informers, key)
				}
			}
			w.mu.Unlock()
		}
	}
}

// getThenWatch opts the per-Secret informer out of client-go's streaming
// list (WatchListClient). A streaming list skips ListFunc and opens a watch
// that sends the initial state, so the single GET that records a read
// failure, and answers an absent Secret as empty, would never run.
type getThenWatch struct{ *cache.ListWatch }

// IsWatchListSemanticsUnSupported tells the reflector to list with ListFunc.
func (getThenWatch) IsWatchListSemanticsUnSupported() bool { return true }
