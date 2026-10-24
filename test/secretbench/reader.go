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
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/win07xp/kaalm/internal/secretwatch"
)

// The two client limits today's watcher runs at. cmd/gateway/main.go sets
// the gateway's (--client-qps 100, --client-burst 200); cmd/manager/main.go
// sets the controller's (--client-qps 20, --client-burst 30, the
// controller-runtime defaults).
const (
	gatewayQPS      = 100
	gatewayBurst    = 200
	controllerQPS   = 20
	controllerBurst = 30
)

// secretReader is the read shape every variant shares with
// secretwatch.Watcher, so the benchmark drives them all the same way.
type secretReader interface {
	Get(ctx context.Context, namespace, name string) (*corev1.Secret, error)
	// Stop ends every informer the reader started. The benchmark then waits
	// for the goroutines and watches to drain before the next variant.
	Stop()
}

// variant is one reader design at one client limit.
type variant struct {
	Name  string
	QPS   float32
	Burst int
	// Build starts a cold reader on client. rc is the config client was
	// built from, for a reader that needs a second client of its own.
	Build func(ctx context.Context, rc *rest.Config, client kubernetes.Interface) (secretReader, error)
	// ExpectedWatches is how many distinct watches the reader opens once it
	// has served secrets Secrets spread over namespacesUsed namespaces.
	ExpectedWatches func(secrets, namespacesUsed int) int
}

func perSecret(secrets, _ int) int { return secrets }

func perNamespace(_, namespacesUsed int) int { return namespacesUsed }

func buildToday(ctx context.Context, _ *rest.Config, client kubernetes.Interface) (secretReader, error) {
	ctx, cancel := context.WithCancel(ctx)
	return &todayReader{w: secretwatch.New(ctx, client), cancel: cancel}, nil
}

func buildC(ctx context.Context, _ *rest.Config, client kubernetes.Interface) (secretReader, error) {
	return newNameInformerReader(ctx, client, 0), nil
}

func buildLean(ctx context.Context, rc *rest.Config, client kubernetes.Interface) (secretReader, error) {
	return newLeanReader(ctx, rc, client, 0)
}

func buildA(ctx context.Context, _ *rest.Config, client kubernetes.Interface) (secretReader, error) {
	return newLabelInformerReader(ctx, client), nil
}

// variants lists every variant in its default run order. B runs at the
// configured limit; A runs at the controller's limit because its request
// count does not grow with the Secret count, so the limit cannot shape it.
// C runs at both limits today runs at, so C against today isolates the
// design change from the limit. lean runs at the gateway's limit because it
// is compared with C-gw: same requests, without the informer machinery.
func variants(cfg config) []variant {
	return []variant{
		{Name: "today-gw", QPS: gatewayQPS, Burst: gatewayBurst, Build: buildToday, ExpectedWatches: perSecret},
		{Name: "today-ctrl", QPS: controllerQPS, Burst: controllerBurst, Build: buildToday, ExpectedWatches: perSecret},
		{Name: "B", QPS: float32(cfg.BQPS), Burst: cfg.BBurst, Build: buildToday, ExpectedWatches: perSecret},
		{Name: "C-gw", QPS: gatewayQPS, Burst: gatewayBurst, Build: buildC, ExpectedWatches: perSecret},
		{Name: "C-ctrl", QPS: controllerQPS, Burst: controllerBurst, Build: buildC, ExpectedWatches: perSecret},
		{Name: "lean", QPS: gatewayQPS, Burst: gatewayBurst, Build: buildLean, ExpectedWatches: perSecret},
		{Name: "A", QPS: controllerQPS, Burst: controllerBurst, Build: buildA, ExpectedWatches: perNamespace},
	}
}

func variantNamesAll() []string {
	var names []string
	for _, v := range variants(config{BQPS: 1, BBurst: 1}) {
		names = append(names, v.Name)
	}
	return names
}

// selectVariants returns the variants cfg names, in cfg's order.
func selectVariants(cfg config) []variant {
	byName := map[string]variant{}
	for _, v := range variants(cfg) {
		byName[v.Name] = v
	}
	var out []variant
	for _, name := range cfg.Variants {
		out = append(out, byName[name])
	}
	return out
}

// todayReader is the shipped secretwatch.Watcher with its defaults. Stop
// cancels the watcher's context, on which its janitor stops every informer.
type todayReader struct {
	w      *secretwatch.Watcher
	cancel context.CancelFunc
}

func (r *todayReader) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return r.w.Get(ctx, namespace, name)
}

func (r *todayReader) Stop() { r.cancel() }

// informerEntry is one prototype informer and its last list result.
type informerEntry struct {
	informer cache.SharedInformer
	stop     chan struct{}
	stopOnce sync.Once
	listErr  atomic.Pointer[error]
	// revoked is set when the informer's list was refused as Forbidden and
	// the informer was stopped for it, as today's Watcher does. Reads answer
	// the refusal, and the next read replaces the entry.
	revoked atomic.Bool
}

// halt stops the informer once.
func (e *informerEntry) halt() { e.stopOnce.Do(func() { close(e.stop) }) }

// stopOnForbidden mirrors the shipped Watcher's watch error handler: it
// logs as the default handler does, then stops the informer when its latest
// list was refused as Forbidden, so a revoked grant is never served from the
// store. A refused watch alone is left to the reflector's relist, whose LIST
// decides. It adds no goroutine and no buffer, so C's figures stay
// comparable with the sweep's.
func stopOnForbidden(e *informerEntry) cache.WatchErrorHandlerWithContext {
	return func(ctx context.Context, r *cache.Reflector, err error) {
		cache.DefaultWatchErrorHandler(ctx, r, err)
		if apierrors.IsForbidden(e.failed()) {
			e.revoked.Store(true)
			e.halt()
		}
	}
}

// start installs the Forbidden handler and runs the informer. Setting the
// handler fails only once the informer runs, and it does not run yet.
func (e *informerEntry) start() {
	_ = e.informer.SetWatchErrorHandlerWithContext(stopOnForbidden(e))
	go e.informer.Run(e.stop)
}

// recordList keeps the informer's last list error, or clears it on success.
func (e *informerEntry) recordList(err error) {
	if err == nil {
		e.listErr.Store(nil)
		return
	}
	e.listErr.Store(&err)
}

func (e *informerEntry) failed() error {
	if p := e.listErr.Load(); p != nil {
		return *p
	}
	return nil
}

// read follows the Watcher's read contract: wait up to syncTimeout for the
// informer to sync, return its failed list as the error, read live when it
// has not synced in time, and otherwise serve from the store, where an
// absent key is NotFound.
func (e *informerEntry) read(ctx context.Context, client kubernetes.Interface,
	namespace, name string) (*corev1.Secret, error) {
	synced, err := waitSynced(ctx, e, syncTimeout)
	if err != nil {
		return nil, err
	}
	if !synced {
		return client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	obj, exists, err := e.informer.GetStore().GetByKey(namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	sec, ok := obj.(*corev1.Secret)
	if !exists || !ok {
		return nil, apierrors.NewNotFound(corev1.Resource("secrets"), name)
	}
	return sec, nil
}

// waitSynced polls HasSynced every 5 ms, as secretwatch does, until the
// informer syncs (true), its list fails (the error), or the timeout or ctx
// ends (false, nil). A revoked informer answers its refusal even if it had
// synced, so its stale store is never served.
func waitSynced(ctx context.Context, e *informerEntry, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		if e.revoked.Load() {
			return false, e.failed()
		}
		if e.informer.HasSynced() {
			return true, nil
		}
		if err := e.failed(); err != nil {
			return false, err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// listThenWatch opts a prototype informer out of client-go's streaming list
// (the WatchListClient feature gate, on by default in this client-go), the
// same way secretwatch does, so the informer issues a LIST and then a WATCH.
type listThenWatch struct{ *cache.ListWatch }

// IsWatchListSemanticsUnSupported tells the reflector to list with ListFunc.
func (listThenWatch) IsWatchListSemanticsUnSupported() bool { return true }
