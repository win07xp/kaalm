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

	"github.com/prometheus/client_golang/prometheus/testutil"
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

// countVerb counts the fake clientset's Secret requests of one verb.
func countVerb(cs *fake.Clientset, verb string) *atomic.Int32 {
	var n atomic.Int32
	cs.PrependReactor(verb, "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		n.Add(1)
		return false, nil, nil
	})
	return &n
}

// refuse answers every Secret request of the given verbs as Forbidden.
func refuse(cs *fake.Clientset, verbs ...string) {
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("no grant"))
	for _, verb := range verbs {
		cs.PrependReactor(verb, "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})
	}
}

func TestWatcherServesAndFollowsRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset(secretObj("kaalm-system", "openai-key", "v1"))
	gets := countVerb(cs, "get")
	lists := countVerb(cs, "list")
	watches := watchStarts(cs)
	w := New(ctx, cs)

	sec, err := w.Get(ctx, "kaalm-system", "openai-key")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := string(sec.Data["token"]); got != "v1" {
		t.Fatalf("token = %q, want v1", got)
	}
	eventually(t, func() bool { return watches() == 1 })
	// Rotation: an in-place update reaches the cache through the watch, with
	// no further request.
	rotated := secretObj("kaalm-system", "openai-key", "v2")
	if _, err := cs.CoreV1().Secrets("kaalm-system").Update(ctx, rotated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		sec, err := w.Get(ctx, "kaalm-system", "openai-key")
		return err == nil && string(sec.Data["token"]) == "v2"
	})
	if got := gets.Load(); got != 0 {
		t.Errorf("reads cost %d GETs, want 0", got)
	}
	if got := lists.Load(); got != 1 {
		t.Errorf("reads cost %d LISTs, want 1", got)
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
	refuse(cs, "list", "get")
	w := New(ctx, cs)
	w.SyncTimeout = 100 * time.Millisecond

	_, err := w.Get(ctx, "team-a", "hook")
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want the informer LIST's Forbidden", err)
	}
}

// grantable is a clientset holding team-a/hook whose LISTs and GETs are
// refused as Forbidden until grant is called, counting every LIST and GET.
type grantable struct {
	cs      *fake.Clientset
	granted atomic.Bool
	lists   atomic.Int32
	gets    atomic.Int32
}

func newGrantable() *grantable {
	g := &grantable{cs: fake.NewSimpleClientset(secretObj("team-a", "hook", "s3cr3t"))}
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("no grant"))
	for verb, n := range map[string]*atomic.Int32{"list": &g.lists, "get": &g.gets} {
		g.cs.PrependReactor(verb, "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			n.Add(1)
			if g.granted.Load() {
				return false, nil, nil
			}
			return true, nil, forbidden
		})
	}
	return g
}

func (g *grantable) grant() { g.granted.Store(true) }

// requests is every LIST and GET the clientset has answered.
func (g *grantable) requests() int32 { return g.lists.Load() + g.gets.Load() }

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
	// The first LIST and its probe GET only: the refused informer stops
	// instead of retrying.
	if got, probes := g.lists.Load(), g.gets.Load(); got != 1 || probes != 1 {
		t.Errorf("20 reads of a forbidden Secret cost %d LISTs and %d GETs, want 1 and 1", got, probes)
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
	before := g.requests()
	for range 10 {
		if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
			t.Fatalf("cached read: %v", err)
		}
	}
	if got := g.requests() - before; got != 0 {
		t.Errorf("reads after the grant cost %d requests, want 0 (cache hits)", got)
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

// An informer whose first LIST is refused, with GET refused too, stops
// instead of retrying on its own: until ErrorTTL passes, reads answer the
// refusal and cost no request, even once a grant lands.
func TestWatcherStopsAnInformerWhoseListIsForbidden(t *testing.T) {
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
	if got := g.requests(); got != 2 {
		t.Errorf("a stopped informer and 11 reads cost %d requests, want 2 (one LIST, one probe GET)", got)
	}
}

// hookKey is the Secret the revocable and grantable clientsets hold.
var hookKey = types.NamespacedName{Namespace: "team-a", Name: "hook"}

// hookSelector is the field selector the watcher's requests for hook carry.
const hookSelector = "metadata.name=hook"

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
// 1, UID hook-1) whose LISTs, GETs, and watches can be made to fail. A LIST
// answers the namespace's Secrets at collection version listRV ("7"). Every
// watch it opens is a fake the test drives: endWatch ends the newest one, as
// an API server does when a watch times out, and latestWatch delivers events
// through it. After expireOldVersions, a watch from any version but listRV
// ends at once with 410 Expired and is not counted as open. It records the
// field selector of every LIST and watch and the version every watch starts
// from.
type revocable struct {
	cs      *fake.Clientset
	lists   atomic.Int32 // every LIST
	refused atomic.Int32 // LISTs answered with listErr
	gets    atomic.Int32 // every GET
	expired atomic.Int32 // watches answered with 410 Expired

	mu             sync.Mutex
	listErr        error
	getErr         error
	watchErr       error
	listRV         string
	expire         bool
	listSelectors  []string
	watchSelectors []string
	watchVersions  []string
	watches        []*watch.RaceFreeFakeWatcher
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

var (
	secretsGVR = corev1.SchemeGroupVersion.WithResource("secrets")
	secretGVK  = corev1.SchemeGroupVersion.WithKind("Secret")
)

func newRevocable() *revocable {
	r := &revocable{cs: fake.NewSimpleClientset(hookAt(hookUID, "1")), listRV: "7"}
	r.cs.PrependReactor("list", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		r.lists.Add(1)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.listSelectors = append(r.listSelectors, action.(k8stesting.ListAction).GetListRestrictions().Fields.String())
		if r.listErr != nil {
			r.refused.Add(1)
			return true, nil, r.listErr
		}
		obj, err := r.cs.Tracker().List(secretsGVR, secretGVK, action.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		list := obj.(*corev1.SecretList)
		list.ResourceVersion = r.listRV
		return true, list, nil
	})
	r.cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		r.gets.Add(1)
		r.mu.Lock()
		err := r.getErr
		r.mu.Unlock()
		if err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	r.cs.PrependWatchReactor("secrets", func(action k8stesting.Action) (bool, watch.Interface, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		restrictions := action.(k8stesting.WatchAction).GetWatchRestrictions()
		r.watchSelectors = append(r.watchSelectors, restrictions.Fields.String())
		r.watchVersions = append(r.watchVersions, restrictions.ResourceVersion)
		if r.watchErr != nil {
			return true, nil, r.watchErr
		}
		if rv := restrictions.ResourceVersion; r.expire && rv != r.listRV {
			r.expired.Add(1)
			fw := watch.NewRaceFreeFake()
			st := apierrors.NewResourceExpired(fmt.Sprintf("too old resource version: %s (100)", rv)).ErrStatus
			fw.Error(&st)
			return true, fw, nil
		}
		fw := watch.NewRaceFreeFake()
		r.watches = append(r.watches, fw)
		return true, fw, nil
	})
	return r
}

// settled waits across a reflector backoff until the clientset has answered
// lists LISTs and opened watches watches.
func (r *revocable) settled(t *testing.T, lists int32, watches int) {
	t.Helper()
	eventuallyAcrossBackoff(t, func() bool { return r.lists.Load() == lists && r.watchCount() == watches })
}

// removeHook deletes team-a/hook from the clientset's store without an
// event.
func (r *revocable) removeHook() {
	if err := r.cs.Tracker().Delete(secretsGVR, "team-a", "hook"); err != nil {
		panic(err)
	}
}

// fail makes later LISTs answer listErr and later watches watchErr; nil lets
// them through. GETs are left as they are.
func (r *revocable) fail(listErr, watchErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listErr, r.watchErr = listErr, watchErr
}

// failGets makes later GETs answer err; nil lets them through.
func (r *revocable) failGets(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getErr = err
}

// revoke refuses later LISTs, GETs, and watches as Forbidden, as the API
// server does once the Role granting them is gone.
func (r *revocable) revoke() {
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("role deleted"))
	r.fail(forbidden, forbidden)
	r.failGets(forbidden)
}

// refuseListOnly refuses later LISTs as Forbidden and leaves GETs and
// watches allowed, as a Role from before the watcher listed does.
func (r *revocable) refuseListOnly() {
	r.fail(apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("get and watch only")), nil)
}

// expireOldVersions makes later watches from any version but the LIST's
// end at once with 410 Expired, as the API server answers once its watch
// cache has dropped that version.
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

// recorded returns the field selectors of every LIST and watch so far and
// the version every watch started from.
func (r *revocable) recorded() (lists, watches, versions []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.listSelectors...), append([]string{}, r.watchSelectors...),
		append([]string{}, r.watchVersions...)
}

// endWatch ends the newest watch, so the reflector watches or lists again.
func (r *revocable) endWatch() { r.latestWatch().Stop() }

// A read costs one LIST selected to the Secret's name and one watch with the
// same selector from that LIST's version, and no GET. Later reads are cache
// hits.
func TestWatcherReadCostsOneNameSelectedListAndOneWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)

	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil || string(sec.Data["token"]) != "v" {
		t.Fatalf("first read: sec = %v, err = %v, want token v", sec, err)
	}
	r.settled(t, 1, 1)
	for range 10 {
		if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
			t.Fatalf("cached read: %v", err)
		}
	}
	// Longer than the reflector's first retry, so a late request would show.
	time.Sleep(time.Second)
	lists, watches, versions := r.recorded()
	if len(lists) != 1 || lists[0] != hookSelector {
		t.Errorf("LIST selectors = %q, want one %q", lists, hookSelector)
	}
	if len(watches) != 1 || watches[0] != hookSelector {
		t.Errorf("watch selectors = %q, want one %q", watches, hookSelector)
	}
	if len(versions) != 1 || versions[0] != "7" {
		t.Errorf("watch versions = %q, want one from the LIST's version 7", versions)
	}
	if got := r.gets.Load(); got != 0 {
		t.Errorf("reads cost %d GETs, want 0", got)
	}
}

// A Secret deleted after the LIST arrives on the watch, which starts at the
// LIST's version: the informer drops it and reports the delete, with no
// GET.
func TestWatcherDropsASecretDeletedBetweenListAndWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)
	rec := newRecorder()
	w.Subscribe(rec.notify)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	r.settled(t, 1, 1)
	if _, _, versions := r.recorded(); len(versions) != 1 || versions[0] != "7" {
		t.Fatalf("watch versions = %q, want one from the LIST's version 7", versions)
	}
	// The API server replays the delete that landed after version 7.
	r.removeHook()
	r.latestWatch().Delete(hookAt(hookUID, "8"))
	eventually(t, func() bool {
		_, err := w.Get(ctx, "team-a", "hook")
		return apierrors.IsNotFound(err)
	})
	rec.expect(t, hookKey)
	rec.expectNone(t, 200*time.Millisecond)
	if got := r.gets.Load(); got != 0 {
		t.Errorf("GETs = %d, want 0", got)
	}
}

// A synced informer whose relist is refused, with GET refused too (its Role
// was deleted), stops, and the next read answers Forbidden instead of the
// cached Secret.
func TestWatcherStopsAnInformerWhoseRelistIsForbidden(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	r.settled(t, 1, 1)
	entry := currentEntry(w, hookKey)
	r.revoke()
	// The watch re-opens into the refusal, and the relist after it decides.
	r.endWatch()
	eventuallyAcrossBackoff(t, func() bool { return r.refused.Load() >= 1 })
	eventuallyAcrossBackoff(t, entry.informer.IsStopped)

	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("read after the refusal: err = %v, want Forbidden, not the cached Secret", err)
	}
}

// A LIST refused while GET is still allowed is a Role that predates the
// watcher's list (an upgrade in progress): reads go live through GET until
// ErrorTTL, then the next read tries the LIST again.
func TestWatcherReadsLiveWhileListIsForbiddenButGetAllowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	r.refuseListOnly()
	w := New(ctx, r.cs)
	w.ErrorTTL = time.Hour

	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil || string(sec.Data["token"]) != "v" {
		t.Fatalf("first read: sec = %v, err = %v, want the live Secret", sec, err)
	}
	entry := currentEntry(w, hookKey)
	eventuallyAcrossBackoff(t, entry.informer.IsStopped)
	lists, gets := r.lists.Load(), r.gets.Load()
	for i := range 10 {
		sec, err := w.Get(ctx, "team-a", "hook")
		if err != nil || string(sec.Data["token"]) != "v" {
			t.Fatalf("read %d: sec = %v, err = %v, want the live Secret", i, sec, err)
		}
	}
	if got := r.gets.Load() - gets; got != 10 {
		t.Errorf("10 reads cost %d GETs, want 10 live reads", got)
	}
	if got := r.lists.Load() - lists; got != 0 {
		t.Errorf("10 reads within ErrorTTL cost %d LISTs, want 0", got)
	}

	// Past ErrorTTL the next read tries the LIST again.
	w.ErrorTTL = 0
	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("read after ErrorTTL: %v", err)
	}
	if got := r.lists.Load() - lists; got != 1 {
		t.Errorf("the read after ErrorTTL cost %d LISTs, want 1", got)
	}

	// Once the Role carries list, a read syncs and later reads are cache
	// hits.
	r.fail(nil, nil)
	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("read after the grant: %v", err)
	}
	r.settled(t, r.lists.Load(), 1)
	lists, gets = r.lists.Load(), r.gets.Load()
	for range 10 {
		if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
			t.Fatalf("cached read: %v", err)
		}
	}
	if r.lists.Load() != lists || r.gets.Load() != gets {
		t.Errorf("10 cached reads cost %d LISTs and %d GETs, want none",
			r.lists.Load()-lists, r.gets.Load()-gets)
	}
}

// While the LIST is refused, a read answers what the live GET answers: an
// absent Secret is NotFound and a failing API server is its own error, never
// the LIST's Forbidden.
func TestWatcherAnswersTheLiveGetWhileListIsForbidden(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(r *revocable)
		want  func(error) bool
	}{
		{"absent Secret", func(r *revocable) { r.removeHook() }, apierrors.IsNotFound},
		{"internal error", func(r *revocable) {
			r.failGets(apierrors.NewInternalError(errors.New("etcd unavailable")))
		}, apierrors.IsInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newRevocable()
			r.refuseListOnly()
			tc.setup(r)
			w := New(ctx, r.cs)

			if _, err := w.Get(ctx, "team-a", "hook"); !tc.want(err) {
				t.Fatalf("read: err = %v, want the live GET's error", err)
			}
		})
	}
}

// WatchTimeout replaces the reflector's watch timeout; 0 keeps it.
func TestWatcherWatchTimeoutSetsTheRequestTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    func(int64) bool
	}{
		{"set", 5 * time.Second, func(s int64) bool { return s == 5 }},
		{"default", 0, func(s int64) bool { return s >= 300 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			var timeouts []int64
			cs := hookedClient{
				Interface: fake.NewSimpleClientset(secretObj("team-a", "hook", "x")),
				watch: func(opts metav1.ListOptions) {
					mu.Lock()
					defer mu.Unlock()
					if opts.TimeoutSeconds != nil {
						timeouts = append(timeouts, *opts.TimeoutSeconds)
					} else {
						timeouts = append(timeouts, -1)
					}
				},
			}
			w := New(ctx, cs)
			w.WatchTimeout = tc.timeout
			if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
				t.Fatalf("read: %v", err)
			}
			eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(timeouts) == 1
			})
			mu.Lock()
			defer mu.Unlock()
			if !tc.want(timeouts[0]) {
				t.Errorf("watch TimeoutSeconds = %d", timeouts[0])
			}
		})
	}
}

// The gauge counts running informers: one per Secret read, none for a
// stopped one.
func TestWatcherReportsWatchedSecretsGauge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cs := fake.NewSimpleClientset(secretObj("team-a", "hook", "x"), secretObj("team-a", "other", "y"))
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "denied", errors.New("no grant"))
	for _, verb := range []string{"list", "get"} {
		cs.PrependReactor(verb, "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if la, ok := action.(k8stesting.ListAction); ok && la.GetListRestrictions().Fields.String() == "metadata.name=denied" {
				return true, nil, forbidden
			}
			if ga, ok := action.(k8stesting.GetAction); ok && ga.GetName() == "denied" {
				return true, nil, forbidden
			}
			return false, nil, nil
		})
	}
	w := New(ctx, cs)
	gauge := w.Collector()
	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Fatalf("gauge before any read = %v, want 0", got)
	}
	for _, name := range []string{"hook", "other"} {
		if _, err := w.Get(ctx, "team-a", name); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
	}
	if got := testutil.ToFloat64(gauge); got != 2 {
		t.Fatalf("gauge after two reads = %v, want 2", got)
	}

	// One janitor pass by hand: other has idled out.
	w.mu.Lock()
	other := types.NamespacedName{Namespace: "team-a", Name: "other"}
	w.informers[other].halt()
	w.informers[other].halt()
	delete(w.informers, other)
	w.mu.Unlock()
	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Fatalf("gauge after an eviction = %v, want 1 (a halt counts once)", got)
	}

	if _, err := w.Get(ctx, "team-a", "denied"); !apierrors.IsForbidden(err) {
		t.Fatalf("read denied: err = %v, want Forbidden", err)
	}
	denied := currentEntry(w, types.NamespacedName{Namespace: "team-a", Name: "denied"})
	eventuallyAcrossBackoff(t, denied.informer.IsStopped)
	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Fatalf("gauge after a refused read = %v, want 1", got)
	}

	cancel()
	eventually(t, func() bool { return testutil.ToFloat64(gauge) == 0 })
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
	r.settled(t, 1, 1)
	old := currentEntry(w, hookKey)
	r.revoke()
	r.endWatch()
	eventuallyAcrossBackoff(t, old.informer.IsStopped)

	before := r.lists.Load() + r.gets.Load()
	if _, err := w.Get(ctx, "team-a", "hook"); !apierrors.IsForbidden(err) {
		t.Fatalf("read within ErrorTTL: err = %v, want the cached Forbidden", err)
	}
	if got := r.lists.Load() + r.gets.Load() - before; got != 0 {
		t.Errorf("read within ErrorTTL cost %d requests, want 0", got)
	}

	r.fail(nil, nil)
	r.failGets(nil)
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

// Errors other than a refused LIST leave the informer running: a synced one
// keeps serving its cache and resumes once the error clears.
func TestWatcherKeepsServingThroughTransientErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"internal error", apierrors.NewInternalError(errors.New("etcd unavailable"))},
		{"server timeout", apierrors.NewServerTimeout(corev1.Resource("secrets"), "list", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newRevocable()
			w := New(ctx, r.cs)

			if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
				t.Fatalf("first read: %v", err)
			}
			r.settled(t, 1, 1)
			entry := currentEntry(w, hookKey)
			r.fail(tc.err, tc.err)
			r.endWatch()
			// A second failed LIST means the informer retried after the first.
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
			if got := r.gets.Load(); got != 0 {
				t.Errorf("GETs = %d, want 0", got)
			}
		})
	}
}

// A Secret gone at a relist is a valid empty state, not a reason to stop.
func TestWatcherKeepsRunningOnANotFoundSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	r.settled(t, 1, 1)
	entry := currentEntry(w, hookKey)
	// A bookmark moves the reflector past the LIST's version, the re-watch
	// from there expires, and the relist finds the Secret gone.
	r.latestWatch().Action(watch.Bookmark, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "50"}})
	r.expireOldVersions()
	r.removeHook()
	r.endWatch()
	r.settled(t, 2, 2)
	if entry.informer.IsStopped() {
		t.Fatal("an empty relist stopped the informer")
	}
	// The relist queues the deletion and opens the next watch at once; the
	// informer drops the Secret from its store a moment later, on its own
	// goroutine.
	eventually(t, func() bool {
		_, err := w.Get(ctx, "team-a", "hook")
		return apierrors.IsNotFound(err)
	})
	if got := r.gets.Load(); got != 0 {
		t.Errorf("GETs = %d, want 0", got)
	}
}

// Only a refused LIST stops an informer. A refused watch alone leaves the
// informer relisting and serving its cache.
func TestWatcherKeepsRunningWhenOnlyTheWatchIsForbidden(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRevocable()
	w := New(ctx, r.cs)

	if _, err := w.Get(ctx, "team-a", "hook"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	r.settled(t, 1, 1)
	entry := currentEntry(w, hookKey)
	r.fail(nil, apierrors.NewForbidden(corev1.Resource("secrets"), "hook", errors.New("no watch")))
	r.endWatch()
	// The first LIST and two relists after refused watches.
	eventuallyAcrossBackoff(t, func() bool { return r.lists.Load() >= 3 })
	if entry.informer.IsStopped() {
		t.Fatal("a refused watch stopped the informer")
	}
	sec, err := w.Get(ctx, "team-a", "hook")
	if err != nil || string(sec.Data["token"]) != "v" {
		t.Fatalf("read: sec = %v, err = %v, want the Secret", sec, err)
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
	r.settled(t, 1, 1)
	// A bookmark moves the reflector to version 50; the next watch resumes
	// from there, and the API server has dropped it by then.
	r.latestWatch().Action(watch.Bookmark, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "50"}})
	r.expireOldVersions()
	r.endWatch()
	r.settled(t, 2, 2)
	if r.expired.Load() < 1 {
		t.Fatal("no watch expired, so the test did not exercise the relist")
	}
	// The relist brings back the unchanged Secret, which is no change.
	// Longer than the reflector's first retry, so a relist loop would show.
	rec.expectNone(t, 2*time.Second)
	if got := r.lists.Load(); got != 2 {
		t.Errorf("LISTs = %d, want 2", got)
	}
	if got := r.watchCount(); got != 2 {
		t.Errorf("open watches = %d, want 2", got)
	}

	changed := secretObj("team-a", "hook", "v2")
	changed.ResourceVersion = "51"
	r.latestWatch().Modify(changed)
	rec.expect(t, hookKey)
	rec.expectNone(t, 200*time.Millisecond)
}

// hookedClient calls list before each Secret LIST and watch before each
// Secret watch, when set.
type hookedClient struct {
	kubernetes.Interface
	list  func(ctx context.Context)
	watch func(opts metav1.ListOptions)
}

func (c hookedClient) CoreV1() typedcorev1.CoreV1Interface {
	return hookedCore{CoreV1Interface: c.Interface.CoreV1(), c: c}
}

type hookedCore struct {
	typedcorev1.CoreV1Interface
	c hookedClient
}

func (h hookedCore) Secrets(namespace string) typedcorev1.SecretInterface {
	return hookedSecrets{SecretInterface: h.CoreV1Interface.Secrets(namespace), c: h.c}
}

type hookedSecrets struct {
	typedcorev1.SecretInterface
	c hookedClient
}

func (h hookedSecrets) List(ctx context.Context, opts metav1.ListOptions) (*corev1.SecretList, error) {
	if h.c.list != nil {
		h.c.list(ctx)
	}
	return h.SecretInterface.List(ctx, opts)
}

func (h hookedSecrets) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	if h.c.watch != nil {
		h.c.watch(opts)
	}
	return h.SecretInterface.Watch(ctx, opts)
}

func TestWatcherFallsBackToLiveReadWhenTheListHangs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var first atomic.Bool
	cs := hookedClient{
		Interface: fake.NewSimpleClientset(secretObj("team-a", "hook", "s3cr3t")),
		list: func(context.Context) {
			if first.CompareAndSwap(false, true) {
				<-release
			}
		},
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
