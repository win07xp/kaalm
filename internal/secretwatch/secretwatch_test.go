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
	"fmt"
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
	waitUntil(t, waitTimeout, fn)
}

// waitUntil polls fn until it returns true or d passes.
func waitUntil(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
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
	var gets atomic.Int32
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		return false, nil, nil
	})
	watches := watchStarts(cs)
	w := New(ctx, cs)

	sec, err := w.Get(ctx, "kaalm-system", "openai-key")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := string(sec.Data["token"]); got != "v1" {
		t.Fatalf("token = %q, want v1", got)
	}
	// The read and the confirmation once the watch opens.
	eventually(t, func() bool { return watches() == 1 && gets.Load() == 2 })
	// Rotation: an in-place update reaches the cache through the watch, with
	// no further GET.
	rotated := secretObj("kaalm-system", "openai-key", "v2")
	if _, err := cs.CoreV1().Secrets("kaalm-system").Update(ctx, rotated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		sec, err := w.Get(ctx, "kaalm-system", "openai-key")
		return err == nil && string(sec.Data["token"]) == "v2"
	})
	if got := gets.Load() - 2; got != 0 {
		t.Fatalf("rotation cost %d live GETs, want 0", got)
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
	// The first list only: the refused informer stops instead of retrying.
	if got := g.gets.Load(); got != 1 {
		t.Errorf("20 reads of a forbidden Secret cost %d GETs, want 1", got)
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

// An informer whose first GET is refused stops instead of retrying on its
// own: until ErrorTTL passes, reads answer the refusal and cost no GET, even
// once a grant lands.
func TestWatcherStopsAnInformerWhoseFirstGetIsForbidden(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newGrantable()
	w := New(ctx, g.cs)
	w.ErrorTTL = time.Hour

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("first read: err = %v, want Forbidden", err)
	}
	entry := currentEntry(w, hookKey)
	eventuallyAcrossBackoff(t, entry.informer.IsStopped)
	g.grant()
	for i := range 10 {
		if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
			t.Fatalf("read %d within ErrorTTL: err = %v, want the cached Forbidden", i, err)
		}
	}
	if got := g.gets.Load(); got != 1 {
		t.Errorf("a stopped informer and 11 reads cost %d GETs, want 1", got)
	}
}

// hookKey is the Secret the revocable and grantable clientsets hold.
var hookKey = types.NamespacedName{Namespace: "team-a", Name: "hook"}

// reflectorWait bounds a wait that spans a reflector backoff: its first
// retry comes 0.8 to 1.6s after a failure, the next 1.6 to 3.2s later.
const reflectorWait = 10 * time.Second

// eventuallyAcrossBackoff polls fn until it returns true or reflectorWait
// passes.
func eventuallyAcrossBackoff(t *testing.T, fn func() bool) {
	t.Helper()
	waitUntil(t, reflectorWait, fn)
}

// currentEntry returns the watcher's entry for key, or nil.
func currentEntry(w *Watcher, key types.NamespacedName) *secretInformer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.informers[key]
}

// revocable is a clientset holding team-a/hook (token "v", resourceVersion
// 1, UID hook-1) whose GETs and watches can be made to fail. Every watch it
// opens is a fake the test drives: endWatch ends the newest one, as an API
// server does when a watch times out, and latestWatch delivers events
// through it. After expireOldVersions, a watch from a given resourceVersion
// ends at once with 410 Expired and is not counted as open.
type revocable struct {
	cs      *fake.Clientset
	gets    atomic.Int32 // every GET
	refused atomic.Int32 // GETs answered with getErr
	expired atomic.Int32 // watches answered with 410 Expired

	mu           sync.Mutex
	getErr       error
	watchErr     error
	expire       bool
	deleteOnOpen bool
	onOpen       func()
	watches      []*watch.RaceFreeFakeWatcher
}

// hookUID is the UID of the Secret the revocable clientset holds.
const hookUID = "hook-1"

// hookAt returns team-a/hook with token v, the given UID, and the given
// resourceVersion.
func hookAt(uid, rv string) *corev1.Secret {
	sec := secretObj("team-a", "hook", "v")
	sec.UID = types.UID(uid)
	sec.ResourceVersion = rv
	return sec
}

func newRevocable() *revocable {
	r := &revocable{cs: fake.NewSimpleClientset(hookAt(hookUID, "1"))}
	r.cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		r.gets.Add(1)
		r.mu.Lock()
		err := r.getErr
		r.mu.Unlock()
		if err != nil {
			r.refused.Add(1)
			return true, nil, err
		}
		return false, nil, nil
	})
	r.cs.PrependWatchReactor("secrets", func(action k8stesting.Action) (bool, watch.Interface, error) {
		r.mu.Lock()
		if r.watchErr != nil {
			defer r.mu.Unlock()
			return true, nil, r.watchErr
		}
		if rv := action.(k8stesting.WatchAction).GetWatchRestrictions().ResourceVersion; r.expire && rv != "" {
			defer r.mu.Unlock()
			r.expired.Add(1)
			fw := watch.NewRaceFreeFake()
			st := apierrors.NewResourceExpired(fmt.Sprintf("too old resource version: %s (100)", rv)).ErrStatus
			fw.Error(&st)
			return true, fw, nil
		}
		fw := watch.NewRaceFreeFake()
		r.watches = append(r.watches, fw)
		if r.deleteOnOpen {
			r.deleteOnOpen = false
			r.removeHook()
		}
		onOpen := r.onOpen
		r.onOpen = nil
		r.mu.Unlock()
		if onOpen != nil {
			onOpen()
		}
		return true, fw, nil
	})
	return r
}

// whenOpened runs fn once, outside r.mu, as the next watch opens.
func (r *revocable) whenOpened(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onOpen = fn
}

// settled waits across a reflector backoff until the clientset has opened
// watches watches and answered gets GETs.
func (r *revocable) settled(t *testing.T, watches int, gets int32) {
	t.Helper()
	eventuallyAcrossBackoff(t, func() bool { return r.watchCount() == watches && r.gets.Load() == gets })
}

// removeHook deletes team-a/hook from the clientset's store without an
// event, as the API server holds it once a delete has landed before a
// watch's start version.
func (r *revocable) removeHook() {
	if err := r.cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), "team-a", "hook"); err != nil {
		panic(err)
	}
}

// deleteBeforeOpen deletes team-a/hook as the next watch opens, so the
// informer's GET saw the Secret and the watch's opening replay does not
// carry it: the delete landed between the two.
func (r *revocable) deleteBeforeOpen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteOnOpen = true
}

// fail makes later GETs and watches answer getErr and watchErr; nil lets
// them through.
func (r *revocable) fail(getErr, watchErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getErr, r.watchErr = getErr, watchErr
}

// revoke refuses later GETs and watches as Forbidden, as the API server does
// once the Role granting them is gone.
func (r *revocable) revoke() {
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("role deleted"))
	r.fail(forbidden, forbidden)
}

// expireOldVersions makes later watches from a given resourceVersion end at
// once with 410 Expired, as the API server answers once its watch cache has
// dropped that version. A watch from "" (the most recent version) still
// opens.
func (r *revocable) expireOldVersions() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire = true
}

func (r *revocable) watchCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.watches)
}

func (r *revocable) latestWatch() *watch.RaceFreeFakeWatcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.watches[len(r.watches)-1]
}

// endWatch ends the newest watch, so the reflector watches or lists again.
func (r *revocable) endWatch() { r.latestWatch().Stop() }

// A synced informer whose GET is refused (its Role was deleted) stops, and
// the next read answers Forbidden instead of the cached Secret.
func TestWatcherStopsAnInformerWhoseGetIsForbidden(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	// The refusal lands on the relist's GET, not the confirmation.
	r.settled(t, 1, 2)
	entry := currentEntry(w, hookKey)
	r.revoke()
	r.endWatch()
	eventuallyAcrossBackoff(t, func() bool { return r.refused.Load() >= 1 })
	eventuallyAcrossBackoff(t, entry.informer.IsStopped)

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("read after the refusal: err = %v, want Forbidden, not the cached Secret", err)
	}
}

// A later read starts a fresh informer once ErrorTTL has passed, and
// subscribers hear about changes through it.
func TestWatcherRestartsAForbiddenInformerOnALaterRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)
	w.ErrorTTL = 200 * time.Millisecond
	rec := newRecorder()
	w.Subscribe(rec.notify)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	eventually(t, func() bool { return r.watchCount() == 1 })
	old := currentEntry(w, hookKey)
	r.revoke()
	r.endWatch()
	eventuallyAcrossBackoff(t, old.informer.IsStopped)

	before := r.gets.Load()
	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("read within ErrorTTL: err = %v, want the cached Forbidden", err)
	}
	if got := r.gets.Load() - before; got != 0 {
		t.Errorf("read within ErrorTTL cost %d GETs, want 0", got)
	}

	r.fail(nil, nil)
	time.Sleep(250 * time.Millisecond)
	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil {
		t.Fatalf("read after ErrorTTL: %v", err)
	}
	if got := string(sec.Data["token"]); got != "v" {
		t.Fatalf("token = %q, want v", got)
	}
	fresh := currentEntry(w, hookKey)
	if fresh == old {
		t.Fatal("the read after ErrorTTL reused the stopped informer")
	}
	if fresh.informer.IsStopped() {
		t.Fatal("the fresh informer is stopped")
	}

	eventually(t, func() bool { return r.watchCount() == 2 })
	changed := secretObj("team-a", "hook", "v2")
	changed.ResourceVersion = "2"
	r.latestWatch().Modify(changed)
	rec.expect(t, hookKey)
	rec.expectNone(t, 200*time.Millisecond)
}

// Errors other than a refused GET leave the informer running: a synced one
// keeps serving its cache and resumes once the error clears.
func TestWatcherKeepsServingThroughTransientErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"internal error", apierrors.NewInternalError(errors.New("etcd unavailable"))},
		{"server timeout", apierrors.NewServerTimeout(corev1.Resource("secrets"), "get", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newRevocable()
			w := New(ctx, r.cs)

			if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
				t.Fatalf("first read: %v", err)
			}
			// The errors land on relists, not the confirmation.
			r.settled(t, 1, 2)
			entry := currentEntry(w, hookKey)
			r.fail(tc.err, tc.err)
			r.endWatch()
			// A second failed GET means the informer retried after the first.
			eventuallyAcrossBackoff(t, func() bool { return r.refused.Load() >= 2 })
			if entry.informer.IsStopped() {
				t.Fatal("a transient error stopped the informer")
			}
			sec, err := w.Get(ctx, "team-a", "hook")
			if err != nil || string(sec.Data["token"]) != "v" {
				t.Fatalf("read during the error: sec = %v, err = %v, want the cached Secret", sec, err)
			}

			r.fail(nil, nil)
			eventuallyAcrossBackoff(t, func() bool { return r.watchCount() == 2 })
			if entry.informer.IsStopped() {
				t.Fatal("the informer stopped after the error cleared")
			}
			if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
				t.Fatalf("read after the error cleared: %v", err)
			}
		})
	}
}

// A Secret deleted while its informer runs is a valid empty state, not a
// reason to stop.
func TestWatcherKeepsRunningOnANotFoundSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	eventually(t, func() bool { return r.watchCount() == 1 })
	entry := currentEntry(w, hookKey)
	r.fail(apierrors.NewNotFound(corev1.Resource("secrets"), "hook"), nil)
	r.endWatch()
	// The relist's GET answers NotFound, then the informer watches again.
	eventuallyAcrossBackoff(t, func() bool { return r.watchCount() == 2 })
	if entry.informer.IsStopped() {
		t.Fatal("a NotFound GET stopped the informer")
	}
	// The relist queues the deletion and opens the next watch at once; the
	// informer drops the Secret from its store a moment later, on its own
	// goroutine.
	eventually(t, func() bool {
		_, err := w.Get(ctx, "team-a", "hook")
		return apierrors.IsNotFound(err)
	})
}

// Only a refused GET stops an informer. A refused watch alone (a get-only
// grant) leaves the informer relisting and serving its cache.
func TestWatcherKeepsRunningWhenOnlyTheWatchIsForbidden(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	r.settled(t, 1, 2)
	entry := currentEntry(w, hookKey)
	r.fail(nil, apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("get only")))
	r.endWatch()
	// The initial GET and its confirmation, a relist, and a relist after a
	// refused watch.
	eventuallyAcrossBackoff(t, func() bool { return r.gets.Load() >= 4 })
	if entry.informer.IsStopped() {
		t.Fatal("a refused watch stopped the informer")
	}
	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil || string(sec.Data["token"]) != "v" {
		t.Fatalf("read: sec = %v, err = %v, want the Secret", sec, err)
	}
}

// A Secret whose own resourceVersion the API server no longer holds still
// gets an open watch that delivers later changes, at the cost of one watch
// and two GETs: the read and its confirmation once the watch opens.
func TestWatcherOpensAWatchWhenTheSecretVersionHasExpired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	r.expireOldVersions()
	w := New(ctx, r.cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil || string(sec.Data["token"]) != "v" {
		t.Fatalf("first read: sec = %v, err = %v, want token v", sec, err)
	}
	r.settled(t, 1, 2)

	// The API server opens a watch from the most recent version by
	// replaying the Secret's current state.
	replay := secretObj("team-a", "hook", "v")
	replay.ResourceVersion = "1"
	r.latestWatch().Add(replay)
	// Longer than the reflector's first retry, so a relist loop would show.
	rec.expectNone(t, 2*time.Second)
	if got := r.gets.Load(); got != 2 {
		t.Errorf("GETs = %d, want 2", got)
	}
	if got := r.watchCount(); got != 1 {
		t.Errorf("open watches = %d, want 1", got)
	}
	if r.latestWatch().IsStopped() {
		t.Fatal("the watch closed")
	}

	changed := secretObj("team-a", "hook", "v2")
	changed.ResourceVersion = "2"
	r.latestWatch().Modify(changed)
	rec.expect(t, hookKey)
	rec.expectNone(t, 200*time.Millisecond)
	eventually(t, func() bool {
		sec, err := w.Get(ctx, "team-a", "hook")
		return err == nil && string(sec.Data["token"]) == "v2"
	})
	if got := r.gets.Load(); got != 2 {
		t.Errorf("GETs after the change = %d, want 2", got)
	}
}

// A running informer whose resumed watch expires lists again once and goes
// back to an open watch, instead of relisting the same expired version.
func TestWatcherRecoversWhenItsWatchExpires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	eventually(t, func() bool { return r.watchCount() == 1 })
	// A bookmark moves the reflector to version 50; the next watch resumes
	// from there, and the API server has dropped it by then.
	r.latestWatch().Action(watch.Bookmark, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "50"}})
	r.expireOldVersions()
	r.endWatch()
	// Each watch from the most recent version follows a GET and is
	// confirmed by another.
	r.settled(t, 2, 4)
	if r.expired.Load() < 1 {
		t.Fatal("no watch expired, so the test did not exercise the relist")
	}

	gets := r.gets.Load()
	replay := secretObj("team-a", "hook", "v")
	replay.ResourceVersion = "1"
	r.latestWatch().Add(replay)
	rec.expectNone(t, 2*time.Second)
	if got := r.gets.Load(); got != gets {
		t.Errorf("GETs on the open watch = %d, want %d", got, gets)
	}
	if got := r.watchCount(); got != 2 {
		t.Errorf("open watches = %d, want 2", got)
	}

	changed := secretObj("team-a", "hook", "v2")
	changed.ResourceVersion = "2"
	r.latestWatch().Modify(changed)
	rec.expect(t, hookKey)
	rec.expectNone(t, 200*time.Millisecond)
}

// expectGone waits until reads of team-a/hook answer NotFound.
func expectGone(t *testing.T, w *Watcher) {
	t.Helper()
	eventually(t, func() bool {
		_, err := w.Get(context.Background(), "team-a", "hook")
		return apierrors.IsNotFound(err)
	})
}

// A Secret deleted after the informer's GET but before its watch's start
// version is in neither the GET nor the watch's opening replay. The informer
// still drops it and reports the delete once.
func TestWatcherDropsASecretDeletedBeforeItsWatchOpens(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	r.deleteBeforeOpen()
	w := New(ctx, r.cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	// The informer's GET saw the Secret; the delete may already have reached
	// the store by the time the read looks.
	if _, err := w.Get(ctx, "team-a", "hook"); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("first read: %v", err)
	}
	expectGone(t, w)
	rec.expect(t, hookKey)
	rec.expectNone(t, 500*time.Millisecond)
	if got := r.watchCount(); got != 1 {
		t.Errorf("open watches = %d, want 1", got)
	}
	if r.latestWatch().IsStopped() {
		t.Error("the watch closed")
	}
	if got := r.gets.Load(); got != 2 {
		t.Errorf("GETs = %d, want 2 (the read and the confirmation)", got)
	}
}

// A watch that closes before anything but an add has moved its version opens
// again at the most recent version, which is the same gap.
func TestWatcherDropsASecretDeletedBeforeItsReWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	r.removeHook()
	w := New(ctx, r.cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsNotFound(err) {
		t.Fatalf("first read: err = %v, want NotFound", err)
	}
	eventually(t, func() bool { return r.watchCount() == 1 })
	opened := time.Now()
	created := hookAt(hookUID, "2")
	if err := r.cs.Tracker().Add(created); err != nil {
		t.Fatal(err)
	}
	r.latestWatch().Add(created)
	rec.expect(t, hookKey)
	eventually(t, func() bool {
		_, err := w.Get(ctx, "team-a", "hook")
		return err == nil
	})
	if got := r.gets.Load(); got != 1 {
		t.Errorf("GETs before the re-watch = %d, want 1 (an absent Secret needs no confirmation)", got)
	}

	r.deleteBeforeOpen()
	time.Sleep(time.Until(opened.Add(time.Second)))
	r.endWatch()
	expectGone(t, w)
	rec.expect(t, hookKey)
	rec.expectNone(t, 500*time.Millisecond)
	if got := r.watchCount(); got != 2 {
		t.Errorf("open watches = %d, want 2", got)
	}
	if got := r.gets.Load(); got != 2 {
		t.Errorf("GETs = %d, want 2 (the read and the re-watch's confirmation)", got)
	}
}

// The watch a relist opens has the same gap after the relist's GET.
func TestWatcherDropsASecretDeletedBeforeItsRelistWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	eventually(t, func() bool { return r.watchCount() == 1 })
	r.latestWatch().Action(watch.Bookmark, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "50"}})
	r.expireOldVersions()
	r.deleteBeforeOpen()
	r.endWatch()
	eventuallyAcrossBackoff(t, func() bool { return r.watchCount() == 2 })
	if r.expired.Load() < 1 {
		t.Fatal("no watch expired, so the test did not exercise the relist")
	}
	expectGone(t, w)
	rec.expect(t, hookKey)
	rec.expectNone(t, 500*time.Millisecond)

	// A Secret created again under the same name is a new object.
	recreated := hookAt("hook-2", "3")
	r.latestWatch().Add(recreated)
	rec.expect(t, hookKey)
	rec.expectNone(t, 200*time.Millisecond)
	eventually(t, func() bool {
		sec, err := w.Get(ctx, "team-a", "hook")
		return err == nil && sec.UID == "hook-2"
	})
}

// When the delete lands after the watch's start version, the watch also
// carries the replay and the real delete. Subscribers still hear of it once.
func TestWatcherReportsOneDeleteWhenTheWatchAlsoCarriesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	r.deleteBeforeOpen()
	w := New(ctx, r.cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("first read: %v", err)
	}
	r.settled(t, 1, 2)
	r.latestWatch().Add(hookAt(hookUID, "1"))
	r.latestWatch().Delete(hookAt(hookUID, "2"))
	expectGone(t, w)
	rec.expect(t, hookKey)
	rec.expectNone(t, 500*time.Millisecond)
	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsNotFound(err) {
		t.Fatalf("read after the delete: err = %v, want NotFound", err)
	}

	r.latestWatch().Add(hookAt("hook-2", "3"))
	rec.expect(t, hookKey)
	rec.expectNone(t, 200*time.Millisecond)
	eventually(t, func() bool {
		sec, err := w.Get(ctx, "team-a", "hook")
		return err == nil && sec.UID == "hook-2"
	})
}

// A confirmation that fails for any reason but NotFound changes nothing: the
// informer keeps its Secret and its watch, and never records the failure.
func TestWatcherIgnoresAConfirmingGetThatFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	r.whenOpened(func() { r.fail(apierrors.NewInternalError(errors.New("etcd unavailable")), nil) })
	w := New(ctx, r.cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	r.settled(t, 1, 2)
	if got := r.refused.Load(); got != 1 {
		t.Fatalf("refused GETs = %d, want 1 (the confirmation)", got)
	}
	r.fail(nil, nil)
	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil || string(sec.Data["token"]) != "v" {
		t.Fatalf("read: sec = %v, err = %v, want token v", sec, err)
	}
	rec.expectNone(t, 500*time.Millisecond)
	entry := currentEntry(w, hookKey)
	if entry.failure.Load() != nil {
		t.Error("the confirmation recorded a failure")
	}
	if entry.informer.IsStopped() {
		t.Error("the informer stopped")
	}
	if got := r.watchCount(); got != 1 || r.latestWatch().IsStopped() {
		t.Errorf("open watches = %d, want the first still open", got)
	}
}

// A watch resumed from a known version delivers every later event, so it
// needs no confirmation.
func TestWatcherResumesFromAKnownVersionWithoutAGet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	r.settled(t, 1, 2)
	r.latestWatch().Action(watch.Bookmark, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "50"}})
	r.endWatch()
	r.settled(t, 2, 2)
	// Longer than the reflector's first retry, so a late GET would show.
	time.Sleep(time.Second)
	if got := r.gets.Load(); got != 2 {
		t.Errorf("GETs = %d, want 2", got)
	}
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
			e.halt()
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
	w.informers[key].halt()
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
