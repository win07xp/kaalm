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

package console

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeReviewer authenticates any token in its map. It is safe for
// concurrent use; hook, when set, runs inside every review before it
// answers.
type fakeReviewer struct {
	tokens map[string]Identity
	hook   func()

	mu sync.Mutex
	n  int
}

func (f *fakeReviewer) Review(_ context.Context, token string) (Identity, error) {
	f.mu.Lock()
	f.n++
	id, ok := f.tokens[token]
	f.mu.Unlock()
	if f.hook != nil {
		f.hook()
	}
	if ok {
		return id, nil
	}
	return Identity{}, fmt.Errorf("token not authenticated")
}

// count is the number of reviews run so far.
func (f *fakeReviewer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// fakeAuthorizer answers from a map keyed user/verb/resource/namespace and
// counts calls. It is safe for concurrent use; hook, when set, runs inside
// every call before it answers, and a key in fail answers an error.
type fakeAuthorizer struct {
	allowed map[string]bool
	fail    map[string]bool
	hook    func(key string)

	mu   sync.Mutex
	n    int
	last string
}

func (f *fakeAuthorizer) Allowed(_ context.Context, id Identity, verb, group, resource, namespace string) (bool, error) {
	key := fmt.Sprintf("%s/%s/%s.%s/%s", id.Username, verb, resource, group, namespace)
	f.mu.Lock()
	f.n++
	f.last = key
	allowed, fail := f.allowed[key], f.fail[key]
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(key)
	}
	if fail {
		return false, errors.New("authorizer unavailable")
	}
	return allowed, nil
}

// count is the number of reviews run so far.
func (f *fakeAuthorizer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// lastKey is the last question asked.
func (f *fakeAuthorizer) lastKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func TestAccessChecker_VerbsAndCaching(t *testing.T) {
	az := &fakeAuthorizer{allowed: map[string]bool{
		"priya/list/agents.kaalm.io/team-a":          true,
		"priya/create/agentchannels.kaalm.io/team-a": false,
	}}
	g := NewAccessChecker(az)
	now := time.Now()
	g.now = func() time.Time { return now }
	priya := Identity{Username: "priya"}

	if ok, err := g.CanView(context.Background(), priya, "team-a"); err != nil || !ok {
		t.Fatalf("CanView = %v, %v", ok, err)
	}
	if az.lastKey() != "priya/list/agents.kaalm.io/team-a" {
		t.Errorf("view check asked %q", az.lastKey())
	}
	if ok, _ := g.CanChat(context.Background(), priya, "team-a"); ok {
		t.Error("CanChat must be denied")
	}
	if az.lastKey() != "priya/create/agentchannels.kaalm.io/team-a" {
		t.Errorf("chat check asked %q", az.lastKey())
	}

	// Within the TTL the cached answers are served: no new authorizer calls.
	before := az.count()
	_, _ = g.CanView(context.Background(), priya, "team-a")
	_, _ = g.CanChat(context.Background(), priya, "team-a")
	if az.count() != before {
		t.Errorf("cached checks must not hit the authorizer (calls %d -> %d)", before, az.count())
	}

	// Past the TTL the cache expires.
	now = now.Add(sarCacheTTL + time.Second)
	_, _ = g.CanView(context.Background(), priya, "team-a")
	if az.count() != before+1 {
		t.Error("an expired entry must re-ask the authorizer")
	}
}

func TestCachingReviewer(t *testing.T) {
	fr := &fakeReviewer{tokens: map[string]Identity{"tok": {Username: "priya"}}}
	cr := NewCachingReviewer(fr)
	now := time.Now()
	cr.now = func() time.Time { return now }

	id, err := cr.Review(context.Background(), "tok")
	if err != nil || id.Username != "priya" {
		t.Fatalf("review = %+v, %v", id, err)
	}
	_, _ = cr.Review(context.Background(), "tok")
	if fr.count() != 1 {
		t.Errorf("cached review must not hit the reviewer (calls %d)", fr.count())
	}

	// Failures are never cached.
	if _, err := cr.Review(context.Background(), "bad"); err == nil {
		t.Fatal("bad token must fail")
	}
	if _, err := cr.Review(context.Background(), "bad"); err == nil {
		t.Fatal("bad token must fail again")
	}
	if fr.count() != 3 {
		t.Errorf("failed reviews must pass through every time (calls %d)", fr.count())
	}

	now = now.Add(reviewCacheTTL + time.Second)
	_, _ = cr.Review(context.Background(), "tok")
	if fr.count() != 4 {
		t.Error("an expired review entry must re-review")
	}
}

func TestSessionStore_Lifecycle(t *testing.T) {
	fr := &fakeReviewer{tokens: map[string]Identity{"tok": {Username: "priya"}}}
	st := NewSessionStore(fr)
	now := time.Now()
	st.now = func() time.Time { return now }

	if _, _, err := st.Create(context.Background(), "bad"); err == nil {
		t.Fatal("a dead token must not create a session")
	}

	value, id, err := st.Create(context.Background(), "tok")
	if err != nil || id.Username != "priya" || value == "" {
		t.Fatalf("create = %q, %+v, %v", value, id, err)
	}
	if got, ok := st.Resolve(context.Background(), value); !ok || got.Username != "priya" {
		t.Fatalf("resolve = %+v, %v", got, ok)
	}
	if _, ok := st.Resolve(context.Background(), "unknown"); ok {
		t.Error("an unknown session must not resolve")
	}

	// Within the review interval no re-review happens.
	before := fr.count()
	_, _ = st.Resolve(context.Background(), value)
	if fr.count() != before {
		t.Error("a fresh session must not re-review the token")
	}

	// Past the interval the stored token is re-reviewed; a revoked token
	// kills the session.
	now = now.Add(reviewCacheTTL + time.Second)
	delete(fr.tokens, "tok")
	if _, ok := st.Resolve(context.Background(), value); ok {
		t.Fatal("a session whose token died must not resolve")
	}
	if _, ok := st.Resolve(context.Background(), value); ok {
		t.Fatal("the dead session must be forgotten")
	}
}

func TestSessionStore_MaxAge(t *testing.T) {
	fr := &fakeReviewer{tokens: map[string]Identity{"tok": {Username: "priya"}}}
	st := NewSessionStore(fr)
	now := time.Now()
	st.now = func() time.Time { return now }

	value, _, err := st.Create(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	// The token stays valid, but the absolute cap fires regardless.
	now = now.Add(sessionMaxAge + time.Minute)
	if _, ok := st.Resolve(context.Background(), value); ok {
		t.Error("a session past the 24h cap must not resolve")
	}
}

func TestSessionStore_Delete(t *testing.T) {
	fr := &fakeReviewer{tokens: map[string]Identity{"tok": {Username: "priya"}}}
	st := NewSessionStore(fr)
	value, _, _ := st.Create(context.Background(), "tok")
	st.Delete(value)
	if _, ok := st.Resolve(context.Background(), value); ok {
		t.Error("a deleted session must not resolve")
	}
}

func TestAccessChecker_CanViewAllAsksClusterWideAndCaches(t *testing.T) {
	az := &fakeAuthorizer{allowed: map[string]bool{
		"priya/list/agents.kaalm.io/": true,
	}}
	g := NewAccessChecker(az)
	now := time.Now()
	g.now = func() time.Time { return now }

	if ok, err := g.CanViewAll(context.Background(), Identity{Username: "priya"}); err != nil || !ok {
		t.Fatalf("CanViewAll = %v, %v", ok, err)
	}
	if az.lastKey() != "priya/list/agents.kaalm.io/" {
		t.Errorf("cluster-wide check asked %q, want an empty-namespace review", az.lastKey())
	}
	if ok, _ := g.CanViewAll(context.Background(), Identity{Username: "dev"}); ok {
		t.Error("dev holds no cluster-wide grant")
	}
	before := az.count()
	_, _ = g.CanViewAll(context.Background(), Identity{Username: "priya"})
	if az.count() != before {
		t.Error("a cached cluster-wide answer must not hit the authorizer")
	}
}

func TestAccessChecker_SweepDropsExpiredEntries(t *testing.T) {
	az := &fakeAuthorizer{allowed: map[string]bool{}}
	g := NewAccessChecker(az)
	now := time.Now()
	g.now = func() time.Time { return now }
	priya := Identity{Username: "priya"}

	_, _ = g.CanView(context.Background(), priya, "team-a")
	now = now.Add(sarCacheTTL / 2)
	_, _ = g.CanView(context.Background(), priya, "team-b")

	now = now.Add(sarCacheTTL/2 + time.Second)
	g.Sweep()
	if len(g.cache) != 1 {
		t.Fatalf("after sweep the cache holds %d entries, want only team-b's", len(g.cache))
	}
	if _, ok := g.cache[accessKey{user: "priya", namespace: "team-b", verb: "list:agents"}]; !ok {
		t.Error("the unexpired entry must survive the sweep")
	}
}

func TestCachingReviewer_SweepDropsExpiredEntries(t *testing.T) {
	fr := &fakeReviewer{tokens: map[string]Identity{"old": {Username: "a"}, "new": {Username: "b"}}}
	cr := NewCachingReviewer(fr)
	now := time.Now()
	cr.now = func() time.Time { return now }

	_, _ = cr.Review(context.Background(), "old")
	now = now.Add(reviewCacheTTL / 2)
	_, _ = cr.Review(context.Background(), "new")

	now = now.Add(reviewCacheTTL/2 + time.Second)
	cr.Sweep()
	if len(cr.cache) != 1 {
		t.Fatalf("after sweep the cache holds %d entries, want 1", len(cr.cache))
	}
}

func TestSessionStore_SweepDropsSessionsPastMaxAge(t *testing.T) {
	fr := &fakeReviewer{tokens: map[string]Identity{"tok": {Username: "priya"}}}
	st := NewSessionStore(fr)
	now := time.Now()
	st.now = func() time.Time { return now }

	old, _, _ := st.Create(context.Background(), "tok")
	now = now.Add(sessionMaxAge / 2)
	fresh, _, _ := st.Create(context.Background(), "tok")

	now = now.Add(sessionMaxAge/2 + time.Minute)
	st.Sweep()
	if _, ok := st.m[old]; ok {
		t.Error("a session past the 24h cap must be swept without being touched")
	}
	if _, ok := st.m[fresh]; !ok {
		t.Error("a live session must survive the sweep")
	}
}

// blockingAuthorizer is a fakeAuthorizer whose calls wait on release and
// that closes entered on its first call.
func blockingAuthorizer(allowed map[string]bool) (az *fakeAuthorizer, entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	az = &fakeAuthorizer{allowed: allowed, hook: func(string) {
		once.Do(func() { close(entered) })
		<-release
	}}
	return az, entered, release
}

// Concurrent misses for one answer share one SubjectAccessReview.
func TestAccessChecker_CollapsesConcurrentMisses(t *testing.T) {
	az, entered, release := blockingAuthorizer(map[string]bool{"priya/list/agents.kaalm.io/team-a": true})
	g := NewAccessChecker(az)
	priya := Identity{Username: "priya"}

	results := make(chan bool, 10)
	for range 10 {
		go func() {
			ok, err := g.CanView(context.Background(), priya, "team-a")
			results <- ok && err == nil
		}()
	}
	<-entered
	time.Sleep(100 * time.Millisecond) // let the other callers arrive
	close(release)
	for range 10 {
		if !<-results {
			t.Error("a waiter did not get the shared answer")
		}
	}
	if got := az.count(); got != 1 {
		t.Errorf("10 concurrent misses ran %d reviews, want 1", got)
	}
}

// A waiter whose request ends returns at once; the others still get the
// shared answer.
func TestAccessChecker_CallerCancelDoesNotFailOthers(t *testing.T) {
	az, entered, release := blockingAuthorizer(map[string]bool{"priya/list/agents.kaalm.io/team-a": true})
	g := NewAccessChecker(az)
	priya := Identity{Username: "priya"}

	type result struct {
		ok  bool
		err error
	}
	ctx, cancel := context.WithCancel(context.Background())
	first, second := make(chan result, 1), make(chan result, 1)
	go func() {
		ok, err := g.CanView(ctx, priya, "team-a")
		first <- result{ok, err}
	}()
	<-entered
	go func() {
		ok, err := g.CanView(context.Background(), priya, "team-a")
		second <- result{ok, err}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case r := <-first:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("cancelled waiter = %v, %v; want context.Canceled", r.ok, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled waiter did not return while the review ran")
	}
	close(release)
	if r := <-second; !r.ok || r.err != nil {
		t.Errorf("other waiter = %v, %v; want the shared answer true", r.ok, r.err)
	}
}
