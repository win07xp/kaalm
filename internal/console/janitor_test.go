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
	"sync/atomic"
	"testing"
	"time"
)

// TestJanitor_SweepsAllThreeCachesUntilCancelled proves the janitor empties
// expired entries from the session store, the access-check cache, and the
// review cache without any request touching them, and stops with its context.
func TestJanitor_SweepsAllThreeCachesUntilCancelled(t *testing.T) {
	reviewer := &fakeReviewer{tokens: map[string]Identity{"tok": {Username: "priya"}}}
	s := NewServer(Config{}, seededData(t), reviewer, NewAccessChecker(&fakeAuthorizer{}), &fakeChat{})

	// One shared, advanceable clock for all three caches.
	var offset atomic.Int64
	start := time.Now()
	clock := func() time.Time { return start.Add(time.Duration(offset.Load())) }
	cr := s.Reviewer.(*CachingReviewer)
	cr.now, s.Access.now, s.Sessions.now = clock, clock, clock

	ctx := context.Background()
	_, _ = cr.Review(ctx, "tok")
	_, _ = s.Access.CanView(ctx, Identity{Username: "priya"}, "team-a")
	_, _, _ = s.Sessions.Create(ctx, "tok")
	offset.Store(int64(sessionMaxAge + time.Minute))

	jctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.janitor(jctx, time.Millisecond); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		cr.mu.Lock()
		nReview := len(cr.cache)
		cr.mu.Unlock()
		s.Access.mu.Lock()
		nAccess := len(s.Access.cache)
		s.Access.mu.Unlock()
		s.Sessions.mu.Lock()
		nSess := len(s.Sessions.m)
		s.Sessions.mu.Unlock()
		if nReview == 0 && nAccess == 0 && nSess == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("janitor left review=%d access=%d sessions=%d", nReview, nAccess, nSess)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the janitor must stop when its context is cancelled")
	}
}
