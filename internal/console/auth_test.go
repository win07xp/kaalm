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
	"fmt"
	"testing"
	"time"
)

// fakeReviewer authenticates any token in its map.
type fakeReviewer struct {
	tokens map[string]Identity
	calls  int
}

func (f *fakeReviewer) Review(_ context.Context, token string) (Identity, error) {
	f.calls++
	if id, ok := f.tokens[token]; ok {
		return id, nil
	}
	return Identity{}, fmt.Errorf("token not authenticated")
}

// fakeAuthorizer answers from a map keyed user/verb/resource/namespace and
// counts calls.
type fakeAuthorizer struct {
	allowed map[string]bool
	calls   int
	last    string
}

func (f *fakeAuthorizer) Allowed(_ context.Context, id Identity, verb, group, resource, namespace string) (bool, error) {
	f.calls++
	key := fmt.Sprintf("%s/%s/%s.%s/%s", id.Username, verb, resource, group, namespace)
	f.last = key
	return f.allowed[key], nil
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
	if az.last != "priya/list/agents.kaalm.io/team-a" {
		t.Errorf("view check asked %q", az.last)
	}
	if ok, _ := g.CanChat(context.Background(), priya, "team-a"); ok {
		t.Error("CanChat must be denied")
	}
	if az.last != "priya/create/agentchannels.kaalm.io/team-a" {
		t.Errorf("chat check asked %q", az.last)
	}

	// Within the TTL the cached answers are served: no new authorizer calls.
	before := az.calls
	_, _ = g.CanView(context.Background(), priya, "team-a")
	_, _ = g.CanChat(context.Background(), priya, "team-a")
	if az.calls != before {
		t.Errorf("cached checks must not hit the authorizer (calls %d -> %d)", before, az.calls)
	}

	// Past the TTL the cache expires.
	now = now.Add(sarCacheTTL + time.Second)
	_, _ = g.CanView(context.Background(), priya, "team-a")
	if az.calls != before+1 {
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
	if fr.calls != 1 {
		t.Errorf("cached review must not hit the reviewer (calls %d)", fr.calls)
	}

	// Failures are never cached.
	if _, err := cr.Review(context.Background(), "bad"); err == nil {
		t.Fatal("bad token must fail")
	}
	if _, err := cr.Review(context.Background(), "bad"); err == nil {
		t.Fatal("bad token must fail again")
	}
	if fr.calls != 3 {
		t.Errorf("failed reviews must pass through every time (calls %d)", fr.calls)
	}

	now = now.Add(reviewCacheTTL + time.Second)
	_, _ = cr.Review(context.Background(), "tok")
	if fr.calls != 4 {
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
	before := fr.calls
	_, _ = st.Resolve(context.Background(), value)
	if fr.calls != before {
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
	if az.last != "priya/list/agents.kaalm.io/" {
		t.Errorf("cluster-wide check asked %q, want an empty-namespace review", az.last)
	}
	if ok, _ := g.CanViewAll(context.Background(), Identity{Username: "dev"}); ok {
		t.Error("dev holds no cluster-wide grant")
	}
	before := az.calls
	_, _ = g.CanViewAll(context.Background(), Identity{Username: "priya"})
	if az.calls != before {
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
