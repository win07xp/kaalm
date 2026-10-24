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
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	restclientwatch "k8s.io/client-go/rest/watch"
)

const (
	// The reflector's watch timeout is random in [5, 10) minutes, so lean
	// re-opens its watches as often as C does.
	leanMinWatchTimeout = 5 * time.Minute
	// A stream that ends this soon with no event counts as a failure and
	// backs off, as the reflector's "very short watch" does.
	leanShortWatch = time.Second
	// The reflector's backoff: 800 ms doubling to 30 s.
	leanBackoffStart = 800 * time.Millisecond
	leanBackoffMax   = 30 * time.Second

	secretsResource = "secrets"
)

// leanReader is the lean prototype: C's requests without client-go's
// informer machinery. Per Secret it issues one LIST with an exact
// metadata.name field selector, then one WATCH from that LIST's
// resourceVersion, as C does, but there is no informer, reflector,
// DeltaFIFO, or processor. Every Secret lives in one shared map under one
// lock, and each name spends one goroutine of its own, the loop that lists
// and reads its watch stream.
//
// net/http adds one goroutine per open HTTP/2 stream, which no Go client
// avoids, so lean costs 2 goroutines per Secret. client-go's typed Watch
// would add a third: its StreamWatcher decodes in a goroutine of its own and
// hands events over a channel. Lean instead opens the stream with
// rest.Request.Stream and decodes it in its loop with the same decoders
// rest.Request.Watch uses. Stream waits on the client's rate limiter, which
// Watch skips, so the WATCH streams go through a second client with no
// limit; LIST and the live GET fallback use the variant's limited client.
//
// The read contract is the Watcher's: a Get waits up to syncTimeout for its
// name's first LIST, then reads live. A clean stream end re-opens the WATCH
// from the last version seen, 410 Gone relists, and other failures back off.
// A Forbidden LIST or WATCH stops the name and drops it from the map, so a
// revoked grant is never served from the cache; the next Get starts afresh.
type leanReader struct {
	client  kubernetes.Interface
	streams rest.Interface
	// The watch stream's decoders, the ones rest.Request.Watch builds.
	objects    runtime.Decoder
	serializer runtime.Serializer
	framer     runtime.Framer
	// watchTimeout, when set, replaces the reflector-like random timeout.
	watchTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	entries map[types.NamespacedName]*leanEntry
}

// leanEntry is one name's cache line; every field is guarded by the
// reader's mu.
type leanEntry struct {
	secret *corev1.Secret // nil while the Secret is absent
	err    error          // the error a read answers, if any
	listed chan struct{}  // closed once the first LIST finished or failed
	woken  bool
}

// newLeanReader starts lean on client; rc is the config client was built
// from. watchTimeout 0 keeps the reflector-like random timeout.
func newLeanReader(ctx context.Context, rc *rest.Config, client kubernetes.Interface,
	watchTimeout time.Duration) (*leanReader, error) {
	cfg := rest.CopyConfig(rc)
	cfg.QPS, cfg.Burst = -1, 0
	cfg.ContentType, cfg.AcceptContentTypes = runtime.ContentTypeJSON, runtime.ContentTypeJSON
	core, err := corev1client.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("watch client: %w", err)
	}
	codecs := rest.CodecFactoryForGeneratedClient(scheme.Scheme, scheme.Codecs).WithoutConversion()
	objects, serializer, framer, err := runtime.NewClientNegotiator(codecs, corev1.SchemeGroupVersion).
		StreamDecoder(runtime.ContentTypeJSON, nil)
	if err != nil {
		return nil, fmt.Errorf("watch decoder: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	return &leanReader{
		client: client, streams: core.RESTClient(),
		objects: objects, serializer: serializer, framer: framer,
		watchTimeout: watchTimeout, ctx: ctx, cancel: cancel,
		entries: map[types.NamespacedName]*leanEntry{},
	}, nil
}

func (r *leanReader) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	e := r.entryFor(types.NamespacedName{Namespace: namespace, Name: name})
	select {
	case <-e.listed:
	default:
		timer := time.NewTimer(syncTimeout)
		defer timer.Stop()
		select {
		case <-e.listed:
		case <-timer.C:
			return r.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case e.err != nil:
		return nil, e.err
	case e.secret == nil:
		return nil, apierrors.NewNotFound(corev1.Resource(secretsResource), name)
	}
	return e.secret, nil
}

// entryFor returns the name's entry, starting its loop on first use. The
// check of the reader's context and the WaitGroup's Add share mu with
// Stop's cancel, so no loop starts once Stop has begun.
func (r *leanReader) entryFor(key types.NamespacedName) *leanEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[key]; ok {
		return e
	}
	e := &leanEntry{listed: make(chan struct{})}
	if err := r.ctx.Err(); err != nil {
		e.err = err
		e.wake()
		return e
	}
	r.entries[key] = e
	r.wg.Add(1)
	go r.run(key, e)
	return e
}

// wake releases the reads waiting for the first LIST; callers hold mu.
func (e *leanEntry) wake() {
	if !e.woken {
		e.woken = true
		close(e.listed)
	}
}

// run is the name's one goroutine: LIST, then WATCH until the stream ends,
// and again, until the reader stops or the name is refused.
func (r *leanReader) run(key types.NamespacedName, e *leanEntry) {
	defer r.wg.Done()
	defer func() {
		r.mu.Lock()
		if !e.woken {
			e.err = r.ctx.Err()
			e.wake()
		}
		r.mu.Unlock()
	}()
	selector := fields.OneTermEqualSelector("metadata.name", key.Name).String()
	backoff := leanBackoffStart
	// The first LIST reads the watch cache ("0"), as the reflector's does;
	// a LIST after 410 Gone is a quorum read ("").
	listVersion := "0"
	version := ""
	listedOnce := false
	for r.ctx.Err() == nil {
		if version == "" {
			got, err := r.list(key, e, selector, listVersion, listedOnce)
			switch {
			case apierrors.IsForbidden(err):
				r.refuse(key, e, err)
				return
			case err != nil:
				if !r.sleep(&backoff) {
					return
				}
				continue
			}
			version, listedOnce = got, true
		}
		started := time.Now()
		next, events, err := r.watchOnce(key, e, selector, version)
		switch {
		case r.ctx.Err() != nil:
			return
		case apierrors.IsForbidden(err):
			r.refuse(key, e, err)
			return
		case apierrors.IsGone(err) || apierrors.IsResourceExpired(err):
			version, listVersion = "", ""
			continue
		case err != nil || (events == 0 && time.Since(started) < leanShortWatch):
			if !r.sleep(&backoff) {
				return
			}
		default:
			backoff = leanBackoffStart
		}
		version = next
	}
}

// list LISTs the one name and stores what it finds. A failed first LIST is
// what reads answer until a later one succeeds; a failed relist leaves the
// cache serving, as an informer's store does.
func (r *leanReader) list(key types.NamespacedName, e *leanEntry, selector, version string,
	listedBefore bool) (string, error) {
	list, err := r.client.CoreV1().Secrets(key.Namespace).List(r.ctx,
		metav1.ListOptions{FieldSelector: selector, ResourceVersion: version})
	if err != nil {
		if !listedBefore && !apierrors.IsForbidden(err) {
			r.mu.Lock()
			e.err = err
			e.wake()
			r.mu.Unlock()
		}
		return "", err
	}
	var sec *corev1.Secret
	for i := range list.Items {
		if list.Items[i].Name == key.Name {
			sec = &list.Items[i]
		}
	}
	r.mu.Lock()
	e.secret, e.err = sec, nil
	e.wake()
	r.mu.Unlock()
	return list.ResourceVersion, nil
}

// watchOnce opens one WATCH from version and applies its events until the
// stream ends. It returns the last version seen, how many events arrived,
// and the error that ended the stream; nil means the server closed it.
func (r *leanReader) watchOnce(key types.NamespacedName, e *leanEntry, selector, version string) (string, int, error) {
	timeout := int64(r.nextWatchTimeout() / time.Second)
	body, err := r.streams.Get().Namespace(key.Namespace).Resource(secretsResource).
		VersionedParams(&metav1.ListOptions{
			Watch: true, FieldSelector: selector, ResourceVersion: version,
			TimeoutSeconds: &timeout, AllowWatchBookmarks: true,
		}, scheme.ParameterCodec).
		Stream(r.ctx)
	if err != nil {
		return version, 0, err
	}
	decoder := restclientwatch.NewDecoder(streaming.NewDecoder(r.framer.NewFrameReader(body), r.serializer), r.objects)
	defer decoder.Close()
	events := 0
	for {
		typ, obj, err := decoder.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return version, events, nil
			}
			return version, events, err
		}
		events++
		switch typ {
		case watch.Added, watch.Modified:
			if sec, ok := obj.(*corev1.Secret); ok {
				r.store(e, sec)
				version = sec.ResourceVersion
			}
		case watch.Deleted:
			r.store(e, nil)
			version = resourceVersion(obj, version)
		case watch.Bookmark:
			version = resourceVersion(obj, version)
		case watch.Error:
			return version, events, apierrors.FromObject(obj)
		}
	}
}

func (r *leanReader) store(e *leanEntry, sec *corev1.Secret) {
	r.mu.Lock()
	e.secret = sec
	r.mu.Unlock()
}

// refuse records a Forbidden answer for the reads waiting on e and drops the
// name, so the next Get starts a fresh entry and one fresh LIST.
func (r *leanReader) refuse(key types.NamespacedName, e *leanEntry, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.secret, e.err = nil, err
	e.wake()
	if r.entries[key] == e {
		delete(r.entries, key)
	}
}

// nextWatchTimeout is the set watchTimeout or a random one in [5, 10)
// minutes.
func (r *leanReader) nextWatchTimeout() time.Duration {
	if r.watchTimeout > 0 {
		return r.watchTimeout
	}
	return leanMinWatchTimeout + rand.N(leanMinWatchTimeout)
}

// sleep waits out the backoff and doubles it, up to its cap; false when the
// reader stopped first.
func (r *leanReader) sleep(backoff *time.Duration) bool {
	timer := time.NewTimer(*backoff)
	defer timer.Stop()
	*backoff = min(*backoff*2, leanBackoffMax)
	select {
	case <-timer.C:
		return true
	case <-r.ctx.Done():
		return false
	}
}

// Stop ends every name's loop, waits for them, and clears the cache.
func (r *leanReader) Stop() {
	r.mu.Lock()
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
	r.mu.Lock()
	clear(r.entries)
	r.mu.Unlock()
}

func resourceVersion(obj runtime.Object, fallback string) string {
	if m, err := meta.Accessor(obj); err == nil && m.GetResourceVersion() != "" {
		return m.GetResourceVersion()
	}
	return fallback
}
