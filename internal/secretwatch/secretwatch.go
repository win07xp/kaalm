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

// Package secretwatch serves Secret reads from one name-selected list and
// watch per referenced Secret, and tells subscribers when a watched Secret
// changes. The gateway and the controller share it: both hold get, list, and
// watch on individual Secrets, and the API server allows a list or watch of
// a resourceNames-scoped grant only with an exact metadata.name selector.
package secretwatch

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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
// the controller read Secrets they hold get, list, and watch on: the
// operator namespace outright, user-namespace Secrets through
// resourceNames-scoped grants. The API server checks a list or watch against
// resourceNames only when it carries an exact metadata.name field selector,
// so each referenced Secret gets its own informer that lists with that
// selector and watches with it from the list's resourceVersion. A Secret
// changed or deleted between the two arrives as a watch event. A read is
// then a cache hit and a rotation lands on the watch event, which is the
// contract docs/src/security/credentials.md describes. Without a watcher
// every read is a live GET through the client's rate limiter, which is what
// capped every gateway replica at 20 requests per second and throttled the
// controller's per-minute channel validation.
//
// An informer whose list is refused as Forbidden stops. When a GET of the
// Secret is refused too, the process may no longer read it, and reads answer
// the refusal. When the GET is allowed, the Role predates the list verb (an
// upgrade or rollback in progress), and reads go live through GET. Either
// way a later read starts a fresh informer once ErrorTTL has passed.
//
// Subscribers learn when a watched Secret changes, so a caller can act on a
// change instead of waiting for its next read. A watcher with no subscriber
// (the gateway's) attaches no handler to its informers.
type Watcher struct {
	client kubernetes.Interface
	ctx    context.Context

	// SyncTimeout bounds how long a read waits for its informer's first list
	// before reading live instead. A list that fails is returned as the same
	// API error, so a misconfigured grant surfaces as that error, never as a
	// hang.
	SyncTimeout time.Duration
	// IdleTTL is how long an informer nobody has read outlives its last use.
	// Channel Secrets come and go with their channels; the janitor keeps the
	// watch count bounded by the Secrets still in use.
	IdleTTL time.Duration
	// ErrorTTL is how long reads of a Secret whose informer has not synced,
	// or whose list was refused as Forbidden, keep that informer's answer
	// before the next read replaces it with a fresh informer: the last failed
	// list, or a live GET when the list was refused but GET is allowed. An
	// informer whose list is refused as Forbidden stops at once instead of
	// retrying on its own, so a Secret the process may no longer read costs
	// nothing until it is read again. 0, the default, retries on every read,
	// which the controller relies on to retry a Forbidden while a new Role
	// reaches the authorizer.
	ErrorTTL time.Duration
	// WatchTimeout, when set, is the timeout of every watch an informer
	// opens. 0 keeps client-go's random 5 to 10 minutes, which bounds how
	// long a watch opened before a grant was removed keeps serving. The
	// benchmark's RBAC check sets it to see a revocation in seconds;
	// production leaves it 0.
	WatchTimeout time.Duration

	// live counts the running informers, for the gauge Collector returns.
	live atomic.Int64

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
	// revoked is set when the informer's list and a GET were both refused as
	// Forbidden and the informer was stopped for it. Reads answer failure
	// until ErrorTTL, then the next read replaces the entry.
	revoked atomic.Bool
	// getOnly is set when the informer's list was refused as Forbidden but a
	// GET was not, and the informer was stopped for it. Reads go live until
	// ErrorTTL, then the next read replaces the entry and lists again.
	getOnly atomic.Bool
	// live is the watcher's running-informer count, which halt decrements.
	live *atomic.Int64
	// stopOnce makes halt idempotent: the watch error handler, entryFor's
	// replacement, the janitor, and shutdown can each stop the same entry.
	stopOnce sync.Once
}

// halt stops the informer; later calls do nothing.
func (e *secretInformer) halt() {
	e.stopOnce.Do(func() {
		close(e.stop)
		e.live.Add(-1)
	})
}

// listFailure is a failed informer list and when it failed. getAllowed
// records, for a list refused as Forbidden, whether a GET of the Secret was
// allowed.
type listFailure struct {
	err        error
	at         time.Time
	getAllowed bool
}

// readsLive reports whether the failure is a refused list with GET still
// allowed, which reads answer with a live GET.
func (f *listFailure) readsLive() bool { return f != nil && f.getAllowed }

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
// first use. A failed informer list is returned as its API error, so the
// caller sees the same error a direct read would; see ErrorTTL for when the
// next read tries again. A read refused as Forbidden is answered from the
// stopped informer until ErrorTTL, never from its cache. A read goes live
// with a GET when the informer's list neither succeeds nor fails within
// SyncTimeout, and when the list was refused but GET is allowed, so a Role
// that does not carry list yet still serves.
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
		stopped := entry.revoked.Load() || entry.getOnly.Load()
		if (!stopped && (entry.informer.HasSynced() || failure == nil)) ||
			(failure != nil && time.Since(failure.at) < w.ErrorTTL) {
			entry.lastUsed = time.Now()
			return entry
		}
		// The informer never synced, or was stopped for a refused list, and
		// its last list failed ErrorTTL ago: replace it, so this read issues
		// one fresh list. Concurrent readers share the replacement.
		entry.halt()
	}
	entry := &secretInformer{
		stop:     make(chan struct{}),
		lastUsed: time.Now(),
		live:     &w.live,
	}
	secrets := w.client.CoreV1().Secrets(namespace)
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			opts.FieldSelector = selector
			list, err := secrets.List(ctx, opts)
			if err == nil {
				// An empty list is a valid state for an absent Secret: the
				// watch delivers it when it appears.
				entry.failure.Store(nil)
				return list, nil
			}
			failure := &listFailure{err: err, at: time.Now()}
			if apierrors.IsForbidden(err) {
				// A Role from before the watcher listed grants get and watch
				// only. One GET tells it apart from a removed grant; any
				// answer but Forbidden means a read may still go live.
				_, probeErr := secrets.Get(ctx, name, metav1.GetOptions{})
				failure.getAllowed = !apierrors.IsForbidden(probeErr)
			}
			entry.failure.Store(failure)
			return nil, err
		},
		// The reflector passes the list's resourceVersion in opts, so the
		// watch resumes exactly where the list left off.
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = selector
			if w.WatchTimeout > 0 {
				secs := int64(w.WatchTimeout / time.Second)
				opts.TimeoutSeconds = &secs
			}
			return secrets.Watch(ctx, opts)
		},
	}
	entry.informer = cache.NewSharedInformer(listThenWatch{lw}, &corev1.Secret{}, 0)
	// A handler set before Run cannot fail.
	_ = entry.informer.SetWatchErrorHandlerWithContext(stopOnForbidden(entry))
	if w.hasSubscribers() {
		// Subscribers live on the watcher, so an informer the janitor
		// stopped and a later read restarted gets the handler again. A
		// handler added before Run cannot fail.
		entry.changes, _ = entry.informer.AddEventHandler(w.changeHandler(key))
	}
	w.live.Add(1)
	go entry.informer.Run(entry.stop)
	w.informers[key] = entry
	return entry
}

// Collector returns a gauge of the Secrets this watcher holds a running
// informer, and so an open watch, for. A stopped informer is not counted:
// one refused or idle is gone from the figure.
func (w *Watcher) Collector() prometheus.Collector {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kaalm_watched_secrets",
		Help: "Secrets this process holds an open informer and watch for through the Secret watcher.",
	}, func() float64 { return float64(w.live.Load()) })
}

// Subscribe registers fn to learn when a watched Secret changes: it is
// created, updated to a new resourceVersion, or deleted after its watch's
// first sync. The informer's initial read is never reported, nor is the
// replay a running informer gives a newly added handler, nor a relist that
// brings back a Secret unchanged since the last list. fn runs on the
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
// default handler does, then stops the informer when its latest list was
// refused as Forbidden. With GET refused too, the process may no longer read
// the Secret (its Role was deleted with its channel or workload, or shrank),
// so retrying until IdleTTL only costs a forbidden list and a log line per
// backoff; reads answer the refusal. With GET allowed, the Role lacks only
// list, and reads go live until ErrorTTL. A refused watch alone is left to
// the reflector's relist, whose list decides. It takes no watcher lock: the
// entry stays in the map for entryFor to replace.
func stopOnForbidden(entry *secretInformer) cache.WatchErrorHandlerWithContext {
	return func(ctx context.Context, r *cache.Reflector, err error) {
		cache.DefaultWatchErrorHandler(ctx, r, err)
		f := entry.failure.Load()
		if f == nil || !apierrors.IsForbidden(f.err) {
			return
		}
		if f.getAllowed {
			entry.getOnly.Store(true)
		} else {
			entry.revoked.Store(true)
		}
		entry.halt()
	}
}

// waitSynced polls HasSynced closely (the standard helper polls at 100 ms,
// which would tax the very first read) until synced, a failed list (returned
// as err), the timeout, or ctx. A refused list is answered before the cache,
// even if the informer had synced, so a stale cache is never served: with
// GET still allowed the read goes live (false, nil), otherwise it returns
// the refusal.
func waitSynced(ctx context.Context, entry *secretInformer, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		failure := entry.failure.Load()
		if entry.getOnly.Load() || failure.readsLive() {
			return false, nil
		}
		if entry.revoked.Load() || (failure != nil && apierrors.IsForbidden(failure.err)) {
			return false, failure.err
		}
		if entry.informer.HasSynced() {
			return true, nil
		}
		if failure != nil {
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

// listThenWatch opts the per-Secret informer out of client-go's streaming
// list (WatchListClient). A streaming list skips ListFunc and opens a watch
// that sends the initial state, so the list that records a read failure, and
// probes GET when it is refused, would never run.
type listThenWatch struct{ *cache.ListWatch }

// IsWatchListSemanticsUnSupported tells the reflector to list with ListFunc.
func (listThenWatch) IsWatchListSemanticsUnSupported() bool { return true }
