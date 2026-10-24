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
	"fmt"
	"runtime"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// newTestLean builds a lean reader as an admin through a counting transport.
func newTestLean(t *testing.T, watchTimeout time.Duration) (*leanReader, *countingTransport) {
	t.Helper()
	rc, client, counter := countedClientFor(t, restCfg)
	return newTestLeanFor(t, rc, client, watchTimeout), counter
}

func newTestLeanFor(t *testing.T, rc *rest.Config, client kubernetes.Interface,
	watchTimeout time.Duration) *leanReader {
	t.Helper()
	r, err := newLeanReader(context.Background(), rc, client, watchTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLeanReaderServesFromCacheAfterFirstList(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	r, counter := newTestLean(t, 0)
	defer stopAndDrain(t, r, counter)

	created := createSecret(t, ns, "one", false)
	got, err := r.Get(ctx, ns, "one")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResourceVersion != created.ResourceVersion {
		t.Fatalf("Get returned version %s, want %s", got.ResourceVersion, created.ResourceVersion)
	}
	settle(t, counter, 1)
	if got := counter.snapshot(); got != (requestCounts{List: 1, Watch: 1}) {
		t.Fatalf("requests = %+v, want List 1, Watch 1", got)
	}
	if _, err := r.Get(ctx, ns, "one"); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got := counter.snapshot(); got != (requestCounts{List: 1, Watch: 1}) {
		t.Fatalf("requests after a cached read = %+v, want List 1, Watch 1", got)
	}

	rv, err := updateSecret(ctx, adminClient, ns, "one")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the update to reach the reader", func() bool {
		sec, err := r.Get(ctx, ns, "one")
		return err == nil && sec.ResourceVersion == rv
	})
	if got := counter.snapshot(); got.Get != 0 || got.List != 1 {
		t.Fatalf("requests after the update = %+v, want no GET and one LIST", got)
	}
}

func TestLeanReaderDropsADeletedSecret(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	r, counter := newTestLean(t, 0)
	defer stopAndDrain(t, r, counter)

	createSecret(t, ns, "one", false)
	if _, err := r.Get(ctx, ns, "one"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := adminClient.CoreV1().Secrets(ns).Delete(ctx, "one", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the delete to reach the reader", func() bool {
		_, err := r.Get(ctx, ns, "one")
		return apierrors.IsNotFound(err)
	})

	if _, err := r.Get(ctx, ns, "later"); !apierrors.IsNotFound(err) {
		t.Fatalf("Get of an absent Secret = %v, want NotFound", err)
	}
	createSecret(t, ns, "later", false)
	eventually(t, 5*time.Second, "a created Secret to reach the reader", func() bool {
		_, err := r.Get(ctx, ns, "later")
		return err == nil
	})
}

// A watch the server ends at its timeout re-opens from the last version
// seen, with no new LIST, and keeps delivering.
func TestLeanReaderRestartsAnEndedWatch(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	r, counter := newTestLean(t, time.Second)
	defer stopAndDrain(t, r, counter)

	createSecret(t, ns, "one", false)
	if _, err := r.Get(ctx, ns, "one"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	eventually(t, 10*time.Second, "the watch to re-open", func() bool {
		return counter.watches.Load() >= 2
	})
	if got := counter.list.Load(); got != 1 {
		t.Fatalf("LISTs after the watch re-opened = %d, want 1", got)
	}
	rv, err := updateSecret(ctx, adminClient, ns, "one")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the update to reach the reader", func() bool {
		sec, err := r.Get(ctx, ns, "one")
		return err == nil && sec.ResourceVersion == rv
	})
}

func TestLeanReaderStopsOnForbidden(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	createSecret(t, ns, "granted", false)
	createSecret(t, ns, "other", false)
	createSecret(t, ns, "ungranted", false)
	userCfg, regrant := grantedUser(t, ns, "granted", "other")
	rc, client, counter := countedClientFor(t, userCfg)
	r := newTestLeanFor(t, rc, client, time.Second)
	defer stopAndDrain(t, r, counter)

	if _, err := r.Get(ctx, ns, "ungranted"); !apierrors.IsForbidden(err) {
		t.Fatalf("Get of an ungranted Secret = %v, want Forbidden", err)
	}
	if got := counter.snapshot(); got.List != 1 || got.Get != 0 {
		t.Fatalf("requests = %+v, want one refused LIST and no GET", got)
	}
	r.mu.Lock()
	held := len(r.entries)
	r.mu.Unlock()
	if held != 0 || counter.openWatches.Load() != 0 {
		t.Fatalf("after the refusal: %d entries and %d open watches, want none", held, counter.openWatches.Load())
	}

	if _, err := r.Get(ctx, ns, "granted"); err != nil {
		t.Fatalf("Get of a granted Secret: %v", err)
	}
	regrant("other")
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := r.Get(ctx, ns, "granted")
		if apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Get after the grant was removed = %v, want Forbidden within 15s", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Lean's point is its goroutine bound: its own loop per Secret plus
// net/http's per-stream goroutine.
func TestLeanReaderGoroutinesPerSecret(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t)
	const n = 20
	for i := range n {
		createSecret(t, ns, fmt.Sprintf("s%02d", i), false)
	}
	before := runtime.NumGoroutine()
	r, counter := newTestLean(t, 0)
	for i := range n {
		if _, err := r.Get(ctx, ns, fmt.Sprintf("s%02d", i)); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	settle(t, counter, n)
	got := runtime.NumGoroutine() - before
	t.Logf("%d goroutines for %d Secrets", got, n)
	if got > 2*n+drainSlack {
		t.Fatalf("%d goroutines for %d Secrets, want at most %d", got, n, 2*n+drainSlack)
	}
	stopAndDrain(t, r, counter)
	eventually(t, 10*time.Second, "goroutines to drain after Stop", func() bool {
		return runtime.NumGoroutine() <= before+drainSlack
	})
}
