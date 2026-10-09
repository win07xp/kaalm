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

package secretwatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func secretObj(ns, name, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{"token": []byte(value)},
	}
}

// waitTimeout bounds every wait in these tests.
const waitTimeout = 3 * time.Second

// eventually polls fn until it returns true or waitTimeout passes.
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcherServesAndFollowsRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset(secretObj("kaalm-system", "openai-key", "v1"))
	w := New(ctx, cs)

	sec, err := w.Get(ctx, "kaalm-system", "openai-key")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := string(sec.Data["token"]); got != "v1" {
		t.Fatalf("token = %q, want v1", got)
	}
	// Rotation: an in-place update reaches the cache through the watch, with
	// no further GET.
	gets := 0
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		return false, nil, nil
	})
	rotated := secretObj("kaalm-system", "openai-key", "v2")
	if _, err := cs.CoreV1().Secrets("kaalm-system").Update(ctx, rotated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		sec, err := w.Get(ctx, "kaalm-system", "openai-key")
		return err == nil && string(sec.Data["token"]) == "v2"
	})
	if gets != 0 {
		t.Fatalf("rotation cost %d live GETs, want 0", gets)
	}
}

func TestWatcherReportsAbsentSecretAndSeesItAppear(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset()
	w := New(ctx, cs)

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsNotFound(err) {
		t.Fatalf("absent Secret: err = %v, want NotFound", err)
	}
	if _, err := cs.CoreV1().Secrets("team-a").Create(ctx, secretObj("team-a", "hook", "s3cr3t"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		sec, err := w.Get(ctx, "team-a", "hook")
		return err == nil && string(sec.Data["token"]) == "s3cr3t"
	})
}

func TestWatcherReturnsTheListErrorWhenSyncFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset()
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("no grant"))
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbidden
	})
	w := New(ctx, cs)
	w.SyncTimeout = 100 * time.Millisecond

	_, err := w.Get(ctx, "team-a", "hook")
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want the informer GET's Forbidden", err)
	}
}

// grantable is a clientset holding team-a/hook whose GETs are refused as
// Forbidden until grant is called, counting every GET.
type grantable struct {
	cs      *fake.Clientset
	granted atomic.Bool
	gets    atomic.Int32
}

func newGrantable() *grantable {
	g := &grantable{cs: fake.NewSimpleClientset(secretObj("team-a", "hook", "s3cr3t"))}
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("no grant"))
	g.cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		g.gets.Add(1)
		if g.granted.Load() {
			return false, nil, nil
		}
		return true, nil, forbidden
	})
	return g
}

func (g *grantable) grant() { g.granted.Store(true) }

// A Secret the watcher may not read costs one fast error per read, not a
// SyncTimeout wait and a live GET per read.
func TestWatcherAnswersAFailedSyncAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newGrantable()
	w := New(ctx, g.cs)
	w.ErrorTTL = time.Hour

	readCtx, readCancel := context.WithTimeout(ctx, time.Second)
	defer readCancel()
	for i := range 20 {
		if _, err := w.Get(readCtx, "team-a", "hook"); !apierrors.IsForbidden(err) {
			t.Fatalf("read %d: err = %v, want Forbidden", i, err)
		}
	}
	// The first list, plus at most one reflector retry.
	if got := g.gets.Load(); got > 2 {
		t.Errorf("20 reads of a forbidden Secret cost %d GETs, want at most 2", got)
	}
}

func TestWatcherAnswersTheCachedErrorUntilErrorTTL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newGrantable()
	w := New(ctx, g.cs)
	w.ErrorTTL = 200 * time.Millisecond

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("first read: err = %v, want Forbidden", err)
	}
	g.grant()
	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("read within ErrorTTL: err = %v, want the cached Forbidden", err)
	}
	time.Sleep(250 * time.Millisecond)
	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil {
		t.Fatalf("read after ErrorTTL: %v", err)
	}
	if got := string(sec.Data["token"]); got != "s3cr3t" {
		t.Fatalf("token = %q, want s3cr3t", got)
	}
	before := g.gets.Load()
	for range 10 {
		if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
			t.Fatalf("cached read: %v", err)
		}
	}
	if got := g.gets.Load() - before; got != 0 {
		t.Errorf("reads after the grant cost %d GETs, want 0 (cache hits)", got)
	}
}

// With ErrorTTL 0 (the default) every read after a failure tries again, so
// a caller retrying a Forbidden while a new grant propagates sees it land.
func TestWatcherZeroErrorTTLRetriesOnTheNextRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newGrantable()
	w := New(ctx, g.cs)

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("first read: err = %v, want Forbidden", err)
	}
	g.grant()
	start := time.Now()
	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("read after the grant: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("read after the grant took %s, want well under the 2s SyncTimeout", took)
	}
}

// A grant that lands is never hidden by the cached error: the informer's
// own list retry picks it up even before ErrorTTL passes.
func TestWatcherRecoversThroughTheInformersOwnRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newGrantable()
	w := New(ctx, g.cs)
	w.ErrorTTL = time.Hour

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("first read: err = %v, want Forbidden", err)
	}
	g.grant()
	eventually(t, func() bool {
		_, err := w.Get(ctx, "team-a", "hook")
		return err == nil
	})
}

// hangingClient blocks the first Secret GET until release is closed, so the
// informer's list neither succeeds nor fails.
type hangingClient struct {
	kubernetes.Interface
	first   *atomic.Bool
	release chan struct{}
}

func (c hangingClient) CoreV1() typedcorev1.CoreV1Interface {
	return hangingCore{CoreV1Interface: c.Interface.CoreV1(), c: c}
}

type hangingCore struct {
	typedcorev1.CoreV1Interface
	c hangingClient
}

func (h hangingCore) Secrets(namespace string) typedcorev1.SecretInterface {
	return hangingSecrets{SecretInterface: h.CoreV1Interface.Secrets(namespace), c: h.c}
}

type hangingSecrets struct {
	typedcorev1.SecretInterface
	c hangingClient
}

func (h hangingSecrets) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error) {
	if h.c.first.CompareAndSwap(false, true) {
		<-h.c.release
	}
	return h.SecretInterface.Get(ctx, name, opts)
}

func TestWatcherFallsBackToLiveReadWhenTheListHangs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cs := hangingClient{
		Interface: fake.NewSimpleClientset(secretObj("team-a", "hook", "s3cr3t")),
		first:     &atomic.Bool{},
		release:   release,
	}
	w := New(ctx, cs)
	w.SyncTimeout = 50 * time.Millisecond

	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(sec.Data["token"]); got != "s3cr3t" {
		t.Fatalf("token = %q, want s3cr3t from the live read", got)
	}
}

// Concurrent readers of a failing Secret share the replacement informer;
// this case exists for -race.
func TestWatcherConcurrentReadsOfAFailingSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newGrantable()
	w := New(ctx, g.cs)
	w.ErrorTTL = 20 * time.Millisecond

	// Each reader polls on its own deadline and reports with t.Error:
	// t.Fatal must run on the test goroutine.
	deadline := time.Now().Add(waitTimeout)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := w.Get(ctx, "team-a", "hook")
				if err == nil {
					return
				}
				if time.Now().After(deadline) {
					t.Errorf("reader %d: no successful read in time, last error: %v", i, err)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	g.grant()
	wg.Wait()
}

func TestWatcherStopsIdleInformers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset(secretObj("team-a", "hook", "x"))
	w := New(ctx, cs)
	w.IdleTTL = 0
	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	entry := w.informers[types.NamespacedName{Namespace: "team-a", Name: "hook"}]
	entry.lastUsed = time.Now().Add(-time.Minute)
	w.mu.Unlock()
	// Drive one janitor pass by hand: the ticker is five minutes.
	w.mu.Lock()
	for key, e := range w.informers {
		if time.Since(e.lastUsed) > w.IdleTTL {
			close(e.stop)
			delete(w.informers, key)
		}
	}
	remaining := len(w.informers)
	w.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d informers remain after idle eviction, want 0", remaining)
	}
	// The next read starts a fresh informer transparently.
	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("read after eviction: %v", err)
	}
}

// recorder collects the keys a Watcher reports to its subscriber.
type recorder struct {
	ch chan types.NamespacedName
}

func newRecorder() *recorder { return &recorder{ch: make(chan types.NamespacedName, 16)} }

func (r *recorder) notify(key types.NamespacedName) { r.ch <- key }

// expect waits for one notification and checks its key.
func (r *recorder) expect(t *testing.T, want types.NamespacedName) {
	t.Helper()
	select {
	case got := <-r.ch:
		if got != want {
			t.Fatalf("notified for %s, want %s", got, want)
		}
	case <-time.After(waitTimeout):
		t.Fatalf("no notification for %s", want)
	}
}

// expectNone checks that no notification arrives for d.
func (r *recorder) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case got := <-r.ch:
		t.Fatalf("unexpected notification for %s", got)
	case <-time.After(d):
	}
}

// watchStarts counts the watches the fake clientset opens, so a test can
// wait until an informer is listening before it changes the Secret: the
// fake drops events made before a watch starts.
func watchStarts(cs *fake.Clientset) func() int {
	var mu sync.Mutex
	n := 0
	cs.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		n++
		mu.Unlock()
		return false, nil, nil
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

func updateLabels(t *testing.T, cs *fake.Clientset, ns, name, rv string) {
	t.Helper()
	ctx := context.Background()
	sec, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sec.Labels = map[string]string{"kaalm.io/channel-credential": "true"}
	sec.ResourceVersion = rv
	if _, err := cs.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherNotifiesSubscribersOfChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sec := secretObj("team-a", "hook", "x")
	sec.ResourceVersion = "1"
	cs := fake.NewSimpleClientset(sec)
	watches := watchStarts(cs)
	w := New(ctx, cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)
	key := types.NamespacedName{Namespace: "team-a", Name: "hook"}

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return watches() == 1 })
	// The informer's initial read is not a change.
	rec.expectNone(t, 200*time.Millisecond)

	updateLabels(t, cs, "team-a", "hook", "2")
	rec.expect(t, key)
	rec.expectNone(t, 100*time.Millisecond)

	if err := cs.CoreV1().Secrets("team-a").Delete(ctx, "hook", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	rec.expect(t, key)
}

func TestWatcherNotifiesWhenAbsentSecretAppears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset()
	watches := watchStarts(cs)
	w := New(ctx, cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsNotFound(err) {
		t.Fatalf("absent Secret: err = %v, want NotFound", err)
	}
	eventually(t, func() bool { return watches() == 1 })
	if _, err := cs.CoreV1().Secrets("team-a").Create(ctx, secretObj("team-a", "hook", "x"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rec.expect(t, types.NamespacedName{Namespace: "team-a", Name: "hook"})
}

func TestWatcherSubscribeAfterReadSkipsReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sec := secretObj("team-a", "hook", "x")
	sec.ResourceVersion = "1"
	cs := fake.NewSimpleClientset(sec)
	watches := watchStarts(cs)
	w := New(ctx, cs)
	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return watches() == 1 })

	rec := newRecorder()
	w.Subscribe(rec.notify)
	// A handler added to a running informer gets the store replayed as
	// initial adds; those are not changes.
	rec.expectNone(t, 200*time.Millisecond)

	updateLabels(t, cs, "team-a", "hook", "2")
	rec.expect(t, types.NamespacedName{Namespace: "team-a", Name: "hook"})
	rec.expectNone(t, 100*time.Millisecond)
}

func TestWatcherNotificationsSurviveIdleEviction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sec := secretObj("team-a", "hook", "x")
	sec.ResourceVersion = "1"
	cs := fake.NewSimpleClientset(sec)
	watches := watchStarts(cs)
	w := New(ctx, cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)
	key := types.NamespacedName{Namespace: "team-a", Name: "hook"}

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return watches() == 1 })
	// Evict by hand, as the janitor does.
	w.mu.Lock()
	close(w.informers[key].stop)
	delete(w.informers, key)
	w.mu.Unlock()

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return watches() == 2 })
	rec.expectNone(t, 200*time.Millisecond)

	updateLabels(t, cs, "team-a", "hook", "2")
	rec.expect(t, key)
	// The stopped informer reports nothing, so the change arrives once.
	rec.expectNone(t, 200*time.Millisecond)
}

func TestWatcherWithoutSubscribersAttachesNoHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset(secretObj("team-a", "hook", "x"))
	w := New(ctx, cs)
	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "team-a", Name: "hook"}
	w.mu.Lock()
	attached := w.informers[key].changes != nil
	w.mu.Unlock()
	if attached {
		t.Fatal("a watcher with no subscriber attached a change handler")
	}

	w.Subscribe(func(types.NamespacedName) {})
	w.mu.Lock()
	attached = w.informers[key].changes != nil
	w.mu.Unlock()
	if !attached {
		t.Fatal("Subscribe left a running informer without a change handler")
	}
}

func TestWatcherChangeHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(ctx, fake.NewSimpleClientset())
	rec := newRecorder()
	w.Subscribe(rec.notify)
	key := types.NamespacedName{Namespace: "team-a", Name: "hook"}
	h := w.changeHandler(key)

	at := func(name, rv string) *corev1.Secret {
		s := secretObj("team-a", name, "x")
		s.ResourceVersion = rv
		return s
	}
	h.OnAdd(at("hook", "1"), true)
	rec.expectNone(t, 50*time.Millisecond)
	h.OnAdd(at("hook", "1"), false)
	rec.expect(t, key)

	h.OnUpdate(at("hook", "1"), at("hook", "1"))
	rec.expectNone(t, 50*time.Millisecond)
	h.OnUpdate(at("hook", "1"), at("hook", "2"))
	rec.expect(t, key)

	h.OnAdd(at("other", "3"), false)
	h.OnUpdate(at("other", "3"), at("other", "4"))
	h.OnDelete(at("other", "4"))
	other := secretObj("team-b", "hook", "x")
	other.ResourceVersion = "5"
	h.OnAdd(other, false)
	rec.expectNone(t, 50*time.Millisecond)

	h.OnDelete(cache.DeletedFinalStateUnknown{Key: "team-a/hook", Obj: at("hook", "2")})
	rec.expect(t, key)
	h.OnDelete(at("hook", "2"))
	rec.expect(t, key)
}
